// Package probe is hoptrail's ICMP probe engine: the ICMP primitive, the
// reducer that owns the in-memory PathState, and the loops that feed it
// observations. The reducer is the central authority — no other
// goroutine reads or writes PathState directly; everything goes through
// channels.
//
// Package layout:
//
//	events.go   — types that flow on the channels (this file)
//	icmp.go     — Prober: raw ICMP socket, concurrent-safe probe API
//	path.go     — PathState and Hop: the central in-memory data structure
//	engine.go   — the reducer goroutine + lifecycle
//	discovery.go (step-5) — path-discovery sweep loop
//	pinger.go   (step-5) — per-hop pinger loop
//
// Storage is intentionally pulled out of this package via the Sink
// interface, so the probe engine is testable without dragging SQLite
// into unit tests and so Phase 3 can substitute a different sink for
// distributed-mode aggregation without touching the engine.
package probe

import (
	"net/netip"
	"time"
)

// ProbeResult is a single per-hop observation produced by a probe loop
// (per-hop pinger, or one TTL slot of a discovery sweep). Always carries
// a timestamp and target; the response fields are zero on timeout.
type ProbeResult struct {
	// Target the probe was directed at.
	Target netip.Addr

	// TTL the probe was sent with — i.e. the hop position being probed.
	TTL uint8

	// Ts is when the probe was sent (not when the response arrived).
	// Display timelines use Ts so the latency timeline's x-axis aligns
	// with probe cadence rather than response jitter.
	Ts time.Time

	// RespIP is the router (TimeExceeded) or destination (EchoReply)
	// that responded. Zero (invalid) if TimedOut.
	RespIP netip.Addr

	// RTT measured from probe send to response receipt. Zero on timeout.
	RTT time.Duration

	// Reply classifies the response (EchoReply / TimeExceeded /
	// DestUnreachable). Zero value on timeout.
	Reply ReplyType

	// TimedOut is set when no response arrived within the configured
	// window. RespIP/RTT/Reply are zero when this is true.
	TimedOut bool
}

// SweepResult is the assembled output of one full path-discovery sweep:
// a ProbeResult for each TTL from 1 to the sweep's maxTTL. The reducer
// processes a sweep atomically because route-change detection requires
// examining the full path at once — a flicker at one TTL needs context
// from neighboring TTLs to interpret.
type SweepResult struct {
	Target netip.Addr
	Ts     time.Time

	// Results is indexed by TTL slot (TTL-1 → index 0). Length equals
	// the sweep's maxTTL. Entries past the destination are typically
	// zero-value ProbeResults with TimedOut=false and Reply unset, which
	// the reducer interprets as "no probe sent past terminal."
	Results []ProbeResult

	// TerminalTTL is the smallest TTL at which the target itself
	// responded (EchoReply). Zero if no TTL reached the destination.
	TerminalTTL uint8
}

// Sample is the per-hop time-series row written to storage. One Sample
// per probe per hop — that's the source-of-truth, raw observation. The
// loss-attribution rule (Section 5 in the design doc) is applied at
// read time, not when Samples are written.
type Sample struct {
	// Target is the resolved IPv4 the pinger is hitting — engine-
	// internal identity, used for the prober.
	Target netip.Addr

	// TargetID is the operator-typed identifier ("8.8.8.8" or
	// "dns.google") that storage uses as the row's `target` column.
	// Decoupling this from the resolved IP is what makes
	// hostname-targets survive periodic re-resolution (step-34):
	// when cloudflare.com's IP rotates, Target changes but TargetID
	// stays "cloudflare.com", so historical samples remain reachable
	// under the same identifier.
	TargetID string

	TTL uint8
	Ts  time.Time

	// IP of the responding hop. Zero on timeout.
	IP netip.Addr

	// RTT of the response. Zero on timeout.
	RTT time.Duration
}

// RouteChange is the event emitted when a hop's modal IP stabilizes to
// something new. The detection logic (in internal/analysis) suppresses
// ECMP flapping by requiring N consecutive sightings before firing.
type RouteChange struct {
	Target   netip.Addr
	TargetID string // see Sample.TargetID

	TTL uint8
	Ts  time.Time

	// OldIP is what the hop displayed before the change. Zero if the
	// hop was previously anonymous (no identity).
	OldIP netip.Addr

	// NewIP is the IP the hop now displays. Always valid (the detector
	// never fires with an invalid candidate).
	NewIP netip.Addr
}

// Sink is the boundary between the probe engine and storage. internal/
// storage implements it; tests can implement a synthetic version.
//
// Both write methods return an error so storage failures surface; the
// reducer logs and continues rather than crashing — losing a sample is
// preferable to losing the daemon. The methods are called from the
// reducer goroutine only, so implementations need not be
// goroutine-safe.
type Sink interface {
	WriteSample(s Sample) error
	WriteRouteChange(rc RouteChange) error
}

// NoopSink discards everything. Useful in tests and for early
// integration before storage is wired up.
type NoopSink struct{}

func (NoopSink) WriteSample(Sample) error           { return nil }
func (NoopSink) WriteRouteChange(RouteChange) error { return nil }
