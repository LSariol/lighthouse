package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Input is what Check needs about one deploy.
type Input struct {
	Project string // the compose project
	// Config is `docker compose config --no-interpolate --format json`:
	// paths are absolute, and ${...} are left as they are.
	Config []byte
	Dir    string // the deploy folder: the repository's files
	// Storage is the root of the projects' data folders (/srv/server/storage):
	// a project may use Storage/<compose project>/ and nothing else on the host.
	Storage string
	Secrets []string // the ${KEY}s fetched from Cove
	// Taken holds the names already answered on the shared network by other
	// projects' containers: name → the container that has it.
	Taken map[string]string
	// Resolve follows symlinks in a host path; ResolvePath if nil. A symlink
	// in the repository, or one a container left in its data folder, mustn't
	// lead a mount outside the allowed folders.
	Resolve func(path string) string
}

// readOnlyHostFiles may be mounted read-only by anyone: they're harmless, and
// common (a container's time zone).
var readOnlyHostFiles = map[string]bool{"/etc/localtime": true, "/etc/timezone": true}

// Check returns everything in the compose file the rules refuse or warn
// about. Pass the findings to Policy.Apply for the ones that stop a deploy.
func Check(in Input) ([]Finding, error) {
	var cfg config
	if err := json.Unmarshal(in.Config, &cfg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if in.Resolve == nil {
		in.Resolve = ResolvePath
	}
	c := &checker{in: in, cfg: cfg}
	if in.Dir != "" {
		c.roots = append(c.roots, filepath.Clean(in.Dir))
	}
	if in.Storage != "" {
		c.storage = filepath.Join(in.Storage, in.Project)
		c.roots = append(c.roots, c.storage)
	}
	for _, r := range c.roots {
		c.resolvedRoots = append(c.resolvedRoots, in.Resolve(r))
	}

	names := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c.service(name, cfg.Services[name])
	}
	c.project()
	c.secrets()
	return c.findings, nil
}

// config is the part of a compose config the rules read.
type config struct {
	Services map[string]service `json:"services"`
	Volumes  map[string]*struct {
		Name       string            `json:"name"`
		External   bool              `json:"external"`
		DriverOpts map[string]string `json:"driver_opts"`
	} `json:"volumes"`
	Networks map[string]*struct {
		Name     string `json:"name"`
		External bool   `json:"external"`
	} `json:"networks"`
	Secrets map[string]*fileSource `json:"secrets"`
	Configs map[string]*fileSource `json:"configs"`
}

type fileSource struct {
	File string `json:"file"`
}

type service struct {
	Privileged        bool       `json:"privileged"`
	NetworkMode       string     `json:"network_mode"`
	Pid               string     `json:"pid"`
	Ipc               string     `json:"ipc"`
	Uts               string     `json:"uts"`
	UsernsMode        string     `json:"userns_mode"`
	Cgroup            string     `json:"cgroup"`
	CapAdd            []string   `json:"cap_add"`
	Devices           []pathItem `json:"devices"`
	DeviceCgroupRules []string   `json:"device_cgroup_rules"`
	SecurityOpt       []string   `json:"security_opt"`
	VolumesFrom       []string   `json:"volumes_from"`
	Volumes           []mount    `json:"volumes"`
	EnvFile           []pathItem `json:"env_file"`
	Build             *build     `json:"build"`
	ContainerName     string     `json:"container_name"`
	Networks          map[string]*struct {
		Aliases []string `json:"aliases"`
	} `json:"networks"`
	Ports []struct {
		HostIP    string     `json:"host_ip"`
		Published flexString `json:"published"`
		Target    flexString `json:"target"`
	} `json:"ports"`
}

type mount struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type build struct {
	Context            string            `json:"context"`
	Dockerfile         string            `json:"dockerfile"`
	AdditionalContexts map[string]string `json:"additional_contexts"`
	Privileged         bool              `json:"privileged"`
	Network            string            `json:"network"`
	Entitlements       []string          `json:"entitlements"`
}

// pathItem is a path written either as a string or as an object with a
// source or path (Compose versions differ).
type pathItem string

func (p *pathItem) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		// The short syntax "host:container[:perms]" names the host side first.
		if i := strings.Index(s, ":"); i > 1 { // not a Windows drive letter
			s = s[:i]
		}
		*p = pathItem(s)
		return nil
	}
	var o struct{ Source, Path string }
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	*p = pathItem(o.Source + o.Path)
	return nil
}

// flexString accepts a JSON string or number.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*f = flexString(s)
		return nil
	}
	*f = flexString(strings.TrimSpace(string(b)))
	return nil
}

type checker struct {
	in      Input
	cfg     config
	roots   []string // where the project's host paths may be: its files and its data folder
	storage string
	// resolvedRoots are roots with symlinks followed, to compare resolved
	// paths with.
	resolvedRoots []string
	findings      []Finding
}

func (c *checker) add(service string, id string, format string, args ...any) {
	c.findings = append(c.findings, Finding{ID: id, Service: service, Problem: fmt.Sprintf(format, args...)})
}

func (c *checker) warn(service string, format string, args ...any) {
	c.findings = append(c.findings, Finding{Service: service, Problem: fmt.Sprintf(format, args...), Warning: true})
}

func (c *checker) service(name string, s service) {
	if s.Privileged {
		c.add(name, "privileged", "privileged: true gives the container root on the host")
	}

	c.namespace(name, "network_mode", s.NetworkMode)
	c.namespace(name, "pid", s.Pid)
	c.namespace(name, "ipc", s.Ipc)
	c.namespace(name, "uts", s.Uts)
	c.namespace(name, "userns_mode", s.UsernsMode)
	c.namespace(name, "cgroup", s.Cgroup)

	for _, capability := range s.CapAdd {
		capability = strings.TrimPrefix(strings.ToUpper(capability), "CAP_")
		c.add(name, "cap_add:"+capability, "cap_add: %s gives the container a privilege containers don't have", capability)
	}
	for _, d := range s.Devices {
		c.add(name, "device:"+string(d), "devices: %s gives the container a host device", d)
	}
	for _, r := range s.DeviceCgroupRules {
		c.add(name, "device_cgroup_rule:"+r, "device_cgroup_rules: %q opens host devices to the container", r)
	}
	for _, opt := range s.SecurityOpt {
		o := strings.ToLower(opt)
		if strings.Contains(o, "unconfined") || strings.Contains(o, "label=disable") || strings.Contains(o, "label:disable") {
			c.add(name, "security_opt:"+opt, "security_opt: %s turns off a protection", opt)
		}
	}
	for _, from := range s.VolumesFrom {
		if strings.HasPrefix(from, "container:") {
			c.add(name, "volumes_from:"+from, "volumes_from: %s mounts another container's data", from)
		}
	}

	for _, m := range s.Volumes {
		switch m.Type {
		case "bind":
			c.hostMount(name, m.Source, m.ReadOnly)
		case "volume":
			// A named volume is checked with the project's volumes; an
			// anonymous one belongs to the container.
		}
	}
	for _, f := range s.EnvFile {
		c.file(name, "env_file", string(f))
	}

	if b := s.Build; b != nil {
		c.buildContext(name, "build context", b.Context)
		if b.Dockerfile != "" && isLocal(b.Context) {
			path := b.Dockerfile
			if !filepath.IsAbs(path) && !strings.HasPrefix(path, "/") {
				path = filepath.Join(b.Context, path)
			}
			c.file(name, "dockerfile", path)
		}
		for key, ctx := range b.AdditionalContexts {
			c.buildContext(name, "additional context "+key, ctx)
		}
		if b.Privileged {
			c.add(name, "build:privileged", "build: privileged: true gives the build root on the host")
		}
		if b.Network == "host" {
			c.add(name, "build:network:host", "build: network: host lets the build reach the host's services")
		}
		for _, e := range b.Entitlements {
			c.add(name, "build:entitlement:"+e, "build: entitlements: %s gives the build a privilege", e)
		}
	}

	for _, p := range s.Ports {
		if p.HostIP == "" || p.HostIP == "0.0.0.0" || p.HostIP == "::" {
			c.warn(name, "publishes port %s on every interface, so anything on the local network can reach it directly, around Cloudflare. If only other containers (cloudflared) need it, join %s and reach it by name, without a port",
				p.Published, SharedNetwork)
		}
	}

	c.names(name, s)
}

// namespace refuses sharing the host's namespace, or another container's.
func (c *checker) namespace(service string, field string, value string) {
	switch {
	case value == "host":
		c.add(service, field+":host", "%s: host shares the host's namespace with the container", field)
	case strings.HasPrefix(value, "container:"):
		c.add(service, field+":"+value, "%s: %s shares another container's namespace", field, value)
	}
}

// hostMount allows a bind mount from the repository's files or the project's
// data folder.
func (c *checker) hostMount(service string, source string, readOnly bool) {
	ok, link := c.inside(source)
	path := filepath.Clean(source)
	switch {
	case ok:
		return
	case link != "":
		c.add(service, "link:"+path, "mounts %s, a symlink to %s: outside the project's folders", path, link)
		return
	case readOnly && readOnlyHostFiles[filepath.ToSlash(path)]:
		return
	}
	switch {
	case strings.HasSuffix(filepath.ToSlash(path), "/docker.sock"):
		c.add(service, "mount:"+path, "mounts the Docker socket, which controls every container on the server (root on the host)")
	default:
		c.add(service, "mount:"+path, "mounts %s from the host; data belongs in %s", path, c.storageHint())
	}
}

// file allows a file the compose file reads from the repository or the
// project's data folder.
func (c *checker) file(service string, what string, source string) {
	ok, link := c.inside(source)
	path := filepath.Clean(source)
	switch {
	case ok:
	case link != "":
		c.add(service, "link:"+path, "%s %s is a symlink to %s: outside the project's folders", what, path, link)
	default:
		c.add(service, "file:"+path, "%s %s is outside the repository and the project's data folder", what, path)
	}
}

// buildContext allows a local build context inside the repository; a remote
// one (a Git URL) is fetched by the build and can't reach the host.
func (c *checker) buildContext(service string, what string, ctx string) {
	if isLocal(ctx) {
		c.file(service, what, ctx)
	}
}

// isLocal reports whether a build context is a folder on this machine.
func isLocal(ctx string) bool {
	return ctx != "" && !strings.Contains(ctx, "://") && !strings.HasPrefix(ctx, "git@") &&
		!strings.HasPrefix(ctx, "service:") && !strings.HasPrefix(ctx, "target:")
}

func (c *checker) storageHint() string {
	if c.storage == "" {
		return "the project's data folder"
	}
	return filepath.ToSlash(c.storage) + "/"
}

// inside reports whether a host path is in one of the project's own folders.
// Those are where a repository or a container could leave a symlink, so a
// path inside them is followed; when it leads out, link is where to.
// Elsewhere the path is taken as written, so an exception can name it.
func (c *checker) inside(path string) (ok bool, link string) {
	if path == "" {
		return true, ""
	}
	clean := filepath.Clean(path)
	named := false
	for _, root := range c.roots {
		if within(root, clean) {
			named = true
		}
	}
	if !named {
		return false, ""
	}
	resolved := c.in.Resolve(clean)
	for _, root := range c.resolvedRoots {
		if within(root, resolved) {
			return true, ""
		}
	}
	return false, resolved
}

func within(root string, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// project checks what the services share: volumes, networks, and the files
// behind secrets and configs.
func (c *checker) project() {
	own := c.in.Project + "_"

	keys := sortedKeys(c.cfg.Volumes)
	for _, key := range keys {
		v := c.cfg.Volumes[key]
		if v == nil {
			continue
		}
		name := v.Name
		if name == "" {
			name = own + key
		}
		if !strings.HasPrefix(name, own) {
			c.add("", "volume:"+name, "volume %q isn't this project's (its volumes are named %s…); it may hold another project's data", name, own)
		}
		if dev := v.DriverOpts["device"]; dev != "" && strings.Contains(v.DriverOpts["o"], "bind") {
			c.hostMount("", dev, false)
		}
	}

	for _, key := range sortedKeys(c.cfg.Networks) {
		n := c.cfg.Networks[key]
		name := own + key
		if n != nil && n.Name != "" {
			name = n.Name
		}
		if name != SharedNetwork && !strings.HasPrefix(name, own) {
			c.add("", "network:"+name, "network %q isn't this project's or %q; it reaches into another project", name, SharedNetwork)
		}
	}

	for _, key := range sortedKeys(c.cfg.Secrets) {
		if s := c.cfg.Secrets[key]; s != nil && s.File != "" {
			c.file("", "secret "+key, s.File)
		}
	}
	for _, key := range sortedKeys(c.cfg.Configs) {
		if s := c.cfg.Configs[key]; s != nil && s.File != "" {
			c.file("", "config "+key, s.File)
		}
	}
}

// secrets allows the project's own Cove keys and the shared ones.
func (c *checker) secrets() {
	prefix := SecretPrefix(c.in.Project)
	for _, key := range c.in.Secrets {
		if !strings.HasPrefix(key, prefix) && !strings.HasPrefix(key, SharedPrefix) {
			c.add("", "secret:"+key, "${%s} isn't this project's: it may use %s* and %s* keys", key, prefix, SharedPrefix)
		}
	}
}

// SecretPrefix is the start of a compose project's own Cove keys: "website"
// has WEBSITE_*, "my-site" MY_SITE_*.
func SecretPrefix(project string) string {
	return strings.ToUpper(strings.ReplaceAll(project, "-", "_")) + "_"
}

// names refuses names on the shared network that another project's
// container already answers to: Docker's DNS would send requests to either.
func (c *checker) names(service string, s service) {
	for netKey, settings := range s.Networks {
		if !c.isShared(netKey) {
			continue
		}
		// Compose adds the service's name to every network it joins.
		names := []string{service}
		if s.ContainerName != "" {
			names = append(names, s.ContainerName)
		}
		if settings != nil {
			names = append(names, settings.Aliases...)
		}
		seen := map[string]bool{}
		for _, n := range names {
			if seen[n] {
				continue
			}
			seen[n] = true
			if other, ok := c.in.Taken[n]; ok {
				c.add(service, "name:"+n, "%q on %s is taken by container %s, so requests for it would reach either; rename the service, or its container_name or alias",
					n, SharedNetwork, other)
			}
		}
	}
}

func (c *checker) isShared(key string) bool {
	n := c.cfg.Networks[key]
	if n != nil && n.Name != "" {
		return n.Name == SharedNetwork
	}
	return key == SharedNetwork && n != nil && n.External
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ResolvePath follows symlinks in path as far as it exists; the rest (a
// folder Docker will create) is joined on unchanged.
func ResolvePath(path string) string {
	rest := ""
	for p := path; ; p = filepath.Dir(p) {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(resolved, rest)
		} else if _, statErr := os.Lstat(p); statErr == nil {
			// It exists but can't be resolved (a broken or looping link):
			// don't trust it.
			return path + string(filepath.Separator) + "(unresolvable link)"
		}
		parent := filepath.Dir(p)
		if parent == p {
			return path
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}
