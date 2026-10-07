package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/docker"
	"github.com/lsariol/lighthouse/internal/projects"
)

// newSelfEnv is Lighthouse deploying itself: compose project "lighthouse",
// one built service, no secrets.
func newSelfEnv(t *testing.T) *env {
	e := newEnv(t)
	e.d.self = "lighthouse"
	e.compose.name = "lighthouse"
	e.compose.services = []compose.Service{{Name: "lighthouse", Build: true}}
	e.compose.vars = []compose.Variable{{Name: DeployVersionVar, Default: "dev"}}
	e.compose.up = func(dir string) {
		e.docker.set("lighthouse", "lighthouse", "lh-"+filepath.Base(dir), "img-"+filepath.Base(dir), healthy)
	}
	return e
}

var lighthouse = projects.Project{Name: "lighthouse", Repo: landing.Repo}

func TestSelfDeployHandsOff(t *testing.T) {
	e := newSelfEnv(t)
	res := e.d.Deploy(context.Background(), Request{Project: lighthouse, SHA: sha1, Version: "v1.0.0", Trigger: projects.TriggerCheck})

	if !res.HandedOff || res.Err != nil || res.Status != "" {
		t.Fatalf("Deploy = %+v", res)
	}
	if len(e.compose.ups) != 0 {
		t.Error("Lighthouse swapped itself")
	}
	want := HelperName + " lighthouse-lighthouse:lh-" + sha1[:12] + " /lighthouse self-update " + filepath.Join(e.root, ".self-update", sha1[:12]+".json")
	if len(e.compose.helpers) != 1 || e.compose.helpers[0] != want {
		t.Errorf("helpers %v, want %q", e.compose.helpers, want)
	}
	h, err := readHandOff(filepath.Join(e.root, ".self-update", sha1[:12]+".json"))
	if err != nil || h.SHA != sha1 || h.Version != "v1.0.0" || h.Trigger != projects.TriggerCheck || h.Done || len(h.Steps) == 0 {
		t.Errorf("hand-off %+v, %v", h, err)
	}
	if last := res.Steps[len(res.Steps)-1]; last.Name != StepHandOff || last.Status != projects.StepSucceeded {
		t.Errorf("last step %+v", last)
	}

	// Not ready to record until the helper is done.
	if ready, _ := e.d.HandOffs(); len(ready) != 0 {
		t.Errorf("ready before the helper finished: %+v", ready)
	}
}

func TestSelfDeployRefusesSecrets(t *testing.T) {
	e := newSelfEnv(t)
	e.compose.vars = []compose.Variable{{Name: "LIGHTHOUSE_GITHUB_TOKEN"}}
	e.secrets.values["LIGHTHOUSE_GITHUB_TOKEN"] = "ghp_xxxx"
	res := e.d.Deploy(context.Background(), Request{Project: lighthouse, SHA: sha1})
	if res.FailedStep != StepHandOff || !strings.Contains(res.Err.Error(), "can't use secrets") || len(e.compose.helpers) != 0 {
		t.Errorf("Deploy = step %s, %v, helpers %v", res.FailedStep, res.Err, e.compose.helpers)
	}
}

func TestHelperSwapsAndVerifies(t *testing.T) {
	e := newSelfEnv(t)
	p := lighthouse
	p.DeployedSHA = sha1
	os.MkdirAll(filepath.Join(e.root, "lighthouse", sha1[:12]), 0o755) // the running version's files
	e.docker.set("lighthouse", "lighthouse", "lh-old", "img-old", healthy)
	res := e.d.Deploy(context.Background(), Request{Project: p, SHA: sha2, Trigger: projects.TriggerManual})
	if !res.HandedOff {
		t.Fatalf("Deploy = %+v", res)
	}
	path := filepath.Join(e.root, ".self-update", sha2[:12]+".json")

	// The helper (the new image) swaps; the new Lighthouse comes up healthy.
	if err := e.d.FinishHandOff(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if len(e.compose.ups) != 1 || filepath.Base(e.compose.ups[0]) != sha2[:12] {
		t.Errorf("ups %v", e.compose.ups)
	}
	if !slices.Contains(e.compose.upEnv[0], DeployCommitVar+"="+sha2) {
		t.Errorf("swap environment %v", e.compose.upEnv[0])
	}
	ready, err := e.d.HandOffs()
	if err != nil || len(ready) != 1 || ready[0].Status != projects.StatusSucceeded {
		t.Fatalf("HandOffs = %+v, %v", ready, err)
	}
	d := ready[0].Deployment()
	var names []string
	for _, s := range d.Steps {
		names = append(names, s.Name)
	}
	if d.SHA != sha2 || d.Trigger != projects.TriggerManual || !strings.Contains(strings.Join(names, " "), "build backup handoff swap verify cleanup") {
		t.Errorf("deployment %+v, steps %v", d, names)
	}
	if err := e.d.Forget(ready[0]); err != nil {
		t.Fatal(err)
	}
	if ready, _ := e.d.HandOffs(); len(ready) != 0 {
		t.Error("forgotten, but still there")
	}
	if err := e.d.FinishHandOff(context.Background(), path); err == nil {
		t.Error("finishing a forgotten hand-off worked")
	}
}

func TestHelperRollsBack(t *testing.T) {
	e := newSelfEnv(t)
	p := lighthouse
	p.DeployedSHA = sha1
	os.MkdirAll(filepath.Join(e.root, "lighthouse", sha1[:12]), 0o755)
	e.docker.set("lighthouse", "lighthouse", "lh-old", "img-old", healthy)
	e.d.Deploy(context.Background(), Request{Project: p, SHA: sha2})

	// The new Lighthouse never becomes healthy; the old one is put back.
	e.compose.up = func(dir string) {
		if filepath.Base(dir) == sha2[:12] {
			e.docker.set("lighthouse", "lighthouse", "lh-new", "img-new", docker.Detail{State: "running", Health: "unhealthy", RestartPolicy: "unless-stopped"})
		} else {
			e.docker.set("lighthouse", "lighthouse", "lh-old", "img-old", healthy)
		}
	}
	err := e.d.FinishHandOff(context.Background(), filepath.Join(e.root, ".self-update", sha2[:12]+".json"))
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("FinishHandOff = %v", err)
	}
	ready, _ := e.d.HandOffs()
	if len(ready) != 1 || ready[0].Status != projects.StatusRolledBack || ready[0].FailedStep != StepVerify {
		t.Fatalf("HandOffs = %+v", ready)
	}
	if last := e.compose.ups[len(e.compose.ups)-1]; filepath.Base(last) != sha1[:12] || e.docker.tags["lighthouse-lighthouse"] != "img-old" {
		t.Errorf("rolled back in %s, image %q", last, e.docker.tags["lighthouse-lighthouse"])
	}
}

func TestHelperRollsBackWithoutTheOldFolder(t *testing.T) {
	// The first self-update after Lighthouse was deployed by hand: the old
	// version's folder isn't Lighthouse's, so the old image is put back and
	// run with the new compose file.
	e := newSelfEnv(t)
	p := lighthouse
	p.DeployedSHA = sha1
	e.docker.set("lighthouse", "lighthouse", "lh-old", "img-old", healthy)
	e.d.Deploy(context.Background(), Request{Project: p, SHA: sha2})
	e.compose.up = func(dir string) {
		e.docker.set("lighthouse", "lighthouse", "lh-x", "img-x", docker.Detail{State: "exited", ExitCode: 1, RestartPolicy: "unless-stopped"})
	}
	e.d.FinishHandOff(context.Background(), filepath.Join(e.root, ".self-update", sha2[:12]+".json"))
	ready, _ := e.d.HandOffs()
	if len(ready) != 1 || ready[0].Status != projects.StatusRolledBack || len(e.compose.ups) != 2 || e.docker.tags["lighthouse-lighthouse"] != "img-old" {
		t.Errorf("HandOffs %+v, ups %v, tags %v", ready, e.compose.ups, e.docker.tags)
	}
}

func TestStaleHandOff(t *testing.T) {
	e := newSelfEnv(t)
	e.d.Deploy(context.Background(), Request{Project: lighthouse, SHA: sha1})
	path := filepath.Join(e.root, ".self-update", sha1[:12]+".json")
	h, _ := readHandOff(path)
	h.StartedAt = time.Now().Add(-handOffWait - time.Minute)
	writeHandOff(h)

	ready, _ := e.d.HandOffs()
	if len(ready) != 1 || ready[0].Status != projects.StatusFailed || !strings.Contains(ready[0].Error, "didn't finish") {
		t.Errorf("a stale hand-off: %+v", ready)
	}
}

func TestHelperFailsToStart(t *testing.T) {
	e := newSelfEnv(t)
	e.compose.runErr = errors.New("docker run failed (exit status 125): Conflict. The container name is already in use")
	res := e.d.Deploy(context.Background(), Request{Project: lighthouse, SHA: sha1})
	if res.HandedOff || res.FailedStep != StepHandOff || res.FailureKind != projects.FailureTransient || !strings.Contains(res.Err.Error(), "start the helper") {
		t.Errorf("Deploy = %+v", res)
	}
	if ready, _ := e.d.HandOffs(); len(ready) != 0 {
		t.Error("a hand-off was left behind")
	}
}
