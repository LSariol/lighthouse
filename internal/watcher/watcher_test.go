package watcher

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/LSariol/LightHouse/internal/models"
)

func TestParseRepoURL(t *testing.T) {
	good := []string{
		"https://github.com/LSariol/plop",
		"https://github.com/LSariol/plop/",
		"https://github.com/LSariol/plop.git",
		"http://github.com/LSariol/plop",
		"https://www.github.com/LSariol/plop",
		"github.com/LSariol/plop",
		"  https://github.com/LSariol/plop  ",
	}
	for _, raw := range good {
		gh, err := ParseRepoURL(raw)
		if err != nil {
			t.Errorf("ParseRepoURL(%q): %v", raw, err)
			continue
		}
		if gh.URL() != "https://github.com/LSariol/plop" {
			t.Errorf("ParseRepoURL(%q).URL() = %q", raw, gh.URL())
		}
	}

	bad := []string{
		"", "https://gitlab.com/a/b", "https://github.com/onlyowner",
		"https://github.com/a/b/tree/main", "https://github.com/a b/c", "git@github.com:a/b.git",
	}
	for _, raw := range bad {
		if _, err := ParseRepoURL(raw); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("ParseRepoURL(%q) = %v, want ErrInvalidURL", raw, err)
		}
	}

	gh, _ := ParseRepoURL("https://github.com/LSariol/plop")
	if gh.APIURL() != "https://api.github.com/repos/LSariol/plop" {
		t.Errorf("APIURL = %q", gh.APIURL())
	}
	if gh.DownloadURL() != "https://github.com/LSariol/plop/archive/refs/heads/main.zip" {
		t.Errorf("DownloadURL = %q", gh.DownloadURL())
	}
}

// newWatcher returns a Watcher on an empty watchlist file in a temp folder.
func newWatcher(t *testing.T, d Deployer) *Watcher {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repos.json")
	if err := os.WriteFile(path, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := New(http.DefaultClient, d, path)
	if err := w.Load(); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWatchlist(t *testing.T) {
	w := newWatcher(t, nil)

	if _, err := w.Add("plop", "https://github.com/LSariol/plop"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Add("PLOP", "https://github.com/LSariol/other"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("Add with a name differing only in case = %v, want ErrNameTaken", err)
	}
	if _, err := w.Add("other", "https://github.com/lsariol/PLOP.git"); !errors.Is(err, ErrURLWatched) {
		t.Errorf("Add of the same repository = %v, want ErrURLWatched", err)
	}
	if _, err := w.Add("bad name", "https://github.com/a/b"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("Add with a space in the name = %v, want ErrInvalidName", err)
	}

	if _, ok := w.Find("Plop"); !ok {
		t.Error("Find isn't case-insensitive")
	}

	if err := w.Rename("plop", "plop-web"); err != nil {
		t.Fatal(err)
	}
	if err := w.SetURL("plop-web", "https://github.com/LSariol/plop-site"); err != nil {
		t.Fatal(err)
	}
	repo, _ := w.Find("plop-web")
	if repo.ContainerName != "plop-site" || repo.APIURL != "https://api.github.com/repos/LSariol/plop-site" {
		t.Errorf("after SetURL: container %q, API URL %q", repo.ContainerName, repo.APIURL)
	}

	// Everything was saved: a fresh Watcher on the same file sees it.
	w2 := New(http.DefaultClient, nil, w.repoPath)
	if err := w2.Load(); err != nil {
		t.Fatal(err)
	}
	if _, ok := w2.Find("plop-web"); !ok {
		t.Error("the watchlist file doesn't have the renamed project")
	}

	if err := w.Remove("PLOP-WEB"); err != nil {
		t.Fatal(err)
	}
	if err := w.Remove("plop-web"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Remove = %v, want ErrNotFound", err)
	}
}

// fakeDeployer records deploys and fails for the names in fail.
type fakeDeployer struct {
	mu       sync.Mutex
	deployed []string
	fail     map[string]bool
}

func (f *fakeDeployer) Build(ctx context.Context, repo models.WatchedRepo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deployed = append(f.deployed, repo.DisplayName)
	if f.fail[repo.DisplayName] {
		return errors.New("build failed")
	}
	return nil
}

// fakeGitHub serves /repos/<owner>/<name>/commits: a commit for repos in
// shas, 404 for the rest.
func fakeGitHub(t *testing.T, shas map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/o/"), "/commits")
		sha, ok := shas[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode([]map[string]string{{"sha": sha}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// watch adds projects whose API URLs point at the fake GitHub.
func watch(t *testing.T, w *Watcher, gh *httptest.Server, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := w.Add(name, "https://github.com/o/"+name); err != nil {
			t.Fatal(err)
		}
		w.update(name, func(r *models.WatchedRepo) { r.APIURL = gh.URL + "/repos/o/" + name })
	}
}

func TestScanDeploysNewCommits(t *testing.T) {
	gh := fakeGitHub(t, map[string]string{"a": "aaaaaaaa", "c": "cccccccc"})
	d := &fakeDeployer{fail: map[string]bool{}}
	w := newWatcher(t, d)
	w.SetGitToken("test-token")

	// "b" doesn't exist on GitHub: the projects after it are still checked.
	watch(t, w, gh, "a", "b", "c")

	err := w.Scan(context.Background())
	if err == nil || !strings.Contains(err.Error(), "b") {
		t.Errorf("Scan = %v, want an error naming b", err)
	}
	if strings.Join(d.deployed, ",") != "a,c" {
		t.Errorf("deployed %v, want [a c]", d.deployed)
	}

	b, _ := w.Find("b")
	if b.Stats.Queries.LastErrorMessage == nil {
		t.Error("b's error wasn't recorded")
	}
	a, _ := w.Find("a")
	if a.Stats.Updates.LastSeenCommitSha == nil || *a.Stats.Updates.LastSeenCommitSha != "aaaaaaaa" {
		t.Error("a's commit wasn't recorded")
	}

	// Nothing changed on GitHub: nothing is deployed again.
	d.deployed = nil
	w.Scan(context.Background())
	if len(d.deployed) != 0 {
		t.Errorf("second scan deployed %v, want nothing", d.deployed)
	}
}

func TestScanFailedDeployIsRetried(t *testing.T) {
	gh := fakeGitHub(t, map[string]string{"a": "aaaaaaaa"})
	d := &fakeDeployer{fail: map[string]bool{"a": true}}
	w := newWatcher(t, d)
	w.SetGitToken("test-token")
	watch(t, w, gh, "a")

	w.Scan(context.Background())
	a, _ := w.Find("a")
	if a.Stats.Updates.LastSeenCommitSha != nil {
		t.Error("a failed deploy recorded the commit")
	}
	if a.Stats.Queries.LastErrorMessage == nil {
		t.Error("the failed deploy wasn't recorded as an error")
	}

	// Fixed: the next scan deploys it, records it and clears the error.
	d.fail["a"] = false
	if err := w.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, _ = w.Find("a")
	if a.Stats.Updates.LastSeenCommitSha == nil || a.Stats.Queries.LastErrorMessage != nil {
		t.Errorf("after a successful deploy: commit %v, error %v", a.Stats.Updates.LastSeenCommitSha, a.Stats.Queries.LastErrorMessage)
	}
}

func TestScanWithoutToken(t *testing.T) {
	gh := fakeGitHub(t, map[string]string{"a": "aaaaaaaa"})
	d := &fakeDeployer{}
	w := newWatcher(t, d)
	watch(t, w, gh, "a")

	if err := w.Scan(context.Background()); err == nil {
		t.Error("Scan without a GitHub token succeeded")
	}
	if len(d.deployed) != 0 {
		t.Error("deployed without a GitHub token")
	}
}

func TestOneScanAtATime(t *testing.T) {
	w := newWatcher(t, &fakeDeployer{})
	w.scanning.Lock()
	defer w.scanning.Unlock()
	if err := w.Scan(context.Background()); !errors.Is(err, ErrScanRunning) {
		t.Errorf("Scan during a scan = %v, want ErrScanRunning", err)
	}
}

// Removing a project while a scan runs must not touch another project's
// entry (the old code wrote stats back by position).
func TestRemoveDuringScan(t *testing.T) {
	gh := fakeGitHub(t, map[string]string{"a": "aaaaaaaa", "b": "bbbbbbbb"})
	w := newWatcher(t, nil)
	w.SetGitToken("test-token")

	removed := make(chan struct{})
	w.deployer = deployFunc(func(ctx context.Context, repo models.WatchedRepo) error {
		if repo.DisplayName == "a" {
			w.Remove("a")
			close(removed)
		}
		return nil
	})
	watch(t, w, gh, "a", "b")

	w.Scan(context.Background())
	<-removed

	repos := w.Repos()
	if len(repos) != 1 || repos[0].DisplayName != "b" {
		t.Fatalf("watchlist = %+v, want only b", repos)
	}
	if sha := repos[0].Stats.Updates.LastSeenCommitSha; sha == nil || *sha != "bbbbbbbb" {
		t.Errorf("b's commit = %v, want bbbbbbbb", sha)
	}
}

type deployFunc func(ctx context.Context, repo models.WatchedRepo) error

func (f deployFunc) Build(ctx context.Context, repo models.WatchedRepo) error { return f(ctx, repo) }
