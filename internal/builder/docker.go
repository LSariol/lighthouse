package builder

import (
	"fmt"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
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

func (b *Builder) GetAllContainers() ([]types.Container, error) {

	containers, err := b.Docker.ContainerList(b.Ctx, container.ListOptions{
		All: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	return containers, nil
}

func (b *Builder) GetRunningContainers() ([]types.Container, error) {

	containers, err := b.Docker.ContainerList(b.Ctx, container.ListOptions{
		All: false,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	return containers, nil
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
