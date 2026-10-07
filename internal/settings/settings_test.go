package settings

import (
	"strings"
	"testing"
)

func TestFromYAML(t *testing.T) {
	cases := []struct {
		text string
		want Settings
	}{
		{"name: plop\nservices: {}\n", Default},
		{"name: sparkdb\nx-lighthouse:\n  deploy: releases   # tags only\n  tier: data\n  backup: postgres\nservices:\n  db: {}\n",
			Settings{Deploy: DeployReleases, Tier: TierData, Backup: BackupPostgres}},
		{"x-lighthouse: {deploy: releases, tier: infra}\nservices: {}\n", Settings{Deploy: DeployReleases, Tier: TierInfra}},
		{"x-lighthouse:\r\n    # a comment\r\n\r\n    deploy: \"releases\"\r\nname: cove\r\n", Settings{Deploy: DeployReleases, Tier: TierApp}},
		{"x-lighthouse: {}\n", Default},
		{"services:\n  web:\n    x-lighthouse:\n      deploy: releases\n", Default},
	}
	for i, c := range cases {
		got, err := FromYAML([]byte(c.text))
		if err != nil || got != c.want {
			t.Errorf("case %d: %+v, %v; want %+v", i, got, err, c.want)
		}
	}
}

func TestFromYAMLRefuses(t *testing.T) {
	for _, text := range []string{
		"x-lighthouse:\n  deploy: tags\n",
		"x-lighthouse:\n  deplyo: releases\n",
		"x-lighthouse:\n  backup:\n    service: db\n",
		"x-lighthouse:\n  deploy: releases\n    tier: data\n",
		"x-lighthouse: releases\n",
		"x-lighthouse: {deploy}\n",
	} {
		if s, err := FromYAML([]byte(text)); err == nil {
			t.Errorf("FromYAML accepted %q as %+v", text, s)
		} else if !strings.Contains(err.Error(), "x-lighthouse") {
			t.Errorf("error doesn't say where: %v", err)
		}
	}
}

func TestFromConfig(t *testing.T) {
	s, err := FromConfig([]byte(`{"name":"cove","services":{},"x-lighthouse":{"deploy":"releases","tier":"infra"}}`))
	if err != nil || s != (Settings{Deploy: DeployReleases, Tier: TierInfra}) {
		t.Errorf("FromConfig = %+v, %v", s, err)
	}
	if s, err := FromConfig([]byte(`{"services":{}}`)); err != nil || s != Default {
		t.Errorf("no block: %+v, %v", s, err)
	}
	for _, bad := range []string{`{"x-lighthouse":{"tier":"core"}}`, `{"x-lighthouse":{"deploy":true}}`, `{"x-lighthouse":"releases"}`} {
		if _, err := FromConfig([]byte(bad)); err == nil {
			t.Errorf("FromConfig accepted %s", bad)
		}
	}
}

func TestOrder(t *testing.T) {
	if !(Settings{Tier: TierData}.Order() < Settings{Tier: TierInfra}.Order() && Settings{Tier: TierInfra}.Order() < Default.Order()) {
		t.Error("tiers aren't ordered data, infra, app")
	}
}
