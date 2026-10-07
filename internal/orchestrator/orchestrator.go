// Package orchestrator decides when projects deploy.
//
// On a schedule it checks each project on GitHub: the newest commit on its
// default branch, or, for a project whose compose file says
// `x-lighthouse: {deploy: releases}`, its newest version tag. Requests carry
// the last ETag, so an unchanged repository costs nothing against GitHub's
// rate limit. What's new is deployed. The CLI deploys on request, and the
// reconcile loop brings back projects whose containers are gone.
//
// Deploys run one at a time, in order: data projects (sparkdb), then infra
// (Cove, cloudflared), then apps. Every attempt is recorded in the
// projects.Store. A commit that keeps failing for a reason retrying can't fix
// marks its project broken; a passing problem (GitHub, Cove, the network) is
// tried again after a growing wait, up to 30 minutes.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsariol/lighthouse/internal/deploy"
	"github.com/lsariol/lighthouse/internal/docker"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/projects"
	"github.com/lsariol/lighthouse/internal/release"
	"github.com/lsariol/lighthouse/internal/settings"
)

// Deployer deploys a project, or checks it without deploying.
// *deploy.Deployer implements it.
type Deployer interface {
	Deploy(ctx context.Context, req deploy.Request) deploy.Result
	Check(ctx context.Context, req deploy.Request) deploy.Result
}

// GitHub answers what's new in a repository. github.Client implements it.
type GitHub interface {
	LatestCommit(ctx context.Context, repo github.Repo, token string) (string, error)
	CheckCommit(ctx context.Context, repo github.Repo, token string, etag string) (string, string, error)
	Tags(ctx context.Context, repo github.Repo, token string, etag string) ([]github.Tag, string, error)
	ComposeFile(ctx context.Context, repo github.Repo, sha string, token string) (string, []byte, error)
}

// Containers tells the reconcile loop what's running. *docker.Client
// implements it.
type Containers interface {
	ProjectContainers(ctx context.Context, project string) ([]docker.Container, error)
	Inspect(ctx context.Context, id string) (docker.Detail, error)
}

// ErrScanRunning is returned by Scan when another scan is in progress.
var ErrScanRunning = errors.New("a scan is already running")

// Backoff limits: the first wait after a passing failure, and the longest.
const (
	minBackoff = time.Minute
	maxBackoff = 30 * time.Minute
)

type Orchestrator struct {
	store      projects.Store
	github     GitHub
	deployer   Deployer
	containers Containers
	gitToken   string
	now        func() time.Time

	scanning sync.Mutex // one scan at a time
	paused   atomic.Bool
	turn     turn // one deploy at a time, in order

	mu      sync.Mutex
	watches map[string]*watch // by lowercased project name
}

// watch is what's remembered about a project between checks. It's only in
// memory: after a restart, the first check of each project costs a request.
type watch struct {
	commitETag  string
	head        string // the default branch's newest commit, as last seen
	settingsSHA string // the commit the settings were read from
	tagsETag    string
	tags        []github.Tag
	movedWarned string // the release whose moved tag was already warned about

	backoff     time.Duration
	nextAttempt time.Time // no automatic deploy before then
	downSince   time.Time // when the reconcile loop first saw it down
}

// New returns an Orchestrator that checks with gitToken (Lighthouse's GitHub
// token, read from Cove at startup). containers may be nil: then there's no
// reconcile loop.
func New(store projects.Store, gh GitHub, deployer Deployer, containers Containers, gitToken string) *Orchestrator {
	return &Orchestrator{store: store, github: gh, deployer: deployer, containers: containers, gitToken: gitToken,
		now: time.Now, watches: map[string]*watch{}}
}

func (o *Orchestrator) Pause()         { o.paused.Store(true) }
func (o *Orchestrator) Resume()        { o.paused.Store(false) }
func (o *Orchestrator) IsPaused() bool { return o.paused.Load() }

// ReconcileEvery is how often the reconcile loop looks for projects that are
// down.
const ReconcileEvery = time.Minute

// Run scans now and then every interval, and reconciles every
// ReconcileEvery, until ctx is cancelled. Both are skipped while paused.
// Errors are logged; they don't stop the loop.
func (o *Orchestrator) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastReconcile := o.now()

	for {
		if !o.IsPaused() {
			if err := o.Scan(ctx); err != nil && !errors.Is(err, ErrScanRunning) && ctx.Err() == nil {
				slog.Error("scan finished with errors", "err", err)
			}
			if o.now().Sub(lastReconcile) >= ReconcileEvery {
				lastReconcile = o.now()
				if err := o.Reconcile(ctx); err != nil && ctx.Err() == nil {
					slog.Error("reconcile finished with errors", "err", err)
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (o *Orchestrator) watchOf(name string) *watch {
	o.mu.Lock()
	defer o.mu.Unlock()
	key := strings.ToLower(name)
	w, ok := o.watches[key]
	if !ok {
		w = &watch{}
		o.watches[key] = w
	}
	return w
}

// byOrder sorts projects data first, then infra, then apps, by name within.
func byOrder(list []projects.Project) {
	sort.SliceStable(list, func(i, j int) bool {
		return settings.Settings{Tier: list[i].Tier}.Order() < settings.Settings{Tier: list[j].Tier}.Order()
	})
}

// Scan checks every project once and deploys those with something new. A
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
	byOrder(list)

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

// target is what to deploy: a commit, and the release it is, if it is one.
type target struct {
	sha     string
	version string
}

func (t target) String() string {
	if t.version != "" {
		return t.version + " (" + short(t.sha) + ")"
	}
	return short(t.sha)
}

// check looks at p on GitHub and deploys what's new, unless p is stopped,
// waiting after a passing failure, or broken by that very commit.
func (o *Orchestrator) check(ctx context.Context, p projects.Project) error {
	w := o.watchOf(p.Name)
	if p.Stopped || o.now().Before(w.nextAttempt) {
		return nil
	}

	t, ok, err := o.latest(ctx, &p, w)
	switch {
	case err != nil:
		o.wait(w, p.Name, err)
	case !ok:
		err = nil
	case p.Broken && t.sha == p.FailingSHA:
		// Not tried again; the error stays the one that broke it.
		return o.record(ctx, p.Name, nil, false)
	default:
		slog.Info("deploying", "project", p.Name, "what", t.String())
		err = o.deploy(ctx, p, t, projects.TriggerCheck)
	}
	return o.record(ctx, p.Name, err, true)
}

// latest finds what p should run: the default branch's newest commit, or, in
// release mode, the newest release if it's newer than the deployed one. ok is
// false when there's nothing new. It refreshes p's settings when the branch
// moved, since they decide which.
func (o *Orchestrator) latest(ctx context.Context, p *projects.Project, w *watch) (target, bool, error) {
	sha, etag, err := o.github.CheckCommit(ctx, p.Repo, o.gitToken, w.commitETag)
	switch {
	case errors.Is(err, github.ErrNotModified):
		sha = w.head
	case err != nil:
		return target{}, false, err
	default:
		w.head, w.commitETag = sha, etag
	}

	if w.settingsSHA != sha {
		if err := o.readSettings(ctx, p, sha); err != nil {
			return target{}, false, err
		}
		w.settingsSHA = sha
	}

	if p.Mode != settings.DeployReleases {
		if sha == p.DeployedSHA && p.DeployedVersion == "" {
			return target{}, false, nil
		}
		return target{sha: sha}, true, nil
	}

	tags, etag, err := o.github.Tags(ctx, p.Repo, o.gitToken, w.tagsETag)
	switch {
	case errors.Is(err, github.ErrNotModified):
		tags = w.tags
	case err != nil:
		return target{}, false, err
	default:
		w.tags, w.tagsETag = tags, etag
	}
	newest, v, found := release.Newest(tags)
	if !found {
		return target{}, false, nil // no release yet
	}
	// Only a release newer than any deployed before: going back to an older
	// one by hand isn't undone here.
	if highest, ok := release.Parse(release.Higher(p.HighestVersion, p.DeployedVersion)); ok && !v.Newer(highest) {
		if newest.Name == p.DeployedVersion && newest.SHA != p.DeployedSHA && w.movedWarned != newest.Name {
			w.movedWarned = newest.Name
			slog.Warn("a deployed release's tag moved to another commit; it isn't deployed again (deploy it by hand if that's wanted)",
				"project", p.Name, "version", newest.Name, "deployed", short(p.DeployedSHA), "tag", short(newest.SHA))
		}
		return target{}, false, nil
	}
	return target{sha: newest.SHA, version: newest.Name}, true, nil
}

// readSettings reads the x-lighthouse settings from the compose file at sha
// and records them.
func (o *Orchestrator) readSettings(ctx context.Context, p *projects.Project, sha string) error {
	name, text, err := o.github.ComposeFile(ctx, p.Repo, sha, o.gitToken)
	if err != nil {
		return err
	}
	s, err := settings.FromYAML(text)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if s.Deploy != p.Mode || s.Tier != p.Tier {
		if err := o.store.SetSettings(ctx, p.Name, s.Deploy, s.Tier); err != nil {
			return err
		}
		slog.Info("settings", "project", p.Name, "deploy", s.Deploy, "tier", s.Tier)
		p.Mode, p.Tier = s.Deploy, s.Tier
	}
	return nil
}

// wait makes automatic deploys of a project wait after a passing failure:
// a minute, then twice as long each time, up to 30 minutes.
func (o *Orchestrator) wait(w *watch, name string, err error) {
	if !transientErr(err) {
		w.backoff, w.nextAttempt = 0, time.Time{}
		return
	}
	w.backoff = min(max(2*w.backoff, minBackoff), maxBackoff)
	w.nextAttempt = o.now().Add(w.backoff)
	slog.Info("trying again later", "project", name, "in", w.backoff.String())
}

// transient is a deploy that failed for a passing reason.
type transient struct{ err error }

func (t transient) Error() string { return t.err.Error() }
func (t transient) Unwrap() error { return t.err }

func transientErr(err error) bool {
	var t transient
	if errors.As(err, &t) {
		return true
	}
	var gh *github.Error
	return errors.As(err, &gh) && gh.Temporary()
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

// Deploy deploys the project called name now, even if it's what's deployed
// or what broke it: the given version (a tag), or else its newest release
// (release mode) or the newest commit on its branch. A stopped project is
// started again.
func (o *Orchestrator) Deploy(ctx context.Context, name string, version string) error {
	p, err := o.store.Get(ctx, name)
	if err != nil {
		return err
	}
	t, err := o.manualTarget(ctx, p, version)
	if err != nil {
		return err
	}
	if p.Stopped {
		if err := o.store.SetStopped(ctx, p.Name, false); err != nil {
			return err
		}
	}
	w := o.watchOf(p.Name)
	w.backoff, w.nextAttempt = 0, time.Time{}
	return o.deploy(ctx, p, t, projects.TriggerManual)
}

// manualTarget is what `deploy` and `check` act on.
func (o *Orchestrator) manualTarget(ctx context.Context, p projects.Project, version string) (target, error) {
	if version == "" && p.Mode != settings.DeployReleases {
		sha, err := o.github.LatestCommit(ctx, p.Repo, o.gitToken)
		return target{sha: sha}, err
	}
	tags, _, err := o.github.Tags(ctx, p.Repo, o.gitToken, "")
	if err != nil {
		return target{}, err
	}
	if version == "" {
		newest, _, found := release.Newest(tags)
		if !found {
			return target{}, fmt.Errorf("%s has no releases yet (tags such as v1.0.0)", p.Repo)
		}
		return target{sha: newest.SHA, version: newest.Name}, nil
	}
	tag, found := release.Find(tags, version)
	if !found {
		return target{}, fmt.Errorf("%s has no tag %q", p.Repo, version)
	}
	return target{sha: tag.SHA, version: tag.Name}, nil
}

// Check runs the project's newest commit (or release) through the deploy's
// checks (fetch, inspect, the rules, the test stage) without deploying or
// recording it. It returns the commit it checked.
func (o *Orchestrator) Check(ctx context.Context, name string) (deploy.Result, string, error) {
	p, err := o.store.Get(ctx, name)
	if err != nil {
		return deploy.Result{}, "", err
	}
	t, err := o.manualTarget(ctx, p, "")
	if err != nil {
		return deploy.Result{}, "", err
	}
	res := o.deployer.Check(ctx, deploy.Request{
		Project: p,
		SHA:     t.sha,
		Version: t.version,
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
	return res, t.sha, nil
}

// Retry clears a project's failures (and broken state) and deploys it now.
func (o *Orchestrator) Retry(ctx context.Context, name string) error {
	if err := o.store.ClearFailures(ctx, name); err != nil {
		return err
	}
	return o.Deploy(ctx, name, "")
}

// deploy waits for its turn, runs one deployment of p and records it.
func (o *Orchestrator) deploy(ctx context.Context, p projects.Project, t target, trigger string) error {
	if err := o.turn.acquire(ctx, settings.Settings{Tier: p.Tier}.Order()); err != nil {
		return err
	}
	defer o.turn.release()

	// What's queued may be stale: the project may have been removed or
	// changed while it waited.
	if now, err := o.store.Get(ctx, p.Name); err == nil {
		p = now
	} else if errors.Is(err, projects.ErrNotFound) {
		return nil
	}

	started := o.now()
	res := o.deployer.Deploy(ctx, deploy.Request{
		Project: p,
		SHA:     t.sha,
		Version: t.version,
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
		SHA:         t.sha,
		Version:     t.version,
		Trigger:     trigger,
		Status:      res.Status,
		FailureKind: res.FailureKind,
		FailedStep:  res.FailedStep,
		StartedAt:   started,
		FinishedAt:  o.now(),
		Steps:       res.Steps,
	}
	if res.Err != nil {
		d.Error = projects.ErrorText(res.Err)
	}
	o.recordDeployment(ctx, d)
	if s := res.Settings; s.Tier != "" && (s.Deploy != p.Mode || s.Tier != p.Tier) {
		o.store.SetSettings(context.WithoutCancel(ctx), p.Name, s.Deploy, s.Tier)
	}

	w := o.watchOf(p.Name)
	if res.Err == nil {
		w.backoff, w.nextAttempt = 0, time.Time{}
		return nil
	}
	if res.FailureKind == projects.FailureTransient {
		err := transient{res.Err}
		o.wait(w, p.Name, err)
		return err
	}
	w.backoff, w.nextAttempt = 0, time.Time{}
	if now, err := o.store.Get(context.WithoutCancel(ctx), p.Name); err == nil && now.Broken {
		slog.Warn("project is broken: its latest commit isn't tried again until a new one or `retry`",
			"project", p.Name, "sha", short(t.sha), "failures", now.FailureCount)
		return fmt.Errorf("%w after %d failed deploys of %s: %v", ErrBroken, now.FailureCount, t, res.Err)
	}
	return res.Err
}

// recordRetries is how long recording a deployment keeps trying while the
// database is away: deploying sparkdb takes Lighthouse's database down too.
const recordRetries = 2 * time.Minute

func (o *Orchestrator) recordDeployment(ctx context.Context, d projects.Deployment) {
	// A context cancelled by shutdown mustn't lose the record.
	ctx = context.WithoutCancel(ctx)
	deadline := time.Now().Add(recordRetries)
	for {
		err := o.store.RecordDeployment(ctx, d)
		if err == nil || errors.Is(err, projects.ErrNotFound) {
			return
		}
		if time.Now().After(deadline) {
			slog.Error("recording a deployment failed", "project", d.Project, "err", err)
			return
		}
		slog.Warn("recording a deployment failed; trying again", "project", d.Project, "err", err)
		time.Sleep(2 * time.Second)
	}
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
