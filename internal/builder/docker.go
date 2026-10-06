package builder

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

func (b *Builder) StartContainer(ctx context.Context, name string) error {
	_, err := b.docker.ContainerStart(ctx, name, client.ContainerStartOptions{})
	return err
}

func (b *Builder) StopContainer(ctx context.Context, name string) error {
	_, err := b.docker.ContainerStop(ctx, name, client.ContainerStopOptions{})
	return err
}

func (b *Builder) RestartContainer(ctx context.Context, name string) error {
	_, err := b.docker.ContainerRestart(ctx, name, client.ContainerRestartOptions{})
	return err
}

// ContainerState is a container's state as Docker reports it ("running",
// "exited", ...), or "missing" when there's no such container.
func (b *Builder) ContainerState(ctx context.Context, name string) (string, error) {
	result, err := b.docker.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return "missing", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect %q: %w", name, err)
	}
	if result.Container.State == nil {
		return "", fmt.Errorf("inspect %q: Docker reported no state", name)
	}
	return string(result.Container.State.Status), nil
}

// ContainerLogs returns the last tail lines of a container's output, stdout
// and stderr combined.
func (b *Builder) ContainerLogs(ctx context.Context, name string, tail int) (string, error) {
	rc, err := b.docker.ContainerLogs(ctx, name, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(tail),
	})
	if err != nil {
		return "", fmt.Errorf("logs %q: %w", name, err)
	}
	defer rc.Close()

	var output bytes.Buffer
	if _, err := stdcopy.StdCopy(&output, &output, rc); err != nil && err != io.EOF {
		return "", fmt.Errorf("logs %q: read: %w", name, err)
	}
	return output.String(), nil
}

// IsNotFound reports whether err means Docker has no such container.
func IsNotFound(err error) bool {
	return cerrdefs.IsNotFound(err)
}
