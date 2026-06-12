package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestHostAllowed(t *testing.T) {
	s := &Server{cfg: Config{AllowedHosts: []string{"hoptrail.example.com"}}}
	cases := []struct {
		host string
		want bool
	}{
		{"localhost:8080", true},
		{"127.0.0.1:8080", true},
		{"[::1]:8080", true},
		{"192.168.1.50:8080", true}, // bare LAN IP — operator typed it
		{"10.0.0.5", true},
		{"hoptrail-lan:8080", true}, // single-label intranet name
		{"nas.local:8080", true},     // mDNS
		{"hoptrail.example.com", true}, // explicitly allowlisted
		{"HOPTRAIL.EXAMPLE.COM:8080", true}, // case-insensitive + port
		{"evil.attacker.com:8080", false},   // multi-label FQDN, rebinding shape
		{"victim.example.org", false},
		{"", false},
	}
	for _, c := range cases {
		if got := s.hostAllowed(c.host); got != c.want {
			t.Errorf("hostAllowed(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	r, _ := http.NewRequest("POST", "http://box:8080/api/x", nil)
	r.Host = "box:8080"
	if !sameOrigin("http://box:8080", r) {
		t.Error("same host:port should be same-origin")
	}
	if !sameOrigin("https://box:8080", r) {
		t.Error("scheme is intentionally not compared (proxy/TLS termination)")
	}
	if sameOrigin("http://evil.com:8080", r) {
		t.Error("different host must not be same-origin")
	}
}

// csrfTestServer wraps a trivial handler in the guard.
func csrfHandler(t *testing.T, allowed []string) http.Handler {
	t.Helper()
	s := &Server{cfg: Config{AllowedHosts: allowed}}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	return s.crossOriginGuard(inner)
}

func doReq(h http.Handler, method, host string, headers map[string]string) *recorder {
	r, _ := http.NewRequest(method, "http://"+host+"/api/update/upload", strings.NewReader("body"))
	r.Host = host
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := &recorder{code: 200}
	h.ServeHTTP(rec, r)
	return rec
}

type recorder struct {
	code int
	body strings.Builder
}

func (r *recorder) Header() http.Header        { return http.Header{} }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(c int)           { r.code = c }

func TestCrossOriginGuard(t *testing.T) {
	h := csrfHandler(t, nil)

	// GET passes untouched.
	if rec := doReqGet(h, "box:8080"); rec.code != 200 {
		t.Errorf("GET = %d, want 200", rec.code)
	}

	// Same-origin POST with the header: allowed.
	if rec := doReq(h, "POST", "192.168.1.5:8080", map[string]string{
		"Origin": "http://192.168.1.5:8080", "X-Hoptrail-CSRF": "1",
	}); rec.code != 200 {
		t.Errorf("legit same-origin POST = %d, want 200", rec.code)
	}

	// THE DRIVE-BY: cross-origin POST with no custom header (a no-cors
	// simple request) — blocked. Even though Origin is cross, the
	// header gate alone stops it.
	if rec := doReq(h, "POST", "192.168.1.5:8080", map[string]string{
		"Origin": "http://evil.com",
	}); rec.code != http.StatusForbidden {
		t.Errorf("drive-by POST = %d, want 403", rec.code)
	}

	// Cross-origin POST that somehow set the header still fails the
	// Origin same-origin check.
	if rec := doReq(h, "POST", "192.168.1.5:8080", map[string]string{
		"Origin": "http://evil.com", "X-Hoptrail-CSRF": "1",
	}); rec.code != http.StatusForbidden {
		t.Errorf("cross-origin-with-header POST = %d, want 403", rec.code)
	}

	// DNS-rebinding shape: Host is a registered FQDN not allowlisted.
	if rec := doReq(h, "POST", "attacker.example.com:8080", map[string]string{
		"X-Hoptrail-CSRF": "1",
	}); rec.code != http.StatusForbidden {
		t.Errorf("rebinding Host = %d, want 403", rec.code)
	}

	// Missing header, same-origin: blocked (the header is mandatory).
	if rec := doReq(h, "POST", "192.168.1.5:8080", map[string]string{
		"Origin": "http://192.168.1.5:8080",
	}); rec.code != http.StatusForbidden {
		t.Errorf("missing-header POST = %d, want 403", rec.code)
	}

	// Ingest surface is exempt (bearer-authed machine client, no header).
	r, _ := http.NewRequest("POST", "http://anyhost/api/ingest/heartbeat", strings.NewReader("{}"))
	r.Host = "anyhost"
	rec := &recorder{code: 200}
	h.ServeHTTP(rec, r)
	if rec.code != 200 {
		t.Errorf("ingest POST through guard = %d, want 200 (exempt)", rec.code)
	}
}

func doReqGet(h http.Handler, host string) *recorder {
	r, _ := http.NewRequest("GET", "http://"+host+"/api/status", nil)
	r.Host = host
	rec := &recorder{code: 200}
	h.ServeHTTP(rec, r)
	return rec
}
