// Package watcher keeps the watchlist and checks GitHub for new commits,
// deploying a project when its commit changes.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LSariol/LightHouse/internal/models"
)

// Deployer deploys a project. *builder.Builder implements it.
type Deployer interface {
	Build(ctx context.Context, repo models.WatchedRepo) error
}

// ErrScanRunning is returned by Scan when another scan is in progress.
var ErrScanRunning = errors.New("a scan is already running")

type Watcher struct {
	http     *http.Client
	deployer Deployer
	repoPath string

	// mu guards watchList and gitToken. It's never held during network
	// calls or deploys, so the CLI stays responsive while a scan runs.
	mu        sync.Mutex
	watchList []models.WatchedRepo
	gitToken  string

	scanning sync.Mutex // one scan at a time
	paused   atomic.Bool
}

func New(httpClient *http.Client, deployer Deployer, repoPath string) *Watcher {
	return &Watcher{http: httpClient, deployer: deployer, repoPath: repoPath}
}

func (w *Watcher) Pause()         { w.paused.Store(true) }
func (w *Watcher) Resume()        { w.paused.Store(false) }
func (w *Watcher) IsPaused() bool { return w.paused.Load() }

// SetGitToken sets the GitHub token used for API requests.
func (w *Watcher) SetGitToken(token string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.gitToken = token
}

// HasGitToken reports whether a GitHub token has been set.
func (w *Watcher) HasGitToken() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gitToken != ""
}

// Run scans every interval until ctx is cancelled, skipping scans while
// paused. A scan's errors are logged; they don't stop the loop.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if !w.IsPaused() {
			if err := w.Scan(ctx); err != nil && !errors.Is(err, ErrScanRunning) && ctx.Err() == nil {
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
func (w *Watcher) Scan(ctx context.Context) error {
	if !w.scanning.TryLock() {
		return ErrScanRunning
	}
	defer w.scanning.Unlock()

	w.mu.Lock()
	token := w.gitToken
	w.mu.Unlock()

	var failed []string
	for _, repo := range w.Repos() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.check(ctx, repo, token); err != nil {
			slog.Error("check failed", "project", repo.DisplayName, "err", err)
			failed = append(failed, repo.DisplayName)
		}
	}

	if err := w.store(); err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

// check looks up repo's latest commit and deploys it if it's new.
func (w *Watcher) check(ctx context.Context, repo models.WatchedRepo, token string) error {
	sha, err := latestSHA(ctx, w.http, repo.APIURL, token)
	if err != nil {
		w.recordError(repo.DisplayName, err)
		return err
	}

	seen := repo.Stats.Updates.LastSeenCommitSha
	if seen == nil || *seen != sha {
		slog.Info("new commit", "project", repo.DisplayName, "sha", short(sha))
		if err := w.deployer.Build(ctx, repo); err != nil {
			// The commit isn't recorded, so the next scan tries again.
			w.recordError(repo.DisplayName, err)
			return err
		}
		w.update(repo.DisplayName, func(r *models.WatchedRepo) {
			*r = models.UpdateUpdateStats(*r, sha)
		})
	}

	w.update(repo.DisplayName, func(r *models.WatchedRepo) {
		*r = models.ClearErrorStats(models.UpdateQueryStats(*r))
	})
	return nil
}

// Deploy deploys a project now, whether or not its commit changed, and
// records the commit it deployed.
func (w *Watcher) Deploy(ctx context.Context, name string) error {
	repo, ok := w.Find(name)
	if !ok {
		return ErrNotFound
	}

	w.mu.Lock()
	token := w.gitToken
	w.mu.Unlock()

	sha, err := latestSHA(ctx, w.http, repo.APIURL, token)
	if err != nil {
		w.recordError(repo.DisplayName, err)
		return err
	}

	if err := w.deployer.Build(ctx, repo); err != nil {
		w.recordError(repo.DisplayName, err)
		_ = w.store() // the deploy error is the one to report
		return err
	}

	w.update(repo.DisplayName, func(r *models.WatchedRepo) {
		*r = models.ClearErrorStats(models.UpdateUpdateStats(*r, sha))
	})
	return w.store()
}

func (w *Watcher) recordError(name string, err error) {
	w.update(name, func(r *models.WatchedRepo) {
		*r = models.UpdateQueryStats(models.UpdateErrorStats(*r, err.Error()))
	})
}

// update applies fn to the project called name, if it's still watched (it
// may have been removed while a scan was running).
func (w *Watcher) update(name string, fn func(*models.WatchedRepo)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if i := w.index(name); i >= 0 {
		fn(&w.watchList[i])
	}
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
