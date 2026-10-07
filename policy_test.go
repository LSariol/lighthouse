package lighthouse

import (
	"testing"

	"github.com/lsariol/lighthouse/internal/policy"
)

// The policy.json that ships must load, or Lighthouse won't start.
func TestPolicyFileLoads(t *testing.T) {
	if _, err := policy.Parse(PolicyFile); err != nil {
		t.Fatal(err)
	}
}
