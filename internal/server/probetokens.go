// UI-facing probe management endpoints (step-120, design §5.2):
// minting and revoking ingest bearer tokens, and forgetting registered
// probes — the no-yaml-no-restart replacement for the v0.3
// token-gen-and-edit flow. The ingest-side consumption of these tokens
// lives in ingest.go (authAgent).

package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/preston-peterson/hoptrail/internal/alert"
	"github.com/preston-peterson/hoptrail/internal/release"
	"github.com/preston-peterson/hoptrail/internal/storage"
)

// probeTokenBytes mirrors cmd/hoptrail's token entropy: 32 bytes /
// 256 bits, base64url → 43 chars. Kept as a separate const (rather
// than importing cmd) to preserve the one-way package dependency.
const probeTokenBytes = 32

// generateProbeToken returns a new opaque bearer token from r
// (crypto/rand.Reader in production; injectable for tests).
func generateProbeToken(r io.Reader) (string, error) {
	buf := make([]byte, probeTokenBytes)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("probe token: read entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// tokenPrefix returns the loggable/listable first 4 chars of a token.
func tokenPrefix(token string) string {
	if len(token) > 4 {
		return token[:4]
	}
	return token
}

// ---------- /api/probe-tokens ----------

type probeTokenJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	TokenPrefix string `json:"token_prefix"`
	CreatedAt   int64  `json:"created_at"`
	LastUsedAt  *int64 `json:"last_used_at"`
}

type probeTokensResponse struct {
	Tokens []probeTokenJSON `json:"tokens"`
}

type createProbeTokenRequest struct {
	Name string `json:"name"`
}

// createProbeTokenResponse is the ONLY place the full token crosses
// the wire — the list endpoint exposes just the prefix. Lose it and
// the recovery is revoke + re-add, which the UI says out loud.
type createProbeTokenResponse struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

func (s *Server) handleProbeTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tokens, err := s.cfg.Store.ListProbeTokens(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("probe tokens: %v", err), http.StatusInternalServerError)
			return
		}
		resp := probeTokensResponse{Tokens: []probeTokenJSON{}}
		for _, t := range tokens {
			resp.Tokens = append(resp.Tokens, probeTokenJSON{
				ID:          t.ID,
				Name:        t.Name,
				TokenPrefix: tokenPrefix(t.Token),
				CreatedAt:   t.CreatedAt,
				LastUsedAt:  t.LastUsedAt,
			})
		}
		writeJSON(w, resp)

	case http.MethodPost:
		var req createProbeTokenRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
		// The name is the intended probe_id — same shape rules,
		// including the reserved names, so the one-liner the UI builds
		// from it can't mint an unusable identity.
		if err := validateProbeID(req.Name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		token, err := generateProbeToken(rand.Reader)
		if err != nil {
			http.Error(w, fmt.Sprintf("token generation: %v", err), http.StatusInternalServerError)
			return
		}
		id, err := s.cfg.Store.InsertProbeToken(r.Context(), token, req.Name, time.Now())
		if err != nil {
			http.Error(w, fmt.Sprintf("token store: %v", err), http.StatusInternalServerError)
			return
		}
		s.log.Info("probe token created", "id", id, "name", req.Name, "token_prefix", tokenPrefix(token))
		writeJSON(w, createProbeTokenResponse{ID: id, Name: req.Name, Token: token})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleProbeTokenByPath serves DELETE /api/probe-tokens/{id} —
// revocation. Takes effect on the probe's next request (it 401s,
// stops sending, and spills to its local buffer — the designed
// behavior for a decommissioned token).
func (s *Server) handleProbeTokenByPath(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/probe-tokens/"
	suffix := strings.TrimPrefix(r.URL.Path, prefix)
	if suffix == "" || strings.Contains(suffix, "/") {
		http.Error(w, "token id required in path", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(suffix, 10, 64)
	if err != nil {
		http.Error(w, "token id must be an integer", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	found, err := s.cfg.Store.DeleteProbeToken(r.Context(), id)
	if err != nil {
		http.Error(w, fmt.Sprintf("token delete: %v", err), http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "no such token", http.StatusNotFound)
		return
	}
	s.log.Info("probe token revoked", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- /api/probes/{probe_id} and /api/probes/{probe_id}/update ----------

// handleProbeByPath dispatches per-probe operations:
//
//	DELETE /api/probes/{id}          — forget the probe (step-121)
//	PATCH  /api/probes/{id}          — {pin: bool} fleet-update opt-out (#22)
//	POST   /api/probes/{id}/update   — command a central-driven update (#22)
//	DELETE /api/probes/{id}/update   — cancel a still-pending command (#22)
func (s *Server) handleProbeByPath(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/probes/"
	suffix := strings.TrimPrefix(r.URL.Path, prefix)
	if id, found := strings.CutSuffix(suffix, "/update"); found {
		s.handleProbeUpdateOp(w, r, id)
		return
	}
	if suffix == "" || strings.Contains(suffix, "/") {
		http.Error(w, "probe_id required in path", http.StatusBadRequest)
		return
	}
	if suffix == storage.LocalProbeID || suffix == "all" {
		http.Error(w, fmt.Sprintf("probe %q cannot be modified", suffix), http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodDelete:
		// Forget: probes row + path snapshots gone, tabs pointed at it
		// reset to local. Samples age out via retention. A probe whose
		// token is still valid will simply re-register on its next
		// heartbeat — the UI warns to revoke first.
		found, err := s.cfg.Store.DeleteProbe(r.Context(), suffix)
		if err != nil {
			http.Error(w, fmt.Sprintf("probe delete: %v", err), http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, "no such probe", http.StatusNotFound)
			return
		}
		// Any in-flight update command dies with the registration —
		// and so does any active probe_offline incident, which would
		// otherwise be orphaned forever (step-195).
		_ = s.cfg.Store.ClearProbeUpdate(r.Context(), suffix)
		s.clearSubjectAlertStates(r.Context(), suffix, alert.EventProbeOffline)
		s.log.Info("probe forgotten", "probe_id", suffix)
		w.WriteHeader(http.StatusNoContent)

	case http.MethodPatch:
		var req struct {
			Pin *bool `json:"pin"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Pin == nil {
			http.Error(w, "body must be {\"pin\": bool}", http.StatusBadRequest)
			return
		}
		if err := s.cfg.Store.SetProbePin(r.Context(), suffix, *req.Pin); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		s.log.Info("probe pin set", "probe_id", suffix, "pin", *req.Pin)
		writeJSON(w, map[string]bool{"pin": *req.Pin})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleProbeUpdateOp commands (POST) or cancels (DELETE) one probe's
// update.
func (s *Server) handleProbeUpdateOp(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" || id == storage.LocalProbeID || id == "all" {
		http.Error(w, "invalid probe_id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPost:
		p, err := s.probeByID(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if p == nil {
			http.Error(w, "no such probe", http.StatusNotFound)
			return
		}
		if code, err := s.commandUpdateFor(r.Context(), *p); err != nil {
			http.Error(w, err.Error(), code)
			return
		}
		pu, _ := s.cfg.Store.GetProbeUpdate(r.Context(), id)
		writeJSON(w, pu)

	case http.MethodDelete:
		pu, err := s.cfg.Store.GetProbeUpdate(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if pu == nil {
			http.Error(w, "no update to cancel", http.StatusNotFound)
			return
		}
		// Only a never-acknowledged command can be safely canceled —
		// once the probe is applying, the train has left.
		if pu.State == storage.ProbeUpdateApplying {
			http.Error(w, "probe is already applying — too late to cancel", http.StatusConflict)
			return
		}
		if err := s.cfg.Store.ClearProbeUpdate(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.log.Info("probe-update: canceled", "probe_id", id)
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---------- POST /api/probes/update-all · GET status ----------

// handleProbesUpdateAll starts (POST) or reports (GET) the sequential
// fleet rollout: outdated, online, arch-known, unpinned probes, one
// at a time, stop on first failure.
func (s *Server) handleProbesUpdateAll(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.rolloutStatus())

	case http.MethodPost:
		s.rollout.mu.Lock()
		if s.rollout.running {
			s.rollout.mu.Unlock()
			http.Error(w, "a rollout is already running", http.StatusConflict)
			return
		}

		probes, err := s.cfg.Store.ListProbes(r.Context())
		if err != nil {
			s.rollout.mu.Unlock()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		central := s.versionString()
		now := time.Now()
		candidates := []storage.Probe{}
		for _, p := range probes {
			if p.Pin || p.Arch == nil || p.Version == nil {
				continue
			}
			if now.Sub(time.UnixMilli(p.LastSeenAt)) >= probeOfflineAfter {
				continue
			}
			if release.Newer(central, *p.Version) {
				candidates = append(candidates, p)
			}
		}
		if len(candidates) == 0 {
			s.rollout.mu.Unlock()
			http.Error(w, "no updatable probes (outdated + online + unpinned + arch known)", http.StatusConflict)
			return
		}
		s.rollout.running = true
		s.rollout.current = ""
		s.rollout.done = nil
		s.rollout.failed = ""
		s.rollout.mu.Unlock()

		s.log.Info("probe-update: rollout started", "candidates", len(candidates))
		go s.runRollout(candidates)
		writeJSON(w, s.rolloutStatus())

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
