package probe

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// captureSink records every Sample and RouteChange it receives. Tests
// inspect its slices to verify what the reducer emitted. Safe for
// concurrent use because the engine's reducer calls these from one
// goroutine, but tests may inspect from another.
type captureSink struct {
	mu      sync.Mutex
	samples []Sample
	changes []RouteChange

	// attempts increments on every WriteSample call regardless of
	// success/failure. Tests use this to wait for the reducer to have
	// processed N probes when failSamples is true (the samples slice
	// would otherwise stay empty and tests couldn't synchronize on it).
	attempts int

	// failNext, when set, makes the next write return the given error.
	// Used to verify the reducer survives sink failures.
	failSamples bool
	failChanges bool
}

func (c *captureSink) WriteSample(s Sample) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts++
	if c.failSamples {
		return errors.New("captureSink: intentional WriteSample failure")
	}
	c.samples = append(c.samples, s)
	return nil
}

func (c *captureSink) WriteRouteChange(rc RouteChange) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failChanges {
		return errors.New("captureSink: intentional WriteRouteChange failure")
	}
	c.changes = append(c.changes, rc)
	return nil
}

func (c *captureSink) sampleCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.samples)
}

func (c *captureSink) attemptCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts
}

func (c *captureSink) changeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.changes)
}

func (c *captureSink) snapshotChanges() []RouteChange {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]RouteChange, len(c.changes))
	copy(out, c.changes)
	return out
}

// newRunningEngine builds an Engine and starts its reducer in a
// background goroutine. The returned cleanup function cancels the
// engine's context and waits for the reducer to exit.
func newRunningEngine(t *testing.T, target string, sink Sink) (*Engine, func()) {
	t.Helper()

	addr, err := netip.ParseAddr(target)
	if err != nil {
		t.Fatalf("bad target %q: %v", target, err)
	}
	eng, err := NewEngine(EngineConfig{
		Target:               addr,
		RouteChangeThreshold: 3,
	}, sink, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = eng.Run(ctx)
	}()

	cleanup := func() {
		cancel()
		select {
		case <-eng.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("engine did not exit within 2s of context cancel")
		}
	}
	return eng, cleanup
}

// waitFor polls until cond returns true, or the deadline elapses. The
// reducer is concurrent with the test; we can't just check immediately
// after SendProbe because the reducer may not have processed yet.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

func TestEngine_ProcessesProbeResult_EmitsSample(t *testing.T) {
	sink := &captureSink{}
	eng, cleanup := newRunningEngine(t, "8.8.8.8", sink)
	defer cleanup()

	router, _ := netip.ParseAddr("10.0.0.1")
	err := eng.SendProbe(context.Background(), ProbeResult{
		Target: eng.cfg.Target,
		TTL:    1,
		Ts:     time.Now(),
		RespIP: router,
		RTT:    3 * time.Millisecond,
		Reply:  ReplyTimeExceeded,
	})
	if err != nil {
		t.Fatalf("SendProbe: %v", err)
	}

	waitFor(t, func() bool { return sink.sampleCount() == 1 }, "1 sample")
}

func TestEngine_RouteChangeEmittedAtThreshold(t *testing.T) {
	sink := &captureSink{}
	eng, cleanup := newRunningEngine(t, "8.8.8.8", sink)
	defer cleanup()

	a, _ := netip.ParseAddr("10.0.0.1")
	b, _ := netip.ParseAddr("10.0.0.2")
	ts := time.Now()

	// Seed with A so CurrentIP becomes A.
	for i := 0; i < 3; i++ {
		_ = eng.SendProbe(context.Background(), ProbeResult{
			Target: eng.cfg.Target, TTL: 1, Ts: ts, RespIP: a, RTT: time.Millisecond, Reply: ReplyTimeExceeded,
		})
	}
	// Three sightings of B should fire the route change.
	for i := 0; i < 3; i++ {
		_ = eng.SendProbe(context.Background(), ProbeResult{
			Target: eng.cfg.Target, TTL: 1, Ts: ts, RespIP: b, RTT: time.Millisecond, Reply: ReplyTimeExceeded,
		})
	}

	waitFor(t, func() bool { return sink.changeCount() >= 1 }, "1 RouteChange")
	changes := sink.snapshotChanges()
	if changes[0].OldIP != a || changes[0].NewIP != b {
		t.Errorf("RouteChange = %v→%v, want %v→%v", changes[0].OldIP, changes[0].NewIP, a, b)
	}
	if changes[0].TTL != 1 {
		t.Errorf("RouteChange.TTL = %d, want 1", changes[0].TTL)
	}
}

func TestEngine_SweepResult_AppliesEveryTTL(t *testing.T) {
	sink := &captureSink{}
	eng, cleanup := newRunningEngine(t, "8.8.8.8", sink)
	defer cleanup()

	r1, _ := netip.ParseAddr("10.0.0.1")
	r2, _ := netip.ParseAddr("10.0.0.2")
	r3, _ := netip.ParseAddr("10.0.0.3")
	ts := time.Now()

	sweep := SweepResult{
		Target: eng.cfg.Target,
		Ts:     ts,
		Results: []ProbeResult{
			{Target: eng.cfg.Target, TTL: 1, Ts: ts, RespIP: r1, RTT: 1 * time.Millisecond, Reply: ReplyTimeExceeded},
			{Target: eng.cfg.Target, TTL: 2, Ts: ts, RespIP: r2, RTT: 2 * time.Millisecond, Reply: ReplyTimeExceeded},
			{Target: eng.cfg.Target, TTL: 3, Ts: ts, RespIP: r3, RTT: 3 * time.Millisecond, Reply: ReplyTimeExceeded},
		},
	}
	if err := eng.SendSweep(context.Background(), sweep); err != nil {
		t.Fatalf("SendSweep: %v", err)
	}

	waitFor(t, func() bool { return sink.sampleCount() == 3 }, "3 samples from sweep")
}

func TestEngine_SweepResult_SkipsZeroSlots(t *testing.T) {
	sink := &captureSink{}
	eng, cleanup := newRunningEngine(t, "8.8.8.8", sink)
	defer cleanup()

	r1, _ := netip.ParseAddr("10.0.0.1")
	ts := time.Now()

	// Sweep with TTL 1 populated and an empty zero-value slot afterward.
	sweep := SweepResult{
		Target: eng.cfg.Target,
		Ts:     ts,
		Results: []ProbeResult{
			{Target: eng.cfg.Target, TTL: 1, Ts: ts, RespIP: r1, RTT: time.Millisecond, Reply: ReplyTimeExceeded},
			{}, // zero TTL → reducer should skip
		},
	}
	if err := eng.SendSweep(context.Background(), sweep); err != nil {
		t.Fatalf("SendSweep: %v", err)
	}

	// Wait a beat so the reducer has time to process. Can't waitFor an
	// exact count without racing; use a short sleep then assert.
	time.Sleep(50 * time.Millisecond)
	if got := sink.sampleCount(); got != 1 {
		t.Errorf("sample count = %d, want 1 (zero slot should be skipped)", got)
	}
}

func TestEngine_PathSnapshot_ReturnsAllResponded(t *testing.T) {
	sink := &captureSink{}
	eng, cleanup := newRunningEngine(t, "8.8.8.8", sink)
	defer cleanup()

	r1, _ := netip.ParseAddr("10.0.0.1")
	r3, _ := netip.ParseAddr("10.0.0.3")
	ts := time.Now()

	for _, p := range []ProbeResult{
		{Target: eng.cfg.Target, TTL: 1, Ts: ts, RespIP: r1, RTT: 1 * time.Millisecond, Reply: ReplyTimeExceeded},
		{Target: eng.cfg.Target, TTL: 3, Ts: ts, RespIP: r3, RTT: 3 * time.Millisecond, Reply: ReplyTimeExceeded},
	} {
		_ = eng.SendProbe(context.Background(), p)
	}
	waitFor(t, func() bool { return sink.sampleCount() == 2 }, "2 samples processed")

	snap, err := eng.PathSnapshot(context.Background())
	if err != nil {
		t.Fatalf("PathSnapshot: %v", err)
	}
	if len(snap.Hops) != 2 {
		t.Fatalf("snapshot length = %d, want 2", len(snap.Hops))
	}
	if snap.Hops[0].TTL != 1 || snap.Hops[1].TTL != 3 {
		t.Errorf("snapshot TTLs = %d,%d, want 1,3", snap.Hops[0].TTL, snap.Hops[1].TTL)
	}
}

func TestEngine_SinkFailureDoesNotCrashReducer(t *testing.T) {
	sink := &captureSink{failSamples: true}
	eng, cleanup := newRunningEngine(t, "8.8.8.8", sink)
	defer cleanup()

	router, _ := netip.ParseAddr("10.0.0.1")
	ts := time.Now()
	// Several probes that should all fail at the sink — reducer must
	// log and continue, not crash.
	for i := 0; i < 5; i++ {
		err := eng.SendProbe(context.Background(), ProbeResult{
			Target: eng.cfg.Target, TTL: 1, Ts: ts, RespIP: router, RTT: time.Millisecond, Reply: ReplyTimeExceeded,
		})
		if err != nil {
			t.Fatalf("SendProbe %d: %v", i, err)
		}
	}

	// Wait until every probe has been processed by the reducer. With
	// failSamples=true, the samples slice stays empty, but the attempt
	// counter still increments — that's our synchronization point. Without
	// this, the snapshot query could race the probe events on separate
	// channels and arrive at the reducer first.
	waitFor(t, func() bool { return sink.attemptCount() == 5 }, "5 probe attempts")

	// If the reducer survived, queries still work and state was updated
	// before the sink errors.
	snap, err := eng.PathSnapshot(context.Background())
	if err != nil {
		t.Fatalf("PathSnapshot after sink failures: %v", err)
	}
	if len(snap.Hops) != 1 {
		t.Errorf("snapshot length = %d, want 1 (state should still be updated despite sink errors)", len(snap.Hops))
	}
}

func TestEngine_GracefulShutdown(t *testing.T) {
	sink := &captureSink{}
	addr, _ := netip.ParseAddr("8.8.8.8")
	eng, err := NewEngine(EngineConfig{Target: addr, RouteChangeThreshold: 3}, sink, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = eng.Run(ctx) }()

	cancel()
	select {
	case <-eng.Done():
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("engine did not exit within 2s of context cancel")
	}
}

func TestNewEngine_RejectsInvalidConfig(t *testing.T) {
	sink := &captureSink{}
	addr, _ := netip.ParseAddr("8.8.8.8")

	tests := []struct {
		name string
		cfg  EngineConfig
		sink Sink
	}{
		{"invalid target", EngineConfig{Target: netip.Addr{}, RouteChangeThreshold: 3}, sink},
		{"zero threshold", EngineConfig{Target: addr, RouteChangeThreshold: 0}, sink},
		{"negative threshold", EngineConfig{Target: addr, RouteChangeThreshold: -1}, sink},
		{"nil sink", EngineConfig{Target: addr, RouteChangeThreshold: 3}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewEngine(tt.cfg, tt.sink, nil); err == nil {
				t.Errorf("NewEngine accepted invalid config")
			}
		})
	}
}
