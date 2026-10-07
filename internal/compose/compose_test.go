package compose

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
)

// These tests run the real docker compose. Inspect and Variables need only
// the CLI; Build, Up and Down need a Docker daemon. Without them, the tests
// are skipped.

func needCompose(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose isn't installed")
	}
}

func needDaemon(t *testing.T) {
	t.Helper()
	needCompose(t)
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no Docker daemon")
	}
}

func writeCompose(t *testing.T, dir string, contents string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

const sample = `
services:
  web:
    build: .
    environment:
      - DATABASE_URL=${PLOP_DATABASE_URL}
      - LEVEL=${LEVEL:-info}
      - LITERAL=$${NOT_A_VARIABLE}
  db:
    image: postgres:16
    environment:
      - PASSWORD=${PLOP_DATABASE_PASSWORD:?needed}
`

func TestInspect(t *testing.T) {
	needCompose(t)
	ctx := context.Background()

	dir := filepath.Join(t.TempDir(), "0123456789ab")
	writeCompose(t, dir, sample)
	p, err := Runner{}.Inspect(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "0123456789ab" || len(p.Services) != 2 {
		t.Fatalf("Inspect = %+v", p)
	}
	db, web := p.Services[0], p.Services[1]
	if db.Name != "db" || db.Image != "postgres:16" || db.Build || web.Name != "web" || !web.Build || web.Image != "" {
		t.Errorf("services = %+v", p.Services)
	}
	if web.ImageName("plop") != "plop-web" || db.ImageName("plop") != "postgres:16" {
		t.Errorf("image names: %s, %s", web.ImageName("plop"), db.ImageName("plop"))
	}
	if web.Context != dir || web.Dockerfile != filepath.Join(dir, "Dockerfile") || db.Context != "" {
		t.Errorf("build: context %q, Dockerfile %q", web.Context, web.Dockerfile)
	}
	if !strings.Contains(string(p.Config), `"services"`) {
		t.Errorf("Config isn't the compose config: %.80s", p.Config)
	}

	writeCompose(t, dir, "name: website\n"+sample)
	if p, err := (Runner{}).Inspect(ctx, dir); err != nil || p.Name != "website" {
		t.Errorf("with name: %q, %v", p.Name, err)
	}

	writeCompose(t, dir, "services: [this is not a map")
	if _, err := (Runner{}).Inspect(ctx, dir); err == nil || !strings.Contains(err.Error(), "config failed") {
		t.Errorf("broken file: %v", err)
	}
}

func TestVariables(t *testing.T) {
	needCompose(t)
	dir := t.TempDir()
	writeCompose(t, dir, sample)

	vars, err := Runner{}.Variables(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]Variable{}
	for _, v := range vars {
		names = append(names, v.Name)
		byName[v.Name] = v
	}
	if !slices.Equal(names, []string{"LEVEL", "PLOP_DATABASE_PASSWORD", "PLOP_DATABASE_URL"}) {
		t.Fatalf("variables = %v", names)
	}
	if byName["LEVEL"].Default != "info" || byName["PLOP_DATABASE_URL"].Default != "" || !byName["PLOP_DATABASE_PASSWORD"].Required {
		t.Errorf("variables = %+v", vars)
	}
}

// Lighthouse's own settings must never reach a project's compose file.
func TestBaseEnv(t *testing.T) {
	t.Setenv("COVE_URL", "http://cove:2100")
	t.Setenv("LIGHTHOUSE_CONTROL_SOCKET", "/run/x")
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	env := strings.Join(BaseEnv(), "\n")
	if strings.Contains(env, "COVE_URL") || strings.Contains(env, "LIGHTHOUSE_") {
		t.Error("Lighthouse's settings are in the base environment")
	}
	if !strings.Contains(env, "DOCKER_HOST=") {
		t.Error("DOCKER_HOST isn't passed on")
	}
}

func TestBuildUpDown(t *testing.T) {
	needDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	project := "lighthouse-compose-test"
	dir := filepath.Join(t.TempDir(), "src")
	writeCompose(t, dir, `
services:
  app:
    build: .
    command: ["sleep", "300"]
    environment:
      - SECRET=${LHTEST_SECRET}
`)
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine:3.24\n"), 0o644)

	r := Runner{}
	var out bytes.Buffer
	env := []string{"LHTEST_SECRET=s3cret"}
	t.Cleanup(func() { r.Down(context.Background(), t.TempDir(), project, &out) })

	if err := r.Build(ctx, dir, project, env, &out); err != nil {
		t.Fatalf("Build: %v\n%s", err, out.String())
	}
	if err := r.Up(ctx, dir, project, env, &out); err != nil {
		t.Fatalf("Up: %v\n%s", err, out.String())
	}
	got, err := exec.Command("docker", "exec", project+"-app-1", "printenv", "SECRET").Output()
	if err != nil || strings.TrimSpace(string(got)) != "s3cret" {
		t.Errorf("the secret in the container: %q, %v", got, err)
	}
	var dump bytes.Buffer
	if err := r.Exec(ctx, project+"-app-1", []string{"sh", "-c", `echo "dump of $SECRET"`}, &dump); err != nil || dump.String() != "dump of s3cret\n" {
		t.Errorf("Exec = %q, %v", dump.String(), err)
	}
	if err := r.Exec(ctx, project+"-app-1", []string{"sh", "-c", "echo broken >&2; exit 3"}, &dump); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("a failing Exec = %v", err)
	}
	if err := r.Down(ctx, t.TempDir(), project, &out); err != nil {
		t.Fatalf("Down: %v\n%s", err, out.String())
	}
	if ids, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+project).Output(); len(strings.TrimSpace(string(ids))) > 0 {
		t.Error("Down left containers behind")
	}
}

func TestBuildStage(t *testing.T) {
	needDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dir := t.TempDir()
	dockerfile := filepath.Join(dir, "Dockerfile")
	write := func(test string) {
		os.WriteFile(dockerfile, []byte("FROM alpine:3.24 AS base\nFROM base AS test\nRUN "+test+"\nFROM base\n"), 0o644)
	}

	var out bytes.Buffer
	write("echo tests pass")
	if err := (Runner{}).BuildStage(ctx, dir, dockerfile, "test", &out); err != nil {
		t.Fatalf("a passing test stage: %v\n%s", err, out.String())
	}
	write("echo a test failed && exit 1")
	err := Runner{}.BuildStage(ctx, dir, dockerfile, "test", &out)
	if err == nil || !strings.Contains(err.Error(), "--target test failed") {
		t.Errorf("a failing test stage: %v", err)
	}
}
