package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/projects"
)

// Self-update. Lighthouse can't swap its own container: `docker compose up`
// would stop the process doing it. So a deploy of Lighthouse's own compose
// project (Options.Self) runs as usual up to the swap, building the new
// image while this Lighthouse keeps working, and then hands off:
//
//  1. It writes what the swap needs (no secrets) to
//     <root>/.self-update/<commit>.json, and starts a helper container from
//     the new image: `lighthouse self-update <file>`.
//  2. The helper swaps (docker compose up), waits until the new Lighthouse
//     is healthy (`lighthouse health`), and puts the old one back if it
//     isn't. It writes the result into the file and exits. Its container
//     stays, stopped, so `docker logs lighthouse-updater` shows what
//     happened; the next update removes it.
//  3. Whichever Lighthouse runs afterwards records the result (HandOffs,
//     Forget).

// HelperName is the update helper's container.
const HelperName = "lighthouse-updater"

// StepHandOff hands a self-update to the helper.
const StepHandOff = "handoff"

// HandOffWait is how long a hand-off may stay unfinished before it's
// recorded as failed: the helper died, or was never started.
const HandOffWait = 30 * time.Minute

// HandOff is a self-update in progress: what the helper needs, and, once
// it's done, how it went.
type HandOff struct {
	Path string `json:"-"` // the file it's in

	Project        projects.Project  `json:"project"` // as before the deploy: DeployedSHA is the version to go back to
	SHA            string            `json:"sha"`
	Version        string            `json:"version,omitempty"`
	Trigger        string            `json:"trigger"`
	ComposeProject string            `json:"composeProject"`
	Dir            string            `json:"dir"`
	Services       []compose.Service `json:"services"`
	Previous       map[string]string `json:"previous"` // service → image ID running before
	StartedAt      time.Time         `json:"startedAt"`
	Steps          []projects.Step   `json:"steps"`

	// Written by the helper.
	Done        bool      `json:"done,omitempty"`
	Status      string    `json:"status,omitempty"`
	FailureKind string    `json:"failureKind,omitempty"`
	FailedStep  string    `json:"failedStep,omitempty"`
	Error       string    `json:"error,omitempty"`
	FinishedAt  time.Time `json:"finishedAt,omitempty"`
}

// Deployment is the hand-off as a deployment to record.
func (h HandOff) Deployment() projects.Deployment {
	return projects.Deployment{
		Project: h.Project.Name, SHA: h.SHA, Version: h.Version, Trigger: h.Trigger,
		Status: h.Status, FailureKind: h.FailureKind, FailedStep: h.FailedStep,
		StartedAt: h.StartedAt, FinishedAt: h.FinishedAt, Error: h.Error, Steps: h.Steps,
	}
}

func (d *Deployer) handOffDir() string { return filepath.Join(d.root, ".self-update") }

// handOff writes the hand-off and starts the helper from the new image.
func (r *run) handOff(ctx context.Context, project compose.Project, dir string, secrets int, out io.Writer) *failure {
	if secrets > 0 {
		return permanent(errors.New("Lighthouse's own compose file can't use secrets (${...} without a default): the update helper would have to keep them on disk. Lighthouse reads its secrets from Cove itself"))
	}
	image := ""
	for _, s := range project.Services {
		if s.Build {
			image = rollbackRef(s.ImageName(project.Name), r.req.SHA)
			break
		}
	}
	if image == "" {
		return permanent(errors.New("Lighthouse's compose file builds no image, so there's no new version to run the update helper from"))
	}

	// The file carries the steps so far, and this one: it's done by the
	// time the helper reads it.
	now := time.Now()
	steps := append(append([]projects.Step{}, r.steps...), projects.Step{Name: StepHandOff, Status: projects.StepSucceeded,
		StartedAt: now, FinishedAt: now, Log: "handed off to the update helper (" + HelperName + ") running " + image + "\n"})
	h := HandOff{
		Project: r.req.Project, SHA: r.req.SHA, Version: r.req.Version, Trigger: r.req.Trigger,
		ComposeProject: project.Name, Dir: dir, Services: project.Services, Previous: r.previous,
		StartedAt: r.started, Steps: steps,
	}
	if err := os.MkdirAll(r.d.handOffDir(), 0o700); err != nil {
		return transient(fmt.Errorf("self-update: create %s: %w", r.d.handOffDir(), err))
	}
	h.Path = filepath.Join(r.d.handOffDir(), short12(r.req.SHA)+".json")
	if err := writeHandOff(h); err != nil {
		return transient(err)
	}

	// A helper left from the last update (stopped) is removed first.
	if err := r.d.compose.RemoveContainer(ctx, HelperName); err != nil {
		fmt.Fprintf(out, "! remove the last update's helper: %v\n", err)
	}
	fmt.Fprintf(out, "starting the update helper (%s) from %s\n", HelperName, image)
	err := r.d.compose.RunDetached(ctx, HelperName, image,
		[]string{"/var/run/docker.sock:/var/run/docker.sock", r.d.root + ":" + r.d.root},
		[]string{"/lighthouse", "self-update", h.Path})
	if err != nil {
		os.Remove(h.Path)
		return transient(fmt.Errorf("self-update: start the helper: %w", err))
	}
	fmt.Fprintf(out, "handed off: the helper swaps Lighthouse now; \"docker logs -f %s\" shows how it goes\n", HelperName)
	return nil
}

func writeHandOff(h HandOff) error {
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return fmt.Errorf("self-update: %w", err)
	}
	tmp := h.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("self-update: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, h.Path); err != nil {
		return fmt.Errorf("self-update: write %s: %w", h.Path, err)
	}
	return nil
}

func readHandOff(path string) (HandOff, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return HandOff{}, fmt.Errorf("self-update: read %s: %w", path, err)
	}
	var h HandOff
	if err := json.Unmarshal(data, &h); err != nil {
		return HandOff{}, fmt.Errorf("self-update: %s isn't a hand-off: %w", path, err)
	}
	h.Path = path
	return h, nil
}

// FinishHandOff is the helper's work: swap to the new Lighthouse, wait until
// it's healthy, put the old one back if not, and write the result into the
// hand-off file.
func (d *Deployer) FinishHandOff(ctx context.Context, path string) error {
	h, err := readHandOff(path)
	if err != nil {
		return err
	}
	if h.Done {
		return fmt.Errorf("self-update: %s is already finished (%s)", path, h.Status)
	}

	r := &run{d: d, req: Request{Project: h.Project, SHA: h.SHA, Version: h.Version, Trigger: h.Trigger},
		log: slog.With("project", h.Project.Name, "sha", short(h.SHA)), previous: h.Previous,
		prevEnv: []string{}, fallbackDir: h.Dir}
	project := compose.Project{Name: h.ComposeProject, Services: h.Services}
	env := deployVars(h.SHA, h.Version)

	res := Result{}
	fail := func(step string, f *failure) Result {
		res.Status, res.FailedStep, res.FailureKind = projects.StatusFailed, step, f.kind
		res.Err = fmt.Errorf("%s: %w", step, f.err)
		return res
	}
	if f := r.step(ctx, StepSwap, func(ctx context.Context, out io.Writer) *failure {
		if err := d.compose.Up(ctx, h.Dir, project.Name, env, out); err != nil {
			return permanent(err)
		}
		return nil
	}); f != nil {
		res = r.rollBack(ctx, fail(StepSwap, f), project, h.Dir)
	} else if f := r.step(ctx, StepVerify, func(ctx context.Context, out io.Writer) *failure {
		return r.verify(ctx, project, out)
	}); f != nil {
		res = r.rollBack(ctx, fail(StepVerify, f), project, h.Dir)
	} else {
		r.step(ctx, StepCleanup, func(ctx context.Context, out io.Writer) *failure {
			r.cleanup(ctx, project, h.Dir, out)
			return nil
		})
		res.Status = projects.StatusSucceeded
	}

	h.Done, h.Status, h.FailureKind, h.FailedStep = true, res.Status, res.FailureKind, res.FailedStep
	if res.Err != nil {
		h.Error = projects.ErrorText(res.Err)
	}
	h.FinishedAt = time.Now()
	h.Steps = append(h.Steps, r.steps...)
	if err := writeHandOff(h); err != nil {
		return err
	}
	if res.Err != nil {
		return fmt.Errorf("self-update of %s: %w", short(h.SHA), res.Err)
	}
	return nil
}

// handOffs reads every hand-off file.
func (d *Deployer) handOffs() ([]HandOff, error) {
	entries, err := os.ReadDir(d.handOffDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("self-update: read %s: %w", d.handOffDir(), err)
	}
	var all []HandOff
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		h, err := readHandOff(filepath.Join(d.handOffDir(), e.Name()))
		if err != nil {
			slog.Error("a self-update hand-off can't be read; it's left for a person to look at", "err", err)
			continue
		}
		all = append(all, h)
	}
	return all, nil
}

// HandOffs returns the self-updates that are ready to record: finished by
// the helper, or unfinished for too long (recorded as failed).
func (d *Deployer) HandOffs() ([]HandOff, error) {
	all, err := d.handOffs()
	if err != nil {
		return nil, err
	}
	var ready []HandOff
	for _, h := range all {
		if !h.Done {
			if time.Since(h.StartedAt) < HandOffWait {
				continue
			}
			h.Done, h.Status, h.FailureKind, h.FailedStep = true, projects.StatusFailed, projects.FailureTransient, StepHandOff
			h.Error = fmt.Sprintf("the update helper didn't finish within %s; \"docker logs %s\" may say why", HandOffWait, HelperName)
			h.FinishedAt = time.Now()
		}
		ready = append(ready, h)
	}
	return ready, nil
}

// HandOffInProgress reports whether the helper is still working on a
// self-update of the project: one handed off, not yet finished, and not
// given up on. Meanwhile nothing else deploys it: the Lighthouse that
// starts during the swap would otherwise update itself again.
func (d *Deployer) HandOffInProgress(project string) bool {
	all, _ := d.handOffs()
	for _, h := range all {
		if !h.Done && strings.EqualFold(h.Project.Name, project) && time.Since(h.StartedAt) < HandOffWait {
			return true
		}
	}
	return false
}

// Forget deletes a recorded hand-off.
func (d *Deployer) Forget(h HandOff) error {
	if err := os.Remove(h.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("self-update: remove %s: %w", h.Path, err)
	}
	return nil
}

// The variables every deploy gives Compose (they aren't secrets), so a
// compose file can label what it runs: ${LIGHTHOUSE_DEPLOY_VERSION:-dev}.
const (
	DeployCommitVar  = "LIGHTHOUSE_DEPLOY_COMMIT"
	DeployVersionVar = "LIGHTHOUSE_DEPLOY_VERSION"
)

// deployVars are the deploy's variables: the commit, and the release (or
// the commit's first 7 characters).
func deployVars(sha string, version string) []string {
	if version == "" {
		version = short(sha)
	}
	return []string{DeployCommitVar + "=" + sha, DeployVersionVar + "=" + version}
}
