// Package lighthouse holds what the binary embeds from the repository's top
// folder.
package lighthouse

import _ "embed"

// PolicyFile is policy.json: the exceptions to the deploy rules (see
// internal/policy). It's built into the binary, so a change is reviewed in
// git and takes effect when Lighthouse is deployed.
//
//go:embed policy.json
var PolicyFile []byte
