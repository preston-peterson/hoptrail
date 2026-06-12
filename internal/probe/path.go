package probe

import (
	"fmt"
	"net/netip"
	"time"

	"github.com/preston-peterson/hoptrail/internal/analysis"
)

// maxTTL is the hard upper bound on TTL for PathState's array sizing.
// Matches config.maxHopsCeiling. Beyond this is impractical (a path
// of 64 hops is already chasing a routing loop more often than not).
const maxTTL = 64

// Default ring buffer capacities for per-hop history. The candidates
// buffer is sized to 2× the typical route-change threshold; the
// samples buffers are sized for ~5 minutes at 1-second cadence, which
// is the window the UI's per-hop metric cards summarize.
const (
	defaultCandidatesCap = 10  // 2 × default RouteChangeThreshold of 5
	defaultSamplesCap    = 300 // 5 min × 60 s
)

// PathState is the in-memory snapshot of one path: per-TTL hop entries,
// each tracking recent observations for route-change detection and the
// short-window metrics shown in the UI.
//
// PathState is owned by a single goroutine (the reducer). No methods
// here take locks. All reads and writes from other goroutines must go
// through the reducer's channel API (see engine.go).
type PathState struct {
	target   netip.Addr
	targetID string // operator-typed identifier; stamped into emitted Samples/RouteChanges
	started  time.Time

	// hops is indexed by TTL (1..maxTTL). Index 0 is unused (TTL 0 is
	// meaningless). Pointers because most TTLs won't have responded
	// yet; absent positions are nil.
	hops [maxTTL + 1]*Hop

	// maxSeen is the largest TTL that has produced at least one
	// response. Useful for sizing snapshots and for the API's "hop
	// count" summary.
	maxSeen uint8

	// targetTTL is the smallest TTL at which the destination itself
	// has responded with EchoReply. Once known, discovery sweeps and
	// the pinger restrict themselves to TTLs 1..targetTTL — probing
	// higher serves no purpose since the destination responds at every
	// TTL >= path length. Zero means "no TTL has reached the
	// destination yet." Cleared if the current target TTL later
	// returns TimeExceeded (path has grown).
	targetTTL uint8

	// Ring buffer capacities — captured at construction so all hops in
	// this PathState share the same sizing.
	candidatesCap int
	samplesCap    int
}

// Hop is the per-TTL observation history.
type Hop struct {
	// TTL position. Constant after Hop creation; matches the slot in
	// PathState.hops where this Hop lives.
	TTL uint8

	// CurrentIP is the hop's currently-displayed identity — the modal
	// of recent candidates, updated only when route-change detection
	// fires (or when the hop first acquires an identity from anonymity).
	// Zero value means the hop has been anonymous on every recent
	// observation.
	CurrentIP netip.Addr

	// candidates is recent IP-at-TTL observations, used by
	// DetectRouteChange. The buffer is read newest-first.
	candidates *analysis.RingBuffer[netip.Addr]

	// recentRTTs and recentLosses are the short-window history the UI
	// summarizes for the per-hop row (avg latency, current, loss %).
	// recentLosses entries: true = probe timed out, false = responded.
	recentRTTs   *analysis.RingBuffer[time.Duration]
	recentLosses *analysis.RingBuffer[bool]

	// LastResponse is the timestamp of the most recent successful
	// probe of this hop. Used to drive "hop has gone quiet" UI cues.
	LastResponse time.Time
}

// NewPathState creates a fresh PathState for one target with default
// buffer capacities. targetID is the operator-typed identifier (an
// IP string or a hostname); it's stamped into emitted Samples and
// RouteChanges so storage rows can be queried by the stable typed
// identifier rather than by the (potentially-re-resolved) IP.
func NewPathState(target netip.Addr, targetID string) *PathState {
	return newPathStateWithCaps(target, targetID, defaultCandidatesCap, defaultSamplesCap)
}

// newPathStateWithCaps is the test-friendly constructor that lets tests
// use tiny buffers so they don't have to fabricate hundreds of probes.
func newPathStateWithCaps(target netip.Addr, targetID string, candidatesCap, samplesCap int) *PathState {
	return &PathState{
		target:        target,
		targetID:      targetID,
		started:       time.Now(),
		candidatesCap: candidatesCap,
		samplesCap:    samplesCap,
	}
}

// Target returns the target address this PathState is tracking.
func (s *PathState) Target() netip.Addr { return s.target }

// MaxSeen returns the largest TTL that has produced at least one
// non-anonymous response.
func (s *PathState) MaxSeen() uint8 { return s.maxSeen }

// ApplyProbeResult updates the per-hop state for one observation and
// returns (Sample, *RouteChange). The Sample is always non-nil; the
// RouteChange is non-nil only when route-change detection fired.
//
// Called by the reducer goroutine — must not be called concurrently
// for the same PathState.
//
// Panics if r.TTL is 0 or > maxTTL — those are caller bugs, not data
// to be tolerated. The reducer validates incoming events.
func (s *PathState) ApplyProbeResult(r ProbeResult, threshold int) (Sample, *RouteChange) {
	if r.TTL == 0 || r.TTL > maxTTL {
		panic(fmt.Sprintf("ApplyProbeResult: TTL %d out of range (1..%d)", r.TTL, maxTTL))
	}

	hop := s.getOrCreateHop(r.TTL)

	// Three reply types map to two semantics for hop-identity tracking:
	//   - TimeExceeded: a real intermediate router decremented the TTL
	//     to 0 here and reports its source IP. This IS the hop at this
	//     TTL — push RespIP.
	//   - EchoReply: the destination itself answered at or before the
	//     target TTL. This IS the hop at this TTL — push RespIP.
	//   - DestUnreachable: something signals the packet couldn't be
	//     delivered. The source of that ICMP error is NOT the hop at
	//     the probed TTL — it's whoever gave up on delivery, often the
	//     local kernel when egress fails (NIC down, gateway lost, route
	//     missing). The step-64 bug attributed the local box's IP to
	//     every TTL during a brief network outage: the reducer saw the
	//     local LAN address "responding" at every TTL for the duration
	//     of the outage, the route-change detector flipped every hop's
	//     identity to it, then flipped back when egress recovered.
	//     Treat DestUnreachable the same as a timeout: anonymous
	//     candidate, loss=true, no LastResponse update.
	isMiss := r.TimedOut || r.Reply == ReplyDestUnreachable
	if isMiss {
		hop.candidates.Push(netip.Addr{}) // anonymous
		hop.recentRTTs.Push(0)
		hop.recentLosses.Push(true)
	} else {
		hop.candidates.Push(r.RespIP)
		hop.recentRTTs.Push(r.RTT)
		hop.recentLosses.Push(false)
		hop.LastResponse = r.Ts
		if r.TTL > s.maxSeen {
			s.maxSeen = r.TTL
		}
	}

	// Maintain targetTTL — smallest TTL at which the destination
	// responds with EchoReply. EchoReply at a TTL ≤ current target
	// lowers it; TimeExceeded at the current target TTL clears it
	// (route has grown longer, target moved). Timeouts at the target
	// don't change targetTTL — transient loss is common and clearing
	// would cause unhelpful re-sweeping above. Only TimeExceeded
	// (active evidence that a transit router is now at this TTL) is
	// strong enough to invalidate the target.
	switch r.Reply {
	case ReplyEchoReply:
		if s.targetTTL == 0 || r.TTL < s.targetTTL {
			s.targetTTL = r.TTL
		}
	case ReplyTimeExceeded:
		if s.targetTTL != 0 && r.TTL == s.targetTTL {
			s.targetTTL = 0
		}
	}

	// Run route-change detection on the freshly-updated buffer.
	obs := hop.candidates.NewestFirst()
	changed, newIP := analysis.DetectRouteChange(obs, hop.CurrentIP, threshold)

	var rc *RouteChange
	switch {
	case changed:
		// Route-change detector fired — record old/new and update.
		rc = &RouteChange{
			Target:   s.target,
			TargetID: s.targetID,
			TTL:      r.TTL,
			Ts:       r.Ts,
			OldIP:    hop.CurrentIP,
			NewIP:    newIP,
		}
		hop.CurrentIP = newIP

	case !hop.CurrentIP.IsValid():
		// Hop has no identity yet but the detector didn't fire (not
		// enough consecutive observations to cross the threshold). If
		// there's a modal IP at all, show it as a tentative identity
		// so the UI isn't blank — the route-change detector will
		// confirm or replace it on subsequent observations.
		if modal, ok := analysis.ModalIP(obs); ok {
			hop.CurrentIP = modal
		}
	}

	sample := Sample{
		Target:   s.target,
		TargetID: s.targetID,
		TTL:      r.TTL,
		Ts:       r.Ts,
	}
	if !isMiss {
		sample.IP = r.RespIP
		sample.RTT = r.RTT
	}
	return sample, rc
}

// getOrCreateHop returns the Hop at ttl, creating it if absent.
func (s *PathState) getOrCreateHop(ttl uint8) *Hop {
	if s.hops[ttl] == nil {
		s.hops[ttl] = &Hop{
			TTL:          ttl,
			candidates:   analysis.NewRingBuffer[netip.Addr](s.candidatesCap),
			recentRTTs:   analysis.NewRingBuffer[time.Duration](s.samplesCap),
			recentLosses: analysis.NewRingBuffer[bool](s.samplesCap),
		}
	}
	return s.hops[ttl]
}

// HopSnapshot is a read-only view of one hop suitable for the API.
// All fields are values (no pointers, no shared buffers), so callers
// can hold a HopSnapshot indefinitely without affecting PathState.
type HopSnapshot struct {
	TTL          uint8
	CurrentIP    netip.Addr
	LastResponse time.Time

	// AvgRTT is the mean of the recent-RTTs buffer over responses
	// (timeouts excluded). Zero if no responses are in the window.
	AvgRTT time.Duration

	// CurrentRTT is the most recent non-timeout RTT in the window.
	// Zero if every recent observation was a timeout.
	CurrentRTT time.Duration

	// MinRTT is the smallest non-timeout RTT in the recent window —
	// the baseline against which avg/current can be read as "this hop
	// is doing about as well as it usually does" or "this hop is
	// noticeably slower right now." Zero if every recent observation
	// was a timeout.
	MinRTT time.Duration

	// LossPercent is the percentage of timeouts over the recent-losses
	// window (0..100).
	LossPercent float64
}

// Snapshot returns a point-in-time view of every responded TTL in
// ascending order. Anonymous-only hops (those with no recent
// non-timeout observations) are included so the UI can render them as
// `* * *` rows; their CurrentIP is zero.
//
// Cheap to call — copies a small number of summary fields per hop, no
// shared state.
// Snapshot is the externally-visible read-state of a PathState. Hops
// is ordered by ascending TTL; TargetTTL is the lowest TTL at which
// the destination has responded with EchoReply, or 0 if unknown.
type Snapshot struct {
	Hops      []HopSnapshot
	TargetTTL uint8
}

// Snapshot returns a point-in-time copy of the externally-visible
// state. Called by the reducer in response to query requests.
//
// When TargetTTL is known, Hops contains only TTLs 1..TargetTTL —
// higher TTLs have entries (the destination responds at every TTL
// past the path length) but they aren't useful information and
// returning them just creates redundant rows in the UI. When
// TargetTTL is unknown (no EchoReply yet), Hops contains TTLs
// 1..maxSeen.
func (s *PathState) Snapshot() Snapshot {
	out := Snapshot{TargetTTL: s.targetTTL}
	if s.maxSeen == 0 {
		return out
	}
	maxVisible := s.maxSeen
	if s.targetTTL != 0 && s.targetTTL < maxVisible {
		maxVisible = s.targetTTL
	}
	out.Hops = make([]HopSnapshot, 0, maxVisible)
	for ttl := uint8(1); ttl <= maxVisible; ttl++ {
		hop := s.hops[ttl]
		if hop == nil {
			// TTL never observed — leave as a gap. The UI can decide
			// whether to render placeholder rows.
			continue
		}
		out.Hops = append(out.Hops, hop.snapshot())
	}
	return out
}

// snapshot computes the summary fields for one hop.
func (h *Hop) snapshot() HopSnapshot {
	hs := HopSnapshot{
		TTL:          h.TTL,
		CurrentIP:    h.CurrentIP,
		LastResponse: h.LastResponse,
	}

	// Average/min/current RTT over non-zero entries (zero = timeout).
	rtts := h.recentRTTs.OldestFirst()
	var sum time.Duration
	var count int
	var last time.Duration
	var min time.Duration
	for _, r := range rtts {
		if r > 0 {
			sum += r
			count++
			last = r
			if min == 0 || r < min {
				min = r
			}
		}
	}
	if count > 0 {
		hs.AvgRTT = sum / time.Duration(count)
		hs.CurrentRTT = last
		hs.MinRTT = min
	}

	// Loss percent over the window.
	losses := h.recentLosses.OldestFirst()
	if len(losses) > 0 {
		var lost int
		for _, l := range losses {
			if l {
				lost++
			}
		}
		hs.LossPercent = 100.0 * float64(lost) / float64(len(losses))
	}

	return hs
}
