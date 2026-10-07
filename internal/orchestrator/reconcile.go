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

// Reconcile redeploys projects that have been down for two passes.
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
		return fmt.Errorf("couldn't bring back %s (each project's error is logged)", strings.Join(failed, ", "))
	}
	return nil
}

// down reports whether nothing of p is running that should be.
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
			continue
		}
		if d.RestartPolicy != "" && d.RestartPolicy != "no" {
			return true, fmt.Sprintf("%s is %s", c.Service, c.State), nil
		}
	}
	return false, "", nil
}
