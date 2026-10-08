// Package runner carries the pool's job entrypoint, which the runner image
// ships next to the runner itself.
package runner

import _ "embed"

// Entrypoint is runner/entrypoint.sh.
//
//go:embed entrypoint.sh
var Entrypoint []byte
