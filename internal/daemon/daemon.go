// Package daemon is the running Lighthouse as the CLI sees it: it implements
// control.Service on top of the watchlist, the orchestrator and Docker, and
// turns their errors into messages that say how to fix them.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/LSariol/LightHouse/internal/config"
	"github.com/LSariol/LightHouse/internal/control"
	"github.com/LSariol/LightHouse/internal/docker"
	"github.com/LSariol/LightHouse/internal/github"
	"github.com/LSariol/LightHouse/internal/orchestrator"
	"github.com/LSariol/LightHouse/internal/watchlist"
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

// Phases the daemon goes through at startup, shown by `status`.
const (
	PhaseStarting = "starting"
	PhaseWaiting  = "waiting for Cove"
	PhaseRunning  = "running"
)

// MaxLogLines is the most lines `logs` returns.
const MaxLogLines = 10000

type Daemon struct {
	cfg          config.Config
	version      string
	projects     *watchlist.List
	orchestrator *orchestrator.Orchestrator
	containers   Containers
	started      time.Time
	phase        atomic.Value // string
}

func New(cfg config.Config, version string, projects *watchlist.List, o *orchestrator.Orchestrator, c Containers) *Daemon {
	d := &Daemon{cfg: cfg, version: version, projects: projects, orchestrator: o, containers: c, started: time.Now()}
	d.phase.Store(PhaseStarting)
	return d
}

// SetPhase records how far startup has got.
func (d *Daemon) SetPhase(phase string) { d.phase.Store(phase) }

func (d *Daemon) running() error {
	if phase := d.phase.Load().(string); phase != PhaseRunning {
		return control.Errorf(control.KindUnavailable, "Lighthouse is still starting (%s). \"status\" shows progress; \"docker logs lighthouse\" says what it's waiting for.", phase)
	}
	return nil
}

func (d *Daemon) Status(ctx context.Context) (control.Status, error) {
	projects, _ := d.Projects(ctx)
	for i := range projects {
		state, err := d.containers.State(ctx, projects[i].Container)
		if err != nil {
			state = "unknown"
		}
		projects[i].State = state
	}

	return control.Status{
		Version:      d.version,
		Env:          d.cfg.Env,
		StartedAt:    d.started,
		Phase:        d.phase.Load().(string),
		Paused:       d.orchestrator.IsPaused(),
		PollInterval: d.cfg.PollInterval,
		CoveURL:      d.cfg.CoveURL,
		GitHubToken:  d.orchestrator.HasGitToken(),
		Projects:     projects,
	}, nil
}

func (d *Daemon) Projects(ctx context.Context) ([]control.Project, error) {
	all := d.projects.All()
	projects := make([]control.Project, 0, len(all))
	for _, p := range all {
		projects = append(projects, toProject(p))
	}
	return projects, nil
}

func toProject(p watchlist.Project) control.Project {
	return control.Project{
		Name:          p.Name,
		URL:           p.URL,
		Container:     p.Container(),
		Commit:        p.Commit(),
		WatchingSince: p.Stats.Meta.StartedWatchingAt,
		LastDeployed:  p.Stats.Updates.LastUpdatedAt,
		LastChecked:   p.Stats.Queries.LastQueriedAt,
		Checks:        p.Stats.Queries.QueryCount,
		LastError:     p.LastError(),
		LastErrorAt:   p.Stats.Queries.LastErrorAt,
	}
}

func (d *Daemon) Add(ctx context.Context, name string, url string) (control.Project, error) {
	p, err := d.projects.Add(name, url)
	if err != nil {
		return control.Project{}, projectError(err, name, url)
	}
	return toProject(p), nil
}

func (d *Daemon) Remove(ctx context.Context, name string) error {
	return projectError(d.projects.Remove(name), name, "")
}

func (d *Daemon) Rename(ctx context.Context, name string, newName string) error {
	err := d.projects.Rename(name, newName)
	if errors.Is(err, watchlist.ErrNameTaken) || errors.Is(err, watchlist.ErrInvalidName) {
		return projectError(err, newName, "")
	}
	return projectError(err, name, "")
}

func (d *Daemon) SetURL(ctx context.Context, name string, url string) error {
	return projectError(d.projects.SetURL(name, url), name, url)
}

// projectError turns a watchlist error into a message that says how to fix it.
func projectError(err error, name string, url string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, watchlist.ErrNotFound):
		return control.Errorf(control.KindNotFound, "No project named %q. \"list\" shows the watched projects.", name)
	case errors.Is(err, watchlist.ErrNameTaken):
		return control.Errorf(control.KindConflict, "A project named %q already exists. Pick another name.", name)
	case errors.Is(err, watchlist.ErrInvalidName):
		return control.Errorf(control.KindInvalid, "%q isn't a valid project name: use letters, digits, - and _ (up to 64), starting with a letter or digit.", name)
	case errors.Is(err, github.ErrInvalidURL):
		return control.Errorf(control.KindInvalid, "%q isn't a GitHub repository URL. Use https://github.com/<owner>/<repo>.", url)
	case errors.Is(err, watchlist.ErrURLWatched):
		return control.Errorf(control.KindConflict, "%s is already watched under another name. \"list\" shows which.", url)
	default:
		return err
	}
}

// find returns the project called name, or a not-found error.
func (d *Daemon) find(name string) (watchlist.Project, error) {
	p, ok := d.projects.Find(name)
	if !ok {
		return p, projectError(watchlist.ErrNotFound, name, "")
	}
	return p, nil
}

func (d *Daemon) Deploy(ctx context.Context, name string) error {
	if err := d.running(); err != nil {
		return err
	}
	if _, err := d.find(name); err != nil {
		return err
	}
	if err := d.orchestrator.Deploy(ctx, name); err != nil {
		return control.Errorf(control.KindInternal, "Deploying %s failed: %v. \"docker logs lighthouse\" has the full output.", name, err)
	}
	return nil
}

func (d *Daemon) Scan(ctx context.Context) error {
	if err := d.running(); err != nil {
		return err
	}
	err := d.orchestrator.Scan(ctx)
	switch {
	case errors.Is(err, orchestrator.ErrScanRunning):
		return control.Errorf(control.KindConflict, "A scan is already running. Try again when it's done.")
	case err != nil:
		return control.Errorf(control.KindInternal, "Scan finished with errors (%v). \"list\" shows each project's last error.", err)
	}
	return nil
}

func (d *Daemon) Pause(ctx context.Context) error {
	d.orchestrator.Pause()
	return nil
}

func (d *Daemon) Resume(ctx context.Context) error {
	d.orchestrator.Resume()
	return nil
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
	p, err := d.find(name)
	if err != nil {
		return err
	}

	if err := fn(ctx, p.Container()); err != nil {
		if docker.IsNotFound(err) {
			return control.Errorf(control.KindNotFound, "%s has no container named %q yet. \"deploy %s\" creates it.", p.Name, p.Container(), p.Name)
		}
		return control.Errorf(control.KindInternal, "Couldn't %s %s: %v", verb, p.Name, err)
	}
	return nil
}

func (d *Daemon) Logs(ctx context.Context, name string, lines int) (string, error) {
	if lines < 1 || lines > MaxLogLines {
		return "", control.Errorf(control.KindInvalid, "Lines must be between 1 and %d.", MaxLogLines)
	}
	p, err := d.find(name)
	if err != nil {
		return "", err
	}

	logs, err := d.containers.Logs(ctx, p.Container(), lines)
	if err != nil {
		if docker.IsNotFound(err) {
			return "", control.Errorf(control.KindNotFound, "%s has no container yet. \"deploy %s\" creates it.", p.Name, p.Name)
		}
		return "", fmt.Errorf("logs for %s: %w", p.Name, err)
	}
	return logs, nil
}
