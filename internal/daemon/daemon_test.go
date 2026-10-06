package daemon

import (
	"context"
	"errors"

	cerrdefs "github.com/containerd/errdefs"
	"strings"
	"testing"
	"time"

	"github.com/LSariol/LightHouse/internal/config"
	"github.com/LSariol/LightHouse/internal/control"
	"github.com/LSariol/LightHouse/internal/orchestrator"
	"github.com/LSariol/LightHouse/internal/projects"
	"github.com/LSariol/LightHouse/internal/projects/projectstest"
)

// fakeContainers records container actions.
type fakeContainers struct {
	actions []string
	missing bool // every container is missing
}

func (f *fakeContainers) Start(ctx context.Context, name string) error {
	f.actions = append(f.actions, "start "+name)
	return nil
}
func (f *fakeContainers) Stop(ctx context.Context, name string) error {
	if f.missing {
		return cerrdefs.ErrNotFound
	}
	f.actions = append(f.actions, "stop "+name)
	return nil
}
func (f *fakeContainers) Restart(ctx context.Context, name string) error {
	f.actions = append(f.actions, "restart "+name)
	return nil
}
func (f *fakeContainers) State(ctx context.Context, name string) (string, error) {
	return "running", nil
}
func (f *fakeContainers) Logs(ctx context.Context, name string, tail int) (string, error) {
	return "", nil
}

// fakeHealth is a database that answers, at the given schema versions.
type fakeHealth struct{ have, want int64 }

func (f fakeHealth) Ping(ctx context.Context) error { return nil }
func (f fakeHealth) SchemaVersion(ctx context.Context) (int64, int64, error) {
	return f.have, f.want, nil
}

// newDaemon returns a running daemon on an in-memory store.
func newDaemon(t *testing.T) (*Daemon, *fakeContainers, *projectstest.Store) {
	t.Helper()
	store := &projectstest.Store{}
	c := &fakeContainers{}
	d := New(config.Config{Env: "dev"}, "test", c)
	d.Ready(store, orchestrator.New(store, nil, nil, "t"), fakeHealth{2, 2})
	return d, c, store
}

func kind(err error) string {
	var e *control.Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

func TestOnlyWatchedProjectsAreControlled(t *testing.T) {
	d, c, _ := newDaemon(t)
	ctx := context.Background()

	if err := d.Stop(ctx, "sparkdb"); kind(err) != control.KindNotFound {
		t.Errorf("Stop of an unwatched container = %v, want not_found", err)
	}

	if _, err := d.Add(ctx, "Plop", "https://github.com/LSariol/Plop"); err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(ctx, "plop"); err != nil {
		t.Fatal(err)
	}
	// The container is the lowercased repository name.
	if len(c.actions) != 1 || c.actions[0] != "stop plop" {
		t.Errorf("actions = %v, want [stop plop]", c.actions)
	}
}

func TestErrorsExplainTheFix(t *testing.T) {
	d, _, _ := newDaemon(t)
	ctx := context.Background()
	d.Add(ctx, "plop", "https://github.com/LSariol/plop")

	cases := []struct {
		err      error
		kind     string
		contains string
	}{
		{d.Remove(ctx, "nope"), control.KindNotFound, `"list"`},
		{d.Rename(ctx, "plop", "bad name"), control.KindInvalid, "letters, digits"},
		{d.SetURL(ctx, "plop", "https://gitlab.com/a/b"), control.KindInvalid, "https://github.com/<owner>/<repo>"},
		{func() error { _, err := d.Add(ctx, "plop", "https://github.com/a/b"); return err }(), control.KindConflict, "already exists"},
		{func() error { _, err := d.Add(ctx, "x", "https://github.com/LSariol/plop"); return err }(), control.KindConflict, "already watched"},
		{func() error { _, err := d.Logs(ctx, "plop", 0); return err }(), control.KindInvalid, "between 1 and"},
		{func() error { _, err := d.History(ctx, "plop", 1000); return err }(), control.KindInvalid, "between 1 and"},
		{func() error { _, err := d.History(ctx, "nope", 10); return err }(), control.KindNotFound, `"list"`},
	}
	for i, c := range cases {
		if kind(c.err) != c.kind || !strings.Contains(c.err.Error(), c.contains) {
			t.Errorf("case %d: %v (kind %q), want kind %q mentioning %q", i, c.err, kind(c.err), c.kind, c.contains)
		}
	}
}

// Before Ready, everything but status says Lighthouse is starting.
func TestBeforeReady(t *testing.T) {
	d := New(config.Config{}, "test", &fakeContainers{})
	d.SetPhase(PhaseDatabase)
	ctx := context.Background()

	status, err := d.Status(ctx)
	if err != nil || status.Phase != PhaseDatabase || status.GitHubToken || len(status.Projects) != 0 {
		t.Errorf("Status = %+v, %v", status, err)
	}

	for name, err := range map[string]error{
		"deploy":   d.Deploy(ctx, "plop"),
		"scan":     d.Scan(ctx),
		"pause":    d.Pause(ctx),
		"remove":   d.Remove(ctx, "plop"),
		"projects": func() error { _, err := d.Projects(ctx); return err }(),
	} {
		if kind(err) != control.KindUnavailable || !strings.Contains(err.Error(), PhaseDatabase) {
			t.Errorf("%s before Ready = %v, want unavailable naming the phase", name, err)
		}
	}
}

func TestStatusWhenRunning(t *testing.T) {
	d, _, _ := newDaemon(t)
	ctx := context.Background()
	d.Add(ctx, "plop", "https://github.com/LSariol/plop")

	s, err := d.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Phase != PhaseRunning || !s.GitHubToken || s.Database != "reachable" || s.Schema != "version 2 (up to date)" ||
		len(s.Projects) != 1 || s.Projects[0].State != "running" {
		t.Errorf("Status = %+v", s)
	}
}

func TestHistory(t *testing.T) {
	d, _, store := newDaemon(t)
	ctx := context.Background()
	d.Add(ctx, "plop", "https://github.com/LSariol/plop")
	now := time.Now()
	store.RecordDeployment(ctx, projects.Deployment{Project: "plop", SHA: "abc", Trigger: projects.TriggerManual,
		Status: projects.StatusFailed, StartedAt: now, FinishedAt: now, Error: "build failed"})

	h, err := d.History(ctx, "PLOP", 10)
	if err != nil || len(h) != 1 || h[0].Commit != "abc" || h[0].Error != "build failed" || h[0].Trigger != "manual" {
		t.Errorf("History = %+v, %v", h, err)
	}
}

// A project whose compose file names its containers differently (here: repo
// "landing", containers "web", "www", "bot") gets an explanation, not the
// advice to deploy it.
func TestMissingContainerExplained(t *testing.T) {
	d, c, _ := newDaemon(t)
	c.missing = true
	ctx := context.Background()
	d.Add(ctx, "personalWebsite", "https://github.com/lsariol/landing")

	err := d.Stop(ctx, "personalWebsite")
	if kind(err) != control.KindNotFound {
		t.Fatalf("Stop = %v, want not_found", err)
	}
	for _, want := range []string{`"landing"`, "deploy personalWebsite", "names its containers differently", "docker compose"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q: %v", want, err)
		}
	}
}
