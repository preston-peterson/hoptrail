// Probe-side self-update (step-168, #22): act on the update command
// the heartbeat reply delivered. Download the binary from the central
// (authenticated), re-verify its sha256 against the command (end-to-
// end integrity — the central verified GitHub's checksums, we verify
// the central), then apply IN-PROCESS, mirroring the central's own UI
// apply path (internal/server/update.go): probe the staged binary
// with `version` (catches wrong-arch/glibc before anything changes),
// back up the live binary, atomic rename, re-apply cap_net_raw
// through the sudoers rule (rolling back the binary if setcap fails),
// and restart the unit via the sudoers rule.
//
// Why not exec update.sh --staged? It runs inside this process's
// cgroup: the moment it stops hoptrail-probe, systemd kills the whole
// cgroup — including the script mid-update. The in-process swap
// finishes BEFORE the restart is requested, exactly like the
// central's updater.
//
// Failure reporting: every dead end POSTs /api/ingest/update-status
// {failed, why} so the central's UI and alert history tell the story.
// If the new binary starts but can't probe, systemd's crash-loop is
// the loud signal (lesson #9) and the central's apply-timeout marks
// the update failed.

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// UpdateCommand is the heartbeat-delivered instruction.
type UpdateCommand struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Path    string `json:"path"`
}

// UpdaterHooks are the privileged/exec seams, injectable for tests
// (the suite must never touch sudo, setcap, or systemctl).
type UpdaterHooks struct {
	// VersionProbe runs `<path> version` and returns its first line.
	VersionProbe func(ctx context.Context, path string) (string, error)
	// Setcap re-applies cap_net_raw+ep via sudo -n.
	Setcap func(ctx context.Context, binPath string) ([]byte, error)
	// Restart restarts the probe unit via sudo -n, non-blocking.
	Restart func(ctx context.Context) error
	// SudoersCheck verifies the sudoers rule is readable (the cheap
	// "will sudo -n work at all" preflight, before anything changes).
	SudoersCheck func(ctx context.Context) error
}

func prodVersionProbe(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	// SECURITY (step-170, audit #11): probe the downloaded binary with
	// no inherited env before trusting it. Its sha256 is already
	// verified against the central's command at this point; the empty
	// env is defense in depth.
	cmd.Env = []string{}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("staged binary won't run: %v", err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(strings.TrimPrefix(line, "hoptrail ")), nil
}

func prodUpdaterSetcap(ctx context.Context, binPath string) ([]byte, error) {
	return exec.CommandContext(ctx, "sudo", "-n", "/usr/sbin/setcap", "cap_net_raw+ep", binPath).CombinedOutput()
}

func prodUpdaterRestart(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "sudo", "-n", "/usr/bin/systemctl", "restart", "--no-block", "hoptrail-probe").CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart hoptrail-probe: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func prodSudoersCheck(ctx context.Context) error {
	if out, err := exec.CommandContext(ctx, "sudo", "-n", "/usr/bin/cat", "/etc/sudoers.d/hoptrail").CombinedOutput(); err != nil {
		return fmt.Errorf("sudoers rule unusable (%v: %s) — re-run install.sh on this probe", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Updater applies heartbeat-delivered update commands, one at a time.
type Updater struct {
	Client     *Client
	ProbeID    string
	Version    string // this process's running version
	InstallDir string // "" → /opt/hoptrail
	Hooks      UpdaterHooks
	Log        *slog.Logger

	mu      sync.Mutex
	running bool
}

func (u *Updater) installDir() string {
	if u.InstallDir != "" {
		return u.InstallDir
	}
	return "/opt/hoptrail"
}

func (u *Updater) hooks() UpdaterHooks {
	h := u.Hooks
	if h.VersionProbe == nil {
		h.VersionProbe = prodVersionProbe
	}
	if h.Setcap == nil {
		h.Setcap = prodUpdaterSetcap
	}
	if h.Restart == nil {
		h.Restart = prodUpdaterRestart
	}
	if h.SudoersCheck == nil {
		h.SudoersCheck = prodSudoersCheck
	}
	return h
}

// OnCommand is the heartbeat loop's callback: spawn one apply attempt
// if none is in flight. Idempotent against the command re-arriving on
// every heartbeat while the apply runs.
func (u *Updater) OnCommand(cmd UpdateCommand) {
	if baseOf(u.Version) == cmd.Version {
		return // already there; central's success detection handles it
	}
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		return
	}
	u.running = true
	u.mu.Unlock()
	go func() {
		defer func() {
			u.mu.Lock()
			u.running = false
			u.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := u.apply(ctx, cmd); err != nil {
			u.Log.Error("probe update failed", "target", cmd.Version, "err", err)
			u.reportStatus(ctx, "failed", err.Error())
		}
	}()
}

func baseOf(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	return v
}

func (u *Updater) reportStatus(ctx context.Context, state, errMsg string) {
	body, _ := json.Marshal(map[string]string{
		"probe_id": u.ProbeID, "state": state, "error": errMsg,
	})
	if _, _, err := u.Client.PostJSON(ctx, "/api/ingest/update-status", body); err != nil {
		u.Log.Warn("probe update: status report failed", "err", err)
	}
}

func (u *Updater) apply(ctx context.Context, cmd UpdateCommand) error {
	hooks := u.hooks()
	u.Log.Info("probe update: starting", "target", cmd.Version, "from", u.Version)
	u.reportStatus(ctx, "applying", "")

	// Preflight the sudoers rule before anything changes on disk.
	if err := hooks.SudoersCheck(ctx); err != nil {
		return err
	}

	// Download from the central, hashing as it lands.
	stagingDir := filepath.Join(u.installDir(), "update")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return fmt.Errorf("staging dir: %w", err)
	}
	tmp, err := os.CreateTemp(stagingDir, ".self-update-*")
	if err != nil {
		return fmt.Errorf("staging temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed/applied

	hash := sha256.New()
	n, err := u.Client.DownloadFile(ctx, cmd.Path, io.MultiWriter(tmp, hash))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != cmd.SHA256 {
		return fmt.Errorf("sha256 mismatch (got %s, want %s) after %d bytes — refusing to install", got, cmd.SHA256, n)
	}
	head := make([]byte, 4)
	if f, rerr := os.Open(tmpName); rerr == nil {
		_, _ = io.ReadFull(f, head)
		f.Close()
	}
	if !bytes.Equal(head, []byte("\x7fELF")) {
		return fmt.Errorf("downloaded file is not a Linux executable")
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("chmod staged: %w", err)
	}

	// Will it even run here? Catches wrong-glibc/wrong-arch BEFORE the
	// live binary is touched.
	stagedVer, err := hooks.VersionProbe(ctx, tmpName)
	if err != nil {
		return err
	}
	if baseOf(stagedVer) != cmd.Version {
		return fmt.Errorf("staged binary reports %q, expected %s", stagedVer, cmd.Version)
	}

	// Backup, swap, setcap (rollback on failure), restart — the
	// central's apply path, mirrored.
	liveBin := filepath.Join(u.installDir(), "bin", "hoptrail")
	backupDir := filepath.Join(u.installDir(), ".backups", "self-update-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return fmt.Errorf("backup dir: %w", err)
	}
	backupPath := filepath.Join(backupDir, "hoptrail")
	if err := copyFileSync(liveBin, backupPath, 0o755); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if err := os.Rename(tmpName, liveBin); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	if out, err := hooks.Setcap(ctx, liveBin); err != nil {
		_ = copyFileSync(backupPath, liveBin, 0o755)
		return fmt.Errorf("setcap failed (binary rolled back): %v: %s", err, strings.TrimSpace(string(out)))
	}

	// The binary is now swapped and capability-stamped — the apply has
	// materially SUCCEEDED. The restart only makes it take effect, and
	// restarting hoptrail-probe terminates THIS process (systemd kills
	// the unit's cgroup, including the systemctl child) — so a
	// "signal: terminated" here is the expected outcome, not a failure.
	// Therefore: issue the restart best-effort and return nil
	// regardless. The central confirms real success from the new
	// binary's heartbeat, and catches a genuinely-failed restart (old
	// version still heartbeating) via its apply-timeout. Reporting
	// "failed" on our own death would be a false negative on an update
	// that actually worked.
	u.Log.Info("probe update: applied — restarting", "version", stagedVer, "backup", backupPath)
	if err := hooks.Restart(ctx); err != nil {
		u.Log.Warn("probe update: restart returned (typically this process being terminated by the restart) — central confirms via heartbeat", "err", err)
	}
	return nil
}

// copyFileSync copies src → dst with mode, fsyncing before close —
// the rollback safety net must actually be on disk.
func copyFileSync(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
