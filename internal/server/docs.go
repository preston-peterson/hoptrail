// In-app documentation endpoints (steps 143-144): list + raw markdown
// for the gear → Documentation viewer. Same shape as the sibling
// project's /api/docs — the client renders the markdown itself. Two
// embed sources (go:embed can't cross package directories): the
// guides in docs/, and the repo-level documents at the module root.

package server

import (
	"net/http"
	"strings"

	hoptrail "github.com/preston-peterson/hoptrail"
	"github.com/preston-peterson/hoptrail/docs"
)

// docEntry maps a viewer slug to its embed source + filename. Only
// slugs listed here are served — anything else (or a path-traversal
// shape) 404s.
type docEntry struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	root  bool   `json:"-"` // true = RootDocs, false = docs.FS
	file  string `json:"-"`
}

// docIndex is the curated viewer order: overview first, the guides,
// then the project-meta documents.
var docIndex = []docEntry{
	{Slug: "readme", Title: "Overview", root: true, file: "README.md"},
	{Slug: "user-guide", Title: "User guide", file: "user-guide.md"},
	{Slug: "distributed-probing", Title: "Distributed probing", file: "distributed-probing.md"},
	{Slug: "operations", Title: "Operations", file: "operations.md"},
	{Slug: "api", Title: "API reference", file: "api.md"},
	{Slug: "changelog", Title: "Changelog", root: true, file: "CHANGELOG.md"},
	{Slug: "contributing", Title: "Contributing", root: true, file: "CONTRIBUTING.md"},
	{Slug: "security", Title: "Security policy", root: true, file: "SECURITY.md"},
	{Slug: "license", Title: "License", root: true, file: "LICENSE"},
}

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{"docs": docIndex})
}

func (s *Server) handleDocBySlug(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	slug := strings.TrimPrefix(r.URL.Path, "/api/docs/")
	for _, d := range docIndex {
		if d.Slug != slug {
			continue
		}
		var content []byte
		var err error
		if d.root {
			content, err = hoptrail.RootDocs.ReadFile(d.file)
		} else {
			content, err = docs.FS.ReadFile(d.file)
		}
		if err != nil {
			http.Error(w, "doc unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(content)
		return
	}
	http.Error(w, "no such doc", http.StatusNotFound)
}
