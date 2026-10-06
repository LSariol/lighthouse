package builder

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
)

func (b *Builder) StartContainer(name string) error {
	return b.Docker.ContainerStart(b.Ctx, name, container.StartOptions{})
}

func (b *Builder) StopContainer(name string) error {
	return b.Docker.ContainerStop(b.Ctx, name, container.StopOptions{})
}

func (b *Builder) RestartContainer(name string) error {
	return b.Docker.ContainerRestart(b.Ctx, name, container.StopOptions{})
}

func (b *Builder) IsContainerRunning(nameOrId string) (bool, error) {

	info, err := b.Docker.ContainerInspect(b.Ctx, nameOrId)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect %q: %w", nameOrId, err)
	}

	if info.State == nil {
		return false, fmt.Errorf("no state for %q", nameOrId)
	}

	return info.State.Running, nil
}

func (b *Builder) GetContainerLogs(name string, tail int) (string, error) {
	rc, err := b.Docker.ContainerLogs(b.Ctx, name, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(tail),
	})
	if err != nil {
		return "", fmt.Errorf("logs %q: %w", name, err)
	}
	defer rc.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, rc); err != nil && err != io.EOF {
		return "", fmt.Errorf("logs %q: read: %w", name, err)
	}

	combined := stdout.String()
	if s := stderr.String(); s != "" {
		combined += s
	}
	return combined, nil
}

func (b *Builder) StartAllContainers() error {

	for _, repo := range b.WatchList {
		name := strings.ToLower(repo.ContainerName)

		err := b.StartContainer(name)
		if err != nil {
			return fmt.Errorf("starting all containers: %s: %w", name, err)
		}
	}

	return nil
}

func (b *Builder) StopAllContainers() error {

	for _, repo := range b.WatchList {
		name := strings.ToLower(repo.ContainerName)

		err := b.StopContainer(name)
		if err != nil {
			return fmt.Errorf("starting all containers: %s: %w", name, err)
		}
	}

	return nil
}
