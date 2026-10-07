package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lsariol/lighthouse/internal/projects"
)

// Reconcile brings back projects that are down: deployed, not stopped on
// purpose, and with no container running where one should be. Docker's
// restart policies bring containers back after a crash or a reboot; this is
// for what they can't, such as containers that were removed. A project must
// look down on two passes in a row, so one Docker is restarting isn't
// mistaken for one that's gone. Projects come back in order, data first,
// each deploy waiting until it's healthy.
func (o *Orchestrator) Reconcile(ctx context.Context) error {
	if o.containers == nil {
		return nil
	}
	list, err := o.store.List(ctx)
	if err != nil {
		return err
	}
	byOrder(list)

	var failed []string
	for _, p := range list {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w := o.watchOf(p.Name)
		if p.Stopped || p.DeployedSHA == "" {
			w.downSince = time.Time{}
			continue
		}
		down, why, err := o.down(ctx, p)
		if err != nil {
			failed = append(failed, p.Name)
			slog.Error("reconcile: can't tell whether a project is running", "project", p.Name, "err", err)
			continue
		}
		if !down {
			w.downSince = time.Time{}
			continue
		}
		if w.downSince.IsZero() {
			w.downSince = o.now()
			slog.Info("reconcile: project looks down; checking again next pass", "project", p.Name, "why", why)
			continue
		}
		if o.now().Before(w.nextAttempt) {
			continue
		}

		t := target{sha: p.DeployedSHA, version: p.DeployedVersion}
		slog.Warn("reconcile: bringing a project back", "project", p.Name, "why", why, "what", t.String())
		if err := o.deploy(ctx, p, t, projects.TriggerReconcile); err != nil {
			failed = append(failed, p.Name)
			o.record(ctx, p.Name, err, true)
			continue
		}
		w.downSince = time.Time{}
	}

	if len(failed) > 0 {
		return fmt.Errorf("failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

// down reports whether p has nothing running that should be: no containers
// at all, or none running while a long-running service (one with a restart
// policy) is stopped. A project of one-off jobs that finished isn't down.
func (o *Orchestrator) down(ctx context.Context, p projects.Project) (bool, string, error) {
	cs, err := o.containers.ProjectContainers(ctx, p.ComposeName())
	if err != nil {
		return false, "", err
	}
	if len(cs) == 0 {
		return true, "it has no containers", nil
	}
	for _, c := range cs {
		if c.State == "running" || c.State == "restarting" {
			return false, "", nil
		}
	}
	for _, c := range cs {
		d, err := o.containers.Inspect(ctx, c.ID)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return false, "", err
			}
			continue // removed meanwhile
		}
		if d.RestartPolicy != "" && d.RestartPolicy != "no" {
			return true, fmt.Sprintf("%s is %s", c.Service, c.State), nil
		}
	}
	return false, "", nil
}
