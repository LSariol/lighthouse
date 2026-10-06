package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/lsariol/lighthouse/internal/deploy"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/projects"
	"github.com/lsariol/lighthouse/internal/projects/projectstest"
)

// fakeCommits answers with a commit per repository name; others fail.
type fakeCommits map[string]string

func (f fakeCommits) LatestCommit(ctx context.Context, repo github.Repo, token string) (string, error) {
	if token == "" {
		return "", errors.New("no token")
	}
	if sha, ok := f[repo.Name]; ok {
		return sha, nil
	}
	return "", errors.New("GitHub: 404 Not Found")
}

// fakeDeployer records deploys; result decides how each goes (success if nil).
type fakeDeployer struct {
	mu       sync.Mutex
	deployed []string // "name@sha"
	result   func(req deploy.Request) deploy.Result
}

func (f *fakeDeployer) Deploy(ctx context.Context, req deploy.Request) deploy.Result {
	f.mu.Lock()
	f.deployed = append(f.deployed, req.Project.Name+"@"+req.SHA)
	f.mu.Unlock()

	compose := strings.ToLower(req.Project.Repo.Name)
	if req.Claim != nil {
		if err := req.Claim(ctx, compose); err != nil {
			return deploy.Result{Status: projects.StatusFailed, FailureKind: projects.FailurePermanent, FailedStep: deploy.StepInspect, Err: err}
		}
	}
	if f.result != nil {
		return f.result(req)
	}
	return deploy.Result{Status: projects.StatusSucceeded, ComposeProject: compose,
		Steps: []projects.Step{{Name: deploy.StepFetch, Status: projects.StepSucceeded}}}
}

func (f *fakeDeployer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deployed)
}

func failing(kind string) func(deploy.Request) deploy.Result {
	return func(deploy.Request) deploy.Result {
		return deploy.Result{Status: projects.StatusFailed, FailureKind: kind, FailedStep: deploy.StepBuild,
			Err: errors.New("build: exit status 1")}
	}
}

func setup(t *testing.T, commits fakeCommits, names ...string) (*Orchestrator, *projectstest.Store, *fakeDeployer) {
	t.Helper()
	store := &projectstest.Store{}
	for _, name := range names {
		if _, err := store.Add(context.Background(), name, github.Repo{Owner: "o", Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	d := &fakeDeployer{}
	return New(store, commits, d, "t"), store, d
}

func get(t *testing.T, s projects.Store, name string) projects.Project {
	t.Helper()
	p, err := s.Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScanDeploysNewCommitsAndSkipsFailures(t *testing.T) {
	// "b" doesn't exist on GitHub: the projects after it are still checked.
	o, store, d := setup(t, fakeCommits{"a": "aaa", "c": "ccc"}, "a", "b", "c")
	ctx := context.Background()

	err := o.Scan(ctx)
	if err == nil || !strings.Contains(err.Error(), "b") {
		t.Errorf("Scan = %v, want an error naming b", err)
	}
	if strings.Join(d.deployed, ",") != "a@aaa,c@ccc" {
		t.Errorf("deployed %v", d.deployed)
	}
	if b := get(t, store, "b"); b.LastError == "" || b.Checks != 1 {
		t.Errorf("b: %+v", b)
	}
	a := get(t, store, "a")
	if a.DeployedSHA != "aaa" || a.ComposeProject != "a" || a.LastError != "" {
		t.Errorf("a: %+v", a)
	}
	if h, _ := store.Deployment(ctx, "a", 1); h.Trigger != projects.TriggerCheck || len(h.Steps) != 1 {
		t.Errorf("a's deployment = %+v", h)
	}

	// Nothing changed on GitHub: nothing deploys again.
	o.Scan(ctx)
	if d.count() != 2 {
		t.Errorf("second scan deployed %v", d.deployed)
	}
}

func TestBrokenAfterRepeatedFailures(t *testing.T) {
	commits := fakeCommits{"a": "aaa"}
	o, store, d := setup(t, commits, "a")
	d.result = failing(projects.FailurePermanent)
	ctx := context.Background()

	for i := 1; i <= projects.BrokenAfter; i++ {
		err := o.Scan(ctx)
		if i < projects.BrokenAfter && err == nil {
			t.Fatalf("scan %d succeeded", i)
		}
	}
	a := get(t, store, "a")
	if !a.Broken || a.FailureCount != projects.BrokenAfter || !strings.Contains(a.LastError, "broken after 3") {
		t.Fatalf("after %d failures: %+v", projects.BrokenAfter, a)
	}

	// Broken: the same commit isn't tried again, and the scan isn't an error.
	if err := o.Scan(ctx); err != nil {
		t.Errorf("scan of a broken project = %v", err)
	}
	if d.count() != projects.BrokenAfter {
		t.Errorf("deployed %d times, want %d", d.count(), projects.BrokenAfter)
	}
	if a := get(t, store, "a"); !strings.Contains(a.LastError, "build: exit status 1") {
		t.Errorf("the error that broke it was lost: %q", a.LastError)
	}

	// A new commit is tried.
	commits["a"] = "bbb"
	d.result = nil
	if err := o.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if a := get(t, store, "a"); a.Broken || a.DeployedSHA != "bbb" || a.FailureCount != 0 {
		t.Errorf("after a new commit deployed: %+v", a)
	}
}

func TestTransientFailuresDontBreak(t *testing.T) {
	o, store, d := setup(t, fakeCommits{"a": "aaa"}, "a")
	d.result = failing(projects.FailureTransient)
	for i := 0; i < projects.BrokenAfter+2; i++ {
		o.Scan(context.Background())
	}
	if a := get(t, store, "a"); a.Broken || a.FailureCount != 0 {
		t.Errorf("transient failures broke it: %+v", a)
	}
	if d.count() != projects.BrokenAfter+2 {
		t.Errorf("deployed %d times", d.count())
	}
}

func TestRetry(t *testing.T) {
	o, store, d := setup(t, fakeCommits{"a": "aaa"}, "a")
	d.result = failing(projects.FailurePermanent)
	for i := 0; i < projects.BrokenAfter; i++ {
		o.Scan(context.Background())
	}

	d.result = nil
	if err := o.Retry(context.Background(), "A"); err != nil {
		t.Fatal(err)
	}
	if a := get(t, store, "a"); a.Broken || a.DeployedSHA != "aaa" {
		t.Errorf("after retry: %+v", a)
	}
	if h, _ := store.Deployment(context.Background(), "a", 1); h.Trigger != projects.TriggerManual {
		t.Errorf("retry's trigger = %q", h.Trigger)
	}
}

func TestComposeProjectConflict(t *testing.T) {
	// Two projects whose repositories are both called "site" claim the same
	// compose project.
	store := &projectstest.Store{}
	ctx := context.Background()
	store.Add(ctx, "first", github.Repo{Owner: "x", Name: "site"})
	store.Add(ctx, "second", github.Repo{Owner: "y", Name: "site"})
	d := &fakeDeployer{}
	o := New(store, fakeCommits{"site": "aaa"}, d, "t")

	if err := o.Deploy(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	err := o.Deploy(ctx, "second")
	if err == nil || !strings.Contains(err.Error(), `"site" belongs to first already`) {
		t.Errorf("second deploy = %v, want it to name first", err)
	}
}

func TestNoDeployWithoutToken(t *testing.T) {
	store := &projectstest.Store{}
	store.Add(context.Background(), "a", github.Repo{Owner: "o", Name: "a"})
	d := &fakeDeployer{}
	o := New(store, fakeCommits{"a": "aaa"}, d, "")

	if err := o.Scan(context.Background()); err == nil {
		t.Error("Scan without a GitHub token succeeded")
	}
	if d.count() != 0 {
		t.Error("deployed without a GitHub token")
	}
}

func TestOneScanAtATime(t *testing.T) {
	o, _, _ := setup(t, fakeCommits{})
	o.scanning.Lock()
	defer o.scanning.Unlock()
	if err := o.Scan(context.Background()); !errors.Is(err, ErrScanRunning) {
		t.Errorf("Scan during a scan = %v, want ErrScanRunning", err)
	}
}

// Removing a project while it deploys must not fail the scan or touch
// another project.
func TestRemoveDuringScan(t *testing.T) {
	o, store, d := setup(t, fakeCommits{"a": "aaa", "b": "bbb"}, "a", "b")
	d.result = func(req deploy.Request) deploy.Result {
		if req.Project.Name == "a" {
			store.Remove(context.Background(), "a")
		}
		return deploy.Result{Status: projects.StatusSucceeded}
	}

	if err := o.Scan(context.Background()); err != nil {
		t.Errorf("Scan = %v", err)
	}
	list, _ := store.List(context.Background())
	if len(list) != 1 || list[0].Name != "b" || list[0].DeployedSHA != "bbb" {
		t.Fatalf("projects = %+v, want only b at bbb", list)
	}
}
