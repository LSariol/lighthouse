package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LSariol/LightHouse/internal/config"
	"github.com/LSariol/LightHouse/internal/control"
	"github.com/LSariol/LightHouse/internal/watcher"
)

// fakeContainers records container actions.
type fakeContainers struct {
	actions []string
}

func (f *fakeContainers) StartContainer(ctx context.Context, name string) error {
	f.actions = append(f.actions, "start "+name)
	return nil
}
func (f *fakeContainers) StopContainer(ctx context.Context, name string) error {
	f.actions = append(f.actions, "stop "+name)
	return nil
}
func (f *fakeContainers) RestartContainer(ctx context.Context, name string) error {
	f.actions = append(f.actions, "restart "+name)
	return nil
}
func (f *fakeContainers) ContainerState(ctx context.Context, name string) (string, error) {
	return "running", nil
}
func (f *fakeContainers) ContainerLogs(ctx context.Context, name string, tail int) (string, error) {
	return "", nil
}

func newDaemon(t *testing.T) (*Daemon, *fakeContainers) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repos.json")
	os.WriteFile(path, []byte("[]"), 0o644)
	w := watcher.New(http.DefaultClient, nil, path)
	if err := w.Load(); err != nil {
		t.Fatal(err)
	}
	c := &fakeContainers{}
	return New(config.Config{Env: "dev"}, "test", w, c), c
}

func kind(err error) string {
	var e *control.Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

func TestOnlyWatchedProjectsAreControlled(t *testing.T) {
	d, c := newDaemon(t)
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
	d, _ := newDaemon(t)
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
	}
	for i, c := range cases {
		if kind(c.err) != c.kind || !strings.Contains(c.err.Error(), c.contains) {
			t.Errorf("case %d: %v (kind %q), want kind %q mentioning %q", i, c.err, kind(c.err), c.kind, c.contains)
		}
	}
}

func TestDeployWaitsForStartup(t *testing.T) {
	d, _ := newDaemon(t)
	ctx := context.Background()
	d.Add(ctx, "plop", "https://github.com/LSariol/plop")
	d.SetPhase(PhaseWaiting)

	for name, err := range map[string]error{"deploy": d.Deploy(ctx, "plop"), "scan": d.Scan(ctx)} {
		if kind(err) != control.KindUnavailable || !strings.Contains(err.Error(), PhaseWaiting) {
			t.Errorf("%s while waiting for Cove = %v, want unavailable", name, err)
		}
	}

	status, _ := d.Status(ctx)
	if status.Phase != PhaseWaiting || len(status.Projects) != 1 || status.Projects[0].State != "running" {
		t.Errorf("Status = %+v", status)
	}
}
