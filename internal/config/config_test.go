package config

import (
	"strings"
	"testing"
	"time"
)

// setEnv sets the variables Load reads, clearing the rest, and points
// APP_ENV_PATH at a file that doesn't exist so a developer's .env is ignored.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for _, name := range []string{"COVE_URL", "COVE_TOKEN_PATH", "APP_REPO_PATH", "STAGING_PATH", "DOWNLOAD_PATH",
		"APP_ENV", "LIGHTHOUSE_VERSION", "LIGHTHOUSE_CONTROL_SOCKET", "LIGHTHOUSE_POLL_INTERVAL"} {
		t.Setenv(name, vars[name])
	}
	t.Setenv("APP_ENV_PATH", t.TempDir()+"/missing.env")
}

func valid() map[string]string {
	return map[string]string{
		"COVE_URL":        "http://cove:2100",
		"COVE_TOKEN_PATH": "/app/vault/cove/token",
		"APP_REPO_PATH":   "/app/vault/repos.json",
		"STAGING_PATH":    "/app/server/staging/",
		"DOWNLOAD_PATH":   "/app/server/download/",
		"APP_ENV":         "PROD",
	}
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, valid())

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControlSocket != defaultControlSocket {
		t.Errorf("ControlSocket = %q, want %q", cfg.ControlSocket, defaultControlSocket)
	}
	if cfg.PollInterval != defaultPollInterval {
		t.Errorf("PollInterval = %s, want %s", cfg.PollInterval, defaultPollInterval)
	}
	if cfg.Env != "prod" {
		t.Errorf("Env = %q, want it lowercased to %q", cfg.Env, "prod")
	}
	if err := cfg.ValidateServe(); err != nil {
		t.Errorf("ValidateServe: %v", err)
	}
}

func TestLoadWithoutEnvFile(t *testing.T) {
	setEnv(t, map[string]string{})
	if _, err := Load(); err != nil {
		t.Fatalf("a missing .env must not be an error: %v", err)
	}
}

func TestPollInterval(t *testing.T) {
	vars := valid()
	vars["LIGHTHOUSE_POLL_INTERVAL"] = "1m"
	setEnv(t, vars)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval != time.Minute {
		t.Errorf("PollInterval = %s, want 1m", cfg.PollInterval)
	}

	vars["LIGHTHOUSE_POLL_INTERVAL"] = "often"
	setEnv(t, vars)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "LIGHTHOUSE_POLL_INTERVAL") {
		t.Errorf("Load with a bad interval: err = %v, want one naming the setting", err)
	}

	vars["LIGHTHOUSE_POLL_INTERVAL"] = "1s"
	setEnv(t, vars)
	cfg, _ = Load()
	if err := cfg.ValidateServe(); err == nil {
		t.Error("ValidateServe accepted a 1s poll interval")
	}
}

func TestValidateServeMissing(t *testing.T) {
	setEnv(t, map[string]string{"COVE_URL": "http://cove:2100"})
	cfg, _ := Load()
	err := cfg.ValidateServe()
	if err == nil {
		t.Fatal("ValidateServe accepted missing settings")
	}
	for _, name := range []string{"COVE_TOKEN_PATH", "APP_REPO_PATH", "STAGING_PATH", "DOWNLOAD_PATH"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q doesn't name %s", err, name)
		}
	}
}

func TestValidateServeCoveURL(t *testing.T) {
	vars := valid()
	vars["COVE_URL"] = "cove:2100"
	setEnv(t, vars)
	cfg, _ := Load()
	if err := cfg.ValidateServe(); err == nil || !strings.Contains(err.Error(), "http://") {
		t.Errorf("err = %v, want one explaining the scheme", err)
	}
}

func TestWorkFolderGuard(t *testing.T) {
	for _, path := range []string{"/", ".", "/srv/server", "/srv/server/storage/", "/app"} {
		vars := valid()
		vars["STAGING_PATH"] = path
		setEnv(t, vars)
		cfg, _ := Load()
		if err := cfg.ValidateServe(); err == nil {
			t.Errorf("STAGING_PATH=%q was accepted", path)
		}
	}
	for _, path := range []string{"/app/server/staging", "Server/Staging/", "/srv/server/staging"} {
		if err := checkWorkFolder(path); err != nil {
			t.Errorf("checkWorkFolder(%q) = %v, want nil", path, err)
		}
	}
}
