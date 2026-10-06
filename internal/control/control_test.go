package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeService answers with fixed data and records what it was asked.
type fakeService struct {
	calls []string
}

func (f *fakeService) record(call string) { f.calls = append(f.calls, call) }

func (f *fakeService) Status(ctx context.Context) (Status, error) {
	return Status{Version: "v1.2.3", Phase: "running", PollInterval: 10 * time.Second,
		Projects: []Project{{Name: "plop", State: "running"}}}, nil
}
func (f *fakeService) Projects(ctx context.Context) ([]Project, error) {
	return []Project{{Name: "plop", URL: "https://github.com/LSariol/plop", Container: "plop"}}, nil
}
func (f *fakeService) Add(ctx context.Context, name, url string) (Project, error) {
	f.record("add " + name + " " + url)
	if name == "taken" {
		return Project{}, Errorf(KindConflict, "A project named %q already exists.", name)
	}
	return Project{Name: name, URL: url}, nil
}
func (f *fakeService) Remove(ctx context.Context, name string) error {
	f.record("remove " + name)
	return Errorf(KindNotFound, "No project named %q.", name)
}
func (f *fakeService) Rename(ctx context.Context, name, newName string) error {
	f.record("rename " + name + " " + newName)
	return nil
}
func (f *fakeService) SetURL(ctx context.Context, name, url string) error {
	f.record("set-url " + name + " " + url)
	return nil
}
func (f *fakeService) Deploy(ctx context.Context, name string) error {
	f.record("deploy " + name)
	return errors.New("something internal broke")
}
func (f *fakeService) Scan(ctx context.Context) error   { f.record("scan"); return nil }
func (f *fakeService) Pause(ctx context.Context) error  { f.record("pause"); return nil }
func (f *fakeService) Resume(ctx context.Context) error { f.record("resume"); return nil }
func (f *fakeService) Start(ctx context.Context, name string) error {
	f.record("start " + name)
	return nil
}
func (f *fakeService) Stop(ctx context.Context, name string) error {
	f.record("stop " + name)
	return nil
}
func (f *fakeService) Restart(ctx context.Context, name string) error {
	f.record("restart " + name)
	return nil
}
func (f *fakeService) Logs(ctx context.Context, name string, lines int) (string, error) {
	f.record("logs " + name)
	return "line 1\nline 2\n", nil
}
func (f *fakeService) History(ctx context.Context, name string, limit int) ([]Deployment, error) {
	f.record(fmt.Sprintf("history %s %d", name, limit))
	return []Deployment{{Commit: "abc", Trigger: "manual", Status: "failed", Error: "build failed"}}, nil
}

// serve starts Serve on a socket in a temp folder and returns a Client for it.
func serve(t *testing.T, svc Service) (*Client, string) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "c.sock")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- Serve(ctx, socket, svc) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	for i := 0; i < 100; i++ {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return NewClient(socket), socket
}

func TestRoundTrip(t *testing.T) {
	svc := &fakeService{}
	c, _ := serve(t, svc)
	ctx := context.Background()

	status, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Version != "v1.2.3" || status.PollInterval != 10*time.Second || len(status.Projects) != 1 {
		t.Errorf("Status = %+v", status)
	}

	projects, err := c.Projects(ctx)
	if err != nil || len(projects) != 1 || projects[0].Container != "plop" {
		t.Errorf("Projects = %+v, %v", projects, err)
	}

	p, err := c.Add(ctx, "new", "https://github.com/a/new")
	if err != nil || p.Name != "new" {
		t.Errorf("Add = %+v, %v", p, err)
	}

	logs, err := c.Logs(ctx, "plop", 20)
	if err != nil || logs != "line 1\nline 2\n" {
		t.Errorf("Logs = %q, %v", logs, err)
	}

	history, err := c.History(ctx, "plop", 5)
	if err != nil || len(history) != 1 || history[0].Error != "build failed" {
		t.Errorf("History = %+v, %v", history, err)
	}

	for _, call := range []func() error{
		func() error { return c.Rename(ctx, "plop", "plop2") },
		func() error { return c.SetURL(ctx, "plop", "https://github.com/a/b") },
		func() error { return c.Start(ctx, "plop") },
		func() error { return c.Stop(ctx, "plop") },
		func() error { return c.Restart(ctx, "plop") },
		func() error { return c.Scan(ctx) },
		func() error { return c.Pause(ctx) },
		func() error { return c.Resume(ctx) },
	} {
		if err := call(); err != nil {
			t.Error(err)
		}
	}

	want := []string{"add new https://github.com/a/new", "logs plop", "history plop 5", "rename plop plop2", "set-url plop https://github.com/a/b",
		"start plop", "stop plop", "restart plop", "scan", "pause", "resume"}
	if len(svc.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", svc.calls, want)
	}
	for i := range want {
		if svc.calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, svc.calls[i], want[i])
		}
	}
}

func TestErrorsKeepTheirKind(t *testing.T) {
	c, _ := serve(t, &fakeService{})
	ctx := context.Background()

	_, err := c.Add(ctx, "taken", "https://github.com/a/b")
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindConflict || e.Message != `A project named "taken" already exists.` {
		t.Errorf("Add error = %#v", err)
	}

	if err := c.Remove(ctx, "x"); !errors.As(err, &e) || e.Kind != KindNotFound {
		t.Errorf("Remove error = %#v", err)
	}

	// A plain error becomes an internal one, with its message.
	if err := c.Deploy(ctx, "plop"); !errors.As(err, &e) || e.Kind != KindInternal || e.Message != "something internal broke" {
		t.Errorf("Deploy error = %#v", err)
	}
}

func TestUnreachable(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "nobody.sock"))
	_, err := c.Status(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestSocketInUse(t *testing.T) {
	_, socket := serve(t, &fakeService{})
	if err := Serve(context.Background(), socket, &fakeService{}); err == nil {
		t.Error("a second Serve on the same socket succeeded")
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "c.sock")
	if err := os.WriteFile(socket, nil, 0o600); err != nil { // left behind, nobody listening
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, socket, &fakeService{}) }()

	c := NewClient(socket)
	var err error
	for i := 0; i < 100; i++ {
		if _, err = c.Status(ctx); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Errorf("Status over a replaced stale socket: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Error(err)
	}
}
