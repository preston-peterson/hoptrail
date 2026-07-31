// System settings endpoints (step-125, v0.5 design §6 F): the last
// yaml-only knobs worth a UI — listen address, log level, rdns —
// surfaced in the settings panel, persisted as config KV rows that
// win over yaml at startup (retention.days precedence). Log level
// applies live (slog.LevelVar); listen and rdns need a restart, which
// the panel's restart button performs through the sudoers rule.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// systemState tracks the live-applied log level so GET reflects a
// PATCH without a round-trip through the daemon's boot path.
type systemState struct {
	mu       sync.Mutex
	logLevel string // empty until first read/patch; falls back to cfg
}

func (s *Server) effectiveLogLevel() string {
	s.system.mu.Lock()
	defer s.system.mu.Unlock()
	if s.system.logLevel != "" {
		return s.system.logLevel
	}
	if s.cfg.LogLevel != "" {
		return s.cfg.LogLevel
	}
	return "info"
}

type systemSettingsResponse struct {
	Listen             string `json:"listen"` // address the daemon is bound to NOW
	PendingListen      string `json:"pending_listen,omitempty"`
	LogLevel           string `json:"log_level"` // effective (live)
	RDNSEnabled        bool   `json:"rdns_enabled"`
	PendingRDNSEnabled *bool  `json:"pending_rdns_enabled,omitempty"`
	RestartRequired    bool   `json:"restart_required"`
}

type systemSettingsPatch struct {
	Listen      *string `json:"listen"`
	LogLevel    *string `json:"log_level"`
	RDNSEnabled *bool   `json:"rdns_enabled"`
}

// validateListen accepts the same shapes the yaml field does: a
// host:port where host may be empty (all interfaces) and port is a
// number in range. Shape-only — bindability is checked at the next
// boot, which falls back to yaml loudly rather than crash-looping.
func validateListen(v string) error {
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("listen must be host:port or :port, got %q", v)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("listen port %q must be a number in 1-65535", port)
	}
	if host != "" {
		if ip := net.ParseIP(host); ip == nil {
			return fmt.Errorf("listen host %q must be an IP address (or empty for all interfaces)", host)
		}
	}
	return nil
}

func (s *Server) handleSystemSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		resp, err := s.systemSettings(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("system settings: %v", err), http.StatusInternalServerError)
			return
		}
		writeJSON(w, resp)

	case http.MethodPatch:
		var req systemSettingsPatch
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
			return
		}

		// Validate everything before writing anything — a PATCH is
		// all-or-nothing.
		if req.Listen != nil {
			if err := validateListen(*req.Listen); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if req.LogLevel != nil {
			switch *req.LogLevel {
			case "debug", "info", "warn", "error":
			default:
				http.Error(w, fmt.Sprintf("log_level %q must be debug|info|warn|error", *req.LogLevel), http.StatusBadRequest)
				return
			}
		}

		ctx := r.Context()
		if req.Listen != nil {
			if err := s.cfg.Store.SetConfig(ctx, "server.listen", *req.Listen); err != nil {
				http.Error(w, fmt.Sprintf("store listen: %v", err), http.StatusInternalServerError)
				return
			}
			s.log.Info("system: listen override set (applies on restart)", "listen", *req.Listen)
		}
		if req.RDNSEnabled != nil {
			if err := s.cfg.Store.SetConfig(ctx, "rdns.enabled", strconv.FormatBool(*req.RDNSEnabled)); err != nil {
				http.Error(w, fmt.Sprintf("store rdns: %v", err), http.StatusInternalServerError)
				return
			}
			s.log.Info("system: rdns override set (applies on restart)", "enabled", *req.RDNSEnabled)
		}
		if req.LogLevel != nil {
			if err := s.cfg.Store.SetConfig(ctx, "log.level", *req.LogLevel); err != nil {
				http.Error(w, fmt.Sprintf("store log level: %v", err), http.StatusInternalServerError)
				return
			}
			// Live apply — the one setting that doesn't need a restart.
			if s.cfg.ApplyLogLevel != nil {
				if err := s.cfg.ApplyLogLevel(*req.LogLevel); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
			}
			s.system.mu.Lock()
			s.system.logLevel = *req.LogLevel
			s.system.mu.Unlock()
			s.log.Info("system: log level applied live", "level", *req.LogLevel)
		}

		resp, err := s.systemSettings(ctx)
		if err != nil {
			http.Error(w, fmt.Sprintf("system settings: %v", err), http.StatusInternalServerError)
			return
		}
		writeJSON(w, resp)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) systemSettings(ctx context.Context) (systemSettingsResponse, error) {
	resp := systemSettingsResponse{
		Listen:      s.cfg.ListenAddr,
		LogLevel:    s.effectiveLogLevel(),
		RDNSEnabled: s.cfg.RDNSEnabled,
	}
	if v, ok, err := s.cfg.Store.GetConfig(ctx, "server.listen"); err != nil {
		return resp, err
	} else if ok && v != s.cfg.ListenAddr {
		resp.PendingListen = v
		resp.RestartRequired = true
	}
	if v, ok, err := s.cfg.Store.GetConfig(ctx, "rdns.enabled"); err != nil {
		return resp, err
	} else if ok {
		if b, perr := strconv.ParseBool(v); perr == nil && b != s.cfg.RDNSEnabled {
			pending := b
			resp.PendingRDNSEnabled = &pending
			resp.RestartRequired = true
		}
	}
	return resp, nil
}

// ---------- POST /api/system/restart ----------

// handleSystemRestart restarts the central daemon through the sudoers
// rule, after the response is on the wire. The UI polls /api/version
// back to life. (Same deferred pattern as the update apply path.)
func (s *Server) handleSystemRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	hooks := s.updateHooks()
	s.log.Info("system: restart requested from the UI")
	writeJSON(w, map[string]bool{"restarting": true})
	go func() {
		time.Sleep(700 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := hooks.Restart(ctx, "hoptrail"); err != nil {
			s.log.Error("system: self-restart failed — restart manually: sudo systemctl restart hoptrail", "err", err)
		}
	}()
}
