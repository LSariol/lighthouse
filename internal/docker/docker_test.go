package docker

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LSariol/LightHouse/internal/compose"
)

// These tests need a Docker daemon; without one they're skipped. They create
// a small compose project and remove it afterwards.
func TestProjectContainers(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no Docker daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const project = "lighthouse-docker-test"
	dir := filepath.Join(t.TempDir(), "src")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(`
services:
  web:
    image: alpine:3.24
    command: ["sleep", "300"]
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
  job:
    image: alpine:3.24
    command: ["true"]
`), 0o644)

	r := compose.Runner{}
	var out bytes.Buffer
	t.Cleanup(func() { r.Down(context.Background(), t.TempDir(), project, &out) })
	if err := r.Up(ctx, dir, project, nil, &out); err != nil {
		t.Fatalf("Up: %v\n%s", err, out.String())
	}

	d, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// The healthcheck needs a moment.
	var cs []Container
	for i := 0; i < 30; i++ {
		cs, err = d.ProjectContainers(ctx, project)
		if err == nil && len(cs) == 2 && cs[1].Health == "healthy" && cs[0].State == "exited" {
			break
		}
		time.Sleep(time.Second)
	}
	if len(cs) != 2 || cs[0].Service != "job" || cs[1].Service != "web" || cs[1].Name != project+"-web-1" {
		t.Fatalf("containers = %+v, %v", cs, err)
	}
	if cs[1].State != "running" || cs[1].Health != "healthy" || cs[0].State != "exited" || cs[0].Health != "" {
		t.Errorf("states = %+v", cs)
	}

	web, err := d.Inspect(ctx, cs[1].ID)
	if err != nil || web.RestartPolicy != "unless-stopped" || web.State != "running" || web.StartedAt.IsZero() {
		t.Errorf("Inspect(web) = %+v, %v", web, err)
	}
	job, err := d.Inspect(ctx, cs[0].ID)
	if err != nil || job.RestartPolicy != "no" || job.ExitCode != 0 {
		t.Errorf("Inspect(job) = %+v, %v", job, err)
	}

	if other, _ := d.ProjectContainers(ctx, "no-such-project"); len(other) != 0 {
		t.Errorf("an unknown project has containers: %+v", other)
	}

	// Tags for rollback.
	const repo = "lighthouse-docker-test-img"
	if err := d.Tag(ctx, cs[1].ImageID, repo+":lh-aaa"); err != nil {
		t.Fatal(err)
	}
	d.Tag(ctx, cs[1].ImageID, repo+":lh-bbb")
	d.Tag(ctx, cs[1].ImageID, repo+":other")
	t.Cleanup(func() {
		for _, tag := range []string{"lh-aaa", "lh-bbb", "other"} {
			d.Untag(context.Background(), repo+":"+tag)
		}
	})
	tags, err := d.Tags(ctx, repo, "lh-")
	slices.Sort(tags)
	if err != nil || !slices.Equal(tags, []string{"lh-aaa", "lh-bbb"}) {
		t.Errorf("Tags = %v, %v", tags, err)
	}
	if id, err := d.ImageID(ctx, repo+":lh-aaa"); err != nil || id != cs[1].ImageID {
		t.Errorf("ImageID = %q, %v (want %q)", id, err, cs[1].ImageID)
	}
	if id, err := d.ImageID(ctx, repo+":missing"); err != nil || id != "" {
		t.Errorf("ImageID of a missing image = %q, %v", id, err)
	}
	if err := d.Untag(ctx, repo+":lh-aaa"); err != nil {
		t.Fatal(err)
	}
	if tags, _ := d.Tags(ctx, repo, "lh-"); strings.Join(tags, ",") != "lh-bbb" {
		t.Errorf("after Untag: %v", tags)
	}
}

func TestHealthFromStatus(t *testing.T) {
	for status, want := range map[string]string{
		"Up 3 minutes (healthy)":          "healthy",
		"Up 3 minutes (unhealthy)":        "unhealthy",
		"Up 2 seconds (health: starting)": "starting",
		"Up 3 minutes":                    "",
		"Exited (0) 2 minutes ago":        "",
	} {
		if got := healthFromStatus(status); got != want {
			t.Errorf("healthFromStatus(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestExitCodeFromStatus(t *testing.T) {
	for status, want := range map[string]int{
		"Exited (0) 2 minutes ago":   0,
		"Exited (137) 5 seconds ago": 137,
		"Exited (-1) now":            -1,
		"Up 3 minutes":               0,
	} {
		if got := exitCodeFromStatus(status); got != want {
			t.Errorf("exitCodeFromStatus(%q) = %d, want %d", status, got, want)
		}
	}
}
