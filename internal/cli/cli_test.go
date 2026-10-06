package cli

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LSariol/LightHouse/internal/control"
)

// fakeService is a control.Service with fixed projects that records calls.
type fakeService struct {
	projects []control.Project
	status   control.Status
	calls    []string
	failFor  map[string]bool // project names whose actions fail
}

func newFake(names ...string) *fakeService {
	f := &fakeService{failFor: map[string]bool{}}
	for _, n := range names {
		f.projects = append(f.projects, control.Project{Name: n, URL: "https://github.com/o/" + n, ComposeProject: n, State: "running",
			Services: []control.ServiceStatus{{Name: "web", Container: n + "-web-1", State: "running", Health: "healthy"}}})
	}
	f.status = control.Status{Version: "v1", Env: "dev", Phase: "running", GitHubToken: true, Database: "reachable",
		Schema: "version 2 (up to date)", StartedAt: time.Now(), Projects: f.projects}
	return f
}

func (f *fakeService) act(call string, name string) error {
	f.calls = append(f.calls, call+" "+name)
	if f.failFor[name] {
		return control.Errorf(control.KindInternal, "%s failed", name)
	}
	return nil
}

func (f *fakeService) Status(ctx context.Context) (control.Status, error) { return f.status, nil }
func (f *fakeService) Projects(ctx context.Context) ([]control.Project, error) {
	return f.projects, nil
}
func (f *fakeService) Add(ctx context.Context, name, url string) (control.Project, error) {
	return control.Project{Name: name, URL: url, ComposeProject: name}, f.act("add", name)
}
func (f *fakeService) Remove(ctx context.Context, name string, down bool) error {
	if down {
		return f.act("remove --down", name)
	}
	return f.act("remove", name)
}
func (f *fakeService) Retry(ctx context.Context, name string) error { return f.act("retry", name) }
func (f *fakeService) Report(ctx context.Context, name string, n int) (control.Deployment, error) {
	start := time.Now().Add(-time.Hour)
	return control.Deployment{Commit: "abcdef123", Trigger: "check", Status: "rolled_back", FailureKind: "permanent", FailedStep: "verify",
		StartedAt: start, FinishedAt: start.Add(2 * time.Minute), Error: "verify: web is unhealthy; rolled back to 1111111",
		Steps: []control.Step{
			{Name: "build", Status: "succeeded", StartedAt: start, FinishedAt: start.Add(time.Minute), Log: "built website-web\n"},
			{Name: "verify", Status: "failed", StartedAt: start, FinishedAt: start.Add(time.Minute), Log: "✗ web is unhealthy\n"},
		}}, f.act("report", name)
}
func (f *fakeService) Rename(ctx context.Context, name, newName string) error {
	return f.act("rename", name)
}
func (f *fakeService) SetURL(ctx context.Context, name, url string) error {
	return f.act("set-url", name)
}
func (f *fakeService) Deploy(ctx context.Context, name string) error  { return f.act("deploy", name) }
func (f *fakeService) Scan(ctx context.Context) error                 { return f.act("scan", "") }
func (f *fakeService) Pause(ctx context.Context) error                { return f.act("pause", "") }
func (f *fakeService) Resume(ctx context.Context) error               { return f.act("resume", "") }
func (f *fakeService) Start(ctx context.Context, name string) error   { return f.act("start", name) }
func (f *fakeService) Stop(ctx context.Context, name string) error    { return f.act("stop", name) }
func (f *fakeService) Restart(ctx context.Context, name string) error { return f.act("restart", name) }
func (f *fakeService) Logs(ctx context.Context, name string, lines int) (string, error) {
	return "hello\n", f.act("logs", name)
}
func (f *fakeService) History(ctx context.Context, name string, limit int) ([]control.Deployment, error) {
	start := time.Now().Add(-time.Hour)
	return []control.Deployment{
		{Commit: "abcdef123", Trigger: "check", Status: "failed", FailedStep: "secrets", StartedAt: start, FinishedAt: start.Add(90 * time.Second),
			Error: "secrets: missing: PLOP_DATABASE_URL\nmore detail"},
	}, f.act("history", name)
}

// run executes one command line on a CLI without a terminal and returns its
// stdout, stderr and error.
func run(t *testing.T, svc control.Service, line string) (string, string, error) {
	t.Helper()
	var o, e bytes.Buffer
	oldOut, oldErr, oldColor := stdout, stderr, useColor
	stdout, stderr, useColor = &o, &e, false
	defer func() { stdout, stderr, useColor = oldOut, oldErr, oldColor }()

	c := New(svc, Options{})
	c.interactive = false
	err := c.Exec(context.Background(), strings.Fields(line))
	return o.String(), e.String(), err
}

func TestCommandTable(t *testing.T) {
	seen := map[string]bool{}
	for _, cmd := range commandTable(false) {
		if !slices.Contains(groupOrder, cmd.group) {
			t.Errorf("%s: group %q isn't in groupOrder", cmd.names[0], cmd.group)
		}
		if cmd.summary == "" || len(cmd.usages) == 0 || cmd.run == nil {
			t.Errorf("%s: needs a summary, at least one usage and a run function", cmd.names[0])
		}
		for _, name := range cmd.names {
			if seen[name] {
				t.Errorf("name %q is used twice", name)
			}
			seen[name] = true
		}
	}
}

// Help is wrapped at 80 characters, everywhere.
func TestHelpWidth(t *testing.T) {
	topics := []string{"help"}
	for _, cmd := range commandTable(false) {
		topics = append(topics, "help "+cmd.names[0])
	}
	for _, g := range guides {
		topics = append(topics, "help "+g.name)
	}

	for _, topic := range topics {
		out, _, err := run(t, newFake(), topic)
		if err != nil {
			t.Errorf("%s: %v", topic, err)
		}
		for _, line := range strings.Split(out, "\n") {
			if n := len([]rune(line)); n > helpWidth {
				t.Errorf("%s: line is %d characters: %q", topic, n, line)
			}
		}
	}
}

func TestHelpCommand(t *testing.T) {
	out, _, err := run(t, newFake(), "help stop")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"stop: ", "Usage:", "Flags:", "--yes, -y", "Examples:"} {
		if !strings.Contains(out, want) {
			t.Errorf("help stop doesn't contain %q:\n%s", want, out)
		}
	}

	_, _, err = run(t, newFake(), "help nonsense")
	var usage usageError
	if !errors.As(err, &usage) {
		t.Errorf("help nonsense = %v, want a usage error", err)
	}
}

func TestUnknownAndWrongUsage(t *testing.T) {
	if _, _, err := run(t, newFake(), "frobnicate"); err == nil || !strings.Contains(err.Error(), "help") {
		t.Errorf("unknown command: %v", err)
	}
	_, _, err := run(t, newFake(), "add onlyname")
	var usage usageError
	if !errors.As(err, &usage) || !strings.HasPrefix(err.Error(), "Usage: add") {
		t.Errorf("add with one argument = %v, want \"Usage: add ...\"", err)
	}
	if _, _, err := run(t, newFake(), "logs plop many"); !errors.As(err, &usage) {
		t.Errorf("logs with a bad count = %v, want a usage error", err)
	}
}

// Without a terminal, destructive commands refuse instead of guessing, and
// --yes goes ahead.
func TestConfirmation(t *testing.T) {
	for _, line := range []string{"remove plop", "stop plop", "stop all", "restart all", "deploy all"} {
		svc := newFake("plop")
		_, _, err := run(t, svc, line)
		if err == nil || !strings.Contains(err.Error(), "--yes") {
			t.Errorf("%s without a terminal = %v, want a refusal pointing at --yes", line, err)
		}
		if len(svc.calls) != 0 {
			t.Errorf("%s without confirmation called %v", line, svc.calls)
		}

		svc = newFake("plop")
		if _, _, err := run(t, svc, line+" --yes"); err != nil {
			t.Errorf("%s --yes: %v", line, err)
		}
		if len(svc.calls) == 0 {
			t.Errorf("%s --yes didn't do anything", line)
		}
	}

	// Commands that aren't destructive don't ask.
	svc := newFake("plop")
	if _, _, err := run(t, svc, "restart plop"); err != nil || len(svc.calls) != 1 {
		t.Errorf("restart plop: %v, calls %v", err, svc.calls)
	}
}

func TestAllReportsEachProject(t *testing.T) {
	svc := newFake("a", "b", "c")
	svc.failFor["b"] = true

	_, errOut, err := run(t, svc, "start all")
	if err == nil || !strings.Contains(err.Error(), "1 of 3") {
		t.Errorf("start all = %v, want \"1 of 3 projects failed\"", err)
	}
	if !strings.Contains(errOut, "✓ Started a.") || !strings.Contains(errOut, "✗ b failed") || !strings.Contains(errOut, "✓ Started c.") {
		t.Errorf("messages:\n%s", errOut)
	}
}

func TestOutputStreams(t *testing.T) {
	// Data on stdout, messages on stderr.
	out, errOut, err := run(t, newFake("plop"), "list")
	if err != nil || !strings.Contains(out, "plop") || errOut != "" {
		t.Errorf("list: out %q, err %q, %v", out, errOut, err)
	}

	out, errOut, err = run(t, newFake("plop"), "logs plop")
	if err != nil || out != "hello\n" || errOut != "" {
		t.Errorf("logs: out %q, err %q, %v", out, errOut, err)
	}

	out, errOut, err = run(t, newFake("plop"), "pause")
	if err != nil || out != "" || !strings.HasPrefix(errOut, "✓ ") {
		t.Errorf("pause: out %q, err %q, %v", out, errOut, err)
	}
}

func TestStatus(t *testing.T) {
	svc := newFake("plop")
	out, errOut, err := run(t, svc, "status")
	if err != nil {
		t.Fatalf("healthy status = %v", err)
	}
	if !strings.Contains(out, "plop") || !strings.Contains(errOut, "healthy") {
		t.Errorf("out %q, err %q", out, errOut)
	}

	svc.status.Projects[0].State = "degraded"
	svc.status.Projects[0].LastError = "GitHub: 404 Not Found"
	_, _, err = run(t, svc, "status")
	if err == nil || !strings.Contains(err.Error(), "2 things need attention") {
		t.Errorf("unhealthy status = %v, want two problems", err)
	}
}

func TestPrompt(t *testing.T) {
	old := useColor
	useColor = false
	defer func() { useColor = old }()

	for env, want := range map[string]string{"": "lighthouse> ", "DEV": "lighthouse (dev)> ", "prod": "lighthouse (prod)> "} {
		if got := promptFor(env); got != want {
			t.Errorf("promptFor(%q) = %q, want %q", env, got, want)
		}
	}

	useColor = true
	if got := promptFor("prod"); !strings.Contains(got, red) {
		t.Errorf("the prod prompt isn't red: %q", got)
	}
}

func TestCompletion(t *testing.T) {
	c := New(newFake("plop", "plex"), Options{})

	line, pos, ok := c.complete("dep", 3, '\t')
	if !ok || line != "deploy " || pos != len("deploy ") {
		t.Errorf("complete(dep) = %q, %d, %v", line, pos, ok)
	}

	line, _, ok = c.complete("deploy plo", len("deploy plo"), '\t')
	if !ok || line != "deploy plop " {
		t.Errorf("complete(deploy plo) = %q, %v", line, ok)
	}

	line, _, ok = c.complete("stop a", len("stop a"), '\t')
	if !ok || line != "stop all " {
		t.Errorf("complete(stop a) = %q, %v", line, ok)
	}

	if _, _, ok := c.complete("help se", len("help se"), '\t'); !ok {
		t.Error("help topics don't complete")
	}
}

func TestTakeYesFlag(t *testing.T) {
	yes, rest := takeYesFlag([]string{"plop", "-y"})
	if !yes || len(rest) != 1 || rest[0] != "plop" {
		t.Errorf("takeYesFlag = %v, %v", yes, rest)
	}
}

func TestHistory(t *testing.T) {
	out, _, err := run(t, newFake("plop"), "history plop")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"WHEN", "check", "failed at secrets", "abcdef1", "1m30s", "missing: PLOP_DATABASE_URL"} {
		if !strings.Contains(out, want) {
			t.Errorf("history output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "more detail") {
		t.Error("history printed more than the error's first line")
	}

	var usage usageError
	if _, _, err := run(t, newFake("plop"), "history plop lots"); !errors.As(err, &usage) {
		t.Errorf("history with a bad count = %v, want a usage error", err)
	}
}

func TestStatusDatabase(t *testing.T) {
	svc := newFake("plop")
	out, _, _ := run(t, svc, "status")
	if !strings.Contains(out, "reachable, schema version 2 (up to date)") {
		t.Errorf("status doesn't show the database:\n%s", out)
	}

	svc.status.Database = "unreachable"
	svc.status.Schema = ""
	if _, _, err := run(t, svc, "status"); err == nil || !strings.Contains(err.Error(), "database is unreachable") {
		t.Errorf("status with the database down = %v", err)
	}
}

func TestEmbeddedExit(t *testing.T) {
	for _, embedded := range []bool{false, true} {
		var exit command
		for _, cmd := range commandTable(embedded) {
			if cmd.names[0] == "exit" {
				exit = cmd
			}
		}
		stops := strings.Contains(exit.summary, "Stop Lighthouse")
		if stops != embedded {
			t.Errorf("embedded=%v: exit summary %q", embedded, exit.summary)
		}
	}

	stopped := false
	c := New(newFake(), Options{Embedded: true})
	c.leave = func() { stopped = true }
	if err := c.Exec(context.Background(), []string{"exit"}); err != nil || !stopped {
		t.Errorf("exit: %v, stopped %v", err, stopped)
	}
}

func TestRemoveDown(t *testing.T) {
	svc := newFake("plop")
	if _, _, err := run(t, svc, "remove plop --down --yes"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(svc.calls, ",") != "remove --down plop" {
		t.Errorf("calls = %v", svc.calls)
	}
}

func TestReport(t *testing.T) {
	out, _, err := run(t, newFake("plop"), "report plop")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rolled back at verify", "counts toward broken", "== build: succeeded", "built website-web",
		"== verify: failed", "✗ web is unhealthy", "rolled back to 1111111"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	var usage usageError
	if _, _, err := run(t, newFake("plop"), "report plop zero"); !errors.As(err, &usage) {
		t.Errorf("report with a bad number = %v", err)
	}
}

func TestStatusShowsServicesAndBroken(t *testing.T) {
	svc := newFake("plop")
	out, _, err := run(t, svc, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "web") || !strings.Contains(out, "running, healthy") {
		t.Errorf("status doesn't list the service:\n%s", out)
	}

	svc.status.Projects[0].Broken = true
	svc.status.Projects[0].LastError = "broken after 3 failed deploys"
	_, _, err = run(t, svc, "status")
	if err == nil || !strings.Contains(err.Error(), "plop is broken") || !strings.Contains(err.Error(), "retry plop") {
		t.Errorf("status of a broken project = %v", err)
	}
}

func TestTargetCompletion(t *testing.T) {
	c := New(newFake("plop"), Options{})
	line, _, ok := c.complete("logs plop:", len("logs plop:"), '\t')
	if !ok || line != "logs plop:web " {
		t.Errorf("complete(logs plop:) = %q, %v", line, ok)
	}
}
