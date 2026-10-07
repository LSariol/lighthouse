package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/docker"
	"github.com/lsariol/lighthouse/internal/policy"
	"github.com/lsariol/lighthouse/internal/projects"
)

const privilegedConfig = `{"services": {"web": {"privileged": true}}}`

func TestCheckRefusesBeforeAnythingChanges(t *testing.T) {
	e := newEnv(t)
	e.compose.config = privilegedConfig

	res := e.deploy(t, landing, sha1)
	if res.Status != projects.StatusFailed || res.FailedStep != StepCheck || res.FailureKind != projects.FailurePermanent {
		t.Fatalf("Deploy = status %s, step %s, kind %s, err %v", res.Status, res.FailedStep, res.FailureKind, res.Err)
	}
	if !strings.Contains(res.Err.Error(), "privileged") || !strings.Contains(res.Err.Error(), "policy.json") {
		t.Errorf("error: %v", res.Err)
	}
	if !strings.Contains(res.Steps[2].Log, "✗ web: privileged: true") {
		t.Errorf("check log: %q", res.Steps[2].Log)
	}
	if len(e.secrets.asked) != 0 || len(e.compose.builds) != 0 || len(e.compose.stages) != 0 || len(e.compose.ups) != 0 {
		t.Errorf("went on after the check: asked %v, builds %v, stages %v, ups %v", e.secrets.asked, e.compose.builds, e.compose.stages, e.compose.ups)
	}
	if _, err := os.Stat(filepath.Join(e.root, "website", sha1[:12])); err == nil {
		t.Error("the refused commit's folder is still there")
	}
}

func TestCheckException(t *testing.T) {
	e := newEnv(t)
	e.compose.config = privilegedConfig
	p, err := policy.Parse([]byte(`{"exceptions": [{"project": "website", "allow": ["privileged"], "reason": "needs it"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e.d.policy = p

	res := e.deploy(t, landing, sha1)
	if res.Status != projects.StatusSucceeded {
		t.Fatalf("Deploy = %+v", res)
	}
	if !strings.Contains(res.Steps[2].Log, "allowed by policy.json: needs it") {
		t.Errorf("check log: %q", res.Steps[2].Log)
	}
}

func TestCheckSecretsAndNames(t *testing.T) {
	// Another project's key: refused before Cove is asked for anything.
	e := newEnv(t)
	e.compose.vars = append(e.compose.vars, compose.Variable{Name: "BOTSUITE_COVE_TOKEN"})
	res := e.deploy(t, landing, sha1)
	if res.FailedStep != StepCheck || !strings.Contains(res.Err.Error(), "secret:BOTSUITE_COVE_TOKEN") || len(e.secrets.asked) != 0 {
		t.Errorf("another project's key: step %s, err %v, asked %v", res.FailedStep, res.Err, e.secrets.asked)
	}

	// A name another project's container has on spark; the project's own
	// containers don't count.
	e = newEnv(t)
	e.compose.config = `{"services": {"web": {"container_name": "site", "networks": {"spark": null}}},
		"networks": {"spark": {"name": "spark", "external": true}}}`
	e.docker.names = []docker.NetworkName{
		{Name: "web", Container: "website-web-1", Project: "website"},
		{Name: "site", Container: "site", Project: "other"},
	}
	res = e.deploy(t, landing, sha1)
	if res.FailedStep != StepCheck || !strings.Contains(res.Err.Error(), "name:site") || strings.Contains(res.Err.Error(), "name:web") {
		t.Errorf("name taken: step %s, err %v", res.FailedStep, res.Err)
	}
}

func TestCheckSeesTheFinalFolder(t *testing.T) {
	// The config is read again once the files are in place, so its paths
	// are the deploy folder's, not the download's.
	e := newEnv(t)
	var dirs []string
	inner := e.compose
	e.d.compose = inspectSpy{inner, &dirs}

	if res := e.deploy(t, landing, sha1); res.Status != projects.StatusSucceeded {
		t.Fatalf("Deploy = %+v", res)
	}
	final := filepath.Join(e.root, "website", sha1[:12])
	if len(dirs) != 2 || dirs[1] != final {
		t.Errorf("inspected %v, want the download, then %s", dirs, final)
	}
}

type inspectSpy struct {
	*fakeCompose
	dirs *[]string
}

func (s inspectSpy) Inspect(ctx context.Context, dir string) (compose.Project, error) {
	*s.dirs = append(*s.dirs, dir)
	return s.fakeCompose.Inspect(ctx, dir)
}

func TestTestStage(t *testing.T) {
	withStage := map[string]string{"compose.yaml": "services: {}", "Dockerfile": "FROM golang:1.27 AS build\nFROM build as Test\nRUN go test ./...\nFROM alpine\n"}

	e := newEnv(t)
	e.source.archive = tarball(t, withStage)
	res := e.deploy(t, landing, sha1)
	if res.Status != projects.StatusSucceeded {
		t.Fatalf("Deploy = %+v", res)
	}
	want := filepath.Join(e.root, "website", sha1[:12], "Dockerfile")
	if len(e.compose.stages) != 1 || e.compose.stages[0] != want {
		t.Errorf("test stages built: %v, want %s", e.compose.stages, want)
	}

	// Failing tests stop the deploy before Cove is asked or anything is built.
	e = newEnv(t)
	e.source.archive = tarball(t, withStage)
	e.compose.stageErr = errors.New("docker build --target test failed (exit status 1): FAIL")
	res = e.deploy(t, landing, sha1)
	if res.FailedStep != StepTest || res.FailureKind != projects.FailurePermanent || !strings.Contains(res.Err.Error(), "web's tests failed") {
		t.Errorf("failing tests: step %s, kind %s, err %v", res.FailedStep, res.FailureKind, res.Err)
	}
	if len(e.secrets.asked) != 0 || len(e.compose.builds) != 0 {
		t.Error("went on after failing tests")
	}

	// Without a test stage, nothing runs.
	e = newEnv(t)
	res = e.deploy(t, landing, sha1)
	if len(e.compose.stages) != 0 || !strings.Contains(res.Steps[3].Log, "no test stage") {
		t.Errorf("stages %v, log %q", e.compose.stages, res.Steps[3].Log)
	}
}

func TestDryCheck(t *testing.T) {
	e := newEnv(t)
	e.source.archive = tarball(t, map[string]string{"compose.yaml": "services: {}", "Dockerfile": "FROM alpine AS test\n"})
	claimed := ""
	res := e.d.Check(context.Background(), Request{Project: landing, SHA: sha1, Token: "t",
		Claim: func(ctx context.Context, cp string) error { claimed = cp; return nil }})

	if res.Status != projects.StatusSucceeded || stepStatuses(res) != "fetch=succeeded inspect=succeeded check=succeeded test=succeeded" {
		t.Fatalf("Check = %s, %s, %v", res.Status, stepStatuses(res), res.Err)
	}
	if claimed != "website" || len(e.compose.stages) != 1 {
		t.Errorf("claimed %q, stages %v", claimed, e.compose.stages)
	}
	if len(e.secrets.asked) != 0 || len(e.compose.builds) != 0 || len(e.compose.ups) != 0 {
		t.Error("Check went past the test step")
	}
	entries, _ := os.ReadDir(e.root)
	for _, en := range entries {
		if inside, _ := os.ReadDir(filepath.Join(e.root, en.Name())); len(inside) > 0 {
			t.Errorf("Check left %s/%s behind", en.Name(), inside[0].Name())
		}
	}

	// A refused commit: the remaining check steps are skipped, nothing more.
	e = newEnv(t)
	e.compose.config = privilegedConfig
	res = e.d.Check(context.Background(), Request{Project: landing, SHA: sha1, Token: "t"})
	if res.FailedStep != StepCheck || stepStatuses(res) != "fetch=succeeded inspect=succeeded check=failed test=skipped" {
		t.Errorf("Check of a refused commit: %s, %v", stepStatuses(res), res.Err)
	}
}

func TestBackup(t *testing.T) {
	backups := t.TempDir()
	newBackupEnv := func() *env {
		e := newEnv(t)
		e.d.backups = backups
		e.compose.config = `{"services": {}, "x-lighthouse": {"tier": "data", "backup": "postgres"}}`
		e.compose.services = []compose.Service{{Name: "db", Image: "postgres:16.4"}}
		e.compose.dump = "-- PostgreSQL database cluster dump\n"
		e.compose.up = func(dir string) {
			e.docker.set("website", "db", "db-"+filepath.Base(dir), "pg", running)
		}
		return e
	}

	// The first deploy: nothing runs yet, so there's nothing to back up.
	e := newBackupEnv()
	res := e.deploy(t, landing, sha1)
	if res.Status != projects.StatusSucceeded || res.Settings.Tier != "data" || len(e.compose.execs) != 0 {
		t.Fatalf("first deploy: %s, settings %+v, execs %v, %v", res.Status, res.Settings, e.compose.execs, res.Err)
	}
	if !strings.Contains(res.Steps[6].Log, "isn't running") {
		t.Errorf("backup log: %q", res.Steps[6].Log)
	}

	// The next one dumps the running database first, gzipped, owner-only.
	e.docker.containers["website"][0].Name = "sparkdb"
	p := landing
	p.DeployedSHA = sha1
	res = e.deploy(t, p, sha2)
	if res.Status != projects.StatusSucceeded {
		t.Fatalf("second deploy: %+v", res.Err)
	}
	if len(e.compose.execs) != 1 || !strings.HasPrefix(e.compose.execs[0], "sparkdb: sh -c pg_dumpall") {
		t.Errorf("execs: %v", e.compose.execs)
	}
	files, _ := os.ReadDir(filepath.Join(backups, "website"))
	if len(files) != 1 || !strings.HasSuffix(files[0].Name(), "-"+sha2[:12]+".sql.gz") {
		t.Fatalf("backups: %v", files)
	}
	if info, _ := files[0].Info(); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %v", info.Mode().Perm())
	}

	// A failed backup stops the deploy before the swap.
	e = newBackupEnv()
	e.docker.set("website", "db", "db-1", "pg", running)
	e.docker.containers["website"][0].Name = "sparkdb"
	e.compose.execErr = errors.New("docker exec sparkdb failed (exit status 1): no space left on device")
	res = e.deploy(t, p, sha2)
	if res.FailedStep != StepBackup || res.FailureKind != projects.FailureTransient || len(e.compose.ups) != 0 {
		t.Errorf("failed backup: step %s, kind %s, ups %v, %v", res.FailedStep, res.FailureKind, e.compose.ups, res.Err)
	}

	// Only the newest KeepBackups are kept.
	dir := t.TempDir()
	for i := 0; i < KeepBackups+2; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("x-2026010%d-000000-abc.sql.gz", i)), nil, 0o600)
	}
	pruneBackups(dir, KeepBackups, io.Discard)
	if files, _ := os.ReadDir(dir); len(files) != KeepBackups || files[0].Name() != "x-20260102-000000-abc.sql.gz" {
		t.Errorf("after pruning: %v", files)
	}
}

func TestBadSettings(t *testing.T) {
	e := newEnv(t)
	e.compose.config = `{"services": {}, "x-lighthouse": {"deploy": "tags"}}`
	res := e.deploy(t, landing, sha1)
	if res.FailedStep != StepInspect || !strings.Contains(res.Err.Error(), "deploy can be branch or releases") {
		t.Errorf("Deploy = step %s, %v", res.FailedStep, res.Err)
	}
}

func TestRollbackWithoutCove(t *testing.T) {
	// Swapping sparkdb takes Cove's database away: the rollback must not
	// need Cove. The previous version's secrets were fetched before the swap.
	e := newEnv(t)
	p := deployed(t, e)
	e.compose.up = func(dir string) {
		if filepath.Base(dir) == sha2[:12] {
			e.secrets.err = errors.New("Cove: the database is unreachable")
			e.docker.set("website", "web", "web-new", "img-new", docker.Detail{State: "exited", ExitCode: 1, RestartPolicy: "unless-stopped"})
		} else {
			e.docker.set("website", "web", "web-old", "img-old", healthy)
		}
	}
	res := e.deploy(t, p, sha2)
	if res.Status != projects.StatusRolledBack {
		t.Fatalf("Deploy = %s, %v", res.Status, res.Err)
	}
	last := e.compose.upEnv[len(e.compose.upEnv)-1]
	if !slices.Contains(last, "WEBSITE_DATABASE_URL=postgres://app:hunter22@sparkdb/db") {
		t.Errorf("the rollback's environment: %v", last)
	}
	for _, st := range res.Steps {
		if strings.Contains(st.Log, "hunter22") {
			t.Errorf("a secret in the %s step's output", st.Name)
		}
	}
}
