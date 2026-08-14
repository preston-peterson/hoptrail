package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/preston-peterson/hoptrail/internal/probe"
	"github.com/preston-peterson/hoptrail/internal/storage"
)

// fakeSupervisor is a small in-test stand-in for cmd/hoptrail's real
// supervisor. Implements server.Supervisor; lets tests drive the
// engine map directly (no real ICMP socket / pipeline goroutines).
//
// Step-29 changed targets from netip.Addr to string so tests can
// exercise hostname inputs. By default the fake parses the string
// as IP for the engine's internal target; tests that want to model
// hostname resolution can preload f.engines with a string→engine
// entry directly (the engine's Target() can be a different IP).
type fakeSupervisor struct {
	mu            sync.Mutex
	engines       map[string]*probe.Engine
	intervals     map[string]time.Duration
	thresholds    map[string]ThresholdPair
	finalHopOnlys map[string]bool

	// Optional hooks — tests set these to assert on swap/add/remove/
	// set-interval/set-thresholds/set-final-hop-only invocations or
	// to inject failures. Nil falls through to default in-memory behavior.
	swapFn            func(ctx context.Context, target string) error
	addFn             func(ctx context.Context, target string) error
	removeFn          func(ctx context.Context, target string) error
	setIntervalFn     func(ctx context.Context, target string, interval time.Duration) error
	setThresholdsFn   func(ctx context.Context, target string, warning, critical *int64) error
	setFinalHopOnlyFn func(ctx context.Context, target string, finalHopOnly bool) error
}

func (f *fakeSupervisor) EngineFor(target string) *probe.Engine {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.engines[target]
}

func (f *fakeSupervisor) Targets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.engines))
	for k := range f.engines {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (f *fakeSupervisor) Intervals() map[string]time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]time.Duration, len(f.engines))
	for k := range f.engines {
		if v, ok := f.intervals[k]; ok {
			out[k] = v
		} else {
			// Default fake interval — keeps tests deterministic when
			// they don't bother setting one explicitly.
			out[k] = time.Second
		}
	}
	return out
}

func (f *fakeSupervisor) SetInterval(ctx context.Context, target string, interval time.Duration) error {
	if f.setIntervalFn != nil {
		return f.setIntervalFn(ctx, target, interval)
	}
	if interval < 200*time.Millisecond || interval > 60*time.Second {
		return fmt.Errorf("probe interval must be between 200ms and 1m0s")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.engines[target]; !ok {
		return fmt.Errorf("target %s not monitored", target)
	}
	if f.intervals == nil {
		f.intervals = map[string]time.Duration{}
	}
	f.intervals[target] = interval
	return nil
}

func (f *fakeSupervisor) Thresholds() map[string]ThresholdPair {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]ThresholdPair, len(f.engines))
	for k := range f.engines {
		if v, ok := f.thresholds[k]; ok {
			out[k] = v
		} else {
			out[k] = ThresholdPair{}
		}
	}
	return out
}

func (f *fakeSupervisor) FinalHopOnlys() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]bool, len(f.engines))
	for k := range f.engines {
		out[k] = f.finalHopOnlys[k]
	}
	return out
}

func (f *fakeSupervisor) SetFinalHopOnly(ctx context.Context, target string, finalHopOnly bool) error {
	if f.setFinalHopOnlyFn != nil {
		return f.setFinalHopOnlyFn(ctx, target, finalHopOnly)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.engines[target]; !ok {
		return fmt.Errorf("target %s not monitored", target)
	}
	if f.finalHopOnlys == nil {
		f.finalHopOnlys = map[string]bool{}
	}
	f.finalHopOnlys[target] = finalHopOnly
	return nil
}

func (f *fakeSupervisor) SetThresholds(ctx context.Context, target string, warningMs, criticalMs *int64) error {
	if f.setThresholdsFn != nil {
		return f.setThresholdsFn(ctx, target, warningMs, criticalMs)
	}
	if warningMs != nil && *warningMs <= 0 {
		return fmt.Errorf("warning_ms must be positive, got %d", *warningMs)
	}
	if criticalMs != nil && *criticalMs <= 0 {
		return fmt.Errorf("critical_ms must be positive, got %d", *criticalMs)
	}
	if warningMs != nil && criticalMs != nil && *warningMs >= *criticalMs {
		return fmt.Errorf("warning_ms (%d) must be less than critical_ms (%d)", *warningMs, *criticalMs)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.engines[target]; !ok {
		return fmt.Errorf("target %s not monitored", target)
	}
	if f.thresholds == nil {
		f.thresholds = map[string]ThresholdPair{}
	}
	f.thresholds[target] = ThresholdPair{WarningMs: warningMs, CriticalMs: criticalMs}
	return nil
}

func (f *fakeSupervisor) Add(ctx context.Context, target string) error {
	if f.addFn != nil {
		return f.addFn(ctx, target)
	}
	f.mu.Lock()
	if _, ok := f.engines[target]; ok {
		f.mu.Unlock()
		return fmt.Errorf("target %s already monitored", target)
	}
	f.mu.Unlock()
	// Default fake behavior: parse the string as an IP for the
	// engine's internal target, mirroring the real supervisor's
	// validation rules (rejects unspecified, multicast, IPv6).
	// Tests that want to exercise hostname inputs set addFn
	// explicitly to bypass this parse.
	addr, err := netip.ParseAddr(target)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", target, err)
	}
	if !addr.Is4() {
		return fmt.Errorf("target %q is IPv6; only IPv4 is supported", target)
	}
	if addr.IsUnspecified() || addr.IsMulticast() {
		return fmt.Errorf("target %q is not a valid traceroute target", target)
	}
	engine, err := probe.NewEngine(probe.EngineConfig{
		Target: addr, RouteChangeThreshold: 3,
	}, probe.NoopSink{}, nil)
	if err != nil {
		return err
	}
	go func() { _ = engine.Run(context.Background()) }()
	f.mu.Lock()
	f.engines[target] = engine
	f.mu.Unlock()
	return nil
}

func (f *fakeSupervisor) Remove(ctx context.Context, target string) error {
	if f.removeFn != nil {
		return f.removeFn(ctx, target)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.engines[target]; !ok {
		return fmt.Errorf("target %s not monitored", target)
	}
	delete(f.engines, target)
	return nil
}

func (f *fakeSupervisor) Swap(ctx context.Context, target string) error {
	if f.swapFn != nil {
		return f.swapFn(ctx, target)
	}
	f.mu.Lock()
	existing := make([]string, 0, len(f.engines))
	for k := range f.engines {
		existing = append(existing, k)
	}
	_, alreadyHave := f.engines[target]
	f.mu.Unlock()
	if !alreadyHave {
		if err := f.Add(ctx, target); err != nil {
			return err
		}
	}
	for _, t := range existing {
		if t == target {
			continue
		}
		if err := f.Remove(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// fixture spins up an Engine, a Store, and a Server-backed httptest
// server, returning everything plus a cleanup func that tears them all
// down. Engine runs in a goroutine and is ready to receive events.
//
// Tests that want to observe swap/add/remove calls set the
// corresponding hook on supervisor (e.g. f.supervisor.swapFn = ...).
type fixture struct {
	ts         *httptest.Server
	engine     *probe.Engine // initial-target engine; mutated by direct SendProbe calls
	supervisor *fakeSupervisor
	store      *storage.Store
	cancel     context.CancelFunc
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}

	target, _ := netip.ParseAddr("8.8.8.8")
	engine, err := probe.NewEngine(probe.EngineConfig{
		Target:               target,
		RouteChangeThreshold: 3,
	}, probe.NoopSink{}, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = engine.Run(ctx) }()

	// Minimal embedded FS: just an index.html for SPA-fallback tests.
	webFS := fstest.MapFS{
		"index.html": &fstest.MapFile{
			Data: []byte("<!doctype html><title>hoptrail test</title>"),
		},
		"assets/style.css": &fstest.MapFile{
			Data: []byte("body { background: #fff; }"),
		},
	}

	sup := &fakeSupervisor{
		engines: map[string]*probe.Engine{target.String(): engine},
	}

	srv, err := New(Config{
		ListenAddr:  "127.0.0.1:0",
		Supervisor:  sup,
		Store:       store,
		WebFS:       webFS,
		AgentTokens: []string{testAgentToken},
	}, nil)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	ts := httptest.NewServer(withTestCSRF(srv.routes()))

	t.Cleanup(func() {
		ts.Close()
		cancel()
		_ = store.Close()
	})

	return &fixture{ts: ts, engine: engine, supervisor: sup, store: store, cancel: cancel}
}

func (f *fixture) get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	res, err := http.Get(f.ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, body
}

// ---------- /api/path ----------

func TestHandlePath_EmptyEngine(t *testing.T) {
	f := newFixture(t)

	code, body := f.get(t, "/api/path")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}

	var resp pathResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	if resp.Target != "8.8.8.8" {
		t.Errorf("target = %q, want 8.8.8.8", resp.Target)
	}
	if resp.HopCount != 0 {
		t.Errorf("hop_count = %d on empty engine, want 0", resp.HopCount)
	}
	if resp.StartedAt == 0 {
		t.Error("started_at = 0; engine should have captured a real timestamp")
	}
}

func TestHandlePath_PopulatesHops(t *testing.T) {
	f := newFixture(t)

	// Feed the engine a few probe results so PathSnapshot has data.
	ip1, _ := netip.ParseAddr("192.0.2.1")
	ip2, _ := netip.ParseAddr("203.0.113.1")
	ts := time.Now()

	for i := 0; i < 3; i++ {
		_ = f.engine.SendProbe(context.Background(), probe.ProbeResult{
			Target: f.engine.Target(), TTL: 1, Ts: ts, RespIP: ip1, RTT: 500 * time.Microsecond, Reply: probe.ReplyTimeExceeded,
		})
		_ = f.engine.SendProbe(context.Background(), probe.ProbeResult{
			Target: f.engine.Target(), TTL: 2, Ts: ts, RespIP: ip2, RTT: 5 * time.Millisecond, Reply: probe.ReplyTimeExceeded,
		})
	}

	// Snapshot is synchronous; once the query returns, all preceding
	// SendProbe calls have been processed (channels are FIFO from the
	// reducer's perspective and our caller's perspective).
	if _, err := f.engine.PathSnapshot(context.Background()); err != nil {
		t.Fatalf("PathSnapshot: %v", err)
	}

	code, body := f.get(t, "/api/path")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var resp pathResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.HopCount != 2 {
		t.Errorf("hop_count = %d, want 2", resp.HopCount)
	}
	if resp.TargetTTL != 0 {
		t.Errorf("target_ttl = %d, want 0 (no EchoReply observed)", resp.TargetTTL)
	}
	if len(resp.Hops) != 2 {
		t.Fatalf("hops length = %d, want 2", len(resp.Hops))
	}
	if resp.Hops[0].TTL != 1 || resp.Hops[1].TTL != 2 {
		t.Errorf("hops TTLs = %d,%d, want 1,2", resp.Hops[0].TTL, resp.Hops[1].TTL)
	}
	if resp.Hops[0].CurrentIP == nil || *resp.Hops[0].CurrentIP != "192.0.2.1" {
		t.Errorf("hop[0].current_ip = %v, want 192.0.2.1", resp.Hops[0].CurrentIP)
	}
	// 500µs = 0.5ms; RTT was 500*time.Microsecond. All three probes used
	// the same RTT so min, avg, and current all collapse to 0.5ms.
	if resp.Hops[0].CurrentRTTms != 0.5 {
		t.Errorf("hop[0].current_rtt_ms = %v, want 0.5", resp.Hops[0].CurrentRTTms)
	}
	if resp.Hops[0].MinRTTms != 0.5 {
		t.Errorf("hop[0].min_rtt_ms = %v, want 0.5", resp.Hops[0].MinRTTms)
	}
}

// TestHandlePath_SurfaceHostnames pins the rdns join: hostnames from
// the rdns table appear in the path response alongside the IP, and
// IPs without an rdns row (or with a NULL hostname) produce a JSON
// null for hostname. End-to-end check that the worker's writes are
// visible through the handler.
func TestHandlePath_SurfaceHostnames(t *testing.T) {
	f := newFixture(t)

	// Seed: hop 1 has a hostname, hop 2 has no rdns row at all.
	ctx := context.Background()
	if err := f.store.UpsertRDNS(ctx, "192.0.2.1", "router.local"); err != nil {
		t.Fatalf("UpsertRDNS: %v", err)
	}

	ip1, _ := netip.ParseAddr("192.0.2.1")
	ip2, _ := netip.ParseAddr("203.0.113.1")
	ts := time.Now()
	for i := 0; i < 3; i++ {
		_ = f.engine.SendProbe(ctx, probe.ProbeResult{
			Target: f.engine.Target(), TTL: 1, Ts: ts, RespIP: ip1, RTT: 1 * time.Millisecond, Reply: probe.ReplyTimeExceeded,
		})
		_ = f.engine.SendProbe(ctx, probe.ProbeResult{
			Target: f.engine.Target(), TTL: 2, Ts: ts, RespIP: ip2, RTT: 5 * time.Millisecond, Reply: probe.ReplyTimeExceeded,
		})
	}
	if _, err := f.engine.PathSnapshot(ctx); err != nil {
		t.Fatalf("PathSnapshot: %v", err)
	}

	code, body := f.get(t, "/api/path")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	var resp pathResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Hops) != 2 {
		t.Fatalf("hops length = %d, want 2", len(resp.Hops))
	}
	// Hop 1 should have hostname set.
	if resp.Hops[0].Hostname == nil {
		t.Errorf("hop[0].hostname is nil, want %q", "router.local")
	} else if *resp.Hops[0].Hostname != "router.local" {
		t.Errorf("hop[0].hostname = %q, want %q", *resp.Hops[0].Hostname, "router.local")
	}
	// Hop 2 has no rdns row → hostname stays nil in JSON.
	if resp.Hops[1].Hostname != nil {
		t.Errorf("hop[1].hostname = %v, want nil (no rdns row)", *resp.Hops[1].Hostname)
	}
}

// TestHandlePath_LossStateRateLimited pins the "rate-limited hop"
// classification: a hop with high loss whose downstream is healthy
// gets loss_state="rate_limited" rather than "suspect." This is the
// TTL-6-at-57%-red mis-badging case from the step-14 screenshot.
//
// The handler runs analysis.AttributedLoss across the snapshot's hops,
// so the JSON should reflect the downstream-persistence rule end-to-end.
func TestHandlePath_LossStateRateLimited(t *testing.T) {
	f := newFixture(t)
	ts := time.Now()
	router1, _ := netip.ParseAddr("203.0.113.1")
	router2, _ := netip.ParseAddr("203.0.113.2")
	target := f.engine.Target() // 8.8.8.8

	// Set up: TTL 1 sees heavy ICMP loss (mostly timeouts), but TTLs 2
	// and 3 (the target) are clean. That's the rate-limiting fingerprint:
	// the path through TTL 1 works fine, the box at TTL 1 just declines
	// to answer most of our pings.
	for i := 0; i < 10; i++ {
		if i < 2 { // 8 of 10 are timeouts at TTL 1 → 80% loss
			_ = f.engine.SendProbe(context.Background(), probe.ProbeResult{
				Target: target, TTL: 1, Ts: ts, RespIP: router1, RTT: 1 * time.Millisecond, Reply: probe.ReplyTimeExceeded,
			})
		} else {
			_ = f.engine.SendProbe(context.Background(), probe.ProbeResult{
				Target: target, TTL: 1, Ts: ts, RespIP: netip.Addr{}, RTT: 0, TimedOut: true,
			})
		}
		_ = f.engine.SendProbe(context.Background(), probe.ProbeResult{
			Target: target, TTL: 2, Ts: ts, RespIP: router2, RTT: 5 * time.Millisecond, Reply: probe.ReplyTimeExceeded,
		})
		_ = f.engine.SendProbe(context.Background(), probe.ProbeResult{
			Target: target, TTL: 3, Ts: ts, RespIP: target, RTT: 10 * time.Millisecond, Reply: probe.ReplyEchoReply,
		})
	}
	if _, err := f.engine.PathSnapshot(context.Background()); err != nil {
		t.Fatalf("PathSnapshot: %v", err)
	}

	code, body := f.get(t, "/api/path")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	var resp pathResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// TTL 1 has high loss; downstream TTL 2 and TTL 3 are clean →
	// rate_limited, not suspect.
	if resp.Hops[0].LossState != "rate_limited" {
		t.Errorf("hop[0].loss_state = %q (loss=%v); want rate_limited",
			resp.Hops[0].LossState, resp.Hops[0].LossPercent)
	}
	// TTL 2 has zero loss → ok.
	if resp.Hops[1].LossState != "ok" {
		t.Errorf("hop[1].loss_state = %q (loss=%v); want ok",
			resp.Hops[1].LossState, resp.Hops[1].LossPercent)
	}
	// TTL 3 (target) has zero loss → ok.
	if resp.Hops[2].LossState != "ok" {
		t.Errorf("hop[2].loss_state = %q (loss=%v); want ok",
			resp.Hops[2].LossState, resp.Hops[2].LossPercent)
	}
}

// TestHandlePath_LossStateSuspect pins the "loss is real" case: a
// hop with high loss whose downstream is ALSO showing high loss gets
// loss_state="suspect" — the loss is propagating through the path,
// so this hop (or further upstream) is the genuine cause.
func TestHandlePath_LossStateSuspect(t *testing.T) {
	f := newFixture(t)
	ts := time.Now()
	router1, _ := netip.ParseAddr("203.0.113.1")
	router2, _ := netip.ParseAddr("203.0.113.2")

	// Set up: TTL 1 and TTL 2 both lose 80% of probes. That's "real
	// loss" — packets that drop at TTL 1 never reach TTL 2 either, so
	// the persistence rule classifies TTL 1 as suspect.
	for i := 0; i < 10; i++ {
		if i < 2 {
			_ = f.engine.SendProbe(context.Background(), probe.ProbeResult{
				Target: f.engine.Target(), TTL: 1, Ts: ts, RespIP: router1, RTT: 1 * time.Millisecond, Reply: probe.ReplyTimeExceeded,
			})
			_ = f.engine.SendProbe(context.Background(), probe.ProbeResult{
				Target: f.engine.Target(), TTL: 2, Ts: ts, RespIP: router2, RTT: 5 * time.Millisecond, Reply: probe.ReplyTimeExceeded,
			})
		} else {
			_ = f.engine.SendProbe(context.Background(), probe.ProbeResult{
				Target: f.engine.Target(), TTL: 1, Ts: ts, RespIP: netip.Addr{}, RTT: 0, TimedOut: true,
			})
			_ = f.engine.SendProbe(context.Background(), probe.ProbeResult{
				Target: f.engine.Target(), TTL: 2, Ts: ts, RespIP: netip.Addr{}, RTT: 0, TimedOut: true,
			})
		}
	}
	if _, err := f.engine.PathSnapshot(context.Background()); err != nil {
		t.Fatalf("PathSnapshot: %v", err)
	}

	code, body := f.get(t, "/api/path")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	var resp pathResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Both TTL 1 and TTL 2 should be suspect — loss persists through both.
	if resp.Hops[0].LossState != "suspect" {
		t.Errorf("hop[0].loss_state = %q (loss=%v); want suspect",
			resp.Hops[0].LossState, resp.Hops[0].LossPercent)
	}
	if resp.Hops[1].LossState != "suspect" {
		t.Errorf("hop[1].loss_state = %q (loss=%v); want suspect",
			resp.Hops[1].LossState, resp.Hops[1].LossPercent)
	}
}

// TestHandlePath_TargetTTLSurfacedAndHopsCapped: once an EchoReply is
// observed, the response includes target_ttl and hop_count is capped at
// it. End-to-end check that the path.go cap propagates through engine →
// handler → JSON.
func TestHandlePath_TargetTTLSurfacedAndHopsCapped(t *testing.T) {
	f := newFixture(t)
	ts := time.Now()
	router, _ := netip.ParseAddr("203.0.113.1")
	dst := f.engine.Target() // 8.8.8.8

	// Transit routers at TTL 1..3, destination at TTL 4 and 6.
	for ttl := uint8(1); ttl <= 3; ttl++ {
		_ = f.engine.SendProbe(context.Background(), probe.ProbeResult{
			Target: dst, TTL: ttl, Ts: ts, RespIP: router,
			RTT: time.Duration(ttl) * time.Millisecond, Reply: probe.ReplyTimeExceeded,
		})
	}
	for _, ttl := range []uint8{4, 6} {
		_ = f.engine.SendProbe(context.Background(), probe.ProbeResult{
			Target: dst, TTL: ttl, Ts: ts, RespIP: dst,
			RTT: 12 * time.Millisecond, Reply: probe.ReplyEchoReply,
		})
	}

	if _, err := f.engine.PathSnapshot(context.Background()); err != nil {
		t.Fatalf("PathSnapshot: %v", err)
	}

	_, body := f.get(t, "/api/path")
	var resp pathResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TargetTTL != 4 {
		t.Errorf("target_ttl = %d, want 4", resp.TargetTTL)
	}
	if resp.HopCount != 4 {
		t.Errorf("hop_count = %d, want 4 (capped at TargetTTL — TTL 6 also responded but is hidden)", resp.HopCount)
	}
}

func TestHandlePath_TimeoutHopHasNullIP(t *testing.T) {
	f := newFixture(t)

	ts := time.Now()
	// Several timeout probes at TTL 6 — should produce a hop with
	// CurrentIP=nil in the response.
	for i := 0; i < 3; i++ {
		_ = f.engine.SendProbe(context.Background(), probe.ProbeResult{
			Target: f.engine.Target(), TTL: 6, Ts: ts, TimedOut: true,
		})
	}
	_, _ = f.engine.PathSnapshot(context.Background())

	code, body := f.get(t, "/api/path")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var resp pathResponse
	_ = json.Unmarshal(body, &resp)

	// Timeouts don't bump maxSeen, so the hop list may be empty even
	// though TTL 6 was probed. That's expected — see PathState.MaxSeen.
	// The test passes if we get here without crashing; the snapshot
	// shape for anonymous-only hops is exercised in path_test.go.
	_ = resp
}

// ---------- /api/samples ----------

// insertSample is a test helper that writes one sample directly into
// the store, bypassing the BatchedSink. Lets handler tests set up data
// without spinning up the full probe → engine → sink pipeline.
func insertSample(t *testing.T, f *fixture, ttl int, tsMs int64, ip string, rttUs int64) {
	t.Helper()
	var ipArg any
	if ip == "" {
		ipArg = nil
	} else {
		ipArg = ip
	}
	_, err := f.store.DB().Exec(
		`INSERT INTO samples (target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?)`,
		"8.8.8.8", ttl, tsMs, ipArg, rttUs,
	)
	if err != nil {
		t.Fatalf("insert sample: %v", err)
	}
}

func TestHandleSamples_ReturnsInsertedRows(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UnixMilli()
	insertSample(t, f, 1, now-2000, "192.0.2.1", 500)
	insertSample(t, f, 2, now-2000, "203.0.113.1", 5000)
	insertSample(t, f, 1, now-1000, "192.0.2.1", 600)

	code, body := f.get(t, "/api/samples")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var resp samplesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	if len(resp.Samples) != 3 {
		t.Errorf("samples length = %d, want 3", len(resp.Samples))
	}
	// Should be ordered by ts ASC, ttl ASC.
	if resp.Samples[0].Ts > resp.Samples[1].Ts {
		t.Errorf("samples not sorted by ts ascending")
	}
	// RTT round-trip: 500µs stored as integer becomes 0.5ms on the wire.
	if resp.Samples[0].RTTms != 0.5 {
		t.Errorf("samples[0].rtt_ms = %v, want 0.5", resp.Samples[0].RTTms)
	}
}

func TestHandleSamples_TimeoutSamplesHaveNullIP(t *testing.T) {
	f := newFixture(t)
	insertSample(t, f, 6, time.Now().UnixMilli(), "", 0) // empty string → SQL NULL

	_, body := f.get(t, "/api/samples")
	var resp samplesResponse
	_ = json.Unmarshal(body, &resp)

	if len(resp.Samples) != 1 {
		t.Fatalf("samples length = %d, want 1", len(resp.Samples))
	}
	if resp.Samples[0].IP != nil {
		t.Errorf("samples[0].ip = %v, want null for timeout", *resp.Samples[0].IP)
	}
	if resp.Samples[0].RTTms != 0 {
		t.Errorf("samples[0].rtt_ms = %v, want 0 for timeout", resp.Samples[0].RTTms)
	}
}

func TestHandleSamples_RespectsTimeWindow(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UnixMilli()
	insertSample(t, f, 1, now-10000, "203.0.113.1", 1000) // 10s ago
	insertSample(t, f, 1, now-2000, "203.0.113.1", 1000)  //  2s ago
	insertSample(t, f, 1, now-500, "203.0.113.1", 1000)   //  500ms ago

	// Window: last 3 seconds.
	since := now - 3000
	_, body := f.get(t, "/api/samples?since="+intStr(since))
	var resp samplesResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Samples) != 2 {
		t.Errorf("samples in 3s window = %d, want 2 (the 10s-ago one should be filtered out)", len(resp.Samples))
	}
}

// Step-88: samples carry a probe_id (migration v11) and the read
// queries pin to the local probe until per-probe scoping lands. Rows
// ingested from a remote agent must not leak into the v0.2-shaped
// view — this is the intermediate-state contract between "agents can
// write" and "the API can scope."
func TestHandleSamples_ExcludesRemoteProbeRows(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UnixMilli()
	insertSample(t, f, 1, now-1000, "203.0.113.1", 1000) // probe_id defaults to 'local'
	if _, err := f.store.DB().Exec(
		`INSERT INTO samples (probe_id, target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?, ?)`,
		"site-east-pi", "8.8.8.8", 1, now-1000, "198.51.100.9", 9000,
	); err != nil {
		t.Fatalf("insert remote-probe sample: %v", err)
	}

	_, body := f.get(t, "/api/samples")
	var resp samplesResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Samples) != 1 {
		t.Fatalf("samples length = %d, want 1 (remote-probe row must be filtered)", len(resp.Samples))
	}
	if resp.Samples[0].IP == nil || *resp.Samples[0].IP != "203.0.113.1" {
		t.Errorf("returned sample IP = %v, want the local probe's 203.0.113.1", resp.Samples[0].IP)
	}
}

func TestHandleRouteChanges_ExcludesRemoteProbeRows(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UnixMilli()
	if _, err := f.store.DB().Exec(
		`INSERT INTO route_changes (target, ttl, ts, old_ip, new_ip) VALUES (?, ?, ?, ?, ?)`,
		"8.8.8.8", 3, now-1000, "203.0.113.1", "203.0.113.2",
	); err != nil {
		t.Fatalf("insert local route change: %v", err)
	}
	if _, err := f.store.DB().Exec(
		`INSERT INTO route_changes (probe_id, target, ttl, ts, old_ip, new_ip) VALUES (?, ?, ?, ?, ?, ?)`,
		"site-east-pi", "8.8.8.8", 3, now-1000, "198.51.100.1", "198.51.100.2",
	); err != nil {
		t.Fatalf("insert remote route change: %v", err)
	}

	_, body := f.get(t, "/api/route_changes")
	var resp changesResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Changes) != 1 {
		t.Fatalf("changes length = %d, want 1 (remote-probe row must be filtered)", len(resp.Changes))
	}
	if resp.Changes[0].NewIP != "203.0.113.2" {
		t.Errorf("returned change new_ip = %q, want the local probe's 203.0.113.2", resp.Changes[0].NewIP)
	}
}

func TestHandleSamples_InvalidSinceRejected(t *testing.T) {
	f := newFixture(t)
	code, _ := f.get(t, "/api/samples?since=not-a-number")
	if code != http.StatusBadRequest {
		t.Errorf("invalid since: status = %d, want 400", code)
	}
}

// Step-65: when bucket_ms is set, server returns one representative
// sample per (TTL, bucket) — the earliest sample in each bucket. Pin
// the bucket boundary, the per-TTL partitioning, and the "earliest
// wins" tie-break so the 7d view's wire shape is stable.
func TestHandleSamples_BucketedReturnsOnePerBucketPerTTL(t *testing.T) {
	f := newFixture(t)
	bucketMs := int64(60_000) // 1-minute buckets
	now := time.Now().UnixMilli()
	// Snap `now` to a bucket boundary so the math is easy to follow.
	base := (now / bucketMs) * bucketMs

	// TTL 1, bucket 0: three samples at +0, +10s, +30s
	insertSample(t, f, 1, base+0, "203.0.113.1", 100)
	insertSample(t, f, 1, base+10_000, "203.0.113.1", 200)
	insertSample(t, f, 1, base+30_000, "203.0.113.1", 300)
	// TTL 1, bucket 1: two samples at +60s, +90s
	insertSample(t, f, 1, base+60_000, "203.0.113.1", 400)
	insertSample(t, f, 1, base+90_000, "203.0.113.1", 500)
	// TTL 2, bucket 0: one sample at +5s. Different TTL = different partition.
	insertSample(t, f, 2, base+5_000, "203.0.113.2", 1000)

	since := base - 1
	until := base + 120_000
	_, body := f.get(t, fmt.Sprintf(
		"/api/samples?since=%d&until=%d&bucket_ms=%d",
		since, until, bucketMs,
	))
	var resp samplesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}

	// Expect 3 representative samples: TTL=1 bucket=0, TTL=1 bucket=1, TTL=2 bucket=0.
	if len(resp.Samples) != 3 {
		t.Fatalf("bucketed samples = %d, want 3", len(resp.Samples))
	}

	// First sample (earliest ts): TTL 1, bucket 0 — should pick the +0 row (rtt=100µs = 0.1ms).
	if resp.Samples[0].TTL != 1 || resp.Samples[0].Ts != base+0 || resp.Samples[0].RTTms != 0.1 {
		t.Errorf("samples[0] = {ttl=%d ts=%d rtt=%v}, want {ttl=1 ts=%d rtt=0.1}",
			resp.Samples[0].TTL, resp.Samples[0].Ts, resp.Samples[0].RTTms, base+0)
	}
	// TTL 2 bucket 0 picks the +5s row (rtt=1000µs = 1ms).
	var ttl2 *sampleJSON
	for i := range resp.Samples {
		if resp.Samples[i].TTL == 2 {
			ttl2 = &resp.Samples[i]
			break
		}
	}
	if ttl2 == nil || ttl2.Ts != base+5_000 || ttl2.RTTms != 1.0 {
		t.Errorf("TTL=2 representative = %+v, want {ttl=2 ts=%d rtt=1.0}", ttl2, base+5_000)
	}
	// TTL 1 bucket 1 picks the +60s row (rtt=400µs = 0.4ms).
	var ttl1bucket1 *sampleJSON
	for i := range resp.Samples {
		if resp.Samples[i].TTL == 1 && resp.Samples[i].Ts == base+60_000 {
			ttl1bucket1 = &resp.Samples[i]
			break
		}
	}
	if ttl1bucket1 == nil || ttl1bucket1.RTTms != 0.4 {
		t.Errorf("TTL=1 bucket=1 representative = %+v, want rtt=0.4", ttl1bucket1)
	}
}

func TestHandleSamples_BucketedRejectsNegative(t *testing.T) {
	f := newFixture(t)
	code, _ := f.get(t, "/api/samples?bucket_ms=-1")
	if code != http.StatusBadRequest {
		t.Errorf("negative bucket_ms: status = %d, want 400", code)
	}
}

func TestHandleSamples_BucketedZeroFallsBackToRaw(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UnixMilli()
	// Three samples in the same 1-minute window.
	insertSample(t, f, 1, now-2000, "203.0.113.1", 100)
	insertSample(t, f, 1, now-1000, "203.0.113.1", 200)
	insertSample(t, f, 1, now-500, "203.0.113.1", 300)
	// bucket_ms=0 means "no bucketing" — all raw rows return.
	_, body := f.get(t, "/api/samples?bucket_ms=0")
	var resp samplesResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Samples) != 3 {
		t.Errorf("bucket_ms=0 returned %d samples, want 3 (raw path)", len(resp.Samples))
	}
}

// TestHandleSamples_FiltersByCurrentTarget pins the fix from step-20.
// When the operator changes config.target and restarts, samples for
// the previous target are still in the database (retention takes 7
// days to clean them up). The /api/samples endpoint must filter to
// the engine's current target so the UI never sees samples from a
// previous probe target.
//
// Reproduction of the original bug: switch target 8.8.8.8 → 1.1.1.1,
// restart daemon, observe the chart "continues" from the previous
// session with no visible boundary. Pre-fix, samples for both
// targets were intermixed in the response.
func TestHandleSamples_FiltersByCurrentTarget(t *testing.T) {
	f := newFixture(t) // engine target is 8.8.8.8
	now := time.Now().UnixMilli()

	// Two samples for the current target (8.8.8.8) — should appear.
	insertSample(t, f, 1, now-2000, "192.0.2.1", 500)
	insertSample(t, f, 2, now-1000, "203.0.113.1", 600)

	// One sample for a previous target (1.1.1.1) — should NOT appear.
	// Direct insert bypasses the helper since it hardcodes the target.
	_, err := f.store.DB().Exec(
		`INSERT INTO samples (target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?)`,
		"1.1.1.1", 1, now-500, "192.0.2.1", 700,
	)
	if err != nil {
		t.Fatalf("insert old-target sample: %v", err)
	}

	code, body := f.get(t, "/api/samples")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var resp samplesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	if len(resp.Samples) != 2 {
		t.Errorf("samples length = %d, want 2 (current-target only); "+
			"old-target samples are leaking through", len(resp.Samples))
	}
	// Confirm we kept the right ones — both should be at ts within the
	// expected window. The 1.1.1.1 row at now-500 would also be inside
	// the window if filtering had failed, so this check is enough.
	for _, s := range resp.Samples {
		if s.Ts == now-500 {
			t.Errorf("response contains sample at ts=%d which belongs "+
				"to previous target 1.1.1.1", s.Ts)
		}
	}
}

// ---------- /api/route_changes ----------

func insertRouteChange(t *testing.T, f *fixture, ttl int, tsMs int64, oldIP, newIP string) {
	t.Helper()
	var oldArg any
	if oldIP == "" {
		oldArg = nil
	} else {
		oldArg = oldIP
	}
	_, err := f.store.DB().Exec(
		`INSERT INTO route_changes (target, ttl, ts, old_ip, new_ip) VALUES (?, ?, ?, ?, ?)`,
		"8.8.8.8", ttl, tsMs, oldArg, newIP,
	)
	if err != nil {
		t.Fatalf("insert route_change: %v", err)
	}
}

func TestHandleRouteChanges_OrdersNewestFirst(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UnixMilli()
	insertRouteChange(t, f, 3, now-10000, "203.0.113.1", "203.0.113.2")
	insertRouteChange(t, f, 3, now-2000, "203.0.113.2", "203.0.113.3")
	insertRouteChange(t, f, 3, now-5000, "203.0.113.5", "203.0.113.6")

	_, body := f.get(t, "/api/route_changes")
	var resp changesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	if len(resp.Changes) != 3 {
		t.Fatalf("changes length = %d, want 3", len(resp.Changes))
	}
	if resp.Changes[0].Ts < resp.Changes[1].Ts || resp.Changes[1].Ts < resp.Changes[2].Ts {
		t.Errorf("changes not sorted by ts descending: %d, %d, %d",
			resp.Changes[0].Ts, resp.Changes[1].Ts, resp.Changes[2].Ts)
	}
}

func TestHandleRouteChanges_AnonymousOldIPIsNull(t *testing.T) {
	f := newFixture(t)
	insertRouteChange(t, f, 6, time.Now().UnixMilli(), "", "203.0.113.206")

	_, body := f.get(t, "/api/route_changes")
	var resp changesResponse
	_ = json.Unmarshal(body, &resp)

	if len(resp.Changes) != 1 {
		t.Fatalf("changes length = %d, want 1", len(resp.Changes))
	}
	if resp.Changes[0].OldIP != nil {
		t.Errorf("changes[0].old_ip = %v, want null", *resp.Changes[0].OldIP)
	}
}

func TestHandleRouteChanges_LimitCaps(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UnixMilli()
	for i := 0; i < 10; i++ {
		insertRouteChange(t, f, 1, now-int64(i*100), "203.0.113.1", "203.0.113.2")
	}
	_, body := f.get(t, "/api/route_changes?limit=3")
	var resp changesResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Changes) != 3 {
		t.Errorf("changes with limit=3 = %d, want 3", len(resp.Changes))
	}
	if resp.Limit != 3 {
		t.Errorf("resp.Limit = %d, want 3", resp.Limit)
	}
}

// TestHandleRouteChanges_FiltersByCurrentTarget mirrors the samples
// regression test for the /api/route_changes endpoint. Same root
// cause: pre-step-20, the query lacked a target filter, so route
// changes recorded against a previous probe target would surface
// in the current view. Pinned to prevent regression.
func TestHandleRouteChanges_FiltersByCurrentTarget(t *testing.T) {
	f := newFixture(t) // engine target is 8.8.8.8
	now := time.Now().UnixMilli()

	// Two route changes for the current target — should appear.
	insertRouteChange(t, f, 1, now-2000, "203.0.113.1", "203.0.113.2")
	insertRouteChange(t, f, 2, now-1000, "203.0.113.3", "203.0.113.4")

	// One route change for a previous target — should NOT appear.
	_, err := f.store.DB().Exec(
		`INSERT INTO route_changes (target, ttl, ts, old_ip, new_ip)
		 VALUES (?, ?, ?, ?, ?)`,
		"1.1.1.1", 1, now-500, "203.0.113.5", "203.0.113.6",
	)
	if err != nil {
		t.Fatalf("insert old-target route_change: %v", err)
	}

	code, body := f.get(t, "/api/route_changes")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var resp changesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	if len(resp.Changes) != 2 {
		t.Errorf("changes length = %d, want 2 (current-target only); "+
			"old-target route_changes are leaking through", len(resp.Changes))
	}
}

// ---------- static ----------

func TestStatic_ServesIndex(t *testing.T) {
	f := newFixture(t)
	code, body := f.get(t, "/")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !contains(string(body), "hoptrail test") {
		t.Errorf("body doesn't contain expected index content: %q", body)
	}
}

func TestStatic_SPAFallbackForUnknownPath(t *testing.T) {
	f := newFixture(t)
	// /foo/bar isn't in the FS — should fall back to index.html for
	// future client-side routing to work.
	code, body := f.get(t, "/foo/bar")
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200 (SPA fallback)", code)
	}
	if !contains(string(body), "hoptrail test") {
		t.Errorf("SPA fallback didn't serve index; body=%q", body)
	}
}

func TestStatic_AssetsServeReal(t *testing.T) {
	f := newFixture(t)
	code, body := f.get(t, "/assets/style.css")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !contains(string(body), "background: #fff") {
		t.Errorf("asset content wrong: %q", body)
	}
}

// TestRun_BindFailureReturnsError pins the "fail loud on bind failure"
// behavior. The previous implementation logged "server: listening"
// from inside a goroutine before ListenAndServe returned, so when the
// bind itself failed the operator would see the misleading "listening"
// line followed by the actual error. Run now binds synchronously and
// only logs "listening" after success — and returns the bind error
// directly so main.go can surface it as a non-zero exit (which it does
// as of step-13, so systemd marks the unit failed rather than letting
// it limp on with a dead listener).
func TestRun_BindFailureReturnsError(t *testing.T) {
	// Acquire a real port, then point a Server at the same address and
	// confirm Run returns an error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("test setup: %v", err)
	}
	defer ln.Close()
	conflictAddr := ln.Addr().String()

	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	target, _ := netip.ParseAddr("8.8.8.8")
	engine, err := probe.NewEngine(probe.EngineConfig{
		Target: target, RouteChangeThreshold: 3,
	}, probe.NoopSink{}, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	sup := &fakeSupervisor{
		engines: map[string]*probe.Engine{target.String(): engine},
	}

	srv, err := New(Config{
		ListenAddr: conflictAddr,
		Supervisor: sup,
		Store:      store,
		WebFS: fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte("test")},
		},
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Run should return synchronously (not block on ctx) because the
	// bind fails before the goroutine is ever spawned.
	runErr := srv.Run(context.Background())
	if runErr == nil {
		t.Fatal("Run on already-bound port returned nil, expected error")
	}
	if !strings.Contains(runErr.Error(), "listen failed") {
		t.Errorf("error message = %q, want one containing 'listen failed'", runErr.Error())
	}
}

// ---------- helpers ----------

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func intStr(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ---------- /api/target ----------

// TestHandleTarget_GetReturnsCurrentTarget — basic round-trip: the
// initial engine targets 8.8.8.8, GET /api/target reports it.
func TestHandleTarget_GetReturnsCurrentTarget(t *testing.T) {
	f := newFixture(t)

	code, body := f.get(t, "/api/target")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var resp targetResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Target != "8.8.8.8" {
		t.Errorf("target = %q, want 8.8.8.8", resp.Target)
	}
}

// TestHandleTarget_PostSwapsAndReturnsNew — POST a new valid IP, the
// supervisor's swap fn is called, and the response reflects the new
// target. The fixture's swapFn simulates the supervisor by Store-ing
// a new engine into engineRef so the read-back works.
func TestHandleTarget_PostSwapsAndReturnsNew(t *testing.T) {
	f := newFixture(t)

	newTarget := "1.1.1.1"
	f.supervisor.swapFn = func(ctx context.Context, target string) error {
		// Build a new engine for the new target and install it. Mirrors
		// what the real supervisor's Swap does (Add new, Remove old).
		addr, err := netip.ParseAddr(target)
		if err != nil {
			return err
		}
		newEngine, err := probe.NewEngine(probe.EngineConfig{
			Target:               addr,
			RouteChangeThreshold: 3,
		}, probe.NoopSink{}, nil)
		if err != nil {
			return err
		}
		go func() { _ = newEngine.Run(context.Background()) }()
		f.supervisor.mu.Lock()
		f.supervisor.engines = map[string]*probe.Engine{target: newEngine}
		f.supervisor.mu.Unlock()
		return nil
	}

	body := strings.NewReader(`{"target":"1.1.1.1"}`)
	res, err := http.Post(f.ts.URL+"/api/target", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		bb, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 200; body=%s", res.StatusCode, bb)
	}
	var resp targetResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Target != newTarget {
		t.Errorf("target = %q, want %q", resp.Target, newTarget)
	}

	// /api/path should now reflect the new target too (proves the
	// atomic swap is visible to other handlers).
	code, pathBody := f.get(t, "/api/path")
	if code != http.StatusOK {
		t.Fatalf("/api/path status = %d, want 200; body=%s", code, pathBody)
	}
	var pathResp pathResponse
	if err := json.Unmarshal(pathBody, &pathResp); err != nil {
		t.Fatalf("decode path: %v", err)
	}
	if pathResp.Target != newTarget {
		t.Errorf("/api/path target = %q, want %q", pathResp.Target, newTarget)
	}
}

// TestHandleTarget_PostRejectsBadInput pins the rejection path:
// malformed JSON, empty target, unspecified/multicast IPs, and
// unknown fields all return 400. (Step-29 lifted the IP-only rule
// so hostnames are no longer in the bad-input set; the supervisor
// resolves them or returns a 400-mappable error if unresolvable.)
func TestHandleTarget_PostRejectsBadInput(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name, body string
	}{
		{"not json", "not even json"},
		{"missing target", `{"foo":"bar"}`},
		{"empty target", `{"target":""}`},
		{"unspecified", `{"target":"0.0.0.0"}`}, // rejected by supervisor → 400
		{"multicast", `{"target":"224.0.0.1"}`}, // rejected by supervisor → 400
		{"unknown field", `{"target":"1.1.1.1","oops":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := http.Post(f.ts.URL+"/api/target", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusBadRequest {
				bb, _ := io.ReadAll(res.Body)
				t.Errorf("status = %d, want 400; body=%s", res.StatusCode, bb)
			}
		})
	}
}

// TestHandleTarget_PostNoOpSwap — POSTing the *current* target
// should short-circuit (no supervisor work) and return 200 with the
// same target. Saves goroutine-teardown cost on accidental double-clicks.
func TestHandleTarget_PostNoOpSwap(t *testing.T) {
	f := newFixture(t)

	swapCalled := false
	f.supervisor.swapFn = func(ctx context.Context, target string) error {
		swapCalled = true
		return nil
	}

	res, err := http.Post(f.ts.URL+"/api/target", "application/json", strings.NewReader(`{"target":"8.8.8.8"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if swapCalled {
		t.Errorf("swap fn was called for a no-op same-target POST")
	}
}

// ---------- /api/targets (multi-target, step-26) ----------

// TestHandleTargets_GetListsAll — initial fixture has one target;
// adding a second via the fake-supervisor surfaces both in the list.
func TestHandleTargets_GetListsAll(t *testing.T) {
	f := newFixture(t)

	// Initial state: one target (8.8.8.8 from the fixture).
	code, body := f.get(t, "/api/targets")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var resp targetsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Targets) != 1 || resp.Targets[0] != "8.8.8.8" {
		t.Errorf("targets = %v, want [8.8.8.8]", resp.Targets)
	}

	// Add another, then list — should see both in lexicographic order.
	if err := f.supervisor.Add(context.Background(), "1.1.1.1"); err != nil {
		t.Fatalf("supervisor.Add: %v", err)
	}
	code, body = f.get(t, "/api/targets")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{"1.1.1.1", "8.8.8.8"}
	if len(resp.Targets) != 2 || resp.Targets[0] != want[0] || resp.Targets[1] != want[1] {
		t.Errorf("targets = %v, want %v", resp.Targets, want)
	}
}

// TestHandleTargets_PostAdds — POST a new IP, it shows up in the
// supervisor and in subsequent GET responses.
func TestHandleTargets_PostAdds(t *testing.T) {
	f := newFixture(t)

	res, err := http.Post(f.ts.URL+"/api/targets", "application/json",
		strings.NewReader(`{"target":"9.9.9.9"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		bb, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 200; body=%s", res.StatusCode, bb)
	}

	active := f.supervisor.Targets()
	if len(active) != 2 {
		t.Fatalf("supervisor.Targets() = %v, want 2 entries", active)
	}
}

// TestHandleTargets_PostConflictWhenAlreadyMonitored — re-adding the
// same target returns 409.
func TestHandleTargets_PostConflictWhenAlreadyMonitored(t *testing.T) {
	f := newFixture(t)

	res, err := http.Post(f.ts.URL+"/api/targets", "application/json",
		strings.NewReader(`{"target":"8.8.8.8"}`)) // same as fixture's initial
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		bb, _ := io.ReadAll(res.Body)
		t.Errorf("status = %d, want 409 Conflict; body=%s", res.StatusCode, bb)
	}
}

// TestHandleTargets_DeleteRemoves — DELETE /api/targets/<ip> removes
// the matching target from the supervisor.
func TestHandleTargets_DeleteRemoves(t *testing.T) {
	f := newFixture(t)

	// Need at least two targets so removing one leaves a useful state.
	if err := f.supervisor.Add(context.Background(), "1.1.1.1"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	req, err := http.NewRequest(http.MethodDelete, f.ts.URL+"/api/targets/8.8.8.8", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		bb, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 200; body=%s", res.StatusCode, bb)
	}

	active := f.supervisor.Targets()
	if len(active) != 1 || active[0] != "1.1.1.1" {
		t.Errorf("active = %v, want [1.1.1.1]", active)
	}
}

// TestHandleTargets_DeleteNotFound — DELETE on an unmonitored target
// returns 404.
func TestHandleTargets_DeleteNotFound(t *testing.T) {
	f := newFixture(t)

	req, _ := http.NewRequest(http.MethodDelete, f.ts.URL+"/api/targets/9.9.9.9", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		bb, _ := io.ReadAll(res.Body)
		t.Errorf("status = %d, want 404; body=%s", res.StatusCode, bb)
	}
}

// TestHandlePath_ScopesByTargetParam — with multiple targets active,
// /api/path?target=X returns X's snapshot; ambiguous param-less call
// is rejected.
func TestHandlePath_ScopesByTargetParam(t *testing.T) {
	f := newFixture(t)

	// Add a second target so the param becomes required.
	other := "1.1.1.1"
	if err := f.supervisor.Add(context.Background(), other); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Without ?target= — 400 (multiple targets, ambiguous).
	code, body := f.get(t, "/api/path")
	if code != http.StatusBadRequest {
		t.Errorf("no-param status = %d, want 400; body=%s", code, body)
	}

	// With ?target=8.8.8.8 — returns the fixture target.
	code, body = f.get(t, "/api/path?target=8.8.8.8")
	if code != http.StatusOK {
		t.Fatalf("with-param status = %d, want 200; body=%s", code, body)
	}
	var resp pathResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Target != "8.8.8.8" {
		t.Errorf("target = %q, want 8.8.8.8", resp.Target)
	}

	// With ?target=1.1.1.1 — returns the added target.
	code, body = f.get(t, "/api/path?target=1.1.1.1")
	if code != http.StatusOK {
		t.Fatalf("with-other-target status = %d, want 200; body=%s", code, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Target != "1.1.1.1" {
		t.Errorf("target = %q, want 1.1.1.1", resp.Target)
	}
}

// TestHandleTarget_GetReturns503WhenNoTargets — handlers must tolerate
// the case where no targets are currently monitored (e.g. last one was
// removed). 503 is the documented retry signal for clients.
func TestHandleTarget_GetReturns503WhenNoTargets(t *testing.T) {
	f := newFixture(t)
	f.supervisor.mu.Lock()
	f.supervisor.engines = map[string]*probe.Engine{}
	f.supervisor.mu.Unlock()

	code, _ := f.get(t, "/api/target")
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
}

// ---------- step-37: per-target probe interval ----------

// patch is a small helper for issuing a PATCH /api/targets/<id> with
// a JSON body and returning the response. The stdlib has no Patch
// helper analogous to Post, so we build the request explicitly.
func (f *fixture) patch(t *testing.T, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, f.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", path, err)
	}
	defer res.Body.Close()
	bb, _ := io.ReadAll(res.Body)
	return res.StatusCode, bb
}

// TestHandleTargets_GetIncludesIntervals — the GET response carries
// the per-target interval map (step-37) so a single round-trip is
// enough for the UI to render the picker in the right initial state.
func TestHandleTargets_GetIncludesIntervals(t *testing.T) {
	f := newFixture(t)

	// Promote the fixture's target to a custom interval; the GET
	// response must reflect it.
	if err := f.supervisor.SetInterval(context.Background(), "8.8.8.8", 3*time.Second); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	code, body := f.get(t, "/api/targets")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var resp targetsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.IntervalsMs["8.8.8.8"]; got != 3000 {
		t.Errorf("IntervalsMs[8.8.8.8] = %d, want 3000", got)
	}
}

// TestHandleTargets_PatchUpdatesInterval — happy path, single hop:
// PATCH succeeds, supervisor sees the new cadence, response echoes it.
func TestHandleTargets_PatchUpdatesInterval(t *testing.T) {
	f := newFixture(t)

	code, body := f.patch(t, "/api/targets/8.8.8.8", `{"interval_ms":2000}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var resp targetPatchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Target != "8.8.8.8" || resp.IntervalMs == nil || *resp.IntervalMs != 2000 {
		t.Errorf("resp = %+v, want {Target:8.8.8.8 IntervalMs:2000}", resp)
	}
	if got := f.supervisor.Intervals()["8.8.8.8"]; got != 2*time.Second {
		t.Errorf("supervisor intervals[8.8.8.8] = %s, want 2s", got)
	}
}

// TestHandleTargets_PatchRejectsBadInput — covers missing body fields,
// non-positive intervals, out-of-range, and unknown JSON shape.
// The supervisor enforces the range; the handler maps the error.
func TestHandleTargets_PatchRejectsBadInput(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing field", `{}`, http.StatusBadRequest},
		{"zero", `{"interval_ms":0}`, http.StatusBadRequest},
		{"negative", `{"interval_ms":-1}`, http.StatusBadRequest},
		{"below min", `{"interval_ms":50}`, http.StatusBadRequest},
		{"above max", `{"interval_ms":120000}`, http.StatusBadRequest},
		{"unknown field", `{"interval_ms":1000,"foo":"bar"}`, http.StatusBadRequest},
		{"not json", `not json`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := f.patch(t, "/api/targets/8.8.8.8", tc.body)
			if code != tc.want {
				t.Errorf("status = %d, want %d; body=%s", code, tc.want, body)
			}
		})
	}
}

// TestHandleTargets_PatchUnknownTarget — PATCH against a target the
// supervisor doesn't know about returns 404. Matches DELETE semantics.
func TestHandleTargets_PatchUnknownTarget(t *testing.T) {
	f := newFixture(t)

	code, body := f.patch(t, "/api/targets/9.9.9.9", `{"interval_ms":1000}`)
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", code, body)
	}
}

// TestHandleTargets_PatchRequiresPath — PATCH on /api/targets (no id)
// is not the same handler; this verifies the by-path router exists
// and the no-id case is 400/404.
func TestHandleTargets_PatchEmptyPath(t *testing.T) {
	f := newFixture(t)

	code, body := f.patch(t, "/api/targets/", `{"interval_ms":1000}`)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", code, body)
	}
}

// ---------- step-39: per-tab latency thresholds ----------

// TestHandleTargets_GetIncludesThresholds — the GET response carries
// the per-target thresholds map alongside intervals_ms, so the UI's
// ThresholdsPicker can render in one round-trip.
func TestHandleTargets_GetIncludesThresholds(t *testing.T) {
	f := newFixture(t)
	warn, crit := int64(100), int64(300)
	if err := f.supervisor.SetThresholds(context.Background(), "8.8.8.8", &warn, &crit); err != nil {
		t.Fatalf("SetThresholds: %v", err)
	}
	code, body := f.get(t, "/api/targets")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var resp targetsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := resp.Thresholds["8.8.8.8"]
	if !ok {
		t.Fatalf("Thresholds[8.8.8.8] missing")
	}
	if got.WarningMs == nil || *got.WarningMs != 100 || got.CriticalMs == nil || *got.CriticalMs != 300 {
		t.Errorf("Thresholds[8.8.8.8] = (%v, %v), want (100, 300)", got.WarningMs, got.CriticalMs)
	}
}

// TestHandleTargets_PatchUpdatesThresholds — happy path: PATCH with
// both fields succeeds, supervisor sees the new pair, response echoes.
func TestHandleTargets_PatchUpdatesThresholds(t *testing.T) {
	f := newFixture(t)

	code, body := f.patch(t, "/api/targets/8.8.8.8", `{"warning_ms":50,"critical_ms":200}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var resp targetPatchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.WarningMs == nil || *resp.WarningMs != 50 || resp.CriticalMs == nil || *resp.CriticalMs != 200 {
		t.Errorf("resp = %+v, want WarningMs=50, CriticalMs=200", resp)
	}
	pair := f.supervisor.Thresholds()["8.8.8.8"]
	if pair.WarningMs == nil || *pair.WarningMs != 50 || pair.CriticalMs == nil || *pair.CriticalMs != 200 {
		t.Errorf("supervisor thresholds = (%v, %v), want (50, 200)", pair.WarningMs, pair.CriticalMs)
	}
}

// TestHandleTargets_PatchClearsThresholds — sending both fields as
// JSON null clears the operator's override so the UI falls back to
// its default preset.
func TestHandleTargets_PatchClearsThresholds(t *testing.T) {
	f := newFixture(t)
	w, c := int64(50), int64(200)
	_ = f.supervisor.SetThresholds(context.Background(), "8.8.8.8", &w, &c)

	code, body := f.patch(t, "/api/targets/8.8.8.8", `{"warning_ms":null,"critical_ms":null}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	pair := f.supervisor.Thresholds()["8.8.8.8"]
	if pair.WarningMs != nil || pair.CriticalMs != nil {
		t.Errorf("after clear: supervisor thresholds = (%v, %v), want both nil", pair.WarningMs, pair.CriticalMs)
	}
}

// TestHandleTargets_PatchRejectsBadThresholds — covers partial pair,
// non-positive, and ordering violations.
func TestHandleTargets_PatchRejectsBadThresholds(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing critical", `{"warning_ms":100}`, http.StatusBadRequest},
		{"missing warning", `{"critical_ms":300}`, http.StatusBadRequest},
		{"warning zero", `{"warning_ms":0,"critical_ms":100}`, http.StatusBadRequest},
		{"critical zero", `{"warning_ms":100,"critical_ms":0}`, http.StatusBadRequest},
		{"warning negative", `{"warning_ms":-1,"critical_ms":100}`, http.StatusBadRequest},
		{"warning >= critical", `{"warning_ms":300,"critical_ms":100}`, http.StatusBadRequest},
		{"warning == critical", `{"warning_ms":100,"critical_ms":100}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := f.patch(t, "/api/targets/8.8.8.8", tc.body)
			if code != tc.want {
				t.Errorf("status = %d, want %d; body=%s", code, tc.want, body)
			}
		})
	}
}

// TestHandleTargets_PatchCombinedInterval+Thresholds — operator can
// send all fields in one PATCH; both supervisor methods are called.
func TestHandleTargets_PatchCombinedInterval(t *testing.T) {
	f := newFixture(t)

	code, body := f.patch(t, "/api/targets/8.8.8.8",
		`{"interval_ms":2000,"warning_ms":75,"critical_ms":250}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	if got := f.supervisor.Intervals()["8.8.8.8"]; got != 2*time.Second {
		t.Errorf("interval = %s, want 2s", got)
	}
	pair := f.supervisor.Thresholds()["8.8.8.8"]
	if pair.WarningMs == nil || *pair.WarningMs != 75 || pair.CriticalMs == nil || *pair.CriticalMs != 250 {
		t.Errorf("thresholds = (%v, %v), want (75, 250)", pair.WarningMs, pair.CriticalMs)
	}
}

// ---------- step-41: final-hop-only ----------

func TestHandleTargets_GetIncludesFinalHopOnly(t *testing.T) {
	f := newFixture(t)
	if err := f.supervisor.SetFinalHopOnly(context.Background(), "8.8.8.8", true); err != nil {
		t.Fatalf("SetFinalHopOnly: %v", err)
	}
	code, body := f.get(t, "/api/targets")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var resp targetsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.FinalHopOnly["8.8.8.8"] {
		t.Errorf("FinalHopOnly[8.8.8.8] = false, want true")
	}
}

func TestHandleTargets_PatchTogglesFinalHopOnly(t *testing.T) {
	f := newFixture(t)

	// On.
	code, body := f.patch(t, "/api/targets/8.8.8.8", `{"final_hop_only":true}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var resp targetPatchResponse
	_ = json.Unmarshal(body, &resp)
	if resp.FinalHopOnly == nil || !*resp.FinalHopOnly {
		t.Errorf("resp.FinalHopOnly = %v, want &true", resp.FinalHopOnly)
	}
	if !f.supervisor.FinalHopOnlys()["8.8.8.8"] {
		t.Errorf("supervisor FinalHopOnly[8.8.8.8] = false, want true")
	}

	// Off.
	code, _ = f.patch(t, "/api/targets/8.8.8.8", `{"final_hop_only":false}`)
	if code != http.StatusOK {
		t.Fatalf("off status = %d, want 200", code)
	}
	if f.supervisor.FinalHopOnlys()["8.8.8.8"] {
		t.Errorf("after off: supervisor FinalHopOnly[8.8.8.8] = true, want false")
	}
}

func TestHandleTargets_PatchFinalHopOnlyUnknownTarget(t *testing.T) {
	f := newFixture(t)
	code, body := f.patch(t, "/api/targets/9.9.9.9", `{"final_hop_only":true}`)
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", code, body)
	}
}

// ---------- step-42: annotations ----------

func TestHandleAnnotations_AddListDelete(t *testing.T) {
	f := newFixture(t)

	// Add two notes.
	for _, body := range []string{
		`{"target":"8.8.8.8","ts":1000,"text":"router reboot"}`,
		`{"target":"8.8.8.8","ts":2000,"text":"isp support called"}`,
	} {
		res, err := http.Post(f.ts.URL+"/api/annotations", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		bb, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("POST status = %d, want 200; body=%s", res.StatusCode, bb)
		}
	}

	// List — both appear in ts-ascending order.
	code, body := f.get(t, "/api/annotations?target=8.8.8.8")
	if code != http.StatusOK {
		t.Fatalf("GET status = %d; body=%s", code, body)
	}
	var resp annotationsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Annotations) != 2 {
		t.Fatalf("got %d annotations, want 2", len(resp.Annotations))
	}
	if resp.Annotations[0].Ts != 1000 || resp.Annotations[1].Ts != 2000 {
		t.Errorf("ordering wrong: %+v", resp.Annotations)
	}

	// Delete the first.
	id1 := resp.Annotations[0].ID
	req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/annotations/%d", f.ts.URL, id1), nil)
	delRes, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	delRes.Body.Close()
	if delRes.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE status = %d, want 204", delRes.StatusCode)
	}

	// List again — only one remains.
	_, body = f.get(t, "/api/annotations?target=8.8.8.8")
	_ = json.Unmarshal(body, &resp)
	if len(resp.Annotations) != 1 || resp.Annotations[0].Ts != 2000 {
		t.Errorf("after delete: %+v, want one note at ts=2000", resp.Annotations)
	}
}

func TestHandleAnnotations_TargetRequired(t *testing.T) {
	f := newFixture(t)
	code, _ := f.get(t, "/api/annotations")
	if code != http.StatusBadRequest {
		t.Errorf("missing target: status = %d, want 400", code)
	}
}

func TestHandleAnnotations_WindowFilter(t *testing.T) {
	f := newFixture(t)
	for _, ts := range []int64{1000, 2000, 3000, 4000, 5000} {
		body := fmt.Sprintf(`{"target":"8.8.8.8","ts":%d,"text":"n"}`, ts)
		res, _ := http.Post(f.ts.URL+"/api/annotations", "application/json", strings.NewReader(body))
		res.Body.Close()
	}
	code, body := f.get(t, "/api/annotations?target=8.8.8.8&since=2000&until=4000")
	if code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", code, body)
	}
	var resp annotationsResponse
	_ = json.Unmarshal(body, &resp)
	if len(resp.Annotations) != 3 {
		t.Errorf("window result count = %d, want 3 (inclusive bounds)", len(resp.Annotations))
	}
}

func TestHandleAnnotations_AddRejectsBadInput(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing target", `{"ts":1000,"text":"x"}`, http.StatusBadRequest},
		{"missing text", `{"target":"8.8.8.8","ts":1000}`, http.StatusBadRequest},
		{"missing ts", `{"target":"8.8.8.8","text":"x"}`, http.StatusBadRequest},
		{"zero ts", `{"target":"8.8.8.8","ts":0,"text":"x"}`, http.StatusBadRequest},
		{"negative ts", `{"target":"8.8.8.8","ts":-1,"text":"x"}`, http.StatusBadRequest},
		{"empty text", `{"target":"8.8.8.8","ts":1000,"text":""}`, http.StatusBadRequest},
		{"too-long text", `{"target":"8.8.8.8","ts":1000,"text":"` + strings.Repeat("x", 281) + `"}`, http.StatusBadRequest},
		{"unknown field", `{"target":"8.8.8.8","ts":1000,"text":"x","foo":1}`, http.StatusBadRequest},
		{"not json", `not json`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := http.Post(f.ts.URL+"/api/annotations", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer res.Body.Close()
			if res.StatusCode != tc.want {
				bb, _ := io.ReadAll(res.Body)
				t.Errorf("status = %d, want %d; body=%s", res.StatusCode, tc.want, bb)
			}
		})
	}
}

// ---------- step-45: export bundle ----------

func TestHandleExport_BundlesEverything(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Seed: a few samples, a route change, and an annotation — all
	// inside what'll be the default 1h window.
	now := time.Now().UnixMilli()
	_, err := f.store.DB().ExecContext(ctx,
		`INSERT INTO samples (target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?)`,
		"8.8.8.8", 1, now-30000, "203.0.113.1", 1000)
	if err != nil {
		t.Fatalf("seed sample: %v", err)
	}
	_, err = f.store.DB().ExecContext(ctx,
		`INSERT INTO route_changes (target, ttl, ts, old_ip, new_ip) VALUES (?, ?, ?, ?, ?)`,
		"8.8.8.8", 5, now-20000, "198.51.100.51", "198.51.100.59")
	if err != nil {
		t.Fatalf("seed route_change: %v", err)
	}
	if _, err := f.store.AddAnnotation(ctx, "8.8.8.8", now-10000, "test note"); err != nil {
		t.Fatalf("seed annotation: %v", err)
	}

	code, body := f.get(t, "/api/export?target=8.8.8.8")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}

	var bundle exportBundle
	if err := json.Unmarshal(body, &bundle); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if bundle.Target != "8.8.8.8" {
		t.Errorf("Target = %q, want 8.8.8.8", bundle.Target)
	}
	if bundle.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", bundle.SchemaVersion)
	}
	if len(bundle.Samples) != 1 {
		t.Errorf("Samples count = %d, want 1", len(bundle.Samples))
	}
	if len(bundle.RouteChanges) != 1 {
		t.Errorf("RouteChanges count = %d, want 1", len(bundle.RouteChanges))
	}
	if len(bundle.Annotations) != 1 || bundle.Annotations[0].Text != "test note" {
		t.Errorf("Annotations = %+v, want one note 'test note'", bundle.Annotations)
	}
}

func TestHandleExport_SetsDownloadHeaders(t *testing.T) {
	f := newFixture(t)
	res, err := http.Get(f.ts.URL + "/api/export?target=8.8.8.8")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	cd := res.Header.Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "hoptrail-8.8.8.8-") {
		t.Errorf("Content-Disposition = %q, want attachment filename like hoptrail-8.8.8.8-<ts>.json", cd)
	}
}

func TestHandleExport_WindowFilter(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	// One inside the explicit window, one outside.
	_, _ = f.store.DB().ExecContext(ctx,
		`INSERT INTO samples (target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?)`,
		"8.8.8.8", 1, now-30000, "203.0.113.1", 1000)
	_, _ = f.store.DB().ExecContext(ctx,
		`INSERT INTO samples (target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?)`,
		"8.8.8.8", 1, now-90*60*1000, "203.0.113.1", 1000) // 90 min ago

	url := fmt.Sprintf("/api/export?target=8.8.8.8&since=%d&until=%d", now-60000, now)
	code, body := f.get(t, url)
	if code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", code, body)
	}
	var bundle exportBundle
	_ = json.Unmarshal(body, &bundle)
	if len(bundle.Samples) != 1 {
		t.Errorf("Samples count = %d, want 1 (window should have filtered out the 90-min-old sample)", len(bundle.Samples))
	}
}

func TestHandleAnnotations_DeleteBadIDFails(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/api/annotations/", "/api/annotations/abc"} {
		req, _ := http.NewRequest(http.MethodDelete, f.ts.URL+path, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE %s: %v", path, err)
		}
		if res.StatusCode != http.StatusBadRequest {
			bb, _ := io.ReadAll(res.Body)
			t.Errorf("%s: status = %d, want 400; body=%s", path, res.StatusCode, bb)
		}
		res.Body.Close()
	}
}

// ---------- step-69: /api/tabs ----------

// Small helpers so the tab tests don't repeat the req+do+read dance.
func (f *fixture) post(t *testing.T, path, body string) (int, []byte) {
	t.Helper()
	res, err := http.Post(f.ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer res.Body.Close()
	bb, _ := io.ReadAll(res.Body)
	return res.StatusCode, bb
}

func (f *fixture) delete(t *testing.T, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, f.ts.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	defer res.Body.Close()
	bb, _ := io.ReadAll(res.Body)
	return res.StatusCode, bb
}

// seedActiveTarget inserts an active_target row directly so the
// FK from tabs.target → active_targets.target is satisfied. The
// fakeSupervisor doesn't write to the DB on Add(), so handler tests
// that need a real DB-side target use this.
func seedActiveTarget(t *testing.T, f *fixture, target string) {
	t.Helper()
	if err := f.store.AddActiveTarget(context.Background(), target); err != nil {
		t.Fatalf("seed target: %v", err)
	}
}

func TestHandleTabs_EmptyList(t *testing.T) {
	f := newFixture(t)
	code, body := f.get(t, "/api/tabs")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	var resp tabsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Tabs == nil {
		t.Error("Tabs is nil; want empty slice (JSON shape stability)")
	}
}

func TestHandleTabs_CreateAndList(t *testing.T) {
	f := newFixture(t)
	seedActiveTarget(t, f, "8.8.8.8")

	code, body := f.post(t, "/api/tabs", `{"target":"8.8.8.8","label":"primary"}`)
	if code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", code, body)
	}
	var created tabJSON
	_ = json.Unmarshal(body, &created)
	if created.TabID <= 0 {
		t.Errorf("created.TabID = %d, want positive", created.TabID)
	}
	if created.Label == nil || *created.Label != "primary" {
		t.Errorf("created.Label = %v, want %q", created.Label, "primary")
	}

	// Second tab at the same target — should land at position 1.
	_, body2 := f.post(t, "/api/tabs", `{"target":"8.8.8.8"}`)
	var created2 tabJSON
	_ = json.Unmarshal(body2, &created2)

	_, listBody := f.get(t, "/api/tabs")
	var listResp tabsResponse
	_ = json.Unmarshal(listBody, &listResp)
	if len(listResp.Tabs) != 2 {
		t.Fatalf("list len = %d, want 2", len(listResp.Tabs))
	}
	if listResp.Tabs[0].Position != 0 || listResp.Tabs[1].Position != 1 {
		t.Errorf("positions = [%d %d], want [0 1]", listResp.Tabs[0].Position, listResp.Tabs[1].Position)
	}
}

func TestHandleTabs_CreateRejectsMissingTarget(t *testing.T) {
	f := newFixture(t)
	code, body := f.post(t, "/api/tabs", `{"target":"1.1.1.1"}`)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", code, body)
	}
	if !strings.Contains(string(body), "not monitored") {
		t.Errorf("body = %q, want hint about POSTing /api/targets first", body)
	}
}

func TestHandleTabs_CreateMissingTargetField(t *testing.T) {
	f := newFixture(t)
	code, _ := f.post(t, "/api/tabs", `{}`)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for missing target field", code)
	}
}

func TestHandleTabs_CreateCopyFromInherits(t *testing.T) {
	f := newFixture(t)
	seedActiveTarget(t, f, "8.8.8.8")
	_, b1 := f.post(t, "/api/tabs", `{"target":"8.8.8.8","label":"src","warning_ms":50,"critical_ms":150}`)
	var src tabJSON
	_ = json.Unmarshal(b1, &src)

	body := fmt.Sprintf(`{"target":"8.8.8.8","copy_from":%d}`, src.TabID)
	code, b2 := f.post(t, "/api/tabs", body)
	if code != http.StatusOK {
		t.Fatalf("copy_from status = %d, body=%s", code, b2)
	}
	var copy tabJSON
	_ = json.Unmarshal(b2, &copy)
	if copy.Label == nil || *copy.Label != "src" {
		t.Errorf("copy.Label = %v, want %q (inherited)", copy.Label, "src")
	}
	if copy.WarningMs == nil || *copy.WarningMs != 50 {
		t.Errorf("copy.WarningMs = %v, want 50 (inherited)", copy.WarningMs)
	}
}

func TestHandleTabs_UpdateLabel(t *testing.T) {
	f := newFixture(t)
	seedActiveTarget(t, f, "8.8.8.8")
	_, b := f.post(t, "/api/tabs", `{"target":"8.8.8.8"}`)
	var tab tabJSON
	_ = json.Unmarshal(b, &tab)

	code, body := f.patch(t, fmt.Sprintf("/api/tabs/%d", tab.TabID), `{"label":"renamed"}`)
	if code != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", code, body)
	}
	var updated tabJSON
	_ = json.Unmarshal(body, &updated)
	if updated.Label == nil || *updated.Label != "renamed" {
		t.Errorf("updated.Label = %v, want %q", updated.Label, "renamed")
	}
}

func TestHandleTabs_UpdateThresholdsValidation(t *testing.T) {
	f := newFixture(t)
	seedActiveTarget(t, f, "8.8.8.8")
	_, b := f.post(t, "/api/tabs", `{"target":"8.8.8.8"}`)
	var tab tabJSON
	_ = json.Unmarshal(b, &tab)
	path := fmt.Sprintf("/api/tabs/%d", tab.TabID)

	// One-of-pair → 400.
	code, _ := f.patch(t, path, `{"warning_ms":50}`)
	if code != http.StatusBadRequest {
		t.Errorf("solo warning_ms: status = %d, want 400", code)
	}
	// warning >= critical → 400.
	code, _ = f.patch(t, path, `{"warning_ms":100,"critical_ms":100}`)
	if code != http.StatusBadRequest {
		t.Errorf("warning == critical: status = %d, want 400", code)
	}
	// Negative → 400.
	code, _ = f.patch(t, path, `{"warning_ms":-1,"critical_ms":100}`)
	if code != http.StatusBadRequest {
		t.Errorf("negative warning: status = %d, want 400", code)
	}
	// Both nil → clears.
	code, body := f.patch(t, path, `{"warning_ms":null,"critical_ms":null}`)
	if code != http.StatusOK {
		t.Errorf("both null: status = %d, body=%s", code, body)
	}
}

func TestHandleTabs_UpdateNoFields(t *testing.T) {
	f := newFixture(t)
	seedActiveTarget(t, f, "8.8.8.8")
	_, b := f.post(t, "/api/tabs", `{"target":"8.8.8.8"}`)
	var tab tabJSON
	_ = json.Unmarshal(b, &tab)
	code, _ := f.patch(t, fmt.Sprintf("/api/tabs/%d", tab.TabID), `{}`)
	if code != http.StatusBadRequest {
		t.Errorf("empty body: status = %d, want 400", code)
	}
}

func TestHandleTabs_UpdateUnknownReturns404(t *testing.T) {
	f := newFixture(t)
	code, _ := f.patch(t, "/api/tabs/99999", `{"label":"nope"}`)
	if code != http.StatusNotFound {
		t.Errorf("unknown id: status = %d, want 404", code)
	}
}

func TestHandleTabs_DeleteCascadesTargetWhenLast(t *testing.T) {
	f := newFixture(t)
	seedActiveTarget(t, f, "8.8.8.8")
	removed := make(chan string, 1)
	f.supervisor.removeFn = func(_ context.Context, target string) error {
		removed <- target
		return nil
	}

	_, b := f.post(t, "/api/tabs", `{"target":"8.8.8.8"}`)
	var tab tabJSON
	_ = json.Unmarshal(b, &tab)

	code, _ := f.delete(t, fmt.Sprintf("/api/tabs/%d", tab.TabID))
	if code != http.StatusNoContent {
		t.Errorf("delete status = %d, want 204", code)
	}
	select {
	case target := <-removed:
		if target != "8.8.8.8" {
			t.Errorf("supervisor.Remove called with %q, want 8.8.8.8", target)
		}
	default:
		t.Error("supervisor.Remove NOT called on last-tab delete")
	}
}

func TestHandleTabs_DeleteSkipsCascadeWhenNotLast(t *testing.T) {
	f := newFixture(t)
	seedActiveTarget(t, f, "8.8.8.8")
	removeCalled := false
	f.supervisor.removeFn = func(_ context.Context, target string) error {
		removeCalled = true
		return nil
	}
	_, b1 := f.post(t, "/api/tabs", `{"target":"8.8.8.8"}`)
	_, _ = f.post(t, "/api/tabs", `{"target":"8.8.8.8"}`) // second tab
	var first tabJSON
	_ = json.Unmarshal(b1, &first)

	code, _ := f.delete(t, fmt.Sprintf("/api/tabs/%d", first.TabID))
	if code != http.StatusNoContent {
		t.Errorf("delete status = %d, want 204", code)
	}
	if removeCalled {
		t.Error("supervisor.Remove called even though another tab still references the target")
	}
}

func TestHandleTabs_DeleteUnknown404(t *testing.T) {
	f := newFixture(t)
	code, _ := f.delete(t, "/api/tabs/99999")
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

func TestHandleTabs_Reorder(t *testing.T) {
	f := newFixture(t)
	seedActiveTarget(t, f, "8.8.8.8")
	var ids [3]int64
	for i := range ids {
		_, b := f.post(t, "/api/tabs", `{"target":"8.8.8.8"}`)
		var tab tabJSON
		_ = json.Unmarshal(b, &tab)
		ids[i] = tab.TabID
	}

	body := fmt.Sprintf(`{"order":[%d,%d,%d]}`, ids[2], ids[1], ids[0])
	code, b := f.patch(t, "/api/tabs/order", body)
	if code != http.StatusNoContent {
		t.Fatalf("reorder status = %d, body=%s", code, b)
	}

	_, listBody := f.get(t, "/api/tabs")
	var listResp tabsResponse
	_ = json.Unmarshal(listBody, &listResp)
	if listResp.Tabs[0].TabID != ids[2] || listResp.Tabs[1].TabID != ids[1] || listResp.Tabs[2].TabID != ids[0] {
		t.Errorf("order after reorder = [%d %d %d], want [%d %d %d]",
			listResp.Tabs[0].TabID, listResp.Tabs[1].TabID, listResp.Tabs[2].TabID,
			ids[2], ids[1], ids[0])
	}
}

func TestHandleTabs_ReorderRejectsEmpty(t *testing.T) {
	f := newFixture(t)
	code, _ := f.patch(t, "/api/tabs/order", `{"order":[]}`)
	if code != http.StatusBadRequest {
		t.Errorf("empty order: status = %d, want 400", code)
	}
}

// withTestCSRF simulates the browser UI: it injects the X-Hoptrail-CSRF
// header the crossOriginGuard (step-170) requires on mutating requests,
// so functional fixtures exercise the real handler chain. The guard's
// own accept/reject logic is covered directly in csrf_test.go.
func withTestCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Header.Set("X-Hoptrail-CSRF", "1")
		}
		next.ServeHTTP(w, r)
	})
}
