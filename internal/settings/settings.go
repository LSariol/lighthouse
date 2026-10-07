// Package settings reads a project's x-lighthouse block (DOCUMENTATION.md §7.2).
package settings

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Settings are a project's x-lighthouse settings.
type Settings struct {
	Deploy string
	Tier   string
	Backup string
}

const (
	DeployBranch   = "branch"
	DeployReleases = "releases"

	TierData  = "data"
	TierInfra = "infra"
	TierApp   = "app"

	BackupPostgres = "postgres"
)

// Key is the compose file's top-level key for the settings.
const Key = "x-lighthouse"

// Default is a project without an x-lighthouse block.
var Default = Settings{Deploy: DeployBranch, Tier: TierApp}

var allowed = map[string][]string{
	"deploy": {DeployBranch, DeployReleases},
	"tier":   {TierData, TierInfra, TierApp},
	"backup": {BackupPostgres},
}

// Order is the tier's place in line: data first, then infra, then apps.
func (s Settings) Order() int {
	switch s.Tier {
	case TierData:
		return 0
	case TierInfra:
		return 1
	default:
		return 2
	}
}

// Releases reports whether the project deploys only version tags.
func (s Settings) Releases() bool { return s.Deploy == DeployReleases }

// fromMap checks each setting and fills in the defaults.
func fromMap(m map[string]string) (Settings, error) {
	s := Default
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		values, ok := allowed[k]
		if !ok {
			return Settings{}, fmt.Errorf("%s: unknown setting %q (there are deploy, tier and backup)", Key, k)
		}
		known := false
		for _, a := range values {
			if v == a {
				known = true
			}
		}
		if !known {
			return Settings{}, fmt.Errorf("%s: %s can be %s, not %q", Key, k, strings.Join(values, " or "), v)
		}
		switch k {
		case "deploy":
			s.Deploy = v
		case "tier":
			s.Tier = v
		case "backup":
			s.Backup = v
		}
	}
	return s, nil
}

// FromConfig reads the settings from `docker compose config --format json`.
func FromConfig(config []byte) (Settings, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(config, &raw); err != nil {
		return Settings{}, fmt.Errorf("compose config: %v", err)
	}
	block, ok := raw[Key]
	if !ok || string(block) == "null" {
		return Default, nil
	}
	var values map[string]any
	if err := json.Unmarshal(block, &values); err != nil {
		return Settings{}, fmt.Errorf("%s must be a mapping of settings", Key)
	}
	m := map[string]string{}
	for k, v := range values {
		s, ok := v.(string)
		if !ok {
			return Settings{}, fmt.Errorf("%s: %s must be a word, such as %s", Key, k, allowed[k])
		}
		m[k] = s
	}
	return fromMap(m)
}

// FromYAML reads the settings from a compose file's text.
func FromYAML(text []byte) (Settings, error) {
	lines := strings.Split(strings.ReplaceAll(string(text), "\r\n", "\n"), "\n")
	for i, line := range lines {
		rest, ok := strings.CutPrefix(stripComment(line), Key+":")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest != "" {
			return flow(rest)
		}
		return block(lines[i+1:])
	}
	return Default, nil
}

// flow reads "{deploy: releases, tier: infra}".
func flow(s string) (Settings, error) {
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return Settings{}, fmt.Errorf("%s: write the settings as indented key: value lines, or {key: value, ...}", Key)
	}
	m := map[string]string{}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return Default, nil
	}
	for _, pair := range strings.Split(inner, ",") {
		k, v, err := keyValue(pair)
		if err != nil {
			return Settings{}, err
		}
		m[k] = v
	}
	return fromMap(m)
}

// block reads the indented lines under x-lighthouse:.
func block(lines []string) (Settings, error) {
	m := map[string]string{}
	indent := ""
	for _, line := range lines {
		line = stripComment(line)
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			break
		}
		lead := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if indent == "" {
			indent = lead
		}
		if lead != indent {
			return Settings{}, fmt.Errorf("%s: settings are single key: value lines, nothing nested", Key)
		}
		k, v, err := keyValue(line)
		if err != nil {
			return Settings{}, err
		}
		m[k] = v
	}
	return fromMap(m)
}

func keyValue(s string) (string, string, error) {
	k, v, ok := strings.Cut(s, ":")
	k, v = strings.TrimSpace(k), unquote(strings.TrimSpace(v))
	if !ok || k == "" || v == "" {
		return "", "", fmt.Errorf("%s: %q isn't a key: value setting", Key, strings.TrimSpace(s))
	}
	return k, v, nil
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// stripComment removes a # comment (one at the start of the line, or after a
// space), outside quotes, and trailing spaces.
func stripComment(line string) string {
	quote := byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return strings.TrimRight(line[:i], " \t")
		}
	}
	return strings.TrimRight(line, " \t")
}
