package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/preston-peterson/hoptrail/internal/bandwidth"
	"github.com/preston-peterson/hoptrail/internal/storage"
)

type fakeBandwidthRunner struct {
	inFlight     bool
	reconfigured int
	lastCfg      bandwidth.Config
}

func (f *fakeBandwidthRunner) RunNow() bool   { return !f.inFlight }
func (f *fakeBandwidthRunner) InFlight() bool { return f.inFlight }
func (f *fakeBandwidthRunner) Reconfigure(c bandwidth.Config) {
	f.reconfigured++
	f.lastCfg = c
}

// bwFixture spins a server with bandwidth wiring on top of the
// standard fixture's store/supervisor.
func bwFixture(t *testing.T, runner BandwidthRunner, cap bandwidth.Capability) (*fixture, *httptest.Server) {
	t.Helper()
	f := newFixture(t)
	srv, err := New(Config{
		ListenAddr:          "127.0.0.1:0",
		Supervisor:          f.supervisor,
		Store:               f.store,
		WebFS:               fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}},
		BandwidthRunner:     runner,
		BandwidthCapability: func() bandwidth.Capability { return cap },
	}, nil)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(withTestCSRF(srv.routes()))
	t.Cleanup(ts.Close)
	return f, ts
}

func httpDo(t *testing.T, method, url, body string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func TestBandwidthConfig_GetDefaultsAndCapability(t *testing.T) {
	_, ts := bwFixture(t, &fakeBandwidthRunner{}, bandwidth.Capability{Available: true, Version: "1.2.0"})
	code, body := httpDo(t, "GET", ts.URL+"/api/bandwidth/config", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", code, body)
	}
	var resp bandwidthConfigResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Capability.Available || resp.Capability.Version != "1.2.0" {
		t.Errorf("capability = %+v", resp.Capability)
	}
	if resp.Enabled || resp.DerateThreshold != 0.5 || len(resp.ScheduledTimes) != 1 || resp.ScheduledTimes[0] != "02:00" {
		t.Errorf("defaults = %+v", resp)
	}
	if resp.RunInFlight {
		t.Error("run_in_flight true with idle runner")
	}
}

func TestBandwidthConfig_PatchPersistsAndReconfigures(t *testing.T) {
	runner := &fakeBandwidthRunner{}
	f, ts := bwFixture(t, runner, bandwidth.Capability{Available: true})

	code, body := httpDo(t, "PATCH", ts.URL+"/api/bandwidth/config",
		`{"enabled":true,"scheduled_times":["02:00","14:00"],"derate_threshold":0.3}`)
	if code != http.StatusNoContent {
		t.Fatalf("status = %d; body=%s", code, body)
	}
	if runner.reconfigured != 1 || !runner.lastCfg.Enabled || runner.lastCfg.DerateThresh != 0.3 {
		t.Errorf("runner reconfigure = %d/%+v", runner.reconfigured, runner.lastCfg)
	}
	cfg, err := bandwidth.LoadConfig(context.Background(), f.store)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Enabled || len(cfg.ScheduledTimes) != 2 || cfg.DerateThresh != 0.3 {
		t.Errorf("persisted = %+v", cfg)
	}

	// Composite validation: pinning without an id must 400 and write
	// nothing.
	code, body = httpDo(t, "PATCH", ts.URL+"/api/bandwidth/config", `{"server_mode":"pinned"}`)
	if code != http.StatusBadRequest {
		t.Errorf("pinned-without-id: status = %d; body=%s", code, body)
	}
	// Unknown fields rejected (capability/run_in_flight are read-only).
	code, _ = httpDo(t, "PATCH", ts.URL+"/api/bandwidth/config", `{"run_in_flight":true}`)
	if code != http.StatusBadRequest {
		t.Errorf("read-only field: status = %d, want 400", code)
	}
	code, _ = httpDo(t, "PATCH", ts.URL+"/api/bandwidth/config", `{}`)
	if code != http.StatusBadRequest {
		t.Errorf("empty patch: status = %d, want 400", code)
	}
}

func TestBandwidthHistory_WindowedRows(t *testing.T) {
	f, ts := bwFixture(t, &fakeBandwidthRunner{}, bandwidth.Capability{Available: true})
	ctx := context.Background()
	now := time.Now().UnixMilli()
	for i, mbps := range []float64{900, 920, 940} {
		if err := f.store.RecordBandwidthSample(ctx, storage.BandwidthSample{
			Ts: now - int64(i)*3_600_000, Ok: true, DownMbps: mbps, UpMbps: mbps - 50,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	code, body := httpDo(t, "GET", ts.URL+"/api/bandwidth/history", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	var resp struct {
		Samples []bandwidthSampleJSON `json:"samples"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Samples) != 3 || resp.Samples[2].DownMbps != 900 {
		t.Errorf("history = %+v, want 3 ascending rows ending at 900", resp.Samples)
	}
}

func TestBandwidthDerateStatus_IncidentShape(t *testing.T) {
	f, ts := bwFixture(t, &fakeBandwidthRunner{}, bandwidth.Capability{Available: true})
	ctx := context.Background()

	// Empty: derated=false, last_test null.
	code, body := httpDo(t, "GET", ts.URL+"/api/bandwidth/derate-status", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	var resp derateStatusResponse
	_ = json.Unmarshal(body, &resp)
	if resp.Derated || resp.LastTest != nil {
		t.Errorf("empty status = %+v", resp)
	}

	// 8 healthy samples (baseline) then 2 flagged ones → derated with
	// since = first flagged ts.
	day := int64(86_400_000)
	now := time.Now().UnixMilli()
	for i := 1; i <= 8; i++ {
		if err := f.store.RecordBandwidthSample(ctx, storage.BandwidthSample{
			Ts: now - int64(i)*day/2, Ok: true, DownMbps: 1000, UpMbps: 950,
		}); err != nil {
			t.Fatal(err)
		}
	}
	incidentStart := now - 3_600_000
	for i, ts := range []int64{incidentStart, now - 60_000} {
		if err := f.store.RecordBandwidthSample(ctx, storage.BandwidthSample{
			Ts: ts, Ok: true, DownMbps: 1000, UpMbps: 180, DerateFlag: true,
		}); err != nil {
			t.Fatalf("seed flagged %d: %v", i, err)
		}
	}

	code, body = httpDo(t, "GET", ts.URL+"/api/bandwidth/derate-status", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	resp = derateStatusResponse{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Derated || resp.LastTest == nil || !resp.LastTest.DerateFlag {
		t.Fatalf("status = %+v, want derated with flagged last_test", resp)
	}
	if resp.Since == nil || *resp.Since != incidentStart {
		t.Errorf("since = %v, want incident start %d", resp.Since, incidentStart)
	}
	if resp.Baseline == nil || resp.Baseline.UpMbps != 950 {
		t.Errorf("baseline = %+v, want up 950", resp.Baseline)
	}
}

func TestBandwidthRun_StatusCodes(t *testing.T) {
	// Capability missing → 503.
	_, ts := bwFixture(t, &fakeBandwidthRunner{}, bandwidth.Capability{Available: false, Error: "not found"})
	if code, _ := httpDo(t, "POST", ts.URL+"/api/bandwidth/run", ""); code != http.StatusServiceUnavailable {
		t.Errorf("no capability: status = %d, want 503", code)
	}

	// Available + idle → 202.
	_, ts2 := bwFixture(t, &fakeBandwidthRunner{}, bandwidth.Capability{Available: true})
	if code, _ := httpDo(t, "POST", ts2.URL+"/api/bandwidth/run", ""); code != http.StatusAccepted {
		t.Errorf("idle: status = %d, want 202", code)
	}

	// In flight → 409.
	_, ts3 := bwFixture(t, &fakeBandwidthRunner{inFlight: true}, bandwidth.Capability{Available: true})
	if code, _ := httpDo(t, "POST", ts3.URL+"/api/bandwidth/run", ""); code != http.StatusConflict {
		t.Errorf("in flight: status = %d, want 409", code)
	}
}
