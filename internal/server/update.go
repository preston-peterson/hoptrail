// In-UI self-update, local-upload mode (step-124, v0.5 design §6
// C-local): the operator uploads a hoptrail binary from the settings
// panel, the daemon stages it, and apply = backup → rename into place
// → re-apply cap_net_raw via the sudoers rule → restart via the
// sudoers rule. The binary is service-user-owned, so only setcap and
// restart need root — exactly the two commands
// /etc/sudoers.d/hoptrail whitelists.
//
// Apply is gated on a sudoers drift check (the SUDOERS_VERSION
// marker): if the live rule's version doesn't match
// what this build expects, the update is blocked with a "re-run
// install.sh" message instead of half-applying and leaving a daemon
// that can't restart itself.
//
// The staged path is the same one update.sh --staged consumes
// (<install>/update/hoptrail), so an upload can also be applied over
// SSH and vice versa.

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// expectedSudoersVersion is the SUDOERS_VERSION this build's apply
// path requires. Bump only when a release adds/changes sudoers rules;
// install.sh writes the matching file.
const expectedSudoersVersion = 2

// maxUploadBytes caps an uploaded binary. The real binary is ~17 MB;
// 100 MB leaves debug-build headroom while bounding abuse.
const maxUploadBytes = 100 << 20

// sudoersVersionRe extracts the marker from /etc/sudoers.d/hoptrail.
var sudoersVersionRe = regexp.MustCompile(`(?m)^# SUDOERS_VERSION:\s*(\d+)`)

// UpdateHooks are the privileged/exec seams of the apply path,
// injectable for tests (the suite must never touch sudo, setcap, or
// systemctl). Nil hooks get the production implementations.
type UpdateHooks struct {
	// ReadSudoers returns the live /etc/sudoers.d/hoptrail content
	// (production: `sudo -n cat`, itself a whitelisted command).
	ReadSudoers func(ctx context.Context) ([]byte, error)

	// Setcap re-applies cap_net_raw+ep to the installed binary
	// (production: `sudo -n setcap ... <bin>`). Returns combined
	// output for the failure story.
	Setcap func(ctx context.Context, binPath string) ([]byte, error)

	// Restart restarts a systemd unit without blocking (production:
	// `sudo -n systemctl restart --no-block <unit>`).
	Restart func(ctx context.Context, unit string) error

	// StagedVersion interrogates a staged binary (production: exec
	// `<path> version` and return the first line).
	StagedVersion func(ctx context.Context, path string) (string, error)
}

func prodReadSudoers(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "sudo", "-n", "/usr/bin/cat", "/etc/sudoers.d/hoptrail").Output()
}

func prodSetcap(ctx context.Context, binPath string) ([]byte, error) {
	return exec.CommandContext(ctx, "sudo", "-n", "/usr/sbin/setcap", "cap_net_raw+ep", binPath).CombinedOutput()
}

func prodRestart(ctx context.Context, unit string) error {
	out, err := exec.CommandContext(ctx, "sudo", "-n", "/usr/bin/systemctl", "restart", "--no-block", unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart %s: %v: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func prodStagedVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return "", fmt.Errorf("staged binary won't run: %w", err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(strings.TrimPrefix(line, "hoptrail ")), nil
}

func (s *Server) updateHooks() UpdateHooks {
	h := UpdateHooks{
		ReadSudoers:   prodReadSudoers,
		Setcap:        prodSetcap,
		Restart:       prodRestart,
		StagedVersion: prodStagedVersion,
	}
	if s.cfg.UpdateHooks != nil {
		if s.cfg.UpdateHooks.ReadSudoers != nil {
			h.ReadSudoers = s.cfg.UpdateHooks.ReadSudoers
		}
		if s.cfg.UpdateHooks.Setcap != nil {
			h.Setcap = s.cfg.UpdateHooks.Setcap
		}
		if s.cfg.UpdateHooks.Restart != nil {
			h.Restart = s.cfg.UpdateHooks.Restart
		}
		if s.cfg.UpdateHooks.StagedVersion != nil {
			h.StagedVersion = s.cfg.UpdateHooks.StagedVersion
		}
	}
	return h
}

func (s *Server) installDir() string {
	if s.cfg.InstallDir != "" {
		return s.cfg.InstallDir
	}
	return "/opt/hoptrail"
}

func (s *Server) stagedBinPath() string { return filepath.Join(s.installDir(), "update", "hoptrail") }
func (s *Server) liveBinPath() string   { return filepath.Join(s.installDir(), "bin", "hoptrail") }

// ---------- wire shapes ----------

type updateStagedJSON struct {
	Present    bool   `json:"present"`
	Version    string `json:"version,omitempty"`
	Error      string `json:"error,omitempty"` // staged file exists but won't run
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	ModifiedAt int64  `json:"modified_at,omitempty"` // unix ms
}

type updateSudoersJSON struct {
	OK       bool   `json:"ok"`
	Version  int    `json:"version,omitempty"`
	Expected int    `json:"expected"`
	Error    string `json:"error,omitempty"`
}

type updateStatusResponse struct {
	RunningVersion string            `json:"running_version"`
	Staged         updateStagedJSON  `json:"staged"`
	Sudoers        updateSudoersJSON `json:"sudoers"`
}

// ---------- GET /api/update ----------

func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := updateStatusResponse{
		RunningVersion: s.versionString(),
		Staged:         s.stagedInfo(r.Context()),
		Sudoers:        s.sudoersCheck(r.Context()),
	}
	writeJSON(w, resp)
}

func (s *Server) versionString() string {
	if s.cfg.Version == "" {
		return "dev"
	}
	return s.cfg.Version
}

func (s *Server) stagedInfo(ctx context.Context) updateStagedJSON {
	fi, err := os.Stat(s.stagedBinPath())
	if err != nil {
		return updateStagedJSON{Present: false}
	}
	out := updateStagedJSON{
		Present:    true,
		SizeBytes:  fi.Size(),
		ModifiedAt: fi.ModTime().UnixMilli(),
	}
	ver, verr := s.updateHooks().StagedVersion(ctx, s.stagedBinPath())
	if verr != nil {
		out.Error = verr.Error()
	} else {
		out.Version = ver
	}
	return out
}

// sudoersCheck reads the live rule through the whitelisted cat and
// compares its SUDOERS_VERSION against this build's expectation. Any
// failure (no rule installed, sudo -n refused, marker missing) is a
// blocking condition with a readable reason.
func (s *Server) sudoersCheck(ctx context.Context) updateSudoersJSON {
	out := updateSudoersJSON{Expected: expectedSudoersVersion}
	content, err := s.updateHooks().ReadSudoers(ctx)
	if err != nil {
		out.Error = "cannot read /etc/sudoers.d/hoptrail — re-run install.sh to set up UI-driven updates"
		return out
	}
	m := sudoersVersionRe.FindSubmatch(content)
	if m == nil {
		out.Error = "sudoers rule has no SUDOERS_VERSION marker — re-run install.sh"
		return out
	}
	fmt.Sscanf(string(m[1]), "%d", &out.Version)
	if out.Version != expectedSudoersVersion {
		out.Error = fmt.Sprintf("sudoers rule is version %d, this build needs %d — re-run install.sh", out.Version, expectedSudoersVersion)
		return out
	}
	out.OK = true
	return out
}

// ---------- POST /api/update/upload ----------

// handleUpdateUpload stages a binary from the request body (raw
// octet-stream — the UI sends the picked file directly). Lands at the
// same path update.sh --staged consumes. Written via a temp file +
// rename so a half-received upload never sits at the staged path.
func (s *Server) handleUpdateUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.stagedBinPath()), 0o755); err != nil {
		http.Error(w, fmt.Sprintf("staging dir: %v", err), http.StatusInternalServerError)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.stagedBinPath()), ".upload-*")
	if err != nil {
		http.Error(w, fmt.Sprintf("staging temp: %v", err), http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, maxUploadBytes))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, fmt.Sprintf("upload exceeds %d bytes", maxErr.Limit), http.StatusBadRequest)
			return
		}
		http.Error(w, fmt.Sprintf("upload: %v", err), http.StatusInternalServerError)
		return
	}
	if n == 0 {
		http.Error(w, "empty upload", http.StatusBadRequest)
		return
	}
	// Cheap shape check before accepting: an ELF executable starts
	// with \x7fELF. Catches the someone-uploaded-a-zip mistake with a
	// clear message instead of a confusing version-probe failure.
	head := make([]byte, 4)
	if f, rerr := os.Open(tmpName); rerr == nil {
		_, _ = io.ReadFull(f, head)
		f.Close()
	}
	if string(head) != "\x7fELF" {
		http.Error(w, "not a Linux executable (expected a hoptrail binary, not an archive)", http.StatusBadRequest)
		return
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		http.Error(w, fmt.Sprintf("chmod staged: %v", err), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmpName, s.stagedBinPath()); err != nil {
		http.Error(w, fmt.Sprintf("stage: %v", err), http.StatusInternalServerError)
		return
	}
	s.log.Info("update: binary staged via UI", "bytes", n)
	writeJSON(w, s.stagedInfo(r.Context()))
}

// ---------- POST /api/update/apply ----------

type updateApplyResponse struct {
	Applied    bool   `json:"applied"`
	NewVersion string `json:"new_version"`
	Restarting bool   `json:"restarting"`
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	hooks := s.updateHooks()
	ctx := r.Context()

	staged := s.stagedInfo(ctx)
	if !staged.Present {
		http.Error(w, "nothing staged — upload a binary first", http.StatusConflict)
		return
	}
	if staged.Error != "" {
		http.Error(w, fmt.Sprintf("staged binary is not usable: %s", staged.Error), http.StatusConflict)
		return
	}
	if sd := s.sudoersCheck(ctx); !sd.OK {
		http.Error(w, sd.Error, http.StatusConflict)
		return
	}

	// Backup the live binary (user-owned file, plain copy). Kept under
	// .backups/ with a timestamp — same place update.sh puts its own.
	backupDir := filepath.Join(s.installDir(), ".backups", "ui-update-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		http.Error(w, fmt.Sprintf("backup dir: %v", err), http.StatusInternalServerError)
		return
	}
	backupPath := filepath.Join(backupDir, "hoptrail")
	if err := copyFile(s.liveBinPath(), backupPath, 0o755); err != nil {
		http.Error(w, fmt.Sprintf("backup: %v", err), http.StatusInternalServerError)
		return
	}

	// Swap: rename is atomic within the install dir's filesystem. The
	// running process keeps its old inode; the restart below is what
	// actually changes the running version.
	if err := os.Rename(s.stagedBinPath(), s.liveBinPath()); err != nil {
		http.Error(w, fmt.Sprintf("install: %v", err), http.StatusInternalServerError)
		return
	}

	// Capability bits died with the old inode (lesson #7) — re-apply
	// through the sudoers rule. On failure, roll the binary back: a
	// cap-less binary would start, bind HTTP, and fail every probe.
	if out, err := hooks.Setcap(ctx, s.liveBinPath()); err != nil {
		_ = copyFile(backupPath, s.liveBinPath(), 0o755)
		http.Error(w, fmt.Sprintf("setcap failed (binary rolled back): %v: %s", err, strings.TrimSpace(string(out))), http.StatusInternalServerError)
		return
	}

	s.log.Info("update: applied via UI", "new_version", staged.Version, "backup", backupPath)
	writeJSON(w, updateApplyResponse{Applied: true, NewVersion: staged.Version, Restarting: true})

	// Restart AFTER the response is on the wire. A co-located probe
	// unit shares the binary — bounce it too, best-effort (the unit
	// may simply not exist on this box).
	go func() {
		time.Sleep(700 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := hooks.Restart(ctx, "hoptrail-probe"); err != nil {
			s.log.Info("update: probe unit not restarted (likely not installed)", "err", err)
		}
		if err := hooks.Restart(ctx, "hoptrail"); err != nil {
			s.log.Error("update: self-restart failed — restart manually: sudo systemctl restart hoptrail", "err", err)
		}
	}()
}

// copyFile copies src → dst with the given mode, fsyncing before
// close — this is the rollback safety net, so it must actually be on
// disk.
func copyFile(src, dst string, mode os.FileMode) error {
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
