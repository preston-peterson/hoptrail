package probe

import (
	"net/netip"
	"testing"
	"time"
)

func mustIP(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("bad test IP %q: %v", s, err)
	}
	return a
}

// newTestState gives tests a PathState with small ring buffers so they
// don't need to fabricate hundreds of probes to exercise behavior.
func newTestState(t *testing.T, target string) *PathState {
	t.Helper()
	return newPathStateWithCaps(mustIP(t, target), target, 6, 10)
}

func TestApplyProbeResult_FirstObservationSetsCurrentIP(t *testing.T) {
	s := newTestState(t, "8.8.8.8")
	router := mustIP(t, "203.0.113.1")

	sample, rc := s.ApplyProbeResult(ProbeResult{
		Target: mustIP(t, "8.8.8.8"),
		TTL:    1,
		Ts:     time.Now(),
		RespIP: router,
		RTT:    5 * time.Millisecond,
		Reply:  ReplyTimeExceeded,
	}, 3)

	if rc != nil {
		t.Errorf("first observation fired a RouteChange (%v→%v); want none — change is from invalid to first IP, not a 'change'", rc.OldIP, rc.NewIP)
	}
	if sample.IP != router {
		t.Errorf("Sample.IP = %v, want %v", sample.IP, router)
	}
	if hop := s.hops[1]; hop == nil || hop.CurrentIP != router {
		t.Errorf("Hop.CurrentIP after one observation = %v, want %v (tentative identity should be set from modal)", hop.CurrentIP, router)
	}
	if s.maxSeen != 1 {
		t.Errorf("maxSeen = %d, want 1", s.maxSeen)
	}
}

func TestApplyProbeResult_RouteChangeFiresAtThreshold(t *testing.T) {
	s := newTestState(t, "8.8.8.8")
	a := mustIP(t, "203.0.113.1")
	b := mustIP(t, "203.0.113.2")
	ts := time.Now()

	// Seed the hop with A so CurrentIP = A.
	for i := 0; i < 3; i++ {
		s.ApplyProbeResult(ProbeResult{
			Target: s.Target(), TTL: 1, Ts: ts, RespIP: a, RTT: time.Millisecond, Reply: ReplyTimeExceeded,
		}, 3)
	}
	if s.hops[1].CurrentIP != a {
		t.Fatalf("setup: CurrentIP after 3×A = %v, want %v", s.hops[1].CurrentIP, a)
	}

	// Now feed B three times — at the third, the route change should fire.
	var lastRC *RouteChange
	for i := 0; i < 3; i++ {
		_, rc := s.ApplyProbeResult(ProbeResult{
			Target: s.Target(), TTL: 1, Ts: ts, RespIP: b, RTT: time.Millisecond, Reply: ReplyTimeExceeded,
		}, 3)
		if rc != nil {
			lastRC = rc
		}
	}

	if lastRC == nil {
		t.Fatal("3 consecutive sightings of B did not fire a RouteChange")
	}
	if lastRC.OldIP != a {
		t.Errorf("RouteChange.OldIP = %v, want %v", lastRC.OldIP, a)
	}
	if lastRC.NewIP != b {
		t.Errorf("RouteChange.NewIP = %v, want %v", lastRC.NewIP, b)
	}
	if s.hops[1].CurrentIP != b {
		t.Errorf("Hop.CurrentIP after change fired = %v, want %v", s.hops[1].CurrentIP, b)
	}
}

func TestApplyProbeResult_ECMPFlappingDoesNotFire(t *testing.T) {
	s := newTestState(t, "8.8.8.8")
	a := mustIP(t, "203.0.113.1")
	b := mustIP(t, "203.0.113.2")
	ts := time.Now()

	// Alternate A and B; threshold 3 should never be reached.
	for i := 0; i < 10; i++ {
		ip := a
		if i%2 == 1 {
			ip = b
		}
		_, rc := s.ApplyProbeResult(ProbeResult{
			Target: s.Target(), TTL: 1, Ts: ts, RespIP: ip, RTT: time.Millisecond, Reply: ReplyTimeExceeded,
		}, 3)
		if rc != nil {
			t.Fatalf("iteration %d: ECMP flapping fired a route change", i)
		}
	}
}

func TestApplyProbeResult_TimeoutRecordedAsLoss(t *testing.T) {
	s := newTestState(t, "8.8.8.8")
	ts := time.Now()

	sample, rc := s.ApplyProbeResult(ProbeResult{
		Target:   s.Target(),
		TTL:      1,
		Ts:       ts,
		TimedOut: true,
	}, 3)

	if rc != nil {
		t.Error("timeout fired a RouteChange; want none")
	}
	if sample.IP.IsValid() {
		t.Errorf("Sample.IP for timeout = %v, want zero", sample.IP)
	}
	if sample.RTT != 0 {
		t.Errorf("Sample.RTT for timeout = %s, want 0", sample.RTT)
	}
	if s.maxSeen != 0 {
		t.Errorf("maxSeen after only-timeouts = %d, want 0 (timeouts don't bump maxSeen)", s.maxSeen)
	}
}

// Step-64 regression: when the local network briefly drops, the
// kernel returns ICMP DestUnreachable for every outbound probe with
// the source IP set to the local box. Previously the reducer treated
// these like real hop responses, pushing the local IP into every
// TTL's candidates buffer; after enough consecutive DestUnreach the
// route-change detector flipped every hop's identity to the local IP,
// then flipped back when the network recovered (operator caught this
// in the route-changes panel: every TTL on the path "moved" to the
// local LAN address simultaneously, then back, ~15 seconds apart).
// Fix: DestUnreachable is treated like a timeout for candidate
// tracking, loss counting, and last-response.
func TestApplyProbeResult_DestUnreachableDoesNotPolluteHopIdentity(t *testing.T) {
	s := newTestState(t, "1.1.1.1")
	router := mustIP(t, "203.0.113.1")
	localBox := mustIP(t, "192.0.2.77")
	ts := time.Now()

	// Seed TTL=2 with a real router via TimeExceeded so it has a known identity.
	for i := 0; i < 3; i++ {
		s.ApplyProbeResult(ProbeResult{
			Target: s.Target(), TTL: 2, Ts: ts, RespIP: router, RTT: time.Millisecond, Reply: ReplyTimeExceeded,
		}, 3)
	}
	if s.hops[2].CurrentIP != router {
		t.Fatalf("setup: CurrentIP = %v, want %v", s.hops[2].CurrentIP, router)
	}

	// Now feed 5 DestUnreachable "responses" from the local box — what
	// the kernel returns when egress fails. This must NOT flip the hop's
	// identity to localBox.
	for i := 0; i < 5; i++ {
		sample, rc := s.ApplyProbeResult(ProbeResult{
			Target: s.Target(), TTL: 2, Ts: ts, RespIP: localBox, RTT: 50 * time.Microsecond, Reply: ReplyDestUnreachable,
		}, 3)
		if rc != nil {
			t.Fatalf("iter %d: DestUnreachable from localBox fired a RouteChange to %v (want none)", i, rc.NewIP)
		}
		// Sample should NOT carry the local box's IP — it's a miss.
		if sample.IP.IsValid() {
			t.Errorf("iter %d: Sample.IP = %v, want zero (DestUnreachable is a miss)", i, sample.IP)
		}
		if sample.RTT != 0 {
			t.Errorf("iter %d: Sample.RTT = %s, want 0 (DestUnreachable is a miss)", i, sample.RTT)
		}
	}

	if s.hops[2].CurrentIP != router {
		t.Errorf("CurrentIP after 5×DestUnreachable from localBox = %v, want %v (hop identity must be sticky across local-egress failures)", s.hops[2].CurrentIP, router)
	}

	// Loss should reflect the misses — 5 anonymous out of 8 total.
	losses := s.hops[2].recentLosses.NewestFirst()
	missCount := 0
	for _, l := range losses {
		if l {
			missCount++
		}
	}
	if missCount != 5 {
		t.Errorf("recent loss count after 5 DestUnreachable = %d, want 5 (DestUnreachable counts as a miss)", missCount)
	}
}

func TestApplyProbeResult_AnonymousHopStaysAnonymous(t *testing.T) {
	s := newTestState(t, "8.8.8.8")
	ts := time.Now()

	for i := 0; i < 4; i++ {
		s.ApplyProbeResult(ProbeResult{Target: s.Target(), TTL: 4, Ts: ts, TimedOut: true}, 3)
	}

	hop := s.hops[4]
	if hop == nil {
		t.Fatal("Hop at TTL 4 was not created despite observations")
	}
	if hop.CurrentIP.IsValid() {
		t.Errorf("CurrentIP for all-anonymous hop = %v, want zero", hop.CurrentIP)
	}
}

func TestApplyProbeResult_PanicsOnInvalidTTL(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("ApplyProbeResult(TTL=0) did not panic")
		}
	}()
	s := newTestState(t, "8.8.8.8")
	s.ApplyProbeResult(ProbeResult{Target: s.Target(), TTL: 0, Ts: time.Now()}, 3)
}

func TestSnapshot_OrdersByTTL(t *testing.T) {
	s := newTestState(t, "8.8.8.8")
	ts := time.Now()

	// Out-of-order applies: TTL 3 first, then 1.
	s.ApplyProbeResult(ProbeResult{Target: s.Target(), TTL: 3, Ts: ts, RespIP: mustIP(t, "203.0.113.3"), RTT: 3 * time.Millisecond, Reply: ReplyTimeExceeded}, 3)
	s.ApplyProbeResult(ProbeResult{Target: s.Target(), TTL: 1, Ts: ts, RespIP: mustIP(t, "203.0.113.1"), RTT: 1 * time.Millisecond, Reply: ReplyTimeExceeded}, 3)

	snap := s.Snapshot()
	if len(snap.Hops) != 2 {
		t.Fatalf("Snapshot length = %d, want 2 (TTLs 1 and 3; TTL 2 was never observed and should be skipped)", len(snap.Hops))
	}
	if snap.Hops[0].TTL != 1 {
		t.Errorf("Snapshot[0].TTL = %d, want 1", snap.Hops[0].TTL)
	}
	if snap.Hops[1].TTL != 3 {
		t.Errorf("Snapshot[1].TTL = %d, want 3", snap.Hops[1].TTL)
	}
}

func TestSnapshot_ComputesAvgAndLoss(t *testing.T) {
	s := newTestState(t, "8.8.8.8")
	ts := time.Now()
	router := mustIP(t, "203.0.113.1")

	// Mix of responses and timeouts to exercise both averages.
	for _, rtt := range []time.Duration{2 * time.Millisecond, 4 * time.Millisecond, 6 * time.Millisecond} {
		s.ApplyProbeResult(ProbeResult{Target: s.Target(), TTL: 1, Ts: ts, RespIP: router, RTT: rtt, Reply: ReplyTimeExceeded}, 3)
	}
	// One timeout in the window.
	s.ApplyProbeResult(ProbeResult{Target: s.Target(), TTL: 1, Ts: ts, TimedOut: true}, 3)

	snap := s.Snapshot()
	if len(snap.Hops) != 1 {
		t.Fatalf("Snapshot length = %d, want 1", len(snap.Hops))
	}
	hs := snap.Hops[0]

	wantAvg := 4 * time.Millisecond // (2+4+6)/3
	if hs.AvgRTT != wantAvg {
		t.Errorf("AvgRTT = %s, want %s", hs.AvgRTT, wantAvg)
	}
	if hs.CurrentRTT != 6*time.Millisecond {
		t.Errorf("CurrentRTT = %s, want 6ms (most recent non-timeout RTT)", hs.CurrentRTT)
	}
	if hs.MinRTT != 2*time.Millisecond {
		t.Errorf("MinRTT = %s, want 2ms (smallest non-timeout RTT)", hs.MinRTT)
	}
	wantLossPct := 100.0 * 1.0 / 4.0 // 1 of 4 observations was a timeout
	if hs.LossPercent != wantLossPct {
		t.Errorf("LossPercent = %v, want %v", hs.LossPercent, wantLossPct)
	}
}

func TestSnapshot_EmptyStateReturnsNil(t *testing.T) {
	s := newTestState(t, "8.8.8.8")
	snap := s.Snapshot()
	if snap.Hops != nil {
		t.Errorf("Snapshot().Hops on empty state = %v, want nil", snap.Hops)
	}
	if snap.TargetTTL != 0 {
		t.Errorf("Snapshot().TargetTTL on empty state = %d, want 0", snap.TargetTTL)
	}
}

// TestTargetTTL_SetByEchoReply pins the core "stop at target" behavior:
// when a TTL responds with EchoReply, that TTL becomes the path's
// target and the Snapshot's TargetTTL is set. Subsequent EchoReplies at
// lower TTLs lower TargetTTL (route shortened); at higher TTLs leave it
// unchanged (just more echoes from the same destination).
func TestTargetTTL_SetByEchoReply(t *testing.T) {
	s := newTestState(t, "8.8.8.8")
	ts := time.Now()
	dst := mustIP(t, "8.8.8.8")
	router := mustIP(t, "203.0.113.1")

	// Phase 1: a transit router at TTL 1 — TargetTTL stays 0.
	s.ApplyProbeResult(ProbeResult{
		Target: s.Target(), TTL: 1, Ts: ts, RespIP: router,
		RTT: time.Millisecond, Reply: ReplyTimeExceeded,
	}, 3)
	if got := s.Snapshot().TargetTTL; got != 0 {
		t.Errorf("after TimeExceeded only: TargetTTL = %d, want 0", got)
	}

	// Phase 2: destination responds at TTL 5 — TargetTTL := 5.
	s.ApplyProbeResult(ProbeResult{
		Target: s.Target(), TTL: 5, Ts: ts, RespIP: dst,
		RTT: 12 * time.Millisecond, Reply: ReplyEchoReply,
	}, 3)
	if got := s.Snapshot().TargetTTL; got != 5 {
		t.Errorf("after EchoReply at TTL 5: TargetTTL = %d, want 5", got)
	}

	// Phase 3: destination also responds at TTL 7 — TargetTTL stays 5.
	s.ApplyProbeResult(ProbeResult{
		Target: s.Target(), TTL: 7, Ts: ts, RespIP: dst,
		RTT: 12 * time.Millisecond, Reply: ReplyEchoReply,
	}, 3)
	if got := s.Snapshot().TargetTTL; got != 5 {
		t.Errorf("after EchoReply at higher TTL 7: TargetTTL = %d, want 5 (unchanged)", got)
	}

	// Phase 4: destination responds at TTL 3 — TargetTTL drops to 3.
	s.ApplyProbeResult(ProbeResult{
		Target: s.Target(), TTL: 3, Ts: ts, RespIP: dst,
		RTT: 11 * time.Millisecond, Reply: ReplyEchoReply,
	}, 3)
	if got := s.Snapshot().TargetTTL; got != 3 {
		t.Errorf("after EchoReply at lower TTL 3: TargetTTL = %d, want 3 (route shortened)", got)
	}
}

// TestTargetTTL_ClearedWhenPathGrows: if the current target TTL later
// returns TimeExceeded (a transit router responded where the
// destination used to), the route has grown longer and we need to
// re-discover the new target TTL. The previous TargetTTL must clear.
// Timeouts at the target TTL, by contrast, do NOT clear it — those are
// common transient loss and clearing would cause unhelpful re-sweeping.
func TestTargetTTL_ClearedWhenPathGrows(t *testing.T) {
	s := newTestState(t, "8.8.8.8")
	ts := time.Now()
	dst := mustIP(t, "8.8.8.8")
	router := mustIP(t, "203.0.113.99")

	s.ApplyProbeResult(ProbeResult{
		Target: s.Target(), TTL: 5, Ts: ts, RespIP: dst,
		RTT: 10 * time.Millisecond, Reply: ReplyEchoReply,
	}, 3)
	if got := s.Snapshot().TargetTTL; got != 5 {
		t.Fatalf("setup: TargetTTL = %d, want 5", got)
	}

	// Timeout at the target TTL — must NOT clear. Transient loss is
	// common; clearing on a single timeout would cause sweeps to
	// re-expand and probe traffic to flap.
	s.ApplyProbeResult(ProbeResult{
		Target: s.Target(), TTL: 5, Ts: ts, TimedOut: true,
	}, 3)
	if got := s.Snapshot().TargetTTL; got != 5 {
		t.Errorf("after timeout at target: TargetTTL = %d, want 5 (timeouts must not clear)", got)
	}

	// TimeExceeded at the target TTL — must clear. Active evidence
	// that a transit router (not the destination) now responds at
	// this TTL means the route has actually grown.
	s.ApplyProbeResult(ProbeResult{
		Target: s.Target(), TTL: 5, Ts: ts, RespIP: router,
		RTT: 8 * time.Millisecond, Reply: ReplyTimeExceeded,
	}, 3)
	if got := s.Snapshot().TargetTTL; got != 0 {
		t.Errorf("after TimeExceeded at target: TargetTTL = %d, want 0 (path grew, target moved)", got)
	}
}

// TestSnapshot_CapsHopsAtTargetTTL: once the target TTL is known, the
// snapshot returns only TTLs 1..TargetTTL even if higher TTLs have
// also responded (which they do — the destination responds at every
// TTL ≥ path length). This is what prevents 20 redundant hop-list
// rows of "8.8.8.8" once the real target is found at TTL 11.
func TestSnapshot_CapsHopsAtTargetTTL(t *testing.T) {
	s := newTestState(t, "8.8.8.8")
	ts := time.Now()
	router := mustIP(t, "203.0.113.1")
	dst := mustIP(t, "8.8.8.8")

	// Populate TTLs 1..3 with transit routers.
	for ttl := uint8(1); ttl <= 3; ttl++ {
		s.ApplyProbeResult(ProbeResult{
			Target: s.Target(), TTL: ttl, Ts: ts, RespIP: router,
			RTT: time.Duration(ttl) * time.Millisecond, Reply: ReplyTimeExceeded,
		}, 3)
	}
	// Destination at TTL 4..7 — TargetTTL becomes 4.
	for ttl := uint8(4); ttl <= 7; ttl++ {
		s.ApplyProbeResult(ProbeResult{
			Target: s.Target(), TTL: ttl, Ts: ts, RespIP: dst,
			RTT: 12 * time.Millisecond, Reply: ReplyEchoReply,
		}, 3)
	}

	snap := s.Snapshot()
	if snap.TargetTTL != 4 {
		t.Fatalf("TargetTTL = %d, want 4", snap.TargetTTL)
	}
	if len(snap.Hops) != 4 {
		t.Errorf("Hops length = %d, want 4 (capped at TargetTTL)", len(snap.Hops))
	}
	// All visible hops should be TTL ≤ 4.
	for _, h := range snap.Hops {
		if h.TTL > 4 {
			t.Errorf("snapshot includes TTL %d, want capped at 4", h.TTL)
		}
	}
}
