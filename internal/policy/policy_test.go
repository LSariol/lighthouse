package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// cfg builds a compose config with one service, "web", on spark.
func cfg(t *testing.T, web map[string]any, top map[string]any) []byte {
	t.Helper()
	svc := map[string]any{"image": "nginx", "networks": map[string]any{"spark": nil}}
	for k, v := range web {
		svc[k] = v
	}
	c := map[string]any{
		"name":     "site",
		"services": map[string]any{"web": svc},
		"networks": map[string]any{"spark": map[string]any{"name": "spark", "external": true}},
	}
	for k, v := range top {
		c[k] = v
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type setup struct {
	dir, storage string
}

func newSetup(t *testing.T) setup {
	root := t.TempDir()
	s := setup{dir: filepath.Join(root, "staging", "site", "abc"), storage: filepath.Join(root, "storage")}
	os.MkdirAll(s.dir, 0o755)
	os.MkdirAll(filepath.Join(s.storage, "site"), 0o755)
	return s
}

func (s setup) check(t *testing.T, config []byte, secrets ...string) []Finding {
	t.Helper()
	f, err := Check(Input{Project: "site", Config: config, Dir: s.dir, Storage: s.storage, Secrets: secrets,
		Taken: map[string]string{"db": "sparkdb", "cove": "cove"}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func ids(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		if !f.Warning {
			out = append(out, f.ID)
		}
	}
	slices.Sort(out)
	return out
}

func TestCleanProjectPasses(t *testing.T) {
	s := newSetup(t)
	config := cfg(t, map[string]any{
		"build":        map[string]any{"context": s.dir, "dockerfile": "Dockerfile"},
		"cap_drop":     []string{"ALL"},
		"read_only":    true,
		"security_opt": []string{"no-new-privileges:true"},
		"volumes": []any{
			map[string]any{"type": "bind", "source": filepath.Join(s.storage, "site", "data"), "target": "/data"},
			map[string]any{"type": "bind", "source": filepath.Join(s.dir, "config.yml"), "target": "/app/config.yml"},
			map[string]any{"type": "bind", "source": "/etc/localtime", "target": "/etc/localtime", "read_only": true},
			map[string]any{"type": "volume", "source": "cache", "target": "/cache"},
			map[string]any{"type": "tmpfs", "target": "/tmp"},
		},
		"env_file":       []any{map[string]any{"path": filepath.Join(s.dir, ".env.app")}},
		"container_name": "site-web",
		"networks":       map[string]any{"spark": map[string]any{"aliases": []string{"site"}}, "default": nil},
		"ports":          []any{map[string]any{"host_ip": "127.0.0.1", "published": "8080", "target": 80}},
	}, map[string]any{
		"volumes":  map[string]any{"cache": map[string]any{"name": "site_cache"}},
		"networks": map[string]any{"spark": map[string]any{"name": "spark", "external": true}, "default": map[string]any{"name": "site_default"}},
		"secrets":  map[string]any{"key": map[string]any{"file": filepath.Join(s.dir, "key.txt")}},
	})
	if f := s.check(t, config, "SITE_DATABASE_URL", "SHARED_TMDB_API_KEY"); len(f) != 0 {
		t.Errorf("a clean project has findings: %v", f)
	}
}

func TestEachRuleRefuses(t *testing.T) {
	s := newSetup(t)
	cases := []struct {
		web  map[string]any
		top  map[string]any
		want string
	}{
		{map[string]any{"privileged": true}, nil, "privileged"},
		{map[string]any{"network_mode": "host"}, nil, "network_mode:host"},
		{map[string]any{"network_mode": "container:cove"}, nil, "network_mode:container:cove"},
		{map[string]any{"pid": "host"}, nil, "pid:host"},
		{map[string]any{"ipc": "host"}, nil, "ipc:host"},
		{map[string]any{"uts": "host"}, nil, "uts:host"},
		{map[string]any{"userns_mode": "host"}, nil, "userns_mode:host"},
		{map[string]any{"cgroup": "host"}, nil, "cgroup:host"},
		{map[string]any{"cap_add": []string{"cap_sys_admin"}}, nil, "cap_add:SYS_ADMIN"},
		{map[string]any{"devices": []any{map[string]any{"source": "/dev/sda", "target": "/dev/sda"}}}, nil, "device:/dev/sda"},
		{map[string]any{"devices": []string{"/dev/ttyUSB0:/dev/ttyUSB0"}}, nil, "device:/dev/ttyUSB0"},
		{map[string]any{"device_cgroup_rules": []string{"c 1:3 mr"}}, nil, "device_cgroup_rule:c 1:3 mr"},
		{map[string]any{"security_opt": []string{"seccomp=unconfined"}}, nil, "security_opt:seccomp=unconfined"},
		{map[string]any{"security_opt": []string{"label:disable"}}, nil, "security_opt:label:disable"},
		{map[string]any{"volumes_from": []string{"container:cove"}}, nil, "volumes_from:container:cove"},
		{map[string]any{"volumes": []any{map[string]any{"type": "bind", "source": "/var/run/docker.sock", "target": "/var/run/docker.sock"}}}, nil,
			"mount:" + filepath.ToSlash(filepath.Clean("/var/run/docker.sock"))},
		{map[string]any{"volumes": []any{map[string]any{"type": "bind", "source": "/", "target": "/host", "read_only": true}}}, nil, "mount:" + filepath.ToSlash(filepath.Clean("/"))},
		{map[string]any{"volumes": []any{map[string]any{"type": "bind", "source": filepath.Join(s.storage, "cove", ".env"), "target": "/x"}}}, nil,
			"mount:" + filepath.ToSlash(filepath.Join(s.storage, "cove", ".env"))},
		{map[string]any{"volumes": []any{map[string]any{"type": "bind", "source": filepath.Join(s.storage, "site-other"), "target": "/x"}}}, nil,
			"mount:" + filepath.ToSlash(filepath.Join(s.storage, "site-other"))},
		{map[string]any{"volumes": []any{map[string]any{"type": "bind", "source": filepath.Join(s.dir, "..", "..", "cove"), "target": "/x"}}}, nil,
			"mount:" + filepath.ToSlash(filepath.Clean(filepath.Join(s.dir, "..", "..", "cove")))},
		{map[string]any{"volumes": []any{map[string]any{"type": "bind", "source": "/etc/localtime", "target": "/etc/localtime"}}}, nil, "mount:" + filepath.ToSlash(filepath.Clean("/etc/localtime"))},
		{map[string]any{"env_file": []any{map[string]any{"path": "/app/vault/cove/token"}}}, nil, "file:" + filepath.ToSlash(filepath.Clean("/app/vault/cove/token"))},
		{map[string]any{"build": map[string]any{"context": "/"}}, nil, "file:" + filepath.ToSlash(filepath.Clean("/"))},
		{map[string]any{"build": map[string]any{"context": s.dir, "dockerfile": "/etc/shadow"}}, nil, "file:" + filepath.ToSlash(filepath.Clean("/etc/shadow"))},
		{map[string]any{"build": map[string]any{"context": s.dir, "additional_contexts": map[string]string{"x": "/etc"}}}, nil, "file:" + filepath.ToSlash(filepath.Clean("/etc"))},
		{map[string]any{"build": map[string]any{"context": s.dir, "privileged": true}}, nil, "build:privileged"},
		{map[string]any{"build": map[string]any{"context": s.dir, "network": "host"}}, nil, "build:network:host"},
		{map[string]any{"build": map[string]any{"context": s.dir, "entitlements": []string{"security.insecure"}}}, nil, "build:entitlement:security.insecure"},
		{nil, map[string]any{"volumes": map[string]any{"data": map[string]any{"name": "cove_data", "external": true}}}, "volume:cove_data"},
		{nil, map[string]any{"volumes": map[string]any{"etc": map[string]any{"name": "site_etc", "driver_opts": map[string]string{"type": "none", "o": "bind", "device": "/etc"}}}},
			"mount:" + filepath.ToSlash(filepath.Clean("/etc"))},
		{nil, map[string]any{"networks": map[string]any{"spark": map[string]any{"name": "spark", "external": true}, "w": map[string]any{"name": "website_default", "external": true}}},
			"network:website_default"},
		{nil, map[string]any{"secrets": map[string]any{"s": map[string]any{"file": "/etc/shadow"}}}, "file:" + filepath.ToSlash(filepath.Clean("/etc/shadow"))},
		{nil, map[string]any{"configs": map[string]any{"c": map[string]any{"file": "/etc/passwd"}}}, "file:" + filepath.ToSlash(filepath.Clean("/etc/passwd"))},
		{map[string]any{"container_name": "cove"}, nil, "name:cove"},
		{map[string]any{"networks": map[string]any{"spark": map[string]any{"aliases": []string{"db"}}}}, nil, "name:db"},
	}
	for i, c := range cases {
		got := ids(s.check(t, cfg(t, c.web, c.top)))
		if !slices.Equal(got, []string{c.want}) {
			t.Errorf("case %d: findings %v, want [%s]", i, got, c.want)
		}
	}
}

func TestServiceNameOnSpark(t *testing.T) {
	// Compose adds a service's name to its networks: a service called "db"
	// on spark answers to the name sparkdb's service has.
	s := newSetup(t)
	c := []byte(`{"name":"site","services":{"db":{"image":"postgres","networks":{"spark":null}},"cove":{"image":"x"}},
		"networks":{"spark":{"name":"spark","external":true},"default":{"name":"site_default"}}}`)
	if got := ids(s.check(t, c)); !slices.Equal(got, []string{"name:db"}) {
		t.Errorf("findings %v, want [name:db] (cove isn't on spark)", got)
	}
}

func TestSecretPrefix(t *testing.T) {
	s := newSetup(t)
	got := ids(s.check(t, cfg(t, nil, nil), "SITE_DATABASE_URL", "SHARED_X_API_KEY", "SPARK_DATABASE_ADMIN_PASSWORD", "SITEX_TOKEN"))
	want := []string{"secret:SITEX_TOKEN", "secret:SPARK_DATABASE_ADMIN_PASSWORD"}
	if !slices.Equal(got, want) {
		t.Errorf("findings %v, want %v", got, want)
	}
	if p := SecretPrefix("my-site"); p != "MY_SITE_" {
		t.Errorf("SecretPrefix(my-site) = %q", p)
	}
}

func TestPortsWarnOnly(t *testing.T) {
	s := newSetup(t)
	f := s.check(t, cfg(t, map[string]any{"ports": []any{map[string]any{"published": "2400", "target": 2400}}}, nil))
	if len(f) != 1 || !f[0].Warning || !strings.Contains(f[0].Problem, "join spark") {
		t.Errorf("findings %+v, want one warning suggesting spark", f)
	}
	if refused := (Policy{}).Apply("site", f); len(refused) != 0 {
		t.Error("a warning stopped the deploy")
	}
}

func TestSymlinkOutOfOwnFolders(t *testing.T) {
	s := newSetup(t)
	// A link the repository (or a container, in its data folder) left behind.
	resolve := func(p string) string {
		if p == filepath.Join(s.dir, "data") || p == filepath.Join(s.storage, "site", "evil") {
			return filepath.Clean("/")
		}
		return p
	}
	for _, src := range []string{filepath.Join(s.dir, "data"), filepath.Join(s.storage, "site", "evil")} {
		f, err := Check(Input{Project: "site", Dir: s.dir, Storage: s.storage, Resolve: resolve,
			Config: cfg(t, map[string]any{"volumes": []any{map[string]any{"type": "bind", "source": src, "target": "/x"}}}, nil)})
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(f); !slices.Equal(got, []string{"link:" + filepath.ToSlash(src)}) {
			t.Errorf("%s: findings %v, want [link:%s]", src, got, filepath.ToSlash(src))
		}
	}
}

func TestRealSymlink(t *testing.T) {
	s := newSetup(t)
	outside := t.TempDir()
	link := filepath.Join(s.dir, "data")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("can't create symlinks here: %v", err)
	}
	got := ids(s.check(t, cfg(t, map[string]any{"volumes": []any{
		map[string]any{"type": "bind", "source": link, "target": "/x"},
		map[string]any{"type": "bind", "source": filepath.Join(link, "sub", "new"), "target": "/y"},
	}}, nil)))
	want := []string{"link:" + filepath.ToSlash(link), "link:" + filepath.ToSlash(filepath.Join(link, "sub", "new"))}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("findings %v, want %v", got, want)
	}
}

func TestExceptions(t *testing.T) {
	p, err := Parse([]byte(`{"exceptions": [
		{"project": "sonar", "allow": ["mount:/var/run/docker.sock"], "reason": "reads container stats"},
		{"project": "media", "allow": ["mount:/srv/media/*"], "reason": "the library"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	fs := []Finding{{ID: "mount:/var/run/docker.sock"}, {ID: "privileged"}}
	refused := p.Apply("sonar", fs)
	if len(refused) != 1 || refused[0].ID != "privileged" {
		t.Errorf("refused %v, want only privileged", refused)
	}
	if fs[0].Allowed != "reads container stats" || !strings.Contains(fs[0].String(), "allowed by policy.json") {
		t.Errorf("allowed finding: %+v / %s", fs[0], fs[0].String())
	}
	if refused := p.Apply("other", []Finding{{ID: "mount:/var/run/docker.sock"}}); len(refused) != 1 {
		t.Error("an exception applied to another project")
	}
	if refused := p.Apply("media", []Finding{{ID: "mount:/srv/media/films"}, {ID: "mount:/srv/mediax"}}); len(refused) != 1 || refused[0].ID != "mount:/srv/mediax" {
		t.Errorf("wildcard: refused %v", refused)
	}
}

func TestParseRefusesSloppyPolicies(t *testing.T) {
	for _, bad := range []string{
		`{"exceptions": [{"project": "x", "allow": ["privileged"]}]}`,       // no reason
		`{"exceptions": [{"project": "x", "allow": [], "reason": "r"}]}`,    // nothing allowed
		`{"exceptions": [{"project": "x", "allow": ["*"], "reason": "r"}]}`, // everything
		`{"exceptions": [{"project": "Bad Name", "allow": ["privileged"], "reason": "r"}]}`,
		`{"exceptions": [{"project": "x", "alow": ["privileged"], "reason": "r"}]}`, // typo
		`{"exception": []}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse accepted %s", bad)
		}
	}
	if _, err := Parse([]byte(`{"exceptions": []}`)); err != nil {
		t.Errorf("an empty policy: %v", err)
	}
}

func TestSummary(t *testing.T) {
	s := Summary([]Finding{{ID: "privileged"}, {ID: "cap_add:NET_ADMIN"}})
	if !strings.Contains(s, "cap_add:NET_ADMIN, privileged") || !strings.Contains(s, "policy.json") {
		t.Errorf("Summary = %q", s)
	}
}
