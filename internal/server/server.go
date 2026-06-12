// Package server is hoptrail's HTTP layer. It serves the embedded
// Svelte UI and three read-only JSON endpoints (see docs/api-v0.1.md)
// over a single net/http server. Lifecycle is driven by a context —
// cancelling the parent context triggers graceful shutdown that drains
// in-flight requests up to a short timeout.
//
// The server has no business logic of its own: handlers are thin glue
// over probe.Engine (for live snapshots) and storage.Store (for
// historical reads). That separation keeps the engine independently
// testable and keeps the API surface a single file's worth of code.
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/preston-peterson/hoptrail/internal/alert"
	"github.com/preston-peterson/hoptrail/internal/bandwidth"
	"github.com/preston-peterson/hoptrail/internal/logring"
	"github.com/preston-peterson/hoptrail/internal/probe"
	"github.com/preston-peterson/hoptrail/internal/storage"
)

// Supervisor is the surface area the server needs from cmd/hoptrail's
// supervisor type. Defined here as an interface (rather than the server
// importing cmd) so the package dependency stays one-way and tests can
// substitute a fake implementation.
//
// Step-29 changed the target identity type from netip.Addr to string
// so operators can monitor hostnames (`dns.google`) as well as raw
// IPs. The string is the API-level identity (map key, label, query
// param); the supervisor resolves it to an IP internally and the
// engine continues to probe a netip.Addr. Handlers needing the
// underlying IP for storage queries get it via EngineFor(id).Target().
type Supervisor interface {
	// EngineFor returns the engine monitoring `target`, or nil if no
	// such target is active. Handlers call this once per request;
	// the returned pointer is stable for the pipeline's life.
	EngineFor(target string) *probe.Engine

	// Targets returns the active target identifiers in a
	// deterministic order.
	Targets() []string

	// Intervals returns the per-target pinger cadence for every
	// active target. Used by GET /api/targets so the UI can render
	// the interval picker without an extra round-trip.
	Intervals() map[string]time.Duration

	// Thresholds returns the per-target latency thresholds (step-39)
	// for every active target. Either field may be nil meaning "the
	// operator hasn't overridden — UI should use its default preset."
	// Bundled into GET /api/targets alongside Intervals.
	Thresholds() map[string]ThresholdPair

	// Add starts monitoring a new target. The string may be a raw
	// IPv4 or a hostname; hostnames are resolved at add-time.
	// Returns an error if already monitored, unresolvable, or not
	// a valid traceroute destination.
	Add(ctx context.Context, target string) error

	// Remove stops monitoring a target. Blocks until the pipeline is
	// drained (or ctx fires).
	Remove(ctx context.Context, target string) error

	// Swap retains step-25's single-active-target semantic for the
	// legacy POST /api/target endpoint.
	Swap(ctx context.Context, target string) error

	// SetInterval changes the per-hop pinger cadence for an active
	// target. Returns an error if target isn't monitored or interval
	// is out of bounds. The supervisor persists the change so the
	// new cadence survives a restart.
	SetInterval(ctx context.Context, target string, interval time.Duration) error

	// SetThresholds changes the per-tab latency thresholds (step-39).
	// Both pointers nil clears the override. The supervisor persists
	// the change so the new thresholds survive a restart. Returns an
	// error if target isn't monitored or values are non-positive /
	// out of ordering.
	SetThresholds(ctx context.Context, target string, warningMs, criticalMs *int64) error

	// FinalHopOnlys returns the per-target final-hop-only flag
	// (step-41) for every active target. Bundled into GET /api/targets.
	FinalHopOnlys() map[string]bool

	// SetFinalHopOnly toggles the per-tab final-hop-only mode.
	// Triggers a pipeline rebuild internally; persists through to
	// active_targets so the change survives restart. Returns an
	// error if target isn't monitored.
	SetFinalHopOnly(ctx context.Context, target string, finalHopOnly bool) error
}

// ThresholdPair is the server-package mirror of the supervisor's
// ThresholdPair — declared here so the Supervisor interface stays
// importable without the server package depending on cmd/hoptrail.
// Same shape and semantics; pointer fields encode "no override."
type ThresholdPair struct {
	WarningMs  *int64
	CriticalMs *int64
}

// Config is everything the server needs to handle requests.
type Config struct {
	// ListenAddr is the address to bind, e.g. ":8080" or "127.0.0.1:8080".
	ListenAddr string

	// Supervisor is the live probe-pipeline manager. Required.
	Supervisor Supervisor

	// Store provides historical reads for /api/samples and /api/route_changes.
	Store *storage.Store

	// WebFS is the embedded Svelte bundle; index.html and assets/ are
	// served from here. Typically the result of internal/web.FS().
	WebFS fs.FS

	// ShutdownTimeout caps how long the server will wait for in-flight
	// requests to finish during graceful shutdown. Defaults to 5s.
	ShutdownTimeout time.Duration

	// Version is the hoptrail version string set at build time via
	// -ldflags. Empty falls back to "dev" so the /api/version response
	// always has something readable. Surfaced to the UI for display.
	Version string

	// AgentTokens is the list of bearer tokens accepted on the
	// /api/ingest/* surface (v0.3 §6). Empty disables probe ingest —
	// every ingest request 401s, which is the correct shape for a
	// zero-probe deploy.
	AgentTokens []string

	// RetentionDays mirrors cfg.Storage.RetentionDays for the
	// /api/retention display endpoint (step-97). Zero is rendered
	// as-is — config validation guarantees >= 1 in practice.
	RetentionDays int

	// BandwidthRunner + BandwidthCapability wire the v0.4 bandwidth
	// engine (step-100). Both nil-able: a daemon wired without them
	// serves the bandwidth endpoints in a degraded "engine not
	// running / capability unknown" mode rather than 404ing, so the
	// UI's capability-routed display states work uniformly.
	BandwidthRunner     BandwidthRunner
	BandwidthCapability func() bandwidth.Capability

	// SpeedtestInstall runs the speedtest-CLI install helper
	// (step-123). Nil gets the production implementation (`sudo -n`
	// of the sudoers-whitelisted root-owned script); tests inject a
	// fake so the suite never touches sudo or package managers.
	SpeedtestInstall func(ctx context.Context) ([]byte, error)

	// RecheckCapability forces an immediate capability re-detect
	// after a successful UI-driven CLI install, so the settings panel
	// flips from the install card to the real fields on its next poll
	// instead of waiting out the 60s background re-detect. Nil-able.
	RecheckCapability func()

	// InstallDir is the hoptrail install root for the self-update
	// endpoints (step-124). Empty = the production /opt/hoptrail;
	// tests point it at a temp dir.
	InstallDir string

	// UpdateHooks injects the privileged/exec seams of the update
	// apply path (sudo cat/setcap/systemctl, staged-binary version
	// probe). Nil = production implementations.
	UpdateHooks *UpdateHooks

	// ReleaseSource fetches GitHub release metadata and binaries for
	// the release-update mode (#11). Nil disables the check/download
	// endpoints (they answer 501) — tests inject a fake.
	ReleaseSource ReleaseSource

	// System-settings wiring (step-125). LogLevel is the level the
	// daemon booted with (yaml or override row); ApplyLogLevel makes
	// a new level effective immediately (slog.LevelVar). RDNSEnabled
	// is whether the rdns worker is actually running this boot —
	// toggling it is a config-row write + restart.
	LogLevel      string
	ApplyLogLevel func(level string) error
	RDNSEnabled   bool

	// LogRing feeds GET /api/logs (step-128, the web-UI log viewer).
	// Nil → the endpoint answers 503 (probe role / older wiring).
	LogRing *logring.Ring

	// Alerting wiring (step-136). AlertReconfigure pushes a PATCHed
	// config into the live engine; AlertSenderStatus reports the last
	// delivery attempt; AlertPost is the test-send seam (nil =
	// production ntfy POST).
	AlertReconfigure  func(alert.Config)
	AlertSenderStatus func() (time.Time, string)
	AlertPost         AlertPostFunc

	// NtfyInstall runs the local-ntfy install helper (step-136). Nil
	// gets the production `sudo -n` implementation.
	NtfyInstall func(ctx context.Context) ([]byte, error)

	// Status-page wiring (step-140): the daemon's start time (uptime)
	// and the SQLite path (disk-size display).
	StartedAt time.Time
	DBPath    string

	// AllowedHosts is the operator's extra Host-header allowlist for the
	// anti-rebinding guard (step-170): reverse-proxy hostnames or public
	// FQDNs the UI is legitimately reached at. Loopback, bare IP
	// literals, and single-label/.local intranet names are always
	// allowed without listing.
	AllowedHosts []string
}

// Server owns the *http.Server, the route table, and the references it
// needs to serve requests. One Server per running daemon.
type Server struct {
	cfg Config
	log *slog.Logger
	srv *http.Server

	// cliInstall tracks the one-at-a-time UI-driven speedtest-CLI
	// install (step-123). Zero value = idle.
	cliInstall cliInstallState

	// system tracks the live-applied system settings (step-125).
	system systemState

	// ntfyInstall tracks the one-at-a-time local-ntfy install (step-136).
	ntfyInstall cliInstallState

	// rollout tracks the one-at-a-time fleet probe-update (#22).
	rollout rolloutState
}

// New constructs a Server with the given config. The HTTP listener is
// not started until Run is called.
func New(cfg Config, log *slog.Logger) (*Server, error) {
	if cfg.ListenAddr == "" {
		return nil, errors.New("server: ListenAddr must not be empty")
	}
	if cfg.Supervisor == nil {
		return nil, errors.New("server: Supervisor must not be nil")
	}
	if cfg.Store == nil {
		return nil, errors.New("server: Store must not be nil")
	}
	if cfg.WebFS == nil {
		return nil, errors.New("server: WebFS must not be nil")
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 5 * time.Second
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}

	s := &Server{cfg: cfg, log: log}
	s.srv = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		// SECURITY (step-170, audit/critic): bound slow-body (Slowloris)
		// and idle-connection exhaustion. ReadTimeout covers the whole
		// request incl. body (the largest legitimate body is a ~20 MB
		// binary upload, comfortable in 60s on a LAN); IdleTimeout reaps
		// kept-alive sockets. No WriteTimeout — the log-follow and some
		// reads are deliberately long-lived; those are GET reads, not a
		// DoS lever.
		ReadTimeout: 60 * time.Second,
		IdleTimeout: 120 * time.Second,
	}
	return s, nil
}

// routes builds the request mux. /api/* routes to the JSON handlers;
// everything else falls through to the static-file handler, which
// serves the SPA bundle from WebFS with an index.html fallback for
// unknown paths (so client-side routing works in v0.2+).
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/path", s.handlePath)
	mux.HandleFunc("/api/samples", s.handleSamples)
	mux.HandleFunc("/api/route_changes", s.handleRouteChanges)
	mux.HandleFunc("/api/target", s.handleTarget)
	mux.HandleFunc("/api/targets", s.handleTargets)
	mux.HandleFunc("/api/targets/", s.handleTargetByPath)
	mux.HandleFunc("/api/target_history", s.handleTargetHistory)
	mux.HandleFunc("/api/bundles", s.handleBundles)
	mux.HandleFunc("/api/bundles/", s.handleBundleByPath)
	mux.HandleFunc("/api/annotations", s.handleAnnotations)
	mux.HandleFunc("/api/annotations/", s.handleAnnotationByPath)
	mux.HandleFunc("/api/export", s.handleExport)
	// Step-69: multi-tab-per-target endpoints. /api/tabs is GET (list)
	// + POST (create). /api/tabs/order is a special-cased PATCH for
	// bulk reorder. /api/tabs/<id> is PATCH (partial update) + DELETE.
	mux.HandleFunc("/api/tabs", s.handleTabs)
	mux.HandleFunc("/api/tabs/", s.handleTabByPath)
	// Step-85: surfaced to the UI for display next to the wordmark.
	mux.HandleFunc("/api/version", s.handleVersion)
	// Step-89: agent-ingest surface (v0.3 §3). Bearer-token gated;
	// disabled (401) when no agents.tokens are configured.
	mux.HandleFunc("/api/ingest/heartbeat", s.handleIngestHeartbeat)
	mux.HandleFunc("/api/ingest/samples", s.handleIngestSamples)
	mux.HandleFunc("/api/ingest/path", s.handleIngestPath)
	mux.HandleFunc("/api/ingest/update-binary", s.handleIngestUpdateBinary)
	mux.HandleFunc("/api/ingest/update-status", s.handleIngestUpdateStatus)
	// Step-93: probe list for the UI's ProbePicker.
	mux.HandleFunc("/api/probes", s.handleProbes)
	mux.HandleFunc("/api/probes/update-all", s.handleProbesUpdateAll)
	mux.HandleFunc("/api/probes/", s.handleProbeByPath)
	mux.HandleFunc("/api/probe-tokens", s.handleProbeTokens)
	mux.HandleFunc("/api/probe-tokens/", s.handleProbeTokenByPath)
	// Step-97: retention policy display ("how far back do stats go").
	mux.HandleFunc("/api/retention", s.handleRetention)
	// Step-111: resume-vs-new support.
	mux.HandleFunc("/api/target_stats", s.handleTargetStats)
	mux.HandleFunc("/api/target_data", s.handleTargetData)
	// Step-100: v0.4 bandwidth surface.
	mux.HandleFunc("/api/bandwidth/config", s.handleBandwidthConfig)
	mux.HandleFunc("/api/bandwidth/history", s.handleBandwidthHistory)
	mux.HandleFunc("/api/bandwidth/derate-status", s.handleBandwidthDerateStatus)
	mux.HandleFunc("/api/bandwidth/run", s.handleBandwidthRun)
	mux.HandleFunc("/api/bandwidth/install-cli", s.handleBandwidthInstallCLI)

	// Self-update, local-upload mode (step-124).
	mux.HandleFunc("/api/update", s.handleUpdateStatus)
	mux.HandleFunc("/api/update/upload", s.handleUpdateUpload)
	mux.HandleFunc("/api/update/apply", s.handleUpdateApply)
	mux.HandleFunc("/api/update/check", s.handleUpdateCheck)
	mux.HandleFunc("/api/update/download", s.handleUpdateDownload)

	// System settings + UI-driven restart (step-125).
	mux.HandleFunc("/api/system", s.handleSystemSettings)
	mux.HandleFunc("/api/system/restart", s.handleSystemRestart)

	// Dashboard section layout (step-126).
	mux.HandleFunc("/api/layout", s.handleLayout)

	// Web-UI log viewer feed (step-128).
	mux.HandleFunc("/api/logs", s.handleLogs)

	// Alerting (step-136).
	mux.HandleFunc("/api/alerts/config", s.handleAlertsConfig)
	mux.HandleFunc("/api/alerts/test", s.handleAlertsTest)
	mux.HandleFunc("/api/alerts/status", s.handleAlertsStatus)
	mux.HandleFunc("/api/alerts/install-ntfy", s.handleAlertsInstallNtfy)
	mux.HandleFunc("/api/alerts/history", s.handleAlertsHistory)
	mux.HandleFunc("/api/alerts/sound", s.handleAlertsSound)

	// Environment status overview (step-140).
	mux.HandleFunc("/api/status", s.handleStatus)

	// In-app documentation (step-143).
	mux.HandleFunc("/api/docs", s.handleDocs)
	mux.HandleFunc("/api/docs/", s.handleDocBySlug)
	mux.Handle("/", s.staticHandler())
	// Order: logging outermost (records everything, incl. CSRF 403s),
	// then the cross-origin guard, then the mux.
	return s.withLogging(s.crossOriginGuard(mux))
}

// Run starts the HTTP listener and blocks until ctx is canceled. On
// cancellation, performs a graceful shutdown bounded by
// ShutdownTimeout. Returns any error from net.Listen, Serve, or
// Shutdown — except for http.ErrServerClosed which is the normal
// shutdown path.
//
// The listener is bound synchronously before the "listening" log line
// fires. The previous implementation logged "listening" inside the
// goroutine before ListenAndServe returned, which produced confusing
// log sequences when the bind itself failed (the operator would see
// "server: listening" immediately followed by "bind: address already
// in use" — the first line lied about what had actually happened).
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("server: listen failed: %w", err)
	}
	s.log.Info("server: listening", "addr", s.cfg.ListenAddr)

	serveErr := make(chan error, 1)
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("server: serve failed: %w", err)
		}
		return nil
	case <-ctx.Done():
		// Graceful shutdown — let in-flight requests drain.
		shutCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		if err := s.srv.Shutdown(shutCtx); err != nil {
			return fmt.Errorf("server: shutdown: %w", err)
		}
		s.log.Info("server: shut down cleanly")
		return nil
	}
}

// staticHandler serves the embedded Svelte bundle with SPA-style
// fallback: if the requested path isn't a file in the bundle, serve
// index.html instead. This preserves the option to add client-side
// routes in v0.2+ without re-engineering the server.
func (s *Server) staticHandler() http.Handler {
	fileServer := http.FileServer(http.FS(s.cfg.WebFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := s.cfg.WebFS.Open(path); err != nil {
			// Path doesn't exist in the bundle — fall back to index.html.
			// Rewrite the URL on a copy of the request to avoid mutating
			// shared state.
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// withLogging wraps a handler in a one-line-per-request slog logger,
// tagged with method, path, status, and duration. Errors get a higher
// log level than successful responses.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)

		// Don't spam the log with static-asset hits; only log API and
		// non-200 responses.
		if !strings.HasPrefix(r.URL.Path, "/api/") && rec.status < 400 {
			return
		}
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		} else if rec.status >= 400 {
			level = slog.LevelWarn
		}
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", dur.Milliseconds(),
		}
		// Step-55: enrich 4xx log lines with the query string and
		// User-Agent so we can identify the offending caller when a
		// validation fails. Successful requests stay terse to keep
		// the access log readable at probe scale.
		if rec.status >= 400 && rec.status < 500 {
			if r.URL.RawQuery != "" {
				attrs = append(attrs, "query", r.URL.RawQuery)
			}
			if ua := r.Header.Get("User-Agent"); ua != "" {
				attrs = append(attrs, "user_agent", ua)
			}
			if ref := r.Header.Get("Referer"); ref != "" {
				attrs = append(attrs, "referer", ref)
			}
		}
		s.log.Log(r.Context(), level, "http", attrs...)
	})
}

// statusRecorder wraps http.ResponseWriter to capture the response
// status code for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// discardWriter is the slog default when callers pass a nil logger.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
