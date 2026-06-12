// Central-driven probe updates (step-168, #22): the Update button on
// an outdated probe DOES the update. The central downloads the
// release binary for the probe's arch (sha256-verified against GitHub,
// the #11 machinery), caches it, and rides an update command on the
// probe's next heartbeat reply; the probe fetches the binary back over
// the same authenticated channel, re-verifies the sha256 itself, and
// applies in-process (the agent-side mirror of the central's own UI
// apply path — see internal/agent/updater.go for why update.sh can't
// be used from inside the unit's cgroup).
//
// Lifecycle (probe_updates row): pending → applying → applied|failed.
// Success is detected by the probe's heartbeat arriving with the
// target version; a probe that received the command twice without
// acknowledging is too old to understand it; an applying probe silent
// for applyTimeout has failed (its rollback heartbeat, arriving with
// the OLD version, also lands here as a failure with a clear story).
// Failures append to alert_history so the bell tells the tale.

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/preston-peterson/hoptrail/internal/release"
	"github.com/preston-peterson/hoptrail/internal/storage"
)

// applyTimeout is how long an acknowledged (applying) update may stay
// silent before the central declares it failed. Covers download +
// swap + restart + first heartbeat with generous slack.
const applyTimeout = 5 * time.Minute

// maxUpdateDeliveries: a pending command delivered this many times
// without the probe acknowledging means the probe predates the
// feature.
const maxUpdateDeliveries = 2

var (
	updateVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	updateArchRe    = regexp.MustCompile(`^(amd64|arm64)$`)
)

// releaseCacheDir is where central-downloaded probe binaries live.
func (s *Server) releaseCacheDir(version, arch string) string {
	return filepath.Join(s.installDir(), "release-cache", version, arch)
}

// heartbeatUpdateFor is the lifecycle brain on the heartbeat path:
// detects success (probe now reports the target version), detects
// too-old probes (delivered twice, never acknowledged), times out
// silent applies, and otherwise returns the command to ride this
// reply.
func (s *Server) heartbeatUpdateFor(ctx context.Context, probeID, reportedVersion string, now time.Time) *heartbeatUpdateCommand {
	pu, err := s.cfg.Store.GetProbeUpdate(ctx, probeID)
	if err != nil {
		s.log.Error("probe-update: lookup failed", "probe_id", probeID, "err", err)
		return nil
	}
	if pu == nil || pu.State == storage.ProbeUpdateApplied || pu.State == storage.ProbeUpdateFailed {
		return nil
	}

	// Success: the probe reports the target version (base compare —
	// release builds report exactly vX.Y.Z).
	if baseVersion(reportedVersion) == pu.TargetVersion {
		if err := s.cfg.Store.SetProbeUpdateState(ctx, probeID, storage.ProbeUpdateApplied, "", now.UnixMilli()); err != nil {
			s.log.Error("probe-update: mark applied failed", "probe_id", probeID, "err", err)
		}
		s.log.Info("probe-update: applied", "probe_id", probeID, "version", pu.TargetVersion)
		return nil
	}

	switch pu.State {
	case storage.ProbeUpdateApplying:
		if now.UnixMilli()-pu.UpdatedAt > applyTimeout.Milliseconds() {
			s.failProbeUpdate(ctx, probeID,
				fmt.Sprintf("no heartbeat on v%s within %s of starting — likely rolled back or wedged; check the probe's journal", pu.TargetVersion, applyTimeout), now)
			return nil
		}
		// A heartbeat with the OLD version while applying (and inside
		// the timeout) usually means the probe restarted mid-apply or
		// rolled back — keep offering the command; the probe
		// re-acknowledges if it retries, and the timeout catches a
		// true wedge.
		return &heartbeatUpdateCommand{Version: pu.TargetVersion, SHA256: pu.SHA256, Path: updateBinaryPath(pu.TargetVersion, pu.Arch)}

	case storage.ProbeUpdatePending:
		n, err := s.cfg.Store.IncrementProbeUpdateDeliveries(ctx, probeID, now.UnixMilli())
		if err != nil {
			s.log.Error("probe-update: delivery count failed", "probe_id", probeID, "err", err)
			return nil
		}
		if n > maxUpdateDeliveries {
			s.failProbeUpdate(ctx, probeID,
				"probe never acknowledged the command — its version predates central-driven updates; update it manually once (Probes → manual instructions)", now)
			return nil
		}
		return &heartbeatUpdateCommand{Version: pu.TargetVersion, SHA256: pu.SHA256, Path: updateBinaryPath(pu.TargetVersion, pu.Arch)}
	}
	return nil
}

func updateBinaryPath(version, arch string) string {
	return fmt.Sprintf("/api/ingest/update-binary?version=%s&arch=%s", version, arch)
}

// failProbeUpdate transitions to failed and tells the alert history —
// a remote site silently stuck on an old build is exactly what the
// bell exists for.
func (s *Server) failProbeUpdate(ctx context.Context, probeID, msg string, now time.Time) {
	if err := s.cfg.Store.SetProbeUpdateState(ctx, probeID, storage.ProbeUpdateFailed, msg, now.UnixMilli()); err != nil {
		s.log.Error("probe-update: mark failed failed", "probe_id", probeID, "err", err)
		return
	}
	s.log.Warn("probe-update: failed", "probe_id", probeID, "reason", msg)
	if err := s.cfg.Store.AppendAlertHistory(ctx, storage.AlertHistoryEntry{
		Ts: now.UnixMilli(), EventType: "probe_update", Subject: probeID,
		Kind: "alert", Message: "probe update failed: " + msg,
	}); err != nil {
		s.log.Error("probe-update: history append failed", "err", err)
	}
}

// baseVersion strips the leading v and any -N-g<hash> dev suffix.
func baseVersion(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	return v
}

// ---------- GET /api/ingest/update-binary ----------

// handleIngestUpdateBinary serves a cached release binary to an
// authenticated probe. version/arch are allowlist-validated — no
// path traversal through query params.
func (s *Server) handleIngestUpdateBinary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.authAgent(w, r); !ok {
		return
	}
	version := r.URL.Query().Get("version")
	arch := r.URL.Query().Get("arch")
	if !updateVersionRe.MatchString(version) || !updateArchRe.MatchString(arch) {
		http.Error(w, "invalid version or arch", http.StatusBadRequest)
		return
	}
	path := filepath.Join(s.releaseCacheDir(version, arch), "hoptrail")
	if _, err := os.Stat(path); err != nil {
		http.Error(w, "binary not cached — re-issue the update from the central's UI", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}

// ---------- POST /api/ingest/update-status ----------

type ingestUpdateStatusRequest struct {
	ProbeID string `json:"probe_id"`
	State   string `json:"state"` // applying | failed
	Error   string `json:"error,omitempty"`
}

// handleIngestUpdateStatus receives the probe's progress reports:
// "applying" on acknowledgment, "failed" with the story when the
// probe-side apply gave up (download, verify, setcap-rollback, …).
func (s *Server) handleIngestUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.authAgent(w, r); !ok {
		return
	}
	var req ingestUpdateStatusRequest
	if err := decodeIngestBody(w, r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateProbeID(req.ProbeID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now()
	switch req.State {
	case storage.ProbeUpdateApplying:
		if err := s.cfg.Store.SetProbeUpdateState(r.Context(), req.ProbeID, storage.ProbeUpdateApplying, "", now.UnixMilli()); err != nil {
			http.Error(w, "no update in flight for this probe", http.StatusConflict)
			return
		}
		s.log.Info("probe-update: probe acknowledged", "probe_id", req.ProbeID)
	case storage.ProbeUpdateFailed:
		msg := req.Error
		if msg == "" {
			msg = "probe reported failure without detail"
		}
		s.failProbeUpdate(r.Context(), req.ProbeID, msg, now)
	default:
		http.Error(w, fmt.Sprintf("state %q must be applying|failed", req.State), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------- POST /api/probes/<id>/update · DELETE (cancel) ----------

// commandUpdateFor validates and issues one update command: resolves
// the latest release, ensures the probe's arch binary is cached, and
// writes the pending row the next heartbeat picks up.
func (s *Server) commandUpdateFor(ctx context.Context, p storage.Probe) (int, error) {
	if s.cfg.ReleaseSource == nil {
		return http.StatusNotImplemented, fmt.Errorf("release updates not available in this build")
	}
	if p.Arch == nil {
		return http.StatusConflict, fmt.Errorf("probe hasn't reported its architecture — it predates central-driven updates; update it manually once")
	}
	rel, err := s.cfg.ReleaseSource.Latest(ctx)
	if err != nil {
		return http.StatusBadGateway, err
	}
	target := rel.Version()
	if !updateVersionRe.MatchString(target) {
		return http.StatusBadGateway, fmt.Errorf("release tag %q has no parseable version", rel.TagName)
	}
	if p.Version != nil && !release.Newer(target, *p.Version) {
		return http.StatusConflict, fmt.Errorf("probe is already on %s (latest release is %s)", *p.Version, target)
	}

	binPath := filepath.Join(s.releaseCacheDir(target, *p.Arch), "hoptrail")
	if _, err := os.Stat(binPath); err != nil {
		if err := s.cfg.ReleaseSource.DownloadBinary(ctx, rel, *p.Arch, binPath); err != nil {
			return http.StatusBadGateway, err
		}
	}
	sum, err := fileSHA256(binPath)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("hash cached binary: %w", err)
	}

	now := time.Now().UnixMilli()
	if err := s.cfg.Store.CommandProbeUpdate(ctx, storage.ProbeUpdate{
		ProbeID: p.ProbeID, TargetVersion: target, Arch: *p.Arch,
		SHA256: sum, RequestedAt: now,
	}); err != nil {
		return http.StatusInternalServerError, err
	}
	s.log.Info("probe-update: commanded", "probe_id", p.ProbeID, "target", target, "arch", *p.Arch)
	return http.StatusOK, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Server) probeByID(ctx context.Context, id string) (*storage.Probe, error) {
	probes, err := s.cfg.Store.ListProbes(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range probes {
		if p.ProbeID == id {
			return &p, nil
		}
	}
	return nil, nil
}

// ---------- update-all (sequential rollout) ----------

// rolloutState is in-memory only: a central restart mid-rollout
// abandons the remaining probes (already-commanded ones finish on
// their own) — re-click rather than carry a persistent rollout
// machine. Same shape as the install-button watchers.
type rolloutState struct {
	mu      sync.Mutex
	running bool
	current string
	done    []string
	failed  string // probe_id that stopped the rollout, "" if none
}

func (s *Server) rolloutStatus() map[string]any {
	s.rollout.mu.Lock()
	defer s.rollout.mu.Unlock()
	return map[string]any{
		"running": s.rollout.running,
		"current": s.rollout.current,
		"done":    append([]string{}, s.rollout.done...),
		"failed":  s.rollout.failed,
	}
}

// runRollout updates each candidate in turn, waiting for a terminal
// state (or applyTimeout + slack) before the next — a failure stops
// the line so a bad build never ships fleet-wide.
func (s *Server) runRollout(candidates []storage.Probe) {
	defer func() {
		s.rollout.mu.Lock()
		s.rollout.running = false
		s.rollout.current = ""
		s.rollout.mu.Unlock()
	}()
	ctx := context.Background()
	for _, p := range candidates {
		s.rollout.mu.Lock()
		s.rollout.current = p.ProbeID
		s.rollout.mu.Unlock()

		if code, err := s.commandUpdateFor(ctx, p); err != nil {
			s.log.Warn("probe-update: rollout command failed", "probe_id", p.ProbeID, "code", code, "err", err)
			s.rollout.mu.Lock()
			s.rollout.failed = p.ProbeID
			s.rollout.mu.Unlock()
			return
		}

		// Poll until terminal. Ceiling: command delivery (≤2
		// heartbeats) + applyTimeout + slack.
		deadline := time.Now().Add(3*time.Minute + applyTimeout)
		for {
			time.Sleep(5 * time.Second)
			pu, err := s.cfg.Store.GetProbeUpdate(ctx, p.ProbeID)
			if err != nil || pu == nil {
				continue
			}
			if pu.State == storage.ProbeUpdateApplied {
				s.rollout.mu.Lock()
				s.rollout.done = append(s.rollout.done, p.ProbeID)
				s.rollout.mu.Unlock()
				break
			}
			if pu.State == storage.ProbeUpdateFailed {
				s.rollout.mu.Lock()
				s.rollout.failed = p.ProbeID
				s.rollout.mu.Unlock()
				return
			}
			if time.Now().After(deadline) {
				s.failProbeUpdate(ctx, p.ProbeID, "rollout gave up waiting — probe never reached a terminal state", time.Now())
				s.rollout.mu.Lock()
				s.rollout.failed = p.ProbeID
				s.rollout.mu.Unlock()
				return
			}
		}
	}
}
