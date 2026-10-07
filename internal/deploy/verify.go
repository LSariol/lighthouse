package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/docker"
)

// verify waits until every service is healthy, or stable without a healthcheck.
func (r *run) verify(ctx context.Context, project compose.Project, out io.Writer) *failure {
	deadline := time.Now().Add(r.d.timeouts.Verify)
	stableSince := map[string]time.Time{} // container ID → first seen running
	restarts := map[string]int{}          // container ID → restart count when first seen

	for {
		pending, f := r.checkServices(ctx, project, stableSince, restarts)
		if f != nil {
			return f
		}
		if len(pending) == 0 {
			fmt.Fprintf(out, "every service is up: %s\n", serviceNames(project))
			return nil
		}
		if time.Now().After(deadline) {
			return permanent(fmt.Errorf("not up after %s: %s", r.d.timeouts.Verify, strings.Join(pending, "; ")))
		}

		select {
		case <-ctx.Done():
			return permanent(ctx.Err())
		case <-time.After(r.d.poll):
		}
	}
}

// checkServices looks at every service once.
func (r *run) checkServices(ctx context.Context, project compose.Project, stableSince map[string]time.Time, restarts map[string]int) ([]string, *failure) {
	containers, err := r.d.docker.ProjectContainers(ctx, project.Name)
	if err != nil {
		return nil, transient(err)
	}
	byService := map[string][]docker.Container{}
	for _, c := range containers {
		byService[c.Service] = append(byService[c.Service], c)
	}

	var pending []string
	for _, s := range project.Services {
		cs := byService[s.Name]
		if len(cs) == 0 {
			pending = append(pending, s.Name+": no container yet")
			continue
		}
		for _, c := range cs {
			d, err := r.d.docker.Inspect(ctx, c.ID)
			if err != nil {
				return nil, transient(err)
			}
			if _, seen := restarts[c.ID]; !seen {
				restarts[c.ID] = d.RestartCount
			}
			why, f := r.serviceState(s.Name, c.ID, d, stableSince, restarts[c.ID])
			if f != nil {
				return nil, f
			}
			if why != "" {
				pending = append(pending, s.Name+": "+why)
			}
		}
	}
	return pending, nil
}

// serviceState judges one container: "" if it's up, a reason if it may still
// come up, or a failure if it won't.
func (r *run) serviceState(service string, id string, d docker.Detail, stableSince map[string]time.Time, firstRestarts int) (string, *failure) {
	oneOff := d.RestartPolicy == "" || d.RestartPolicy == "no"

	switch {
	case d.Health == "unhealthy":
		return "", permanent(fmt.Errorf("%s is unhealthy", service))
	case d.RestartCount > firstRestarts || d.State == "restarting":
		return "", permanent(fmt.Errorf("%s keeps restarting (last exit code %d)", service, d.ExitCode))
	case d.State == "exited" && oneOff && d.ExitCode == 0:
		return "", nil
	case d.State == "exited" || d.State == "dead":
		return "", permanent(fmt.Errorf("%s stopped with exit code %d", service, d.ExitCode))
	case d.State != "running":
		return "state " + d.State, nil
	case d.Health == "healthy":
		return "", nil
	case d.Health == "starting":
		return "health check starting", nil
	}

	since, ok := stableSince[id]
	if !ok {
		since = time.Now()
		stableSince[id] = since
	}
	if left := r.d.timeouts.Stable - time.Since(since); left > 0 {
		return fmt.Sprintf("running, watching for %s more", left.Round(time.Second)), nil
	}
	return "", nil
}

// cleanup removes old deploy folders, rollback tags, unused images and build cache.
func (r *run) cleanup(ctx context.Context, project compose.Project, dir string, out io.Writer) {
	keep := map[string]bool{short12(r.req.SHA): true}
	if prev := r.req.Project.DeployedSHA; prev != "" {
		keep[short12(prev)] = true
	}

	entries, _ := os.ReadDir(filepath.Dir(dir))
	var removed []string
	for _, e := range entries {
		if e.IsDir() && !keep[e.Name()] {
			if err := os.RemoveAll(filepath.Join(filepath.Dir(dir), e.Name())); err != nil {
				fmt.Fprintf(out, "! remove the old deploy folder %s: %v\n", e.Name(), err)
			} else {
				removed = append(removed, e.Name())
			}
		}
	}
	if len(removed) > 0 {
		sort.Strings(removed)
		fmt.Fprintf(out, "removed old deploy folders: %s\n", strings.Join(removed, ", "))
	}

	for _, s := range project.Services {
		if !s.Build {
			continue
		}
		repo := repository(s.ImageName(project.Name))
		tags, err := r.d.docker.Tags(ctx, repo, rollbackPrefix)
		if err != nil {
			fmt.Fprintf(out, "! %v\n", err)
			continue
		}
		for _, tag := range tags {
			if !keep[strings.TrimPrefix(tag, rollbackPrefix)] {
				if err := r.d.docker.Untag(ctx, repo+":"+tag); err != nil {
					fmt.Fprintf(out, "! %v\n", err)
				}
			}
		}
	}

	if err := r.d.docker.PruneDangling(ctx); err != nil {
		fmt.Fprintf(out, "! remove unused images: %v\n", err)
	}
	if time.Since(r.d.lastCachePrune) > 24*time.Hour {
		if err := r.d.docker.PruneBuildCache(ctx, 7*24*time.Hour); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(out, "! remove old build cache: %v\n", err)
		} else {
			r.d.lastCachePrune = time.Now()
		}
	}
}
