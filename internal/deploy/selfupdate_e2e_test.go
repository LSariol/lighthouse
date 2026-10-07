package deploy

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/docker"
	"github.com/lsariol/lighthouse/internal/projects"
)

// TestRealSelfUpdate runs a whole self-update against real Docker: a project
// built FROM the Lighthouse image hands off to the real update helper, which
// swaps it; then a version that never becomes healthy is handed off and must
// be rolled back.
func TestRealSelfUpdate(t *testing.T) {
	base := os.Getenv("LIGHTHOUSE_E2E_SELF_UPDATE")
	if base == "" {
		t.Skip("set by scripts/e2e-self-update.sh, which runs it inside a container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	const project = "lhselftest"
	root := "/tmp/lhtest/staging"
	os.RemoveAll(root)
	files := func(healthy bool) map[string]string {
		check := `["CMD", "/lighthouse", "health"]`
		if !healthy {
			check = `["CMD", "false"]`
		}
		return map[string]string{
			"compose.yaml": `name: ` + project + `
services:
  app:
    build: .
    restart: unless-stopped
    environment:
      - APP_ENV=e2e
      - COVE_URL=http://127.0.0.1:1
      - COVE_TOKEN_PATH=/tmp/token
      - STAGING_PATH=/tmp/staging
      - LIGHTHOUSE_VERSION=${LIGHTHOUSE_DEPLOY_VERSION:-dev}
    healthcheck:
      test: ` + check + `
      interval: 1s
      retries: 3
      start_period: 3s
`,
			"Dockerfile": "FROM " + base + "\nCMD [\"/lighthouse\", \"serve\"]\n",
		}
	}

	runner := compose.Runner{}
	d, err := docker.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var out bytes.Buffer
		runner.Down(context.Background(), t.TempDir(), project, &out)
		runner.RemoveContainer(context.Background(), HelperName)
		for _, sha := range []string{sha1, sha2} {
			d.Untag(context.Background(), project+"-app:lh-"+sha[:12])
		}
		d.Untag(context.Background(), project+"-app")
	})

	source := &fakeSource{}
	var log bytes.Buffer
	dep := New(runner, d, source, &fakeSecrets{}, Options{
		Root: root, Self: project, Log: &log, Poll: 500 * time.Millisecond,
		Timeouts: Timeouts{Verify: 60 * time.Second, Stable: 2 * time.Second},
	})
	p := projects.Project{Name: project, Repo: landing.Repo}

	running := func() string {
		out, _ := exec.Command("docker", "exec", project+"-app-1", "printenv", "LIGHTHOUSE_VERSION").Output()
		return strings.TrimSpace(string(out))
	}
	finish := func(sha string, version string, healthy bool) HandOff {
		t.Helper()
		source.archive = tarball(t, files(healthy))
		res := dep.Deploy(ctx, Request{Project: p, SHA: sha, Version: version, Trigger: projects.TriggerCheck})
		if !res.HandedOff {
			t.Fatalf("Deploy %s = %+v\n%s", version, res, log.String())
		}
		if again := dep.Deploy(ctx, Request{Project: p, SHA: sha, Version: version}); again.HandedOff || again.FailedStep != StepHandOff {
			t.Errorf("a second hand-off during the first: %+v", again)
		}
		for {
			out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", HelperName).Output()
			if err != nil || strings.TrimSpace(string(out)) == "false" {
				break
			}
			time.Sleep(time.Second)
		}
		ready, err := dep.HandOffs()
		if err != nil || len(ready) != 1 {
			logs, _ := exec.Command("docker", "logs", HelperName).CombinedOutput()
			t.Fatalf("HandOffs = %+v, %v\nhelper:\n%s", ready, err, logs)
		}
		dep.Forget(ready[0])
		if label, _ := exec.Command("docker", "inspect", "-f", `{{index .Config.Labels "com.docker.compose.project"}}`, HelperName).Output(); strings.TrimSpace(string(label)) != "" {
			t.Errorf("the helper is labelled as compose project %q", strings.TrimSpace(string(label)))
		}
		return ready[0]
	}
	h := finish(sha1, "v1.0.0", true)
	if h.Status != projects.StatusSucceeded || running() != "v1.0.0" {
		logs, _ := exec.Command("docker", "logs", HelperName).CombinedOutput()
		t.Fatalf("v1: %s (%s), running %q\nhelper:\n%s", h.Status, h.Error, running(), logs)
	}
	p.DeployedSHA, p.DeployedVersion = sha1, "v1.0.0"

	h = finish(sha2, "v2.0.0", false)
	if h.Status != projects.StatusRolledBack || h.FailedStep != StepVerify {
		logs, _ := exec.Command("docker", "logs", HelperName).CombinedOutput()
		t.Fatalf("v2: %s at %s (%s)\nhelper:\n%s", h.Status, h.FailedStep, h.Error, logs)
	}
	if running() != "v1.0.0" {
		t.Errorf("after the rollback, running %q", running())
	}
}
