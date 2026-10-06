package builder

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

func (b *Builder) StartContainer(name string) error {
	_, err := b.Docker.ContainerStart(b.Ctx, name, client.ContainerStartOptions{})
	return err
}

func (b *Builder) StopContainer(name string) error {
	_, err := b.Docker.ContainerStop(b.Ctx, name, client.ContainerStopOptions{})
	return err
}

func (b *Builder) RestartContainer(name string) error {
	_, err := b.Docker.ContainerRestart(b.Ctx, name, client.ContainerRestartOptions{})
	return err
}

func (b *Builder) IsContainerRunning(nameOrId string) (bool, error) {

	result, err := b.Docker.ContainerInspect(b.Ctx, nameOrId, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect %q: %w", nameOrId, err)
	}

	info := result.Container
	if info.State == nil {
		return false, fmt.Errorf("no state for %q", nameOrId)
	}

	return info.State.Running, nil
}

func (b *Builder) GetContainerLogs(name string, tail int) (string, error) {
	rc, err := b.Docker.ContainerLogs(b.Ctx, name, client.ContainerLogsOptions{
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
