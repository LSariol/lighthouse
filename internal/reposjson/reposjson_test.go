package reposjson

import (
	"strings"
	"testing"

	"github.com/LSariol/LightHouse/internal/github"
)

// A file as the pre-1.0 Lighthouse wrote it.
const old = `[
	{
		"displayName": "cove",
		"containerName": "Cove",
		"url": "https://github.com/LSariol/Cove",
		"apiURL": "https://api.github.com/repos/LSariol/Cove",
		"downloadURL": "https://github.com/LSariol/Cove/archive/refs/heads/main.zip",
		"stats": {
			"meta": {"startedWatchingAt": "2025-08-05T12:14:29.9115454-04:00", "lastModifiedAt": null},
			"queries": {"lastQueriedAt": "2025-10-01T14:12:06.87-04:00", "queryCount": 119, "lastErrorAt": "2025-10-01T14:00:00-04:00", "lastErrorMessage": "GitHub API Error: 502"},
			"updates": {"lastUpdatedAt": "2025-08-05T12:15:09.13-04:00", "lastSeenCommitSha": "2827a7556a3a50e06ebd3d69ab83f00ee57137ef", "lastSeenTag": null, "updateCount": 2},
			"builds": {"lastBuildAt": null, "lastBuildStatus": "Success", "buildTriggeredCount": 2},
			"downloads": {"lastDownloadAt": null, "lastDownloadStatus": null, "downloadTriggeredCount": 0}
		}
	},
	{
		"displayName": "plop",
		"containerName": "plop",
		"url": "https://github.com/LSariol/plop",
		"stats": {"meta": {"startedWatchingAt": "2026-01-01T00:00:00Z"}, "queries": {}, "updates": {}}
	}
]`

func TestRead(t *testing.T) {
	list, err := Read(strings.NewReader(old))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("read %d projects, want 2", len(list))
	}

	cove := list[0]
	if cove.Name != "cove" || cove.Repo != (github.Repo{Owner: "LSariol", Name: "Cove"}) ||
		cove.DeployedSHA != "2827a7556a3a50e06ebd3d69ab83f00ee57137ef" || cove.Checks != 119 ||
		cove.LastError != "GitHub API Error: 502" || cove.DeployedAt == nil || cove.CreatedAt.Year() != 2025 {
		t.Errorf("cove = %+v", cove)
	}

	plop := list[1]
	if plop.DeployedSHA != "" || plop.DeployedAt != nil || plop.LastError != "" || plop.Checks != 0 {
		t.Errorf("plop (never deployed) = %+v", plop)
	}
}

func TestReadErrors(t *testing.T) {
	if _, err := Read(strings.NewReader("{not json")); err == nil {
		t.Error("invalid JSON was accepted")
	}
	_, err := Read(strings.NewReader(`[{"displayName": "x", "url": "https://gitlab.com/a/b"}]`))
	if err == nil || !strings.Contains(err.Error(), `"x"`) {
		t.Errorf("a non-GitHub URL: %v", err)
	}
	if list, err := Read(strings.NewReader("[]")); err != nil || len(list) != 0 {
		t.Errorf("an empty list: %v, %v", list, err)
	}
}
