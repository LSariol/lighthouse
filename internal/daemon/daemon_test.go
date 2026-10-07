package daemon

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/lighthouse/internal/config"
	"github.com/lsariol/lighthouse/internal/control"
	"github.com/lsariol/lighthouse/internal/deploy"
	"github.com/lsariol/lighthouse/internal/docker"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/orchestrator"
	"github.com/lsariol/lighthouse/internal/projects"
	"github.com/lsariol/lighthouse/internal/projects/projectstest"
)

// fakeContainers holds containers per compose project and records actions.
type fakeContainers struct {
	byProject map[string][]docker.Container
	actions   []string
}

func (f *fakeContainers) ProjectContainers(ctx context.Context, project string) ([]docker.Container, error) {
	return f.byProject[project], nil
}
func (f *fakeContainers) act(verb string, id string) error {
	f.actions = append(f.actions, verb+" "+id)
	return nil
}
func (f *fakeContainers) Start(ctx context.Context, id string) error   { return f.act("start", id) }
func (f *fakeContainers) Stop(ctx context.Context, id string) error    { return f.act("stop", id) }
func (f *fakeContainers) Restart(ctx context.Context, id string) error { return f.act("restart", id) }
func (f *fakeContainers) Logs(ctx context.Context, id string, tail int) (string, error) {
	return "output of " + id + "\n", nil
}

type fakeCompose struct{ downs []string }

func (f *fakeCompose) Down(ctx context.Context, dir string, project string, out io.Writer) error {
	f.downs = append(f.downs, project)
	return nil
}

// fakeHealth is a database that answers, at the given schema versions.
type fakeHealth struct{ have, want int64 }

func (f fakeHealth) Ping(ctx context.Context) error { return nil }
func (f fakeHealth) SchemaVersion(ctx context.Context) (int64, int64, error) {
	return f.have, f.want, nil
}

type fakeDeployer struct{}

func (fakeDeployer) Deploy(ctx context.Context, req deploy.Request) deploy.Result {
	return deploy.Result{Status: projects.StatusSucceeded}
}

func (fakeDeployer) Check(ctx context.Context, req deploy.Request) deploy.Result {
	return deploy.Result{Status: projects.StatusFailed, FailedStep: deploy.StepCheck, FailureKind: projects.FailurePermanent,
		Err:   errors.New("check: the compose file breaks the deploy rules (privileged)"),
		Steps: []projects.Step{{Name: deploy.StepCheck, Status: projects.StepFailed, Log: "✗ web: privileged: true"}}}
}

func (fakeDeployer) HandOffs() ([]deploy.HandOff, error) { return nil, nil }
func (fakeDeployer) Forget(deploy.HandOff) error         { return nil }

type fakeCommits struct{}

func (fakeCommits) CheckCommit(ctx context.Context, repo github.Repo, token string, etag string) (string, string, error) {
	return "abc", "", nil
}
func (fakeCommits) ResolveCommit(ctx context.Context, repo github.Repo, ref string, token string) (string, error) {
	return ref, nil
}
func (fakeCommits) Tags(ctx context.Context, repo github.Repo, token string, etag string) ([]github.Tag, string, error) {
	return nil, "", nil
}
func (fakeCommits) ComposeFile(ctx context.Context, repo github.Repo, sha string, token string) (string, []byte, error) {
	return "compose.yaml", []byte("services: {}\n"), nil
}

func (fakeCommits) LatestCommit(ctx context.Context, repo github.Repo, token string) (string, error) {
	return "abc", nil
}

// newDaemon returns a running daemon on an in-memory store, with
// personalWebsite (compose project "website": web, www, bot) watched.
func newDaemon(t *testing.T) (*Daemon, *fakeContainers, *fakeCompose, *projectstest.Store) {
	t.Helper()
	store := &projectstest.Store{}
	c := &fakeContainers{byProject: map[string][]docker.Container{
		"website": {
			{ID: "id-bot", Name: "bot", Service: "bot", State: "running"},
			{ID: "id-web", Name: "web", Service: "web", State: "running", Health: "healthy"},
			{ID: "id-www", Name: "www", Service: "www", State: "running"},
		},
	}}
	cmp := &fakeCompose{}
	d := New(config.Config{Env: "dev", StagingPath: t.TempDir()}, "test", c, cmp)
	d.Ready(store, orchestrator.New(store, fakeCommits{}, fakeDeployer{}, nil, "t"), fakeHealth{3, 3})

	ctx := context.Background()
	if _, err := d.Add(ctx, "personalWebsite", "https://github.com/lsariol/landing"); err != nil {
		t.Fatal(err)
	}
	store.SetComposeProject(ctx, "personalWebsite", "website")
	return d, c, cmp, store
}

func kind(err error) string {
	var e *control.Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

func TestTargets(t *testing.T) {
	d, c, _, _ := newDaemon(t)
	ctx := context.Background()

	// The whole project: every service.
	if err := d.Restart(ctx, "personalwebsite"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.actions, ",") != "restart id-bot,restart id-web,restart id-www" {
		t.Errorf("actions = %v", c.actions)
	}

	// One service.
	c.actions = nil
	if err := d.Stop(ctx, "personalWebsite:bot"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.actions, ",") != "stop id-bot" {
		t.Errorf("actions = %v", c.actions)
	}

	err := d.Stop(ctx, "personalWebsite:db")
	if kind(err) != control.KindNotFound || !strings.Contains(err.Error(), "Its services: bot, web, www") {
		t.Errorf("an unknown service = %v", err)
	}
	if err := d.Stop(ctx, "sparkdb"); kind(err) != control.KindNotFound {
		t.Errorf("an unwatched name = %v", err)
	}

	// Stopping one service isn't stopping the project; stopping the whole
	// project is, until it's started (or restarted) again.
	if p, _ := d.Projects(ctx); p[0].Stopped {
		t.Error("stopping one service marked the project stopped")
	}
	d.Stop(ctx, "personalWebsite")
	if p, _ := d.Projects(ctx); !p[0].Stopped {
		t.Error("stop didn't mark the project stopped")
	}
	d.Start(ctx, "personalwebsite")
	if p, _ := d.Projects(ctx); p[0].Stopped {
		t.Error("start didn't clear stopped")
	}

	// Logs of several services come with headers; of one, without.
	logs, err := d.Logs(ctx, "personalWebsite", 10)
	if err != nil || !strings.Contains(logs, "==> personalWebsite:web <==\noutput of id-web") {
		t.Errorf("Logs = %q, %v", logs, err)
	}
	if logs, _ := d.Logs(ctx, "personalWebsite:web", 10); logs != "output of id-web\n" {
		t.Errorf("Logs of one service = %q", logs)
	}
}

func TestNoContainers(t *testing.T) {
	d, c, _, _ := newDaemon(t)
	c.byProject = map[string][]docker.Container{}
	err := d.Start(context.Background(), "personalWebsite")
	if kind(err) != control.KindNotFound || !strings.Contains(err.Error(), `compose project "website"`) || !strings.Contains(err.Error(), "deploy personalWebsite") {
		t.Errorf("Start with no containers = %v", err)
	}
}

func TestStatusSummarizesServices(t *testing.T) {
	d, c, _, _ := newDaemon(t)
	ctx := context.Background()

	state := func() string {
		s, err := d.Status(ctx)
		if err != nil || len(s.Projects) != 1 {
			t.Fatalf("Status = %+v, %v", s, err)
		}
		return s.Projects[0].State
	}

	if got := state(); got != "running" {
		t.Errorf("all running: %q", got)
	}
	s, _ := d.Status(ctx)
	if svc := s.Projects[0].Services; len(svc) != 3 || svc[1].Name != "web" || svc[1].Health != "healthy" {
		t.Errorf("services = %+v", svc)
	}

	// A one-off job that finished doesn't count against it.
	c.byProject["website"] = append(c.byProject["website"], docker.Container{ID: "id-mig", Service: "migrate", State: "exited", ExitCode: 0})
	if got := state(); got != "running" {
		t.Errorf("with a finished job: %q", got)
	}

	c.byProject["website"][0].Health = "unhealthy"
	if got := state(); got != "degraded" {
		t.Errorf("one unhealthy: %q", got)
	}

	for i := range c.byProject["website"] {
		c.byProject["website"][i].State, c.byProject["website"][i].ExitCode = "exited", 137
	}
	if got := state(); got != "stopped" {
		t.Errorf("all stopped: %q", got)
	}

	c.byProject = map[string][]docker.Container{}
	if got := state(); got != "missing" {
		t.Errorf("none: %q", got)
	}
}

func TestRemove(t *testing.T) {
	d, _, cmp, store := newDaemon(t)
	ctx := context.Background()

	if err := d.Remove(ctx, "personalWebsite", true); err != nil {
		t.Fatal(err)
	}
	if strings.Join(cmp.downs, ",") != "website" {
		t.Errorf("Down = %v, want the compose project", cmp.downs)
	}
	if _, err := store.Get(ctx, "personalWebsite"); !errors.Is(err, projects.ErrNotFound) {
		t.Error("still watched")
	}
}

func TestErrorsExplainTheFix(t *testing.T) {
	d, _, _, _ := newDaemon(t)
	ctx := context.Background()

	cases := []struct {
		err      error
		kind     string
		contains string
	}{
		{d.Remove(ctx, "nope", false), control.KindNotFound, `"list"`},
		{d.Rename(ctx, "personalWebsite", "bad name"), control.KindInvalid, "letters, digits"},
		{d.SetURL(ctx, "personalWebsite", "https://gitlab.com/a/b"), control.KindInvalid, "https://github.com/<owner>/<repo>"},
		{func() error { _, err := d.Add(ctx, "personalWebsite", "https://github.com/a/b"); return err }(), control.KindNameTaken, "already exists"},
		{func() error { _, err := d.Add(ctx, "x", "https://github.com/lsariol/landing"); return err }(), control.KindConflict, "already watched"},
		{func() error { _, err := d.Logs(ctx, "personalWebsite", 0); return err }(), control.KindInvalid, "between 1 and"},
		{func() error { _, err := d.History(ctx, "personalWebsite", 1000); return err }(), control.KindInvalid, "between 1 and"},
		{func() error { _, err := d.Report(ctx, "personalWebsite", 1); return err }(), control.KindNotFound, "fewer than 1 deploys"},
	}
	for i, c := range cases {
		if kind(c.err) != c.kind || !strings.Contains(c.err.Error(), c.contains) {
			t.Errorf("case %d: %v (kind %q), want kind %q mentioning %q", i, c.err, kind(c.err), c.kind, c.contains)
		}
	}
}

// Before Ready, everything but status says Lighthouse is starting.
func TestBeforeReady(t *testing.T) {
	d := New(config.Config{}, "test", &fakeContainers{}, &fakeCompose{})
	d.SetPhase(PhaseDatabase)
	ctx := context.Background()

	status, err := d.Status(ctx)
	if err != nil || status.Phase != PhaseDatabase || status.GitHubToken || len(status.Projects) != 0 {
		t.Errorf("Status = %+v, %v", status, err)
	}
	for name, err := range map[string]error{
		"deploy":   d.Deploy(ctx, "plop", ""),
		"rollback": func() error { _, err := d.Rollback(ctx, "plop"); return err }(),
		"retry":    d.Retry(ctx, "plop"),
		"scan":     d.Scan(ctx),
		"pause":    d.Pause(ctx),
		"remove":   d.Remove(ctx, "plop", false),
		"stop":     d.Stop(ctx, "plop"),
	} {
		if kind(err) != control.KindUnavailable || !strings.Contains(err.Error(), PhaseDatabase) {
			t.Errorf("%s before Ready = %v, want unavailable naming the phase", name, err)
		}
	}
}

func TestCheck(t *testing.T) {
	d, _, _, store := newDaemon(t)
	ctx := context.Background()

	r, err := d.Check(ctx, "PERSONALWEBSITE")
	if err != nil || r.Commit != "abc" || r.FailedStep != "check" || !strings.Contains(r.Error, "privileged") ||
		len(r.Steps) != 1 || r.Steps[0].Log != "✗ web: privileged: true" {
		t.Errorf("Check = %+v, %v", r, err)
	}
	if h, _ := store.History(ctx, "personalWebsite", 10); len(h) != 0 {
		t.Error("a check was recorded as a deploy")
	}
	if _, err := d.Check(ctx, "nope"); kind(err) != control.KindNotFound {
		t.Errorf("Check of an unknown project = %v", err)
	}
}

func TestHistoryAndReport(t *testing.T) {
	d, _, _, store := newDaemon(t)
	ctx := context.Background()
	now := time.Now()
	store.RecordDeployment(ctx, projects.Deployment{Project: "personalWebsite", SHA: "abc", Trigger: projects.TriggerManual,
		Status: projects.StatusRolledBack, FailureKind: projects.FailurePermanent, FailedStep: "verify",
		StartedAt: now, FinishedAt: now, Error: "web is unhealthy",
		Steps: []projects.Step{{Name: "verify", Status: projects.StepFailed, Log: "web is unhealthy"}}})

	h, err := d.History(ctx, "PERSONALWEBSITE", 10)
	if err != nil || len(h) != 1 || h[0].Status != "rolled_back" || h[0].FailedStep != "verify" || h[0].Steps != nil {
		t.Errorf("History = %+v, %v", h, err)
	}
	r, err := d.Report(ctx, "personalWebsite", 1)
	if err != nil || len(r.Steps) != 1 || r.Steps[0].Log != "web is unhealthy" {
		t.Errorf("Report = %+v, %v", r, err)
	}
}
