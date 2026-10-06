package deploy

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/docker"
	"github.com/lsariol/lighthouse/internal/projects"
)

// TestRealDeployAndRollback deploys a small project with the real docker
// compose and Docker: version 1, then a version 2 that never becomes
// healthy, which must be rolled back to version 1. It needs a Docker daemon
// and is skipped without one (or with -short).
func TestRealDeployAndRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no Docker daemon")
	}

	const project = "lighthouse-e2e"
	version := func(v string, healthy bool) map[string]string {
		marker := "RUN touch /ok"
		if !healthy {
			marker = "# this version never becomes healthy"
		}
		return map[string]string{
			"compose.yaml": `name: ` + project + `
services:
  app:
    build: .
    restart: unless-stopped
    environment:
      - SECRET=${E2E_SECRET}
    healthcheck:
      test: ["CMD", "test", "-f", "/ok"]
      interval: 1s
      retries: 2
`,
			"Dockerfile": "FROM alpine:3.24\n" + marker + "\nRUN echo " + v + " > /version\nCMD [\"sleep\", \"600\"]\n",
		}
	}

	d, err := docker.New()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	runner := compose.Runner{}
	t.Cleanup(func() {
		var out bytes.Buffer
		runner.Down(context.Background(), t.TempDir(), project, &out)
		for _, ref := range []string{project + "-app", project + "-app:lh-" + sha1[:12], project + "-app:lh-" + sha2[:12]} {
			d.Untag(context.Background(), ref)
		}
	})

	source := &fakeSource{}
	var log bytes.Buffer
	dep := New(runner, d, source, &fakeSecrets{values: map[string]string{"E2E_SECRET": "very-secret-value"}}, Options{
		Root: t.TempDir(), Log: &log, Poll: 500 * time.Millisecond,
		Timeouts: Timeouts{Verify: 60 * time.Second, Stable: 2 * time.Second},
	})
	p := projects.Project{Name: "e2e", Repo: landing.Repo}
	ctx := context.Background()

	running := func() string {
		out, err := exec.Command("docker", "exec", project+"-app-1", "cat", "/version").Output()
		if err != nil {
			return "(not running: " + err.Error() + ")"
		}
		return strings.TrimSpace(string(out))
	}

	// Version 1.
	source.archive = tarball(t, version("v1", true))
	res := dep.Deploy(ctx, Request{Project: p, SHA: sha1, Token: "t"})
	if res.Status != projects.StatusSucceeded {
		t.Fatalf("deploy v1: %v\n%s", res.Err, log.String())
	}
	if got := running(); got != "v1" {
		t.Fatalf("running %q after v1", got)
	}
	if strings.Contains(log.String(), "very-secret-value") {
		t.Error("the secret's value is in the log")
	}

	// Version 2 never becomes healthy: rolled back to version 1.
	p.DeployedSHA, p.ComposeProject = sha1, project
	source.archive = tarball(t, version("v2", false))
	res = dep.Deploy(ctx, Request{Project: p, SHA: sha2, Token: "t"})
	if res.Status != projects.StatusRolledBack {
		t.Fatalf("deploy v2 = %s (%v), want rolled back\n%s", res.Status, res.Err, log.String())
	}
	if !strings.Contains(res.Err.Error(), "unhealthy") && !strings.Contains(res.Err.Error(), "not up") {
		t.Errorf("v2's error: %v", res.Err)
	}

	// The rollback brought version 1 back, and it's healthy.
	deadline := time.Now().Add(30 * time.Second)
	for running() != "v1" && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	if got := running(); got != "v1" {
		t.Errorf("running %q after the rollback, want v1\n%s", got, log.String())
	}
}
