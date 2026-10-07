// Package deploy deploys one commit of a project, building first and
// swapping last: everything that can fail happens while the running version
// keeps serving, and if the new version doesn't come up healthy, the previous
// one is put back.
//
//	fetch    download the exact commit through the GitHub API and unpack it
//	inspect  read the compose file: its project name, services, variables
//	check    the deploy rules (internal/policy): nothing that reaches outside
//	         the project, its own secrets only, no name taken on spark
//	test     the Dockerfile's test stage, if it has one
//	secrets  fetch every ${KEY} without a default from Cove, in one request
//	build    build the images; tag the running ones for rollback first
//	backup   for a project with x-lighthouse backup: postgres, a pg_dumpall of
//	         the running database; and the previous version's secrets, so a
//	         rollback doesn't need Cove (whose database may be what's swapped)
//	swap     docker compose up: the only moment of downtime
//	verify   wait until every service is healthy (or stable); else roll back
//	cleanup  old deploy folders, old rollback tags, unused images and cache
//
// Every step's output is kept (the tail, with secret values hidden) and
// returned in the Result, and also written to the log as it happens.
//
// Check runs the steps up to test as a dry run, changing nothing.
package deploy

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lsariol/coveclient"
	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/docker"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/policy"
	"github.com/lsariol/lighthouse/internal/projects"
	"github.com/lsariol/lighthouse/internal/settings"
)

// Compose runs docker compose. compose.Runner implements it.
type Compose interface {
	Inspect(ctx context.Context, dir string) (compose.Project, error)
	Variables(ctx context.Context, dir string) ([]compose.Variable, error)
	Build(ctx context.Context, dir string, project string, env []string, out io.Writer) error
	BuildStage(ctx context.Context, buildContext string, dockerfile string, target string, out io.Writer) error
	Exec(ctx context.Context, container string, command []string, stdout io.Writer) error
	RunDetached(ctx context.Context, name string, image string, volumes []string, command []string) error
	RemoveContainer(ctx context.Context, name string) error
	Up(ctx context.Context, dir string, project string, env []string, out io.Writer) error
}

// Docker reads containers and manages images. *docker.Client implements it.
type Docker interface {
	ProjectContainers(ctx context.Context, project string) ([]docker.Container, error)
	NetworkNames(ctx context.Context, network string) ([]docker.NetworkName, error)
	Inspect(ctx context.Context, id string) (docker.Detail, error)
	Tag(ctx context.Context, source string, target string) error
	Tags(ctx context.Context, repository string, prefix string) ([]string, error)
	Untag(ctx context.Context, ref string) error
	PruneDangling(ctx context.Context) error
	PruneBuildCache(ctx context.Context, unused time.Duration) error
}

// Source downloads a commit. github.Client implements it.
type Source interface {
	Archive(ctx context.Context, repo github.Repo, sha string, token string) (io.ReadCloser, error)
}

// Secrets fetches secrets. *coveclient.Client implements it.
type Secrets interface {
	GetSecretsContext(ctx context.Context, keys ...string) (map[string]string, error)
}

// Timeouts limit each step.
type Timeouts struct {
	Fetch  time.Duration // download and unpack
	Build  time.Duration
	Swap   time.Duration // docker compose up
	Verify time.Duration // how long services may take to become healthy
	Stable time.Duration // how long a service without a healthcheck must stay up
}

// DefaultTimeouts are used for any Timeouts field left zero.
var DefaultTimeouts = Timeouts{
	Fetch:  2 * time.Minute,
	Build:  20 * time.Minute,
	Swap:   5 * time.Minute,
	Verify: 2 * time.Minute,
	Stable: 10 * time.Second,
}

// Options configure a Deployer.
type Options struct {
	// Root holds a folder per compose project, with a folder per deployed
	// commit. In Docker it must be mounted at the same path on the host,
	// so a relative bind mount in a project's compose file resolves to the
	// same files for Compose (in Lighthouse's container) and Docker (on the
	// host).
	Root string
	// Storage is the root of the projects' data folders: a project may
	// mount host paths from Storage/<compose project>/ (and its own files).
	Storage string
	// Policy holds the exceptions to the deploy rules (policy.json).
	Policy policy.Policy
	// Self is Lighthouse's own compose project, "" when it doesn't run in
	// Docker. Its deploys hand off to the update helper (selfupdate.go).
	Self string
	// Backups is where database backups go: <Backups>/<compose project>/.
	// The newest KeepBackups of each project are kept.
	Backups  string
	Timeouts Timeouts
	// Log receives every step's output as it happens (secret values hidden).
	// Nothing if nil.
	Log io.Writer
	// Poll is how often verify looks at the containers; 2 s if zero.
	Poll time.Duration
}

// Deployer runs deploys, one at a time.
type Deployer struct {
	compose  Compose
	docker   Docker
	source   Source
	secrets  Secrets
	root     string
	storage  string
	backups  string
	self     string
	policy   policy.Policy
	timeouts Timeouts
	log      io.Writer
	poll     time.Duration

	mu             sync.Mutex
	lastCachePrune time.Time
}

func New(c Compose, d Docker, s Source, sec Secrets, opts Options) *Deployer {
	t := opts.Timeouts
	for _, f := range []struct {
		v *time.Duration
		d time.Duration
	}{
		{&t.Fetch, DefaultTimeouts.Fetch}, {&t.Build, DefaultTimeouts.Build}, {&t.Swap, DefaultTimeouts.Swap},
		{&t.Verify, DefaultTimeouts.Verify}, {&t.Stable, DefaultTimeouts.Stable},
	} {
		if *f.v == 0 {
			*f.v = f.d
		}
	}
	log := opts.Log
	if log == nil {
		log = io.Discard
	}
	poll := opts.Poll
	if poll == 0 {
		poll = 2 * time.Second
	}
	return &Deployer{compose: c, docker: d, source: s, secrets: sec, root: opts.Root, storage: opts.Storage, backups: opts.Backups, self: opts.Self, policy: opts.Policy,
		timeouts: t, log: log, poll: poll}
}

// Request is one deploy.
type Request struct {
	Project projects.Project
	SHA     string // the commit to deploy
	Version string // the release it is (v1.2.3), or "" for a commit on the branch
	Trigger string // what started it (projects.Trigger*), kept with a self-update's hand-off
	Token   string // GitHub token, for the download

	// Claim is called with the compose project the compose file names, before
	// anything changes. An error (e.g. another project has it) stops the
	// deploy. The orchestrator records it in the store (for a Check, it only
	// looks).
	Claim func(ctx context.Context, composeProject string) error
}

// Result is how a deploy went.
type Result struct {
	Status         string // projects.StatusSucceeded, StatusFailed or StatusRolledBack
	FailureKind    string // projects.FailureTransient or FailurePermanent, for a failure
	FailedStep     string
	ComposeProject string // as the compose file names it, once inspected
	// Settings are the commit's x-lighthouse settings, once inspected.
	Settings settings.Settings
	// HandedOff means this was Lighthouse's own deploy, now in the update
	// helper's hands: nothing is recorded yet (HandOffs has it later), and
	// this Lighthouse is about to be replaced.
	HandedOff bool
	Steps     []projects.Step
	Err       error // nil on success
}

// The steps, in order.
const (
	StepFetch   = "fetch"
	StepInspect = "inspect"
	StepCheck   = "check"
	StepTest    = "test"
	StepSecrets = "secrets"
	StepBuild   = "build"
	StepBackup  = "backup"
	StepSwap    = "swap"
	StepVerify  = "verify"
	StepCleanup = "cleanup"
)

var stepOrder = []string{StepFetch, StepInspect, StepCheck, StepTest, StepSecrets, StepBuild, StepBackup, StepSwap, StepVerify, StepCleanup}

// checkOrder are the steps Check runs.
var checkOrder = stepOrder[:4]

// failure is a step's error, with whether retrying later may help.
type failure struct {
	kind string
	err  error
}

func transient(err error) *failure { return &failure{projects.FailureTransient, err} }
func permanent(err error) *failure { return &failure{projects.FailurePermanent, err} }

// run is one deploy in progress.
type run struct {
	d        *Deployer
	req      Request
	log      *slog.Logger
	steps    []projects.Step
	scrub    *scrubber // hides secret values in output, once they're known
	stepLog  *tail
	previous map[string]string // service → image ID running before the swap
	prevEnv  []string          // the previous version's secrets, fetched before the swap
	settings settings.Settings // the commit's x-lighthouse settings
	started  time.Time
	// fallbackDir is where a rollback runs when the previous version's
	// folder isn't kept (the update helper: the new folder, with the
	// previous images put back).
	fallbackDir string
	dry         bool // Check: change nothing, stop after the test step
}

// Deploy deploys req.Project at req.SHA. A second call waits for the first.
func (d *Deployer) Deploy(ctx context.Context, req Request) Result {
	d.mu.Lock()
	defer d.mu.Unlock()

	r := &run{d: d, req: req, log: slog.With("project", req.Project.Name, "sha", short(req.SHA)), started: time.Now()}
	r.log.Info("deploy started")
	res := r.deploy(ctx)
	res.Steps = r.steps

	switch {
	case res.HandedOff:
		r.log.Info("deploy handed off to the update helper", "helper", HelperName)
	case res.Status == projects.StatusSucceeded:
		r.log.Info("deploy finished")
	default:
		r.log.Error("deploy failed", "status", res.Status, "step", res.FailedStep, "kind", res.FailureKind, "err", res.Err)
	}
	return res
}

// Check runs a deploy's first steps (fetch, inspect, check, test) as a dry
// run: nothing is claimed, moved into place, built or started, and the
// download is deleted afterwards. A second call waits for a running deploy.
func (d *Deployer) Check(ctx context.Context, req Request) Result {
	d.mu.Lock()
	defer d.mu.Unlock()

	r := &run{d: d, req: req, dry: true, log: slog.With("project", req.Project.Name, "sha", short(req.SHA), "check", true), started: time.Now()}
	res := r.deploy(ctx)
	res.Steps = r.steps
	return res
}

func (r *run) deploy(ctx context.Context) Result {
	var res Result
	fail := func(step string, f *failure) Result {
		res.Status = projects.StatusFailed
		res.FailedStep = step
		res.FailureKind = f.kind
		res.Err = fmt.Errorf("%s: %w", step, f.err)
		r.skipRemaining(step)
		return res
	}

	incoming := filepath.Join(r.d.root, ".incoming", short12(r.req.SHA))
	if r.dry {
		incoming = filepath.Join(r.d.root, ".check", short12(r.req.SHA))
		defer os.RemoveAll(incoming)
	}
	if f := r.step(ctx, StepFetch, func(ctx context.Context, out io.Writer) *failure {
		return r.fetch(ctx, incoming, out)
	}); f != nil {
		os.RemoveAll(incoming)
		return fail(StepFetch, f)
	}

	var project compose.Project
	var dir string
	var keys []string
	if f := r.step(ctx, StepInspect, func(ctx context.Context, out io.Writer) *failure {
		var f *failure
		project, dir, keys, f = r.inspect(ctx, incoming, out)
		return f
	}); f != nil {
		os.RemoveAll(incoming)
		return fail(StepInspect, f)
	}
	res.ComposeProject = project.Name
	res.Settings = r.settings

	if f := r.step(ctx, StepCheck, func(ctx context.Context, out io.Writer) *failure {
		return r.check(ctx, project, dir, keys, out)
	}); f != nil {
		r.removeUnlessCurrent(dir)
		return fail(StepCheck, f)
	}

	if f := r.step(ctx, StepTest, func(ctx context.Context, out io.Writer) *failure {
		return r.test(ctx, project, out)
	}); f != nil {
		r.removeUnlessCurrent(dir)
		return fail(StepTest, f)
	}

	if r.dry {
		res.Status = projects.StatusSucceeded
		return res
	}

	var env []string
	if f := r.step(ctx, StepSecrets, func(ctx context.Context, out io.Writer) *failure {
		var f *failure
		env, f = r.fetchSecrets(ctx, keys, out)
		return f
	}); f != nil {
		r.removeUnlessCurrent(dir)
		return fail(StepSecrets, f)
	}
	secrets := len(env)
	env = append(env, deployVars(r.req.SHA, r.req.Version)...)

	if f := r.step(ctx, StepBuild, func(ctx context.Context, out io.Writer) *failure {
		return r.build(ctx, dir, project, env, out)
	}); f != nil {
		r.removeUnlessCurrent(dir)
		return fail(StepBuild, f)
	}

	if f := r.step(ctx, StepBackup, func(ctx context.Context, out io.Writer) *failure {
		return r.backup(ctx, project, out)
	}); f != nil {
		r.removeUnlessCurrent(dir)
		return fail(StepBackup, f)
	}

	if r.d.self != "" && project.Name == r.d.self {
		if f := r.step(ctx, StepHandOff, func(ctx context.Context, out io.Writer) *failure {
			return r.handOff(ctx, project, dir, secrets, out)
		}); f != nil {
			r.removeUnlessCurrent(dir)
			return fail(StepHandOff, f)
		}
		res.HandedOff = true
		return res
	}

	// From here the running version is replaced: shutting Lighthouse down
	// mustn't cut a swap, a check or a rollback off halfway.
	ctx = context.WithoutCancel(ctx)

	if f := r.step(ctx, StepSwap, func(ctx context.Context, out io.Writer) *failure {
		if err := r.d.compose.Up(ctx, dir, project.Name, env, out); err != nil {
			return permanent(err)
		}
		return nil
	}); f != nil {
		res = fail(StepSwap, f)
		return r.rollBack(ctx, res, project, dir)
	}

	if f := r.step(ctx, StepVerify, func(ctx context.Context, out io.Writer) *failure {
		return r.verify(ctx, project, out)
	}); f != nil {
		res = fail(StepVerify, f)
		return r.rollBack(ctx, res, project, dir)
	}

	r.step(ctx, StepCleanup, func(ctx context.Context, out io.Writer) *failure {
		r.cleanup(ctx, project, dir, out)
		return nil
	})
	res.Status = projects.StatusSucceeded
	return res
}

// step runs fn as the named step: with its timeout, its output kept (and
// scrubbed) and written to the log, and recorded in r.steps.
func (r *run) step(ctx context.Context, name string, fn func(ctx context.Context, out io.Writer) *failure) *failure {
	ctx, cancel := context.WithTimeout(ctx, r.d.timeout(name))
	defer cancel()

	r.stepLog = newTail(projects.MaxLogLength)
	prefixed := &prefixWriter{w: r.d.log, prefix: "[" + r.req.Project.Name + " " + name + "] "}
	if r.scrub == nil {
		r.scrub = newScrubber(nil)
	}
	r.scrub.out = []io.Writer{r.stepLog, prefixed}

	st := projects.Step{Name: name, StartedAt: time.Now()}
	f := fn(ctx, r.scrub)
	if f != nil && errors.Is(f.err, context.DeadlineExceeded) {
		f.err = fmt.Errorf("took longer than %s", r.d.timeout(name))
	}
	if f != nil {
		fmt.Fprintf(r.scrub, "✗ %v\n", f.err)
	}
	r.scrub.Flush()

	st.FinishedAt = time.Now()
	st.Log = r.stepLog.String()
	st.Status = projects.StepSucceeded
	if f != nil {
		st.Status = projects.StepFailed
		f.err = errors.New(r.scrub.scrub(f.err.Error()))
	}
	r.steps = append(r.steps, st)
	return f
}

// skipRemaining records every step after failed as skipped, except the
// rollback's own record.
func (r *run) skipRemaining(failed string) {
	order := stepOrder
	if r.dry {
		order = checkOrder
	}
	after := false
	for _, name := range order {
		if after {
			now := time.Now()
			r.steps = append(r.steps, projects.Step{Name: name, Status: projects.StepSkipped, StartedAt: now, FinishedAt: now})
		}
		if name == failed {
			after = true
		}
	}
}

func (d *Deployer) timeout(step string) time.Duration {
	switch step {
	case StepFetch:
		return d.timeouts.Fetch
	case StepBuild, StepTest:
		return d.timeouts.Build
	case StepSwap:
		return d.timeouts.Swap
	case StepVerify:
		return d.timeouts.Verify + d.timeouts.Stable + time.Minute
	default:
		return 2 * time.Minute
	}
}

// fetch downloads the commit and unpacks it into dir.
func (r *run) fetch(ctx context.Context, dir string, out io.Writer) *failure {
	if err := os.RemoveAll(dir); err != nil {
		return transient(err)
	}
	fmt.Fprintf(out, "downloading %s at %s\n", r.req.Project.Repo, r.req.SHA)
	archive, err := r.d.source.Archive(ctx, r.req.Project.Repo, r.req.SHA, r.req.Token)
	if err != nil {
		var gh *github.Error
		if errors.As(err, &gh) && !gh.Temporary() {
			return permanent(err)
		}
		return transient(err)
	}
	defer archive.Close()

	if err := extract(archive, dir); err != nil {
		if ctx.Err() != nil {
			return transient(fmt.Errorf("download: %w", ctx.Err()))
		}
		return permanent(err)
	}
	return nil
}

// composeName is how Compose normalizes a project name.
var composeNameInvalid = regexp.MustCompile(`[^a-z0-9_-]`)

func normalizeName(s string) string {
	s = composeNameInvalid.ReplaceAllString(strings.ToLower(s), "")
	return strings.TrimLeft(s, "_-")
}

// inspect reads the compose file, settles the compose project's name (the
// file's `name:`, else the repository's), claims it, moves the files to
// <root>/<name>/<commit>, and lists the secrets to fetch.
func (r *run) inspect(ctx context.Context, incoming string, out io.Writer) (compose.Project, string, []string, *failure) {
	project, err := r.d.compose.Inspect(ctx, incoming)
	if err != nil {
		return project, "", nil, permanent(fmt.Errorf("the compose file can't be read: %w", err))
	}

	// Without `name:`, Compose names the project after its folder, which is
	// the commit: then the repository's name is used instead.
	if project.Name == filepath.Base(incoming) {
		project.Name = normalizeName(r.req.Project.Repo.Name)
		fmt.Fprintf(out, "the compose file has no name:, so the compose project is %q (the repository's name)\n", project.Name)
	}
	if project.Name == "" {
		return project, "", nil, permanent(errors.New("the compose project needs a name: set `name:` in the compose file"))
	}
	fmt.Fprintf(out, "compose project %q, services: %s\n", project.Name, serviceNames(project))
	if old := r.req.Project.ComposeProject; old != "" && old != project.Name {
		fmt.Fprintf(out, "! the compose project was %q: its containers keep running until removed (docker compose -p %s down)\n", old, old)
	}

	if r.req.Claim != nil {
		if err := r.req.Claim(ctx, project.Name); err != nil {
			return project, "", nil, permanent(err)
		}
	}

	dir := incoming
	if !r.dry {
		dir = filepath.Join(r.d.root, project.Name, short12(r.req.SHA))
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return project, "", nil, transient(err)
		}
		if dir != r.currentDir(project.Name) {
			os.RemoveAll(dir)
		}
		if err := os.Rename(incoming, dir); err != nil {
			if _, statErr := os.Stat(dir); statErr != nil {
				return project, "", nil, transient(fmt.Errorf("move the files into place: %w", err))
			}
			// The same commit is already unpacked there (it's the one running):
			// use it as it is.
			os.RemoveAll(incoming)
		}

		// The config's paths are absolute: read it again where the files are
		// now, for the checks and the build.
		moved, err := r.d.compose.Inspect(ctx, dir)
		if err != nil {
			return project, dir, nil, permanent(fmt.Errorf("the compose file can't be read: %w", err))
		}
		project.Services, project.Config = moved.Services, moved.Config
	}

	r.settings, err = settings.FromConfig(project.Config)
	if err != nil {
		return project, dir, nil, permanent(err)
	}
	if r.settings != settings.Default {
		fmt.Fprintf(out, "x-lighthouse: deploy %s, tier %s, backup %s\n", r.settings.Deploy, r.settings.Tier, orNone(r.settings.Backup))
	}

	vars, err := r.d.compose.Variables(ctx, dir)
	if err != nil {
		return project, dir, nil, permanent(fmt.Errorf("the compose file's variables can't be read: %w", err))
	}
	var keys []string
	for _, v := range vars {
		if v.Name == DeployCommitVar || v.Name == DeployVersionVar {
			continue // given by Lighthouse, not a secret
		}
		if v.Default != "" {
			// Only secrets belong in ${...}; a default means it's a setting.
			fmt.Fprintf(out, "! ${%s} has a default (%q), so it isn't fetched from Cove; write plain settings out as values\n", v.Name, v.Default)
			continue
		}
		keys = append(keys, v.Name)
	}
	return project, dir, keys, nil
}

// check applies the deploy rules (internal/policy) to the compose file, with
// the exceptions in policy.json.
func (r *run) check(ctx context.Context, project compose.Project, dir string, keys []string, out io.Writer) *failure {
	names, err := r.d.docker.NetworkNames(ctx, policy.SharedNetwork)
	if err != nil {
		return transient(err)
	}
	taken := map[string]string{}
	for _, n := range names {
		if n.Project != project.Name {
			taken[n.Name] = n.Container
		}
	}

	findings, err := policy.Check(policy.Input{
		Project: project.Name, Config: project.Config, Dir: dir, Storage: r.d.storage, Secrets: keys, Taken: taken,
	})
	if err != nil {
		return permanent(err)
	}
	refused := r.d.policy.Apply(project.Name, findings)
	for _, f := range findings {
		fmt.Fprintln(out, f)
	}
	if len(refused) > 0 {
		return permanent(errors.New(policy.Summary(refused)))
	}
	fmt.Fprintln(out, "the compose file follows the deploy rules")
	return nil
}

// testStage finds a Dockerfile stage named test: FROM <image> AS test.
var testStage = regexp.MustCompile(`(?im)^\s*FROM\s+.+\s+AS\s+test\s*$`)

// test builds the test stage of every Dockerfile that has one. It gets
// nothing from the compose file (no secrets, no build arguments), and a
// failure stops the deploy before anything else happens.
func (r *run) test(ctx context.Context, project compose.Project, out io.Writer) *failure {
	seen := map[string]bool{}
	ran := 0
	for _, s := range project.Services {
		if !s.Build || s.Dockerfile == "" || seen[s.Dockerfile] {
			continue
		}
		seen[s.Dockerfile] = true
		data, err := os.ReadFile(s.Dockerfile)
		if err != nil || !testStage.Match(data) {
			continue
		}
		fmt.Fprintf(out, "running the test stage of %s's Dockerfile\n", s.Name)
		if err := r.d.compose.BuildStage(ctx, s.Context, s.Dockerfile, "test", out); err != nil {
			return permanent(fmt.Errorf("%s's tests failed: %w", s.Name, err))
		}
		ran++
	}
	if ran == 0 {
		fmt.Fprintln(out, "no test stage (a Dockerfile stage named test, FROM ... AS test, runs here before every deploy)")
	}
	return nil
}

// fetchSecrets gets the values of keys from Cove, as KEY=value lines, and
// starts hiding them in all output.
func (r *run) fetchSecrets(ctx context.Context, keys []string, out io.Writer) ([]string, *failure) {
	if len(keys) == 0 {
		fmt.Fprintln(out, "the compose file uses no secrets")
		return nil, nil
	}
	fmt.Fprintf(out, "fetching %d secrets from Cove: %s\n", len(keys), strings.Join(keys, ", "))

	values, err := r.d.secrets.GetSecretsContext(ctx, keys...)
	switch {
	case errors.Is(err, coveclient.ErrNotFound), errors.Is(err, coveclient.ErrForbidden), errors.Is(err, coveclient.ErrInvalidKey):
		return nil, permanent(fmt.Errorf("Cove: %w (create the keys in Cove, or fix their names in the compose file)", err))
	case err != nil:
		return nil, transient(fmt.Errorf("Cove: %w", err))
	}

	r.scrub.add(values)
	env := make([]string, 0, len(values))
	for _, k := range keys {
		env = append(env, k+"="+values[k])
	}
	return env, nil
}

// build tags the images running now (so a rollback can put them back), then
// builds the new ones and tags them with the commit.
func (r *run) build(ctx context.Context, dir string, project compose.Project, env []string, out io.Writer) *failure {
	running, err := r.d.docker.ProjectContainers(ctx, project.Name)
	if err != nil {
		return transient(err)
	}
	r.previous = map[string]string{}
	for _, c := range running {
		r.previous[c.Service] = c.ImageID
	}
	if prev := r.req.Project.DeployedSHA; prev != "" {
		for _, s := range project.Services {
			if id := r.previous[s.Name]; s.Build && id != "" {
				if err := r.d.docker.Tag(ctx, id, rollbackRef(s.ImageName(project.Name), prev)); err != nil {
					fmt.Fprintf(out, "! %v: a rollback of %s may not find its image\n", err, s.Name)
				}
			}
		}
	}

	if err := r.d.compose.Build(ctx, dir, project.Name, env, out); err != nil {
		return permanent(err)
	}

	for _, s := range project.Services {
		if s.Build {
			image := s.ImageName(project.Name)
			if err := r.d.docker.Tag(ctx, image, rollbackRef(image, r.req.SHA)); err != nil {
				fmt.Fprintf(out, "! %v\n", err)
			}
		}
	}
	return nil
}

// KeepBackups is how many backups of each project are kept.
const KeepBackups = 5

// backup prepares for the swap. It fetches the previous version's secrets,
// so a rollback doesn't need Cove: Cove's database may be what's being
// swapped. And for a project with backup: postgres, it dumps the running
// database (pg_dumpall) to <backups>/<compose project>/; if that fails, the
// deploy stops.
func (r *run) backup(ctx context.Context, project compose.Project, out io.Writer) *failure {
	if prevDir := r.currentDir(project.Name); prevDir != "" && len(r.previous) > 0 {
		env, f := r.fetchSecretsFor(ctx, prevDir, out)
		if f != nil {
			fmt.Fprintf(out, "! the previous version's secrets couldn't be fetched now (%v); a rollback will try again\n", f.err)
		} else {
			r.prevEnv = append([]string{}, env...)
		}
	}

	if r.settings.Backup != settings.BackupPostgres {
		fmt.Fprintln(out, "no backup (x-lighthouse backup: postgres dumps a database before each deploy)")
		return nil
	}
	service := ""
	for _, s := range project.Services {
		if s.Image == "postgres" || strings.HasPrefix(s.Image, "postgres:") || strings.HasPrefix(s.Image, "postgres@") {
			if service != "" {
				return permanent(errors.New("backup: postgres needs exactly one service with a postgres image, and there are several"))
			}
			service = s.Name
		}
	}
	if service == "" {
		return permanent(errors.New("backup: postgres needs a service with a postgres image (postgres:17)"))
	}

	containers, err := r.d.docker.ProjectContainers(ctx, project.Name)
	if err != nil {
		return transient(err)
	}
	container := ""
	for _, c := range containers {
		if c.Service == service && c.State == "running" {
			container = c.Name
		}
	}
	if container == "" {
		fmt.Fprintf(out, "! %s isn't running, so there's nothing to back up\n", service)
		return nil
	}

	if r.d.backups == "" {
		return transient(errors.New("backup: no backup folder is set (BACKUP_PATH)"))
	}
	dir := filepath.Join(r.d.backups, project.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return transient(fmt.Errorf("backup: %w", err))
	}
	name := filepath.Join(dir, fmt.Sprintf("%s-%s-%s.sql.gz", project.Name, time.Now().UTC().Format("20060102-150405"), short12(r.req.SHA)))
	fmt.Fprintf(out, "backing up %s (pg_dumpall) to %s\n", container, name)
	if err := dump(ctx, r.d.compose, container, name); err != nil {
		os.Remove(name)
		return transient(fmt.Errorf("backup: %w", err))
	}
	if info, err := os.Stat(name); err == nil {
		fmt.Fprintf(out, "backed up: %d KB\n", info.Size()>>10)
	}
	pruneBackups(dir, KeepBackups, out)
	return nil
}

// dump writes pg_dumpall's output, gzipped, to a new file only its owner can
// read: it holds every role's password hash.
func dump(ctx context.Context, c Compose, container string, path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	err = c.Exec(ctx, container, []string{"sh", "-c", `pg_dumpall -U "$POSTGRES_USER"`}, gz)
	if closeErr := gz.Close(); err == nil {
		err = closeErr
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// pruneBackups keeps the newest keep backups in dir (their names sort by
// time).
func pruneBackups(dir string, keep int, out io.Writer) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql.gz") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keep {
		if err := os.Remove(filepath.Join(dir, names[0])); err == nil {
			fmt.Fprintf(out, "removed the old backup %s\n", names[0])
		}
		names = names[1:]
	}
}

// rollBack restores the version that ran before: its images under their
// usual names, then docker compose up from its folder. res is the failure
// that caused it.
func (r *run) rollBack(ctx context.Context, res Result, project compose.Project, failedDir string) Result {
	prevSHA := r.req.Project.DeployedSHA
	prevDir := r.currentDir(project.Name)
	note := func(msg string) Result {
		res.Err = fmt.Errorf("%w; %s", res.Err, msg)
		return res
	}

	switch {
	case len(r.previous) == 0:
		return note("nothing ran before, so there's nothing to roll back to")
	case (prevSHA == "" || prevDir == "") && r.fallbackDir == "":
		return note("the previous version's files aren't kept (it wasn't deployed by this Lighthouse), so it wasn't restored automatically")
	case prevDir == "":
		prevDir = r.fallbackDir
	}
	what := short(prevSHA)
	if what == "" {
		what = "the previous images"
	}

	f := r.step(ctx, "rollback", func(ctx context.Context, out io.Writer) *failure {
		fmt.Fprintf(out, "restoring %s\n", what)
		for _, s := range project.Services {
			if s.Build && r.previous[s.Name] != "" {
				if err := r.d.docker.Tag(ctx, r.previous[s.Name], s.ImageName(project.Name)); err != nil {
					return permanent(err)
				}
			}
		}
		env := r.prevEnv
		if env == nil {
			var f *failure
			if env, f = r.fetchSecretsFor(ctx, prevDir, out); f != nil {
				return f
			}
		}
		env = append(append([]string{}, env...), deployVars(prevSHA, r.req.Project.DeployedVersion)...)
		if err := r.d.compose.Up(ctx, prevDir, project.Name, env, out); err != nil {
			return permanent(err)
		}
		return nil
	})
	if f != nil {
		return note(fmt.Sprintf("the rollback failed too (%v): the project may be down", f.err))
	}

	if failedDir != prevDir {
		os.RemoveAll(failedDir)
	}
	res.Status = projects.StatusRolledBack
	return note("rolled back to " + what)
}

// fetchSecretsFor fetches the secrets the compose file in dir needs.
func (r *run) fetchSecretsFor(ctx context.Context, dir string, out io.Writer) ([]string, *failure) {
	vars, err := r.d.compose.Variables(ctx, dir)
	if err != nil {
		return nil, permanent(err)
	}
	var keys []string
	for _, v := range vars {
		if v.Default == "" {
			keys = append(keys, v.Name)
		}
	}
	return r.fetchSecrets(ctx, keys, out)
}

// currentDir is the folder of the version deployed now, if it's kept.
func (r *run) currentDir(composeProject string) string {
	if r.req.Project.DeployedSHA == "" {
		return ""
	}
	dir := filepath.Join(r.d.root, composeProject, short12(r.req.Project.DeployedSHA))
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return dir
}

// removeUnlessCurrent deletes a failed deploy's folder, unless it's the one
// running (a redeploy of the same commit).
func (r *run) removeUnlessCurrent(dir string) {
	if dir != "" && dir != r.currentDir(filepath.Base(filepath.Dir(dir))) {
		os.RemoveAll(dir)
	}
}

// rollbackRef is the image name Lighthouse keeps a version under:
// <repository>:lh-<first 12 of the commit>.
func rollbackRef(image string, sha string) string {
	return repository(image) + ":" + rollbackPrefix + short12(sha)
}

const rollbackPrefix = "lh-"

// repository is an image reference without its tag: "website-web" for
// "website-web:latest", "registry:5000/app" for "registry:5000/app:1.2".
func repository(ref string) string {
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return ref[:colon]
	}
	return ref
}

func serviceNames(p compose.Project) string {
	names := make([]string, len(p.Services))
	for i, s := range p.Services {
		names[i] = s.Name
	}
	return strings.Join(names, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func short12(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// prefixWriter starts every line with prefix.
type prefixWriter struct {
	w      io.Writer
	prefix string
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	lines := strings.SplitAfter(string(b), "\n")
	for _, l := range lines {
		if l != "" {
			io.WriteString(p.w, p.prefix+l)
		}
	}
	return len(b), nil
}
