package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Handler serves svc over HTTP, under /v1.
func Handler(svc Service) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		status, err := svc.Status(r.Context())
		respond(w, status, err)
	})
	mux.HandleFunc("GET /v1/projects", func(w http.ResponseWriter, r *http.Request) {
		projects, err := svc.Projects(r.Context())
		respond(w, projects, err)
	})
	mux.HandleFunc("POST /v1/projects", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Name, URL string }
		if !decode(w, r, &body) {
			return
		}
		project, err := svc.Add(r.Context(), body.Name, body.URL)
		respond(w, project, err)
	})
	mux.HandleFunc("DELETE /v1/projects/{name}", func(w http.ResponseWriter, r *http.Request) {
		respond(w, nil, svc.Remove(r.Context(), r.PathValue("name"), r.URL.Query().Get("down") == "true"))
	})
	mux.HandleFunc("POST /v1/projects/{name}/rename", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Name string }
		if !decode(w, r, &body) {
			return
		}
		respond(w, nil, svc.Rename(r.Context(), r.PathValue("name"), body.Name))
	})
	mux.HandleFunc("POST /v1/projects/{name}/url", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ URL string }
		if !decode(w, r, &body) {
			return
		}
		respond(w, nil, svc.SetURL(r.Context(), r.PathValue("name"), body.URL))
	})

	actions := map[string]func(context.Context, string) error{
		"deploy":  svc.Deploy,
		"retry":   svc.Retry,
		"start":   svc.Start,
		"stop":    svc.Stop,
		"restart": svc.Restart,
	}
	for action, fn := range actions {
		mux.HandleFunc("POST /v1/projects/{name}/"+action, func(w http.ResponseWriter, r *http.Request) {
			respond(w, nil, fn(r.Context(), r.PathValue("name")))
		})
	}

	mux.HandleFunc("GET /v1/projects/{name}/logs", func(w http.ResponseWriter, r *http.Request) {
		lines, err := strconv.Atoi(r.URL.Query().Get("lines"))
		if err != nil {
			respond(w, nil, Errorf(KindInvalid, "lines must be a number"))
			return
		}
		logs, err := svc.Logs(r.Context(), r.PathValue("name"), lines)
		respond(w, logs, err)
	})

	mux.HandleFunc("GET /v1/projects/{name}/report", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.URL.Query().Get("n"))
		if err != nil {
			respond(w, nil, Errorf(KindInvalid, "n must be a number"))
			return
		}
		report, err := svc.Report(r.Context(), r.PathValue("name"), n)
		respond(w, report, err)
	})
	mux.HandleFunc("GET /v1/projects/{name}/history", func(w http.ResponseWriter, r *http.Request) {
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil {
			respond(w, nil, Errorf(KindInvalid, "limit must be a number"))
			return
		}
		history, err := svc.History(r.Context(), r.PathValue("name"), limit)
		respond(w, history, err)
	})

	global := map[string]func(context.Context) error{
		"scan":   svc.Scan,
		"pause":  svc.Pause,
		"resume": svc.Resume,
	}
	for action, fn := range global {
		mux.HandleFunc("POST /v1/"+action, func(w http.ResponseWriter, r *http.Request) {
			respond(w, nil, fn(r.Context()))
		})
	}

	return mux
}

// envelope is every response body: data on success, error otherwise.
type envelope struct {
	Data  json.RawMessage `json:"data,omitempty"`
	Error *Error          `json:"error,omitempty"`
}

var statusFor = map[string]int{
	KindInvalid:     http.StatusBadRequest,
	KindNotFound:    http.StatusNotFound,
	KindConflict:    http.StatusConflict,
	KindNameTaken:   http.StatusConflict,
	KindUnavailable: http.StatusServiceUnavailable,
	KindInternal:    http.StatusInternalServerError,
}

func respond(w http.ResponseWriter, data any, err error) {
	w.Header().Set("Content-Type", "application/json")

	if err != nil {
		var e *Error
		if !errors.As(err, &e) {
			slog.Error("control request failed", "err", err)
			e = &Error{Kind: KindInternal, Message: err.Error()}
		}
		status, ok := statusFor[e.Kind]
		if !ok {
			status = http.StatusInternalServerError
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(envelope{Error: e})
		return
	}

	raw, err := json.Marshal(data)
	if err != nil {
		respond(w, nil, err)
		return
	}
	json.NewEncoder(w).Encode(envelope{Data: raw})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		respond(w, nil, Errorf(KindInvalid, "invalid request body: %v", err))
		return false
	}
	return true
}

// Serve listens on the Unix socket at path and serves svc until ctx is
// cancelled. The socket is readable and writable by its owner only: anyone
// who can use it controls Lighthouse.
func Serve(ctx context.Context, path string, svc Service) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("control socket: %w", err)
	}

	// A socket left behind by a previous run (e.g. after a crash) blocks
	// Listen. Remove it, unless something is still answering on it.
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		conn.Close()
		return fmt.Errorf("control socket %s is in use: is another Lighthouse running?", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("control socket: %w", err)
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return fmt.Errorf("control socket: %w", err)
	}

	server := &http.Server{
		Handler:           Handler(svc),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: a deploy answers when it's done, which can take
		// several minutes.
	}

	errs := make(chan error, 1)
	go func() { errs <- server.Serve(listener) }()

	select {
	case err := <-errs:
		return fmt.Errorf("control socket: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
		os.Remove(path)
		return nil
	}
}
