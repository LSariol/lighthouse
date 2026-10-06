// Package builder deploys a project: download, unpack, resolve secrets from
// Cove, docker compose up. It also starts, stops and inspects containers.
package builder

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/LSariol/LightHouse/internal/models"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/lsariol/coveclient"
	"github.com/moby/moby/client"
)

type Builder struct {
	docker       *client.Client
	cove         *coveclient.Client
	stagingPath  string
	downloadPath string

	// deploying allows one deploy at a time: every deploy empties the same
	// staging and download folders.
	deploying sync.Mutex
}

func New(docker *client.Client, cove *coveclient.Client, stagingPath, downloadPath string) *Builder {
	return &Builder{
		docker:       docker,
		cove:         cove,
		stagingPath:  stagingPath,
		downloadPath: downloadPath,
	}
}

// ContainerName is the container Lighthouse manages for a project: the
// lowercased repository name (the project's compose file must use it).
func ContainerName(repo models.WatchedRepo) string {
	return strings.ToLower(repo.ContainerName)
}

// Build deploys repo's main branch. Deploys run one at a time; a second call
// waits for the first to finish.
func (b *Builder) Build(ctx context.Context, repo models.WatchedRepo) error {
	b.deploying.Lock()
	defer b.deploying.Unlock()

	log := slog.With("project", repo.DisplayName)
	log.Info("deploy started")

	if err := b.cleanUp(); err != nil {
		return fmt.Errorf("clean up: %w", err)
	}

	if err := b.downloadNewCommit(repo.DownloadURL, repo.ContainerName); err != nil {
		return fmt.Errorf("download: %w", err)
	}

	// The running container is stopped before the new one is built. A failed
	// build leaves the project down; fixing that order is the v1.0.0 pipeline.
	if err := b.StopContainer(ctx, ContainerName(repo)); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("stop container: %w", err)
	}

	if err := b.unpackNewProject(repo.ContainerName); err != nil {
		return fmt.Errorf("unpack: %w", err)
	}

	if err := b.createContainer(ContainerName(repo)); err != nil {
		return fmt.Errorf("docker compose: %w", err)
	}

	if err := b.cleanUp(); err != nil {
		return fmt.Errorf("clean up after deploy: %w", err)
	}

	log.Info("deploy finished")
	return nil
}
