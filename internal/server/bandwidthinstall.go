// POST/GET /api/bandwidth/install-cli (step-123): the web UI's
// "install the speedtest CLI" button. Runs the root-owned helper
// script through the sudoers whitelist (`sudo -n` — never prompts; a
// missing /etc/sudoers.d/hoptrail fails fast with a readable error in
// the output). One install at a time; status is poll-able because a
// package install can take a minute and the UI must show progress
// (the run-now lesson from step-104: a button that "just sits there"
// reads as broken).

package server

import (
	"context"
	"net/http"
	"os/exec"
	"sync"
	"time"
)

// speedtestHelperPath is the exact path install.sh whitelists in
// /etc/sudoers.d/hoptrail. Root-owned so the service user can't swap
// the file behind the sudo rule.
const speedtestHelperPath = "/usr/local/lib/hoptrail/install-speedtest.sh"

// speedtestInstallTimeout bounds a runaway package install.
const speedtestInstallTimeout = 10 * time.Minute

// maxInstallOutput caps the stored helper output. Failures show this
// verbatim in the UI; package managers can be chatty.
const maxInstallOutput = 8 << 10

// defaultSpeedtestInstall is the production installer: the helper via
// the sudoers rule. CombinedOutput because apt/dnf interleave
// stdout/stderr and the failure story needs both.
func defaultSpeedtestInstall(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, speedtestInstallTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "sudo", "-n", speedtestHelperPath).CombinedOutput()
}

// cliInstallState is the one-at-a-time install tracker on Server.
type cliInstallState struct {
	mu      sync.Mutex
	running bool
	done    bool
	ok      bool
	output  string
}

type cliInstallStatus struct {
	// Status: idle | running | ok | failed.
	Status string `json:"status"`
	// Output is the helper's combined output, set once done. The UI
	// shows it on failure (and the manual-install pointer for the
	// unsupported-distro exit lives in it too).
	Output string `json:"output,omitempty"`
}

func (s *Server) handleBandwidthInstallCLI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.cliInstallStatus())

	case http.MethodPost:
		s.cliInstall.mu.Lock()
		if s.cliInstall.running {
			s.cliInstall.mu.Unlock()
			http.Error(w, "an install is already running", http.StatusConflict)
			return
		}
		s.cliInstall.running = true
		s.cliInstall.done = false
		s.cliInstall.ok = false
		s.cliInstall.output = ""
		s.cliInstall.mu.Unlock()

		install := s.cfg.SpeedtestInstall
		if install == nil {
			install = defaultSpeedtestInstall
		}
		// Detached from the request context: the POST returns
		// immediately and the install outlives it. Daemon shutdown
		// mid-install leaves the package manager to finish or not —
		// same as a Ctrl-C'd --add-bandwidth, and re-runnable.
		go func() {
			out, err := install(context.Background())
			if len(out) > maxInstallOutput {
				out = out[len(out)-maxInstallOutput:]
			}
			s.cliInstall.mu.Lock()
			s.cliInstall.running = false
			s.cliInstall.done = true
			s.cliInstall.ok = err == nil
			s.cliInstall.output = string(out)
			s.cliInstall.mu.Unlock()
			if err == nil {
				s.log.Info("bandwidth: speedtest CLI installed via UI")
				// Flip capability now rather than waiting out the 60s
				// re-detect — the UI's next config poll shows the
				// fields instead of the install card.
				if s.cfg.RecheckCapability != nil {
					s.cfg.RecheckCapability()
				}
			} else {
				s.log.Warn("bandwidth: speedtest CLI install failed", "err", err)
			}
		}()
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, cliInstallStatus{Status: "running"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) cliInstallStatus() cliInstallStatus {
	s.cliInstall.mu.Lock()
	defer s.cliInstall.mu.Unlock()
	st := cliInstallStatus{Status: "idle"}
	switch {
	case s.cliInstall.running:
		st.Status = "running"
	case s.cliInstall.done && s.cliInstall.ok:
		st.Status = "ok"
		st.Output = s.cliInstall.output
	case s.cliInstall.done:
		st.Status = "failed"
		st.Output = s.cliInstall.output
	}
	return st
}
