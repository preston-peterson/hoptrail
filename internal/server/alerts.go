// Alerting endpoints (step-136, design §5): config get/patch, an
// always-available test send (enable gates automation only), and a
// status line for the settings panel.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"time"

	"github.com/preston-peterson/hoptrail/internal/alert"
	"github.com/preston-peterson/hoptrail/internal/storage"
)

// alertConfigJSON is the wire shape — the full config every time (the
// panel always renders and PATCHes the complete state, like the
// bandwidth section).
type alertConfigJSON struct {
	Enabled           bool    `json:"enabled"`
	ServerURL         string  `json:"server_url"`
	Topic             string  `json:"topic"`
	// Token is WRITE-ONLY over the wire (step-170, audit #9): the GET
	// response never includes the secret (the API is unauthenticated;
	// returning it let any LAN page read it). TokenSet tells the UI a
	// token exists; on PATCH an empty Token preserves the stored one.
	Token             string  `json:"token"`
	TokenSet          bool    `json:"token_set"`
	EventProbeOffline bool    `json:"event_probe_offline"`
	EventTargetLoss   bool    `json:"event_target_loss"`
	EventLatency      bool    `json:"event_latency"`
	EventDerate       bool    `json:"event_derate"`
	EventCapacity     bool    `json:"event_capacity"`
	LossPct           float64 `json:"loss_pct"`
	SustainS          int     `json:"sustain_s"`
	LatencyLevel      string  `json:"latency_level"`
	DiskFreeFloorMB   int     `json:"disk_free_floor_mb"`
	DiskFreePctFloor  float64 `json:"disk_free_pct_floor"`
	HeadroomThreshold float64 `json:"headroom_threshold"`
	CooldownS         int     `json:"cooldown_s"`
	RateLimitPerH     int     `json:"rate_limit_per_h"`
	QuietStart        string  `json:"quiet_start"`
	QuietEnd          string  `json:"quiet_end"`
}

func alertConfigToJSON(c alert.Config) alertConfigJSON {
	return alertConfigJSON{
		// Token deliberately omitted from the response — see struct doc.
		Enabled: c.Enabled, ServerURL: c.ServerURL, Topic: c.Topic, Token: "", TokenSet: c.Token != "",
		EventProbeOffline: c.EventProbeOffline, EventTargetLoss: c.EventTargetLoss,
		EventLatency: c.EventLatency, EventDerate: c.EventDerate, EventCapacity: c.EventCapacity,
		LossPct: c.LossPct, SustainS: c.SustainS, LatencyLevel: c.LatencyLevel,
		DiskFreeFloorMB: c.DiskFreeFloorMB, DiskFreePctFloor: c.DiskFreePctFloor,
		HeadroomThreshold: c.HeadroomThreshold,
		CooldownS:         c.CooldownS, RateLimitPerH: c.RateLimitPerH,
		QuietStart: c.QuietStart, QuietEnd: c.QuietEnd,
	}
}

func alertConfigFromJSON(j alertConfigJSON) alert.Config {
	return alert.Config{
		Enabled: j.Enabled, ServerURL: j.ServerURL, Topic: j.Topic, Token: j.Token,
		EventProbeOffline: j.EventProbeOffline, EventTargetLoss: j.EventTargetLoss,
		EventLatency: j.EventLatency, EventDerate: j.EventDerate, EventCapacity: j.EventCapacity,
		LossPct: j.LossPct, SustainS: j.SustainS, LatencyLevel: j.LatencyLevel,
		DiskFreeFloorMB: j.DiskFreeFloorMB, DiskFreePctFloor: j.DiskFreePctFloor,
		HeadroomThreshold: j.HeadroomThreshold,
		CooldownS:         j.CooldownS, RateLimitPerH: j.RateLimitPerH,
		QuietStart: j.QuietStart, QuietEnd: j.QuietEnd,
	}
}

func (s *Server) handleAlertsConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, warnings, err := alert.LoadConfig(r.Context(), s.cfg.Store)
		if err != nil {
			http.Error(w, fmt.Sprintf("alerts: %v", err), http.StatusInternalServerError)
			return
		}
		for _, warn := range warnings {
			s.log.Warn("alerts: config", "warning", warn)
		}
		writeJSON(w, alertConfigToJSON(cfg))

	case http.MethodPatch:
		var req alertConfigJSON
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
		cfg := alertConfigFromJSON(req)
		// Audit #9: the GET never returns the token, so the UI PATCHes it
		// back empty on every unrelated change — preserve the stored
		// secret rather than wiping it. (Changing the token means typing
		// a new non-empty value.)
		if req.Token == "" {
			if existing, _, lerr := alert.LoadConfig(r.Context(), s.cfg.Store); lerr == nil {
				cfg.Token = existing.Token
			}
		}
		// Capacity thresholds are newer than this endpoint's first
		// clients. HeadroomThreshold has a floor of 1, so a zero value
		// reliably means "this client didn't send the capacity fields" —
		// preserve the stored trio (defaults on a fresh store) rather
		// than failing validation on the omission.
		if req.HeadroomThreshold == 0 {
			if existing, _, lerr := alert.LoadConfig(r.Context(), s.cfg.Store); lerr == nil {
				cfg.HeadroomThreshold = existing.HeadroomThreshold
				cfg.DiskFreeFloorMB = existing.DiskFreeFloorMB
				cfg.DiskFreePctFloor = existing.DiskFreePctFloor
			}
		}
		if err := cfg.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := alert.SaveConfig(r.Context(), s.cfg.Store, cfg); err != nil {
			http.Error(w, fmt.Sprintf("alerts store: %v", err), http.StatusInternalServerError)
			return
		}
		if s.cfg.AlertReconfigure != nil {
			s.cfg.AlertReconfigure(cfg)
		}
		s.log.Info("alerts: config updated", "enabled", cfg.Enabled)
		writeJSON(w, alertConfigToJSON(cfg))

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAlertsTest sends a test notification immediately — bypassing
// the queue so failure surfaces as a readable error in the panel, and
// ignoring `enabled` (manual actions never hide behind automation
// opt-ins).
func (s *Server) handleAlertsTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, _, err := alert.LoadConfig(r.Context(), s.cfg.Store)
	if err != nil {
		http.Error(w, fmt.Sprintf("alerts: %v", err), http.StatusInternalServerError)
		return
	}
	if cfg.ServerURL == "" || cfg.Topic == "" {
		http.Error(w, "set server_url and topic first", http.StatusConflict)
		return
	}
	post := s.cfg.AlertPost
	if post == nil {
		post = alert.NtfyPost
	}
	item := storage.AlertQueueItem{
		Title:    "hoptrail: test notification",
		Body:     "If you can read this, alert delivery works.",
		Priority: "default",
	}
	if err := post(r.Context(), cfg, item); err != nil {
		http.Error(w, fmt.Sprintf("delivery failed: %v", err), http.StatusBadGateway)
		return
	}
	s.log.Info("alerts: test notification delivered")
	writeJSON(w, map[string]bool{"delivered": true})
}

type alertIncidentJSON struct {
	EventType  string `json:"event_type"`
	Subject    string `json:"subject"`
	State      string `json:"state"`
	Since      int64  `json:"since"`
	NotifiedAt *int64 `json:"notified_at"`
}

type alertStatusResponse struct {
	QueueDepth      int                 `json:"queue_depth"`
	LastDeliveryAt  *int64              `json:"last_delivery_at"`
	LastDeliveryErr string              `json:"last_delivery_err,omitempty"`
	Incidents       []alertIncidentJSON `json:"incidents"`
}

func (s *Server) handleAlertsStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	depth, err := s.cfg.Store.AlertQueueDepth(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("alerts: %v", err), http.StatusInternalServerError)
		return
	}
	states, err := s.cfg.Store.ListAlertStates(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("alerts: %v", err), http.StatusInternalServerError)
		return
	}
	resp := alertStatusResponse{QueueDepth: depth, Incidents: []alertIncidentJSON{}}
	for _, st := range states {
		resp.Incidents = append(resp.Incidents, alertIncidentJSON{
			EventType: st.EventType, Subject: st.Subject, State: st.State,
			Since: st.Since, NotifiedAt: st.NotifiedAt,
		})
	}
	if s.cfg.AlertSenderStatus != nil {
		at, errStr := s.cfg.AlertSenderStatus()
		if !at.IsZero() {
			ms := at.UnixMilli()
			resp.LastDeliveryAt = &ms
			resp.LastDeliveryErr = errStr
		}
	}
	writeJSON(w, resp)
}

// AlertPostFunc mirrors alert.Poster for Config wiring without an
// import cycle concern (none exists, but keep the alias local).
type AlertPostFunc func(ctx context.Context, cfg alert.Config, item storage.AlertQueueItem) error

// handleAlertsHistory serves the append-only alert log (step-149),
// newest first.
func (s *Server) handleAlertsHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	entries, err := s.cfg.Store.ListAlertHistory(r.Context(), limit)
	if err != nil {
		http.Error(w, fmt.Sprintf("alerts history: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"entries": entries})
}

// ---------- POST/GET /api/alerts/install-ntfy ----------

// Same one-at-a-time install pattern as the speedtest button
// (step-123): the root-owned helper via the sudoers rule, 3s-pollable
// status, output surfaced on failure. Exit 3 (ntfy already present)
// arrives as a failure whose output tells the operator to point
// hoptrail at the existing server instead.

const ntfyHelperPath = "/usr/local/lib/hoptrail/install-ntfy.sh"

func defaultNtfyInstall(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, speedtestInstallTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "sudo", "-n", ntfyHelperPath).CombinedOutput()
}

func (s *Server) handleAlertsInstallNtfy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, installStatusOf(&s.ntfyInstall))

	case http.MethodPost:
		s.ntfyInstall.mu.Lock()
		if s.ntfyInstall.running {
			s.ntfyInstall.mu.Unlock()
			http.Error(w, "an install is already running", http.StatusConflict)
			return
		}
		s.ntfyInstall.running = true
		s.ntfyInstall.done = false
		s.ntfyInstall.ok = false
		s.ntfyInstall.output = ""
		s.ntfyInstall.mu.Unlock()

		install := s.cfg.NtfyInstall
		if install == nil {
			install = defaultNtfyInstall
		}
		go func() {
			out, err := install(context.Background())
			if len(out) > maxInstallOutput {
				out = out[len(out)-maxInstallOutput:]
			}
			s.ntfyInstall.mu.Lock()
			s.ntfyInstall.running = false
			s.ntfyInstall.done = true
			s.ntfyInstall.ok = err == nil
			s.ntfyInstall.output = string(out)
			s.ntfyInstall.mu.Unlock()
			if err == nil {
				s.log.Info("alerts: local ntfy installed via UI")
			} else {
				s.log.Warn("alerts: ntfy install failed", "err", err)
			}
		}()
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, cliInstallStatus{Status: "running"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func installStatusOf(st *cliInstallState) cliInstallStatus {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := cliInstallStatus{Status: "idle"}
	switch {
	case st.running:
		out.Status = "running"
	case st.done && st.ok:
		out.Status = "ok"
		out.Output = st.output
	case st.done:
		out.Status = "failed"
		out.Output = st.output
	}
	return out
}

var _ = time.Now // keep time import for the status conversion path

// clearSubjectAlertStates removes alert-state rows for a subject the
// operator is deliberately removing from monitoring (probe forgotten,
// target deleted). Without this, the state row is orphaned: the
// engine only clears an incident when it evaluates the subject and
// finds the condition false, and a subject that vanishes from the
// provider is never evaluated again — live case: a probe removed
// from the registry stayed "active" on the status card for 40+ days
// (step-195). History gets a "cleared" entry, NOT a recovery
// notification — nothing recovered, the subject stopped existing.
// The engine's per-tick orphan sweep is the backstop for paths that
// don't come through a handler; doing it here too makes the status
// card honest immediately. Best-effort: failures log and continue,
// the sweep catches anything missed.
func (s *Server) clearSubjectAlertStates(ctx context.Context, subject string, events ...string) {
	states, err := s.cfg.Store.ListAlertStates(ctx)
	if err != nil {
		s.log.Warn("alert: list states for subject clear", "subject", subject, "err", err)
		return
	}
	match := map[string]bool{}
	for _, ev := range events {
		match[ev] = true
	}
	for _, st := range states {
		if st.Subject != subject || !match[st.EventType] {
			continue
		}
		if st.State == "active" {
			if herr := s.cfg.Store.AppendAlertHistory(ctx, storage.AlertHistoryEntry{
				Ts: time.Now().UnixMilli(), EventType: st.EventType, Subject: subject, Kind: "cleared",
				Message: fmt.Sprintf("%s removed from monitoring — %s incident cleared", subject, st.EventType),
			}); herr != nil {
				s.log.Warn("alert: clear history write failed", "err", herr)
			}
		}
		if derr := s.cfg.Store.DeleteAlertState(ctx, st.EventType, st.Subject); derr != nil {
			s.log.Warn("alert: clear state delete failed", "err", derr)
			continue
		}
		s.log.Info("alert: cleared incident for removed subject", "event", st.EventType, "subject", subject)
	}
}
