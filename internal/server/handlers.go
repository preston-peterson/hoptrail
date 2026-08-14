package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/preston-peterson/hoptrail/internal/analysis"
	"github.com/preston-peterson/hoptrail/internal/probe"
	"github.com/preston-peterson/hoptrail/internal/release"
	"github.com/preston-peterson/hoptrail/internal/storage"
)

// engineUnavailable is the standard response when the supervisor has
// no engine for the requested target — usually mid-add/remove or a
// target that was never added. The handler returns 503 and the client
// (UI) treats it like a connection error: retried on next poll.
const engineUnavailable = "engine not yet available"

// resolveTarget returns the target identifier an /api/{path,samples,
// route_changes} request should scope to. The identifier is whatever
// the operator supplied via POST /api/targets — IP or hostname —
// kept as a string so the storage/query layer can use it as-is or
// translate to the resolved IP via EngineFor when needed.
//
// Rules:
//
//   - If ?target=<id> is present and non-empty, use it. The caller
//     calls Supervisor.EngineFor; a nil engine means 404 (the
//     identifier isn't currently monitored).
//   - If absent and exactly one target is active, default to that
//     one — back-compat with the single-target API surface.
//   - If absent with zero/multiple targets active, return an error
//     the caller surfaces as 503 / 400 respectively.
//
// errStatus is 0 on success.
func (s *Server) resolveTarget(r *http.Request) (string, int, error) {
	if v := r.URL.Query().Get("target"); v != "" {
		return v, 0, nil
	}
	active := s.cfg.Supervisor.Targets()
	switch len(active) {
	case 0:
		return "", http.StatusServiceUnavailable,
			errors.New("no targets currently monitored")
	case 1:
		return active[0], 0, nil
	default:
		return "", http.StatusBadRequest,
			fmt.Errorf("target query param required (active: %s)",
				strings.Join(active, ", "))
	}
}

// Defaults for the historical-read endpoints when the client doesn't
// specify a window.
const (
	defaultSamplesWindow     = 5 * time.Minute
	defaultRouteChangesLimit = 50
	maxRouteChangesLimit     = 500
)

// probeOfflineAfter is how stale a probe's last_seen_at can be before
// /api/probes reports it offline: 3× the default 60s heartbeat
// interval (v0.3 design §8). Central doesn't know each agent's
// configured interval, so the default-based threshold is the v0.3
// answer; a per-probe interval column can refine it later.
const probeOfflineAfter = 3 * 60 * time.Second

// resolveProbeID returns the probe an /api/{path,samples,
// route_changes,export} request should scope to. Absent or empty
// param defaults to the local probe — the central daemon's own
// engine — which keeps every v0.2 client's requests meaning exactly
// what they always meant. Named agents are checked against the
// probes table (404 on unknown); "all" is reserved for the future
// merged-overview view and rejected until that ships.
func (s *Server) resolveProbeID(r *http.Request) (string, int, error) {
	v := r.URL.Query().Get("probe_id")
	if v == "" || v == storage.LocalProbeID {
		return storage.LocalProbeID, 0, nil
	}
	if v == "all" {
		return "", http.StatusBadRequest,
			errors.New("probe_id=all (merged overview) is not implemented yet")
	}
	probes, err := s.cfg.Store.ListProbes(r.Context())
	if err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("probes lookup: %w", err)
	}
	for _, p := range probes {
		if p.ProbeID == v {
			return v, 0, nil
		}
	}
	return "", http.StatusNotFound, fmt.Errorf("probe %q is not registered", v)
}

// ---------- /api/probes ----------

type probeJSON struct {
	ProbeID    string  `json:"probe_id"`
	Label      *string `json:"label"`
	// IP is the probe's source address as seen by the central on its
	// last heartbeat (step-142). Absent for the local probe.
	IP *string `json:"ip,omitempty"`
	Version    *string `json:"version"`
	Online     bool    `json:"online"`
	LastSeenAt *int64  `json:"last_seen_at"` // nil for the local probe (always here)
	StartedAt  *int64  `json:"started_at"`
	// Outdated says this probe's release is behind the central's
	// (step-163: base-semver compare, so a central dev build doesn't
	// flag probes on the same release). Always false for local — it IS
	// the central's binary.
	Outdated bool `json:"outdated"`
	// Arch + Pin + Update (step-168, #22): what a central-driven
	// update needs to know and show. Update is nil when no command
	// has been issued.
	Arch   *string            `json:"arch,omitempty"`
	Pin    bool               `json:"pin"`
	Update *probeUpdateJSON   `json:"update,omitempty"`
}

type probeUpdateJSON struct {
	TargetVersion string `json:"target_version"`
	State         string `json:"state"`
	Error         string `json:"error,omitempty"`
	RequestedAt   int64  `json:"requested_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

type probesResponse struct {
	Probes []probeJSON `json:"probes"`
}

// handleProbes lists the registered probes for the UI's ProbePicker.
// The local probe is synthesized first — it isn't a row in the probes
// table (it doesn't heartbeat; it IS the daemon) but it's always a
// valid selection.
func (s *Server) handleProbes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	probes, err := s.cfg.Store.ListProbes(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("probes: %v", err), http.StatusInternalServerError)
		return
	}
	updates := map[string]storage.ProbeUpdate{}
	if rows, err := s.cfg.Store.ListProbeUpdates(r.Context()); err == nil {
		for _, pu := range rows {
			updates[pu.ProbeID] = pu
		}
	}

	version := s.cfg.Version
	if version == "" {
		version = "dev"
	}
	resp := probesResponse{Probes: []probeJSON{{
		ProbeID: storage.LocalProbeID,
		Version: &version,
		Online:  true,
	}}}
	now := time.Now()
	for _, p := range probes {
		seen := p.LastSeenAt
		pj := probeJSON{
			ProbeID:    p.ProbeID,
			Label:      p.Label,
			IP:         p.LastIP,
			Version:    p.Version,
			Online:     now.Sub(time.UnixMilli(p.LastSeenAt)) < probeOfflineAfter,
			LastSeenAt: &seen,
			StartedAt:  p.StartedAt,
		}
		if p.Version != nil {
			pj.Outdated = release.Newer(version, *p.Version)
		}
		pj.Arch = p.Arch
		pj.Pin = p.Pin
		if pu, ok := updates[p.ProbeID]; ok {
			pj.Update = &probeUpdateJSON{
				TargetVersion: pu.TargetVersion, State: pu.State, Error: pu.Error,
				RequestedAt: pu.RequestedAt, UpdatedAt: pu.UpdatedAt,
			}
		}
		resp.Probes = append(resp.Probes, pj)
	}
	writeJSON(w, resp)
}

// ---------- /api/path ----------

// pathResponse is the wire shape returned by /api/path. See
// docs/api-v0.1.md for field semantics.
type pathResponse struct {
	Target    string    `json:"target"`
	StartedAt int64     `json:"started_at"`
	HopCount  int       `json:"hop_count"`
	TargetTTL int       `json:"target_ttl"` // 0 if destination not yet reached
	Hops      []hopJSON `json:"hops"`

	// Step-93: set when the response describes a remote probe's path
	// (served from the path_snapshots table rather than a live local
	// engine). SnapshotTs is when the agent took the snapshot — the
	// UI's staleness signal. Both omitted for the local probe so the
	// v0.2 wire shape is byte-identical.
	ProbeID    string `json:"probe_id,omitempty"`
	SnapshotTs *int64 `json:"snapshot_ts,omitempty"`
}

type hopJSON struct {
	TTL          int     `json:"ttl"`
	CurrentIP    *string `json:"current_ip"`
	Hostname     *string `json:"hostname"` // nil if not yet resolved or no PTR record
	CurrentRTTms float64 `json:"current_rtt_ms"`
	AvgRTTms     float64 `json:"avg_rtt_ms"`
	MinRTTms     float64 `json:"min_rtt_ms"`
	LossPercent  float64 `json:"loss_percent"`
	LossState    string  `json:"loss_state"` // "ok" | "suspect" | "rate_limited"
	LastResponse *int64  `json:"last_response"`
}

func (s *Server) handlePath(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	target, errStatus, err := s.resolveTarget(r)
	if err != nil {
		http.Error(w, err.Error(), errStatus)
		return
	}
	probeID, errStatus, err := s.resolveProbeID(r)
	if err != nil {
		http.Error(w, err.Error(), errStatus)
		return
	}

	// Remote probes have no live engine here — their current path is
	// whatever they last reported into path_snapshots.
	if probeID != storage.LocalProbeID {
		s.handleAgentPath(w, r, probeID, target)
		return
	}

	engine := s.cfg.Supervisor.EngineFor(target)
	if engine == nil {
		http.Error(w, fmt.Sprintf("target %s not currently monitored", target), http.StatusNotFound)
		return
	}

	snap, err := engine.PathSnapshot(r.Context())
	if err != nil {
		// Most likely a context cancellation during shutdown.
		http.Error(w, fmt.Sprintf("snapshot: %v", err), http.StatusServiceUnavailable)
		return
	}

	// The response's `target` field surfaces the operator-typed
	// identity (what they see in the tab), not the engine's resolved
	// IP. Step-93 folded the response build into buildPathResponse —
	// the step-45 helper the export endpoint already used; /api/path
	// had kept a duplicated inline copy.
	writeJSON(w, s.buildPathResponse(r.Context(), target, engine, snap))
}

// agentHopJSON is the hops_json element shape agents POST (see
// cmd/hoptrail/agent.go wireHop): hopJSON's field names minus
// hostname and loss_state, which are computed centrally below.
type agentHopJSON struct {
	TTL          int     `json:"ttl"`
	CurrentIP    *string `json:"current_ip"`
	CurrentRTTms float64 `json:"current_rtt_ms"`
	AvgRTTms     float64 `json:"avg_rtt_ms"`
	MinRTTms     float64 `json:"min_rtt_ms"`
	LossPercent  float64 `json:"loss_percent"`
	LastResponse *int64  `json:"last_response"`
}

// handleAgentPath serves /api/path for a remote probe.
func (s *Server) handleAgentPath(w http.ResponseWriter, r *http.Request, probeID, target string) {
	resp, errStatus, err := s.buildAgentPathResponse(r.Context(), probeID, target)
	if err != nil {
		http.Error(w, err.Error(), errStatus)
		return
	}
	writeJSON(w, *resp)
}

// buildAgentPathResponse assembles a pathResponse for a remote probe:
// the stored snapshot's raw per-hop stats, enriched with rdns
// hostnames (the rdns worker resolves agent-seen IPs too — they flow
// through the same samples table) and the attributed-loss
// classification, so the hop list renders identically regardless of
// which probe measured it. Returns a 404-shaped error when the probe
// hasn't reported a path for the target.
func (s *Server) buildAgentPathResponse(ctx context.Context, probeID, target string) (*pathResponse, int, error) {
	snap, err := s.cfg.Store.GetPathSnapshot(ctx, probeID, target)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("path snapshot: %w", err)
	}
	if snap == nil {
		return nil, http.StatusNotFound, fmt.Errorf("probe %s has not reported a path for %s yet", probeID, target)
	}
	var hops []agentHopJSON
	if err := json.Unmarshal([]byte(snap.HopsJSON), &hops); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("stored snapshot is malformed: %w", err)
	}

	ips := make([]string, 0, len(hops))
	for _, h := range hops {
		if h.CurrentIP != nil {
			ips = append(ips, *h.CurrentIP)
		}
	}
	hostnames, err := s.cfg.Store.LookupHostnames(ctx, ips)
	if err != nil {
		s.log.Error("agent path: lookup hostnames", "err", err)
		hostnames = map[string]string{}
	}

	lossWindow := make([]analysis.HopLossSnapshot, 0, len(hops))
	for _, h := range hops {
		lossWindow = append(lossWindow, analysis.HopLossSnapshot{
			TTL:  uint8(h.TTL),
			Loss: h.LossPercent,
		})
	}
	attributed := analysis.AttributedLoss(lossWindow, analysis.DefaultLossTolerance)

	ts := snap.Ts
	resp := pathResponse{
		Target: target,
		// No engine start time for a remote probe; the snapshot time
		// is the closest meaningful anchor and doubles as the
		// staleness signal via snapshot_ts.
		StartedAt:  snap.Ts,
		HopCount:   snap.HopCount,
		TargetTTL:  snap.TargetTTL,
		Hops:       make([]hopJSON, 0, len(hops)),
		ProbeID:    probeID,
		SnapshotTs: &ts,
	}
	for i, h := range hops {
		hop := hopJSON{
			TTL:          h.TTL,
			CurrentIP:    h.CurrentIP,
			CurrentRTTms: h.CurrentRTTms,
			AvgRTTms:     h.AvgRTTms,
			MinRTTms:     h.MinRTTms,
			LossPercent:  h.LossPercent,
			LossState:    lossStateFor(attributed[i]),
			LastResponse: h.LastResponse,
		}
		if h.CurrentIP != nil {
			if name, ok := hostnames[*h.CurrentIP]; ok {
				hop.Hostname = &name
			}
		}
		resp.Hops = append(resp.Hops, hop)
	}
	return &resp, 0, nil
}

// lossStateFor maps an AttributedHopLoss to the JSON `loss_state` enum.
// Three states matter to the UI: no loss to display, real loss (suspect),
// or apparent loss that doesn't affect traffic (rate-limited).
func lossStateFor(a analysis.AttributedHopLoss) string {
	if a.Loss == 0 {
		return "ok"
	}
	if a.Suspect {
		return "suspect"
	}
	return "rate_limited"
}

// ---------- /api/samples ----------

type samplesResponse struct {
	Since   int64        `json:"since"`
	Until   int64        `json:"until"`
	Samples []sampleJSON `json:"samples"`
}

type sampleJSON struct {
	TTL   int     `json:"ttl"`
	Ts    int64   `json:"ts"`
	IP    *string `json:"ip"`
	RTTms float64 `json:"rtt_ms"`
}

func (s *Server) handleSamples(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	now := time.Now()
	until, err := parseTimeMs(r, "until", now.UnixMilli())
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid until: %v", err), http.StatusBadRequest)
		return
	}
	since, err := parseTimeMs(r, "since", now.Add(-defaultSamplesWindow).UnixMilli())
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid since: %v", err), http.StatusBadRequest)
		return
	}
	// Step-65: optional `bucket_ms` parameter for server-side
	// downsampling. When set to a positive value, the server returns
	// one representative sample per (TTL, time-bucket) — the earliest
	// sample in each bucket. This keeps payloads manageable at long
	// windows (7d × 11 hops × 1 sample/sec = 6.6M raw samples; with
	// 5min buckets that drops to ~22k samples, fast over the wire and
	// fast to render). When absent or zero, the raw-sample query runs.
	bucketMs, err := parseTimeMs(r, "bucket_ms", 0)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid bucket_ms: %v", err), http.StatusBadRequest)
		return
	}
	if bucketMs < 0 {
		http.Error(w, "bucket_ms must be non-negative", http.StatusBadRequest)
		return
	}

	// SQLite query: index on (probe_id, target, ttl, ts) covers this
	// directly. The target filter is critical, not optional — without
	// it, samples from one monitored target leak into another's view.
	// The probe_id filter (step-88) is equally load-bearing: SQLite has
	// no skip-scan, so a query without the index's leading column does
	// a full table scan. Step-93: the filter value is the resolved
	// ?probe_id param (absent = local, preserving v0.2 semantics).
	//
	// As of step-34 the storage `target` column holds the operator-
	// typed identifier (Sample.TargetID — IP string or hostname), not
	// the resolved IP. Handler queries pass the typed string directly,
	// matching whatever the operator typed when they added the target.
	// This is what makes hostname-targets keep their history across
	// periodic IP re-resolution.
	target, errStatus, err := s.resolveTarget(r)
	if err != nil {
		http.Error(w, err.Error(), errStatus)
		return
	}
	probeID, errStatus, err := s.resolveProbeID(r)
	if err != nil {
		http.Error(w, err.Error(), errStatus)
		return
	}
	var rows *sql.Rows
	if bucketMs > 0 {
		// Bucketed path: CTE with ROW_NUMBER over (ttl, bucket) to pick
		// the earliest sample in each bucket. Picking by `ts ASC` keeps
		// the result deterministic and avoids any "which IP do we
		// return for a bucket spanning a route change?" ambiguity (the
		// first one wins). Bucket boundary semantics: `ts / bucket_ms`
		// is integer division, so two samples in the same bucket have
		// the same quotient. Bucket-edge samples (ts exactly equal to a
		// multiple of bucket_ms) land in the LATER bucket — fine for
		// the trend view this serves.
		rows, err = s.cfg.Store.DB().QueryContext(r.Context(),
			`WITH bucketed AS (
				SELECT ttl, ts, ip, rtt_us,
					ROW_NUMBER() OVER (
						PARTITION BY ttl, ts / ?
						ORDER BY ts
					) AS rn
				FROM samples
				WHERE probe_id = ? AND target = ? AND ts >= ? AND ts <= ?
			)
			SELECT ttl, ts, ip, rtt_us
			FROM bucketed
			WHERE rn = 1
			ORDER BY ts ASC, ttl ASC`,
			bucketMs, probeID, target, since, until,
		)
	} else {
		rows, err = s.cfg.Store.DB().QueryContext(r.Context(),
			`SELECT ttl, ts, ip, rtt_us
			 FROM samples
			 WHERE probe_id = ? AND target = ? AND ts >= ? AND ts <= ?
			 ORDER BY ts ASC, ttl ASC`,
			probeID, target, since, until,
		)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	resp := samplesResponse{
		Since:   since,
		Until:   until,
		Samples: []sampleJSON{}, // empty array, not null
	}
	for rows.Next() {
		var ttl int64
		var ts int64
		var ip sql.NullString
		var rttUs int64
		if err := rows.Scan(&ttl, &ts, &ip, &rttUs); err != nil {
			http.Error(w, fmt.Sprintf("scan: %v", err), http.StatusInternalServerError)
			return
		}
		s := sampleJSON{
			TTL:   int(ttl),
			Ts:    ts,
			RTTms: float64(rttUs) / 1000.0,
		}
		if ip.Valid {
			val := ip.String
			s.IP = &val
		}
		resp.Samples = append(resp.Samples, s)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, fmt.Sprintf("rows: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, resp)
}

// ---------- /api/route_changes ----------

type changesResponse struct {
	Since   *int64       `json:"since"`
	Limit   int          `json:"limit"`
	Changes []changeJSON `json:"changes"`
}

type changeJSON struct {
	TTL   int     `json:"ttl"`
	Ts    int64   `json:"ts"`
	OldIP *string `json:"old_ip"`
	NewIP string  `json:"new_ip"`
}

func (s *Server) handleRouteChanges(w http.ResponseWriter, r *http.Request) {
	// Step-75: operator-initiated clear via DELETE /api/route_changes?target=X.
	// Requires a target — global wipe is not exposed (operators clear
	// per-tab, not across the daemon).
	if r.Method == http.MethodDelete {
		target, errStatus, err := s.resolveTarget(r)
		if err != nil {
			http.Error(w, err.Error(), errStatus)
			return
		}
		if err := s.cfg.Store.ClearRouteChanges(r.Context(), target); err != nil {
			s.log.Error("route_changes: clear failed", "target", target, "err", err)
			http.Error(w, fmt.Sprintf("clear failed: %v", err), http.StatusInternalServerError)
			return
		}
		s.log.Info("route_changes: cleared", "target", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limit := defaultRouteChangesLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, fmt.Sprintf("invalid limit: %v", err), http.StatusBadRequest)
			return
		}
		if n > maxRouteChangesLimit {
			n = maxRouteChangesLimit
		}
		limit = n
	}

	var sinceMs *int64
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid since: %v", err), http.StatusBadRequest)
			return
		}
		sinceMs = &n
	}

	// Same target-keying as /api/samples (step-34): query by the
	// operator-typed identifier directly. The idx_route_changes_query
	// (probe_id, target, ttl, ts) index covers it; probe_id is the
	// resolved ?probe_id param (absent = local) since step-93.
	target, errStatus, err := s.resolveTarget(r)
	if err != nil {
		http.Error(w, err.Error(), errStatus)
		return
	}
	probeID, errStatus, err := s.resolveProbeID(r)
	if err != nil {
		http.Error(w, err.Error(), errStatus)
		return
	}

	var rows *sql.Rows
	if sinceMs != nil {
		rows, err = s.cfg.Store.DB().QueryContext(r.Context(),
			`SELECT ttl, ts, old_ip, new_ip
			 FROM route_changes
			 WHERE probe_id = ? AND target = ? AND ts >= ?
			 ORDER BY ts DESC
			 LIMIT ?`,
			probeID, target, *sinceMs, limit,
		)
	} else {
		rows, err = s.cfg.Store.DB().QueryContext(r.Context(),
			`SELECT ttl, ts, old_ip, new_ip
			 FROM route_changes
			 WHERE probe_id = ? AND target = ?
			 ORDER BY ts DESC
			 LIMIT ?`,
			probeID, target, limit,
		)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	resp := changesResponse{
		Since:   sinceMs,
		Limit:   limit,
		Changes: []changeJSON{},
	}
	for rows.Next() {
		var ttl, ts int64
		var oldIP sql.NullString
		var newIP string
		if err := rows.Scan(&ttl, &ts, &oldIP, &newIP); err != nil {
			http.Error(w, fmt.Sprintf("scan: %v", err), http.StatusInternalServerError)
			return
		}
		ch := changeJSON{TTL: int(ttl), Ts: ts, NewIP: newIP}
		if oldIP.Valid {
			val := oldIP.String
			ch.OldIP = &val
		}
		resp.Changes = append(resp.Changes, ch)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, fmt.Sprintf("rows: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, resp)
}

// ---------- /api/target (legacy, single-target) ----------
//
// Step-25 introduced this endpoint for the click-to-edit-target UI
// when the daemon was single-target. Step-26 generalized to N
// targets, but this endpoint remains for backward compat: GET returns
// the only active target (or errors if there are multiple), POST
// replaces all active targets with the given one (Supervisor.Swap
// semantic). Once the UI shifts entirely to tab-based affordances,
// this endpoint and Supervisor.Swap go away together.

type targetResponse struct {
	Target string `json:"target"`
}

type targetRequest struct {
	Target string `json:"target"`
}

// Cap on the request body — a JSON object holding one IP string is
// tiny; anything bigger is probably an attack or a bug.
const targetRequestMaxBytes = 256

func (s *Server) handleTarget(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleTargetGet(w, r)
	case http.MethodPost:
		s.handleTargetPost(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleTargetGet(w http.ResponseWriter, r *http.Request) {
	target, errStatus, err := s.resolveTarget(r)
	if err != nil {
		http.Error(w, err.Error(), errStatus)
		return
	}
	writeJSON(w, targetResponse{Target: target})
}

func (s *Server) handleTargetPost(w http.ResponseWriter, r *http.Request) {
	id, err := decodeTargetRequest(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// No-op short-circuit: same target as the only active one.
	if active := s.cfg.Supervisor.Targets(); len(active) == 1 && active[0] == id {
		writeJSON(w, targetResponse{Target: id})
		return
	}

	if err := s.cfg.Supervisor.Swap(r.Context(), id); err != nil {
		// Same error-mapping rules as /api/targets POST — input-level
		// failures (resolve failed, not a valid traceroute target,
		// IPv6) come back as 400, real failures as 500.
		msg := err.Error()
		switch {
		case strings.Contains(msg, "resolve "),
			strings.Contains(msg, "not a valid traceroute target"),
			strings.Contains(msg, "IPv6"),
			strings.Contains(msg, "target is empty"):
			http.Error(w, msg, http.StatusBadRequest)
		default:
			s.log.Error("target: swap failed", "target", id, "err", err)
			http.Error(w, fmt.Sprintf("swap failed: %v", err), http.StatusInternalServerError)
		}
		return
	}
	s.log.Info("target: swapped", "target", id)
	writeJSON(w, targetResponse{Target: id})
}

// ---------- /api/bundles (step-36) ----------
//
// Named target presets persisted in SQLite. Operators save the
// current tab set as a bundle, then load it again later with one
// click. The load semantic (replace vs extend) lives on the client
// — backend just stores and returns the set of targets.

// Step-71: bundles grow a `tabs` field carrying per-tab display state
// (label, thresholds) alongside the legacy `targets` field. The two
// are kept consistent server-side: Targets is the bare list (legacy
// readers); Tabs has the full shape (step-71+ readers). New saves
// accept either shape; the server normalizes to Tabs internally.
type bundleTabJSON struct {
	Target     string  `json:"target"`
	Label      *string `json:"label,omitempty"`
	WarningMs  *int64  `json:"warning_ms,omitempty"`
	CriticalMs *int64  `json:"critical_ms,omitempty"`
}

type bundleJSON struct {
	Name      string          `json:"name"`
	CreatedAt int64           `json:"created_at"`
	Targets   []string        `json:"targets"`
	Tabs      []bundleTabJSON `json:"tabs"`
}

type bundlesResponse struct {
	Bundles []bundleJSON `json:"bundles"`
}

type saveBundleRequest struct {
	Name    string          `json:"name"`
	Targets []string        `json:"targets,omitempty"`
	Tabs    []bundleTabJSON `json:"tabs,omitempty"`
}

func bundleTabsAsJSON(in []storage.BundleTab) []bundleTabJSON {
	out := make([]bundleTabJSON, len(in))
	for i, t := range in {
		out[i] = bundleTabJSON{Target: t.Target, Label: t.Label, WarningMs: t.WarningMs, CriticalMs: t.CriticalMs}
	}
	return out
}

// Cap on the request body — name + ~20 targets × 253 chars hostname
// max ≈ 5 KB. 16 KB is plenty of headroom; anything bigger is a bug
// or attack.
const bundleRequestMaxBytes = 16 * 1024

func (s *Server) handleBundles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		bundles, err := s.cfg.Store.ListBundles(r.Context())
		if err != nil {
			s.log.Error("bundles: list failed", "err", err)
			http.Error(w, "list bundles failed", http.StatusInternalServerError)
			return
		}
		out := make([]bundleJSON, len(bundles))
		for i, b := range bundles {
			out[i] = bundleJSON{Name: b.Name, CreatedAt: b.CreatedAt, Targets: b.Targets, Tabs: bundleTabsAsJSON(b.Tabs)}
		}
		writeJSON(w, bundlesResponse{Bundles: out})
	case http.MethodPost:
		s.handleBundleSave(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleBundleSave(w http.ResponseWriter, r *http.Request) {
	var req saveBundleRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, bundleRequestMaxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		http.Error(w, "bundle name is required", http.StatusBadRequest)
		return
	}
	if len(name) > 64 {
		http.Error(w, "bundle name is too long (max 64 chars)", http.StatusBadRequest)
		return
	}

	// Step-71: accept either the new `tabs` field (preferred) or the
	// legacy `targets` field. When both are present, tabs wins. When
	// neither is present, save an empty bundle.
	var tabs []storage.BundleTab
	if len(req.Tabs) > 0 {
		tabs = make([]storage.BundleTab, len(req.Tabs))
		for i, t := range req.Tabs {
			if t.Target == "" {
				http.Error(w, "tab.target is required", http.StatusBadRequest)
				return
			}
			tabs[i] = storage.BundleTab{Target: t.Target, Label: t.Label, WarningMs: t.WarningMs, CriticalMs: t.CriticalMs}
		}
	} else {
		tabs = make([]storage.BundleTab, len(req.Targets))
		for i, t := range req.Targets {
			tabs[i] = storage.BundleTab{Target: t}
		}
	}

	// Don't bother validating individual targets here — they're
	// the same strings the operator uses for /api/targets POST,
	// and they get validated again at load time when each target
	// is Add()'d. A bundle that contains a since-invalidated
	// hostname is recoverable (delete + recreate); save-time
	// validation would just add friction.
	if err := s.cfg.Store.SaveBundle(r.Context(), name, tabs); err != nil {
		s.log.Error("bundles: save failed", "name", name, "err", err)
		http.Error(w, fmt.Sprintf("save failed: %v", err), http.StatusInternalServerError)
		return
	}
	// Echo the resulting bundle back. ListBundles is fine here because
	// the response shape needs both targets + tabs and we want a single
	// canonical encoding (no duplication of the synthesis logic).
	targets := make([]string, len(tabs))
	for i, t := range tabs {
		targets[i] = t.Target
	}
	s.log.Info("bundles: saved", "name", name, "tab_count", len(tabs))
	writeJSON(w, bundleJSON{Name: name, Targets: targets, Tabs: bundleTabsAsJSON(tabs)})
}

func (s *Server) handleBundleByPath(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	const prefix = "/api/bundles/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, prefix)
	if name == "" || strings.Contains(name, "/") {
		http.Error(w, "bundle name required in path", http.StatusBadRequest)
		return
	}
	if err := s.cfg.Store.DeleteBundle(r.Context(), name); err != nil {
		s.log.Error("bundles: delete failed", "name", name, "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	s.log.Info("bundles: deleted", "name", name)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- /api/annotations (step-42) ----------
//
// Timeline notes — operator-typed strings pinned to specific moments.
// GET lists notes for a target in [since, until] (defaults: full
// history if since omitted, "now" if until omitted). POST creates a
// new note. DELETE on /api/annotations/<id> removes by ID.
// Per-target scoping uses ?target= (no defaulting — annotations are
// always tab-context, never global).

const annotationMaxBytes = 4 * 1024
const annotationTextMaxLen = 280 // tweet-sized; long enough to describe an event, short enough to render inline

type annotationJSON struct {
	ID        int64  `json:"id"`
	Target    string `json:"target"`
	Ts        int64  `json:"ts"`
	Text      string `json:"text"`
	CreatedAt int64  `json:"created_at"`
}

type annotationsResponse struct {
	Annotations []annotationJSON `json:"annotations"`
}

type addAnnotationRequest struct {
	Target string `json:"target"`
	Ts     int64  `json:"ts"`
	Text   string `json:"text"`
}

func (s *Server) handleAnnotations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleAnnotationsList(w, r)
	case http.MethodPost:
		s.handleAnnotationsAdd(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAnnotationsList(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	if target == "" {
		http.Error(w, "target query param is required", http.StatusBadRequest)
		return
	}
	since, err := parseTimeMs(r, "since", 0)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid since: %v", err), http.StatusBadRequest)
		return
	}
	until, err := parseTimeMs(r, "until", 0)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid until: %v", err), http.StatusBadRequest)
		return
	}
	rows, err := s.cfg.Store.ListAnnotations(r.Context(), target, since, until)
	if err != nil {
		s.log.Error("annotations: list failed", "target", target, "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	out := make([]annotationJSON, len(rows))
	for i, a := range rows {
		out[i] = annotationJSON{ID: a.ID, Target: a.Target, Ts: a.Ts, Text: a.Text, CreatedAt: a.CreatedAt}
	}
	writeJSON(w, annotationsResponse{Annotations: out})
}

func (s *Server) handleAnnotationsAdd(w http.ResponseWriter, r *http.Request) {
	var req addAnnotationRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, annotationMaxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	target := strings.TrimSpace(req.Target)
	text := strings.TrimSpace(req.Text)
	if target == "" {
		http.Error(w, "target is required", http.StatusBadRequest)
		return
	}
	if text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	if len([]rune(text)) > annotationTextMaxLen {
		http.Error(w, fmt.Sprintf("text too long (max %d chars)", annotationTextMaxLen), http.StatusBadRequest)
		return
	}
	if req.Ts <= 0 {
		http.Error(w, "ts must be a positive unix-ms timestamp", http.StatusBadRequest)
		return
	}
	id, err := s.cfg.Store.AddAnnotation(r.Context(), target, req.Ts, text)
	if err != nil {
		s.log.Error("annotations: add failed", "target", target, "err", err)
		http.Error(w, "add failed", http.StatusInternalServerError)
		return
	}
	s.log.Info("annotations: added", "id", id, "target", target, "ts", req.Ts)
	// Echo the full inserted row so the UI can render without an
	// extra GET round-trip.
	writeJSON(w, annotationJSON{
		ID: id, Target: target, Ts: req.Ts, Text: text,
		CreatedAt: time.Now().UnixMilli(),
	})
}

func (s *Server) handleAnnotationByPath(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	const prefix = "/api/annotations/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, prefix)
	if idStr == "" || strings.Contains(idStr, "/") {
		http.Error(w, "annotation id required in path", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "id must be an integer", http.StatusBadRequest)
		return
	}
	if err := s.cfg.Store.DeleteAnnotation(r.Context(), id); err != nil {
		s.log.Error("annotations: delete failed", "id", id, "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	s.log.Info("annotations: deleted", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- /api/export (step-45) ----------
//
// Bundles everything an operator needs to make a case to their ISP
// or share with a colleague: the current path snapshot, samples in
// the requested window, route changes in the window, and any
// annotations the operator has pinned. Returned as a single JSON
// download with Content-Disposition so the browser saves it to
// disk. Future-proof on format — JSON opens in any text editor and
// can be re-imported into a future hoptrail to render the chart.
//
// Window defaults to "last 1 hour" if since/until are omitted, which
// covers the common "I'm sharing what just happened" case without
// forcing the operator to think about timestamps.

const defaultExportWindow = time.Hour

type exportBundle struct {
	SchemaVersion int              `json:"schema_version"`
	GeneratedAt   int64            `json:"generated_at"`
	Target        string           `json:"target"`
	ProbeID       string           `json:"probe_id"` // step-93: which probe's data this is
	Window        exportWindow     `json:"window"`
	Path          *pathResponse    `json:"path,omitempty"`
	Samples       []sampleJSON     `json:"samples"`
	RouteChanges  []changeJSON     `json:"route_changes"`
	Annotations   []annotationJSON `json:"annotations"`
}

type exportWindow struct {
	Since int64 `json:"since"`
	Until int64 `json:"until"`
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target, errStatus, err := s.resolveTarget(r)
	if err != nil {
		http.Error(w, err.Error(), errStatus)
		return
	}
	probeID, errStatus, err := s.resolveProbeID(r)
	if err != nil {
		http.Error(w, err.Error(), errStatus)
		return
	}
	now := time.Now()
	until, err := parseTimeMs(r, "until", now.UnixMilli())
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid until: %v", err), http.StatusBadRequest)
		return
	}
	since, err := parseTimeMs(r, "since", now.Add(-defaultExportWindow).UnixMilli())
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid since: %v", err), http.StatusBadRequest)
		return
	}
	// SECURITY (step-170, critic): the export loads the whole window
	// into memory and serializes it as one JSON bundle with no LIMIT.
	// Bound the span so an unauthenticated caller can't request months
	// at second-resolution and exhaust memory. 32 days covers the
	// largest retention setting with headroom.
	const maxExportSpanMs = int64(32 * 24 * 60 * 60 * 1000)
	if until-since > maxExportSpanMs {
		http.Error(w, "export window too large (max ~32 days) — narrow since/until", http.StatusBadRequest)
		return
	}

	bundle := exportBundle{
		SchemaVersion: 1,
		GeneratedAt:   now.UnixMilli(),
		Target:        target,
		ProbeID:       probeID,
		Window:        exportWindow{Since: since, Until: until},
		Samples:       []sampleJSON{},
		RouteChanges:  []changeJSON{},
		Annotations:   []annotationJSON{},
	}

	// Path snapshot is best-effort — if the engine is mid-add or
	// the target was just removed (or a remote probe hasn't reported
	// yet), we still want to export whatever historical data we have.
	if probeID == storage.LocalProbeID {
		if eng := s.cfg.Supervisor.EngineFor(target); eng != nil {
			if snap, err := eng.PathSnapshot(r.Context()); err == nil {
				path := s.buildPathResponse(r.Context(), target, eng, snap)
				bundle.Path = &path
			}
		}
	} else if path, _, err := s.buildAgentPathResponse(r.Context(), probeID, target); err == nil {
		bundle.Path = path
	}

	// Samples in window — same shape and filter as /api/samples.
	rows, err := s.cfg.Store.DB().QueryContext(r.Context(),
		`SELECT ttl, ts, ip, rtt_us
		 FROM samples
		 WHERE probe_id = ? AND target = ? AND ts >= ? AND ts <= ?
		 ORDER BY ts ASC, ttl ASC`,
		probeID, target, since, until,
	)
	if err != nil {
		http.Error(w, fmt.Sprintf("samples query: %v", err), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var ttl, ts int64
		var ip sql.NullString
		var rttUs int64
		if err := rows.Scan(&ttl, &ts, &ip, &rttUs); err != nil {
			rows.Close()
			http.Error(w, fmt.Sprintf("samples scan: %v", err), http.StatusInternalServerError)
			return
		}
		sj := sampleJSON{TTL: int(ttl), Ts: ts, RTTms: float64(rttUs) / 1000.0}
		if ip.Valid {
			v := ip.String
			sj.IP = &v
		}
		bundle.Samples = append(bundle.Samples, sj)
	}
	rows.Close()

	// Route changes in window — same shape and filter as /api/route_changes.
	rcRows, err := s.cfg.Store.DB().QueryContext(r.Context(),
		`SELECT ttl, ts, old_ip, new_ip
		 FROM route_changes
		 WHERE probe_id = ? AND target = ? AND ts >= ? AND ts <= ?
		 ORDER BY ts ASC`,
		probeID, target, since, until,
	)
	if err != nil {
		http.Error(w, fmt.Sprintf("route_changes query: %v", err), http.StatusInternalServerError)
		return
	}
	for rcRows.Next() {
		var ttl, ts int64
		var oldIP sql.NullString
		var newIP string
		if err := rcRows.Scan(&ttl, &ts, &oldIP, &newIP); err != nil {
			rcRows.Close()
			http.Error(w, fmt.Sprintf("route_changes scan: %v", err), http.StatusInternalServerError)
			return
		}
		ch := changeJSON{TTL: int(ttl), Ts: ts, NewIP: newIP}
		if oldIP.Valid {
			v := oldIP.String
			ch.OldIP = &v
		}
		bundle.RouteChanges = append(bundle.RouteChanges, ch)
	}
	rcRows.Close()

	// Annotations in window.
	if anns, err := s.cfg.Store.ListAnnotations(r.Context(), target, since, until); err == nil {
		for _, a := range anns {
			bundle.Annotations = append(bundle.Annotations, annotationJSON{
				ID: a.ID, Target: a.Target, Ts: a.Ts, Text: a.Text, CreatedAt: a.CreatedAt,
			})
		}
	}

	// Filename embeds target + window-end for easy operator triage
	// in the Downloads folder. Replace any path-unsafe chars with
	// underscores; targets can be hostnames which already exclude
	// the worst offenders.
	safeTarget := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', ' ', '\n', '\r', '\t':
			return '_'
		}
		return r
	}, target)
	filename := fmt.Sprintf("hoptrail-%s-%d.json", safeTarget, until)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ") // human-readable; bundle sizes are modest
	_ = enc.Encode(bundle)
}

// buildPathResponse is the shared path-snapshot → JSON conversion,
// extracted in step-45 so /api/export can reuse the same shape /api/path
// returns (rDNS resolution, loss-state classification, etc.) without
// duplicating the rendering logic.
func (s *Server) buildPathResponse(ctx context.Context, target string, eng *probe.Engine, snap probe.Snapshot) pathResponse {
	resp := pathResponse{
		Target:    target,
		StartedAt: eng.StartedAt().UnixMilli(),
		HopCount:  len(snap.Hops),
		TargetTTL: int(snap.TargetTTL),
		Hops:      make([]hopJSON, 0, len(snap.Hops)),
	}

	ips := make([]string, 0, len(snap.Hops))
	for _, h := range snap.Hops {
		if h.CurrentIP.IsValid() {
			ips = append(ips, h.CurrentIP.String())
		}
	}
	hostnames, err := s.cfg.Store.LookupHostnames(ctx, ips)
	if err != nil {
		s.log.Error("export: lookup hostnames", "err", err)
		hostnames = map[string]string{}
	}

	lossWindow := make([]analysis.HopLossSnapshot, 0, len(snap.Hops))
	for _, h := range snap.Hops {
		lossWindow = append(lossWindow, analysis.HopLossSnapshot{
			TTL:  h.TTL,
			Loss: h.LossPercent,
		})
	}
	attributed := analysis.AttributedLoss(lossWindow, analysis.DefaultLossTolerance)

	for i, h := range snap.Hops {
		hop := hopJSON{
			TTL:          int(h.TTL),
			CurrentRTTms: rttToMs(h.CurrentRTT),
			AvgRTTms:     rttToMs(h.AvgRTT),
			MinRTTms:     rttToMs(h.MinRTT),
			LossPercent:  h.LossPercent,
			LossState:    lossStateFor(attributed[i]),
		}
		if h.CurrentIP.IsValid() {
			ip := h.CurrentIP.String()
			hop.CurrentIP = &ip
			if name, ok := hostnames[ip]; ok {
				hop.Hostname = &name
			}
		}
		if !h.LastResponse.IsZero() {
			ts := h.LastResponse.UnixMilli()
			hop.LastResponse = &ts
		}
		resp.Hops = append(resp.Hops, hop)
	}
	return resp
}

// handleTargetHistory returns up to `limit` recently-added target
// identifiers, newest first. The frontend populates the add-form's
// recent-targets dropdown from this. limit defaults to 10 and is
// capped to keep responses bounded for misbehaving clients.
func (s *Server) handleTargetHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, fmt.Sprintf("invalid limit: %v", err), http.StatusBadRequest)
			return
		}
		if n > 100 {
			n = 100
		}
		limit = n
	}
	targets, err := s.cfg.Store.RecentTargets(r.Context(), limit)
	if err != nil {
		s.log.Error("target_history: query failed", "err", err)
		http.Error(w, "history query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, targetsResponse{Targets: targets})
}

// ---------- /api/targets (multi-target, step-26) ----------
//
// GET returns the list of currently-monitored targets in
// supervisor-determined order, plus a parallel map of per-target
// pinger intervals (step-37). POST adds a new target. DELETE on
// /api/targets/<id> removes; PATCH on the same path updates the
// per-target interval.

type targetsResponse struct {
	Targets []string `json:"targets"`

	// IntervalsMs is a parallel map keyed by target identifier
	// (mirroring the Targets slice). Values are the active per-hop
	// pinger interval in milliseconds. Added in step-37; absent for
	// pre-step-37 clients only as an extra unread field.
	IntervalsMs map[string]int64 `json:"intervals_ms"`

	// Thresholds is a parallel map of per-target latency thresholds
	// (step-39). Per-target pair fields may be null when the operator
	// hasn't overridden — the UI then falls back to its default preset.
	Thresholds map[string]thresholdJSON `json:"thresholds"`

	// FinalHopOnly is a parallel map of per-target final-hop-only
	// flags (step-41). Defaults to false for every target.
	FinalHopOnly map[string]bool `json:"final_hop_only"`
}

type thresholdJSON struct {
	WarningMs  *int64 `json:"warning_ms"`
	CriticalMs *int64 `json:"critical_ms"`
}

func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, targetsResponse{
			Targets:      s.cfg.Supervisor.Targets(),
			IntervalsMs:  intervalsAsMs(s.cfg.Supervisor.Intervals()),
			Thresholds:   thresholdsAsJSON(s.cfg.Supervisor.Thresholds()),
			FinalHopOnly: s.cfg.Supervisor.FinalHopOnlys(),
		})
	case http.MethodPost:
		s.handleTargetsAdd(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// intervalsAsMs converts a Duration map to a unit-explicit
// milliseconds map for JSON. Empty input → non-nil empty map so the
// JSON field renders as `{}` rather than `null` (matches the
// rest of the API's "always-an-object/array" convention).
func intervalsAsMs(in map[string]time.Duration) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v.Milliseconds()
	}
	return out
}

// thresholdsAsJSON converts the supervisor's ThresholdPair map to the
// wire shape. Per-target nils stay nil so the UI can distinguish
// "operator chose X" from "operator hasn't overridden" — different
// behavior in the picker's pending state.
func thresholdsAsJSON(in map[string]ThresholdPair) map[string]thresholdJSON {
	out := make(map[string]thresholdJSON, len(in))
	for k, v := range in {
		out[k] = thresholdJSON{WarningMs: v.WarningMs, CriticalMs: v.CriticalMs}
	}
	return out
}

func (s *Server) handleTargetsAdd(w http.ResponseWriter, r *http.Request) {
	id, err := decodeTargetRequest(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.cfg.Supervisor.Add(r.Context(), id); err != nil {
		// "already monitored" is a 409 conflict. DNS resolution
		// failures (no A record, lookup error) and "not a valid
		// traceroute target" come back from the supervisor as
		// regular errors — surface those as 400 since they're
		// caused by bad operator input. Other errors (pipeline
		// build, kernel resource) surface as 500.
		msg := err.Error()
		switch {
		case strings.Contains(msg, "already monitored"):
			http.Error(w, msg, http.StatusConflict)
		case strings.Contains(msg, "resolve "):
			http.Error(w, msg, http.StatusBadRequest)
		case strings.Contains(msg, "not a valid traceroute target"),
			strings.Contains(msg, "IPv6"),
			strings.Contains(msg, "target is empty"):
			http.Error(w, msg, http.StatusBadRequest)
		default:
			s.log.Error("targets: add failed", "target", id, "err", err)
			http.Error(w, fmt.Sprintf("add failed: %v", err), http.StatusInternalServerError)
		}
		return
	}
	// Note: supervisor.Add now records both the active_targets row
	// (so Hydrate sees the target on next startup) and the
	// target_history row (so the UI dropdown surfaces it). No
	// separate persistence call needed here.

	s.log.Info("targets: added", "target", id,
		"active_count", len(s.cfg.Supervisor.Targets()))
	writeJSON(w, targetResponse{Target: id})
}

// handleTargetByPath services /api/targets/<id>. DELETE removes a
// target; PATCH updates its per-hop pinger interval (step-37). <id>
// is the operator's typed identifier — IP or hostname — URL-encoded
// (the frontend's helpers do encodeURIComponent).
func (s *Server) handleTargetByPath(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/targets/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, prefix)
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "target identifier required in path", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodDelete:
		s.handleTargetDelete(w, r, id)
	case http.MethodPatch:
		s.handleTargetPatch(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleTargetDelete(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.cfg.Supervisor.Remove(r.Context(), id); err != nil {
		if strings.Contains(err.Error(), "not monitored") {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		s.log.Error("targets: remove failed", "target", id, "err", err)
		http.Error(w, fmt.Sprintf("remove failed: %v", err), http.StatusInternalServerError)
		return
	}
	s.log.Info("targets: removed", "target", id,
		"active_count", len(s.cfg.Supervisor.Targets()))
	writeJSON(w, targetResponse{Target: id})
}

// targetPatchRequest is the body shape for PATCH /api/targets/<id>.
// All fields are optional; at least one must be present. Sending
// (warning_ms, critical_ms) as JSON null clears that override (step-39).
type targetPatchRequest struct {
	IntervalMs   *int64          `json:"interval_ms"`
	WarningMs    jsonNullableInt `json:"warning_ms"`
	CriticalMs   jsonNullableInt `json:"critical_ms"`
	FinalHopOnly *bool           `json:"final_hop_only"`
}

// jsonNullableInt distinguishes three input states for an int64 field:
//   - absent (Set=false — field wasn't in body)
//   - explicit null (Set=true, Value=nil — operator clearing the override)
//   - explicit value (Set=true, Value=&n — operator picking a number)
//
// Value-typed (not *jsonNullableInt) so the field's UnmarshalJSON
// always runs when the key is present in the JSON body — even when
// the value is null. Pointer-typed wrappers would have the decoder
// short-circuit null to a nil pointer and we'd lose the "explicit
// null" signal.
type jsonNullableInt struct {
	Set   bool
	Value *int64
}

func (n *jsonNullableInt) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Value = nil
		return nil
	}
	var v int64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}

// targetPatchResponse echoes the resulting per-target settings so
// the UI can reconcile rather than trust its local state.
type targetPatchResponse struct {
	Target       string `json:"target"`
	IntervalMs   *int64 `json:"interval_ms,omitempty"`
	WarningMs    *int64 `json:"warning_ms,omitempty"`
	CriticalMs   *int64 `json:"critical_ms,omitempty"`
	FinalHopOnly *bool  `json:"final_hop_only,omitempty"`
}

func (s *Server) handleTargetPatch(w http.ResponseWriter, r *http.Request, id string) {
	var req targetPatchRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, targetRequestMaxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	touchesInterval := req.IntervalMs != nil
	touchesThresholds := req.WarningMs.Set || req.CriticalMs.Set
	touchesFinalHopOnly := req.FinalHopOnly != nil
	if !touchesInterval && !touchesThresholds && !touchesFinalHopOnly {
		http.Error(w, "at least one of interval_ms, warning_ms, critical_ms, final_hop_only is required", http.StatusBadRequest)
		return
	}

	resp := targetPatchResponse{Target: id}

	if touchesInterval {
		if *req.IntervalMs <= 0 {
			http.Error(w, "interval_ms must be positive", http.StatusBadRequest)
			return
		}
		interval := time.Duration(*req.IntervalMs) * time.Millisecond
		if err := s.cfg.Supervisor.SetInterval(r.Context(), id, interval); err != nil {
			mapSupervisorErr(s.log, w, "set interval", id, err)
			return
		}
		s.log.Info("targets: interval set", "target", id, "interval", interval)
		resp.IntervalMs = req.IntervalMs
	}

	if touchesThresholds {
		// Both fields must be supplied together: thresholds are a
		// pair, and partial updates ("change warning, leave critical")
		// would force the UI to round-trip the current value just to
		// preserve it. Easier contract: send both, or send neither.
		if !req.WarningMs.Set || !req.CriticalMs.Set {
			http.Error(w, "warning_ms and critical_ms must be sent together", http.StatusBadRequest)
			return
		}
		if err := s.cfg.Supervisor.SetThresholds(r.Context(), id, req.WarningMs.Value, req.CriticalMs.Value); err != nil {
			mapSupervisorErr(s.log, w, "set thresholds", id, err)
			return
		}
		s.log.Info("targets: thresholds set", "target", id,
			"warning_ms", req.WarningMs.Value, "critical_ms", req.CriticalMs.Value)
		resp.WarningMs = req.WarningMs.Value
		resp.CriticalMs = req.CriticalMs.Value
	}

	if touchesFinalHopOnly {
		if err := s.cfg.Supervisor.SetFinalHopOnly(r.Context(), id, *req.FinalHopOnly); err != nil {
			mapSupervisorErr(s.log, w, "set final_hop_only", id, err)
			return
		}
		s.log.Info("targets: final_hop_only set", "target", id, "final_hop_only", *req.FinalHopOnly)
		resp.FinalHopOnly = req.FinalHopOnly
	}

	writeJSON(w, resp)
}

// mapSupervisorErr is the shared error→HTTP-status mapper for
// SetInterval / SetThresholds. Matches the existing add/swap-path
// pattern: known sentinels → 4xx, anything else → 500.
func mapSupervisorErr(log *slog.Logger, w http.ResponseWriter, action, id string, err error) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not monitored"):
		http.Error(w, msg, http.StatusNotFound)
	case strings.Contains(msg, "probe interval must be"),
		strings.Contains(msg, "warning_ms"),
		strings.Contains(msg, "critical_ms"),
		strings.Contains(msg, "must be less than"):
		http.Error(w, msg, http.StatusBadRequest)
	default:
		log.Error("targets: "+action+" failed", "target", id, "err", err)
		http.Error(w, fmt.Sprintf("%s failed: %v", action, err), http.StatusInternalServerError)
	}
}

// decodeTargetRequest parses the JSON body for both /api/target POST
// and /api/targets POST. Returns the typed target string; supervisor
// is responsible for IP vs hostname semantics (step-29 lifted the
// handler-level IP-only validation, so hostnames pass straight
// through to be resolved by Supervisor.Add / Swap).
func decodeTargetRequest(w http.ResponseWriter, r *http.Request) (string, error) {
	var req targetRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, targetRequestMaxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}
	if strings.TrimSpace(req.Target) == "" {
		return "", errors.New("target is required")
	}
	return strings.TrimSpace(req.Target), nil
}

// Compile-time guard that probe.Engine still exposes Target() — the
// handler depends on it for storage-key lookups. If a probe-package
// refactor changes the signature this stops compiling here, which is
// where we want the failure rather than at runtime.
var _ interface {
	Target() netip.Addr
} = (*probe.Engine)(nil)

// ---------- helpers ----------

// rttToMs converts a Go time.Duration into floating-point milliseconds
// suitable for JSON. Zero in, zero out — the UI treats zero RTT as
// "timeout, render as gap" by checking the ip field, not the rtt.
func rttToMs(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

// parseTimeMs reads a unix-ms timestamp from a query parameter; if the
// parameter is absent, returns the supplied default. Returns an error
// if the parameter is present but unparseable.
func parseTimeMs(r *http.Request, key string, defaultMs int64) (int64, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return defaultMs, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, errors.New("not a unix-ms integer")
	}
	return n, nil
}

// writeJSON encodes a response as JSON with appropriate headers. Errors
// during encoding are logged via the server's logger; the response
// header has already been sent so there's nothing useful to do for the
// client at that point.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// ---------- /api/tabs (step-69) ----------
//
// Multi-tab-per-target's wire surface. See
// docs/multi-tab-per-target-design.md.
//
// GET  /api/tabs            → list every tab, ordered by position
// POST /api/tabs            → create a new tab (target required;
//                             label / thresholds / copy_from optional)
// PATCH /api/tabs/order     → bulk reorder
// PATCH /api/tabs/<id>      → partial update (label / thresholds)
// DELETE /api/tabs/<id>     → remove one tab; cascades target removal
//                             if it was the last tab for that target

type tabJSON struct {
	TabID      int64   `json:"tab_id"`
	Target     string  `json:"target"`
	Label      *string `json:"label"`
	WarningMs  *int64  `json:"warning_ms"`
	CriticalMs *int64  `json:"critical_ms"`
	Position   int64   `json:"position"`
	CreatedAt  int64   `json:"created_at"`
	ProbeID    string  `json:"probe_id"` // step-96: whose measurements this tab displays
	// Step-130: inline-route-changes toggle — display state, but
	// server-persisted so it follows the operator across browsers.
	ShowRouteChanges bool `json:"show_route_changes"`
}

func tabAsJSON(t storage.Tab) tabJSON {
	return tabJSON{
		TabID:      t.TabID,
		Target:     t.Target,
		Label:      t.Label,
		WarningMs:  t.WarningMs,
		CriticalMs: t.CriticalMs,
		Position:   t.Position,
		CreatedAt:  t.CreatedAt,
		ProbeID:    t.ProbeID,

		ShowRouteChanges: t.ShowRouteChanges,
	}
}

// validateTabProbe checks a probe id destined for a tab: 'local' is
// always valid (and what "" means); 'all' is reserved; anything else
// must be a registered probe. Validated at write time so a tab can't
// point at a probe that's never existed — a registered-but-offline
// probe is fine (the tab shows its last-known data).
func (s *Server) validateTabProbe(ctx context.Context, probeID string) (string, error) {
	if probeID == "" || probeID == storage.LocalProbeID {
		return storage.LocalProbeID, nil
	}
	if probeID == "all" {
		return "", errors.New("probe_id \"all\" is reserved")
	}
	probes, err := s.cfg.Store.ListProbes(ctx)
	if err != nil {
		return "", fmt.Errorf("probes lookup: %w", err)
	}
	for _, p := range probes {
		if p.ProbeID == probeID {
			return probeID, nil
		}
	}
	return "", fmt.Errorf("probe %q is not registered", probeID)
}

type tabsResponse struct {
	Tabs []tabJSON `json:"tabs"`
}

type createTabRequest struct {
	Target     string  `json:"target"`
	Label      *string `json:"label,omitempty"`
	WarningMs  *int64  `json:"warning_ms,omitempty"`
	CriticalMs *int64  `json:"critical_ms,omitempty"`
	CopyFrom   *int64  `json:"copy_from,omitempty"`
	ProbeID    string  `json:"probe_id,omitempty"` // step-96; "" = local
}

// 16 KB is generous for a tab body (label is the only free-form field
// and it's bounded). Matches the bundle request cap shape.
const tabRequestMaxBytes = 16 * 1024

func (s *Server) handleTabs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleTabsList(w, r)
	case http.MethodPost:
		s.handleTabsCreate(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleTabsList(w http.ResponseWriter, r *http.Request) {
	tabs, err := s.cfg.Store.ListTabs(r.Context())
	if err != nil {
		s.log.Error("tabs: list failed", "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	out := make([]tabJSON, len(tabs))
	for i, t := range tabs {
		out[i] = tabAsJSON(t)
	}
	writeJSON(w, tabsResponse{Tabs: out})
}

func (s *Server) handleTabsCreate(w http.ResponseWriter, r *http.Request) {
	var req createTabRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, tabRequestMaxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid body: %v", err), http.StatusBadRequest)
		return
	}
	if req.Target == "" {
		http.Error(w, "target is required", http.StatusBadRequest)
		return
	}
	// If copy_from is set, hydrate label + thresholds from the source
	// tab unless the request explicitly overrides each one.
	if req.CopyFrom != nil {
		src, err := s.findTab(r.Context(), *req.CopyFrom)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if req.Label == nil {
			req.Label = src.Label
		}
		if req.WarningMs == nil {
			req.WarningMs = src.WarningMs
		}
		if req.CriticalMs == nil {
			req.CriticalMs = src.CriticalMs
		}
		if req.ProbeID == "" {
			req.ProbeID = src.ProbeID
		}
	}
	probeID, err := s.validateTabProbe(r.Context(), req.ProbeID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := s.cfg.Store.CreateTab(r.Context(), req.Target, req.Label, req.WarningMs, req.CriticalMs, probeID)
	if err != nil {
		// The FK from tabs.target → active_targets.target rejects when
		// the target isn't currently monitored. Map that to 400 with
		// guidance — the client should POST /api/targets first.
		if isSQLiteFKError(err) {
			http.Error(w, fmt.Sprintf("target %q is not monitored; POST /api/targets first", req.Target), http.StatusBadRequest)
			return
		}
		s.log.Error("tabs: create failed", "target", req.Target, "err", err)
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}
	tab, err := s.findTab(r.Context(), id)
	if err != nil {
		s.log.Error("tabs: post-create lookup failed", "tab_id", id, "err", err)
		http.Error(w, "create succeeded but lookup failed", http.StatusInternalServerError)
		return
	}
	s.log.Info("tabs: created", "tab_id", id, "target", req.Target)
	writeJSON(w, tabAsJSON(*tab))
}

func (s *Server) handleTabByPath(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/tabs/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, prefix)
	if suffix == "" || strings.Contains(suffix, "/") {
		http.Error(w, "tab id or 'order' required in path", http.StatusBadRequest)
		return
	}
	// Special-case: /api/tabs/order is the bulk-reorder endpoint.
	if suffix == "order" {
		if r.Method != http.MethodPatch {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleTabsReorder(w, r)
		return
	}
	id, err := strconv.ParseInt(suffix, 10, 64)
	if err != nil {
		http.Error(w, "tab id must be an integer", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		s.handleTabUpdate(w, r, id)
	case http.MethodDelete:
		s.handleTabDelete(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// tabPatchRequest mirrors targetPatchRequest's shape. Label uses
// jsonNullableString to distinguish absent / explicit null / explicit
// value (so the operator can clear the label back to NULL). Threshold
// fields use the existing jsonNullableInt for the same reason.
type tabPatchRequest struct {
	Label      jsonNullableString `json:"label"`
	WarningMs  jsonNullableInt    `json:"warning_ms"`
	CriticalMs jsonNullableInt    `json:"critical_ms"`
	// ProbeID is plain-optional (no null state — a tab always has a
	// probe; "switch to local" is probe_id:"local", not null).
	ProbeID *string `json:"probe_id,omitempty"`
	// ShowRouteChanges is plain-optional too (true/false, never null).
	ShowRouteChanges *bool `json:"show_route_changes,omitempty"`
}

// jsonNullableString is the string analogue of jsonNullableInt: three
// input states (absent / explicit null / explicit value).
type jsonNullableString struct {
	Set   bool
	Value *string
}

func (n *jsonNullableString) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Value = nil
		return nil
	}
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}

func (s *Server) handleTabUpdate(w http.ResponseWriter, r *http.Request, id int64) {
	var req tabPatchRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, tabRequestMaxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid body: %v", err), http.StatusBadRequest)
		return
	}
	if !req.Label.Set && !req.WarningMs.Set && !req.CriticalMs.Set && req.ProbeID == nil && req.ShowRouteChanges == nil {
		http.Error(w, "at least one of label, warning_ms, critical_ms, probe_id, show_route_changes is required", http.StatusBadRequest)
		return
	}
	var probeID *string
	if req.ProbeID != nil {
		validated, err := s.validateTabProbe(r.Context(), *req.ProbeID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		probeID = &validated
	}
	// Threshold pair must be sent together (matches the existing
	// target PATCH semantic so the chart never paints a half-set
	// threshold).
	if req.WarningMs.Set != req.CriticalMs.Set {
		http.Error(w, "warning_ms and critical_ms must be sent together", http.StatusBadRequest)
		return
	}
	// Validate values when present.
	if req.WarningMs.Set && req.WarningMs.Value != nil && req.CriticalMs.Value != nil {
		if *req.WarningMs.Value <= 0 || *req.CriticalMs.Value <= 0 {
			http.Error(w, "warning_ms and critical_ms must be positive", http.StatusBadRequest)
			return
		}
		if *req.WarningMs.Value >= *req.CriticalMs.Value {
			http.Error(w, "warning_ms must be less than critical_ms", http.StatusBadRequest)
			return
		}
	}

	// Map the API's nullable fields to storage's (ptr, clear) pair.
	var label *string
	clearLabel := false
	if req.Label.Set {
		if req.Label.Value == nil {
			clearLabel = true
		} else {
			label = req.Label.Value
		}
	}
	var warn, crit *int64
	clearThresholds := false
	if req.WarningMs.Set {
		if req.WarningMs.Value == nil { // both nil → clear (we already validated they're sent together)
			clearThresholds = true
		} else {
			warn = req.WarningMs.Value
			crit = req.CriticalMs.Value
		}
	}

	if err := s.cfg.Store.UpdateTab(r.Context(), id, label, clearLabel, warn, crit, clearThresholds, probeID, req.ShowRouteChanges); err != nil {
		if errors.Is(err, storage.ErrTabNotFound) {
			http.Error(w, "tab not found", http.StatusNotFound)
			return
		}
		s.log.Error("tabs: update failed", "tab_id", id, "err", err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	tab, err := s.findTab(r.Context(), id)
	if err != nil {
		s.log.Error("tabs: post-update lookup failed", "tab_id", id, "err", err)
		http.Error(w, "update succeeded but lookup failed", http.StatusInternalServerError)
		return
	}
	s.log.Info("tabs: updated", "tab_id", id)
	writeJSON(w, tabAsJSON(*tab))
}

func (s *Server) handleTabDelete(w http.ResponseWriter, r *http.Request, id int64) {
	// Look up first so we know the tab's target — needed for the
	// last-tab-cascades-to-target-delete semantic.
	tab, err := s.findTab(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := s.cfg.Store.DeleteTab(r.Context(), id); err != nil {
		if errors.Is(err, storage.ErrTabNotFound) {
			http.Error(w, "tab not found", http.StatusNotFound)
			return
		}
		s.log.Error("tabs: delete failed", "tab_id", id, "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	// If that was the last tab for its target, cascade-remove the
	// target via the supervisor (which tears down the pipeline AND
	// the active_targets row — the latter cascades any remaining tabs
	// for safety but there are none).
	remaining, err := s.cfg.Store.CountTabsForTarget(r.Context(), tab.Target)
	if err != nil {
		s.log.Error("tabs: post-delete count failed", "target", tab.Target, "err", err)
		// Not a failure path for the client — the tab IS deleted.
	} else if remaining == 0 {
		if err := s.cfg.Supervisor.Remove(r.Context(), tab.Target); err != nil {
			s.log.Warn("tabs: last-tab cascade target-remove failed", "target", tab.Target, "err", err)
			// Not fatal for the response — tab is gone. Operator can
			// remove the orphan target manually if it matters.
		} else {
			s.log.Info("tabs: cascade target-remove on last tab", "target", tab.Target)
		}
	}
	s.log.Info("tabs: deleted", "tab_id", id, "target", tab.Target)
	w.WriteHeader(http.StatusNoContent)
}

type reorderRequest struct {
	Order []int64 `json:"order"`
}

func (s *Server) handleTabsReorder(w http.ResponseWriter, r *http.Request) {
	var req reorderRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, tabRequestMaxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid body: %v", err), http.StatusBadRequest)
		return
	}
	if len(req.Order) == 0 {
		http.Error(w, "order must contain at least one tab_id", http.StatusBadRequest)
		return
	}
	if err := s.cfg.Store.ReorderTabs(r.Context(), req.Order); err != nil {
		s.log.Error("tabs: reorder failed", "err", err)
		http.Error(w, "reorder failed", http.StatusInternalServerError)
		return
	}
	s.log.Info("tabs: reordered", "count", len(req.Order))
	w.WriteHeader(http.StatusNoContent)
}

// findTab returns the tab matching id, or an error suitable for a 404
// body. Stable name across the handlers so they don't each duplicate
// the loop.
func (s *Server) findTab(ctx context.Context, id int64) (*storage.Tab, error) {
	tabs, err := s.cfg.Store.ListTabs(ctx)
	if err != nil {
		return nil, err
	}
	for i := range tabs {
		if tabs[i].TabID == id {
			return &tabs[i], nil
		}
	}
	return nil, fmt.Errorf("tab %d not found", id)
}

// isSQLiteFKError sniffs for SQLite's FK violation error. mattn/go-
// sqlite3 returns these as a typed sqlite3.Error with code 19; we
// match on the message text instead of importing the driver type
// directly so the handler stays driver-agnostic at the package
// boundary.
func isSQLiteFKError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// ---------- /api/version ----------
//
// Step-85: exposes the build-time-injected version string to the UI so
// the wordmark can render it. Always 200; never errors. Empty Version
// in the config falls back to "dev" so the UI always has something
// readable.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	v := s.cfg.Version
	if v == "" {
		v = "dev"
	}
	writeJSON(w, map[string]string{"version": v})
}

// handleTargetStats (step-111) reports whether history exists for a
// target — the add-tab flow asks before creating so it can offer
// resume-vs-start-new. Across all probes by design: "seen before"
// is a target-level question.
func (s *Server) handleTargetStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "target query param required", http.StatusBadRequest)
		return
	}
	count, oldest, newest, err := s.cfg.Store.TargetStats(r.Context(), target)
	if err != nil {
		http.Error(w, fmt.Sprintf("stats: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]int64{"samples": count, "oldest_ts": oldest, "newest_ts": newest})
}

// handleTargetData (step-111) DELETE wipes a target's measurement
// history across all probes ("start new"). Annotations survive.
func (s *Server) handleTargetData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "target query param required", http.StatusBadRequest)
		return
	}
	samples, changes, err := s.cfg.Store.DeleteTargetHistory(r.Context(), target)
	if err != nil {
		http.Error(w, fmt.Sprintf("wipe: %v", err), http.StatusInternalServerError)
		return
	}
	s.log.Info("target history wiped (start-new)", "target", target, "samples", samples, "route_changes", changes)
	w.WriteHeader(http.StatusNoContent)
}

// handleRetention reports the active retention policy — how far back
// the stats go (step-97, operator request: the UI should answer that
// at a glance instead of scroll-back just going dark). GET-only for
// now; the v0.4 settings panel is slated to add PATCH when retention
// moves from yaml to an operator-editable SQLite config row.
func (s *Server) handleRetention(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		days := s.cfg.RetentionDays
		if v, ok, _ := s.cfg.Store.GetConfig(r.Context(), "retention.days"); ok {
			if n, err := strconv.Atoi(v); err == nil {
				days = n
			}
		}
		writeJSON(w, map[string]int{"retention_days": days})
	case http.MethodPatch:
		// Step-110: operator-editable via the settings panel. Stored
		// in the config table; the retention worker reads it live on
		// each hourly sweep (no restart). Range guards against both
		// fat-fingered zero (would delete everything within the hour)
		// and absurd values.
		var req struct {
			RetentionDays *int `json:"retention_days"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || req.RetentionDays == nil {
			http.Error(w, "body must be {\"retention_days\": <int>}", http.StatusBadRequest)
			return
		}
		if *req.RetentionDays < 1 || *req.RetentionDays > 3650 {
			http.Error(w, "retention_days must be 1-3650", http.StatusBadRequest)
			return
		}
		if err := s.cfg.Store.SetConfig(r.Context(), "retention.days", strconv.Itoa(*req.RetentionDays)); err != nil {
			http.Error(w, fmt.Sprintf("config write: %v", err), http.StatusInternalServerError)
			return
		}
		s.log.Info("retention: policy updated", "days", *req.RetentionDays)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
