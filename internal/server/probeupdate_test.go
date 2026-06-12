package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

// probeUpdateFixture: a release fixture (fake GitHub source, version
// v0.5.0 central... overridden to v0.6.0 release target) + a seeded
// remote probe + an ingest token so the probe-facing endpoints work.
func newProbeUpdateFixture(t *testing.T) (*updateFixture, *fakeReleaseSource, string) {
	t.Helper()
	uf, src := newReleaseFixture(t) // central v0.5.0, fake release v0.6.0
	// Mint an ingest token the "probe" can use.
	res := postJSON(t, uf.ts.URL+"/api/probe-tokens", `{"name":"site-east"}`)
	defer res.Body.Close()
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&minted); err != nil || minted.Token == "" {
		t.Fatalf("mint token: %v (%+v)", err, minted)
	}
	return uf, src, minted.Token
}

func seedUpdatableProbe(t *testing.T, uf *updateFixture, id, version, arch string) {
	t.Helper()
	now := time.Now()
	if err := uf.store.UpsertProbeHeartbeat(context.Background(), id, version, now.Add(-time.Hour), now, "192.0.2.77", arch); err != nil {
		t.Fatalf("seed probe: %v", err)
	}
}

func commandUpdate(t *testing.T, uf *updateFixture, id string) *http.Response {
	t.Helper()
	return postJSON(t, uf.ts.URL+"/api/probes/"+id+"/update", "")
}

// heartbeat sends an authenticated heartbeat and returns the decoded
// reply.
func heartbeat(t *testing.T, uf *updateFixture, token, probeID, version string) map[string]any {
	t.Helper()
	body := fmt.Sprintf(`{"probe_id":%q,"version":%q,"started_at":1,"targets":[],"arch":"amd64"}`, probeID, version)
	req, _ := http.NewRequest(http.MethodPost, uf.ts.URL+"/api/ingest/heartbeat", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("heartbeat = %d: %s", res.StatusCode, raw)
	}
	var reply map[string]any
	if err := json.NewDecoder(res.Body).Decode(&reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return reply
}

func TestProbeUpdate_CommandAndDeliver(t *testing.T) {
	uf, src, token := newProbeUpdateFixture(t)
	seedUpdatableProbe(t, uf, "site-east", "v0.5.0", "amd64")

	res := commandUpdate(t, uf, "site-east")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("command = %d: %s", res.StatusCode, raw)
	}
	src.mu.Lock()
	dl := src.dlCount
	src.mu.Unlock()
	if dl != 1 {
		t.Errorf("release download calls = %d, want 1 (cache fill)", dl)
	}
	// The cached binary exists where the serve endpoint looks.
	cached := filepath.Join(uf.installDir, "release-cache", "0.6.0", "amd64", "hoptrail")
	if _, err := os.Stat(cached); err != nil {
		t.Fatalf("cached binary missing: %v", err)
	}

	// Heartbeat reply carries the command.
	reply := heartbeat(t, uf, token, "site-east", "v0.5.0")
	upd, ok := reply["update"].(map[string]any)
	if !ok {
		t.Fatalf("reply has no update command: %v", reply)
	}
	if upd["version"] != "0.6.0" || upd["sha256"] == "" || !strings.Contains(upd["path"].(string), "update-binary") {
		t.Errorf("command = %v", upd)
	}

	// The probe can fetch the binary with its token.
	req, _ := http.NewRequest(http.MethodGet, uf.ts.URL+upd["path"].(string), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	bres, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch binary: %v", err)
	}
	defer bres.Body.Close()
	if bres.StatusCode != http.StatusOK {
		t.Fatalf("fetch binary = %d", bres.StatusCode)
	}
	raw, _ := io.ReadAll(bres.Body)
	if len(raw) == 0 || !strings.HasPrefix(string(raw), "\x7fELF") {
		t.Errorf("served binary looks wrong (%d bytes)", len(raw))
	}

	// Unauthenticated fetch is refused.
	ures, err := http.Get(uf.ts.URL + upd["path"].(string))
	if err != nil {
		t.Fatalf("unauthenticated fetch: %v", err)
	}
	defer ures.Body.Close()
	if ures.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated fetch = %d, want 401", ures.StatusCode)
	}
}

func TestProbeUpdate_SuccessDetection(t *testing.T) {
	uf, _, token := newProbeUpdateFixture(t)
	seedUpdatableProbe(t, uf, "site-east", "v0.5.0", "amd64")
	commandUpdate(t, uf, "site-east").Body.Close()

	heartbeat(t, uf, token, "site-east", "v0.5.0") // delivery 1
	// Probe acknowledges, applies, restarts, and heartbeats on the
	// target version:
	reply := heartbeat(t, uf, token, "site-east", "v0.6.0")
	if reply["update"] != nil {
		t.Errorf("reply still carries command after success: %v", reply["update"])
	}
	pu, err := uf.store.GetProbeUpdate(context.Background(), "site-east")
	if err != nil || pu == nil {
		t.Fatalf("update row: (%v, %v)", pu, err)
	}
	if pu.State != storage.ProbeUpdateApplied {
		t.Errorf("state = %s, want applied", pu.State)
	}
}

func TestProbeUpdate_OldProbeNeverAcks(t *testing.T) {
	uf, _, token := newProbeUpdateFixture(t)
	seedUpdatableProbe(t, uf, "site-east", "v0.5.0", "amd64")
	commandUpdate(t, uf, "site-east").Body.Close()

	heartbeat(t, uf, token, "site-east", "v0.5.0") // delivery 1
	heartbeat(t, uf, token, "site-east", "v0.5.0") // delivery 2
	// Third heartbeat: still pending, still old version → too old.
	reply := heartbeat(t, uf, token, "site-east", "v0.5.0")
	if reply["update"] != nil {
		t.Errorf("command still delivered after giving up: %v", reply["update"])
	}
	pu, _ := uf.store.GetProbeUpdate(context.Background(), "site-east")
	if pu == nil || pu.State != storage.ProbeUpdateFailed || !strings.Contains(pu.Error, "manually") {
		t.Errorf("update row = %+v, want failed with manual hint", pu)
	}
	// The failure reached the alert history.
	hist, err := uf.store.ListAlertHistory(context.Background(), 5)
	if err != nil || len(hist) == 0 {
		t.Fatalf("alert history: (%v, %v)", hist, err)
	}
	if hist[0].EventType != "probe_update" || hist[0].Subject != "site-east" {
		t.Errorf("history entry = %+v", hist[0])
	}
}

func TestProbeUpdate_StatusReportsAndApplyingTimeout(t *testing.T) {
	uf, _, token := newProbeUpdateFixture(t)
	seedUpdatableProbe(t, uf, "site-east", "v0.5.0", "amd64")
	commandUpdate(t, uf, "site-east").Body.Close()

	// Probe acknowledges.
	body := `{"probe_id":"site-east","state":"applying"}`
	req, _ := http.NewRequest(http.MethodPost, uf.ts.URL+"/api/ingest/update-status", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()
	pu, _ := uf.store.GetProbeUpdate(context.Background(), "site-east")
	if pu.State != storage.ProbeUpdateApplying {
		t.Fatalf("state = %s, want applying", pu.State)
	}

	// Backdate the acknowledgment past the apply timeout; the next
	// old-version heartbeat declares it failed.
	stale := time.Now().Add(-applyTimeout - time.Minute).UnixMilli()
	if err := uf.store.SetProbeUpdateState(context.Background(), "site-east", storage.ProbeUpdateApplying, "", stale); err != nil {
		t.Fatal(err)
	}
	heartbeat(t, uf, token, "site-east", "v0.5.0")
	pu, _ = uf.store.GetProbeUpdate(context.Background(), "site-east")
	if pu.State != storage.ProbeUpdateFailed || !strings.Contains(pu.Error, "rolled back or wedged") {
		t.Errorf("update row = %+v, want timeout failure", pu)
	}

	// A probe-reported failure also lands.
	commandUpdate(t, uf, "site-east").Body.Close()
	body = `{"probe_id":"site-east","state":"failed","error":"sha256 mismatch"}`
	req, _ = http.NewRequest(http.MethodPost, uf.ts.URL+"/api/ingest/update-status", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	res, _ = http.DefaultClient.Do(req)
	res.Body.Close()
	pu, _ = uf.store.GetProbeUpdate(context.Background(), "site-east")
	if pu.State != storage.ProbeUpdateFailed || pu.Error != "sha256 mismatch" {
		t.Errorf("update row = %+v, want probe-reported failure", pu)
	}
}

func TestProbeUpdate_CommandValidation(t *testing.T) {
	uf, _, _ := newProbeUpdateFixture(t)

	// Unknown probe.
	res := commandUpdate(t, uf, "ghost")
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown probe = %d, want 404", res.StatusCode)
	}

	// Probe without arch (pre-0.7) can't be commanded.
	seedUpdatableProbe(t, uf, "old-probe", "v0.5.0", "")
	res = commandUpdate(t, uf, "old-probe")
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Errorf("archless probe = %d, want 409", res.StatusCode)
	}

	// Probe already current.
	seedUpdatableProbe(t, uf, "fresh", "v0.6.0", "amd64")
	res2 := commandUpdate(t, uf, "fresh")
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusConflict {
		t.Errorf("current probe = %d, want 409", res2.StatusCode)
	}

	// local is never commandable.
	res3 := commandUpdate(t, uf, "local")
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusBadRequest {
		t.Errorf("local = %d, want 400", res3.StatusCode)
	}
}

func TestProbeUpdate_CancelAndPin(t *testing.T) {
	uf, _, _ := newProbeUpdateFixture(t)
	seedUpdatableProbe(t, uf, "site-east", "v0.5.0", "amd64")
	commandUpdate(t, uf, "site-east").Body.Close()

	// Cancel a pending command.
	req, _ := http.NewRequest(http.MethodDelete, uf.ts.URL+"/api/probes/site-east/update", nil)
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Errorf("cancel = %d, want 204", res.StatusCode)
	}
	if pu, _ := uf.store.GetProbeUpdate(context.Background(), "site-east"); pu != nil {
		t.Errorf("row survives cancel: %+v", pu)
	}

	// Pin round-trip via PATCH.
	preq, _ := http.NewRequest(http.MethodPatch, uf.ts.URL+"/api/probes/site-east", strings.NewReader(`{"pin":true}`))
	pres, _ := http.DefaultClient.Do(preq)
	pres.Body.Close()
	if pres.StatusCode != http.StatusOK {
		t.Fatalf("pin = %d", pres.StatusCode)
	}
	probes, _ := uf.store.ListProbes(context.Background())
	if len(probes) != 1 || !probes[0].Pin {
		t.Errorf("pin not persisted: %+v", probes)
	}

	// Applying commands can't be canceled.
	commandUpdate(t, uf, "site-east").Body.Close()
	now := time.Now().UnixMilli()
	if err := uf.store.SetProbeUpdateState(context.Background(), "site-east", storage.ProbeUpdateApplying, "", now); err != nil {
		t.Fatal(err)
	}
	req2, _ := http.NewRequest(http.MethodDelete, uf.ts.URL+"/api/probes/site-east/update", nil)
	res2, _ := http.DefaultClient.Do(req2)
	res2.Body.Close()
	if res2.StatusCode != http.StatusConflict {
		t.Errorf("cancel mid-apply = %d, want 409", res2.StatusCode)
	}
}

func TestProbeUpdate_BinaryEndpointValidation(t *testing.T) {
	uf, _, token := newProbeUpdateFixture(t)
	for _, q := range []string{
		"version=../../etc/passwd&arch=amd64",
		"version=0.6.0&arch=../../x",
		"version=0.6.0&arch=riscv64",
		"version=v0.6.0&arch=amd64", // leading v not accepted
	} {
		req, _ := http.NewRequest(http.MethodGet, uf.ts.URL+"/api/ingest/update-binary?"+q, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("query %q = %d, want 400", q, res.StatusCode)
		}
	}
	// Valid shape but not cached → 404, not 500.
	req, _ := http.NewRequest(http.MethodGet, uf.ts.URL+"/api/ingest/update-binary?version=9.9.9&arch=amd64", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("uncached = %d, want 404", res.StatusCode)
	}
}
