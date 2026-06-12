// Package web embeds the compiled Svelte bundle and exposes it as an
// fs.FS for the HTTP server to serve as static content. The bundle
// itself is produced by `npm run build` in the web/ directory before
// `go build` runs; see web/README.md and the project Makefile.
//
// Embedding the bundle this way preserves the single-file deploy
// story: no separate `static/` directory to copy alongside the
// binary, no path-juggling at runtime, no version skew between the
// daemon and its UI.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the embedded web bundle rooted at the dist/ directory,
// so callers can request files by their bundle-relative path (e.g.
// "index.html", "assets/index-abc123.js").
//
// At repository state, dist/ contains a placeholder index.html so the
// embed directive always finds at least one file at compile time;
// running `npm run build` overwrites it with the real bundle.
func FS() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}
