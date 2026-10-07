// Package docker talks to the host's Docker daemon (the mounted
// /var/run/docker.sock): a compose project's containers, found by the labels
// Compose puts on them; their state and output; and the images Lighthouse
// keeps for rollback.
package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// The labels Compose puts on every container it creates.
const (
	projectLabel = "com.docker.compose.project"
	serviceLabel = "com.docker.compose.service"
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
		return nil, fmt.Errorf("Docker: connect: %w", err)
	}
	return &Client{api: api}, nil
}

func (c *Client) Close() error { return c.api.Close() }

// IsNotFound reports whether err means there's no such container or image.
func IsNotFound(err error) bool {
	return cerrdefs.IsNotFound(err)
}

// Container is one container of a compose project.
type Container struct {
	ID      string
	Name    string // without the leading "/"
	Service string // the compose service it runs
	State   string // "running", "exited", "restarting", ...
	Health  string // "healthy", "unhealthy", "starting", or "" without a healthcheck
	// ExitCode is the exit code of an exited container, else 0.
	ExitCode int
	ImageID  string
}

// ProjectContainers returns every container of the compose project, running
// or not, sorted by service.
func (c *Client) ProjectContainers(ctx context.Context, project string) ([]Container, error) {
	result, err := c.api.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{}.Add("label", projectLabel+"="+project),
	})
	if err != nil {
		return nil, fmt.Errorf("Docker: list containers of %s: %w", project, err)
	}

	containers := make([]Container, 0, len(result.Items))
	for _, s := range result.Items {
		ct := Container{
			ID:      s.ID,
			Service: s.Labels[serviceLabel],
			State:   string(s.State),
			ImageID: s.ImageID,
		}
		if len(s.Names) > 0 {
			ct.Name = strings.TrimPrefix(s.Names[0], "/")
		}
		if s.Health != nil && s.Health.Status != "none" {
			ct.Health = string(s.Health.Status)
		} else {
			ct.Health = healthFromStatus(s.Status)
		}
		ct.ExitCode = exitCodeFromStatus(s.Status)
		containers = append(containers, ct)
	}
	sort.Slice(containers, func(i, j int) bool {
		if containers[i].Service != containers[j].Service {
			return containers[i].Service < containers[j].Service
		}
		return containers[i].Name < containers[j].Name
	})
	return containers, nil
}

// SelfProject returns the compose project of the container this process
// runs in (found by its hostname, which Docker sets to the container's ID),
// or "" outside Docker.
func (c *Client) SelfProject(ctx context.Context) (string, error) {
	if _, err := os.Stat("/.dockerenv"); err != nil {
		return "", nil // not in a container
	}
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("Docker: find Lighthouse's own container: %w", err)
	}
	info, err := c.api.ContainerInspect(ctx, host, client.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("Docker: find Lighthouse's own container (hostname %q): %w", host, err)
	}
	if info.Container.Config == nil {
		return "", nil
	}
	return info.Container.Config.Labels[projectLabel], nil
}

// NetworkName is a name a container answers to on a network.
type NetworkName struct {
	Name      string
	Container string // the container's name
	Project   string // its compose project, or ""
}

// NetworkNames returns every name containers on the network answer to
// (their names, service names and aliases), including stopped containers',
// which come back when started. A container's short ID isn't listed.
func (c *Client) NetworkNames(ctx context.Context, network string) ([]NetworkName, error) {
	result, err := c.api.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{}.Add("network", network),
	})
	if err != nil {
		return nil, fmt.Errorf("Docker: list containers on %s: %w", network, err)
	}

	var names []NetworkName
	for _, s := range result.Items {
		info, err := c.api.ContainerInspect(ctx, s.ID, client.ContainerInspectOptions{})
		if IsNotFound(err) {
			continue // removed meanwhile
		}
		if err != nil {
			return nil, fmt.Errorf("Docker: inspect container %.12s: %w", s.ID, err)
		}
		ct := info.Container
		container := strings.TrimPrefix(ct.Name, "/")
		project := ct.Config.Labels[projectLabel]

		seen := map[string]bool{}
		add := func(n string) {
			if n == "" || seen[n] || strings.HasPrefix(ct.ID, n) {
				return
			}
			seen[n] = true
			names = append(names, NetworkName{Name: n, Container: container, Project: project})
		}
		add(container)
		if ct.NetworkSettings != nil {
			if ep := ct.NetworkSettings.Networks[network]; ep != nil {
				for _, n := range ep.DNSNames {
					add(n)
				}
				for _, n := range ep.Aliases {
					add(n)
				}
			}
		}
	}
	return names, nil
}

// healthFromStatus reads the health from Docker's status text, e.g.
// "Up 3 minutes (healthy)", for daemons whose container list doesn't
// report it separately.
func healthFromStatus(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	}
	return ""
}

var exitedStatus = regexp.MustCompile(`^Exited \((-?\d+)\)`)

// exitCodeFromStatus reads the exit code from Docker's status text, e.g.
// "Exited (1) 2 hours ago".
func exitCodeFromStatus(status string) int {
	if m := exitedStatus.FindStringSubmatch(status); m != nil {
		code, _ := strconv.Atoi(m[1])
		return code
	}
	return 0
}

// Detail is what a deploy checks about a container after starting it.
type Detail struct {
	State         string // "running", "exited", "restarting", ...
	Health        string // "healthy", "unhealthy", "starting", or "" without a healthcheck
	ExitCode      int
	RestartCount  int
	RestartPolicy string // "no", "always", "unless-stopped", "on-failure"
	StartedAt     time.Time
}

// Inspect returns a container's state. (Docker's inspect also returns the
// container's environment, which holds secrets; only these fields leave
// this package.)
func (c *Client) Inspect(ctx context.Context, id string) (Detail, error) {
	result, err := c.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return Detail{}, fmt.Errorf("Docker: inspect container %.12s: %w", id, err)
	}
	info := result.Container
	var d Detail
	d.RestartCount = info.RestartCount
	if info.HostConfig != nil {
		d.RestartPolicy = string(info.HostConfig.RestartPolicy.Name)
	}
	if s := info.State; s != nil {
		d.State = string(s.Status)
		d.ExitCode = s.ExitCode
		d.StartedAt, _ = time.Parse(time.RFC3339Nano, s.StartedAt)
		if s.Health != nil && s.Health.Status != "none" {
			d.Health = string(s.Health.Status)
		}
	}
	return d, nil
}

func (c *Client) Start(ctx context.Context, id string) error {
	if _, err := c.api.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("Docker: start container %.12s: %w", id, err)
	}
	return nil
}

func (c *Client) Stop(ctx context.Context, id string) error {
	if _, err := c.api.ContainerStop(ctx, id, client.ContainerStopOptions{}); err != nil {
		return fmt.Errorf("Docker: stop container %.12s: %w", id, err)
	}
	return nil
}

func (c *Client) Restart(ctx context.Context, id string) error {
	if _, err := c.api.ContainerRestart(ctx, id, client.ContainerRestartOptions{}); err != nil {
		return fmt.Errorf("Docker: restart container %.12s: %w", id, err)
	}
	return nil
}

// Logs returns the last tail lines of a container's output, stdout and
// stderr combined.
func (c *Client) Logs(ctx context.Context, id string, tail int) (string, error) {
	rc, err := c.api.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(tail),
	})
	if err != nil {
		return "", fmt.Errorf("Docker: read the logs of container %.12s: %w", id, err)
	}
	defer rc.Close()

	var output bytes.Buffer
	if _, err := stdcopy.StdCopy(&output, &output, rc); err != nil && err != io.EOF {
		return "", fmt.Errorf("Docker: read the logs of container %.12s: %w", id, err)
	}
	return output.String(), nil
}

// ImageID returns the ID of the image ref names, or "" if there's none.
func (c *Client) ImageID(ctx context.Context, ref string) (string, error) {
	result, err := c.api.ImageInspect(ctx, ref)
	if IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("Docker: inspect image %s: %w", ref, err)
	}
	return result.ID, nil
}

// Tag gives the image source (an ID or a name) the name target.
func (c *Client) Tag(ctx context.Context, source string, target string) error {
	if _, err := c.api.ImageTag(ctx, client.ImageTagOptions{Source: source, Target: target}); err != nil {
		return fmt.Errorf("Docker: tag %s as %s: %w", source, target, err)
	}
	return nil
}

// Tags returns the tags of repository (e.g. "website-web") that start with
// prefix, e.g. "lh-".
func (c *Client) Tags(ctx context.Context, repository string, prefix string) ([]string, error) {
	result, err := c.api.ImageList(ctx, client.ImageListOptions{
		Filters: client.Filters{}.Add("reference", repository+":"+prefix+"*"),
	})
	if err != nil {
		return nil, fmt.Errorf("Docker: list images of %s: %w", repository, err)
	}
	var tags []string
	for _, img := range result.Items {
		for _, ref := range img.RepoTags {
			if name, tag, ok := strings.Cut(ref, ":"); ok && name == repository && strings.HasPrefix(tag, prefix) {
				tags = append(tags, tag)
			}
		}
	}
	return tags, nil
}

// Untag removes the name ref. The image itself goes only when nothing else
// names or uses it.
func (c *Client) Untag(ctx context.Context, ref string) error {
	_, err := c.api.ImageRemove(ctx, ref, client.ImageRemoveOptions{})
	if err != nil && !IsNotFound(err) {
		return fmt.Errorf("Docker: remove %s: %w", ref, err)
	}
	return nil
}

// PruneDangling removes images that have no name and no container: the ones
// each rebuild leaves behind. Images kept for rollback have names.
func (c *Client) PruneDangling(ctx context.Context) error {
	_, err := c.api.ImagePrune(ctx, client.ImagePruneOptions{
		Filters: client.Filters{}.Add("dangling", "true"),
	})
	return err
}

// PruneBuildCache removes build cache not used for the given time.
func (c *Client) PruneBuildCache(ctx context.Context, unused time.Duration) error {
	_, err := c.api.BuildCachePrune(ctx, client.BuildCachePruneOptions{
		Filters: client.Filters{}.Add("until", unused.String()),
	})
	return err
}
