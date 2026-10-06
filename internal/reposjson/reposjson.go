// Package reposjson reads the watchlist file of the pre-1.0 Lighthouse
// (repos.json), so `lighthouse import` can move it into the database.
package reposjson

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/LSariol/LightHouse/internal/github"
	"github.com/LSariol/LightHouse/internal/projects"
)

// entry is one project as repos.json stored it. Fields the old Lighthouse
// never filled in (builds, downloads, lastSeenTag) are left out.
type entry struct {
	DisplayName string `json:"displayName"`
	URL         string `json:"url"`
	Stats       struct {
		Meta struct {
			StartedWatchingAt time.Time `json:"startedWatchingAt"`
		} `json:"meta"`
		Queries struct {
			LastQueriedAt    *time.Time `json:"lastQueriedAt"`
			QueryCount       int64      `json:"queryCount"`
			LastErrorAt      *time.Time `json:"lastErrorAt"`
			LastErrorMessage *string    `json:"lastErrorMessage"`
		} `json:"queries"`
		Updates struct {
			LastUpdatedAt     *time.Time `json:"lastUpdatedAt"`
			LastSeenCommitSha *string    `json:"lastSeenCommitSha"`
		} `json:"updates"`
	} `json:"stats"`
}

// Read parses a repos.json and returns its projects with their recorded
// state: when watching started, the deployed commit, checks and last error.
func Read(r io.Reader) ([]projects.Project, error) {
	var entries []entry
	if err := json.NewDecoder(r).Decode(&entries); err != nil {
		return nil, fmt.Errorf("not a valid repos.json: %v", err)
	}

	list := make([]projects.Project, 0, len(entries))
	for i, e := range entries {
		repo, err := github.ParseRepoURL(e.URL)
		if err != nil {
			return nil, fmt.Errorf("entry %d (%q): %q isn't a GitHub repository URL", i+1, e.DisplayName, e.URL)
		}

		p := projects.Project{
			Name:          e.DisplayName,
			Repo:          repo,
			CreatedAt:     e.Stats.Meta.StartedWatchingAt,
			DeployedAt:    e.Stats.Updates.LastUpdatedAt,
			LastCheckedAt: e.Stats.Queries.LastQueriedAt,
			Checks:        e.Stats.Queries.QueryCount,
			LastErrorAt:   e.Stats.Queries.LastErrorAt,
		}
		if sha := e.Stats.Updates.LastSeenCommitSha; sha != nil {
			p.DeployedSHA = *sha
		}
		if msg := e.Stats.Queries.LastErrorMessage; msg != nil {
			p.LastError = *msg
		}
		list = append(list, p)
	}
	return list, nil
}
