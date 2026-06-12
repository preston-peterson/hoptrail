package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDocs_IndexAndContent(t *testing.T) {
	f := newFixture(t)

	code, body := f.doJSON(t, http.MethodGet, "/api/docs", "")
	if code != http.StatusOK || !strings.Contains(string(body), "user-guide") {
		t.Fatalf("index: %d %s", code, body)
	}

	res, err := http.Get(f.ts.URL + "/api/docs/user-guide")
	if err != nil {
		t.Fatalf("GET doc: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("doc: %d", res.StatusCode)
	}
	md, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(md), "# Hoptrail User Guide") {
		t.Errorf("doc content unexpected: %.80s", md)
	}

	// Unknown slugs 404 (the slug allowlist is the traversal defense;
	// `../` shapes never even reach the handler — client and mux both
	// canonicalize them away).
	for _, p := range []string{"/api/docs/nope", "/api/docs/user-guide/extra"} {
		res, err := http.Get(f.ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, res.StatusCode)
		}
	}
}
