// Cross-origin request defense (step-170, security audit 2026-06-12,
// findings #1-8/#12): the web API is unauthenticated by design (trusted
// LAN), but that made every state-changing endpoint reachable from a
// malicious page in the operator's LAN browser — a single forged
// cross-origin POST could upload+run a binary, drive sudo, mint tokens,
// or change the listen address. This middleware closes that class.
//
// Three layers, all cheap, on mutating requests (anything but
// GET/HEAD/OPTIONS):
//
//  1. Custom-header gate (the teeth). A mutating request must carry
//     `X-Hoptrail-CSRF`. A cross-origin page CANNOT set a custom header
//     on a "simple" request without triggering a CORS preflight, and we
//     never emit Access-Control-Allow-* — so the preflight fails and the
//     drive-by request never reaches the handler. Same-origin requests
//     (the real UI, served from this same daemon) set the header freely.
//
//  2. Origin same-origin check (defense in depth). If an Origin header
//     is present, its host:port must equal the request's Host.
//
//  3. Host allowlist (anti DNS-rebinding). Classic rebinding navigates
//     the victim to attacker.example (a registered, multi-label FQDN)
//     that re-resolves to the LAN IP; the browser then sends
//     Host: attacker.example. We accept only loopback, bare IP literals
//     (the operator typed the LAN IP directly — not a rebinding shape),
//     single-label/.local intranet names, and any host the operator
//     explicitly allowlisted (top-level allowed_hosts in config.yaml —
//     for reverse proxies / public FQDNs). A registered FQDN that isn't
//     allowlisted is rejected, which is exactly the rebinding vector.
//
// Exemptions: /api/ingest/* is machine-to-machine, bearer-token
// authenticated, and called by the Go probe client (no browser, no
// custom header, arbitrary Host) — CSRF/rebinding don't apply, so it
// bypasses this middleware. Static assets and GET reads pass through
// (reads must stay side-effect-free; the audit confirmed they are).

package server

import (
	"net"
	"net/http"
	"strings"
)

const csrfHeader = "X-Hoptrail-CSRF"

// crossOriginGuard wraps the mux. See file header for the model.
func (s *Server) crossOriginGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reads, preflight, and the bearer-authed ingest surface are
		// out of scope.
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			next.ServeHTTP(w, r)
			return
		case http.MethodOptions:
			// We never approve cross-origin: respond to preflight with
			// no Access-Control-Allow-* headers, so the browser blocks
			// the actual request.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/ingest/") {
			next.ServeHTTP(w, r)
			return
		}
		// Only guard the API; static assets are GETs, but be explicit.
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		// Layer 3: Host allowlist (anti-rebinding).
		if !s.hostAllowed(r.Host) {
			// Name the rejected Host. Without this the guard is silent:
			// the access log records a bare 403 and the operator has no
			// way to see WHICH Host was refused, so a legitimate
			// reverse-proxy/FQDN address that merely needs allowlisting
			// looks identical to an attack. Recovering it otherwise means
			// digging the referer out of the 4xx access log.
			s.log.Warn("csrf: Host rejected by DNS-rebinding guard — add it to allowed_hosts if this is a real address",
				"host", r.Host,
				"method", r.Method,
				"path", r.URL.Path,
				"remote", r.RemoteAddr)
			http.Error(w, "request Host not allowed (DNS-rebinding guard) — add it to the top-level allowed_hosts list in config.yaml if this is your real address", http.StatusForbidden)
			return
		}
		// Layer 2: Origin must be same-origin when present.
		if origin := r.Header.Get("Origin"); origin != "" {
			if !sameOrigin(origin, r) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		// Layer 1 (the teeth): custom header required.
		if r.Header.Get(csrfHeader) == "" {
			http.Error(w, "missing "+csrfHeader+" header (cross-site request protection)", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed implements the anti-rebinding allowlist.
func (s *Server) hostAllowed(hostHeader string) bool {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".") // FQDN trailing dot
	if host == "" {
		return false
	}
	lower := strings.ToLower(host)

	// Loopback names + any IP literal (loopback or LAN — the operator
	// typed an address, which is not a rebinding shape).
	if lower == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return true
	}
	// Single-label intranet names and *.local (mDNS) can't be public
	// rebinding domains — they're not registrable FQDNs.
	if !strings.Contains(lower, ".") || strings.HasSuffix(lower, ".local") {
		return true
	}
	// Operator-allowlisted hosts (reverse proxies, public FQDNs).
	for _, a := range s.cfg.AllowedHosts {
		if lower == strings.ToLower(strings.TrimSuffix(a, ".")) {
			return true
		}
	}
	return false
}

// sameOrigin reports whether the Origin header's host:port matches the
// request's Host (the request is same-origin). Scheme is not compared:
// hoptrail commonly runs plain http and may sit behind a TLS-terminating
// proxy, so host:port equality is the meaningful check.
func sameOrigin(origin string, r *http.Request) bool {
	// Origin is "scheme://host[:port]"; strip the scheme.
	i := strings.Index(origin, "://")
	if i < 0 {
		return false
	}
	originHostPort := origin[i+3:]
	return strings.EqualFold(hostPortKey(originHostPort), hostPortKey(r.Host))
}

// hostPortKey normalizes a host[:port] for comparison: lowercased host,
// trailing-dot stripped, explicit default ports dropped.
func hostPortKey(hp string) string {
	host, port, err := net.SplitHostPort(hp)
	if err != nil {
		host = hp
		port = ""
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if port == "80" || port == "443" {
		port = ""
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}
