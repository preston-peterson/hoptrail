package analysis

// DefaultLossTolerance is the percentage-point slack applied when checking
// whether loss "persists" downstream. Real packet loss tends to attenuate
// slightly hop-to-hop because downstream routers may answer ICMP at
// different rates; a strict equality test produces false negatives.
const DefaultLossTolerance = 5.0

// HopLossSnapshot is the raw, measured loss for one hop over some window.
type HopLossSnapshot struct {
	// TTL is the hop's position in the path (1-based).
	TTL uint8

	// Loss is the measured packet-loss percentage for this hop over the
	// window, 0..100.
	Loss float64
}

// AttributedHopLoss is a hop's loss after the downstream-persistence rule
// has been applied to decide whether the loss is genuine.
type AttributedHopLoss struct {
	TTL uint8

	// Loss is the raw measured percentage, unchanged. hoptrail always
	// shows the real number; attribution only affects how it is framed.
	Loss float64

	// Suspect is true when this hop's loss is believed to be genuine
	// packet loss affecting traffic — that is, the loss persists at
	// every downstream hop. When false, the loss is most likely the hop
	// rate-limiting its own ICMP responses while still forwarding
	// traffic fine, and the UI should de-emphasize it accordingly.
	Suspect bool
}

// AttributedLoss applies the downstream-persistence rule to a path.
//
// window is the per-hop loss snapshots for one path, ordered by TTL
// ascending (hop 1 first, the target last). tolerance is the
// percentage-point slack for the "persists downstream" comparison; pass
// DefaultLossTolerance for the standard behavior.
//
// The rule, per hop i:
//
//	suspect(i) = loss(i) > 0
//	             AND for every downstream hop j > i:
//	                 loss(j) >= loss(i) - tolerance
//
// Intuition: if a hop is really dropping traffic, every hop past it sees
// at least comparable loss, because the dropped packets never reach them.
// If the loss vanishes one hop later, the "loss" was just that one hop
// declining to answer pings — its forwarding path is fine.
//
// The final hop (the target) has no downstream hops, so the universal
// condition is vacuously satisfied: the target is suspect whenever it
// shows any loss at all, which is correct — loss at the destination is
// real by definition.
//
// The returned slice has the same length and order as window. window is
// not mutated.
func AttributedLoss(window []HopLossSnapshot, tolerance float64) []AttributedHopLoss {
	out := make([]AttributedHopLoss, len(window))

	for i, hop := range window {
		suspect := hop.Loss > 0
		if suspect {
			// Check every downstream hop. If any downstream hop's loss
			// falls below this hop's loss minus tolerance, the loss did
			// not persist — so this hop is not the genuine culprit.
			for j := i + 1; j < len(window); j++ {
				if window[j].Loss < hop.Loss-tolerance {
					suspect = false
					break
				}
			}
		}
		out[i] = AttributedHopLoss{
			TTL:     hop.TTL,
			Loss:    hop.Loss,
			Suspect: suspect,
		}
	}

	return out
}
