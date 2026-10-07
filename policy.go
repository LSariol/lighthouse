// Package lighthouse holds what the binary embeds from the repository's top folder.
package lighthouse

import _ "embed"

// PolicyFile is policy.json, built into the binary.
//
//go:embed policy.json
var PolicyFile []byte
