// Package projects defines projects, deployments and the Store that keeps them.
package projects

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/lsariol/lighthouse/internal/github"
)

// Project is one watched repository and what Lighthouse knows about it.
type Project struct {
	Name      string
	Repo      github.Repo
	CreatedAt time.Time

	ComposeProject string

	DeployedSHA     string
	DeployedVersion string
	HighestVersion  string
	DeployedAt      *time.Time
	LastCheckedAt   *time.Time
	Checks          int64
	LastError       string
	LastErrorAt     *time.Time

	FailureCount int
	FailingSHA   string
	Broken       bool

	Mode string
	Tier string

	HeldSHA string

	Stopped bool
}

// ComposeName is p's compose project: as last deployed, else the repository's name.
func (p Project) ComposeName() string {
	if p.ComposeProject != "" {
		return p.ComposeProject
	}
	return strings.ToLower(p.Repo.Name)
}

// BrokenAfter is how many permanent failures in a row of the same commit mark
// a project broken.
const BrokenAfter = 3

// Deployment is one attempt to deploy a project, recorded when it ends.
type Deployment struct {
	Project     string
	SHA         string
	Version     string
	Trigger     string
	Status      string
	FailureKind string
	FailedStep  string
	StartedAt   time.Time
	FinishedAt  time.Time
	Error       string
	Steps       []Step
}

// Step is one step of a deployment.
type Step struct {
	Name       string
	Status     string
	StartedAt  time.Time
	FinishedAt time.Time
	Log        string
}

const (
	TriggerCheck     = "check"     // a new commit found by a scheduled or manual scan
	TriggerManual    = "manual"    // `deploy <name>` or `retry <name>`
	TriggerReconcile = "reconcile" // the reconcile loop brought a project that was down back

	StatusSucceeded  = "succeeded"
	StatusFailed     = "failed"      // nothing changed: the running version kept serving
	StatusRolledBack = "rolled_back" // the new version started but failed its check; the previous one was restored

	// FailureTransient: retrying later may work (GitHub, Cove, the network or
	// Docker had a problem). It doesn't count toward BrokenAfter.
	FailureTransient = "transient"
	// FailurePermanent: the commit itself has a problem (it doesn't build, a
	// secret is missing, it doesn't start). It counts toward BrokenAfter.
	FailurePermanent = "permanent"

	StepSucceeded = "succeeded"
	StepFailed    = "failed"
	StepSkipped   = "skipped"
)

// MaxErrorLength is the most of an error message that's stored.
const MaxErrorLength = 2000

// MaxLogLength is the most of a step's output that's stored (its end).
const MaxLogLength = 16 << 10

var (
	ErrNotFound            = errors.New("no such project")
	ErrNameTaken           = errors.New("name already in use")
	ErrRepoWatched         = errors.New("repository already watched")
	ErrInvalidName         = errors.New("invalid project name")
	ErrComposeProjectTaken = errors.New("compose project belongs to another project")
	ErrNoDeployment        = errors.New("no such deployment")
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

	// SetComposeProject records the compose project the project's compose
	// file names. ErrComposeProjectTaken if another project has it.
	SetComposeProject(ctx context.Context, name string, composeProject string) error

	// SetSettings records the project's x-lighthouse settings: its deploy
	// mode ("branch" or "releases") and tier ("data", "infra" or "app").
	SetSettings(ctx context.Context, name string, mode string, tier string) error

	// SetHeld records the commit checks mustn't deploy again ("" for none).
	SetHeld(ctx context.Context, name string, sha string) error

	// SetStopped records whether the project was stopped on purpose.
	SetStopped(ctx context.Context, name string, stopped bool) error

	// RecordCheck notes that the project was checked; checkErr (nil if it
	// worked) becomes its last error.
	RecordCheck(ctx context.Context, name string, checkErr error) error

	// RecordDeployment adds d, with its steps, to the project's history.
	// A success becomes the deployed commit (and version, which also raises
	// HighestVersion if it's higher) and clears the last error and
	// the failure count. A failure becomes the last error; a permanent one
	// counts toward BrokenAfter (counting restarts with a new commit).
	RecordDeployment(ctx context.Context, d Deployment) error

	// ClearFailures resets the failure count and the broken state.
	ClearFailures(ctx context.Context, name string) error

	// History returns the project's most recent deployments, newest first,
	// without their steps.
	History(ctx context.Context, name string, limit int) ([]Deployment, error)

	// Deployment returns one deployment with its steps: 1 is the most
	// recent, 2 the one before. ErrNoDeployment if there's no such one.
	Deployment(ctx context.Context, name string, n int) (Deployment, error)
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
	return cut(err.Error(), MaxErrorLength, false)
}

// LogText is a step's output as stored: its last MaxLogLength bytes.
func LogText(log string) string {
	return cut(log, MaxLogLength, true)
}

// cut shortens s to max bytes on a character boundary (Postgres rejects
// invalid UTF-8), keeping the start, or the end if keepEnd.
func cut(s string, max int, keepEnd bool) string {
	if len(s) <= max {
		return s
	}
	const mark = "…"
	if keepEnd {
		return mark + strings.ToValidUTF8(s[len(s)-max+len(mark):], "")
	}
	return strings.ToValidUTF8(s[:max-len(mark)], "") + mark
}

// ErrorString lets a stored message go through ErrorText.
type ErrorString string

func (e ErrorString) Error() string { return string(e) }
