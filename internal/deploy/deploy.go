// Package deploy deploys a project: download its main branch, unpack it,
// resolve its ${KEY} placeholders from Cove, and docker compose up.
//
// This is still the pre-1.0 pipeline, which stops the running container
// before building the new one (DOCUMENTATION.md B1). Step 3 of the v1.0.0
// plan replaces it with build first, swap last.
package deploy

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/LSariol/LightHouse/internal/docker"
	"github.com/LSariol/LightHouse/internal/projects"
	"github.com/lsariol/coveclient"
)

// Deployer runs deploys, one at a time: every deploy empties the same staging
// and download folders.
type Deployer struct {
	docker       *docker.Client
	cove         *coveclient.Client
	stagingPath  string
	downloadPath string

	mu sync.Mutex
}

func New(d *docker.Client, cove *coveclient.Client, stagingPath, downloadPath string) *Deployer {
	return &Deployer{docker: d, cove: cove, stagingPath: stagingPath, downloadPath: downloadPath}
}

// Deploy deploys p's main branch. A second call waits for the first.
func (d *Deployer) Deploy(ctx context.Context, p projects.Project) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	log := slog.With("project", p.Name)
	log.Info("deploy started")

	if err := d.cleanUp(); err != nil {
		return fmt.Errorf("clean up: %w", err)
	}

	archive, err := d.download(p.Repo.ArchiveURL(), p.Repo.Name)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	if err := d.docker.Stop(ctx, p.Container()); err != nil && !docker.IsNotFound(err) {
		return fmt.Errorf("stop container: %w", err)
	}

	if err := d.unpack(archive); err != nil {
		return fmt.Errorf("unpack: %w", err)
	}

	if err := d.composeUp(p.Container()); err != nil {
		return fmt.Errorf("docker compose: %w", err)
	}

	if err := d.cleanUp(); err != nil {
		return fmt.Errorf("clean up after deploy: %w", err)
	}

	log.Info("deploy finished")
	return nil
}
