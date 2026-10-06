// Package projects defines the watched projects, their deployments, and the
// Store that keeps them. The database package implements the Store in
// Postgres; projectstest has an in-memory one for tests.
package projects

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/LSariol/LightHouse/internal/github"
)

// Project is one watched repository and what Lighthouse knows about it.
type Project struct {
	Name          string      // Lighthouse's name for it, used in the CLI; matched without regard to case
	Repo          github.Repo // the repository it deploys
	CreatedAt     time.Time   // when Lighthouse started watching it
	DeployedSHA   string      // the commit last deployed successfully, or ""
	DeployedAt    *time.Time
	LastCheckedAt *time.Time
	Checks        int64
	LastError     string // the last check's or deploy's error, or "" if it worked
	LastErrorAt   *time.Time
}

// Container is the name of the project's container: its repository's name,
// lowercased. The project's compose file must use it (container_name and the
// top-level name).
func (p Project) Container() string {
	return strings.ToLower(p.Repo.Name)
}

// Deployment is one attempt to deploy a project, recorded when it ends.
type Deployment struct {
	Project    string // the project's name
	SHA        string // the commit deployed, or "" if it failed before one was known
	Trigger    string // TriggerCheck or TriggerManual
	Status     string // StatusSucceeded or StatusFailed
	StartedAt  time.Time
	FinishedAt time.Time
	Error      string
}

const (
	TriggerCheck  = "check"  // a new commit found by a scheduled or manual scan
	TriggerManual = "manual" // `deploy <name>`

	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// MaxErrorLength is the most of an error message that's stored.
const MaxErrorLength = 2000

var (
	ErrNotFound    = errors.New("no such project")
	ErrNameTaken   = errors.New("name already in use")
	ErrRepoWatched = errors.New("repository already watched")
	ErrInvalidName = errors.New("invalid project name")
)

// Store keeps the projects and their deployments. Names are matched without
// regard to case, and so is the uniqueness of names and repositories.
type Store interface {
	List(ctx context.Context) ([]Project, error)
	Get(ctx context.Context, name string) (Project, error)

	Add(ctx context.Context, name string, repo github.Repo) (Project, error)
	Remove(ctx context.Context, name string) error
	Rename(ctx context.Context, name string, newName string) error
	SetRepo(ctx context.Context, name string, repo github.Repo) error

	// RecordCheck notes that the project was checked; checkErr (nil if it
	// worked) becomes its last error.
	RecordCheck(ctx context.Context, name string, checkErr error) error

	// RecordDeployment adds d to the project's history. A success also
	// becomes the deployed commit and clears the last error; a failure
	// becomes the last error.
	RecordDeployment(ctx context.Context, d Deployment) error

	// History returns the project's most recent deployments, newest first.
	History(ctx context.Context, name string, limit int) ([]Deployment, error)
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidateName returns ErrInvalidName unless name is 1–64 letters, digits,
// - and _, starting with a letter or digit.
func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return ErrInvalidName
	}
	return nil
}

// ErrorText is err's message as stored: "" for nil, cut to MaxErrorLength.
func ErrorText(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > MaxErrorLength {
		// Cut on a character boundary: Postgres rejects invalid UTF-8.
		msg = strings.ToValidUTF8(msg[:MaxErrorLength-len("…")], "") + "…"
	}
	return msg
}
