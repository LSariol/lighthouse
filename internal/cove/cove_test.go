package cove

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lsariol/coveclient"
)

// fakeCove imitates the parts of Cove's API that Connect and GitHubToken use.
type fakeCove struct {
	closedFor   atomic.Int32 // how many more bootstrap requests are refused
	token       string       // the token the bootstrap hands out and auth accepts
	githubToken string       // "" means LIGHTHOUSE_GITHUB_TOKEN doesn't exist
}

func (f *fakeCove) handler() http.Handler {
	reply := func(w http.ResponseWriter, status int, data any, errType string) {
		w.WriteHeader(status)
		if errType != "" {
			json.NewEncoder(w).Encode(map[string]any{"success": false, "error": map[string]string{"type": errType, "message": errType}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
	}
	authed := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+f.token }

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/bootstrap/lighthouse", func(w http.ResponseWriter, r *http.Request) {
		if f.closedFor.Add(-1) >= 0 {
			reply(w, http.StatusForbidden, nil, "bootstrap_locked")
			return
		}
		reply(w, http.StatusOK, map[string]string{"secret": f.token}, "")
	})
	mux.HandleFunc("GET /v0/ready", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, map[string]any{"ready": true}, "")
	})
	mux.HandleFunc("GET /v0/auth", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			reply(w, http.StatusUnauthorized, nil, "invalid_token")
			return
		}
		reply(w, http.StatusOK, map[string]any{"authenticated": true}, "")
	})
	mux.HandleFunc("GET /v0/secrets/{key}", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !authed(r):
			reply(w, http.StatusUnauthorized, nil, "invalid_token")
		case r.PathValue("key") != GitHubTokenKey || f.githubToken == "":
			reply(w, http.StatusNotFound, nil, "not_found")
		default:
			reply(w, http.StatusOK, map[string]any{"key": GitHubTokenKey, "value": f.githubToken, "version": 1}, "")
		}
	})
	return mux
}

func start(t *testing.T, f *fakeCove) *coveclient.Client {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return coveclient.New(srv.URL, "")
}

func TestFirstStartWaitsForBootstrap(t *testing.T) {
	f := &fakeCove{token: "cove_abc", githubToken: "gh"}
	f.closedFor.Store(2) // refused twice, then opened
	c := start(t, f)
	path := filepath.Join(t.TempDir(), "cove", "token")

	if err := Connect(context.Background(), c, path, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(saved)) != "cove_abc" {
		t.Errorf("token file: %q, %v", saved, err)
	}

	token, err := GitHubToken(context.Background(), c)
	if err != nil || token != "gh" {
		t.Errorf("GitHubToken = %q, %v", token, err)
	}
}

func TestRejectedTokenExplainsTheFix(t *testing.T) {
	c := start(t, &fakeCove{token: "cove_new"})
	path := filepath.Join(t.TempDir(), "token")
	os.WriteFile(path, []byte("cove_old\n"), 0o600) // rotated since

	err := Connect(context.Background(), c, path, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "bootstrap open lighthouse") || !strings.Contains(err.Error(), path) {
		t.Errorf("Connect = %v, want the re-bootstrap instructions", err)
	}
}

func TestMissingGitHubToken(t *testing.T) {
	f := &fakeCove{token: "cove_abc"}
	c := start(t, f)
	path := filepath.Join(t.TempDir(), "token")
	os.WriteFile(path, []byte("cove_abc"), 0o600)
	if err := Connect(context.Background(), c, path, time.Millisecond); err != nil {
		t.Fatal(err)
	}

	_, err := GitHubToken(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "create "+GitHubTokenKey) {
		t.Errorf("GitHubToken = %v, want how to create it", err)
	}
}

func TestUnreachableRetriesUntilCancelled(t *testing.T) {
	c := coveclient.New("http://127.0.0.1:1", "")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := Connect(ctx, c, filepath.Join(t.TempDir(), "token"), 10*time.Millisecond)
	if err != context.DeadlineExceeded {
		t.Errorf("Connect = %v, want it to keep retrying until the context ends", err)
	}
}
