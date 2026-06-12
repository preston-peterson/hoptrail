package probe

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// fakeProber satisfies ICMPProber for tests. It records every Probe
// call and lets the test program responses per-TTL.
type fakeProber struct {
	mu sync.Mutex

	// callsByTTL counts how many times each TTL was probed. Tests use
	// this to wait for the loops to have progressed.
	callsByTTL map[uint8]int

	// responder, if non-nil, maps a TTL to a result. Returning
	// (Result{}, ErrTimeout) is the documented "no response" path.
	// If responder is nil, every probe times out.
	responder func(ttl uint8) (Result, error)
}

func newFakeProber() *fakeProber {
	return &fakeProber{callsByTTL: make(map[uint8]int)}
}

func (f *fakeProber) Probe(ctx context.Context, target netip.Addr, ttl uint8, timeout time.Duration) (Result, error) {
	f.mu.Lock()
	f.callsByTTL[ttl]++
	fn := f.responder
	f.mu.Unlock()

	// Respect cancellation immediately — tests assume shutdown is fast.
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	default:
	}

	if fn != nil {
		return fn(ttl)
	}
	return Result{}, ErrTimeout
}

func (f *fakeProber) callCount(ttl uint8) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callsByTTL[ttl]
}

func (f *fakeProber) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	for _, c := range f.callsByTTL {
		n += c
	}
	return n
}

func (f *fakeProber) setResponder(fn func(ttl uint8) (Result, error)) {
	f.mu.Lock()
	f.responder = fn
	f.mu.Unlock()
}

// linearPathResponder programs the fake to look like a real path:
// TTL 1..pathLen-1 respond with TimeExceeded from synthetic routers,
// TTL pathLen and beyond respond with EchoReply from the target.
func linearPathResponder(t *testing.T, target netip.Addr, pathLen uint8) func(uint8) (Result, error) {
	t.Helper()
	return func(ttl uint8) (Result, error) {
		if ttl < pathLen {
			ip, err := netip.ParseAddr("10.0.0." + string([]byte{'0' + byte(ttl)}))
			if err != nil {
				t.Fatalf("test setup: %v", err)
			}
			return Result{RespIP: ip, RTT: time.Duration(ttl) * time.Millisecond, Type: ReplyTimeExceeded}, nil
		}
		return Result{RespIP: target, RTT: 10 * time.Millisecond, Type: ReplyEchoReply}, nil
	}
}

// startEngineForTest is like newRunningEngine but exposes the captureSink
// so loop tests can assert on what reached storage. Returns the engine,
// the sink, and a cancel func that stops the engine and waits for it.
func startEngineForTest(t *testing.T, target string) (*Engine, *captureSink, context.CancelFunc) {
	t.Helper()
	addr, err := netip.ParseAddr(target)
	if err != nil {
		t.Fatalf("bad target: %v", err)
	}
	sink := &captureSink{}
	eng, err := NewEngine(EngineConfig{Target: addr, RouteChangeThreshold: 3}, sink, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = eng.Run(ctx) }()
	return eng, sink, cancel
}

// ---------- Discovery tests ----------

func TestDiscovery_RunSendsImmediateSweep(t *testing.T) {
	eng, sink, cancel := startEngineForTest(t, "8.8.8.8")
	defer cancel()

	proboe := newFakeProber()
	proboe.setResponder(linearPathResponder(t, eng.cfg.Target, 5)) // 5-hop path

	disc, err := NewDiscovery(DiscoveryConfig{
		Target:   eng.cfg.Target,
		Interval: 10 * time.Second, // long; we only care about the immediate sweep
		MaxHops:  8,
		Timeout:  100 * time.Millisecond,
	}, proboe, eng, nil)
	if err != nil {
		t.Fatalf("NewDiscovery: %v", err)
	}

	discCtx, discCancel := context.WithCancel(context.Background())
	go disc.Run(discCtx)
	defer discCancel()

	// Wait for the engine to have written 8 samples (one per TTL in the sweep).
	waitFor(t, func() bool { return sink.sampleCount() >= 8 }, "8 samples from immediate sweep")

	// Every TTL 1..8 should have been probed exactly once.
	for ttl := uint8(1); ttl <= 8; ttl++ {
		if got := proboe.callCount(ttl); got != 1 {
			t.Errorf("TTL %d probed %d times, want 1", ttl, got)
		}
	}
}

func TestDiscovery_RecordsTimeoutsAsTimedOut(t *testing.T) {
	eng, sink, cancel := startEngineForTest(t, "8.8.8.8")
	defer cancel()

	proboe := newFakeProber() // nil responder → everything times out

	disc, err := NewDiscovery(DiscoveryConfig{
		Target: eng.cfg.Target, Interval: 10 * time.Second, MaxHops: 4, Timeout: 100 * time.Millisecond,
	}, proboe, eng, nil)
	if err != nil {
		t.Fatalf("NewDiscovery: %v", err)
	}

	discCtx, discCancel := context.WithCancel(context.Background())
	go disc.Run(discCtx)
	defer discCancel()

	waitFor(t, func() bool { return sink.sampleCount() >= 4 }, "4 timeout samples")

	// Every sample should have zero IP (i.e. the timeout was recorded).
	for _, s := range sink.samples {
		if s.IP.IsValid() {
			t.Errorf("TTL %d: sample has IP %v, want zero (was supposed to time out)", s.TTL, s.IP)
		}
	}
}

func TestDiscovery_HandlesErrorsAsTimedOut(t *testing.T) {
	eng, sink, cancel := startEngineForTest(t, "8.8.8.8")
	defer cancel()

	proboe := newFakeProber()
	proboe.setResponder(func(ttl uint8) (Result, error) {
		return Result{}, errors.New("synthetic socket error")
	})

	disc, err := NewDiscovery(DiscoveryConfig{
		Target: eng.cfg.Target, Interval: 10 * time.Second, MaxHops: 3, Timeout: 100 * time.Millisecond,
	}, proboe, eng, nil)
	if err != nil {
		t.Fatalf("NewDiscovery: %v", err)
	}

	discCtx, discCancel := context.WithCancel(context.Background())
	go disc.Run(discCtx)
	defer discCancel()

	waitFor(t, func() bool { return sink.sampleCount() >= 3 }, "3 samples despite errors")

	// All samples should have zero IP (errors → TimedOut → IP unset).
	for _, s := range sink.samples {
		if s.IP.IsValid() {
			t.Errorf("TTL %d: error path produced a valid IP %v, want zero", s.TTL, s.IP)
		}
	}
}

func TestDiscovery_ExitsCleanlyOnCancel(t *testing.T) {
	eng, _, cancel := startEngineForTest(t, "8.8.8.8")
	defer cancel()

	proboe := newFakeProber()
	disc, err := NewDiscovery(DiscoveryConfig{
		Target: eng.cfg.Target, Interval: 10 * time.Millisecond, MaxHops: 2, Timeout: 100 * time.Millisecond,
	}, proboe, eng, nil)
	if err != nil {
		t.Fatalf("NewDiscovery: %v", err)
	}

	discCtx, discCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		disc.Run(discCtx)
		close(done)
	}()

	// Let it run briefly, then cancel.
	time.Sleep(30 * time.Millisecond)
	discCancel()

	select {
	case <-done:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("Discovery did not exit within 2s of context cancel")
	}
}

func TestNewDiscovery_RejectsInvalidConfig(t *testing.T) {
	eng, _, cancel := startEngineForTest(t, "8.8.8.8")
	defer cancel()
	proboe := newFakeProber()

	good := DiscoveryConfig{
		Target:   eng.cfg.Target,
		Interval: time.Second,
		MaxHops:  30,
		Timeout:  time.Second,
	}

	tests := []struct {
		name   string
		mutate func(*DiscoveryConfig)
	}{
		{"invalid target", func(c *DiscoveryConfig) { c.Target = netip.Addr{} }},
		{"zero interval", func(c *DiscoveryConfig) { c.Interval = 0 }},
		{"zero MaxHops", func(c *DiscoveryConfig) { c.MaxHops = 0 }},
		{"MaxHops > ceiling", func(c *DiscoveryConfig) { c.MaxHops = maxTTL + 1 }},
		{"zero timeout", func(c *DiscoveryConfig) { c.Timeout = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := good
			tt.mutate(&cfg)
			if _, err := NewDiscovery(cfg, proboe, eng, nil); err == nil {
				t.Errorf("NewDiscovery accepted invalid config")
			}
		})
	}
}

// ---------- Pinger tests ----------

func TestPinger_SkipsTickWhenNoHopsKnown(t *testing.T) {
	eng, _, cancel := startEngineForTest(t, "8.8.8.8")
	defer cancel()

	proboe := newFakeProber()
	pinger, err := NewPinger(PingerConfig{
		Target:   eng.cfg.Target,
		Interval: 10 * time.Millisecond,
		Timeout:  100 * time.Millisecond,
	}, proboe, eng, nil)
	if err != nil {
		t.Fatalf("NewPinger: %v", err)
	}

	pingCtx, pingCancel := context.WithCancel(context.Background())
	go pinger.Run(pingCtx)
	defer pingCancel()

	// Let it tick several times.
	time.Sleep(80 * time.Millisecond)

	if got := proboe.totalCalls(); got != 0 {
		t.Errorf("pinger probed %d times with no hops known; want 0", got)
	}
}

func TestPinger_ProbesEveryKnownHop(t *testing.T) {
	eng, sink, cancel := startEngineForTest(t, "8.8.8.8")
	defer cancel()

	// Seed the engine with 3 hops via SendProbe — the pinger will pick
	// these up on its next tick.
	for ttl := uint8(1); ttl <= 3; ttl++ {
		ip, _ := netip.ParseAddr("10.0.0." + string([]byte{'0' + byte(ttl)}))
		_ = eng.SendProbe(context.Background(), ProbeResult{
			Target: eng.cfg.Target, TTL: ttl, Ts: time.Now(),
			RespIP: ip, RTT: time.Millisecond, Reply: ReplyTimeExceeded,
		})
	}
	waitFor(t, func() bool { return sink.sampleCount() >= 3 }, "3 seed samples in engine")

	// Now start the pinger with a fast tick.
	proboe := newFakeProber()
	proboe.setResponder(linearPathResponder(t, eng.cfg.Target, 5))

	pinger, err := NewPinger(PingerConfig{
		Target:   eng.cfg.Target,
		Interval: 10 * time.Millisecond,
		Timeout:  100 * time.Millisecond,
	}, proboe, eng, nil)
	if err != nil {
		t.Fatalf("NewPinger: %v", err)
	}

	pingCtx, pingCancel := context.WithCancel(context.Background())
	go pinger.Run(pingCtx)
	defer pingCancel()

	// Wait for each of TTL 1..3 to have been pinged at least once.
	waitFor(t, func() bool {
		return proboe.callCount(1) >= 1 && proboe.callCount(2) >= 1 && proboe.callCount(3) >= 1
	}, "pinger probes every known hop")
}

// TestPinger_SlowProbeDoesNotBlockNextTick is the regression test for the
// step-5 bug where cohort-blocking in tick() capped the tick cadence to
// the slowest probe in the path. With fire-and-forget per-probe
// goroutines, a slow probe must not delay the next tick.
//
// Setup: one known hop, prober configured to sleep ~80ms per probe.
// Ticker fires every 10ms. After ~60ms wall time, we expect several
// ticks to have fired (and several call counts logged) even though no
// single probe has completed yet.
//
// Cohort-blocking version would have produced exactly 0 or 1 call in
// 60ms (the first probe is still sleeping). Fire-and-forget produces
// at least 3 — one per tick.
func TestPinger_SlowProbeDoesNotBlockNextTick(t *testing.T) {
	eng, sink, cancel := startEngineForTest(t, "8.8.8.8")
	defer cancel()

	// Seed engine with TTL 1 so the pinger has a hop to probe.
	ip, _ := netip.ParseAddr("10.0.0.1")
	_ = eng.SendProbe(context.Background(), ProbeResult{
		Target: eng.cfg.Target, TTL: 1, Ts: time.Now(),
		RespIP: ip, RTT: time.Millisecond, Reply: ReplyTimeExceeded,
	})
	waitFor(t, func() bool { return sink.sampleCount() >= 1 }, "seed sample processed")

	// Prober that sleeps before returning — simulates the slow-anonymous-
	// hop case (a TTL timing out at 2s on a real path during step-5
	// testing).
	slow := newFakeProber()
	slow.setResponder(func(ttl uint8) (Result, error) {
		time.Sleep(80 * time.Millisecond)
		return Result{}, ErrTimeout
	})

	pinger, err := NewPinger(PingerConfig{
		Target:   eng.cfg.Target,
		Interval: 10 * time.Millisecond,
		Timeout:  500 * time.Millisecond,
	}, slow, eng, nil)
	if err != nil {
		t.Fatalf("NewPinger: %v", err)
	}

	pingCtx, pingCancel := context.WithCancel(context.Background())
	go pinger.Run(pingCtx)

	// Let the pinger tick several times. The slow probe takes 80ms;
	// with a 10ms tick interval we should see ~5 ticks in 60ms.
	time.Sleep(60 * time.Millisecond)
	pingCancel()

	// At least 3 calls — strictly more than what cohort-blocking would
	// have produced (cohort-blocking gives 0 or 1 in this window).
	got := slow.callCount(1)
	if got < 3 {
		t.Errorf("pinger made %d calls in 60ms with 10ms interval and 80ms-per-probe sleep; "+
			"want >= 3 (fire-and-forget should not block on probe completion)", got)
	}
}

// Step-41: in FinalHopOnly mode the pinger probes only the target
// TTL, never intermediate hops. Pins the bandwidth-saving promise —
// breaking this would silently 10×-30× the probe traffic for any
// tab the operator opted into final-hop-only mode.
func TestPinger_FinalHopOnlyProbesOnlyTargetTTL(t *testing.T) {
	eng, sink, cancel := startEngineForTest(t, "8.8.8.8")
	defer cancel()

	// Seed 5 hops + mark TTL 5 as the destination (ReplyEchoReply
	// at the target TTL is what PathState reads as "we reached the
	// destination"). Without TargetTTL set, FinalHopOnly idles —
	// that path is covered by the next test.
	for ttl := uint8(1); ttl <= 5; ttl++ {
		ip, _ := netip.ParseAddr("10.0.0." + string([]byte{'0' + byte(ttl)}))
		reply := ReplyTimeExceeded
		if ttl == 5 {
			ip = eng.cfg.Target
			reply = ReplyEchoReply
		}
		_ = eng.SendProbe(context.Background(), ProbeResult{
			Target: eng.cfg.Target, TTL: ttl, Ts: time.Now(),
			RespIP: ip, RTT: time.Millisecond, Reply: reply,
		})
	}
	waitFor(t, func() bool {
		snap, err := eng.PathSnapshot(context.Background())
		return err == nil && snap.TargetTTL == 5
	}, "engine learns target TTL")
	_ = sink

	proboe := newFakeProber()
	proboe.setResponder(linearPathResponder(t, eng.cfg.Target, 5))

	pinger, err := NewPinger(PingerConfig{
		Target:       eng.cfg.Target,
		Interval:     10 * time.Millisecond,
		Timeout:      100 * time.Millisecond,
		FinalHopOnly: true,
	}, proboe, eng, nil)
	if err != nil {
		t.Fatalf("NewPinger: %v", err)
	}

	pingCtx, pingCancel := context.WithCancel(context.Background())
	go pinger.Run(pingCtx)
	defer pingCancel()

	// Wait for the target TTL to be probed several times.
	waitFor(t, func() bool { return proboe.callCount(5) >= 3 }, "target TTL gets pinged")

	// Intermediate TTLs must never have been probed.
	for ttl := uint8(1); ttl <= 4; ttl++ {
		if proboe.callCount(ttl) != 0 {
			t.Errorf("FinalHopOnly: TTL %d probed %d times, want 0", ttl, proboe.callCount(ttl))
		}
	}
}

// Step-41: FinalHopOnly idles when the target TTL isn't known yet —
// probing every TTL while we're waiting for discovery to converge
// would defeat the bandwidth-saving point. Once discovery learns the
// target, the pinger picks up and only probes that TTL.
func TestPinger_FinalHopOnlyIdlesUntilTargetKnown(t *testing.T) {
	eng, _, cancel := startEngineForTest(t, "8.8.8.8")
	defer cancel()

	// Seed a hop but NOT mark the target as reached (all TimeExceeded).
	for ttl := uint8(1); ttl <= 3; ttl++ {
		ip, _ := netip.ParseAddr("10.0.0." + string([]byte{'0' + byte(ttl)}))
		_ = eng.SendProbe(context.Background(), ProbeResult{
			Target: eng.cfg.Target, TTL: ttl, Ts: time.Now(),
			RespIP: ip, RTT: time.Millisecond, Reply: ReplyTimeExceeded,
		})
	}
	waitFor(t, func() bool {
		snap, err := eng.PathSnapshot(context.Background())
		return err == nil && len(snap.Hops) >= 3 && snap.TargetTTL == 0
	}, "engine has hops without a target TTL")

	proboe := newFakeProber()
	proboe.setResponder(linearPathResponder(t, eng.cfg.Target, 5))

	pinger, err := NewPinger(PingerConfig{
		Target: eng.cfg.Target, Interval: 10 * time.Millisecond,
		Timeout: 100 * time.Millisecond, FinalHopOnly: true,
	}, proboe, eng, nil)
	if err != nil {
		t.Fatalf("NewPinger: %v", err)
	}

	pingCtx, pingCancel := context.WithCancel(context.Background())
	go pinger.Run(pingCtx)
	defer pingCancel()

	// Let many ticks fire — the pinger should sit idle since no
	// target TTL is known.
	time.Sleep(80 * time.Millisecond)
	if total := proboe.totalCalls(); total != 0 {
		t.Errorf("FinalHopOnly with unknown target TTL: %d probes sent, want 0", total)
	}
}

func TestPinger_ExitsCleanlyOnCancel(t *testing.T) {
	eng, _, cancel := startEngineForTest(t, "8.8.8.8")
	defer cancel()
	proboe := newFakeProber()

	pinger, err := NewPinger(PingerConfig{
		Target: eng.cfg.Target, Interval: 10 * time.Millisecond, Timeout: 100 * time.Millisecond,
	}, proboe, eng, nil)
	if err != nil {
		t.Fatalf("NewPinger: %v", err)
	}

	pingCtx, pingCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pinger.Run(pingCtx)
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	pingCancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Pinger did not exit within 2s of context cancel")
	}
}

func TestNewPinger_RejectsInvalidConfig(t *testing.T) {
	eng, _, cancel := startEngineForTest(t, "8.8.8.8")
	defer cancel()
	proboe := newFakeProber()

	good := PingerConfig{Target: eng.cfg.Target, Interval: time.Second, Timeout: time.Second}
	tests := []struct {
		name   string
		mutate func(*PingerConfig)
	}{
		{"invalid target", func(c *PingerConfig) { c.Target = netip.Addr{} }},
		{"zero interval", func(c *PingerConfig) { c.Interval = 0 }},
		{"zero timeout", func(c *PingerConfig) { c.Timeout = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := good
			tt.mutate(&cfg)
			if _, err := NewPinger(cfg, proboe, eng, nil); err == nil {
				t.Errorf("NewPinger accepted invalid config")
			}
		})
	}
}

// ensure ctx is touched in test scope (avoids unused-variable warning
// from startEngineForTest's signature when not all callers use it)
var _ = context.Background
