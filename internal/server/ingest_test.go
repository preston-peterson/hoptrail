package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/preston-peterson/hoptrail/internal/agent"
	"github.com/preston-peterson/hoptrail/internal/probe"
)

// testAgentToken is the bearer token newFixture configures. Long
// enough to satisfy the config-layer floor, recognizable in failures.
const testAgentToken = "test-agent-token-0123456789abcdef"

// postIngest sends a POST with an optional bearer token. Empty token
// means no Authorization header at all.
func (f *fixture) postIngest(t *testing.T, path, token, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// ---------- auth ----------

func TestIngest_AuthRequired(t *testing.T) {
	f := newFixture(t)
	endpoints := []string{"/api/ingest/heartbeat", "/api/ingest/samples", "/api/ingest/path"}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			if code, _ := f.postIngest(t, ep, "", `{}`); code != http.StatusUnauthorized {
				t.Errorf("no token: status = %d, want 401", code)
			}
			if code, _ := f.postIngest(t, ep, "wrong-token-wrong-token", `{}`); code != http.StatusUnauthorized {
				t.Errorf("wrong token: status = %d, want 401", code)
			}
		})
	}
}

func TestIngest_DisabledWithoutConfiguredTokens(t *testing.T) {
	// A server with zero AgentTokens: even a request presenting some
	// token must 401 — the surface is off, not open.
	f := newFixture(t)
	srv, err := New(Config{
		ListenAddr: "127.0.0.1:0",
		Supervisor: f.supervisor,
		Store:      f.store,
		WebFS:      fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}},
	}, nil)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/ingest/heartbeat",
		strings.NewReader(`{"probe_id":"site-east-pi","version":"v0.3.0","started_at":1,"targets":[]}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAgentToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(res.Body)
		t.Errorf("status = %d, want 401; body=%s", res.StatusCode, body)
	}
}

func TestIngest_MethodNotAllowed(t *testing.T) {
	f := newFixture(t)
	for _, ep := range []string{"/api/ingest/heartbeat", "/api/ingest/samples", "/api/ingest/path"} {
		code, _ := f.get(t, ep)
		if code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: status = %d, want 405", ep, code)
		}
	}
}

// ---------- heartbeat ----------

func TestIngestHeartbeat_RegistersProbeAndReturnsTargets(t *testing.T) {
	f := newFixture(t)
	started := time.Now().Add(-time.Minute).UnixMilli()
	body := fmt.Sprintf(
		`{"probe_id":"site-east-pi","version":"v0.3.0","started_at":%d,"targets":["8.8.8.8","github.com"]}`,
		started)

	code, respBody := f.postIngest(t, "/api/ingest/heartbeat", testAgentToken, body)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, respBody)
	}
	var resp heartbeatResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, respBody)
	}
	if resp.RegisteredAt == 0 {
		t.Error("registered_at missing")
	}
	// Central owns the target set: the response must be the
	// supervisor's list, not an echo of the agent's announcement.
	if len(resp.CentralTargetSet) != 1 || resp.CentralTargetSet[0] != "8.8.8.8" {
		t.Errorf("central_target_set = %v, want [8.8.8.8]", resp.CentralTargetSet)
	}

	probes, err := f.store.ListProbes(context.Background())
	if err != nil {
		t.Fatalf("ListProbes: %v", err)
	}
	if len(probes) != 1 || probes[0].ProbeID != "site-east-pi" {
		t.Fatalf("probes = %+v, want one row for site-east-pi", probes)
	}
	if probes[0].Version == nil || *probes[0].Version != "v0.3.0" {
		t.Errorf("stored version = %v, want v0.3.0", probes[0].Version)
	}
	if probes[0].StartedAt == nil || *probes[0].StartedAt != started {
		t.Errorf("stored started_at = %v, want %d", probes[0].StartedAt, started)
	}
}

func TestIngestHeartbeat_RejectsInvalidProbeIDs(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name string
		id   string
	}{
		{"reserved local", "local"},
		{"reserved all", "all"},
		{"uppercase", "Site-East"},
		{"too short", "x"},
		{"leading dash", "-east"},
		{"too long", strings.Repeat("a", 33)},
		{"empty", ""},
		{"underscore", "site_east"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"probe_id":%q,"version":"v0.3.0","started_at":1,"targets":[]}`, tc.id)
			code, respBody := f.postIngest(t, "/api/ingest/heartbeat", testAgentToken, body)
			if code != http.StatusBadRequest {
				t.Errorf("probe_id %q: status = %d, want 400; body=%s", tc.id, code, respBody)
			}
		})
	}
}

// ---------- samples ----------

func TestIngestSamples_StoresRowsUnderProbeID(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UnixMilli()
	body := fmt.Sprintf(`{
		"probe_id": "site-east-pi",
		"batch_id": "01HZX5J9Q0BATCH",
		"samples": [
			{"target":"8.8.8.8","ttl":1,"ts":%d,"ip":"192.0.2.1","rtt_ms":0.4},
			{"target":"8.8.8.8","ttl":2,"ts":%d,"ip":null,"rtt_ms":0}
		],
		"route_changes": [
			{"target":"8.8.8.8","ttl":3,"ts":%d,"old_ip":"203.0.113.12","new_ip":"203.0.113.45"}
		]
	}`, now, now, now)

	code, respBody := f.postIngest(t, "/api/ingest/samples", testAgentToken, body)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, respBody)
	}
	var resp ingestSamplesResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.BatchID != "01HZX5J9Q0BATCH" {
		t.Errorf("batch_id = %q, want echo of the request's", resp.BatchID)
	}

	// Rows landed under the agent's probe_id, with the wire's float
	// rtt_ms converted to integer µs and null ip stored as NULL.
	rows, err := f.store.DB().Query(
		`SELECT ttl, ip, rtt_us FROM samples WHERE probe_id = 'site-east-pi' ORDER BY ttl`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	type row struct {
		ttl   int
		ip    *string
		rttUs int64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ttl, &r.ip, &r.rttUs); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("agent sample rows = %d, want 2", len(got))
	}
	if got[0].ip == nil || *got[0].ip != "192.0.2.1" || got[0].rttUs != 400 {
		t.Errorf("row[0] = %+v, want ip=192.0.2.1 rtt_us=400", got[0])
	}
	if got[1].ip != nil {
		t.Errorf("row[1].ip = %v, want NULL for the timeout sample", *got[1].ip)
	}

	var rcCount int
	if err := f.store.DB().QueryRow(
		`SELECT COUNT(*) FROM route_changes WHERE probe_id = 'site-east-pi'`).Scan(&rcCount); err != nil {
		t.Fatalf("count route_changes: %v", err)
	}
	if rcCount != 1 {
		t.Errorf("agent route_change rows = %d, want 1", rcCount)
	}
}

func TestIngestSamples_DuplicateBatchAckedWithoutRewrite(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UnixMilli()
	body := fmt.Sprintf(`{
		"probe_id": "site-east-pi",
		"batch_id": "01HZXDUPLICATE",
		"samples": [{"target":"8.8.8.8","ttl":1,"ts":%d,"ip":"192.0.2.1","rtt_ms":1.5}]
	}`, now)

	for i := 0; i < 2; i++ {
		code, respBody := f.postIngest(t, "/api/ingest/samples", testAgentToken, body)
		if code != http.StatusOK {
			t.Fatalf("delivery %d: status = %d, want 200 (duplicates are acked); body=%s", i+1, code, respBody)
		}
	}

	var n int
	if err := f.store.DB().QueryRow(
		`SELECT COUNT(*) FROM samples WHERE probe_id = 'site-east-pi'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("sample rows after duplicate delivery = %d, want 1", n)
	}
}

func TestIngestSamples_ClockSkewRejected(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name string
		ts   int64
	}{
		{"too far past", time.Now().Add(-25 * time.Hour).UnixMilli()},
		{"too far future", time.Now().Add(25 * time.Hour).UnixMilli()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{
				"probe_id": "site-east-pi",
				"batch_id": "01HZXSKEW%d",
				"samples": [{"target":"8.8.8.8","ttl":1,"ts":%d,"ip":"192.0.2.1","rtt_ms":1}]
			}`, tc.ts, tc.ts)
			code, respBody := f.postIngest(t, "/api/ingest/samples", testAgentToken, body)
			if code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%s", code, respBody)
			}
		})
	}

	// Rejected batches must not be recorded as ingested — the agent
	// will fix its clock and the same batch_id must then be accepted.
	var n int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM ingest_log`).Scan(&n); err != nil {
		t.Fatalf("count ingest_log: %v", err)
	}
	if n != 0 {
		t.Errorf("ingest_log rows after rejected batches = %d, want 0", n)
	}
}

func TestIngestSamples_ValidationRejects(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UnixMilli()
	cases := []struct {
		name string
		body string
	}{
		{"missing batch_id", fmt.Sprintf(`{"probe_id":"site-east-pi","samples":[{"target":"8.8.8.8","ttl":1,"ts":%d,"rtt_ms":1}]}`, now)},
		{"ttl zero", fmt.Sprintf(`{"probe_id":"site-east-pi","batch_id":"b1","samples":[{"target":"8.8.8.8","ttl":0,"ts":%d,"rtt_ms":1}]}`, now)},
		{"ttl over 64", fmt.Sprintf(`{"probe_id":"site-east-pi","batch_id":"b2","samples":[{"target":"8.8.8.8","ttl":65,"ts":%d,"rtt_ms":1}]}`, now)},
		{"negative rtt", fmt.Sprintf(`{"probe_id":"site-east-pi","batch_id":"b3","samples":[{"target":"8.8.8.8","ttl":1,"ts":%d,"rtt_ms":-1}]}`, now)},
		{"empty sample target", fmt.Sprintf(`{"probe_id":"site-east-pi","batch_id":"b4","samples":[{"target":"","ttl":1,"ts":%d,"rtt_ms":1}]}`, now)},
		{"route change empty new_ip", fmt.Sprintf(`{"probe_id":"site-east-pi","batch_id":"b5","route_changes":[{"target":"8.8.8.8","ttl":1,"ts":%d,"new_ip":""}]}`, now)},
		{"malformed json", `{"probe_id":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, respBody := f.postIngest(t, "/api/ingest/samples", testAgentToken, tc.body)
			if code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%s", code, respBody)
			}
		})
	}
}

// ---------- agent wire compatibility ----------

// TestIngest_AgentSinkWireCompat runs the REAL agent HTTPSink against
// the REAL server ingest handler — both sides' JSON shapes pinned
// against each other in-process. The agent-package tests use a mock
// central and the server tests use hand-written JSON; this is the one
// place a wire-shape drift between the two packages fails before the
// two-host e2e.
func TestIngest_AgentSinkWireCompat(t *testing.T) {
	f := newFixture(t)

	buf, err := agent.OpenBuffer(filepath.Join(t.TempDir(), "buf.db"), 1)
	if err != nil {
		t.Fatalf("OpenBuffer: %v", err)
	}
	defer buf.Close()
	sink, err := agent.NewHTTPSink(
		agent.NewClient(f.ts.URL, testAgentToken),
		buf,
		agent.SinkConfig{ProbeID: "site-east-pi", IngestInterval: 30 * time.Millisecond},
		nil,
	)
	if err != nil {
		t.Fatalf("NewHTTPSink: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sink.Run(ctx)

	target := netip.MustParseAddr("8.8.8.8")
	hop := netip.MustParseAddr("192.0.2.1")
	if err := sink.WriteSample(probe.Sample{
		Target: target, TargetID: "dns.google", TTL: 4,
		Ts: time.Now(), IP: hop, RTT: 2500 * time.Microsecond,
	}); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}

	// The sink's ticker flushes within ~30ms; poll the store.
	deadline := time.Now().Add(2 * time.Second)
	for {
		var n int
		if err := f.store.DB().QueryRow(
			`SELECT COUNT(*) FROM samples WHERE probe_id = 'site-east-pi' AND target = 'dns.google' AND ttl = 4 AND rtt_us = 2500`,
		).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent-sink sample never landed in central's store via the real wire")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Nothing should have spilled — the POST path was healthy.
	if n, _ := buf.Count(context.Background()); n != 0 {
		t.Errorf("buffer count = %d, want 0", n)
	}
}

// ---------- path ----------

func TestIngestPath_UpsertsSnapshot(t *testing.T) {
	f := newFixture(t)
	ts := time.Now().UnixMilli()
	hops := `[{"ttl":1,"ip":"192.0.2.1","hostname":"gw.example"},{"ttl":2,"ip":"203.0.113.10"}]`
	body := fmt.Sprintf(
		`{"probe_id":"site-east-pi","target":"8.8.8.8","ts":%d,"hop_count":2,"target_ttl":2,"hops":%s}`,
		ts, hops)

	code, respBody := f.postIngest(t, "/api/ingest/path", testAgentToken, body)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, respBody)
	}

	snap, err := f.store.GetPathSnapshot(context.Background(), "site-east-pi", "8.8.8.8")
	if err != nil {
		t.Fatalf("GetPathSnapshot: %v", err)
	}
	if snap == nil {
		t.Fatal("snapshot not stored")
	}
	if snap.HopCount != 2 || snap.TargetTTL != 2 || snap.Ts != ts {
		t.Errorf("snapshot = %+v, want hop_count=2 target_ttl=2 ts=%d", snap, ts)
	}
	// hops stored verbatim (modulo JSON-compact round-trip through
	// RawMessage, which preserves bytes exactly).
	if snap.HopsJSON != hops {
		t.Errorf("hops_json = %s, want %s", snap.HopsJSON, hops)
	}
}

func TestIngestPath_MissingHopsStoredAsEmptyArray(t *testing.T) {
	f := newFixture(t)
	body := `{"probe_id":"site-east-pi","target":"8.8.8.8","ts":1,"hop_count":0,"target_ttl":0}`
	code, respBody := f.postIngest(t, "/api/ingest/path", testAgentToken, body)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, respBody)
	}
	snap, err := f.store.GetPathSnapshot(context.Background(), "site-east-pi", "8.8.8.8")
	if err != nil || snap == nil {
		t.Fatalf("GetPathSnapshot: %v, snap=%v", err, snap)
	}
	if snap.HopsJSON != "[]" {
		t.Errorf("hops_json = %q, want [] when omitted", snap.HopsJSON)
	}
}
