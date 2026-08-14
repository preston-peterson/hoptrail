// Package docs embeds the published markdown guides into the binary
// so the web UI can serve them in-app (gear → Documentation) — the
// docs you read always match the version you run, and nobody has to
// hunt through the repo for them (step-143).
package docs

import "embed"

// FS holds every top-level markdown guide in this directory.
//
//go:embed *.md
var FS embed.FS
