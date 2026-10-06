// Package orchestrator decides when projects deploy: it checks GitHub for new
// commits on a schedule, deploys the projects that changed, and runs deploys
// asked for by the CLI. Every attempt is recorded in the projects.Store. It
// grows into the v1.0.0 orchestrator (queue, backoff, reconcile loop).
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

	"github.com/LSariol/LightHouse/internal/projects"
)

// Deployer deploys a project. *deploy.Deployer implements it.
type Deployer interface {
	Deploy(ctx context.Context, p projects.Project) error
}

// Commits finds a repository's latest commit. github.Client implements it.
type Commits interface {
	LatestCommit(ctx context.Context, apiURL string, token string) (string, error)
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

// check looks up p's latest commit and deploys it if it's new. A failed
// deploy doesn't change the deployed commit, so the next scan tries again.
func (o *Orchestrator) check(ctx context.Context, p projects.Project) error {
	sha, err := o.commits.LatestCommit(ctx, p.Repo.APIURL(), o.gitToken)
	if err == nil && sha != p.DeployedSHA {
		slog.Info("new commit", "project", p.Name, "sha", short(sha))
		err = o.deploy(ctx, p, sha, projects.TriggerCheck)
	}

	if recErr := o.store.RecordCheck(ctx, p.Name, err); recErr != nil && !errors.Is(recErr, projects.ErrNotFound) {
		// ErrNotFound: removed while it was being checked; nothing to record.
		slog.Error("recording a check failed", "project", p.Name, "err", recErr)
	}
	return err
}

// Deploy deploys the project called name now, whether or not its commit
// changed.
func (o *Orchestrator) Deploy(ctx context.Context, name string) error {
	p, err := o.store.Get(ctx, name)
	if err != nil {
		return err
	}

	sha, err := o.commits.LatestCommit(ctx, p.Repo.APIURL(), o.gitToken)
	if err != nil {
		return err
	}
	return o.deploy(ctx, p, sha, projects.TriggerManual)
}

// deploy runs one deployment of p at sha and records it.
func (o *Orchestrator) deploy(ctx context.Context, p projects.Project, sha string, trigger string) error {
	started := time.Now()
	err := o.deployer.Deploy(ctx, p)

	d := projects.Deployment{
		Project:    p.Name,
		SHA:        sha,
		Trigger:    trigger,
		Status:     projects.StatusSucceeded,
		StartedAt:  started,
		FinishedAt: time.Now(),
	}
	if err != nil {
		d.Status = projects.StatusFailed
		d.Error = projects.ErrorText(err)
	}
	// A context cancelled by shutdown mustn't lose the record.
	if recErr := o.store.RecordDeployment(context.WithoutCancel(ctx), d); recErr != nil && !errors.Is(recErr, projects.ErrNotFound) {
		slog.Error("recording a deployment failed", "project", p.Name, "err", recErr)
	}
	return err
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
