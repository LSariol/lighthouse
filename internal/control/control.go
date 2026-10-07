// Package control connects the CLI to the running daemon. `lighthouse serve`
// owns all state; `lighthouse shell` and one-shot commands are separate
// processes that reach it through a Unix socket, using HTTP and JSON.
//
// Service is what the CLI needs. The daemon implements it; Client implements
// it by calling the daemon; Handler serves a Service over HTTP.
package control

import (
	"context"
	"fmt"
	"time"
)

// Service is everything the CLI can ask the daemon to do. Methods return an
// *Error for problems the user can fix (a wrong name, a duplicate URL).
type Service interface {
	Status(ctx context.Context) (Status, error)
	Projects(ctx context.Context) ([]Project, error)

	// Add starts watching the repository at url. With name "", the project
	// is named after the repository (lowercased); a clash is a
	// KindNameTaken error, and the caller can try again with a name.
	Add(ctx context.Context, name string, url string) (Project, error)
	// Remove stops watching a project; with down, its containers are
	// stopped and removed too.
	Remove(ctx context.Context, name string, down bool) error
	Rename(ctx context.Context, name string, newName string) error
	SetURL(ctx context.Context, name string, url string) error

	// Deploy deploys a project now: ref (a tag, or a commit's full or short
	// SHA), or else its newest release (release mode) or its branch's newest
	// commit.
	Deploy(ctx context.Context, name string, ref string) error
	// Rollback deploys again what ran before the current version, and says
	// what that was ("v1.1.0 (abc1234)" or "abc1234").
	Rollback(ctx context.Context, name string) (string, error)
	// Check runs the project's latest commit through the deploy's checks
	// (the rules and the test stage) without deploying it. The result, with
	// its steps, isn't recorded in the history.
	Check(ctx context.Context, name string) (Deployment, error)
	// Retry clears a project's failures and broken state, and deploys it.
	Retry(ctx context.Context, name string) error
	Scan(ctx context.Context) error
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error

	// Start, Stop, Restart and Logs take a target: a project's name (every
	// service) or "<project>:<service>" (one).
	Start(ctx context.Context, target string) error
	Stop(ctx context.Context, target string) error
	Restart(ctx context.Context, target string) error
	Logs(ctx context.Context, target string, lines int) (string, error)

	History(ctx context.Context, name string, limit int) ([]Deployment, error)
	// Report returns one deployment with its steps: 1 is the latest.
	Report(ctx context.Context, name string, n int) (Deployment, error)
}

// Status is the daemon's health and every project's container state.
type Status struct {
	Version      string        `json:"version"`
	Env          string        `json:"env"`
	StartedAt    time.Time     `json:"startedAt"`
	Phase        string        `json:"phase"` // e.g. "running", "waiting for Cove", "waiting for the database"
	Paused       bool          `json:"paused"`
	PollInterval time.Duration `json:"pollInterval"`
	CoveURL      string        `json:"coveURL"`
	GitHubToken  bool          `json:"githubToken"` // loaded from Cove
	Database     string        `json:"database"`    // "reachable", "unreachable", or "" before startup connects
	Schema       string        `json:"schema"`      // e.g. "version 2 (up to date)"
	Projects     []Project     `json:"projects"`
}

// Project is one watched repository.
type Project struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	ComposeProject string `json:"composeProject"` // as its compose file names it (or the repository's name before a deploy)
	// State sums up its services, in Status only: "running", "degraded"
	// (some aren't), "stopped" (none is) or "missing" (no containers).
	State         string          `json:"state,omitempty"`
	Services      []ServiceStatus `json:"services,omitempty"` // in Status only
	Commit        string          `json:"commit,omitempty"`
	Version       string          `json:"version,omitempty"` // the release deployed, in release mode
	Mode          string          `json:"mode,omitempty"`    // "branch" or "releases" (x-lighthouse deploy)
	Tier          string          `json:"tier,omitempty"`    // "data", "infra" or "app" (x-lighthouse tier)
	Stopped       bool            `json:"stopped,omitempty"` // stopped on purpose: not deployed or brought back until started
	WatchingSince time.Time       `json:"watchingSince"`
	LastDeployed  *time.Time      `json:"lastDeployed,omitempty"`
	LastChecked   *time.Time      `json:"lastChecked,omitempty"`
	Checks        int             `json:"checks"`
	LastError     string          `json:"lastError,omitempty"`
	LastErrorAt   *time.Time      `json:"lastErrorAt,omitempty"`
	FailureCount  int             `json:"failureCount,omitempty"`
	Broken        bool            `json:"broken,omitempty"` // its latest commit failed too often; `retry` or a new commit
}

// ServiceStatus is one service of a project, as running now.
type ServiceStatus struct {
	Name      string `json:"name"`
	Container string `json:"container"`
	State     string `json:"state"`            // "running", "exited", ...
	Health    string `json:"health,omitempty"` // "healthy", "unhealthy", "starting", or "" without a healthcheck
}

// Deployment is one deploy attempt, from the project's history.
type Deployment struct {
	Commit      string    `json:"commit,omitempty"`
	Version     string    `json:"version,omitempty"`     // the release deployed, if it was one
	Trigger     string    `json:"trigger"`               // "check", "manual" or "reconcile"
	Status      string    `json:"status"`                // "succeeded", "failed" or "rolled_back"
	FailureKind string    `json:"failureKind,omitempty"` // "transient" or "permanent"
	FailedStep  string    `json:"failedStep,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt"`
	Error       string    `json:"error,omitempty"`
	Steps       []Step    `json:"steps,omitempty"` // in Report only
}

// Step is one step of a deployment.
type Step struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"` // "succeeded", "failed" or "skipped"
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Log        string    `json:"log,omitempty"`
}

// Error kinds, mapped to HTTP status codes on the socket.
const (
	KindInvalid     = "invalid"     // bad input: fix the arguments
	KindNotFound    = "not_found"   // no such project
	KindConflict    = "conflict"    // a URL already watched, a scan already running
	KindNameTaken   = "name_taken"  // `add`: a project already has that name; pick another
	KindUnavailable = "unavailable" // the daemon isn't ready (still starting)
	KindHandedOff   = "handed_off"  // Lighthouse's own deploy: the update helper is replacing it (not a failure)
	KindInternal    = "internal"    // anything else; details are in docker logs
)

// Error is a problem with a message meant for the user.
type Error struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Errorf returns an *Error of the given kind.
func Errorf(kind string, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}
