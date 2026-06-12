package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/preston-peterson/hoptrail/internal/probe"
)

// ---------- client ----------

func TestPostJSON_OutcomeClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   Outcome
	}{
		{"200 ok", http.StatusOK, OutcomeOK},
		{"500 retry", http.StatusInternalServerError, OutcomeRetry},
		{"503 retry", http.StatusServiceUnavailable, OutcomeRetry},
		{"400 drop", http.StatusBadRequest, OutcomeDrop},
		{"404 drop", http.StatusNotFound, OutcomeDrop},
		{"401 unauthorized", http.StatusUnauthorized, OutcomeUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
			}))
			defer ts.Close()
			c := NewClient(ts.URL, "tok")
			outcome, _, _ := c.PostJSON(context.Background(), "/api/ingest/samples", []byte(`{}`))
			if outcome != tc.want {
				t.Errorf("status %d: outcome = %v, want %v", tc.status, outcome, tc.want)
			}
		})
	}
}

func TestPostJSON_ConnectionFailureIsRetry(t *testing.T) {
	// A server that's already closed → connection refused.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ts.Close()
	c := NewClient(ts.URL, "tok")
	outcome, _, err := c.PostJSON(context.Background(), "/api/ingest/samples", []byte(`{}`))
	if outcome != OutcomeRetry {
		t.Errorf("outcome = %v, want OutcomeRetry", outcome)
	}
	if err == nil {
		t.Error("want a connection error for logging, got nil")
	}
}

func TestPostJSON_SendsBearerToken(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
	}))
	defer ts.Close()
	c := NewClient(ts.URL+"/", "sekrit-token") // trailing slash must not double up
	if outcome, _, _ := c.PostJSON(context.Background(), "/api/ingest/samples", []byte(`{}`)); outcome != OutcomeOK {
		t.Fatalf("outcome = %v, want OK", outcome)
	}
	if gotAuth != "Bearer sekrit-token" {
		t.Errorf("Authorization = %q, want Bearer sekrit-token", gotAuth)
	}
}

func TestNewBatchID_ShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	var prev string
	for i := 0; i < 50; i++ {
		id, err := NewBatchID()
		if err != nil {
			t.Fatalf("NewBatchID: %v", err)
		}
		if len(id) != 28 { // 12 hex ms + 16 hex random
			t.Fatalf("batch id length = %d, want 28: %q", len(id), id)
		}
		if seen[id] {
			t.Fatalf("duplicate batch id: %q", id)
		}
		seen[id] = true
		// Time-sortable: ids generated later never sort before
		// earlier ones (same-ms ids may interleave on the random
		// suffix, which is fine — central only needs uniqueness).
		if prev != "" && id[:12] < prev[:12] {
			t.Fatalf("timestamp prefix went backwards: %q after %q", id, prev)
		}
		prev = id
	}
}

// ---------- buffer ----------

func testBuffer(t *testing.T) *Buffer {
	t.Helper()
	b, err := OpenBuffer(filepath.Join(t.TempDir(), "buf.db"), 1)
	if err != nil {
		t.Fatalf("OpenBuffer: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestBuffer_OldestFirstRoundTrip(t *testing.T) {
	b := testBuffer(t)
	ctx := context.Background()

	for i, id := range []string{"batch-a", "batch-b", "batch-c"} {
		if _, err := b.Add(ctx, id, int64(1000+i), []byte("payload-"+id)); err != nil {
			t.Fatalf("Add %s: %v", id, err)
		}
	}

	// Drain in age order.
	for _, want := range []string{"batch-a", "batch-b", "batch-c"} {
		id, payload, ok, err := b.Oldest(ctx)
		if err != nil || !ok {
			t.Fatalf("Oldest: ok=%v err=%v", ok, err)
		}
		if id != want {
			t.Fatalf("Oldest = %q, want %q", id, want)
		}
		if string(payload) != "payload-"+want {
			t.Errorf("payload = %q, want payload-%s", payload, want)
		}
		if err := b.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}

	if _, _, ok, err := b.Oldest(ctx); err != nil || ok {
		t.Errorf("drained buffer: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

func TestBuffer_EvictsOldestWhenFull(t *testing.T) {
	b := testBuffer(t) // 1 MB bound
	ctx := context.Background()
	big := make([]byte, 400<<10) // 400 KB each

	for i, id := range []string{"old", "mid", "new"} {
		evicted, err := b.Add(ctx, id, int64(i), big)
		if err != nil {
			t.Fatalf("Add %s: %v", id, err)
		}
		// Third add crosses 1 MB → the oldest goes.
		if id == "new" && evicted != 1 {
			t.Errorf("third Add evicted = %d, want 1", evicted)
		}
	}

	id, _, ok, err := b.Oldest(ctx)
	if err != nil || !ok {
		t.Fatalf("Oldest: ok=%v err=%v", ok, err)
	}
	if id != "mid" {
		t.Errorf("oldest after eviction = %q, want mid (old evicted)", id)
	}
	if n, _ := b.Count(ctx); n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

func TestBuffer_RejectsOversizedSingleBatch(t *testing.T) {
	b := testBuffer(t) // 1 MB bound
	if _, err := b.Add(context.Background(), "huge", 1, make([]byte, 2<<20)); err == nil {
		t.Fatal("2MB batch accepted into a 1MB buffer, want error")
	}
}

// ---------- sink ----------

// mockCentral is a configurable ingest endpoint: set status to force
// failures, read batches to see what landed.
type mockCentral struct {
	mu      sync.Mutex
	status  int // 0 → 200
	batches []batchPayload
}

func (m *mockCentral) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.status != 0 {
			w.WriteHeader(m.status)
			return
		}
		var b batchPayload
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.batches = append(m.batches, b)
		w.Write([]byte(`{"received_at":1,"batch_id":"` + b.BatchID + `"}`)) //nolint:errcheck
	}
}

func (m *mockCentral) setStatus(s int) {
	m.mu.Lock()
	m.status = s
	m.mu.Unlock()
}

func (m *mockCentral) batchCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.batches)
}

func newTestSink(t *testing.T, central *mockCentral) (*HTTPSink, *Buffer) {
	t.Helper()
	ts := httptest.NewServer(central.handler())
	t.Cleanup(ts.Close)
	buf := testBuffer(t)
	sink, err := NewHTTPSink(NewClient(ts.URL, "tok"), buf, SinkConfig{ProbeID: "site-east-pi"}, nil)
	if err != nil {
		t.Fatalf("NewHTTPSink: %v", err)
	}
	return sink, buf
}

func sampleFor(t *testing.T, ttl uint8, rtt time.Duration) probe.Sample {
	t.Helper()
	addr, err := netip.ParseAddr("8.8.8.8")
	if err != nil {
		t.Fatal(err)
	}
	hop, err := netip.ParseAddr("192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	return probe.Sample{
		Target: addr, TargetID: "dns.google", TTL: ttl,
		Ts: time.Now(), IP: hop, RTT: rtt,
	}
}

func TestHTTPSink_FlushPostsBatch(t *testing.T) {
	central := &mockCentral{}
	sink, _ := newTestSink(t, central)

	if err := sink.WriteSample(sampleFor(t, 1, 1500*time.Microsecond)); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}
	if err := sink.WriteRouteChange(probe.RouteChange{
		TargetID: "dns.google", TTL: 3, Ts: time.Now(),
		NewIP: netip.MustParseAddr("203.0.113.45"),
	}); err != nil {
		t.Fatalf("WriteRouteChange: %v", err)
	}
	sink.flushOnce(context.Background())

	central.mu.Lock()
	defer central.mu.Unlock()
	if len(central.batches) != 1 {
		t.Fatalf("central received %d batches, want 1", len(central.batches))
	}
	b := central.batches[0]
	if b.ProbeID != "site-east-pi" || b.BatchID == "" {
		t.Errorf("batch identity = %q/%q", b.ProbeID, b.BatchID)
	}
	if len(b.Samples) != 1 || b.Samples[0].Target != "dns.google" || b.Samples[0].RTTms != 1.5 {
		t.Errorf("samples = %+v, want one dns.google sample at 1.5ms", b.Samples)
	}
	if b.Samples[0].IP == nil || *b.Samples[0].IP != "192.0.2.1" {
		t.Errorf("sample ip = %v, want 192.0.2.1", b.Samples[0].IP)
	}
	if len(b.RouteChanges) != 1 || b.RouteChanges[0].NewIP != "203.0.113.45" {
		t.Errorf("route_changes = %+v", b.RouteChanges)
	}
}

func TestHTTPSink_EmptyFlushPostsNothing(t *testing.T) {
	central := &mockCentral{}
	sink, _ := newTestSink(t, central)
	sink.flushOnce(context.Background())
	if n := central.batchCount(); n != 0 {
		t.Errorf("central received %d batches from an empty flush, want 0", n)
	}
}

func TestHTTPSink_SpillsToBufferWhenCentralDown(t *testing.T) {
	central := &mockCentral{}
	sink, buf := newTestSink(t, central)
	central.setStatus(http.StatusInternalServerError)

	if err := sink.WriteSample(sampleFor(t, 1, time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	sink.flushOnce(context.Background())

	if n, _ := buf.Count(context.Background()); n != 1 {
		t.Fatalf("buffer count = %d, want 1 (batch spilled)", n)
	}

	// Central recovers; the drain delivers the SAME payload (stable
	// batch_id) and empties the buffer.
	central.setStatus(0)
	if healthy := sink.drainBuffer(context.Background()); !healthy {
		t.Error("drainBuffer reported unhealthy against a recovered central")
	}
	if n, _ := buf.Count(context.Background()); n != 0 {
		t.Errorf("buffer count after drain = %d, want 0", n)
	}
	central.mu.Lock()
	defer central.mu.Unlock()
	if len(central.batches) != 1 || len(central.batches[0].Samples) != 1 {
		t.Fatalf("central received %+v, want the one spilled batch", central.batches)
	}
}

func TestHTTPSink_RejectedBatchDropped(t *testing.T) {
	central := &mockCentral{}
	sink, buf := newTestSink(t, central)
	central.setStatus(http.StatusBadRequest)

	if err := sink.WriteSample(sampleFor(t, 1, time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	sink.flushOnce(context.Background())

	if n, _ := buf.Count(context.Background()); n != 0 {
		t.Errorf("buffer count = %d, want 0 (4xx batches are dropped, not buffered)", n)
	}
}

func TestHTTPSink_PoisonBufferedBatchDeleted(t *testing.T) {
	central := &mockCentral{}
	sink, buf := newTestSink(t, central)

	// A buffered batch central rejects (e.g. older than the 24h skew
	// bound after a long partition) must be deleted, not wedge the
	// queue forever.
	if _, err := buf.Add(context.Background(), "poison", 1, []byte(`{"probe_id":"site-east-pi","batch_id":"poison","samples":[],"route_changes":[]}`)); err != nil {
		t.Fatal(err)
	}
	central.setStatus(http.StatusBadRequest)
	sink.drainBuffer(context.Background())

	if n, _ := buf.Count(context.Background()); n != 0 {
		t.Errorf("buffer count = %d, want 0 (poison batch deleted)", n)
	}
}

func TestHTTPSink_UnauthorizedDeliversFatal(t *testing.T) {
	central := &mockCentral{}
	sink, _ := newTestSink(t, central)
	central.setStatus(http.StatusUnauthorized)

	if err := sink.WriteSample(sampleFor(t, 1, time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	sink.flushOnce(context.Background())

	select {
	case err := <-sink.Fatal():
		if !strings.Contains(err.Error(), "401") {
			t.Errorf("fatal error = %v, want a 401 mention", err)
		}
	default:
		t.Fatal("Fatal() empty after a 401, want the error delivered")
	}
}

func TestHTTPSink_RunFinalFlushOnCancel(t *testing.T) {
	central := &mockCentral{}
	sink, _ := newTestSink(t, central)

	ctx, cancel := context.WithCancel(context.Background())
	go sink.Run(ctx)

	if err := sink.WriteSample(sampleFor(t, 1, time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-sink.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("sink did not exit within 2s of cancel")
	}
	if n := central.batchCount(); n != 1 {
		t.Errorf("central received %d batches, want 1 (final flush on shutdown)", n)
	}
}

// ---------- heartbeat ----------

func TestRunHeartbeat_FirstBeatImmediateAndTargetSetDelivered(t *testing.T) {
	var gotBody []byte
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotBody = make([]byte, r.ContentLength)
		r.Body.Read(gotBody) //nolint:errcheck
		mu.Unlock()
		w.Write([]byte(`{"registered_at":1716412800123,"central_target_set":["8.8.8.8","1.1.1.1"]}`)) //nolint:errcheck
	}))
	defer ts.Close()

	targetSet := make(chan []string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunHeartbeat(ctx, NewClient(ts.URL, "tok"), HeartbeatConfig{
			ProbeID:   "site-east-pi",
			Version:   "v0.3.0-test",
			StartedAt: time.UnixMilli(1716412800000),
			Interval:  time.Hour, // never reaches a second beat in-test
			Targets:   func() []string { return []string{"8.8.8.8"} },
			OnTargetSet: func(set []string) {
				select {
				case targetSet <- set:
				default:
				}
			},
		}, nil)
	}()

	select {
	case set := <-targetSet:
		if len(set) != 2 || set[0] != "8.8.8.8" || set[1] != "1.1.1.1" {
			t.Errorf("target set = %v, want [8.8.8.8 1.1.1.1]", set)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no target set within 2s — first beat should be immediate")
	}

	mu.Lock()
	var sent heartbeatPayload
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("decode sent heartbeat: %v; body=%s", err, gotBody)
	}
	mu.Unlock()
	if sent.ProbeID != "site-east-pi" || sent.StartedAt != 1716412800000 {
		t.Errorf("heartbeat payload = %+v", sent)
	}
	if len(sent.Targets) != 1 || sent.Targets[0] != "8.8.8.8" {
		t.Errorf("announced targets = %v, want [8.8.8.8]", sent.Targets)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Errorf("RunHeartbeat returned %v on ctx cancel, want nil", err)
	}
}

func TestBeatOnce_FatalClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantFatal bool
		wantOK    bool
	}{
		{"success", http.StatusOK, false, true},
		{"central down retries", http.StatusInternalServerError, false, false},
		{"401 fatal", http.StatusUnauthorized, true, false},
		{"400 fatal", http.StatusBadRequest, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status != http.StatusOK {
					w.WriteHeader(tc.status)
					return
				}
				w.Write([]byte(`{"registered_at":1,"central_target_set":[]}`)) //nolint:errcheck
			}))
			defer ts.Close()
			ok, err := beatOnce(context.Background(), NewClient(ts.URL, "tok"), HeartbeatConfig{
				ProbeID: "site-east-pi", StartedAt: time.Now(),
			})
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if (err != nil) != tc.wantFatal {
				t.Errorf("fatal = %v, wantFatal %v", err, tc.wantFatal)
			}
		})
	}
}
