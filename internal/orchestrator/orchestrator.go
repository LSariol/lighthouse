// Package orchestrator decides when projects deploy: it checks GitHub for new
// commits on a schedule, deploys the projects that changed, and runs deploys
// asked for by the CLI. It grows into the v1.0.0 orchestrator (queue,
// backoff, reconcile loop).
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

	"github.com/LSariol/LightHouse/internal/watchlist"
)

// Deployer deploys a project. *deploy.Deployer implements it.
type Deployer interface {
	Deploy(ctx context.Context, p watchlist.Project) error
}

// Commits finds a repository's latest commit. github.Client implements it.
type Commits interface {
	LatestCommit(ctx context.Context, apiURL string, token string) (string, error)
}

// ErrScanRunning is returned by Scan when another scan is in progress.
var ErrScanRunning = errors.New("a scan is already running")

type Orchestrator struct {
	projects *watchlist.List
	commits  Commits
	deployer Deployer

	mu       sync.Mutex // guards gitToken
	gitToken string

	scanning sync.Mutex // one scan at a time
	paused   atomic.Bool
}

func New(projects *watchlist.List, commits Commits, deployer Deployer) *Orchestrator {
	return &Orchestrator{projects: projects, commits: commits, deployer: deployer}
}

func (o *Orchestrator) Pause()         { o.paused.Store(true) }
func (o *Orchestrator) Resume()        { o.paused.Store(false) }
func (o *Orchestrator) IsPaused() bool { return o.paused.Load() }

// SetGitToken sets the GitHub token used to check for commits.
func (o *Orchestrator) SetGitToken(token string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.gitToken = token
}

// HasGitToken reports whether a GitHub token has been set.
func (o *Orchestrator) HasGitToken() bool {
	return o.token() != ""
}

func (o *Orchestrator) token() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.gitToken
}

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

	var failed []string
	for _, p := range o.projects.All() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := o.check(ctx, p); err != nil {
			slog.Error("check failed", "project", p.Name, "err", err)
			failed = append(failed, p.Name)
		}
	}

	if err := o.projects.Save(); err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

// check looks up p's latest commit and deploys it if it's new. A failed
// deploy doesn't record the commit, so the next scan tries again.
func (o *Orchestrator) check(ctx context.Context, p watchlist.Project) error {
	sha, err := o.commits.LatestCommit(ctx, p.APIURL, o.token())
	if err == nil && sha != p.Commit() {
		slog.Info("new commit", "project", p.Name, "sha", short(sha))
		err = o.deployer.Deploy(ctx, p)
	}

	o.projects.Update(p.Name, func(p *watchlist.Project) {
		p.RecordCheck()
		switch {
		case err != nil:
			p.RecordError(err)
		case sha != p.Commit():
			p.RecordDeploy(sha)
		default:
			p.ClearError()
		}
	})
	return err
}

// Deploy deploys the project called name now, whether or not its commit
// changed, and records the commit it deployed.
func (o *Orchestrator) Deploy(ctx context.Context, name string) error {
	p, ok := o.projects.Find(name)
	if !ok {
		return watchlist.ErrNotFound
	}

	sha, err := o.commits.LatestCommit(ctx, p.APIURL, o.token())
	if err == nil {
		err = o.deployer.Deploy(ctx, p)
	}

	o.projects.Update(p.Name, func(p *watchlist.Project) {
		if err != nil {
			p.RecordError(err)
		} else {
			p.RecordDeploy(sha)
		}
	})
	if saveErr := o.projects.Save(); err == nil {
		err = saveErr
	}
	return err
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
