package watchlist

import (
	"strings"
	"time"

	"github.com/LSariol/LightHouse/internal/github"
)

// Project is one watched repository. The JSON field names are the ones
// repos.json has always used, so existing files load unchanged.
type Project struct {
	Name        string `json:"displayName"`   // Lighthouse's name for it, used in the CLI
	Repo        string `json:"containerName"` // the GitHub repository's name
	URL         string `json:"url"`
	APIURL      string `json:"apiURL"`
	DownloadURL string `json:"downloadURL"`
	Stats       Stats  `json:"stats"`
}

// Container is the name of the project's container: its repository's name,
// lowercased. The project's compose file must use it (container_name and the
// top-level name).
func (p Project) Container() string {
	return strings.ToLower(p.Repo)
}

// Commit is the last deployed commit, or "".
func (p Project) Commit() string {
	if sha := p.Stats.Updates.LastSeenCommitSha; sha != nil {
		return *sha
	}
	return ""
}

// LastError is the error from the last check or deploy, or "" if it worked.
func (p Project) LastError() string {
	if msg := p.Stats.Queries.LastErrorMessage; msg != nil {
		return *msg
	}
	return ""
}

// RecordCheck notes that GitHub was checked.
func (p *Project) RecordCheck() {
	p.Stats.Queries.LastQueriedAt = now()
	p.Stats.Queries.QueryCount++
}

// RecordError notes a failed check or deploy.
func (p *Project) RecordError(err error) {
	msg := err.Error()
	p.Stats.Queries.LastErrorAt = now()
	p.Stats.Queries.LastErrorMessage = &msg
}

// RecordDeploy notes a successful deploy of commit sha, which also clears the
// last error.
func (p *Project) RecordDeploy(sha string) {
	p.Stats.Updates.LastSeenCommitSha = &sha
	p.Stats.Updates.LastUpdatedAt = now()
	p.Stats.Updates.UpdateCount++
	p.ClearError()
}

// ClearError forgets the last error, after something worked.
func (p *Project) ClearError() {
	p.Stats.Queries.LastErrorAt = nil
	p.Stats.Queries.LastErrorMessage = nil
}

func (p *Project) touch() {
	p.Stats.Meta.LastModifiedAt = now()
}

func (p *Project) setRepo(repo github.Repo) {
	p.Repo = repo.Name
	p.URL = repo.URL()
	p.APIURL = repo.APIURL()
	p.DownloadURL = repo.ArchiveURL()
}

func newProject(name string, repo github.Repo) Project {
	p := Project{Name: name}
	p.setRepo(repo)
	p.Stats.Meta.StartedWatchingAt = time.Now()
	return p
}

// Stats is what Lighthouse records about a project. Fields that were never
// written (builds, downloads, lastSeenTag) are kept so old files round-trip;
// the database replaces all of it.
type Stats struct {
	Meta      MetaStats     `json:"meta"`
	Queries   QueryStats    `json:"queries"`
	Updates   UpdateStats   `json:"updates"`
	Builds    BuildStats    `json:"builds"`
	Downloads DownloadStats `json:"downloads"`
}

type MetaStats struct {
	StartedWatchingAt time.Time  `json:"startedWatchingAt"`
	LastModifiedAt    *time.Time `json:"lastModifiedAt"`
}

type QueryStats struct {
	LastQueriedAt    *time.Time `json:"lastQueriedAt"`
	QueryCount       int        `json:"queryCount"`
	LastErrorAt      *time.Time `json:"lastErrorAt"`
	LastErrorMessage *string    `json:"lastErrorMessage"`
}

type UpdateStats struct {
	LastUpdatedAt     *time.Time `json:"lastUpdatedAt"`
	LastSeenCommitSha *string    `json:"lastSeenCommitSha"`
	LastSeenTag       *string    `json:"lastSeenTag"`
	UpdateCount       int        `json:"updateCount"`
}

type BuildStats struct {
	LastBuildAt         *time.Time `json:"lastBuildAt"`
	LastBuildStatus     *string    `json:"lastBuildStatus"`
	BuildTriggeredCount int        `json:"buildTriggeredCount"`
}

type DownloadStats struct {
	LastDownloadAt         *time.Time `json:"lastDownloadAt"`
	LastDownloadStatus     *string    `json:"lastDownloadStatus"`
	DownloadTriggeredCount int        `json:"downloadTriggeredCount"`
}
