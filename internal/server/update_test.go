package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

// updateFixture builds a server with a temp install dir and fully
// injected update hooks. The fake "binary" files are shell-free blobs
// with an ELF magic prefix; the version probe is injected so nothing
// is ever executed.
type updateFixture struct {
	ts         *httptest.Server
	installDir string
	store      *storage.Store

	mu        sync.Mutex
	setcapped []string
	restarted []string
	sudoers   string
	setcapErr error
}

func elfBlob(marker string) []byte {
	return append([]byte("\x7fELF"), []byte(marker)...)
}

func newUpdateFixture(t *testing.T, mutate ...func(*Config)) *updateFixture {
	t.Helper()
	base := newFixture(t)
	uf := &updateFixture{
		installDir: t.TempDir(),
		store:      base.store,
		sudoers:    "# SUDOERS_VERSION: 2\nrules...\n",
	}

	// Live binary in place.
	if err := os.MkdirAll(filepath.Join(uf.installDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uf.installDir, "bin", "hoptrail"), elfBlob("live-v1"), 0o755); err != nil {
		t.Fatal(err)
	}

	hooks := &UpdateHooks{
		ReadSudoers: func(ctx context.Context) ([]byte, error) {
			uf.mu.Lock()
			defer uf.mu.Unlock()
			if uf.sudoers == "" {
				return nil, fmt.Errorf("sudo: a password is required")
			}
			return []byte(uf.sudoers), nil
		},
		Setcap: func(ctx context.Context, binPath string) ([]byte, error) {
			uf.mu.Lock()
			defer uf.mu.Unlock()
			if uf.setcapErr != nil {
				return []byte("setcap output"), uf.setcapErr
			}
			uf.setcapped = append(uf.setcapped, binPath)
			return nil, nil
		},
		Restart: func(ctx context.Context, unit string) error {
			uf.mu.Lock()
			defer uf.mu.Unlock()
			uf.restarted = append(uf.restarted, unit)
			return nil
		},
		StagedVersion: func(ctx context.Context, path string) (string, error) {
			b, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			return "v-" + string(b[4:]), nil
		},
	}

	cfg := Config{
		ListenAddr:  "127.0.0.1:0",
		Supervisor:  base.supervisor,
		Store:       base.store,
		WebFS:       fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}},
		Version:     "v-live-v1",
		InstallDir:  uf.installDir,
		UpdateHooks: hooks,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	srv, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	uf.ts = httptest.NewServer(srv.routes())
	t.Cleanup(uf.ts.Close)
	return uf
}

func (uf *updateFixture) status(t *testing.T) updateStatusResponse {
	t.Helper()
	res, err := http.Get(uf.ts.URL + "/api/update")
	if err != nil {
		t.Fatalf("GET /api/update: %v", err)
	}
	defer res.Body.Close()
	var st updateStatusResponse
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return st
}

func (uf *updateFixture) upload(t *testing.T, body []byte) (int, string) {
	t.Helper()
	res, err := http.Post(uf.ts.URL+"/api/update/upload", "application/octet-stream", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer res.Body.Close()
	buf := new(strings.Builder)
	_, _ = fmt.Fprintf(buf, "")
	b := make([]byte, 4096)
	n, _ := res.Body.Read(b)
	return res.StatusCode, string(b[:n])
}

func (uf *updateFixture) apply(t *testing.T) (int, string) {
	t.Helper()
	res, err := http.Post(uf.ts.URL+"/api/update/apply", "", nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	defer res.Body.Close()
	b := make([]byte, 4096)
	n, _ := res.Body.Read(b)
	return res.StatusCode, string(b[:n])
}

func TestUpdate_UploadAndApply(t *testing.T) {
	uf := newUpdateFixture(t)

	// Fresh state: nothing staged, sudoers ok.
	st := uf.status(t)
	if st.Staged.Present {
		t.Error("fresh status: staged.present = true, want false")
	}
	if !st.Sudoers.OK {
		t.Errorf("fresh status: sudoers not ok: %s", st.Sudoers.Error)
	}
	if st.RunningVersion != "v-live-v1" {
		t.Errorf("running_version = %q", st.RunningVersion)
	}

	// Apply with nothing staged → 409.
	if code, _ := uf.apply(t); code != http.StatusConflict {
		t.Errorf("apply with nothing staged: %d, want 409", code)
	}

	// Non-ELF upload rejected.
	if code, msg := uf.upload(t, []byte("PK\x03\x04zipzip")); code != http.StatusBadRequest {
		t.Errorf("zip upload: %d (%s), want 400", code, msg)
	}

	// Real upload stages and reports the probed version.
	code, _ := uf.upload(t, elfBlob("new-v2"))
	if code != http.StatusOK {
		t.Fatalf("upload: %d", code)
	}
	st = uf.status(t)
	if !st.Staged.Present || st.Staged.Version != "v-new-v2" {
		t.Fatalf("staged = %+v, want present v-new-v2", st.Staged)
	}

	// Apply: binary swapped, backup taken, setcap'd, both units restarted.
	code, body := uf.apply(t)
	if code != http.StatusOK {
		t.Fatalf("apply: %d (%s)", code, body)
	}
	live, err := os.ReadFile(filepath.Join(uf.installDir, "bin", "hoptrail"))
	if err != nil || string(live[4:]) != "new-v2" {
		t.Errorf("live binary after apply = %q, %v", live, err)
	}
	if _, err := os.Stat(filepath.Join(uf.installDir, "update", "hoptrail")); !os.IsNotExist(err) {
		t.Error("staged binary still present after apply")
	}
	backups, _ := filepath.Glob(filepath.Join(uf.installDir, ".backups", "ui-update-*", "hoptrail"))
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}
	if b, _ := os.ReadFile(backups[0]); string(b[4:]) != "live-v1" {
		t.Errorf("backup content = %q, want the old binary", b)
	}
	uf.mu.Lock()
	if len(uf.setcapped) != 1 || uf.setcapped[0] != filepath.Join(uf.installDir, "bin", "hoptrail") {
		t.Errorf("setcap calls = %v", uf.setcapped)
	}
	uf.mu.Unlock()

	// Restart is post-response + delayed; poll for it.
	deadline := time.After(3 * time.Second)
	for {
		uf.mu.Lock()
		n := len(uf.restarted)
		uf.mu.Unlock()
		if n == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("restarts = %d, want 2 (probe + self)", n)
		case <-time.After(20 * time.Millisecond):
		}
	}
	uf.mu.Lock()
	if uf.restarted[1] != "hoptrail" {
		t.Errorf("self-restart must come last: %v", uf.restarted)
	}
	uf.mu.Unlock()
}

func TestUpdate_SudoersDriftBlocksApply(t *testing.T) {
	uf := newUpdateFixture(t)
	if code, _ := uf.upload(t, elfBlob("new-v2")); code != http.StatusOK {
		t.Fatal("upload failed")
	}

	uf.mu.Lock()
	uf.sudoers = "# SUDOERS_VERSION: 99\nrules...\n"
	uf.mu.Unlock()
	code, msg := uf.apply(t)
	if code != http.StatusConflict || !strings.Contains(msg, "re-run install.sh") {
		t.Errorf("drifted sudoers: %d (%s), want 409 + re-run message", code, msg)
	}

	// Missing rule entirely (sudo -n fails) — same block.
	uf.mu.Lock()
	uf.sudoers = ""
	uf.mu.Unlock()
	if code, _ := uf.apply(t); code != http.StatusConflict {
		t.Errorf("missing sudoers: %d, want 409", code)
	}

	// Binary untouched throughout.
	live, _ := os.ReadFile(filepath.Join(uf.installDir, "bin", "hoptrail"))
	if string(live[4:]) != "live-v1" {
		t.Errorf("live binary changed despite blocked apply: %q", live)
	}
}

func TestUpdate_SetcapFailureRollsBack(t *testing.T) {
	uf := newUpdateFixture(t)
	if code, _ := uf.upload(t, elfBlob("new-v2")); code != http.StatusOK {
		t.Fatal("upload failed")
	}
	uf.mu.Lock()
	uf.setcapErr = fmt.Errorf("operation not supported")
	uf.mu.Unlock()

	code, msg := uf.apply(t)
	if code != http.StatusInternalServerError || !strings.Contains(msg, "rolled back") {
		t.Errorf("setcap failure: %d (%s), want 500 + rolled back", code, msg)
	}
	live, _ := os.ReadFile(filepath.Join(uf.installDir, "bin", "hoptrail"))
	if string(live[4:]) != "live-v1" {
		t.Errorf("live binary after rollback = %q, want live-v1", live)
	}
	uf.mu.Lock()
	if len(uf.restarted) != 0 {
		t.Errorf("restarts after failed apply = %v, want none", uf.restarted)
	}
	uf.mu.Unlock()
}
