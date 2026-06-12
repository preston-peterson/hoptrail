// GET /api/status (step-140): the one-call environment overview
// behind the status page — central daemon, probes, database, alerts,
// bandwidth, and update state aggregated from surfaces that already
// exist. The UI polls this lightly (30s) to drive the StatusBar
// health dot and the status overlay.

package server

import (
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/preston-peterson/hoptrail/internal/alert"
	"github.com/preston-peterson/hoptrail/internal/storage"
)

type statusProbeJSON struct {
	ProbeID    string  `json:"probe_id"`
	Online     bool    `json:"online"`
	Version    *string `json:"version"`
	LastSeenAt *int64  `json:"last_seen_at"`
	IP         *string `json:"ip,omitempty"`
}

type statusResponse struct {
	Version   string `json:"version"`
	StartedAt int64  `json:"started_at"` // unix ms
	UptimeS   int64  `json:"uptime_s"`
	Listen    string `json:"listen"`

	Engine struct {
		Targets []string `json:"targets"`
	} `json:"engine"`

	Probes []statusProbeJSON `json:"probes"`

	Database struct {
		SizeBytes     int64 `json:"size_bytes"`
		SchemaVersion int   `json:"schema_version"`
		RetentionDays int   `json:"retention_days"`
	} `json:"database"`

	Alerts struct {
		Enabled         bool   `json:"enabled"`
		Configured      bool   `json:"configured"`
		QueueDepth      int    `json:"queue_depth"`
		ActiveIncidents int    `json:"active_incidents"`
		LastDeliveryAt  *int64 `json:"last_delivery_at"`
		LastDeliveryErr string `json:"last_delivery_err,omitempty"`
		LatestHistoryID int64  `json:"latest_history_id"`
	} `json:"alerts"`

	Bandwidth struct {
		CapabilityAvailable bool   `json:"capability_available"`
		Enabled             bool   `json:"enabled"`
		LastTestTs          *int64 `json:"last_test_ts"`
		LastTestOk          bool   `json:"last_test_ok"`
		Derate              bool   `json:"derate"`
	} `json:"bandwidth"`

	Update struct {
		StagedPresent   bool   `json:"staged_present"`
		StagedVersion   string `json:"staged_version,omitempty"`
		SudoersOK       bool   `json:"sudoers_ok"`
		SudoersErr      string `json:"sudoers_err,omitempty"`
		LatestVersion   string `json:"latest_version,omitempty"`
		UpdateAvailable bool   `json:"update_available"`
		LastCheckAt     *int64 `json:"last_check_at,omitempty"`
	} `json:"update"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	now := time.Now()
	var resp statusResponse

	resp.Version = s.versionString()
	if !s.cfg.StartedAt.IsZero() {
		resp.StartedAt = s.cfg.StartedAt.UnixMilli()
		resp.UptimeS = int64(now.Sub(s.cfg.StartedAt).Seconds())
	}
	resp.Listen = s.cfg.ListenAddr

	resp.Engine.Targets = s.cfg.Supervisor.Targets()
	if resp.Engine.Targets == nil {
		resp.Engine.Targets = []string{}
	}

	// Probes — same synthesis + offline rule as /api/probes.
	version := resp.Version
	resp.Probes = []statusProbeJSON{{ProbeID: storage.LocalProbeID, Online: true, Version: &version}}
	if probes, err := s.cfg.Store.ListProbes(ctx); err == nil {
		for _, p := range probes {
			seen := p.LastSeenAt
			resp.Probes = append(resp.Probes, statusProbeJSON{
				ProbeID:    p.ProbeID,
				Online:     now.Sub(time.UnixMilli(p.LastSeenAt)) < probeOfflineAfter,
				Version:    p.Version,
				LastSeenAt: &seen,
				IP:         p.LastIP,
			})
		}
	}

	// Database — size = main file + WAL (the part that occupies disk).
	if s.cfg.DBPath != "" {
		for _, suffix := range []string{"", "-wal"} {
			if fi, err := os.Stat(s.cfg.DBPath + suffix); err == nil {
				resp.Database.SizeBytes += fi.Size()
			}
		}
	}
	if v, err := s.cfg.Store.SchemaVersion(); err == nil {
		resp.Database.SchemaVersion = v
	}
	resp.Database.RetentionDays = s.cfg.RetentionDays
	if v, ok, _ := s.cfg.Store.GetConfig(ctx, "retention.days"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			resp.Database.RetentionDays = n
		}
	}

	// Alerts.
	if cfg, _, err := alert.LoadConfig(ctx, s.cfg.Store); err == nil {
		resp.Alerts.Enabled = cfg.Enabled
		resp.Alerts.Configured = cfg.ServerURL != "" && cfg.Topic != ""
	}
	if depth, err := s.cfg.Store.AlertQueueDepth(ctx); err == nil {
		resp.Alerts.QueueDepth = depth
	}
	if states, err := s.cfg.Store.ListAlertStates(ctx); err == nil {
		for _, st := range states {
			if st.State == "active" {
				resp.Alerts.ActiveIncidents++
			}
		}
	}
	if s.cfg.AlertSenderStatus != nil {
		if at, errStr := s.cfg.AlertSenderStatus(); !at.IsZero() {
			ms := at.UnixMilli()
			resp.Alerts.LastDeliveryAt = &ms
			resp.Alerts.LastDeliveryErr = errStr
		}
	}
	if hist, err := s.cfg.Store.ListAlertHistory(ctx, 1); err == nil && len(hist) > 0 {
		resp.Alerts.LatestHistoryID = hist[0].ID
	}

	// Bandwidth.
	resp.Bandwidth.CapabilityAvailable = s.bandwidthCapability().Available
	if v, ok, _ := s.cfg.Store.GetConfig(ctx, "bandwidth.enabled"); ok && v == "true" {
		resp.Bandwidth.Enabled = true
	}
	if smp, err := s.cfg.Store.LatestBandwidthSample(ctx); err == nil && smp != nil {
		ts := smp.Ts
		resp.Bandwidth.LastTestTs = &ts
		resp.Bandwidth.LastTestOk = smp.Ok
		resp.Bandwidth.Derate = smp.Ok && smp.DerateFlag
	}

	// Update state — reuses the step-124 helpers.
	staged := s.stagedInfo(ctx)
	resp.Update.StagedPresent = staged.Present
	resp.Update.StagedVersion = staged.Version
	sd := s.sudoersCheck(ctx)
	resp.Update.SudoersOK = sd.OK
	resp.Update.SudoersErr = sd.Error
	if s.cfg.ReleaseSource != nil {
		if lc := s.lastCheckJSON(s.releaseChecker().ReadLastCheck(ctx)); lc != nil {
			resp.Update.LatestVersion = lc.LatestVersion
			resp.Update.UpdateAvailable = lc.UpdateAvailable
			at := lc.CheckedAt
			resp.Update.LastCheckAt = &at
		}
	}

	writeJSON(w, resp)
}
