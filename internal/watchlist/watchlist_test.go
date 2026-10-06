package watchlist

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LSariol/LightHouse/internal/github"
)

func newList(t *testing.T, contents string) *List {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repos.json")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestAddRemoveRenameSetURL(t *testing.T) {
	l := newList(t, "[]")

	p, err := l.Add("plop", "https://github.com/LSariol/Plop")
	if err != nil {
		t.Fatal(err)
	}
	if p.Container() != "plop" || p.APIURL != "https://api.github.com/repos/LSariol/Plop" {
		t.Errorf("new project: container %q, API URL %q", p.Container(), p.APIURL)
	}

	if _, err := l.Add("PLOP", "https://github.com/LSariol/other"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("Add with a name differing only in case = %v, want ErrNameTaken", err)
	}
	if _, err := l.Add("other", "https://github.com/lsariol/PLOP.git"); !errors.Is(err, ErrURLWatched) {
		t.Errorf("Add of the same repository = %v, want ErrURLWatched", err)
	}
	if _, err := l.Add("bad name", "https://github.com/a/b"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("Add with a space in the name = %v, want ErrInvalidName", err)
	}
	if _, err := l.Add("x", "https://gitlab.com/a/b"); !errors.Is(err, github.ErrInvalidURL) {
		t.Errorf("Add with a GitLab URL = %v, want github.ErrInvalidURL", err)
	}

	if _, ok := l.Find("Plop"); !ok {
		t.Error("Find isn't case-insensitive")
	}

	if err := l.Rename("plop", "plop-web"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetURL("plop-web", "https://github.com/LSariol/plop-site"); err != nil {
		t.Fatal(err)
	}
	p, _ = l.Find("plop-web")
	if p.Container() != "plop-site" || p.DownloadURL != "https://github.com/LSariol/plop-site/archive/refs/heads/main.zip" {
		t.Errorf("after SetURL: container %q, download %q", p.Container(), p.DownloadURL)
	}

	// Every change was saved: a fresh load of the file sees it.
	again, err := Load(l.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Find("plop-web"); !ok {
		t.Error("the file doesn't have the renamed project")
	}

	if err := l.Remove("PLOP-WEB"); err != nil {
		t.Fatal(err)
	}
	if err := l.Remove("plop-web"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Remove = %v, want ErrNotFound", err)
	}
}

func TestRecording(t *testing.T) {
	l := newList(t, "[]")
	l.Add("a", "https://github.com/o/a")

	l.Update("a", func(p *Project) {
		p.RecordCheck()
		p.RecordError(errors.New("GitHub: 404 Not Found"))
	})
	a, _ := l.Find("a")
	if a.LastError() != "GitHub: 404 Not Found" || a.Stats.Queries.QueryCount != 1 {
		t.Errorf("after an error: %q, %d checks", a.LastError(), a.Stats.Queries.QueryCount)
	}

	l.Update("a", func(p *Project) { p.RecordDeploy("abc") })
	a, _ = l.Find("a")
	if a.Commit() != "abc" || a.LastError() != "" || a.Stats.Updates.UpdateCount != 1 {
		t.Errorf("after a deploy: commit %q, error %q, %d deploys", a.Commit(), a.LastError(), a.Stats.Updates.UpdateCount)
	}

	if l.Update("gone", func(*Project) {}) {
		t.Error("Update of a missing project reported success")
	}
}

// A repos.json written by the pre-1.0 Lighthouse loads unchanged, and saving
// keeps its field names.
func TestOldFileFormat(t *testing.T) {
	old := `[
	{
		"displayName": "cove",
		"containerName": "Cove",
		"url": "https://github.com/LSariol/Cove",
		"apiURL": "https://api.github.com/repos/LSariol/Cove",
		"downloadURL": "https://github.com/LSariol/Cove/archive/refs/heads/main.zip",
		"stats": {
			"meta": {"startedWatchingAt": "2025-08-05T12:14:29.9115454-04:00", "lastModifiedAt": null},
			"queries": {"lastQueriedAt": "2025-10-01T14:12:06.87-04:00", "queryCount": 119, "lastErrorAt": null, "lastErrorMessage": null},
			"updates": {"lastUpdatedAt": "2025-08-05T12:15:09.13-04:00", "lastSeenCommitSha": "2827a7556a3a50e06ebd3d69ab83f00ee57137ef", "lastSeenTag": null, "updateCount": 2},
			"builds": {"lastBuildAt": null, "lastBuildStatus": "Success", "buildTriggeredCount": 2},
			"downloads": {"lastDownloadAt": null, "lastDownloadStatus": null, "downloadTriggeredCount": 0}
		}
	}
]`
	l := newList(t, old)
	cove, ok := l.Find("cove")
	if !ok || cove.Container() != "cove" || cove.Commit() != "2827a7556a3a50e06ebd3d69ab83f00ee57137ef" || cove.Stats.Queries.QueryCount != 119 {
		t.Fatalf("loaded %+v", cove)
	}

	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(l.path)
	for _, field := range []string{`"displayName": "cove"`, `"containerName": "Cove"`, `"lastBuildStatus": "Success"`} {
		if !strings.Contains(string(saved), field) {
			t.Errorf("saved file lost %s", field)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
	path := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(path, []byte("{not json"), 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "isn't a valid watchlist") {
		t.Errorf("bad file: %v", err)
	}
}
