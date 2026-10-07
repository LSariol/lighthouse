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
	closedFor atomic.Int32
	token     string
	secrets   map[string]string
}

func (f *fakeCove) handler() http.Handler {
	reply := func(w http.ResponseWriter, status int, data any, errType string) {
		w.WriteHeader(status)
		if errType != "" {
			json.NewEncoder(w).Encode(map[string]any{"success": false, "error": map[string]any{"type": errType, "message": errType, "keys": data}})
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
	mux.HandleFunc("POST /v0/batch", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			reply(w, http.StatusUnauthorized, nil, "invalid_token")
			return
		}
		var body struct{ Keys []string }
		json.NewDecoder(r.Body).Decode(&body)
		var found []map[string]any
		var missing []string
		for _, k := range body.Keys {
			if v, ok := f.secrets[k]; ok {
				found = append(found, map[string]any{"key": k, "value": v, "version": 1})
			} else {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			reply(w, http.StatusNotFound, missing, "not_found")
			return
		}
		reply(w, http.StatusOK, map[string]any{"secrets": found}, "")
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
	f := &fakeCove{token: "cove_abc", secrets: map[string]string{
		GitHubTokenKey: "gh", DatabaseURLKey: "postgres://app", MigratorDatabaseURLKey: "postgres://migrator"}}
	f.closedFor.Store(2)
	c := start(t, f)
	path := filepath.Join(t.TempDir(), "cove", "token")

	if err := Connect(context.Background(), c, path, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(saved)) != "cove_abc" {
		t.Errorf("token file: %q, %v", saved, err)
	}

	secrets, err := ReadSecrets(context.Background(), c)
	if err != nil || secrets != (Secrets{GitHubToken: "gh", DatabaseURL: "postgres://app", MigratorDatabaseURL: "postgres://migrator"}) {
		t.Errorf("ReadSecrets = %+v, %v", secrets, err)
	}
}

func TestRejectedTokenExplainsTheFix(t *testing.T) {
	c := start(t, &fakeCove{token: "cove_new"})
	path := filepath.Join(t.TempDir(), "token")
	os.WriteFile(path, []byte("cove_old\n"), 0o600)

	err := Connect(context.Background(), c, path, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "bootstrap open lighthouse") || !strings.Contains(err.Error(), path) {
		t.Errorf("Connect = %v, want the re-bootstrap instructions", err)
	}
}

func TestMissingSecrets(t *testing.T) {
	f := &fakeCove{token: "cove_abc", secrets: map[string]string{GitHubTokenKey: "gh"}}
	c := start(t, f)
	path := filepath.Join(t.TempDir(), "token")
	os.WriteFile(path, []byte("cove_abc"), 0o600)
	if err := Connect(context.Background(), c, path, time.Millisecond); err != nil {
		t.Fatal(err)
	}

	_, err := ReadSecrets(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), DatabaseURLKey) || !strings.Contains(err.Error(), MigratorDatabaseURLKey) ||
		!strings.Contains(err.Error(), "§10.1") {
		t.Errorf("ReadSecrets = %v, want both missing keys named and where to fix it", err)
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
