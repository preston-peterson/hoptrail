package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var updaterELF = append([]byte("\x7fELF"), []byte("new-binary-payload")...)

// fakeCentral serves the update binary + records status reports.
type fakeCentral struct {
	srv *httptest.Server

	mu       sync.Mutex
	statuses []map[string]string
	binary   []byte
}

func newFakeCentral(t *testing.T) *fakeCentral {
	t.Helper()
	f := &fakeCentral{binary: updaterELF}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ingest/update-binary", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Write(f.binary)
	})
	mux.HandleFunc("/api/ingest/update-status", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]string
		json.NewDecoder(r.Body).Decode(&m)
		f.mu.Lock()
		f.statuses = append(f.statuses, m)
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCentral) lastStatus() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.statuses) == 0 {
		return nil
	}
	return f.statuses[len(f.statuses)-1]
}

func sumOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// testUpdater builds an Updater over a temp install dir with a live
// binary in place and fully injected hooks. Returns the updater, the
// install dir, and the hook-call recorder.
type hookCalls struct {
	mu        sync.Mutex
	setcapped []string
	restarted int
}

func testUpdater(t *testing.T, fc *fakeCentral, runningVersion string) (*Updater, string, *hookCalls) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "hoptrail"), []byte("\x7fELFold-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := &hookCalls{}
	u := &Updater{
		Client:     NewClient(fc.srv.URL, "tok"),
		ProbeID:    "site-east",
		Version:    runningVersion,
		InstallDir: dir,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Hooks: UpdaterHooks{
			VersionProbe: func(_ context.Context, path string) (string, error) {
				raw, err := os.ReadFile(path)
				if err != nil {
					return "", err
				}
				if string(raw) == string(updaterELF) {
					return "v0.7.0", nil
				}
				return "v0.0.0", nil
			},
			Setcap: func(_ context.Context, binPath string) ([]byte, error) {
				calls.mu.Lock()
				defer calls.mu.Unlock()
				calls.setcapped = append(calls.setcapped, binPath)
				return nil, nil
			},
			Restart: func(context.Context) error {
				calls.mu.Lock()
				defer calls.mu.Unlock()
				calls.restarted++
				return nil
			},
			SudoersCheck: func(context.Context) error { return nil },
		},
	}
	return u, dir, calls
}

func cmdFor(version string, binary []byte) UpdateCommand {
	return UpdateCommand{
		Version: version,
		SHA256:  sumOf(binary),
		Path:    "/api/ingest/update-binary?version=" + version + "&arch=amd64",
	}
}

func TestUpdater_HappyPath(t *testing.T) {
	fc := newFakeCentral(t)
	u, dir, calls := testUpdater(t, fc, "v0.6.1")

	if err := u.apply(context.Background(), cmdFor("0.7.0", updaterELF)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	live, err := os.ReadFile(filepath.Join(dir, "bin", "hoptrail"))
	if err != nil {
		t.Fatal(err)
	}
	if string(live) != string(updaterELF) {
		t.Error("live binary was not replaced")
	}
	calls.mu.Lock()
	if len(calls.setcapped) != 1 || calls.restarted != 1 {
		t.Errorf("setcap=%v restarts=%d", calls.setcapped, calls.restarted)
	}
	calls.mu.Unlock()
	// Backup of the old binary exists.
	matches, _ := filepath.Glob(filepath.Join(dir, ".backups", "self-update-*", "hoptrail"))
	if len(matches) != 1 {
		t.Fatalf("backups = %v, want exactly one", matches)
	}
	if raw, _ := os.ReadFile(matches[0]); string(raw) != "\x7fELFold-binary" {
		t.Error("backup content is not the old binary")
	}
	// "applying" was reported.
	if st := fc.lastStatus(); st == nil || st["state"] != "applying" {
		t.Errorf("last status = %v, want applying", st)
	}
}

func TestUpdater_Sha256MismatchRefuses(t *testing.T) {
	fc := newFakeCentral(t)
	u, dir, calls := testUpdater(t, fc, "v0.6.1")

	cmd := cmdFor("0.7.0", updaterELF)
	cmd.SHA256 = sumOf([]byte("something else"))
	err := u.apply(context.Background(), cmd)
	if err == nil || !contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("apply = %v, want sha256 mismatch", err)
	}
	live, _ := os.ReadFile(filepath.Join(dir, "bin", "hoptrail"))
	if string(live) != "\x7fELFold-binary" {
		t.Error("live binary touched despite mismatch")
	}
	calls.mu.Lock()
	if calls.restarted != 0 {
		t.Error("restarted despite mismatch")
	}
	calls.mu.Unlock()
}

func TestUpdater_VersionProbeMismatchRefuses(t *testing.T) {
	fc := newFakeCentral(t)
	u, dir, _ := testUpdater(t, fc, "v0.6.1")

	// Central serves a binary whose version probe won't match the
	// command (the fake VersionProbe reports v0.0.0 for unknown bytes).
	other := append([]byte("\x7fELF"), []byte("unexpected")...)
	fc.mu.Lock()
	fc.binary = other
	fc.mu.Unlock()
	cmd := cmdFor("0.7.0", other) // sha matches what's served
	err := u.apply(context.Background(), cmd)
	if err == nil || !contains(err.Error(), "reports") {
		t.Fatalf("apply = %v, want version-report mismatch", err)
	}
	live, _ := os.ReadFile(filepath.Join(dir, "bin", "hoptrail"))
	if string(live) != "\x7fELFold-binary" {
		t.Error("live binary touched despite version mismatch")
	}
}

func TestUpdater_SetcapFailureRollsBack(t *testing.T) {
	fc := newFakeCentral(t)
	u, dir, calls := testUpdater(t, fc, "v0.6.1")
	u.Hooks.Setcap = func(context.Context, string) ([]byte, error) {
		return []byte("nope"), errors.New("sudo says no")
	}

	err := u.apply(context.Background(), cmdFor("0.7.0", updaterELF))
	if err == nil || !contains(err.Error(), "rolled back") {
		t.Fatalf("apply = %v, want rollback error", err)
	}
	live, _ := os.ReadFile(filepath.Join(dir, "bin", "hoptrail"))
	if string(live) != "\x7fELFold-binary" {
		t.Error("rollback did not restore the old binary")
	}
	calls.mu.Lock()
	if calls.restarted != 0 {
		t.Error("restarted after a failed setcap")
	}
	calls.mu.Unlock()
}

func TestUpdater_SudoersPreflightStopsEarly(t *testing.T) {
	fc := newFakeCentral(t)
	u, dir, _ := testUpdater(t, fc, "v0.6.1")
	u.Hooks.SudoersCheck = func(context.Context) error {
		return errors.New("sudoers rule unusable — re-run install.sh on this probe")
	}
	err := u.apply(context.Background(), cmdFor("0.7.0", updaterELF))
	if err == nil || !contains(err.Error(), "install.sh") {
		t.Fatalf("apply = %v, want sudoers preflight error", err)
	}
	// Nothing downloaded, nothing staged.
	entries, _ := os.ReadDir(filepath.Join(dir, "update"))
	if len(entries) != 0 {
		t.Errorf("staging dir has %d entries after preflight refusal", len(entries))
	}
}

func TestUpdater_OnCommandSkipsWhenCurrentAndSingleFlights(t *testing.T) {
	fc := newFakeCentral(t)
	u, _, _ := testUpdater(t, fc, "v0.7.0-2-gabc1234") // base 0.7.0

	// Same base as the command: nothing to do, no status reports.
	u.OnCommand(cmdFor("0.7.0", updaterELF))
	time.Sleep(100 * time.Millisecond)
	if st := fc.lastStatus(); st != nil {
		t.Errorf("status reported for an already-current probe: %v", st)
	}

	// Single-flight: holding the running flag swallows new commands.
	u.mu.Lock()
	u.running = true
	u.mu.Unlock()
	u.Version = "v0.6.1"
	u.OnCommand(cmdFor("0.7.0", updaterELF))
	time.Sleep(100 * time.Millisecond)
	if st := fc.lastStatus(); st != nil {
		t.Errorf("second in-flight apply started: %v", st)
	}
}

func TestUpdater_FailureReportsToCentral(t *testing.T) {
	fc := newFakeCentral(t)
	u, _, _ := testUpdater(t, fc, "v0.6.1")
	cmd := cmdFor("0.7.0", updaterELF)
	cmd.SHA256 = sumOf([]byte("wrong"))

	u.OnCommand(cmd)
	deadline := time.After(5 * time.Second)
	for {
		if st := fc.lastStatus(); st != nil && st["state"] == "failed" {
			if !contains(st["error"], "sha256") {
				t.Errorf("failure report = %v", st)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("no failure report reached the central")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
