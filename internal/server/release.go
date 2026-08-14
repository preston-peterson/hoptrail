// GitHub-release update mode (#11, post-publish): check for a newer
// release, download its arch-matched prebuilt binary with sha256
// verification, and stage it at the exact path the step-124 upload
// mode and update.sh --staged already consume — apply stays one code
// path no matter how the binary arrived.
//
// Nothing here ever applies an update on its own. The background
// checker only learns and records "an update exists"; download happens
// on an operator click; apply is the existing gated endpoint.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/preston-peterson/hoptrail/internal/release"
)

// ReleaseSource is the GitHub-facing seam, injectable for tests
// (*release.Client in production).
type ReleaseSource interface {
	Latest(ctx context.Context) (*release.Release, error)
	DownloadBinary(ctx context.Context, rel *release.Release, goarch, destPath string) error
}

// releaseChecker builds the shared check/persist helper over the
// server's store and source. Used by the manual endpoints; the
// background Checker in main.go persists to the same KV rows.
func (s *Server) releaseChecker() *release.Checker {
	return &release.Checker{
		Store: s.cfg.Store,
		Fetch: s.cfg.ReleaseSource.Latest,
		Log:   s.log,
	}
}

// ---------- wire shapes ----------

type updateCheckJSON struct {
	CheckedAt       int64  `json:"checked_at"` // unix ms
	LatestVersion   string `json:"latest_version,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	ReleaseURL      string `json:"release_url,omitempty"`
	Error           string `json:"error,omitempty"`
}

func (s *Server) lastCheckJSON(lc *release.LastCheck) *updateCheckJSON {
	if lc == nil {
		return nil
	}
	return &updateCheckJSON{
		CheckedAt:       lc.At,
		LatestVersion:   lc.LatestVersion,
		UpdateAvailable: release.Newer(lc.LatestVersion, s.versionString()),
		ReleaseURL:      lc.URL,
		Error:           lc.Err,
	}
}

// checkIntervalSetting reads the operator's cadence choice, defaulted.
func (s *Server) checkIntervalSetting(ctx context.Context) string {
	v, ok, _ := s.cfg.Store.GetConfig(ctx, release.KeyCheckInterval)
	if !ok || !release.ValidInterval(v) {
		return release.DefaultCheckInterval
	}
	return v
}

// ---------- POST /api/update/check ----------

// handleUpdateCheck contacts GitHub now (the always-available manual
// path — the interval setting only governs the background checker)
// and persists the result where the status page reads it.
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.ReleaseSource == nil {
		http.Error(w, "release checking not available in this build", http.StatusNotImplemented)
		return
	}
	lc := s.releaseChecker().CheckNow(r.Context())
	writeJSON(w, s.lastCheckJSON(&lc))
}

// ---------- POST /api/update/download ----------

// handleUpdateDownload fetches the latest release's binary for this
// box's architecture, verifies it against the release checksums, and
// stages it. The response is the same staged-info shape the upload
// path returns, so the UI's apply flow doesn't care which road the
// binary took.
func (s *Server) handleUpdateDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.ReleaseSource == nil {
		http.Error(w, "release downloads not available in this build", http.StatusNotImplemented)
		return
	}
	ctx := r.Context()
	rel, err := s.cfg.ReleaseSource.Latest(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Record what we learned — a download is also a check.
	chk := release.LastCheck{At: time.Now().UnixMilli(), LatestVersion: rel.Version(), URL: rel.HTMLURL}
	if raw, merr := json.Marshal(chk); merr == nil {
		_ = s.cfg.Store.SetConfig(ctx, release.KeyLastCheck, string(raw))
	}
	if err := s.cfg.ReleaseSource.DownloadBinary(ctx, rel, runtime.GOARCH, s.stagedBinPath()); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	staged := s.stagedInfo(ctx)
	s.log.Info("update: release binary staged", "version", staged.Version, "tag", rel.TagName)
	writeJSON(w, staged)
}

// ---------- PATCH /api/update (settings) ----------

type updateSettingsPatch struct {
	CheckInterval *string `json:"check_interval"`
}

// handleUpdateSettingsPatch persists the background-check cadence.
// Folded into the /api/update method switch by handleUpdateStatus.
func (s *Server) handleUpdateSettingsPatch(w http.ResponseWriter, r *http.Request) {
	var req updateSettingsPatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	if req.CheckInterval == nil {
		http.Error(w, "nothing to update", http.StatusBadRequest)
		return
	}
	if !release.ValidInterval(*req.CheckInterval) {
		http.Error(w, fmt.Sprintf("check_interval %q must be off|daily|weekly|monthly", *req.CheckInterval), http.StatusBadRequest)
		return
	}
	if err := s.cfg.Store.SetConfig(r.Context(), release.KeyCheckInterval, *req.CheckInterval); err != nil {
		http.Error(w, fmt.Sprintf("store interval: %v", err), http.StatusInternalServerError)
		return
	}
	s.log.Info("update: check interval set", "interval", *req.CheckInterval)
	s.writeUpdateStatus(w, r)
}
