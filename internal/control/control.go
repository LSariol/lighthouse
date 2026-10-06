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

	Add(ctx context.Context, name string, url string) (Project, error)
	Remove(ctx context.Context, name string) error
	Rename(ctx context.Context, name string, newName string) error
	SetURL(ctx context.Context, name string, url string) error

	Deploy(ctx context.Context, name string) error
	Scan(ctx context.Context) error
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error

	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	Restart(ctx context.Context, name string) error
	Logs(ctx context.Context, name string, lines int) (string, error)
}

// Status is the daemon's health and every project's container state.
type Status struct {
	Version      string        `json:"version"`
	Env          string        `json:"env"`
	StartedAt    time.Time     `json:"startedAt"`
	Phase        string        `json:"phase"` // e.g. "running", "waiting for Cove"
	Paused       bool          `json:"paused"`
	PollInterval time.Duration `json:"pollInterval"`
	CoveURL      string        `json:"coveURL"`
	GitHubToken  bool          `json:"githubToken"` // loaded from Cove
	Projects     []Project     `json:"projects"`
}

// Project is one watched repository.
type Project struct {
	Name          string     `json:"name"`
	URL           string     `json:"url"`
	Container     string     `json:"container"`
	State         string     `json:"state,omitempty"` // Docker's state, in Status only: "running", "exited", "missing"
	Commit        string     `json:"commit,omitempty"`
	WatchingSince time.Time  `json:"watchingSince"`
	LastDeployed  *time.Time `json:"lastDeployed,omitempty"`
	LastChecked   *time.Time `json:"lastChecked,omitempty"`
	Checks        int        `json:"checks"`
	LastError     string     `json:"lastError,omitempty"`
	LastErrorAt   *time.Time `json:"lastErrorAt,omitempty"`
}

// Error kinds, mapped to HTTP status codes on the socket.
const (
	KindInvalid     = "invalid"     // bad input: fix the arguments
	KindNotFound    = "not_found"   // no such project
	KindConflict    = "conflict"    // a name or URL already in use
	KindUnavailable = "unavailable" // the daemon isn't ready (still starting)
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
