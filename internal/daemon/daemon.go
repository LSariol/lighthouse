// Package daemon is the running Lighthouse as the CLI sees it: it implements
// control.Service on top of the project store, the orchestrator and Docker,
// and turns their errors into messages that say how to fix them.
//
// The daemon answers from the moment it starts. The store and orchestrator
// arrive later (Ready), once Cove and the database are reachable; until then
// `status` shows the startup phase and everything else says it's starting.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lsariol/lighthouse/internal/config"
	"github.com/lsariol/lighthouse/internal/control"
	"github.com/lsariol/lighthouse/internal/deploy"
	"github.com/lsariol/lighthouse/internal/docker"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/orchestrator"
	"github.com/lsariol/lighthouse/internal/projects"
)

// Containers finds and controls a compose project's containers.
// *docker.Client implements it.
type Containers interface {
	ProjectContainers(ctx context.Context, project string) ([]docker.Container, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Restart(ctx context.Context, id string) error
	Logs(ctx context.Context, id string, tail int) (string, error)
}

// Compose removes a compose project. compose.Runner implements it.
type Compose interface {
	Down(ctx context.Context, dir string, project string, out io.Writer) error
}

// Health reports on the database for `status`. *database.Database
// implements it.
type Health interface {
	Ping(ctx context.Context) error
	SchemaVersion(ctx context.Context) (have int64, want int64, err error)
}

// Phases the daemon goes through at startup, shown by `status`.
const (
	PhaseStarting  = "starting"
	PhaseWaiting   = "waiting for Cove"
	PhaseDatabase  = "waiting for the database"
	PhaseMigrating = "migrating the database"
	PhaseRunning   = "running"
)

const (
	MaxLogLines     = 10000 // the most lines `logs` returns
	MaxHistoryLimit = 100   // the most deployments `history` returns, and how far back `report` goes
)

type Daemon struct {
	cfg        config.Config
	version    string
	containers Containers
	compose    Compose
	started    time.Time

	mu           sync.RWMutex
	phase        string
	store        projects.Store
	orchestrator *orchestrator.Orchestrator
	health       Health
}

func New(cfg config.Config, version string, c Containers, cmp Compose) *Daemon {
	return &Daemon{cfg: cfg, version: version, containers: c, compose: cmp, started: time.Now(), phase: PhaseStarting}
}

// SetPhase records how far startup has got.
func (d *Daemon) SetPhase(phase string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.phase = phase
}

// Ready hands the daemon its store and orchestrator, once startup has
// connected everything, and marks it running.
func (d *Daemon) Ready(store projects.Store, o *orchestrator.Orchestrator, health Health) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.store, d.orchestrator, d.health = store, o, health
	d.phase = PhaseRunning
}

// parts returns the store and orchestrator, or an unavailable error while
// Lighthouse is still starting.
func (d *Daemon) parts() (projects.Store, *orchestrator.Orchestrator, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.store == nil {
		return nil, nil, control.Errorf(control.KindUnavailable, "Lighthouse is still starting (%s). \"status\" shows progress; \"docker logs lighthouse\" says what it's waiting for.", d.phase)
	}
	return d.store, d.orchestrator, nil
}

func (d *Daemon) Status(ctx context.Context) (control.Status, error) {
	d.mu.RLock()
	phase, health, o := d.phase, d.health, d.orchestrator
	d.mu.RUnlock()

	s := control.Status{
		Version:      d.version,
		Env:          d.cfg.Env,
		StartedAt:    d.started,
		Phase:        phase,
		PollInterval: d.cfg.PollInterval,
		CoveURL:      d.cfg.CoveURL,
	}
	if o == nil {
		return s, nil
	}
	s.Paused = o.IsPaused()
	s.GitHubToken = true // the orchestrator exists only once it was read

	s.Database = "reachable"
	if err := health.Ping(ctx); err != nil {
		s.Database = "unreachable"
		return s, nil
	}
	switch have, want, err := health.SchemaVersion(ctx); {
	case err != nil:
		s.Schema = "unknown"
	case have < want:
		s.Schema = fmt.Sprintf("version %d (this Lighthouse needs %d)", have, want)
	default:
		s.Schema = fmt.Sprintf("version %d (up to date)", have)
	}

	list, err := d.Projects(ctx)
	if err != nil {
		return s, err
	}
	for i := range list {
		d.addServices(ctx, &list[i])
	}
	s.Projects = list
	return s, nil
}

// addServices fills in a project's services and sums up its state.
func (d *Daemon) addServices(ctx context.Context, p *control.Project) {
	cs, err := d.containers.ProjectContainers(ctx, p.ComposeProject)
	if err != nil {
		p.State = "unknown"
		return
	}
	up, down := 0, 0
	for _, c := range cs {
		p.Services = append(p.Services, control.ServiceStatus{Name: c.Service, Container: c.Name, State: c.State, Health: c.Health})
		switch {
		case c.State == "running" && c.Health != "unhealthy":
			up++
		case c.State == "exited" && c.ExitCode == 0:
			// A one-off job that finished: neither up nor a problem.
		default:
			down++
		}
	}
	switch {
	case len(cs) == 0:
		p.State = "missing"
	case down == 0:
		p.State = "running"
	case up == 0:
		p.State = "stopped"
	default:
		p.State = "degraded"
	}
}

func (d *Daemon) Projects(ctx context.Context) ([]control.Project, error) {
	store, _, err := d.parts()
	if err != nil {
		return nil, err
	}
	list, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]control.Project, 0, len(list))
	for _, p := range list {
		out = append(out, toProject(p))
	}
	return out, nil
}

func toProject(p projects.Project) control.Project {
	return control.Project{
		Name:           p.Name,
		URL:            p.Repo.URL(),
		ComposeProject: p.ComposeName(),
		Commit:         p.DeployedSHA,
		Version:        p.DeployedVersion,
		Mode:           p.Mode,
		Tier:           p.Tier,
		Stopped:        p.Stopped,
		WatchingSince:  p.CreatedAt,
		LastDeployed:   p.DeployedAt,
		LastChecked:    p.LastCheckedAt,
		Checks:         int(p.Checks),
		LastError:      p.LastError,
		LastErrorAt:    p.LastErrorAt,
		FailureCount:   p.FailureCount,
		Broken:         p.Broken,
	}
}

func (d *Daemon) Add(ctx context.Context, name string, url string) (control.Project, error) {
	store, _, err := d.parts()
	if err != nil {
		return control.Project{}, err
	}
	repo, err := github.ParseRepoURL(url)
	if err != nil {
		return control.Project{}, projectError(err, name, url)
	}
	chosen := name != ""
	if !chosen {
		name = projectName(repo.Name)
	}
	p, err := store.Add(ctx, name, repo)
	switch {
	case errors.Is(err, projects.ErrNameTaken) && !chosen:
		other, _ := store.Get(ctx, name)
		return control.Project{}, control.Errorf(control.KindNameTaken,
			"A project named %q already exists (watching %s). Add this one under another name: add %s --name <name>", name, other.Repo, url)
	case errors.Is(err, projects.ErrNameTaken):
		return control.Project{}, control.Errorf(control.KindNameTaken, "A project named %q already exists. Pick another name.", name)
	case err != nil:
		return control.Project{}, projectError(err, name, url)
	}
	return toProject(p), nil
}

// projectName is the name a repository's project gets by default: its name,
// lowercased, with anything but letters, digits, - and _ turned into -.
func projectName(repo string) string {
	b := []byte(strings.ToLower(repo))
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			b[i] = '-'
		}
	}
	return strings.Trim(string(b), "-_")
}

func (d *Daemon) Remove(ctx context.Context, name string, down bool) error {
	store, _, err := d.parts()
	if err != nil {
		return err
	}
	p, err := d.find(ctx, name)
	if err != nil {
		return err
	}
	if down {
		// Down needs a folder without a compose file to run in.
		dir := d.cfg.StagingPath
		os.MkdirAll(dir, 0o755)
		var out strings.Builder
		if err := d.compose.Down(ctx, dir, p.ComposeName(), &out); err != nil {
			return control.Errorf(control.KindInternal, "Couldn't remove %s's containers (compose project %q): %v. Nothing was changed; it's still watched.", p.Name, p.ComposeName(), err)
		}
	}
	return projectError(store.Remove(ctx, p.Name), name, "")
}

func (d *Daemon) Rename(ctx context.Context, name string, newName string) error {
	store, _, err := d.parts()
	if err != nil {
		return err
	}
	err = store.Rename(ctx, name, newName)
	if errors.Is(err, projects.ErrNameTaken) || errors.Is(err, projects.ErrInvalidName) {
		return projectError(err, newName, "")
	}
	return projectError(err, name, "")
}

func (d *Daemon) SetURL(ctx context.Context, name string, url string) error {
	store, _, err := d.parts()
	if err != nil {
		return err
	}
	repo, err := github.ParseRepoURL(url)
	if err != nil {
		return projectError(err, name, url)
	}
	return projectError(store.SetRepo(ctx, name, repo), name, url)
}

// projectError turns a store error into a message that says how to fix it.
func projectError(err error, name string, url string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, projects.ErrNotFound):
		return control.Errorf(control.KindNotFound, "No project named %q. \"list\" shows the watched projects.", name)
	case errors.Is(err, projects.ErrNameTaken):
		return control.Errorf(control.KindConflict, "A project named %q already exists. Pick another name.", name)
	case errors.Is(err, projects.ErrInvalidName):
		return control.Errorf(control.KindInvalid, "%q isn't a valid project name: use letters, digits, - and _ (up to 64), starting with a letter or digit.", name)
	case errors.Is(err, github.ErrInvalidURL):
		return control.Errorf(control.KindInvalid, "%q isn't a GitHub repository URL. Use https://github.com/<owner>/<repo>.", url)
	case errors.Is(err, projects.ErrRepoWatched):
		return control.Errorf(control.KindConflict, "%s is already watched under another name. \"list\" shows which.", url)
	default:
		return err
	}
}

// find returns the project called name, or a not-found error.
func (d *Daemon) find(ctx context.Context, name string) (projects.Project, error) {
	store, _, err := d.parts()
	if err != nil {
		return projects.Project{}, err
	}
	p, err := store.Get(ctx, name)
	if err != nil {
		return p, projectError(err, name, "")
	}
	return p, nil
}

func (d *Daemon) Deploy(ctx context.Context, name string, ref string) error {
	_, o, err := d.parts()
	if err != nil {
		return err
	}
	if _, err := d.find(ctx, name); err != nil {
		return err
	}
	return deployError(name, o.Deploy(ctx, name, ref))
}

func (d *Daemon) Rollback(ctx context.Context, name string) (string, error) {
	_, o, err := d.parts()
	if err != nil {
		return "", err
	}
	p, err := d.find(ctx, name)
	if err != nil {
		return "", err
	}
	to, err := o.Rollback(ctx, p.Name)
	if errors.Is(err, orchestrator.ErrNothingToRollBackTo) {
		return "", control.Errorf(control.KindNotFound, "%s has no earlier successful deploy to go back to. \"history %s\" lists its deploys; \"deploy %s <tag or commit>\" deploys any one.", p.Name, p.Name, p.Name)
	}
	return to, deployError(p.Name, err)
}

func (d *Daemon) Retry(ctx context.Context, name string) error {
	_, o, err := d.parts()
	if err != nil {
		return err
	}
	if _, err := d.find(ctx, name); err != nil {
		return err
	}
	return deployError(name, o.Retry(ctx, name))
}

// Check runs a project's latest commit through the deploy's checks without
// deploying it. A commit that fails them isn't an error: the result says
// which step failed and why.
func (d *Daemon) Check(ctx context.Context, name string) (control.Deployment, error) {
	_, o, err := d.parts()
	if err != nil {
		return control.Deployment{}, err
	}
	p, err := d.find(ctx, name)
	if err != nil {
		return control.Deployment{}, err
	}
	started := time.Now()
	res, sha, err := o.Check(ctx, p.Name)
	if err != nil {
		return control.Deployment{}, control.Errorf(control.KindInternal, "Checking %s failed: %v.", p.Name, err)
	}

	out := control.Deployment{
		Commit: sha, Trigger: "dry run", Status: res.Status, FailureKind: res.FailureKind, FailedStep: res.FailedStep,
		StartedAt: started, FinishedAt: time.Now(),
	}
	if res.Err != nil {
		out.Error = projects.ErrorText(res.Err)
	}
	for _, st := range res.Steps {
		out.Steps = append(out.Steps, control.Step{Name: st.Name, Status: st.Status, StartedAt: st.StartedAt, FinishedAt: st.FinishedAt, Log: st.Log})
	}
	return out, nil
}

func deployError(name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, orchestrator.ErrUpdating) {
		return control.Errorf(control.KindConflict, "Lighthouse is updating itself right now: %s isn't deployed until that's done. \"docker logs -f %s\" shows it; try again in a minute.", name, deploy.HelperName)
	}
	if errors.Is(err, orchestrator.ErrHandedOff) {
		return control.Errorf(control.KindHandedOff, "Lighthouse is updating itself: the helper container %s is swapping it now, so this connection ends. \"docker logs -f %s\" shows how it goes; \"history %s\" has the result once Lighthouse is back.", deploy.HelperName, deploy.HelperName, name)
	}
	return control.Errorf(control.KindInternal, "Deploying %s failed: %v. \"report %s\" shows each step's output.", name, err, name)
}

func (d *Daemon) Scan(ctx context.Context) error {
	_, o, err := d.parts()
	if err != nil {
		return err
	}
	err = o.Scan(ctx)
	switch {
	case errors.Is(err, orchestrator.ErrScanRunning):
		return control.Errorf(control.KindConflict, "A scan is already running. Try again when it's done.")
	case err != nil:
		return control.Errorf(control.KindInternal, "Scan finished with errors (%v). \"list\" shows each project's last error.", err)
	}
	return nil
}

func (d *Daemon) Pause(ctx context.Context) error {
	_, o, err := d.parts()
	if err != nil {
		return err
	}
	o.Pause()
	return nil
}

func (d *Daemon) Resume(ctx context.Context) error {
	_, o, err := d.parts()
	if err != nil {
		return err
	}
	o.Resume()
	return nil
}

func (d *Daemon) History(ctx context.Context, name string, limit int) ([]control.Deployment, error) {
	if limit < 1 || limit > MaxHistoryLimit {
		return nil, control.Errorf(control.KindInvalid, "The count must be between 1 and %d.", MaxHistoryLimit)
	}
	store, _, err := d.parts()
	if err != nil {
		return nil, err
	}
	history, err := store.History(ctx, name, limit)
	if err != nil {
		return nil, projectError(err, name, "")
	}
	out := make([]control.Deployment, 0, len(history))
	for _, h := range history {
		out = append(out, toDeployment(h))
	}
	return out, nil
}

func (d *Daemon) Report(ctx context.Context, name string, n int) (control.Deployment, error) {
	if n < 1 || n > MaxHistoryLimit {
		return control.Deployment{}, control.Errorf(control.KindInvalid, "The deploy number must be between 1 (the latest) and %d.", MaxHistoryLimit)
	}
	store, _, err := d.parts()
	if err != nil {
		return control.Deployment{}, err
	}
	dep, err := store.Deployment(ctx, name, n)
	if errors.Is(err, projects.ErrNoDeployment) {
		return control.Deployment{}, control.Errorf(control.KindNotFound, "%s has fewer than %d deploys. \"history %s\" lists them.", name, n, name)
	}
	if err != nil {
		return control.Deployment{}, projectError(err, name, "")
	}
	out := toDeployment(dep)
	for _, st := range dep.Steps {
		out.Steps = append(out.Steps, control.Step{Name: st.Name, Status: st.Status, StartedAt: st.StartedAt, FinishedAt: st.FinishedAt, Log: st.Log})
	}
	return out, nil
}

func toDeployment(d projects.Deployment) control.Deployment {
	return control.Deployment{
		Commit: d.SHA, Version: d.Version, Trigger: d.Trigger, Status: d.Status, FailureKind: d.FailureKind, FailedStep: d.FailedStep,
		StartedAt: d.StartedAt, FinishedAt: d.FinishedAt, Error: d.Error,
	}
}

// Start, Stop and Restart a project (or one service). A whole project
// stopped stays down, on purpose: checks don't deploy it and the reconcile
// loop doesn't bring it back, until it's started, restarted or deployed.
func (d *Daemon) Start(ctx context.Context, target string) error {
	return d.stopped(ctx, target, false, d.containerAction(ctx, target, "start", d.containers.Start))
}

func (d *Daemon) Stop(ctx context.Context, target string) error {
	return d.stopped(ctx, target, true, d.containerAction(ctx, target, "stop", d.containers.Stop))
}

func (d *Daemon) Restart(ctx context.Context, target string) error {
	return d.stopped(ctx, target, false, d.containerAction(ctx, target, "restart", d.containers.Restart))
}

// stopped records whether a whole project was stopped on purpose, after the
// action (err) worked.
func (d *Daemon) stopped(ctx context.Context, target string, stopped bool, err error) error {
	if err != nil || strings.Contains(target, ":") {
		return err
	}
	p, err := d.find(ctx, target)
	if err != nil {
		return err
	}
	if p.Stopped == stopped {
		return nil
	}
	store, _, err := d.parts()
	if err != nil {
		return err
	}
	return store.SetStopped(ctx, p.Name, stopped)
}

// targets resolves "<project>" (every container) or "<project>:<service>"
// (that service's) to containers. Only watched projects' containers can be
// reached, never other containers on the host.
func (d *Daemon) targets(ctx context.Context, target string) (projects.Project, []docker.Container, error) {
	name, service, hasService := strings.Cut(target, ":")
	p, err := d.find(ctx, name)
	if err != nil {
		return p, nil, err
	}

	all, err := d.containers.ProjectContainers(ctx, p.ComposeName())
	if err != nil {
		return p, nil, err
	}
	if len(all) == 0 {
		return p, nil, control.Errorf(control.KindNotFound, "%s has no containers (compose project %q). \"deploy %s\" starts it.", p.Name, p.ComposeName(), p.Name)
	}
	if !hasService {
		return p, all, nil
	}

	var picked []docker.Container
	services := map[string]bool{}
	for _, c := range all {
		services[c.Service] = true
		if c.Service == service {
			picked = append(picked, c)
		}
	}
	if len(picked) == 0 {
		names := make([]string, 0, len(services))
		for s := range services {
			names = append(names, s)
		}
		sort.Strings(names)
		return p, nil, control.Errorf(control.KindNotFound, "%s has no service %q. Its services: %s.", p.Name, service, strings.Join(names, ", "))
	}
	return p, picked, nil
}

func (d *Daemon) containerAction(ctx context.Context, target string, verb string, fn func(context.Context, string) error) error {
	p, cs, err := d.targets(ctx, target)
	if err != nil {
		return err
	}
	var failed []string
	for _, c := range cs {
		if err := fn(ctx, c.ID); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", c.Service, err))
		}
	}
	if len(failed) > 0 {
		return control.Errorf(control.KindInternal, "Couldn't %s %s: %s", verb, p.Name, strings.Join(failed, "; "))
	}
	return nil
}

func (d *Daemon) Logs(ctx context.Context, target string, lines int) (string, error) {
	if lines < 1 || lines > MaxLogLines {
		return "", control.Errorf(control.KindInvalid, "Lines must be between 1 and %d.", MaxLogLines)
	}
	p, cs, err := d.targets(ctx, target)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	for _, c := range cs {
		logs, err := d.containers.Logs(ctx, c.ID, lines)
		if err != nil {
			return "", fmt.Errorf("logs of %s:%s: %w", p.Name, c.Service, err)
		}
		if len(cs) > 1 {
			fmt.Fprintf(&b, "==> %s:%s <==\n", p.Name, c.Service)
		}
		b.WriteString(logs)
		if len(cs) > 1 && logs != "" && !strings.HasSuffix(logs, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String(), nil
}
