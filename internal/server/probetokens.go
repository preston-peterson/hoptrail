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

// ---------- DELETE /api/probes/{probe_id} ----------

// handleProbeByPath forgets a registered probe: probes row + path
// snapshots gone, tabs pointed at it reset to local. Samples age out
// via retention. A probe whose token is still valid will simply
// re-register on its next heartbeat — the UI warns to revoke first.
func (s *Server) handleProbeByPath(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/probes/"
	suffix := strings.TrimPrefix(r.URL.Path, prefix)
	if suffix == "" || strings.Contains(suffix, "/") {
		http.Error(w, "probe_id required in path", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if suffix == storage.LocalProbeID || suffix == "all" {
		http.Error(w, fmt.Sprintf("probe %q cannot be removed", suffix), http.StatusBadRequest)
		return
	}
	found, err := s.cfg.Store.DeleteProbe(r.Context(), suffix)
	if err != nil {
		http.Error(w, fmt.Sprintf("probe delete: %v", err), http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "no such probe", http.StatusNotFound)
		return
	}
	s.log.Info("probe forgotten", "probe_id", suffix)
	w.WriteHeader(http.StatusNoContent)
}
