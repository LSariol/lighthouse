// Package orchestrator decides when projects deploy: it checks GitHub for new
// commits on a schedule, deploys the projects that changed, and runs deploys
// asked for by the CLI. Every attempt is recorded in the projects.Store. A
// commit that keeps failing for a reason retrying can't fix marks its project
// broken, and isn't tried again until a new commit or `retry`.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsariol/lighthouse/internal/deploy"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/projects"
)

// Deployer deploys a project, or checks it without deploying.
// *deploy.Deployer implements it.
type Deployer interface {
	Deploy(ctx context.Context, req deploy.Request) deploy.Result
	Check(ctx context.Context, req deploy.Request) deploy.Result
}

// Commits finds a repository's latest commit. github.Client implements it.
type Commits interface {
	LatestCommit(ctx context.Context, repo github.Repo, token string) (string, error)
}

// ErrScanRunning is returned by Scan when another scan is in progress.
var ErrScanRunning = errors.New("a scan is already running")

type Orchestrator struct {
	store    projects.Store
	commits  Commits
	deployer Deployer
	gitToken string

	scanning sync.Mutex // one scan at a time
	paused   atomic.Bool
}

// New returns an Orchestrator that checks with gitToken (Lighthouse's GitHub
// token, read from Cove at startup).
func New(store projects.Store, commits Commits, deployer Deployer, gitToken string) *Orchestrator {
	return &Orchestrator{store: store, commits: commits, deployer: deployer, gitToken: gitToken}
}

func (o *Orchestrator) Pause()         { o.paused.Store(true) }
func (o *Orchestrator) Resume()        { o.paused.Store(false) }
func (o *Orchestrator) IsPaused() bool { return o.paused.Load() }

// Run scans now and then every interval until ctx is cancelled, skipping
// scans while paused. A scan's errors are logged; they don't stop the loop.
func (o *Orchestrator) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if !o.IsPaused() {
			if err := o.Scan(ctx); err != nil && !errors.Is(err, ErrScanRunning) && ctx.Err() == nil {
				slog.Error("scan finished with errors", "err", err)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Scan checks every project once and deploys those with a new commit. A
// project that fails is recorded and skipped; the others are still checked.
func (o *Orchestrator) Scan(ctx context.Context) error {
	if !o.scanning.TryLock() {
		return ErrScanRunning
	}
	defer o.scanning.Unlock()

	list, err := o.store.List(ctx)
	if err != nil {
		return err
	}

	var failed []string
	for _, p := range list {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := o.check(ctx, p); err != nil {
			slog.Error("check failed", "project", p.Name, "err", err)
			failed = append(failed, p.Name)
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

// ErrBroken means a project's latest commit failed BrokenAfter times and
// isn't tried again until a new commit appears or `retry` clears it.
var ErrBroken = errors.New("broken")

// check looks up p's latest commit and deploys it if it's new, unless it's
// the commit that made the project broken.
func (o *Orchestrator) check(ctx context.Context, p projects.Project) error {
	sha, err := o.commits.LatestCommit(ctx, p.Repo, o.gitToken)
	switch {
	case err != nil:
	case sha == p.DeployedSHA:
	case p.Broken && sha == p.FailingSHA:
		// Not tried again; the error stays the one that broke it.
		return o.record(ctx, p.Name, nil, false)
	default:
		slog.Info("new commit", "project", p.Name, "sha", short(sha))
		err = o.deploy(ctx, p, sha, projects.TriggerCheck)
	}
	return o.record(ctx, p.Name, err, true)
}

// record notes a check. With setError, err (or nil) becomes the last error.
func (o *Orchestrator) record(ctx context.Context, name string, err error, setError bool) error {
	var recErr error
	if setError {
		recErr = o.store.RecordCheck(ctx, name, err)
	} else {
		p, getErr := o.store.Get(ctx, name)
		if getErr == nil {
			recErr = o.store.RecordCheck(ctx, name, projects.ErrorString(p.LastError))
		}
	}
	// ErrNotFound: removed while it was being checked; nothing to record.
	if recErr != nil && !errors.Is(recErr, projects.ErrNotFound) {
		slog.Error("recording a check failed", "project", name, "err", recErr)
	}
	return err
}

// Deploy deploys the project called name now, at its latest commit, even if
// it's the one deployed or the one that broke it.
func (o *Orchestrator) Deploy(ctx context.Context, name string) error {
	p, err := o.store.Get(ctx, name)
	if err != nil {
		return err
	}
	sha, err := o.commits.LatestCommit(ctx, p.Repo, o.gitToken)
	if err != nil {
		return err
	}
	return o.deploy(ctx, p, sha, projects.TriggerManual)
}

// Check runs the project's latest commit through the deploy's checks (fetch,
// inspect, the rules, the test stage) without deploying or recording it. It
// returns the commit it checked.
func (o *Orchestrator) Check(ctx context.Context, name string) (deploy.Result, string, error) {
	p, err := o.store.Get(ctx, name)
	if err != nil {
		return deploy.Result{}, "", err
	}
	sha, err := o.commits.LatestCommit(ctx, p.Repo, o.gitToken)
	if err != nil {
		return deploy.Result{}, "", err
	}
	res := o.deployer.Check(ctx, deploy.Request{
		Project: p,
		SHA:     sha,
		Token:   o.gitToken,
		// Only look: is the compose project another project's?
		Claim: func(ctx context.Context, composeProject string) error {
			list, err := o.store.List(ctx)
			if err != nil {
				return err
			}
			for _, other := range list {
				if other.Name != p.Name && other.ComposeProject == composeProject {
					return o.takenError(ctx, composeProject)
				}
			}
			return nil
		},
	})
	return res, sha, nil
}

// Retry clears a project's failures (and broken state) and deploys it now.
func (o *Orchestrator) Retry(ctx context.Context, name string) error {
	if err := o.store.ClearFailures(ctx, name); err != nil {
		return err
	}
	return o.Deploy(ctx, name)
}

// deploy runs one deployment of p at sha and records it.
func (o *Orchestrator) deploy(ctx context.Context, p projects.Project, sha string, trigger string) error {
	started := time.Now()
	res := o.deployer.Deploy(ctx, deploy.Request{
		Project: p,
		SHA:     sha,
		Token:   o.gitToken,
		Claim: func(ctx context.Context, composeProject string) error {
			err := o.store.SetComposeProject(ctx, p.Name, composeProject)
			if errors.Is(err, projects.ErrComposeProjectTaken) {
				return o.takenError(ctx, composeProject)
			}
			return err
		},
	})

	d := projects.Deployment{
		Project:     p.Name,
		SHA:         sha,
		Trigger:     trigger,
		Status:      res.Status,
		FailureKind: res.FailureKind,
		FailedStep:  res.FailedStep,
		StartedAt:   started,
		FinishedAt:  time.Now(),
		Steps:       res.Steps,
	}
	if res.Err != nil {
		d.Error = projects.ErrorText(res.Err)
	}
	// A context cancelled by shutdown mustn't lose the record.
	if err := o.store.RecordDeployment(context.WithoutCancel(ctx), d); err != nil && !errors.Is(err, projects.ErrNotFound) {
		slog.Error("recording a deployment failed", "project", p.Name, "err", err)
	}

	if res.Err == nil {
		return nil
	}
	if res.FailureKind == projects.FailurePermanent {
		if now, err := o.store.Get(context.WithoutCancel(ctx), p.Name); err == nil && now.Broken {
			slog.Warn("project is broken: its latest commit isn't tried again until a new one or `retry`",
				"project", p.Name, "sha", short(sha), "failures", now.FailureCount)
			return fmt.Errorf("%w after %d failed deploys of %s: %v", ErrBroken, now.FailureCount, short(sha), res.Err)
		}
	}
	return res.Err
}

// takenError names the project that has a compose project already.
func (o *Orchestrator) takenError(ctx context.Context, composeProject string) error {
	list, _ := o.store.List(ctx)
	for _, other := range list {
		if other.ComposeProject == composeProject {
			return fmt.Errorf("the compose project %q belongs to %s already: two projects can't share one (change `name:` in one compose file)", composeProject, other.Name)
		}
	}
	return fmt.Errorf("the compose project %q belongs to another project", composeProject)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
