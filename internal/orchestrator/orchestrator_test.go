package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/LSariol/LightHouse/internal/watchlist"
)

// fakeCommits answers with a fixed commit per API URL; others fail.
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
	during   func(p watchlist.Project) // runs inside each deploy
}

func (f *fakeDeployer) Deploy(ctx context.Context, p watchlist.Project) error {
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

func setup(t *testing.T, commits fakeCommits, names ...string) (*Orchestrator, *watchlist.List, *fakeDeployer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repos.json")
	os.WriteFile(path, []byte("[]"), 0o644)
	list, err := watchlist.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if _, err := list.Add(name, "https://github.com/o/"+name); err != nil {
			t.Fatal(err)
		}
	}
	d := &fakeDeployer{fail: map[string]bool{}}
	o := New(list, commits, d)
	o.SetGitToken("t")
	return o, list, d
}

func TestScanDeploysNewCommitsAndSkipsFailures(t *testing.T) {
	// "b" doesn't exist on GitHub: the projects after it are still checked.
	o, list, d := setup(t, fakeCommits{"a": "aaa", "c": "ccc"}, "a", "b", "c")

	err := o.Scan(context.Background())
	if err == nil || !strings.Contains(err.Error(), "b") {
		t.Errorf("Scan = %v, want an error naming b", err)
	}
	if strings.Join(d.deployed, ",") != "a,c" {
		t.Errorf("deployed %v, want [a c]", d.deployed)
	}
	if b, _ := list.Find("b"); b.LastError() == "" {
		t.Error("b's error wasn't recorded")
	}
	if a, _ := list.Find("a"); a.Commit() != "aaa" || a.Stats.Queries.QueryCount != 1 {
		t.Errorf("a: commit %q, %d checks", a.Commit(), a.Stats.Queries.QueryCount)
	}

	// Nothing changed on GitHub: nothing deploys again.
	d.deployed = nil
	o.Scan(context.Background())
	if len(d.deployed) != 0 {
		t.Errorf("second scan deployed %v", d.deployed)
	}
}

func TestFailedDeployIsRetried(t *testing.T) {
	o, list, d := setup(t, fakeCommits{"a": "aaa"}, "a")
	d.fail["a"] = true

	o.Scan(context.Background())
	a, _ := list.Find("a")
	if a.Commit() != "" || a.LastError() == "" {
		t.Errorf("after a failed deploy: commit %q, error %q", a.Commit(), a.LastError())
	}

	d.fail["a"] = false
	if err := o.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, _ = list.Find("a")
	if a.Commit() != "aaa" || a.LastError() != "" {
		t.Errorf("after a successful deploy: commit %q, error %q", a.Commit(), a.LastError())
	}
	if len(d.deployed) != 2 {
		t.Errorf("deployed %v, want two attempts", d.deployed)
	}
}

func TestNoDeployWithoutToken(t *testing.T) {
	o, _, d := setup(t, fakeCommits{"a": "aaa"}, "a")
	o.SetGitToken("")
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

// Removing a project while a scan runs must not touch another project's
// entry (the pre-1.0 code wrote results back by position).
func TestRemoveDuringScan(t *testing.T) {
	o, list, d := setup(t, fakeCommits{"a": "aaa", "b": "bbb"}, "a", "b")
	d.during = func(p watchlist.Project) {
		if p.Name == "a" {
			list.Remove("a")
		}
	}

	o.Scan(context.Background())

	all := list.All()
	if len(all) != 1 || all[0].Name != "b" || all[0].Commit() != "bbb" {
		t.Fatalf("watchlist = %+v, want only b at bbb", all)
	}
}

func TestDeployNow(t *testing.T) {
	o, list, d := setup(t, fakeCommits{"a": "aaa"}, "a")

	if err := o.Deploy(context.Background(), "A"); err != nil {
		t.Fatal(err)
	}
	if a, _ := list.Find("a"); a.Commit() != "aaa" {
		t.Errorf("commit after Deploy = %q", a.Commit())
	}

	d.fail["a"] = true
	if err := o.Deploy(context.Background(), "a"); err == nil {
		t.Error("a failing Deploy returned no error")
	}
	if a, _ := list.Find("a"); a.LastError() == "" {
		t.Error("the failed Deploy wasn't recorded")
	}

	if err := o.Deploy(context.Background(), "nope"); !errors.Is(err, watchlist.ErrNotFound) {
		t.Errorf("Deploy of an unknown project = %v", err)
	}
}
