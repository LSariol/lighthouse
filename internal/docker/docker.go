// Package docker starts, stops and inspects containers through the host's
// Docker daemon (the mounted /var/run/docker.sock).
package docker

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

// Client is a Docker connection. Create it with New.
type Client struct {
	api *client.Client
}

// New connects to the Docker daemon named by the environment (DOCKER_HOST,
// else the default socket). The API version is negotiated on first use.
func New() (*Client, error) {
	api, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("Docker client: %w", err)
	}
	return &Client{api: api}, nil
}

func (c *Client) Close() error { return c.api.Close() }

// IsNotFound reports whether err means there's no such container.
func IsNotFound(err error) bool {
	return cerrdefs.IsNotFound(err)
}

func (c *Client) Start(ctx context.Context, name string) error {
	_, err := c.api.ContainerStart(ctx, name, client.ContainerStartOptions{})
	return err
}

func (c *Client) Stop(ctx context.Context, name string) error {
	_, err := c.api.ContainerStop(ctx, name, client.ContainerStopOptions{})
	return err
}

func (c *Client) Restart(ctx context.Context, name string) error {
	_, err := c.api.ContainerRestart(ctx, name, client.ContainerRestartOptions{})
	return err
}

// State is a container's state as Docker reports it ("running", "exited",
// ...), or "missing" when there's no such container.
func (c *Client) State(ctx context.Context, name string) (string, error) {
	result, err := c.api.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if IsNotFound(err) {
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

// Logs returns the last tail lines of a container's output, stdout and
// stderr combined.
func (c *Client) Logs(ctx context.Context, name string, tail int) (string, error) {
	rc, err := c.api.ContainerLogs(ctx, name, client.ContainerLogsOptions{
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
