// Package deploy deploys one commit of a project, building first and
// swapping last: everything that can fail happens while the running version
// keeps serving, and if the new version doesn't come up healthy, the previous
// one is put back.
//
//	fetch    download the exact commit through the GitHub API and unpack it
//	inspect  read the compose file: its project name, services, variables
//	secrets  fetch every ${KEY} without a default from Cove, in one request
//	build    build the images; tag the running ones for rollback first
//	swap     docker compose up: the only moment of downtime
//	verify   wait until every service is healthy (or stable); else roll back
//	cleanup  old deploy folders, old rollback tags, unused images and cache
//
// Every step's output is kept (the tail, with secret values hidden) and
// returned in the Result, and also written to the log as it happens.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lsariol/coveclient"
	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/docker"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/projects"
)

// Compose runs docker compose. compose.Runner implements it.
type Compose interface {
	Inspect(ctx context.Context, dir string) (compose.Project, error)
	Variables(ctx context.Context, dir string) ([]compose.Variable, error)
	Build(ctx context.Context, dir string, project string, env []string, out io.Writer) error
	Up(ctx context.Context, dir string, project string, env []string, out io.Writer) error
}

// Docker reads containers and manages images. *docker.Client implements it.
type Docker interface {
	ProjectContainers(ctx context.Context, project string) ([]docker.Container, error)
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
	Root     string
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
	return &Deployer{compose: c, docker: d, source: s, secrets: sec, root: opts.Root, timeouts: t, log: log, poll: poll}
}

// Request is one deploy.
type Request struct {
	Project projects.Project
	SHA     string // the commit to deploy
	Token   string // GitHub token, for the download

	// Claim is called with the compose project the compose file names, before
	// anything changes. An error (e.g. another project has it) stops the
	// deploy. The orchestrator records it in the store.
	Claim func(ctx context.Context, composeProject string) error
}

// Result is how a deploy went.
type Result struct {
	Status         string // projects.StatusSucceeded, StatusFailed or StatusRolledBack
	FailureKind    string // projects.FailureTransient or FailurePermanent, for a failure
	FailedStep     string
	ComposeProject string // as the compose file names it, once inspected
	Steps          []projects.Step
	Err            error // nil on success
}

// The steps, in order.
const (
	StepFetch   = "fetch"
	StepInspect = "inspect"
	StepSecrets = "secrets"
	StepBuild   = "build"
	StepSwap    = "swap"
	StepVerify  = "verify"
	StepCleanup = "cleanup"
)

var stepOrder = []string{StepFetch, StepInspect, StepSecrets, StepBuild, StepSwap, StepVerify, StepCleanup}

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
}

// Deploy deploys req.Project at req.SHA. A second call waits for the first.
func (d *Deployer) Deploy(ctx context.Context, req Request) Result {
	d.mu.Lock()
	defer d.mu.Unlock()

	r := &run{d: d, req: req, log: slog.With("project", req.Project.Name, "sha", short(req.SHA))}
	r.log.Info("deploy started")
	res := r.deploy(ctx)
	res.Steps = r.steps

	switch res.Status {
	case projects.StatusSucceeded:
		r.log.Info("deploy finished")
	default:
		r.log.Error("deploy failed", "status", res.Status, "step", res.FailedStep, "kind", res.FailureKind, "err", res.Err)
	}
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

	var env []string
	if f := r.step(ctx, StepSecrets, func(ctx context.Context, out io.Writer) *failure {
		var f *failure
		env, f = r.fetchSecrets(ctx, keys, out)
		return f
	}); f != nil {
		r.removeUnlessCurrent(dir)
		return fail(StepSecrets, f)
	}

	if f := r.step(ctx, StepBuild, func(ctx context.Context, out io.Writer) *failure {
		return r.build(ctx, dir, project, env, out)
	}); f != nil {
		r.removeUnlessCurrent(dir)
		return fail(StepBuild, f)
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
	after := false
	for _, name := range stepOrder {
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
	case StepBuild:
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

	dir := filepath.Join(r.d.root, project.Name, short12(r.req.SHA))
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

	vars, err := r.d.compose.Variables(ctx, dir)
	if err != nil {
		return project, dir, nil, permanent(fmt.Errorf("the compose file's variables can't be read: %w", err))
	}
	var keys []string
	for _, v := range vars {
		if v.Default != "" {
			// Only secrets belong in ${...}; a default means it's a setting.
			fmt.Fprintf(out, "! ${%s} has a default (%q), so it isn't fetched from Cove; write plain settings out as values\n", v.Name, v.Default)
			continue
		}
		keys = append(keys, v.Name)
	}
	return project, dir, keys, nil
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
		return nil, permanent(fmt.Errorf("%w (create the keys in Cove, or fix their names in the compose file)", err))
	case err != nil:
		return nil, transient(fmt.Errorf("Cove: %w", err))
	}

	r.scrub = newScrubber(values, r.scrub.out...)
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
		return transient(fmt.Errorf("Docker: %w", err))
	}
	r.previous = map[string]string{}
	for _, c := range running {
		r.previous[c.Service] = c.ImageID
	}
	if prev := r.req.Project.DeployedSHA; prev != "" {
		for _, s := range project.Services {
			if id := r.previous[s.Name]; s.Build && id != "" {
				r.d.docker.Tag(ctx, id, rollbackRef(s.ImageName(project.Name), prev))
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
	case prevSHA == "" || prevDir == "":
		return note("the previous version's files aren't kept (it wasn't deployed by this Lighthouse), so it wasn't restored automatically")
	}

	f := r.step(ctx, "rollback", func(ctx context.Context, out io.Writer) *failure {
		fmt.Fprintf(out, "restoring %s\n", short(prevSHA))
		for _, s := range project.Services {
			if s.Build && r.previous[s.Name] != "" {
				if err := r.d.docker.Tag(ctx, r.previous[s.Name], s.ImageName(project.Name)); err != nil {
					return permanent(err)
				}
			}
		}
		env, f := r.fetchSecretsFor(ctx, prevDir, out)
		if f != nil {
			return f
		}
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
	return note("rolled back to " + short(prevSHA))
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
