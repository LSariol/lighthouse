package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsariol/coveclient"
	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/docker"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/projects"
)

const (
	sha1 = "1111111111111111111111111111111111111111"
	sha2 = "2222222222222222222222222222222222222222"
)

// tarball builds a GitHub-style archive: one top-level folder.
func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": sha1}})
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "owner-repo-1111111/", Mode: 0o755})
	for name, body := range files {
		mode := int64(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "owner-repo-1111111/" + name, Mode: mode, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// fakeSource serves one archive for any commit, or fails.
type fakeSource struct {
	archive []byte
	err     error
}

func (f *fakeSource) Archive(ctx context.Context, repo github.Repo, sha string, token string) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(bytes.NewReader(f.archive)), nil
}

type fakeSecrets struct {
	values map[string]string
	err    error
	asked  []string
}

func (f *fakeSecrets) GetSecretsContext(ctx context.Context, keys ...string) (map[string]string, error) {
	f.asked = append(f.asked, keys...)
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]string{}
	for _, k := range keys {
		out[k] = f.values[k]
	}
	return out, nil
}

// fakeCompose answers from its fields; up runs after a successful Up, to
// let the test set the containers' state.
type fakeCompose struct {
	name     string // "" behaves like a compose file without `name:`
	services []compose.Service
	vars     []compose.Variable
	buildErr error
	upErr    error
	buildOut string

	mu     sync.Mutex
	builds []string // dirs
	ups    []string // dirs
	upEnv  [][]string
	up     func(dir string)
}

func (f *fakeCompose) Inspect(ctx context.Context, dir string) (compose.Project, error) {
	name := f.name
	if name == "" {
		name = filepath.Base(dir)
	}
	return compose.Project{Name: name, Services: f.services}, nil
}

func (f *fakeCompose) Variables(ctx context.Context, dir string) ([]compose.Variable, error) {
	return f.vars, nil
}

func (f *fakeCompose) Build(ctx context.Context, dir string, project string, env []string, out io.Writer) error {
	f.mu.Lock()
	f.builds = append(f.builds, dir)
	f.mu.Unlock()
	io.WriteString(out, f.buildOut)
	return f.buildErr
}

func (f *fakeCompose) Up(ctx context.Context, dir string, project string, env []string, out io.Writer) error {
	f.mu.Lock()
	f.ups = append(f.ups, dir)
	f.upEnv = append(f.upEnv, env)
	f.mu.Unlock()
	if f.upErr != nil {
		return f.upErr
	}
	if f.up != nil {
		f.up(dir)
	}
	return nil
}

// fakeDocker holds containers per project and image tags.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string][]docker.Container
	details    map[string]docker.Detail
	tags       map[string]string // ref → image ID
	pruned     int
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{containers: map[string][]docker.Container{}, details: map[string]docker.Detail{}, tags: map[string]string{}}
}

func (f *fakeDocker) set(project string, service string, id string, image string, d docker.Detail) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := f.containers[project][:0:0]
	for _, c := range f.containers[project] {
		if c.Service != service {
			cs = append(cs, c)
		}
	}
	f.containers[project] = append(cs, docker.Container{ID: id, Service: service, State: d.State, Health: d.Health, ImageID: image})
	f.details[id] = d
}

func (f *fakeDocker) ProjectContainers(ctx context.Context, project string) ([]docker.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]docker.Container(nil), f.containers[project]...), nil
}
func (f *fakeDocker) Inspect(ctx context.Context, id string) (docker.Detail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.details[id], nil
}
func (f *fakeDocker) Tag(ctx context.Context, source string, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.tags[source]; ok {
		source = id
	}
	f.tags[target] = source
	return nil
}
func (f *fakeDocker) Tags(ctx context.Context, repository string, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var tags []string
	for ref := range f.tags {
		if name, tag, _ := strings.Cut(ref, ":"); name == repository && strings.HasPrefix(tag, prefix) {
			tags = append(tags, tag)
		}
	}
	return tags, nil
}
func (f *fakeDocker) Untag(ctx context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tags, ref)
	return nil
}
func (f *fakeDocker) PruneDangling(ctx context.Context) error { f.pruned++; return nil }
func (f *fakeDocker) PruneBuildCache(ctx context.Context, unused time.Duration) error {
	return nil
}

var (
	running = docker.Detail{State: "running", RestartPolicy: "unless-stopped"}
	healthy = docker.Detail{State: "running", Health: "healthy", RestartPolicy: "unless-stopped"}
)

type env struct {
	d       *Deployer
	root    string
	source  *fakeSource
	secrets *fakeSecrets
	compose *fakeCompose
	docker  *fakeDocker
	log     *bytes.Buffer
}

// newEnv is a project "website" with a built web service and a database,
// one secret and one plain setting; after Up, both run healthy.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		root:    t.TempDir(),
		source:  &fakeSource{archive: tarball(t, map[string]string{"compose.yaml": "services: {}", "start.sh": "#!/bin/sh\n"})},
		secrets: &fakeSecrets{values: map[string]string{"WEBSITE_DATABASE_URL": "postgres://app:hunter22@sparkdb/db"}},
		docker:  newFakeDocker(),
		log:     &bytes.Buffer{},
	}
	e.compose = &fakeCompose{
		name:     "website",
		services: []compose.Service{{Name: "db", Image: "postgres:16"}, {Name: "web", Build: true}},
		vars:     []compose.Variable{{Name: "WEBSITE_DATABASE_URL"}, {Name: "LOG_LEVEL", Default: "info"}},
		buildOut: "step 1/3: using postgres://app:hunter22@sparkdb/db\n",
	}
	e.compose.up = func(dir string) {
		e.docker.set("website", "web", "web-"+filepath.Base(dir), "img-"+filepath.Base(dir), healthy)
		e.docker.set("website", "db", "db-1", "postgres-img", running)
	}
	e.d = New(e.compose, e.docker, e.source, e.secrets, Options{
		Root: e.root, Log: e.log, Poll: time.Millisecond,
		Timeouts: Timeouts{Verify: 2 * time.Second, Stable: 20 * time.Millisecond},
	})
	return e
}

func (e *env) deploy(t *testing.T, p projects.Project, sha string) Result {
	t.Helper()
	return e.d.Deploy(context.Background(), Request{Project: p, SHA: sha, Token: "t"})
}

var landing = projects.Project{Name: "personalWebsite", Repo: github.Repo{Owner: "lsariol", Name: "Landing"}}

func stepStatuses(res Result) string {
	var parts []string
	for _, s := range res.Steps {
		parts = append(parts, s.Name+"="+s.Status)
	}
	return strings.Join(parts, " ")
}

func TestFirstDeploy(t *testing.T) {
	e := newEnv(t)
	claimed := ""
	res := e.d.Deploy(context.Background(), Request{Project: landing, SHA: sha1, Token: "t",
		Claim: func(ctx context.Context, cp string) error { claimed = cp; return nil }})

	if res.Status != projects.StatusSucceeded || res.Err != nil {
		t.Fatalf("Deploy = %+v", res)
	}
	if res.ComposeProject != "website" || claimed != "website" {
		t.Errorf("compose project %q, claimed %q", res.ComposeProject, claimed)
	}
	want := "fetch=succeeded inspect=succeeded secrets=succeeded build=succeeded swap=succeeded verify=succeeded cleanup=succeeded"
	if got := stepStatuses(res); got != want {
		t.Errorf("steps: %s", got)
	}

	// Unpacked into <root>/<compose project>/<commit>, without the top folder.
	dir := filepath.Join(e.root, "website", sha1[:12])
	if _, err := os.Stat(filepath.Join(dir, "compose.yaml")); err != nil {
		t.Errorf("files aren't in %s: %v", dir, err)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(filepath.Join(dir, "start.sh")); info == nil || info.Mode().Perm()&0o100 == 0 {
			t.Errorf("start.sh lost its executable bit")
		}
	}

	// Only the variable without a default is a secret.
	if !slices.Equal(e.secrets.asked, []string{"WEBSITE_DATABASE_URL"}) {
		t.Errorf("asked Cove for %v", e.secrets.asked)
	}
	if len(e.compose.upEnv) != 1 || !slices.Contains(e.compose.upEnv[0], "WEBSITE_DATABASE_URL=postgres://app:hunter22@sparkdb/db") {
		t.Errorf("Up's environment: %v", e.compose.upEnv)
	}

	// The secret is hidden in the stored output and in the log.
	build := res.Steps[3]
	if strings.Contains(build.Log, "hunter22") || !strings.Contains(build.Log, hidden) {
		t.Errorf("build log: %q", build.Log)
	}
	if strings.Contains(e.log.String(), "hunter22") || !strings.Contains(e.log.String(), "[personalWebsite build] ") {
		t.Errorf("log: %q", e.log.String())
	}
	if !strings.Contains(res.Steps[1].Log, "${LOG_LEVEL} has a default") {
		t.Errorf("inspect doesn't warn about the default: %q", res.Steps[1].Log)
	}

	// The built image is kept for rollback under its commit.
	if e.docker.tags["website-web:lh-"+sha1[:12]] == "" {
		t.Errorf("tags: %v", e.docker.tags)
	}
	if e.docker.pruned != 1 {
		t.Error("unused images weren't pruned")
	}
}

func TestNoComposeName(t *testing.T) {
	e := newEnv(t)
	e.compose.name = ""
	e.compose.up = func(dir string) {
		e.docker.set("landing", "web", "w", "i", healthy)
		e.docker.set("landing", "db", "d", "p", running)
	}

	res := e.deploy(t, landing, sha1)
	if res.Status != projects.StatusSucceeded || res.ComposeProject != "landing" {
		t.Fatalf("Deploy = %+v (want compose project landing, the repository's name)", res)
	}
	if _, err := os.Stat(filepath.Join(e.root, "landing", sha1[:12])); err != nil {
		t.Errorf("not unpacked under the repository's name: %v", err)
	}
}

func TestFailuresBeforeTheSwap(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env)
		step  string
		kind  string
		text  string
	}{
		{"GitHub down", func(e *env) { e.source.err = &github.Error{Status: 502, Err: errors.New("502 Bad Gateway")} },
			StepFetch, projects.FailureTransient, "502"},
		{"commit gone", func(e *env) { e.source.err = &github.Error{Status: 404, Err: errors.New("404 Not Found")} },
			StepFetch, projects.FailurePermanent, "404"},
		{"not a tarball", func(e *env) { e.source.archive = []byte("<html>") },
			StepFetch, projects.FailurePermanent, "gzipped"},
		{"compose project taken", nil, StepInspect, projects.FailurePermanent, "belongs to cove"},
		{"missing secret", func(e *env) {
			e.secrets.err = fmt.Errorf("coveClient: GetSecrets: %w: WEBSITE_DATABASE_URL", coveclient.ErrNotFound)
		},
			StepSecrets, projects.FailurePermanent, "create the keys in Cove"},
		{"Cove down", func(e *env) { e.secrets.err = errors.New("dial tcp: connection refused") },
			StepSecrets, projects.FailureTransient, "Cove"},
		{"build fails", func(e *env) {
			e.compose.buildErr = errors.New("docker compose build failed (exit status 1): no such file")
		},
			StepBuild, projects.FailurePermanent, "no such file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			if c.setup != nil {
				c.setup(e)
			}
			req := Request{Project: landing, SHA: sha1, Token: "t"}
			if c.step == StepInspect {
				req.Claim = func(ctx context.Context, cp string) error {
					return errors.New("compose project website belongs to cove")
				}
			}
			res := e.d.Deploy(context.Background(), req)

			if res.Status != projects.StatusFailed || res.FailedStep != c.step || res.FailureKind != c.kind ||
				res.Err == nil || !strings.Contains(res.Err.Error(), c.text) {
				t.Fatalf("Deploy = status %s, step %s, kind %s, err %v", res.Status, res.FailedStep, res.FailureKind, res.Err)
			}
			if len(e.compose.ups) != 0 {
				t.Error("the running version was replaced")
			}
			if res.Steps[len(res.Steps)-1].Status != projects.StepSkipped {
				t.Errorf("later steps aren't marked skipped: %s", stepStatuses(res))
			}
			// The failed deploy's files are gone.
			entries, _ := os.ReadDir(filepath.Join(e.root, "website"))
			incoming, _ := os.ReadDir(filepath.Join(e.root, ".incoming"))
			if len(entries)+len(incoming) != 0 {
				t.Errorf("left %d folders behind", len(entries)+len(incoming))
			}
		})
	}
}

// deployed runs a first successful deploy at sha1 and returns the project as
// the store would then have it.
func deployed(t *testing.T, e *env) projects.Project {
	t.Helper()
	if res := e.deploy(t, landing, sha1); res.Status != projects.StatusSucceeded {
		t.Fatalf("first deploy: %+v", res)
	}
	p := landing
	p.DeployedSHA, p.ComposeProject = sha1, "website"
	return p
}

func TestRollbackWhenUnhealthy(t *testing.T) {
	e := newEnv(t)
	p := deployed(t, e)
	oldImage := "img-" + sha1[:12]

	// The new version's web turns unhealthy; the old one comes back.
	e.compose.up = func(dir string) {
		if filepath.Base(dir) == sha2[:12] {
			e.docker.set("website", "web", "web-new", "img-new", docker.Detail{State: "running", Health: "unhealthy", RestartPolicy: "unless-stopped"})
		} else {
			e.docker.set("website", "web", "web-old", oldImage, healthy)
		}
	}

	res := e.deploy(t, p, sha2)
	if res.Status != projects.StatusRolledBack || res.FailedStep != StepVerify || res.FailureKind != projects.FailurePermanent {
		t.Fatalf("Deploy = %+v", res)
	}
	if !strings.Contains(res.Err.Error(), "web is unhealthy") || !strings.Contains(res.Err.Error(), "rolled back to 1111111") {
		t.Errorf("error: %v", res.Err)
	}
	if last := e.compose.ups[len(e.compose.ups)-1]; filepath.Base(last) != sha1[:12] {
		t.Errorf("the rollback ran in %s, want the previous version's folder", last)
	}
	if e.docker.tags["website-web"] != oldImage {
		t.Errorf("the previous image wasn't put back: %v", e.docker.tags)
	}
	if _, err := os.Stat(filepath.Join(e.root, "website", sha2[:12])); err == nil {
		t.Error("the failed version's folder is still there")
	}
	if got := stepStatuses(res); !strings.HasSuffix(got, "verify=failed cleanup=skipped rollback=succeeded") {
		t.Errorf("steps: %s", got)
	}
}

func TestNoRollbackOnFirstDeploy(t *testing.T) {
	e := newEnv(t)
	e.compose.up = func(dir string) {
		e.docker.set("website", "web", "w", "i", docker.Detail{State: "exited", ExitCode: 1, RestartPolicy: "unless-stopped"})
	}
	res := e.deploy(t, landing, sha1)
	if res.Status != projects.StatusFailed || !strings.Contains(res.Err.Error(), "nothing ran before") ||
		!strings.Contains(res.Err.Error(), "web stopped with exit code 1") {
		t.Errorf("Deploy = %+v", res)
	}
}

func TestVerifyRules(t *testing.T) {
	cases := []struct {
		name   string
		detail docker.Detail
		ok     bool
		text   string
	}{
		{"healthy", healthy, true, ""},
		{"running, no healthcheck, stays up", running, true, ""},
		{"a job that finished", docker.Detail{State: "exited", ExitCode: 0, RestartPolicy: "no"}, true, ""},
		{"a job that failed", docker.Detail{State: "exited", ExitCode: 2, RestartPolicy: "no"}, false, "exit code 2"},
		{"a service that exited", docker.Detail{State: "exited", ExitCode: 0, RestartPolicy: "always"}, false, "exit code 0"},
		{"restarting", docker.Detail{State: "restarting", RestartPolicy: "always", ExitCode: 1}, false, "keeps restarting"},
		{"never healthy", docker.Detail{State: "running", Health: "starting", RestartPolicy: "always"}, false, "health check starting"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.compose.services = []compose.Service{{Name: "web", Build: true}}
			e.compose.up = func(dir string) { e.docker.set("website", "web", "w", "i", c.detail) }
			e.d.timeouts.Verify = 100 * time.Millisecond

			res := e.deploy(t, landing, sha1)
			if c.ok != (res.Status == projects.StatusSucceeded) {
				t.Fatalf("Deploy = %s, %v", res.Status, res.Err)
			}
			if !c.ok && !strings.Contains(res.Err.Error(), c.text) {
				t.Errorf("error %v, want %q", res.Err, c.text)
			}
		})
	}
}

func TestCleanupKeepsCurrentAndPrevious(t *testing.T) {
	e := newEnv(t)
	p := deployed(t, e)
	e.docker.tags["website-web:lh-000000000000"] = "ancient"
	os.MkdirAll(filepath.Join(e.root, "website", "000000000000"), 0o755)

	if res := e.deploy(t, p, sha2); res.Status != projects.StatusSucceeded {
		t.Fatalf("Deploy = %+v", res)
	}
	entries, _ := os.ReadDir(filepath.Join(e.root, "website"))
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	if !slices.Equal(names, []string{sha1[:12], sha2[:12]}) {
		t.Errorf("deploy folders = %v, want the previous and the new one", names)
	}
	if _, ok := e.docker.tags["website-web:lh-000000000000"]; ok {
		t.Error("an old rollback tag was kept")
	}
	if e.docker.tags["website-web:lh-"+sha1[:12]] == "" || e.docker.tags["website-web:lh-"+sha2[:12]] == "" {
		t.Errorf("tags = %v", e.docker.tags)
	}
}

func TestRedeploySameCommit(t *testing.T) {
	e := newEnv(t)
	p := deployed(t, e)
	if res := e.deploy(t, p, sha1); res.Status != projects.StatusSucceeded {
		t.Fatalf("redeploy = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(e.root, "website", sha1[:12], "compose.yaml")); err != nil {
		t.Errorf("the running version's folder is gone: %v", err)
	}
}

func TestExtractRefusesEscapes(t *testing.T) {
	write := func(entries ...tar.Header) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, h := range entries {
			tw.WriteHeader(&h)
		}
		tw.Close()
		gz.Close()
		return buf.Bytes()
	}
	cases := map[string][]byte{
		"path":          write(tar.Header{Typeflag: tar.TypeReg, Name: "top/../../escaped.txt"}),
		"absolute link": write(tar.Header{Typeflag: tar.TypeSymlink, Name: "top/etc", Linkname: "/etc"}),
		"link outside":  write(tar.Header{Typeflag: tar.TypeSymlink, Name: "top/up", Linkname: "../../.."}),
	}
	for name, archive := range cases {
		dest := filepath.Join(t.TempDir(), "dest")
		if err := extract(bytes.NewReader(archive), dest); err == nil {
			t.Errorf("%s: an escaping entry was accepted", name)
		}
	}
}

func TestScrubber(t *testing.T) {
	var out bytes.Buffer
	s := newScrubber(map[string]string{"a": "hunter22", "b": "abc", "c": "hunter22-long"}, &out)
	io.WriteString(s, "password hun")
	io.WriteString(s, "ter22 and hunter22-long; abc stays\npartial hunter22")
	if strings.Contains(out.String(), "partial") {
		t.Error("a partial line was written before its end")
	}
	s.Flush()
	got := out.String()
	want := "password [secret] and [secret]; abc stays\npartial [secret]"
	if got != want {
		t.Errorf("scrubbed:\n%q\nwant\n%q", got, want)
	}
}

func TestRepository(t *testing.T) {
	for ref, want := range map[string]string{
		"website-web":              "website-web",
		"website-web:latest":       "website-web",
		"registry:5000/app":        "registry:5000/app",
		"registry:5000/app:1.2":    "registry:5000/app",
		"ghcr.io/lsariol/plop:abc": "ghcr.io/lsariol/plop",
	} {
		if got := repository(ref); got != want {
			t.Errorf("repository(%q) = %q, want %q", ref, got, want)
		}
	}
	if normalizeName("My.Site_2") != "mysite_2" {
		t.Errorf("normalizeName = %q", normalizeName("My.Site_2"))
	}
}
