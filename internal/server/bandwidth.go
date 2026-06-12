// v0.4 bandwidth API surface (design §"API surface"): config
// GET/PATCH, history, derate-status, and the manual-run trigger.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/preston-peterson/hoptrail/internal/bandwidth"
	"github.com/preston-peterson/hoptrail/internal/storage"
)

// BandwidthRunner is the slice of bandwidth.Runner the handlers need;
// an interface so tests can fake in-flight state.
type BandwidthRunner interface {
	RunNow() bool
	InFlight() bool
	Reconfigure(bandwidth.Config)
}

// bandwidthConfigResponse is the GET /api/bandwidth/config shape —
// every tunable, the state rows, and the capability snapshot.
type bandwidthConfigResponse struct {
	Capability bandwidth.Capability `json:"capability"`

	Enabled              bool     `json:"enabled"`
	CadenceMode          string   `json:"cadence_mode"`
	ChartWindow          string   `json:"chart_window"`
	IntervalMinutes      int      `json:"interval_minutes"`
	ScheduledTimes       []string `json:"scheduled_times"`
	Timezone             string   `json:"timezone"`
	Directions           string   `json:"directions"`
	ServerMode           string   `json:"server_mode"`
	ServerID             *int64   `json:"server_id"`
	DerateThreshold      float64  `json:"derate_threshold"`
	BaselineDays         int      `json:"baseline_days"`
	BaselineMetric       string   `json:"baseline_metric"`
	HealthCheckFloorMbps float64  `json:"health_check_floor_mbps"`
	PauseICMPDuringTest  bool     `json:"pause_icmp_during_test"`

	InstallBannerDismissedForVersion *string `json:"install_banner_dismissed_for_version"`
	DerateBannerDismissedIncidentTs  *int64  `json:"derate_banner_dismissed_incident_ts"`
	RunInFlight                      bool    `json:"run_in_flight"`
}

func (s *Server) handleBandwidthConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleBandwidthConfigGet(w, r)
	case http.MethodPatch:
		s.handleBandwidthConfigPatch(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleBandwidthConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg, err := bandwidth.LoadConfig(r.Context(), s.cfg.Store)
	if err != nil {
		http.Error(w, fmt.Sprintf("config load: %v", err), http.StatusInternalServerError)
		return
	}
	resp := bandwidthConfigResponse{
		Capability:           s.bandwidthCapability(),
		Enabled:              cfg.Enabled,
		CadenceMode:          cfg.CadenceMode,
		ChartWindow:          cfg.ChartWindow,
		IntervalMinutes:      cfg.IntervalMin,
		ScheduledTimes:       cfg.ScheduledTimes,
		Timezone:             cfg.Timezone,
		Directions:           cfg.Directions,
		ServerMode:           cfg.ServerMode,
		ServerID:             cfg.ServerID,
		DerateThreshold:      cfg.DerateThresh,
		BaselineDays:         cfg.BaselineDays,
		BaselineMetric:       cfg.BaselineMetric,
		HealthCheckFloorMbps: cfg.HealthFloor,
		PauseICMPDuringTest:  cfg.PauseICMP,
	}
	if v, ok, _ := s.cfg.Store.GetConfig(r.Context(), bandwidth.KeyInstallDismissed); ok {
		resp.InstallBannerDismissedForVersion = &v
	}
	if v, ok, _ := s.cfg.Store.GetConfig(r.Context(), bandwidth.KeyDerateDismissedTs); ok {
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
			resp.DerateBannerDismissedIncidentTs = &ts
		}
	}
	if s.cfg.BandwidthRunner != nil {
		resp.RunInFlight = s.cfg.BandwidthRunner.InFlight()
	}
	writeJSON(w, resp)
}

// bandwidthConfigPatch carries any subset of the writable keys.
// Pointer fields distinguish absent from zero.
type bandwidthConfigPatch struct {
	Enabled              *bool     `json:"enabled"`
	CadenceMode          *string   `json:"cadence_mode"`
	ChartWindow          *string   `json:"chart_window"`
	IntervalMinutes      *int      `json:"interval_minutes"`
	ScheduledTimes       *[]string `json:"scheduled_times"`
	Timezone             *string   `json:"timezone"`
	Directions           *string   `json:"directions"`
	ServerMode           *string   `json:"server_mode"`
	ServerID             *int64    `json:"server_id"`
	DerateThreshold      *float64  `json:"derate_threshold"`
	BaselineDays         *int      `json:"baseline_days"`
	BaselineMetric       *string   `json:"baseline_metric"`
	HealthCheckFloorMbps *float64  `json:"health_check_floor_mbps"`
	PauseICMPDuringTest  *bool     `json:"pause_icmp_during_test"`

	InstallBannerDismissedForVersion *string `json:"install_banner_dismissed_for_version"`
	DerateBannerDismissedIncidentTs  *int64  `json:"derate_banner_dismissed_incident_ts"`
}

func (s *Server) handleBandwidthConfigPatch(w http.ResponseWriter, r *http.Request) {
	var req bandwidthConfigPatch
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid body: %v", err), http.StatusBadRequest)
		return
	}

	// Overlay the patch on the current config, validate the COMPOSITE
	// (a valid-looking field can be invalid in combination — e.g.
	// server_mode=pinned with no server_id anywhere).
	cfg, err := bandwidth.LoadConfig(r.Context(), s.cfg.Store)
	if err != nil {
		http.Error(w, fmt.Sprintf("config load: %v", err), http.StatusInternalServerError)
		return
	}
	type rowWrite struct{ key, value string }
	var writes []rowWrite
	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
		writes = append(writes, rowWrite{bandwidth.KeyEnabled, strconv.FormatBool(*req.Enabled)})
	}
	if req.ChartWindow != nil {
		cfg.ChartWindow = *req.ChartWindow
		writes = append(writes, rowWrite{bandwidth.KeyChartWindow, *req.ChartWindow})
	}
	if req.CadenceMode != nil {
		cfg.CadenceMode = *req.CadenceMode
		writes = append(writes, rowWrite{bandwidth.KeyCadenceMode, *req.CadenceMode})
	}
	if req.IntervalMinutes != nil {
		cfg.IntervalMin = *req.IntervalMinutes
		writes = append(writes, rowWrite{bandwidth.KeyIntervalMinutes, strconv.Itoa(*req.IntervalMinutes)})
	}
	if req.ScheduledTimes != nil {
		cfg.ScheduledTimes = *req.ScheduledTimes
		b, _ := json.Marshal(*req.ScheduledTimes)
		writes = append(writes, rowWrite{bandwidth.KeyScheduledTimes, string(b)})
	}
	if req.Timezone != nil {
		cfg.Timezone = *req.Timezone
		writes = append(writes, rowWrite{bandwidth.KeyTimezone, *req.Timezone})
	}
	if req.Directions != nil {
		cfg.Directions = *req.Directions
		writes = append(writes, rowWrite{bandwidth.KeyDirections, *req.Directions})
	}
	if req.ServerMode != nil {
		cfg.ServerMode = *req.ServerMode
		writes = append(writes, rowWrite{bandwidth.KeyServerMode, *req.ServerMode})
	}
	if req.ServerID != nil {
		cfg.ServerID = req.ServerID
		writes = append(writes, rowWrite{bandwidth.KeyServerID, strconv.FormatInt(*req.ServerID, 10)})
	}
	if req.DerateThreshold != nil {
		cfg.DerateThresh = *req.DerateThreshold
		writes = append(writes, rowWrite{bandwidth.KeyDerateThreshold, strconv.FormatFloat(*req.DerateThreshold, 'f', -1, 64)})
	}
	if req.BaselineDays != nil {
		cfg.BaselineDays = *req.BaselineDays
		writes = append(writes, rowWrite{bandwidth.KeyBaselineDays, strconv.Itoa(*req.BaselineDays)})
	}
	if req.BaselineMetric != nil {
		cfg.BaselineMetric = *req.BaselineMetric
		writes = append(writes, rowWrite{bandwidth.KeyBaselineMetric, *req.BaselineMetric})
	}
	if req.HealthCheckFloorMbps != nil {
		cfg.HealthFloor = *req.HealthCheckFloorMbps
		writes = append(writes, rowWrite{bandwidth.KeyHealthFloorMbps, strconv.FormatFloat(*req.HealthCheckFloorMbps, 'f', -1, 64)})
	}
	if req.PauseICMPDuringTest != nil {
		cfg.PauseICMP = *req.PauseICMPDuringTest
		writes = append(writes, rowWrite{bandwidth.KeyPauseICMP, strconv.FormatBool(*req.PauseICMPDuringTest)})
	}
	if req.InstallBannerDismissedForVersion != nil {
		writes = append(writes, rowWrite{bandwidth.KeyInstallDismissed, *req.InstallBannerDismissedForVersion})
	}
	if req.DerateBannerDismissedIncidentTs != nil {
		writes = append(writes, rowWrite{bandwidth.KeyDerateDismissedTs, strconv.FormatInt(*req.DerateBannerDismissedIncidentTs, 10)})
	}
	if len(writes) == 0 {
		http.Error(w, "empty patch", http.StatusBadRequest)
		return
	}
	if err := cfg.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, wr := range writes {
		if err := s.cfg.Store.SetConfig(r.Context(), wr.key, wr.value); err != nil {
			http.Error(w, fmt.Sprintf("config write: %v", err), http.StatusInternalServerError)
			return
		}
	}
	if s.cfg.BandwidthRunner != nil {
		s.cfg.BandwidthRunner.Reconfigure(cfg)
	}
	s.log.Info("bandwidth: config updated", "fields", len(writes))
	w.WriteHeader(http.StatusNoContent)
}

// ---------- /api/bandwidth/history ----------

type bandwidthSampleJSON struct {
	Ts         int64   `json:"ts"`
	DownMbps   float64 `json:"down_mbps"`
	UpMbps     float64 `json:"up_mbps"`
	PingMs     float64 `json:"ping_ms"`
	DurationMs int64   `json:"duration_ms"` // step-105: latency-chart test-window markers
	ServerName *string `json:"server_name"`
	Ok         bool    `json:"ok"`
	Error      *string `json:"error"`
	DerateFlag bool    `json:"derate_flag"`
}

func (s *Server) handleBandwidthHistory(w http.ResponseWriter, r *http.Request) {
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
	// Default window: 7 days — at 1-6 tests/day that's a small payload
	// and matches the default baseline window.
	since, err := parseTimeMs(r, "since", now.Add(-7*24*time.Hour).UnixMilli())
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid since: %v", err), http.StatusBadRequest)
		return
	}
	// bucket_ms accepted for wire-compat with the design but unused —
	// the table holds a handful of rows per day (doc §Schema note).
	samples, err := s.cfg.Store.ListBandwidthSamples(r.Context(), since, until)
	if err != nil {
		http.Error(w, fmt.Sprintf("query: %v", err), http.StatusInternalServerError)
		return
	}
	out := struct {
		Samples []bandwidthSampleJSON `json:"samples"`
	}{Samples: make([]bandwidthSampleJSON, 0, len(samples))}
	for _, smp := range samples {
		out.Samples = append(out.Samples, bandwidthSampleJSON{
			Ts: smp.Ts, DownMbps: smp.DownMbps, UpMbps: smp.UpMbps,
			PingMs: smp.PingMs, DurationMs: smp.DurationMs, ServerName: smp.ServerName,
			Ok: smp.Ok, Error: smp.Error, DerateFlag: smp.DerateFlag,
		})
	}
	writeJSON(w, out)
}

// ---------- /api/bandwidth/derate-status ----------

type derateStatusResponse struct {
	Derated  bool                 `json:"derated"`
	LastTest *bandwidthSampleJSON `json:"last_test"`
	Baseline *struct {
		DownMbps   float64 `json:"down_mbps"`
		UpMbps     float64 `json:"up_mbps"`
		ComputedAt int64   `json:"computed_at"`
		N          int     `json:"n"`
	} `json:"baseline"`
	Since       *int64 `json:"since"`
	DismissedTs *int64 `json:"dismissed_ts"`
}

func (s *Server) handleBandwidthDerateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := derateStatusResponse{}
	latest, err := s.cfg.Store.LatestBandwidthSample(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("query: %v", err), http.StatusInternalServerError)
		return
	}
	if latest != nil {
		resp.LastTest = &bandwidthSampleJSON{
			Ts: latest.Ts, DownMbps: latest.DownMbps, UpMbps: latest.UpMbps,
			PingMs: latest.PingMs, DurationMs: latest.DurationMs, ServerName: latest.ServerName,
			Ok: latest.Ok, Error: latest.Error, DerateFlag: latest.DerateFlag,
		}
		resp.Derated = latest.Ok && latest.DerateFlag
	}
	cfg, err := bandwidth.LoadConfig(r.Context(), s.cfg.Store)
	if err == nil && latest != nil {
		if base, berr := s.cfg.Store.ComputeBandwidthBaseline(r.Context(), cfg.BaselineMetric, cfg.BaselineDays, cfg.HealthFloor, latest.Ts, bandwidth.MinBaselineSamples); berr == nil && base != nil {
			resp.Baseline = &struct {
				DownMbps   float64 `json:"down_mbps"`
				UpMbps     float64 `json:"up_mbps"`
				ComputedAt int64   `json:"computed_at"`
				N          int     `json:"n"`
			}{base.DownMbps, base.UpMbps, base.ComputedAt, base.N}
		}
	}
	if resp.Derated {
		if since, err := derateIncidentStart(r.Context(), s.cfg.Store); err == nil && since > 0 {
			resp.Since = &since
		}
	}
	if v, ok, _ := s.cfg.Store.GetConfig(r.Context(), bandwidth.KeyDerateDismissedTs); ok {
		if ts, perr := strconv.ParseInt(v, 10, 64); perr == nil {
			resp.DismissedTs = &ts
		}
	}
	writeJSON(w, resp)
}

// derateIncidentStart returns the ts of the first flagged sample in
// the CURRENT consecutive derate run (i.e. after the most recent
// healthy successful test).
func derateIncidentStart(ctx context.Context, store *storage.Store) (int64, error) {
	var since int64
	err := store.DB().QueryRowContext(ctx,
		`SELECT COALESCE(MIN(ts), 0) FROM bandwidth_samples
		  WHERE derate_flag = 1
		    AND ts > COALESCE((SELECT MAX(ts) FROM bandwidth_samples WHERE derate_flag = 0 AND ok = 1), 0)`,
	).Scan(&since)
	return since, err
}

// ---------- POST /api/bandwidth/run ----------

func (s *Server) handleBandwidthRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.BandwidthRunner == nil {
		http.Error(w, "bandwidth engine not running", http.StatusServiceUnavailable)
		return
	}
	if cap := s.bandwidthCapability(); !cap.Available {
		http.Error(w, "speedtest CLI not available: "+cap.Error, http.StatusServiceUnavailable)
		return
	}
	if !s.cfg.BandwidthRunner.RunNow() {
		http.Error(w, "a test is already in flight", http.StatusConflict)
		return
	}
	s.log.Info("bandwidth: manual test triggered")
	w.WriteHeader(http.StatusAccepted)
}

// bandwidthCapability resolves the capability getter, defaulting to
// "unavailable" when the daemon was wired without one (tests).
func (s *Server) bandwidthCapability() bandwidth.Capability {
	if s.cfg.BandwidthCapability == nil {
		return bandwidth.Capability{Available: false, Error: "capability detection not configured"}
	}
	return s.cfg.BandwidthCapability()
}
