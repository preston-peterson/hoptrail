// Agent-ingest endpoints (docs/v0.3-protocol-design.md §3): remote
// agents POST heartbeats, sample batches, and path snapshots here.
// All three require a bearer token from the central config's
// agents.tokens list; with no tokens configured the surface is
// effectively disabled (every request 401s).
//
// Response-code contract with agents (§3.2): 4xx means "drop the
// batch and log" (malformed, bad probe_id, clock skew); 5xx and
// connection failures mean "retry with backoff." Duplicate batches
// are acked with 200 so a retry after a lost ack stops cleanly.

package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

// maxIngestBodyBytes bounds an ingest request body. A normal batch is
// one ingest_interval (~5s) of samples — a few KB. Partition-recovery
// replay drains the agent's buffer one batch at a time, so even
// catch-up traffic stays batch-sized. 4 MiB is comfortably above any
// legitimate batch while keeping a misbehaving client from streaming
// unbounded JSON into memory.
const maxIngestBodyBytes = 4 << 20

// clockSkewBound is the accepted window between an agent-reported
// sample timestamp and central's clock (§8): anything further than
// 24h in the past or future means the agent's NTP is broken, and the
// batch is rejected loudly rather than stored at a misleading time.
const clockSkewBound = 24 * time.Hour

// probeIDRe is the agent identity shape (§6): kebab-case, 2-32 chars,
// starting alphanumeric.
var probeIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// validateProbeID enforces the §6 identity rules including the two
// reserved names: 'local' is the central daemon's own engine, 'all'
// is the merged-view virtual probe.
func validateProbeID(id string) error {
	if !probeIDRe.MatchString(id) {
		return fmt.Errorf("probe_id %q must match %s", id, probeIDRe.String())
	}
	if id == storage.LocalProbeID || id == "all" {
		return fmt.Errorf("probe_id %q is reserved", id)
	}
	return nil
}

// authAgent validates the Authorization header against the accepted
// token set: the yaml probes.tokens list (legacy, restart-to-change)
// merged with the probe_tokens table (step-120, UI-minted, applies on
// the next request). On failure it writes the 401 and returns
// ok=false; on success it returns the matched raw token — call sites
// log only its tokenPrefix() (§6: the full token never logs), and the
// heartbeat handler uses it to stamp last_used_at. Comparison is
// constant-time per token; the homelab threat model doesn't strictly
// demand it, but it costs nothing.
// authAgent validates the bearer token and returns it plus the
// probe_id it is BOUND to (step-170, audit #10). UI-minted tokens are
// bound to the probe they were minted for (the token's name); a bound
// token may only push as that probe — enforced by requireProbeBinding.
// Legacy yaml tokens (probes.tokens) are unbound (boundProbeID == "")
// for back-compat; they retain the old impersonate-any behavior, so
// the UI-minted path is the secure default.
func (s *Server) authAgent(w http.ResponseWriter, r *http.Request) (token, boundProbeID string, ok bool) {
	bindings, err := s.cfg.Store.ProbeTokenBindings(r.Context())
	if err != nil {
		s.log.Error("ingest: probe token lookup failed", "err", err)
		http.Error(w, "token lookup failed", http.StatusInternalServerError)
		return "", "", false
	}
	if len(bindings) == 0 && len(s.cfg.AgentTokens) == 0 {
		http.Error(w, "probe ingest disabled: no probe tokens configured (add one in Settings → Probes)", http.StatusUnauthorized)
		return "", "", false
	}
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || token == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return "", "", false
	}
	for _, b := range bindings {
		if subtle.ConstantTimeCompare([]byte(token), []byte(b.Token)) == 1 {
			return token, b.Name, true
		}
	}
	for _, want := range s.cfg.AgentTokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1 {
			return token, "", true // yaml token: unbound (legacy)
		}
	}
	http.Error(w, "unknown token", http.StatusUnauthorized)
	return "", "", false
}

// requireProbeBinding enforces that a bound token only acts as its
// authorized probe. Returns false (and writes 403) on mismatch. An
// empty bound id (yaml token) passes — legacy behavior.
func requireProbeBinding(w http.ResponseWriter, boundProbeID, reqProbeID string) bool {
	if boundProbeID != "" && boundProbeID != reqProbeID {
		http.Error(w, fmt.Sprintf("token is bound to probe %q and cannot act as %q", boundProbeID, reqProbeID), http.StatusForbidden)
		return false
	}
	return true
}

// decodeIngestBody decodes a size-capped JSON body into dst, mapping
// failure modes to operator-readable messages. DisallowUnknownFields
// is deliberately NOT set: a newer agent talking to an older central
// should degrade gracefully, not 400.
func decodeIngestBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxIngestBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return fmt.Errorf("body exceeds %d bytes", maxErr.Limit)
		}
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// ---------- POST /api/ingest/heartbeat ----------

type heartbeatRequest struct {
	ProbeID   string   `json:"probe_id"`
	Version   string   `json:"version"`
	StartedAt int64    `json:"started_at"`
	Targets   []string `json:"targets"`
	// Arch is the probe's GOARCH (step-168, #22) — which release
	// binary a central-driven update must serve. Empty from pre-0.7
	// probes.
	Arch string `json:"arch,omitempty"`
}

// heartbeatUpdateCommand rides the heartbeat reply while an update is
// pending or in flight for the probe (#22). Pre-0.7 probes ignore the
// unknown field — which the deliveries counter detects.
type heartbeatUpdateCommand struct {
	Version string `json:"version"` // bare semver target, e.g. "0.7.0"
	SHA256  string `json:"sha256"`  // of the binary the central serves
	Path    string `json:"path"`    // central-relative fetch path
}

type heartbeatResponse struct {
	RegisteredAt     int64                   `json:"registered_at"`
	CentralTargetSet []string                `json:"central_target_set"`
	Update           *heartbeatUpdateCommand `json:"update,omitempty"`
}

func (s *Server) handleIngestHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token, bound, ok := s.authAgent(w, r)
	if !ok {
		return
	}
	var req heartbeatRequest
	if err := decodeIngestBody(w, r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateProbeID(req.ProbeID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !requireProbeBinding(w, bound, req.ProbeID) {
		return
	}

	now := time.Now()
	remoteIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		remoteIP = host
	}
	if err := s.cfg.Store.UpsertProbeHeartbeat(r.Context(), req.ProbeID, req.Version, time.UnixMilli(req.StartedAt), now, remoteIP, req.Arch); err != nil {
		s.log.Error("ingest: heartbeat upsert failed", "probe_id", req.ProbeID, "err", err)
		http.Error(w, "heartbeat store failed", http.StatusInternalServerError)
		return
	}
	s.log.Info("ingest: heartbeat",
		"probe_id", req.ProbeID,
		"token_prefix", tokenPrefix(token),
		"agent_version", req.Version,
		"announced_targets", len(req.Targets))

	// Stamp last_used_at for UI-minted tokens (no-op for yaml ones).
	// Heartbeat cadence only — sample batches don't touch it. Failure
	// is log-and-continue: a bookkeeping miss must not fail the beat.
	if err := s.cfg.Store.TouchProbeToken(r.Context(), token, now); err != nil {
		s.log.Warn("ingest: token touch failed", "err", err)
	}

	// Central owns the target set (§3.1): the agent reshapes its local
	// probing to whatever comes back here, which is how UI-side target
	// adds/removes propagate without agent config changes.
	targets := s.cfg.Supervisor.Targets()
	if targets == nil {
		targets = []string{}
	}
	writeJSON(w, heartbeatResponse{
		RegisteredAt:     now.UnixMilli(),
		CentralTargetSet: targets,
		Update:           s.heartbeatUpdateFor(r.Context(), req.ProbeID, req.Version, now),
	})
}

// ---------- POST /api/ingest/samples ----------

type ingestSamplesRequest struct {
	ProbeID      string             `json:"probe_id"`
	BatchID      string             `json:"batch_id"`
	Samples      []ingestSampleJSON `json:"samples"`
	RouteChanges []ingestChangeJSON `json:"route_changes"`
}

type ingestSampleJSON struct {
	Target string  `json:"target"`
	TTL    int     `json:"ttl"`
	Ts     int64   `json:"ts"`
	IP     *string `json:"ip"`
	RTTms  float64 `json:"rtt_ms"`
}

type ingestChangeJSON struct {
	Target string  `json:"target"`
	TTL    int     `json:"ttl"`
	Ts     int64   `json:"ts"`
	OldIP  *string `json:"old_ip"`
	NewIP  string  `json:"new_ip"`
}

type ingestSamplesResponse struct {
	ReceivedAt int64  `json:"received_at"`
	BatchID    string `json:"batch_id"`
}

func (s *Server) handleIngestSamples(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token, bound, ok := s.authAgent(w, r)
	if !ok {
		return
	}
	var req ingestSamplesRequest
	if err := decodeIngestBody(w, r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateProbeID(req.ProbeID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !requireProbeBinding(w, bound, req.ProbeID) {
		return
	}
	if req.BatchID == "" || len(req.BatchID) > 128 {
		http.Error(w, "batch_id must be 1-128 chars", http.StatusBadRequest)
		return
	}

	now := time.Now()
	samples := make([]storage.IngestSample, 0, len(req.Samples))
	for i, smp := range req.Samples {
		if smp.Target == "" {
			http.Error(w, fmt.Sprintf("samples[%d]: empty target", i), http.StatusBadRequest)
			return
		}
		if smp.TTL < 1 || smp.TTL > 64 {
			http.Error(w, fmt.Sprintf("samples[%d]: ttl %d out of range 1-64", i, smp.TTL), http.StatusBadRequest)
			return
		}
		if smp.RTTms < 0 {
			http.Error(w, fmt.Sprintf("samples[%d]: negative rtt_ms", i), http.StatusBadRequest)
			return
		}
		if err := checkClockSkew(smp.Ts, now); err != nil {
			s.log.Warn("ingest: clock skew rejected", "probe_id", req.ProbeID, "batch_id", req.BatchID, "err", err)
			http.Error(w, fmt.Sprintf("samples[%d]: %v", i, err), http.StatusBadRequest)
			return
		}
		samples = append(samples, storage.IngestSample{
			Target: smp.Target,
			TTL:    smp.TTL,
			Ts:     smp.Ts,
			IP:     smp.IP,
			// Round at the wire edge: the storage unit is integer µs.
			RTTUs: int64(math.Round(smp.RTTms * 1000)),
		})
	}
	changes := make([]storage.IngestRouteChange, 0, len(req.RouteChanges))
	for i, rc := range req.RouteChanges {
		if rc.Target == "" || rc.NewIP == "" {
			http.Error(w, fmt.Sprintf("route_changes[%d]: empty target or new_ip", i), http.StatusBadRequest)
			return
		}
		if rc.TTL < 1 || rc.TTL > 64 {
			http.Error(w, fmt.Sprintf("route_changes[%d]: ttl %d out of range 1-64", i, rc.TTL), http.StatusBadRequest)
			return
		}
		if err := checkClockSkew(rc.Ts, now); err != nil {
			s.log.Warn("ingest: clock skew rejected", "probe_id", req.ProbeID, "batch_id", req.BatchID, "err", err)
			http.Error(w, fmt.Sprintf("route_changes[%d]: %v", i, err), http.StatusBadRequest)
			return
		}
		changes = append(changes, storage.IngestRouteChange{
			Target: rc.Target,
			TTL:    rc.TTL,
			Ts:     rc.Ts,
			OldIP:  rc.OldIP,
			NewIP:  rc.NewIP,
		})
	}

	fresh, err := s.cfg.Store.IngestBatch(r.Context(), req.ProbeID, req.BatchID, now, samples, changes)
	if err != nil {
		s.log.Error("ingest: batch write failed", "probe_id", req.ProbeID, "batch_id", req.BatchID, "err", err)
		http.Error(w, "batch store failed", http.StatusInternalServerError)
		return
	}
	if !fresh {
		// Duplicate delivery — agent retried after a lost ack. Ack
		// again with 200 so it stops; nothing was written.
		s.log.Info("ingest: duplicate batch acked",
			"probe_id", req.ProbeID, "batch_id", req.BatchID, "token_prefix", tokenPrefix(token))
	} else {
		s.log.Info("ingest: batch stored",
			"probe_id", req.ProbeID,
			"batch_id", req.BatchID,
			"token_prefix", tokenPrefix(token),
			"samples", len(samples),
			"route_changes", len(changes))
	}
	writeJSON(w, ingestSamplesResponse{ReceivedAt: now.UnixMilli(), BatchID: req.BatchID})
}

// checkClockSkew rejects agent timestamps more than clockSkewBound
// away from central's clock in either direction — the "agent's NTP is
// broken" tripwire from §8. Central never adjusts timestamps; it only
// refuses to store ones it can't trust.
func checkClockSkew(tsMs int64, now time.Time) error {
	// SECURITY (step-170, audit #13/14): compare in integer
	// milliseconds. The previous form `time.Duration(tsMs-now) * ms`
	// multiplied by 1e6 INSIDE int64 Duration math, so a far-future
	// tsMs overflowed/wrapped to a small (or negative) skew and slipped
	// past the bound — a crafted timestamp could then be stored and
	// escape retention forever. Integer ms math has no such multiply.
	skewMs := tsMs - now.UnixMilli()
	boundMs := clockSkewBound.Milliseconds()
	if skewMs > boundMs || skewMs < -boundMs {
		return fmt.Errorf("ts %d is %dms from central's clock (bound %s) — check the agent's NTP", tsMs, skewMs, clockSkewBound)
	}
	return nil
}

// ---------- POST /api/ingest/path ----------

type ingestPathRequest struct {
	ProbeID   string          `json:"probe_id"`
	Target    string          `json:"target"`
	Ts        int64           `json:"ts"`
	HopCount  int             `json:"hop_count"`
	TargetTTL int             `json:"target_ttl"`
	Hops      json.RawMessage `json:"hops"`
}

type ingestPathResponse struct {
	ReceivedAt int64 `json:"received_at"`
}

func (s *Server) handleIngestPath(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token, bound, ok := s.authAgent(w, r)
	if !ok {
		return
	}
	var req ingestPathRequest
	if err := decodeIngestBody(w, r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateProbeID(req.ProbeID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !requireProbeBinding(w, bound, req.ProbeID) {
		return
	}
	if req.Target == "" {
		http.Error(w, "empty target", http.StatusBadRequest)
		return
	}
	now := time.Now()
	// SECURITY (step-170, audit #17): apply the same skew tripwire as the
	// samples path, and validate the opaque hops blob instead of storing
	// arbitrary attacker bytes (the snapshot is re-served to the UI).
	if err := checkClockSkew(req.Ts, now); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hopsJSON := string(req.Hops)
	if hopsJSON == "" || hopsJSON == "null" {
		hopsJSON = "[]"
	} else {
		var hops []json.RawMessage
		if err := json.Unmarshal(req.Hops, &hops); err != nil {
			http.Error(w, "hops must be a JSON array", http.StatusBadRequest)
			return
		}
		if len(hops) > 64 { // matches the TTL ceiling
			http.Error(w, "too many hops (max 64)", http.StatusBadRequest)
			return
		}
	}

	if err := s.cfg.Store.UpsertPathSnapshot(r.Context(), storage.PathSnapshot{
		ProbeID:   req.ProbeID,
		Target:    req.Target,
		Ts:        req.Ts,
		HopCount:  req.HopCount,
		TargetTTL: req.TargetTTL,
		HopsJSON:  hopsJSON,
	}); err != nil {
		s.log.Error("ingest: path snapshot upsert failed", "probe_id", req.ProbeID, "target", req.Target, "err", err)
		http.Error(w, "path snapshot store failed", http.StatusInternalServerError)
		return
	}
	s.log.Info("ingest: path snapshot",
		"probe_id", req.ProbeID,
		"token_prefix", tokenPrefix(token),
		"target", req.Target,
		"hop_count", req.HopCount)
	writeJSON(w, ingestPathResponse{ReceivedAt: now.UnixMilli()})
}
