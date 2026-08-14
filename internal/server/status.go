// GET /api/status (step-140): the one-call environment overview
// behind the status page — central daemon, probes, database, alerts,
// bandwidth, and update state aggregated from surfaces that already
// exist. The UI polls this lightly (30s) to drive the StatusBar
// health dot and the status overlay.

package server

import (
	"net/http"
	"time"

	"github.com/preston-peterson/hoptrail/internal/alert"
	"github.com/preston-peterson/hoptrail/internal/bandwidth"
	"github.com/preston-peterson/hoptrail/internal/capacity"
	"github.com/preston-peterson/hoptrail/internal/release"
	"github.com/preston-peterson/hoptrail/internal/storage"
)

type statusProbeJSON struct {
	ProbeID    string  `json:"probe_id"`
	Online     bool    `json:"online"`
	Version    *string `json:"version"`
	LastSeenAt *int64  `json:"last_seen_at"`
	IP         *string `json:"ip,omitempty"`
	Outdated   bool    `json:"outdated"` // older release than the central (#21)
}

// statusActiveIncident is one active alert in the status response:
// what fired and on which subject, so the UI can name it instead of
// leaving the operator to hunt through the alert history.
type statusActiveIncident struct {
	Event   string `json:"event"`
	Subject string `json:"subject"`
	Since   int64  `json:"since"` // unix ms condition first seen
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
		SizeBytes      int64    `json:"size_bytes"` // main file + WAL
		WALBytes       int64    `json:"wal_bytes"`
		FreeBytes      int64    `json:"free_bytes"`  // free disk on the DB's filesystem
		TotalBytes     int64    `json:"total_bytes"` // size of that filesystem
		SchemaVersion  int      `json:"schema_version"`
		RetentionDays  int      `json:"retention_days"`
		GrowthMBPerDay float64  `json:"growth_mb_per_day"`
		DaysOfData     float64  `json:"days_of_data"`
		ProjectedBytes int64    `json:"projected_bytes"`
		HeadroomRatio  *float64 `json:"headroom_ratio"` // nil until enough growth history
		Health         string   `json:"health"`         // ok|warn|critical|unknown
	} `json:"database"`

	Alerts struct {
		Enabled         bool `json:"enabled"`
		Configured      bool `json:"configured"`
		QueueDepth      int  `json:"queue_depth"`
		ActiveIncidents int  `json:"active_incidents"`
		// Active names each active incident — the status card must
		// answer "WHICH alert", not just "how many".
		Active          []statusActiveIncident `json:"active"`
		LastDeliveryAt  *int64                 `json:"last_delivery_at"`
		LastDeliveryErr string                 `json:"last_delivery_err,omitempty"`
		LatestHistoryID int64                  `json:"latest_history_id"`
	} `json:"alerts"`

	Bandwidth struct {
		CapabilityAvailable bool     `json:"capability_available"`
		Enabled             bool     `json:"enabled"`
		LastTestTs          *int64   `json:"last_test_ts"`
		LastTestOk          bool     `json:"last_test_ok"`
		Derate              bool     `json:"derate"`
		BaselineDownMbps    *float64 `json:"baseline_down_mbps"` // nil until ≥ MinBaselineSamples
		BaselineUpMbps      *float64 `json:"baseline_up_mbps"`
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
			sp := statusProbeJSON{
				ProbeID:    p.ProbeID,
				Online:     now.Sub(time.UnixMilli(p.LastSeenAt)) < probeOfflineAfter,
				Version:    p.Version,
				LastSeenAt: &seen,
				IP:         p.LastIP,
			}
			if p.Version != nil {
				sp.Outdated = release.Newer(version, *p.Version)
			}
			resp.Probes = append(resp.Probes, sp)
		}
	}

	// Alert config drives both the alert status fields and the capacity
	// health classification (same thresholds the engine alerts on).
	acfg, _, acfgErr := alert.LoadConfig(ctx, s.cfg.Store)

	// Database + capacity — disk headroom and growth projection.
	if v, err := s.cfg.Store.SchemaVersion(); err == nil {
		resp.Database.SchemaVersion = v
	}
	days := capacity.EffectiveRetentionDays(ctx, s.cfg.Store, s.cfg.RetentionDays)
	resp.Database.RetentionDays = days
	resp.Database.Health = "unknown"
	if m, err := capacity.Measure(ctx, s.cfg.Store, s.cfg.DBPath, days); err == nil {
		resp.Database.SizeBytes = m.DBBytes + m.WALBytes
		resp.Database.WALBytes = m.WALBytes
		resp.Database.FreeBytes = m.FreeBytes
		resp.Database.TotalBytes = m.TotalBytes
		resp.Database.GrowthMBPerDay = m.MBPerDay
		resp.Database.DaysOfData = m.DaysOfData
		resp.Database.ProjectedBytes = m.ProjectedBytes
		if m.HasGrowth && m.ProjectedBytes > 0 {
			hr := m.HeadroomRatio
			resp.Database.HeadroomRatio = &hr
		}
		if acfgErr == nil {
			resp.Database.Health = m.Evaluate(capacity.Thresholds{
				FreeFloorMB:  acfg.DiskFreeFloorMB,
				FreePctFloor: acfg.DiskFreePctFloor,
				HeadroomMin:  acfg.HeadroomThreshold,
			}, false).Health
		}
	}

	// Alerts.
	if acfgErr == nil {
		resp.Alerts.Enabled = acfg.Enabled
		resp.Alerts.Configured = acfg.ServerURL != "" && acfg.Topic != ""
	}
	if depth, err := s.cfg.Store.AlertQueueDepth(ctx); err == nil {
		resp.Alerts.QueueDepth = depth
	}
	if states, err := s.cfg.Store.ListAlertStates(ctx); err == nil {
		for _, st := range states {
			if st.State == "active" {
				resp.Alerts.ActiveIncidents++
				resp.Alerts.Active = append(resp.Alerts.Active, statusActiveIncident{
					Event: st.EventType, Subject: st.Subject, Since: st.Since,
				})
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
		// Rolling baseline (the "what's normal" derate compares against) —
		// same call the derate-status endpoint and chart annotation use;
		// nil until MinBaselineSamples successful tests exist.
		if bcfg, berr := bandwidth.LoadConfig(ctx, s.cfg.Store); berr == nil {
			if base, e := s.cfg.Store.ComputeBandwidthBaseline(ctx, bcfg.BaselineMetric, bcfg.BaselineDays, bcfg.HealthFloor, smp.Ts, bandwidth.MinBaselineSamples); e == nil && base != nil {
				d, u := base.DownMbps, base.UpMbps
				resp.Bandwidth.BaselineDownMbps = &d
				resp.Bandwidth.BaselineUpMbps = &u
			}
		}
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
