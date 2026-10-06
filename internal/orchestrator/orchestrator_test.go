package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/LSariol/LightHouse/internal/github"
	"github.com/LSariol/LightHouse/internal/projects"
	"github.com/LSariol/LightHouse/internal/projects/projectstest"
)

// fakeCommits answers with a fixed commit per repository name; others fail.
type fakeCommits map[string]string

func (f fakeCommits) LatestCommit(ctx context.Context, apiURL string, token string) (string, error) {
	if token == "" {
		return "", errors.New("no token")
	}
	for name, sha := range f {
		if strings.HasSuffix(apiURL, "/"+name) {
			return sha, nil
		}
	}
	return "", errors.New("GitHub: 404 Not Found")
}

// fakeDeployer records deploys and fails for the names in fail.
type fakeDeployer struct {
	mu       sync.Mutex
	deployed []string
	fail     map[string]bool
	during   func(p projects.Project) // runs inside each deploy
}

func (f *fakeDeployer) Deploy(ctx context.Context, p projects.Project) error {
	if f.during != nil {
		f.during(p)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deployed = append(f.deployed, p.Name)
	if f.fail[p.Name] {
		return fmt.Errorf("build of %s failed", p.Name)
	}
	return nil
}

func setup(t *testing.T, commits fakeCommits, names ...string) (*Orchestrator, *projectstest.Store, *fakeDeployer) {
	t.Helper()
	store := &projectstest.Store{}
	for _, name := range names {
		if _, err := store.Add(context.Background(), name, github.Repo{Owner: "o", Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	d := &fakeDeployer{fail: map[string]bool{}}
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
	if strings.Join(d.deployed, ",") != "a,c" {
		t.Errorf("deployed %v, want [a c]", d.deployed)
	}
	if b := get(t, store, "b"); b.LastError == "" || b.Checks != 1 {
		t.Errorf("b: error %q, %d checks", b.LastError, b.Checks)
	}
	if a := get(t, store, "a"); a.DeployedSHA != "aaa" || a.Checks != 1 || a.LastError != "" {
		t.Errorf("a: %+v", a)
	}
	if h, _ := store.History(ctx, "a", 10); len(h) != 1 || h[0].Trigger != projects.TriggerCheck || h[0].Status != projects.StatusSucceeded || h[0].SHA != "aaa" {
		t.Errorf("a's history = %+v", h)
	}

	// Nothing changed on GitHub: nothing deploys again.
	d.deployed = nil
	o.Scan(ctx)
	if len(d.deployed) != 0 {
		t.Errorf("second scan deployed %v", d.deployed)
	}
}

func TestFailedDeployIsRetried(t *testing.T) {
	o, store, d := setup(t, fakeCommits{"a": "aaa"}, "a")
	d.fail["a"] = true
	ctx := context.Background()

	o.Scan(ctx)
	if a := get(t, store, "a"); a.DeployedSHA != "" || !strings.Contains(a.LastError, "build of a failed") {
		t.Errorf("after a failed deploy: %+v", a)
	}

	d.fail["a"] = false
	if err := o.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if a := get(t, store, "a"); a.DeployedSHA != "aaa" || a.LastError != "" {
		t.Errorf("after a successful deploy: %+v", a)
	}

	h, _ := store.History(ctx, "a", 10)
	if len(h) != 2 || h[0].Status != projects.StatusSucceeded || h[1].Status != projects.StatusFailed || h[1].Error == "" {
		t.Errorf("history = %+v", h)
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
	if len(d.deployed) != 0 {
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
	d.during = func(p projects.Project) {
		if p.Name == "a" {
			store.Remove(context.Background(), "a")
		}
	}

	if err := o.Scan(context.Background()); err != nil {
		t.Errorf("Scan = %v", err)
	}
	list, _ := store.List(context.Background())
	if len(list) != 1 || list[0].Name != "b" || list[0].DeployedSHA != "bbb" {
		t.Fatalf("projects = %+v, want only b at bbb", list)
	}
}

func TestDeployNow(t *testing.T) {
	o, store, d := setup(t, fakeCommits{"a": "aaa"}, "a")
	ctx := context.Background()

	if err := o.Deploy(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	if a := get(t, store, "a"); a.DeployedSHA != "aaa" {
		t.Errorf("deployed %q", a.DeployedSHA)
	}

	d.fail["a"] = true
	if err := o.Deploy(ctx, "a"); err == nil {
		t.Error("a failing Deploy returned no error")
	}
	h, _ := store.History(ctx, "a", 10)
	if len(h) != 2 || h[0].Trigger != projects.TriggerManual || h[0].Status != projects.StatusFailed {
		t.Errorf("history = %+v", h)
	}

	if err := o.Deploy(ctx, "nope"); !errors.Is(err, projects.ErrNotFound) {
		t.Errorf("Deploy of an unknown project = %v", err)
	}
}
