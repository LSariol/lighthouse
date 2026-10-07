// Package orchestrator decides when projects deploy: checks, the CLI and the reconcile loop, one deploy at a time.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
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

// Deployer deploys a project, or checks it without deploying, and keeps
// Lighthouse's own updates (hand-offs). *deploy.Deployer implements it.
type Deployer interface {
	Deploy(ctx context.Context, req deploy.Request) deploy.Result
	Check(ctx context.Context, req deploy.Request) deploy.Result
	HandOffs() ([]deploy.HandOff, error)
	HandOffInProgress(project string) bool
	Forget(h deploy.HandOff) error
}

// ErrHandedOff means Lighthouse handed its own update to the helper.
var ErrHandedOff = errors.New("handed off to the update helper")

// ErrUpdating means Lighthouse is updating itself: the update helper hasn't
// finished, so Lighthouse isn't deployed again meanwhile.
var ErrUpdating = errors.New("a self-update is in progress")

// GitHub answers what's new in a repository. github.Client implements it.
type GitHub interface {
	LatestCommit(ctx context.Context, repo github.Repo, token string) (string, error)
	CheckCommit(ctx context.Context, repo github.Repo, token string, etag string) (string, string, error)
	Tags(ctx context.Context, repo github.Repo, token string, etag string) ([]github.Tag, string, error)
	ComposeFile(ctx context.Context, repo github.Repo, sha string, token string) (string, []byte, error)
	ResolveCommit(ctx context.Context, repo github.Repo, ref string, token string) (string, error)
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

	scanning sync.Mutex
	paused   atomic.Bool
	turn     turn

	recording    sync.Mutex
	handOffPoll  time.Duration
	pausedByHand atomic.Bool

	mu      sync.Mutex
	watches map[string]*watch
}

// watch is what's remembered about a project between checks. It's only in
// memory: after a restart, the first check of each project costs a request.
type watch struct {
	commitETag  string
	head        string
	settingsSHA string
	tagsETag    string
	tags        []github.Tag
	movedWarned string

	backoff     time.Duration
	nextAttempt time.Time
	downSince   time.Time
}

// New returns an Orchestrator that checks with gitToken (Lighthouse's GitHub
// token, read from Cove at startup).
func New(store projects.Store, gh GitHub, deployer Deployer, containers Containers, gitToken string) *Orchestrator {
	return &Orchestrator{store: store, github: gh, deployer: deployer, containers: containers, gitToken: gitToken,
		now: time.Now, watches: map[string]*watch{}, handOffPoll: 2 * time.Second}
}

func (o *Orchestrator) Pause()         { o.paused.Store(true) }
func (o *Orchestrator) Resume()        { o.paused.Store(false) }
func (o *Orchestrator) IsPaused() bool { return o.paused.Load() }

// ReconcileEvery is how often the reconcile loop looks for projects that are
// down.
const ReconcileEvery = time.Minute

// Run scans now and then every interval, and reconciles every ReconcileEvery,
// until ctx is cancelled.
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

// Scan checks every project once and deploys those with something new.
func (o *Orchestrator) Scan(ctx context.Context) error {
	if !o.scanning.TryLock() {
		return ErrScanRunning
	}
	defer o.scanning.Unlock()

	o.RecordHandOffs(ctx)

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
		return fmt.Errorf("checks failed for %s (each project's error is logged, and shown by \"list\")", strings.Join(failed, ", "))
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
	case err == nil && ok && o.deployer.HandOffInProgress(p.Name):
		return o.record(ctx, p.Name, nil, true)
	case err != nil:
		o.wait(w, p.Name, err)
	case !ok:
		err = nil
	case p.Broken && t.sha == p.FailingSHA:
		return o.record(ctx, p.Name, nil, false)
	default:
		slog.Info("deploying", "project", p.Name, "what", t.String())
		err = o.deploy(ctx, p, t, projects.TriggerCheck)
		if errors.Is(err, ErrHandedOff) || errors.Is(err, ErrUpdating) {
			err = nil
		}
	}
	return o.record(ctx, p.Name, err, true)
}

// latest finds what p should run next; ok is false when nothing is new.
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
		if sha == p.DeployedSHA && p.DeployedVersion == "" || sha == p.HeldSHA {
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
		return target{}, false, nil
	}
	// Only a release newer than any deployed before, so going back by hand sticks.
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

// record notes a check.
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
	if recErr != nil && !errors.Is(recErr, projects.ErrNotFound) {
		slog.Error("recording a check failed", "project", name, "err", recErr)
	}
	return err
}

// Deploy deploys a project now: ref (a tag or commit), or its newest.
func (o *Orchestrator) Deploy(ctx context.Context, name string, ref string) error {
	p, err := o.store.Get(ctx, name)
	if err != nil {
		return err
	}
	t, err := o.manualTarget(ctx, p, ref)
	if err != nil {
		return err
	}
	return o.deployByHand(ctx, p, t, ref == "")
}

// deployByHand deploys t for `deploy` or `rollback`.
func (o *Orchestrator) deployByHand(ctx context.Context, p projects.Project, t target, latest bool) error {
	if p.Stopped {
		if err := o.store.SetStopped(ctx, p.Name, false); err != nil {
			return fmt.Errorf("record that %s is no longer stopped: %w", p.Name, err)
		}
	}
	if err := o.hold(ctx, p, t, latest); err != nil {
		return err
	}
	w := o.watchOf(p.Name)
	w.backoff, w.nextAttempt = 0, time.Time{}
	return o.deploy(ctx, p, t, projects.TriggerManual)
}

// hold records what a project that deploys its branch went back from, so
// checks don't deploy it again.
func (o *Orchestrator) hold(ctx context.Context, p projects.Project, t target, latest bool) error {
	held := ""
	if !latest && p.Mode != settings.DeployReleases {
		head, err := o.github.LatestCommit(ctx, p.Repo, o.gitToken)
		if err != nil {
			return err
		}
		if head != t.sha {
			held = head
		}
	}
	if held == p.HeldSHA {
		return nil
	}
	if err := o.store.SetHeld(ctx, p.Name, held); err != nil {
		return fmt.Errorf("record the commit %s went back from: %w", p.Name, err)
	}
	if held != "" {
		slog.Info("holding a commit: checks won't deploy it again", "project", p.Name, "sha", short(held))
	}
	return nil
}

// Rollback deploys again what ran before the current version: the newest
// successful deploy of something else.
func (o *Orchestrator) Rollback(ctx context.Context, name string) (string, error) {
	p, err := o.store.Get(ctx, name)
	if err != nil {
		return "", err
	}
	history, err := o.store.History(ctx, p.Name, 200)
	if err != nil {
		return "", fmt.Errorf("read %s's history: %w", p.Name, err)
	}
	for _, d := range history {
		if d.Status == projects.StatusSucceeded && d.SHA != "" && d.SHA != p.DeployedSHA {
			t := target{sha: d.SHA, version: d.Version}
			return t.String(), o.deployByHand(ctx, p, t, false)
		}
	}
	return "", ErrNothingToRollBackTo
}

// ErrNothingToRollBackTo means a project has no earlier successful deploy.
var ErrNothingToRollBackTo = errors.New("no earlier successful deploy to go back to")

// manualTarget is what `deploy` and `check` act on: ref (a tag or a commit),
// or the newest release or commit.
func (o *Orchestrator) manualTarget(ctx context.Context, p projects.Project, ref string) (target, error) {
	if ref == "" {
		head, err := o.github.LatestCommit(ctx, p.Repo, o.gitToken)
		if err != nil {
			return target{}, err
		}
		if w := o.watchOf(p.Name); w.settingsSHA != head {
			if err := o.readSettings(ctx, &p, head); err != nil {
				return target{}, err
			}
			w.settingsSHA = head
		}
		if p.Mode != settings.DeployReleases {
			return target{sha: head}, nil
		}
	}
	tags, _, err := o.github.Tags(ctx, p.Repo, o.gitToken, "")
	if err != nil {
		return target{}, err
	}
	if ref == "" {
		newest, _, found := release.Newest(tags)
		if !found {
			return target{}, fmt.Errorf("%s has no releases yet (tags such as v1.0.0)", p.Repo)
		}
		return target{sha: newest.SHA, version: newest.Name}, nil
	}
	if tag, found := release.Find(tags, ref); found {
		return target{sha: tag.SHA, version: tag.Name}, nil
	}
	if !commitRef.MatchString(ref) {
		return target{}, fmt.Errorf("%s has no tag %q (a commit is 7 to 40 hex characters)", p.Repo, ref)
	}
	sha, err := o.github.ResolveCommit(ctx, p.Repo, ref, o.gitToken)
	var gh *github.Error
	if errors.As(err, &gh) && (gh.Status == 404 || gh.Status == 422) {
		return target{}, fmt.Errorf("%s has no tag or commit %q", p.Repo, ref)
	}
	return target{sha: sha}, err
}

// commitRef is a commit's full or short SHA.
var commitRef = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// Check dry-runs the deploy's checks on the project's newest commit.
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

// RecordHandOffs records Lighthouse's own updates once the helper has
// finished them (or given up), as any deploy is recorded.
func (o *Orchestrator) RecordHandOffs(ctx context.Context) {
	o.recording.Lock()
	defer o.recording.Unlock()
	handOffs, err := o.deployer.HandOffs()
	if err != nil {
		slog.Error("self-update results can't be read", "err", err)
		return
	}
	for _, h := range handOffs {
		d := h.Deployment()
		slog.Info("self-update finished", "project", d.Project, "sha", short(d.SHA), "status", d.Status, "err", d.Error)
		o.recordDeployment(ctx, d)
		if err := o.deployer.Forget(h); err != nil {
			slog.Error("a recorded self-update can't be removed; it may be recorded twice", "err", err)
		}
	}
}

// deploy waits for its turn, runs one deployment of p and records it.
func (o *Orchestrator) deploy(ctx context.Context, p projects.Project, t target, trigger string) error {
	if o.deployer.HandOffInProgress(p.Name) {
		return ErrUpdating
	}
	if err := o.turn.acquire(ctx, settings.Settings{Tier: p.Tier}.Order()); err != nil {
		return err
	}
	release := true
	defer func() {
		if release {
			o.turn.release()
		}
	}()

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
		Trigger: trigger,
		Token:   o.gitToken,
		Claim: func(ctx context.Context, composeProject string) error {
			err := o.store.SetComposeProject(ctx, p.Name, composeProject)
			if errors.Is(err, projects.ErrComposeProjectTaken) {
				return o.takenError(ctx, composeProject)
			}
			return err
		},
	})

	if res.HandedOff {
		// Keep the turn until replaced; awaitHandOff carries on if nothing replaces this Lighthouse.
		release = false
		if !o.IsPaused() {
			o.pausedByHand.Store(true)
			o.Pause()
		}
		slog.Info("self-update handed off: the helper replaces this Lighthouse now", "sha", short(t.sha), "helper", deploy.HelperName)
		go o.awaitHandOff(p.Name)
		return ErrHandedOff
	}

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

// awaitHandOff runs in a Lighthouse that handed its own update off.
func (o *Orchestrator) awaitHandOff(name string) {
	ticker := time.NewTicker(o.handOffPoll)
	defer ticker.Stop()
	deadline := time.Now().Add(deploy.HandOffWait + time.Minute)
	for range ticker.C {
		if o.deployer.HandOffInProgress(name) && time.Now().Before(deadline) {
			continue
		}
		slog.Info("the update helper finished without replacing this Lighthouse; recording the result and carrying on", "project", name)
		o.RecordHandOffs(context.Background())
		o.turn.release()
		if o.pausedByHand.CompareAndSwap(true, false) {
			o.Resume()
		}
		return
	}
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
