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
	"sync"
	"time"

	"github.com/LSariol/LightHouse/internal/config"
	"github.com/LSariol/LightHouse/internal/control"
	"github.com/LSariol/LightHouse/internal/docker"
	"github.com/LSariol/LightHouse/internal/github"
	"github.com/LSariol/LightHouse/internal/orchestrator"
	"github.com/LSariol/LightHouse/internal/projects"
)

// Containers starts, stops and inspects containers. *docker.Client
// implements it.
type Containers interface {
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	Restart(ctx context.Context, name string) error
	State(ctx context.Context, name string) (string, error)
	Logs(ctx context.Context, name string, tail int) (string, error)
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
	MaxHistoryLimit = 100   // the most deployments `history` returns
)

type Daemon struct {
	cfg        config.Config
	version    string
	containers Containers
	started    time.Time

	mu           sync.RWMutex
	phase        string
	store        projects.Store
	orchestrator *orchestrator.Orchestrator
	health       Health
}

func New(cfg config.Config, version string, c Containers) *Daemon {
	return &Daemon{cfg: cfg, version: version, containers: c, started: time.Now(), phase: PhaseStarting}
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

	projects, err := d.Projects(ctx)
	if err != nil {
		return s, err
	}
	for i := range projects {
		state, err := d.containers.State(ctx, projects[i].Container)
		if err != nil {
			state = "unknown"
		}
		projects[i].State = state
	}
	s.Projects = projects
	return s, nil
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
		Name:          p.Name,
		URL:           p.Repo.URL(),
		Container:     p.Container(),
		Commit:        p.DeployedSHA,
		WatchingSince: p.CreatedAt,
		LastDeployed:  p.DeployedAt,
		LastChecked:   p.LastCheckedAt,
		Checks:        int(p.Checks),
		LastError:     p.LastError,
		LastErrorAt:   p.LastErrorAt,
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
	p, err := store.Add(ctx, name, repo)
	if err != nil {
		return control.Project{}, projectError(err, name, url)
	}
	return toProject(p), nil
}

func (d *Daemon) Remove(ctx context.Context, name string) error {
	store, _, err := d.parts()
	if err != nil {
		return err
	}
	return projectError(store.Remove(ctx, name), name, "")
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

func (d *Daemon) Deploy(ctx context.Context, name string) error {
	_, o, err := d.parts()
	if err != nil {
		return err
	}
	if _, err := d.find(ctx, name); err != nil {
		return err
	}
	if err := o.Deploy(ctx, name); err != nil {
		return control.Errorf(control.KindInternal, "Deploying %s failed: %v. \"history %s\" lists the attempts; \"docker logs lighthouse\" has the full output.", name, err, name)
	}
	return nil
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
		out = append(out, control.Deployment{
			Commit: h.SHA, Trigger: h.Trigger, Status: h.Status,
			StartedAt: h.StartedAt, FinishedAt: h.FinishedAt, Error: h.Error,
		})
	}
	return out, nil
}

func (d *Daemon) Start(ctx context.Context, name string) error {
	return d.containerAction(ctx, name, "start", d.containers.Start)
}

func (d *Daemon) Stop(ctx context.Context, name string) error {
	return d.containerAction(ctx, name, "stop", d.containers.Stop)
}

func (d *Daemon) Restart(ctx context.Context, name string) error {
	return d.containerAction(ctx, name, "restart", d.containers.Restart)
}

// containerAction runs fn on a project's container. Only watched projects'
// containers can be controlled, never other containers on the host.
func (d *Daemon) containerAction(ctx context.Context, name string, verb string, fn func(context.Context, string) error) error {
	p, err := d.find(ctx, name)
	if err != nil {
		return err
	}

	if err := fn(ctx, p.Container()); err != nil {
		if docker.IsNotFound(err) {
			return control.Errorf(control.KindNotFound, "%s", missingContainer(p))
		}
		return control.Errorf(control.KindInternal, "Couldn't %s %s: %v", verb, p.Name, err)
	}
	return nil
}

func (d *Daemon) Logs(ctx context.Context, name string, lines int) (string, error) {
	if lines < 1 || lines > MaxLogLines {
		return "", control.Errorf(control.KindInvalid, "Lines must be between 1 and %d.", MaxLogLines)
	}
	p, err := d.find(ctx, name)
	if err != nil {
		return "", err
	}

	logs, err := d.containers.Logs(ctx, p.Container(), lines)
	if err != nil {
		if docker.IsNotFound(err) {
			return "", control.Errorf(control.KindNotFound, "%s", missingContainer(p))
		}
		return "", fmt.Errorf("logs for %s: %w", p.Name, err)
	}
	return logs, nil
}

// missingContainer explains why a project's container wasn't found. Until
// compose projects are read from the compose file (DOCUMENTATION.md 16.9),
// Lighthouse looks for one container named after the repository.
func missingContainer(p projects.Project) string {
	return fmt.Sprintf("There's no container named %q (%s's repository name, which is where Lighthouse looks for now). "+
		"Either %s hasn't been deployed yet (\"deploy %s\"), or its compose file names its containers differently or has several, "+
		"which Lighthouse doesn't handle yet: use docker compose on the server for those.", p.Container(), p.Name, p.Name, p.Name)
}
