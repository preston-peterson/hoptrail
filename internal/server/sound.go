// Audible-alert settings (#20): a master sound switch plus per-event
// toggles, stored server-side as ONE JSON config row (`alert.sound`)
// so every browser shares the same policy — which screens actually
// make noise is still up to each browser's autoplay arming, but what
// SHOULD sound is configured once.
//
// The daemon never plays anything; this is pure persisted UI policy.
// The web client watches latest_history_id and picks the tone.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/preston-peterson/hoptrail/internal/alert"
)

const keySoundConfig = "alert.sound"

// soundEventTypes are the toggleable event types — exactly the alert
// engine's set.
var soundEventTypes = []string{
	alert.EventProbeOffline,
	alert.EventTargetLoss,
	alert.EventLatency,
	alert.EventDerate,
}

type soundConfigJSON struct {
	Enabled bool            `json:"enabled"`
	Events  map[string]bool `json:"events"`
}

// defaultSoundConfig: master off (no surprise noise), every event on —
// so the first master flip gives sound everywhere, then the operator
// prunes. Per-event choices survive master flips because the master is
// just a separate bool.
func defaultSoundConfig() soundConfigJSON {
	events := make(map[string]bool, len(soundEventTypes))
	for _, t := range soundEventTypes {
		events[t] = true
	}
	return soundConfigJSON{Enabled: false, Events: events}
}

func (s *Server) loadSoundConfig(ctx context.Context) soundConfigJSON {
	out := defaultSoundConfig()
	raw, ok, err := s.cfg.Store.GetConfig(ctx, keySoundConfig)
	if err != nil || !ok {
		return out
	}
	var stored soundConfigJSON
	if json.Unmarshal([]byte(raw), &stored) != nil {
		return out
	}
	out.Enabled = stored.Enabled
	// Merge over defaults: unknown keys dropped, missing keys keep
	// their default — a release adding an event type Just Works.
	for _, t := range soundEventTypes {
		if v, ok := stored.Events[t]; ok {
			out.Events[t] = v
		}
	}
	return out
}

// handleAlertsSound serves GET/PATCH /api/alerts/sound. PATCH is
// full-state, like the alerts config panel — the client always renders
// and submits the complete shape.
func (s *Server) handleAlertsSound(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.loadSoundConfig(r.Context()))

	case http.MethodPatch:
		var req soundConfigJSON
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
		for k := range req.Events {
			known := false
			for _, t := range soundEventTypes {
				if k == t {
					known = true
					break
				}
			}
			if !known {
				http.Error(w, fmt.Sprintf("unknown event type %q", k), http.StatusBadRequest)
				return
			}
		}
		// Merge the request over current state so a PATCH with a
		// partial events map can't silently reset omitted toggles.
		cfg := s.loadSoundConfig(r.Context())
		cfg.Enabled = req.Enabled
		for k, v := range req.Events {
			cfg.Events[k] = v
		}
		raw, _ := json.Marshal(cfg)
		if err := s.cfg.Store.SetConfig(r.Context(), keySoundConfig, string(raw)); err != nil {
			http.Error(w, fmt.Sprintf("sound store: %v", err), http.StatusInternalServerError)
			return
		}
		s.log.Info("alerts: sound config updated", "enabled", cfg.Enabled)
		writeJSON(w, cfg)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
