// Package hoptrail (module root) exists to embed the repo-level
// documents — README, CHANGELOG, CONTRIBUTING, SECURITY, LICENSE —
// for the in-app documentation viewer (step-144). The guide set in
// docs/ has its own embed (docs/embed.go); go:embed can't reach
// outside a package's directory, hence two embed points.
package hoptrail

import "embed"

// RootDocs holds the repo-level markdown documents + LICENSE.
//
//go:embed README.md CHANGELOG.md CONTRIBUTING.md SECURITY.md LICENSE
var RootDocs embed.FS
