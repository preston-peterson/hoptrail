package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

// seedProbe registers an agent in the probes table directly.
func seedProbe(t *testing.T, f *fixture, probeID string, lastSeen time.Time) {
	t.Helper()
	if err := f.store.UpsertProbeHeartbeat(context.Background(), probeID, "v0.3.0-test", lastSeen.Add(-time.Hour), lastSeen, "192.0.2.70", "amd64"); err != nil {
		t.Fatalf("seed probe %s: %v", probeID, err)
	}
}

// ---------- /api/probes ----------

func TestHandleProbes_LocalSynthesizedFirstPlusAgents(t *testing.T) {
	f := newFixture(t)
	seedProbe(t, f, "site-east-pi", time.Now())                     // fresh → online
	seedProbe(t, f, "stale-cabin", time.Now().Add(-10*time.Minute)) // > 180s → offline

	code, body := f.get(t, "/api/probes")
	if code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", code, body)
	}
	var resp probesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Probes) != 3 {
		t.Fatalf("probes = %d, want 3 (local + 2 agents)", len(resp.Probes))
	}
	if resp.Probes[0].ProbeID != storage.LocalProbeID || !resp.Probes[0].Online {
		t.Errorf("probes[0] = %+v, want synthesized online local first", resp.Probes[0])
	}
	byID := map[string]probeJSON{}
	for _, p := range resp.Probes {
		byID[p.ProbeID] = p
	}
	if !byID["site-east-pi"].Online {
		t.Error("fresh agent reported offline")
	}
	if byID["stale-cabin"].Online {
		t.Error("stale agent (10min since heartbeat) reported online, want offline past 180s")
	}
}

// ---------- probe_id scoping on the read API ----------

func TestResolveProbeID_Semantics(t *testing.T) {
	f := newFixture(t)
	seedProbe(t, f, "site-east-pi", time.Now())
	now := time.Now().UnixMilli()
	insertSample(t, f, 1, now, "10.0.0.1", 500) // local row

	cases := []struct {
		name     string
		query    string
		wantCode int
	}{
		{"absent defaults to local", "/api/samples", http.StatusOK},
		{"explicit local", "/api/samples?probe_id=local", http.StatusOK},
		{"registered agent", "/api/samples?probe_id=site-east-pi", http.StatusOK},
		{"unknown probe 404s", "/api/samples?probe_id=nope-never", http.StatusNotFound},
		{"all rejected until overview ships", "/api/samples?probe_id=all", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := f.get(t, tc.query)
			if code != tc.wantCode {
				t.Errorf("%s: status = %d, want %d; body=%s", tc.query, code, tc.wantCode, body)
			}
		})
	}
}

func TestHandleSamples_ProbeIDScopesRows(t *testing.T) {
	f := newFixture(t)
	seedProbe(t, f, "site-east-pi", time.Now())
	now := time.Now().UnixMilli()
	insertSample(t, f, 1, now-1000, "10.0.0.1", 500) // local
	if _, err := f.store.DB().Exec(
		`INSERT INTO samples (probe_id, target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?, ?)`,
		"site-east-pi", "8.8.8.8", 1, now-1000, "10.9.9.9", 9000,
	); err != nil {
		t.Fatalf("insert agent sample: %v", err)
	}

	for _, tc := range []struct {
		query  string
		wantIP string
	}{
		{"/api/samples", "10.0.0.1"},
		{"/api/samples?probe_id=site-east-pi", "10.9.9.9"},
	} {
		_, body := f.get(t, tc.query)
		var resp samplesResponse
		_ = json.Unmarshal(body, &resp)
		if len(resp.Samples) != 1 || resp.Samples[0].IP == nil || *resp.Samples[0].IP != tc.wantIP {
			t.Errorf("%s: samples = %+v, want exactly one row with ip %s", tc.query, resp.Samples, tc.wantIP)
		}
	}
}

// ---------- /api/path for a remote probe ----------

func TestHandlePath_AgentServedFromSnapshotWithEnrichment(t *testing.T) {
	f := newFixture(t)
	seedProbe(t, f, "site-east-pi", time.Now())

	// The agent's stored snapshot: hop 1 healthy, hop 2 rate-limited
	// (loss with healthy downstream), hop 3 the destination. Central
	// must classify loss states and add the rdns hostname for hop 1.
	hopsJSON := `[
		{"ttl":1,"current_ip":"192.0.2.1","current_rtt_ms":0.5,"avg_rtt_ms":0.6,"min_rtt_ms":0.4,"loss_percent":0},
		{"ttl":2,"current_ip":"203.0.113.10","current_rtt_ms":5,"avg_rtt_ms":6,"min_rtt_ms":4,"loss_percent":40},
		{"ttl":3,"current_ip":"8.8.8.8","current_rtt_ms":12,"avg_rtt_ms":13,"min_rtt_ms":11,"loss_percent":0}
	]`
	snapTs := time.Now().Add(-20 * time.Second).UnixMilli()
	if err := f.store.UpsertPathSnapshot(context.Background(), storage.PathSnapshot{
		ProbeID: "site-east-pi", Target: "8.8.8.8",
		Ts: snapTs, HopCount: 3, TargetTTL: 3, HopsJSON: hopsJSON,
	}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	if err := f.store.UpsertRDNS(context.Background(), "192.0.2.1", "gw.example"); err != nil {
		t.Fatalf("seed rdns: %v", err)
	}

	code, body := f.get(t, "/api/path?probe_id=site-east-pi")
	if code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", code, body)
	}
	var resp pathResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ProbeID != "site-east-pi" || resp.SnapshotTs == nil || *resp.SnapshotTs != snapTs {
		t.Errorf("probe identity/staleness = %q/%v, want site-east-pi/%d", resp.ProbeID, resp.SnapshotTs, snapTs)
	}
	if resp.HopCount != 3 || resp.TargetTTL != 3 || len(resp.Hops) != 3 {
		t.Fatalf("shape = %d/%d/%d hops, want 3/3/3", resp.HopCount, resp.TargetTTL, len(resp.Hops))
	}
	if resp.Hops[0].Hostname == nil || *resp.Hops[0].Hostname != "gw.example" {
		t.Errorf("hop1 hostname = %v, want rdns-enriched gw.example", resp.Hops[0].Hostname)
	}
	if resp.Hops[1].LossState != "rate_limited" {
		t.Errorf("hop2 loss_state = %q, want rate_limited (loss with healthy downstream)", resp.Hops[1].LossState)
	}
	if resp.Hops[2].LossState != "ok" {
		t.Errorf("hop3 loss_state = %q, want ok", resp.Hops[2].LossState)
	}
}

func TestHandlePath_AgentWithoutSnapshot404s(t *testing.T) {
	f := newFixture(t)
	seedProbe(t, f, "site-east-pi", time.Now())
	code, _ := f.get(t, "/api/path?probe_id=site-east-pi")
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when the probe has no snapshot for the target", code)
	}
}

func TestHandlePath_LocalUnchangedShape(t *testing.T) {
	// The local response must keep its v0.2 wire shape: no probe_id /
	// snapshot_ts keys at all (omitempty), served from the live engine.
	f := newFixture(t)
	_, body := f.get(t, "/api/path")
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["probe_id"]; present {
		t.Error("local /api/path response contains probe_id, want v0.2-identical shape")
	}
	if _, present := raw["snapshot_ts"]; present {
		t.Error("local /api/path response contains snapshot_ts, want v0.2-identical shape")
	}
}

// ---------- /api/retention (step-97) ----------

func TestHandleRetention_ReportsConfiguredDays(t *testing.T) {
	f := newFixture(t)
	srv, err := New(Config{
		ListenAddr:    "127.0.0.1:0",
		Supervisor:    f.supervisor,
		Store:         f.store,
		WebFS:         fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}},
		RetentionDays: 14,
	}, nil)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/retention")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	var resp struct {
		RetentionDays int `json:"retention_days"`
	}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RetentionDays != 14 {
		t.Errorf("retention_days = %d, want 14", resp.RetentionDays)
	}
}

// ---------- per-tab probe via /api/tabs (step-96) ----------

func TestTabs_ProbeIDCreatePatchValidate(t *testing.T) {
	f := newFixture(t)
	seedProbe(t, f, "site-east-pi", time.Now())
	seedActiveTarget(t, f, "8.8.8.8")

	// Create with a registered probe.
	code, body := f.post(t, "/api/tabs", `{"target":"8.8.8.8","probe_id":"site-east-pi"}`)
	if code != http.StatusOK {
		t.Fatalf("create: status = %d; body=%s", code, body)
	}
	var created tabJSON
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ProbeID != "site-east-pi" {
		t.Errorf("created probe_id = %q, want site-east-pi", created.ProbeID)
	}

	// Default create → local.
	code, body = f.post(t, "/api/tabs", `{"target":"8.8.8.8"}`)
	if code != http.StatusOK {
		t.Fatalf("default create: status = %d; body=%s", code, body)
	}
	var defTab tabJSON
	_ = json.Unmarshal(body, &defTab)
	if defTab.ProbeID != "local" {
		t.Errorf("default probe_id = %q, want local", defTab.ProbeID)
	}

	// Unknown + reserved probes rejected at write time.
	for _, bad := range []string{"never-registered", "all"} {
		code, body = f.post(t, "/api/tabs", fmt.Sprintf(`{"target":"8.8.8.8","probe_id":%q}`, bad))
		if code != http.StatusBadRequest {
			t.Errorf("create with probe %q: status = %d, want 400; body=%s", bad, code, body)
		}
	}

	// PATCH flips the probe; PATCH to unknown rejected.
	code, body = f.patch(t, fmt.Sprintf("/api/tabs/%d", defTab.TabID), `{"probe_id":"site-east-pi"}`)
	if code != http.StatusOK {
		t.Fatalf("patch: status = %d; body=%s", code, body)
	}
	var patched tabJSON
	_ = json.Unmarshal(body, &patched)
	if patched.ProbeID != "site-east-pi" {
		t.Errorf("patched probe_id = %q, want site-east-pi", patched.ProbeID)
	}
	if code, _ = f.patch(t, fmt.Sprintf("/api/tabs/%d", defTab.TabID), `{"probe_id":"nope"}`); code != http.StatusBadRequest {
		t.Errorf("patch unknown probe: status = %d, want 400", code)
	}

	// copy_from carries the source tab's probe.
	code, body = f.post(t, "/api/tabs", fmt.Sprintf(`{"target":"8.8.8.8","copy_from":%d}`, created.TabID))
	if code != http.StatusOK {
		t.Fatalf("copy create: status = %d; body=%s", code, body)
	}
	var copied tabJSON
	_ = json.Unmarshal(body, &copied)
	if copied.ProbeID != "site-east-pi" {
		t.Errorf("copied probe_id = %q, want source's site-east-pi", copied.ProbeID)
	}
}

// ---------- export scoping ----------

func TestHandleExport_ProbeIDScoped(t *testing.T) {
	f := newFixture(t)
	seedProbe(t, f, "site-east-pi", time.Now())
	now := time.Now().UnixMilli()
	insertSample(t, f, 1, now-1000, "10.0.0.1", 500) // local
	if _, err := f.store.DB().Exec(
		`INSERT INTO samples (probe_id, target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?, ?)`,
		"site-east-pi", "8.8.8.8", 1, now-1000, "10.9.9.9", 9000,
	); err != nil {
		t.Fatalf("insert agent sample: %v", err)
	}

	_, body := f.get(t, "/api/export?probe_id=site-east-pi")
	var bundle exportBundle
	if err := json.Unmarshal(body, &bundle); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body[:min(200, len(body))])
	}
	if bundle.ProbeID != "site-east-pi" {
		t.Errorf("bundle.probe_id = %q, want site-east-pi", bundle.ProbeID)
	}
	if len(bundle.Samples) != 1 || bundle.Samples[0].IP == nil || *bundle.Samples[0].IP != "10.9.9.9" {
		t.Errorf("bundle.samples = %+v, want only the agent row", bundle.Samples)
	}
	if bundle.Path != nil {
		t.Errorf("bundle.path = %+v, want nil (agent has no snapshot for the target)", bundle.Path)
	}
}

func TestHandleRetention_PatchPersistsAndGetReflects(t *testing.T) {
	f := newFixture(t)
	srv, err := New(Config{
		ListenAddr: "127.0.0.1:0", Supervisor: f.supervisor, Store: f.store,
		WebFS:         fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}},
		RetentionDays: 7,
	}, nil)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	code, body := httpDo(t, "PATCH", ts.URL+"/api/retention", `{"retention_days":30}`)
	if code != http.StatusNoContent {
		t.Fatalf("patch: %d %s", code, body)
	}
	code, body = httpDo(t, "GET", ts.URL+"/api/retention", "")
	if code != http.StatusOK {
		t.Fatalf("get: %d", code)
	}
	var resp struct {
		RetentionDays int `json:"retention_days"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.RetentionDays != 30 {
		t.Errorf("get after patch = %+v (%v), want 30", resp, err)
	}
	// Out-of-range rejected.
	if code, _ := httpDo(t, "PATCH", ts.URL+"/api/retention", `{"retention_days":0}`); code != http.StatusBadRequest {
		t.Errorf("zero days: %d, want 400", code)
	}
}

// ---------- outdated flag (#21) ----------

func TestHandleProbes_OutdatedFlag(t *testing.T) {
	uf := newUpdateFixture(t, func(c *Config) { c.Version = "v0.6.0" })
	now := time.Now()
	seed := func(id, version string) {
		t.Helper()
		if err := uf.store.UpsertProbeHeartbeat(context.Background(), id, version, now.Add(-time.Hour), now, "192.0.2.71", "amd64"); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("old-release", "v0.5.0")          // older release → outdated
	seed("same-release", "v0.6.0")         // current → fine
	seed("dev-ahead", "v0.6.0-3-gabc1234") // dev build on the same base → fine
	seed("unparseable", "dev")             // can't tell → never nag

	res, err := http.Get(uf.ts.URL + "/api/probes")
	if err != nil {
		t.Fatalf("GET /api/probes: %v", err)
	}
	defer res.Body.Close()
	var resp probesResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]bool{
		storage.LocalProbeID: false,
		"old-release":        true,
		"same-release":       false,
		"dev-ahead":          false,
		"unparseable":        false,
	}
	for _, p := range resp.Probes {
		if p.Outdated != want[p.ProbeID] {
			t.Errorf("probe %s outdated = %v, want %v", p.ProbeID, p.Outdated, want[p.ProbeID])
		}
	}
	if len(resp.Probes) != len(want) {
		t.Errorf("probes = %d, want %d", len(resp.Probes), len(want))
	}
}
