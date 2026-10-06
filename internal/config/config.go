// Package config reads Lighthouse's settings from the environment. It is the
// only package that does; everything else gets a Config.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config holds every setting Lighthouse reads. In Docker they come from the
// compose file's environment; for local development, from a .env file.
type Config struct {
	CoveURL       string        // COVE_URL: Cove's base URL, e.g. http://cove:2100
	CoveTokenPath string        // COVE_TOKEN_PATH: where Lighthouse's Cove token is kept
	RepoPath      string        // APP_REPO_PATH: the watchlist file (repos.json)
	StagingPath   string        // STAGING_PATH: where archives are unpacked; emptied on every deploy
	DownloadPath  string        // DOWNLOAD_PATH: where archives are downloaded; emptied on every deploy
	Env           string        // APP_ENV: "dev" or "prod", shown in the shell's prompt
	Version       string        // LIGHTHOUSE_VERSION: reported by status and version
	ControlSocket string        // LIGHTHOUSE_CONTROL_SOCKET: where serve listens for the CLI
	PollInterval  time.Duration // LIGHTHOUSE_POLL_INTERVAL: how often GitHub is checked
}

const (
	defaultControlSocket = "/run/lighthouse/control.sock"
	defaultPollInterval  = 10 * time.Second
	minPollInterval      = 5 * time.Second
)

// Load reads the optional .env file, then the environment. Values already in
// the environment win over the file. A .env is only for local development:
// APP_ENV_PATH if set, else ./.env, and it's fine if neither exists.
func Load() (Config, error) {
	path := os.Getenv("APP_ENV_PATH")
	if path == "" {
		path = ".env"
	}
	if err := godotenv.Load(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("can't read %s: %v", path, err)
	}

	cfg := Config{
		CoveURL:       env("COVE_URL"),
		CoveTokenPath: env("COVE_TOKEN_PATH"),
		RepoPath:      env("APP_REPO_PATH"),
		StagingPath:   env("STAGING_PATH"),
		DownloadPath:  env("DOWNLOAD_PATH"),
		Env:           strings.ToLower(env("APP_ENV")),
		Version:       env("LIGHTHOUSE_VERSION"),
		ControlSocket: env("LIGHTHOUSE_CONTROL_SOCKET"),
		PollInterval:  defaultPollInterval,
	}
	if cfg.ControlSocket == "" {
		cfg.ControlSocket = defaultControlSocket
	}

	if raw := env("LIGHTHOUSE_POLL_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("LIGHTHOUSE_POLL_INTERVAL %q isn't a duration: use a value such as 30s or 1m", raw)
		}
		cfg.PollInterval = d
	}

	return cfg, nil
}

func env(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// ValidateServe checks the settings the daemon needs. The shell and one-shot
// commands only need ControlSocket, which always has a value.
func (c Config) ValidateServe() error {
	required := []struct{ name, value string }{
		{"COVE_URL", c.CoveURL},
		{"COVE_TOKEN_PATH", c.CoveTokenPath},
		{"APP_REPO_PATH", c.RepoPath},
		{"STAGING_PATH", c.StagingPath},
		{"DOWNLOAD_PATH", c.DownloadPath},
	}
	var missing []string
	for _, r := range required {
		if r.value == "" {
			missing = append(missing, r.name)
		}
	}
	if len(missing) > 0 {
		verb, pronoun := "are", "them"
		if len(missing) == 1 {
			verb, pronoun = "is", "it"
		}
		return fmt.Errorf("%s %s not set. In Docker, set %s in docker-compose.yml's environment; locally, in .env (see .env.example)",
			strings.Join(missing, ", "), verb, pronoun)
	}

	if !strings.HasPrefix(c.CoveURL, "http://") && !strings.HasPrefix(c.CoveURL, "https://") {
		return fmt.Errorf("COVE_URL %q must start with http:// or https://, e.g. http://cove:2100", c.CoveURL)
	}

	// Both folders are emptied on every deploy, so a typo here could delete
	// something important. Refuse anything that is obviously not a scratch folder.
	for _, p := range []struct{ name, value string }{{"STAGING_PATH", c.StagingPath}, {"DOWNLOAD_PATH", c.DownloadPath}} {
		if err := checkWorkFolder(p.value); err != nil {
			return fmt.Errorf("%s %q: %v", p.name, p.value, err)
		}
	}

	if c.PollInterval < minPollInterval {
		return fmt.Errorf("LIGHTHOUSE_POLL_INTERVAL %s is too short: use %s or more (GitHub rate-limits a token to 5,000 requests an hour)", c.PollInterval, minPollInterval)
	}
	return nil
}

// checkWorkFolder refuses folders whose contents must never be deleted: the
// filesystem root, the current directory, and a few well-known system folders.
func checkWorkFolder(path string) error {
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." {
		return errors.New("is the current folder; use a dedicated folder Lighthouse may empty")
	}
	if filepath.Dir(clean) == clean { // "/", "C:\"
		return errors.New("is the filesystem root; use a dedicated folder Lighthouse may empty")
	}
	switch filepath.ToSlash(clean) {
	case "/srv", "/srv/server", "/srv/server/storage", "/app", "/home", "/root", "/etc", "/var", "/usr", "/tmp":
		return errors.New("isn't a dedicated folder: it's emptied on every deploy, so use one only Lighthouse uses (e.g. /app/server/staging)")
	}
	return nil
}
