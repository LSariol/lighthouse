// Package daemon is the running Lighthouse: it owns the watcher and builder
// and answers the CLI's requests (control.Service).
package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/LSariol/LightHouse/internal/builder"
	"github.com/LSariol/LightHouse/internal/config"
	"github.com/LSariol/LightHouse/internal/control"
	"github.com/LSariol/LightHouse/internal/models"
	"github.com/LSariol/LightHouse/internal/watcher"
)

// Containers starts, stops and inspects containers. *builder.Builder
// implements it.
type Containers interface {
	StartContainer(ctx context.Context, name string) error
	StopContainer(ctx context.Context, name string) error
	RestartContainer(ctx context.Context, name string) error
	ContainerState(ctx context.Context, name string) (string, error)
	ContainerLogs(ctx context.Context, name string, tail int) (string, error)
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
	cfg        config.Config
	version    string
	watcher    *watcher.Watcher
	containers Containers
	started    time.Time
	phase      atomic.Value // string
}

func New(cfg config.Config, version string, w *watcher.Watcher, c Containers) *Daemon {
	d := &Daemon{cfg: cfg, version: version, watcher: w, containers: c, started: time.Now()}
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
		state, err := d.containers.ContainerState(ctx, projects[i].Container)
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
		Paused:       d.watcher.IsPaused(),
		PollInterval: d.cfg.PollInterval,
		CoveURL:      d.cfg.CoveURL,
		GitHubToken:  d.watcher.HasGitToken(),
		Projects:     projects,
	}, nil
}

func (d *Daemon) Projects(ctx context.Context) ([]control.Project, error) {
	repos := d.watcher.Repos()
	projects := make([]control.Project, 0, len(repos))
	for _, r := range repos {
		projects = append(projects, toProject(r))
	}
	return projects, nil
}

func toProject(r models.WatchedRepo) control.Project {
	p := control.Project{
		Name:          r.DisplayName,
		URL:           r.URL,
		Container:     builder.ContainerName(r),
		WatchingSince: r.Stats.Meta.StartedWatchingAt,
		LastDeployed:  r.Stats.Updates.LastUpdatedAt,
		LastChecked:   r.Stats.Queries.LastQueriedAt,
		Checks:        r.Stats.Queries.QueryCount,
		LastErrorAt:   r.Stats.Queries.LastErrorAt,
	}
	if sha := r.Stats.Updates.LastSeenCommitSha; sha != nil {
		p.Commit = *sha
	}
	if msg := r.Stats.Queries.LastErrorMessage; msg != nil {
		p.LastError = *msg
	}
	return p
}

func (d *Daemon) Add(ctx context.Context, name string, url string) (control.Project, error) {
	repo, err := d.watcher.Add(name, url)
	if err != nil {
		return control.Project{}, d.projectError(err, name, url)
	}
	return toProject(repo), nil
}

func (d *Daemon) Remove(ctx context.Context, name string) error {
	return d.projectError(d.watcher.Remove(name), name, "")
}

func (d *Daemon) Rename(ctx context.Context, name string, newName string) error {
	err := d.watcher.Rename(name, newName)
	if errors.Is(err, watcher.ErrNameTaken) || errors.Is(err, watcher.ErrInvalidName) {
		return d.projectError(err, newName, "")
	}
	return d.projectError(err, name, "")
}

func (d *Daemon) SetURL(ctx context.Context, name string, url string) error {
	return d.projectError(d.watcher.SetURL(name, url), name, url)
}

// projectError turns a watcher error into a message that says how to fix it.
func (d *Daemon) projectError(err error, name string, url string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, watcher.ErrNotFound):
		return control.Errorf(control.KindNotFound, "No project named %q. \"list\" shows the watched projects.", name)
	case errors.Is(err, watcher.ErrNameTaken):
		return control.Errorf(control.KindConflict, "A project named %q already exists. Pick another name.", name)
	case errors.Is(err, watcher.ErrInvalidName):
		return control.Errorf(control.KindInvalid, "%q isn't a valid project name: use letters, digits, - and _ (up to 64), starting with a letter or digit.", name)
	case errors.Is(err, watcher.ErrInvalidURL):
		return control.Errorf(control.KindInvalid, "%q isn't a GitHub repository URL. Use https://github.com/<owner>/<repo>.", url)
	case errors.Is(err, watcher.ErrURLWatched):
		return control.Errorf(control.KindConflict, "%s is already watched under another name. \"list\" shows which.", url)
	default:
		return err
	}
}

// find returns the project called name, or a not-found error.
func (d *Daemon) find(name string) (models.WatchedRepo, error) {
	repo, ok := d.watcher.Find(name)
	if !ok {
		return repo, d.projectError(watcher.ErrNotFound, name, "")
	}
	return repo, nil
}

func (d *Daemon) Deploy(ctx context.Context, name string) error {
	if err := d.running(); err != nil {
		return err
	}
	if _, err := d.find(name); err != nil {
		return err
	}
	if err := d.watcher.Deploy(ctx, name); err != nil {
		return control.Errorf(control.KindInternal, "Deploying %s failed: %v. \"docker logs lighthouse\" has the full output.", name, err)
	}
	return nil
}

func (d *Daemon) Scan(ctx context.Context) error {
	if err := d.running(); err != nil {
		return err
	}
	err := d.watcher.Scan(ctx)
	switch {
	case errors.Is(err, watcher.ErrScanRunning):
		return control.Errorf(control.KindConflict, "A scan is already running. Try again when it's done.")
	case err != nil:
		return control.Errorf(control.KindInternal, "Scan finished with errors (%v). \"list\" shows each project's last error.", err)
	}
	return nil
}

func (d *Daemon) Pause(ctx context.Context) error {
	d.watcher.Pause()
	return nil
}

func (d *Daemon) Resume(ctx context.Context) error {
	d.watcher.Resume()
	return nil
}

func (d *Daemon) Start(ctx context.Context, name string) error {
	return d.containerAction(ctx, name, "start", d.containers.StartContainer)
}

func (d *Daemon) Stop(ctx context.Context, name string) error {
	return d.containerAction(ctx, name, "stop", d.containers.StopContainer)
}

func (d *Daemon) Restart(ctx context.Context, name string) error {
	return d.containerAction(ctx, name, "restart", d.containers.RestartContainer)
}

// containerAction runs fn on a project's container. Only watched projects'
// containers can be controlled, never other containers on the host.
func (d *Daemon) containerAction(ctx context.Context, name string, verb string, fn func(context.Context, string) error) error {
	repo, err := d.find(name)
	if err != nil {
		return err
	}
	container := builder.ContainerName(repo)

	if err := fn(ctx, container); err != nil {
		if builder.IsNotFound(err) {
			return control.Errorf(control.KindNotFound, "%s has no container named %q yet. \"deploy %s\" creates it.", repo.DisplayName, container, repo.DisplayName)
		}
		return control.Errorf(control.KindInternal, "Couldn't %s %s: %v", verb, repo.DisplayName, err)
	}
	return nil
}

func (d *Daemon) Logs(ctx context.Context, name string, lines int) (string, error) {
	if lines < 1 || lines > MaxLogLines {
		return "", control.Errorf(control.KindInvalid, "Lines must be between 1 and %d.", MaxLogLines)
	}
	repo, err := d.find(name)
	if err != nil {
		return "", err
	}

	logs, err := d.containers.ContainerLogs(ctx, builder.ContainerName(repo), lines)
	if err != nil {
		if builder.IsNotFound(err) || strings.Contains(err.Error(), "No such container") {
			return "", control.Errorf(control.KindNotFound, "%s has no container yet. \"deploy %s\" creates it.", repo.DisplayName, repo.DisplayName)
		}
		return "", fmt.Errorf("logs for %s: %w", repo.DisplayName, err)
	}
	return logs, nil
}
