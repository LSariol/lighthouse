package lighthouse

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/policy"
	"github.com/lsariol/lighthouse/internal/settings"
)

// The policy.json that ships must load, or Lighthouse won't start.
func TestPolicyFileLoads(t *testing.T) {
	if _, err := policy.Parse(PolicyFile); err != nil {
		t.Fatal(err)
	}
}

// Lighthouse deploys itself: its own compose file must pass the deploy
// rules with the exceptions policy.json gives it, need no secrets (the
// update helper couldn't keep them), and deploy releases.
func TestOwnComposeFileDeploys(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose isn't installed")
	}
	ctx := context.Background()
	dir, _ := os.Getwd()
	r := compose.Runner{}
	p, err := r.Inspect(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	vars, err := r.Variables(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vars {
		if v.Default == "" {
			t.Errorf("docker-compose.yml needs ${%s} from Cove; Lighthouse's own compose file can't use secrets", v.Name)
		}
	}

	rules, _ := policy.Parse(PolicyFile)
	findings, err := policy.Check(policy.Input{Project: p.Name, Config: p.Config, Dir: dir, Storage: "/srv/server/storage",
		Resolve: func(path string) string { return path }})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rules.Apply(p.Name, findings) {
		t.Errorf("refused: %s", f)
	}

	s, err := settings.FromConfig(p.Config)
	if err != nil || !s.Releases() || s.Tier != settings.TierInfra {
		t.Errorf("x-lighthouse = %+v, %v; want releases, infra", s, err)
	}
	if !strings.Contains(string(p.Config), `"/lighthouse"`) {
		t.Error("no healthcheck running /lighthouse health")
	}
}
