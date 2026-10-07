// Package compose runs docker compose with deadlines and a clean environment.
package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Project is what Lighthouse needs from a compose file.
type Project struct {
	Name     string
	Services []Service
	Config   []byte
}

// Service is one service of a compose project.
type Service struct {
	Name       string
	Image      string
	Build      bool
	Context    string
	Dockerfile string
}

// ImageName is the image Compose gives the service: its `image:`, or
// <project>-<service> for one that's only built.
func (s Service) ImageName(project string) string {
	if s.Image != "" {
		return s.Image
	}
	return project + "-" + s.Name
}

// Variable is a ${...} variable a compose file uses.
type Variable struct {
	Name     string
	Default  string
	Required bool
}

// Runner runs docker compose.
type Runner struct {
	Docker string
}

func (r Runner) command(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
	bin := r.Docker
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"compose"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(BaseEnv(), env...)
	return cmd
}

// run runs a command, sending its output to out and returning an error that
// includes the output's last lines.
func (r Runner) run(ctx context.Context, dir string, env []string, out io.Writer, args ...string) error {
	var tail tailBuffer
	cmd := r.command(ctx, dir, env, args...)
	w := io.MultiWriter(&tail, out)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("docker compose %s: %w", args[0], ctx.Err())
		}
		return fmt.Errorf("docker compose %s failed (%v): %s", firstArg(args), err, tail.lastLines(3))
	}
	return nil
}

// output runs a command and returns its standard output.
func (r Runner) output(ctx context.Context, dir string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := r.command(ctx, dir, nil, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("docker compose %s: %w", firstArg(args), ctx.Err())
		}
		return nil, fmt.Errorf("docker compose %s failed (%v): %s", firstArg(args), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func firstArg(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return args[0]
}

// Inspect reads the compose file in dir without filling in variables.
func (r Runner) Inspect(ctx context.Context, dir string) (Project, error) {
	out, err := r.output(ctx, dir, "config", "--no-interpolate", "--format", "json")
	if err != nil {
		return Project{}, err
	}

	var raw struct {
		Name     string `json:"name"`
		Services map[string]struct {
			Image string `json:"image"`
			Build *struct {
				Context          string `json:"context"`
				Dockerfile       string `json:"dockerfile"`
				DockerfileInline string `json:"dockerfile_inline"`
			} `json:"build"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Project{}, fmt.Errorf("docker compose config: unexpected output: %v", err)
	}

	p := Project{Name: raw.Name, Config: out}
	for name, s := range raw.Services {
		svc := Service{Name: name, Image: s.Image}
		if b := s.Build; b != nil {
			svc.Build = true
			svc.Context = b.Context
			if b.DockerfileInline == "" && !strings.Contains(b.Context, "://") {
				svc.Dockerfile = b.Dockerfile
				if svc.Dockerfile == "" {
					svc.Dockerfile = "Dockerfile"
				}
				if !filepath.IsAbs(svc.Dockerfile) {
					svc.Dockerfile = filepath.Join(b.Context, svc.Dockerfile)
				}
			}
		}
		p.Services = append(p.Services, svc)
	}
	sort.Slice(p.Services, func(i, j int) bool { return p.Services[i].Name < p.Services[j].Name })
	return p, nil
}

// Variables lists the ${...} variables the compose file in dir uses.
func (r Runner) Variables(ctx context.Context, dir string) ([]Variable, error) {
	out, err := r.output(ctx, dir, "config", "--variables", "--format", "json")
	if err != nil {
		return nil, err
	}

	var raw map[string]struct {
		Name         string
		DefaultValue string
		Required     bool
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("docker compose config --variables: unexpected output: %v", err)
	}

	vars := make([]Variable, 0, len(raw))
	for name, v := range raw {
		vars = append(vars, Variable{Name: name, Default: v.DefaultValue, Required: v.Required})
	}
	sort.Slice(vars, func(i, j int) bool { return vars[i].Name < vars[j].Name })
	return vars, nil
}

// Build builds the project's images.
func (r Runner) Build(ctx context.Context, dir string, project string, env []string, out io.Writer) error {
	return r.run(ctx, dir, env, out, "-p", project, "build")
}

// BuildStage builds one stage of a Dockerfile, e.g. its test stage, with
// nothing from the compose file: no build arguments and no secrets.
func (r Runner) BuildStage(ctx context.Context, buildContext string, dockerfile string, target string, out io.Writer) error {
	bin := r.Docker
	if bin == "" {
		bin = "docker"
	}
	var tail tailBuffer
	cmd := exec.CommandContext(ctx, bin, "build", "--target", target, "--file", dockerfile, buildContext)
	cmd.Env = BaseEnv()
	w := io.MultiWriter(&tail, out)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("docker build --target %s: %w", target, ctx.Err())
		}
		return fmt.Errorf("docker build --target %s failed (%v): %s", target, err, tail.lastLines(3))
	}
	return nil
}

// Exec runs a command in a running container, sending its standard output to
// stdout (e.g. a database dump).
func (r Runner) Exec(ctx context.Context, container string, command []string, stdout io.Writer) error {
	bin := r.Docker
	if bin == "" {
		bin = "docker"
	}
	var stderr tailBuffer
	cmd := exec.CommandContext(ctx, bin, append([]string{"exec", container}, command...)...)
	cmd.Env = BaseEnv()
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("docker exec %s: %w", container, ctx.Err())
		}
		return fmt.Errorf("docker exec %s failed (%v): %s", container, err, stderr.lastLines(3))
	}
	return nil
}

// RunDetached starts a container in the background, without Compose labels.
func (r Runner) RunDetached(ctx context.Context, name string, image string, volumes []string, command []string) error {
	args := []string{"run", "-d", "--name", name,
		"--label", "com.docker.compose.project=", "--label", "com.docker.compose.service="}
	for _, v := range volumes {
		args = append(args, "-v", v)
	}
	args = append(args, "--entrypoint", command[0], image)
	args = append(args, command[1:]...)
	return r.docker(ctx, args...)
}

// RemoveContainer removes a container, running or not; one that doesn't
// exist is fine.
func (r Runner) RemoveContainer(ctx context.Context, name string) error {
	err := r.docker(ctx, "rm", "-f", name)
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}

// docker runs a docker command (not compose), with the base environment.
func (r Runner) docker(ctx context.Context, args ...string) error {
	bin := r.Docker
	if bin == "" {
		bin = "docker"
	}
	var out tailBuffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = BaseEnv()
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("docker %s: %w", args[0], ctx.Err())
		}
		return fmt.Errorf("docker %s failed (%v): %s", args[0], err, out.lastLines(3))
	}
	return nil
}

// Up starts the project from images already built, replacing its running
// containers, and removes containers of services it no longer has.
func (r Runner) Up(ctx context.Context, dir string, project string, env []string, out io.Writer) error {
	return r.run(ctx, dir, env, out, "-p", project, "up", "-d", "--no-build", "--remove-orphans")
}

// Down stops and removes the project's containers and networks.
func (r Runner) Down(ctx context.Context, dir string, project string, out io.Writer) error {
	return r.run(ctx, dir, nil, out, "-p", project, "down")
}

// baseVars is the environment docker needs; nothing else of Lighthouse's reaches a project.
var baseVars = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "TMPDIR": true, "TMP": true, "TEMP": true,
	"DOCKER_HOST": true, "DOCKER_CONFIG": true, "DOCKER_CONTEXT": true, "DOCKER_CERT_PATH": true, "DOCKER_TLS_VERIFY": true,
	"SYSTEMROOT": true, "WINDIR": true, "COMSPEC": true, "PATHEXT": true, "USERPROFILE": true,
	"APPDATA": true, "LOCALAPPDATA": true, "PROGRAMDATA": true, "PROGRAMFILES": true, "HOMEDRIVE": true, "HOMEPATH": true,
}

// BaseEnv returns the part of this process's environment that docker needs.
func BaseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if baseVars[strings.ToUpper(name)] {
			env = append(env, kv)
		}
	}
	return env
}

// tailBuffer keeps the last 8 KB written to it.
type tailBuffer struct {
	buf []byte
}

const tailSize = 8 << 10

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailSize {
		t.buf = t.buf[len(t.buf)-tailSize:]
	}
	return len(p), nil
}

func (t *tailBuffer) lastLines(n int) string {
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
