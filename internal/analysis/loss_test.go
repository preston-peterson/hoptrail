package analysis

import "testing"

// snapshot is a brief constructor for HopLossSnapshot to keep the test
// tables readable.
func snapshot(ttl uint8, loss float64) HopLossSnapshot {
	return HopLossSnapshot{TTL: ttl, Loss: loss}
}

func TestAttributedLoss_NoLossAnywhere(t *testing.T) {
	window := []HopLossSnapshot{
		snapshot(1, 0), snapshot(2, 0), snapshot(3, 0),
	}
	got := AttributedLoss(window, DefaultLossTolerance)

	for _, hop := range got {
		if hop.Suspect {
			t.Errorf("TTL %d: suspect=true with zero loss; want false", hop.TTL)
		}
	}
}

func TestAttributedLoss_RateLimitingHopNotSuspect(t *testing.T) {
	// Hop 3 shows 30% loss, but every hop after it shows ~0%. The dropped
	// "packets" clearly still got through — hop 3 is just declining to
	// answer pings. It must NOT be flagged suspect.
	window := []HopLossSnapshot{
		snapshot(1, 0),
		snapshot(2, 0),
		snapshot(3, 30),
		snapshot(4, 0),
		snapshot(5, 1),
	}
	got := AttributedLoss(window, DefaultLossTolerance)

	if got[2].Suspect {
		t.Error("TTL 3: suspect=true, but loss did not persist downstream; want false (rate-limiting)")
	}
	// And the raw number must be preserved regardless.
	if got[2].Loss != 30 {
		t.Errorf("TTL 3: Loss = %v, want 30 (raw value must be unchanged)", got[2].Loss)
	}
}

func TestAttributedLoss_GenuineLossIsSuspect(t *testing.T) {
	// Hop 3 starts losing, and every hop after it loses at least as much.
	// That is genuine packet loss — hop 3 is the culprit and is suspect.
	// Downstream hops are also suspect (they too see persistent loss).
	window := []HopLossSnapshot{
		snapshot(1, 0),
		snapshot(2, 0),
		snapshot(3, 25),
		snapshot(4, 27),
		snapshot(5, 26),
	}
	got := AttributedLoss(window, DefaultLossTolerance)

	if !got[2].Suspect {
		t.Error("TTL 3: suspect=false, but loss persisted at every downstream hop; want true")
	}
	if !got[3].Suspect || !got[4].Suspect {
		t.Error("downstream hops with persistent loss should also be suspect")
	}
	// Hops 1 and 2 have no loss, so they are never suspect.
	if got[0].Suspect || got[1].Suspect {
		t.Error("zero-loss hops upstream of the loss should not be suspect")
	}
}

func TestAttributedLoss_TargetWithLossIsAlwaysSuspect(t *testing.T) {
	// The final hop has no downstream hops, so the "persists downstream"
	// condition is vacuously satisfied. Loss at the destination is real
	// by definition.
	window := []HopLossSnapshot{
		snapshot(1, 0),
		snapshot(2, 0),
		snapshot(3, 12),
	}
	got := AttributedLoss(window, DefaultLossTolerance)

	if !got[2].Suspect {
		t.Error("final hop with loss must be suspect; it has no downstream to disqualify it")
	}
}

func TestAttributedLoss_ToleranceAllowsAttenuation(t *testing.T) {
	// Hop 2 loses 20%. Hop 3 loses 16% — slightly less, but within the
	// default 5-point tolerance. The loss should still count as
	// persisting, so hop 2 stays suspect.
	window := []HopLossSnapshot{
		snapshot(1, 0),
		snapshot(2, 20),
		snapshot(3, 16),
	}
	got := AttributedLoss(window, DefaultLossTolerance)

	if !got[1].Suspect {
		t.Error("TTL 2: downstream loss within tolerance should still count as persisting; want suspect")
	}
}

func TestAttributedLoss_ToleranceBoundaryExcludes(t *testing.T) {
	// Hop 2 loses 20%. Hop 3 loses 14% — that is 6 points below, past the
	// 5-point tolerance. The loss did not persist; hop 2 is not suspect.
	window := []HopLossSnapshot{
		snapshot(1, 0),
		snapshot(2, 20),
		snapshot(3, 14),
	}
	got := AttributedLoss(window, DefaultLossTolerance)

	if got[1].Suspect {
		t.Error("TTL 2: downstream loss beyond tolerance should disqualify; want not suspect")
	}
}

func TestAttributedLoss_ZeroToleranceIsStrict(t *testing.T) {
	// With zero tolerance, any downstream dip at all disqualifies.
	window := []HopLossSnapshot{
		snapshot(1, 10),
		snapshot(2, 9.99),
	}
	got := AttributedLoss(window, 0)

	if got[0].Suspect {
		t.Error("TTL 1: with zero tolerance, a 0.01 downstream dip should disqualify")
	}
}

func TestAttributedLoss_PreservesOrderAndLength(t *testing.T) {
	window := []HopLossSnapshot{
		snapshot(1, 0), snapshot(2, 5), snapshot(3, 0), snapshot(4, 100),
	}
	got := AttributedLoss(window, DefaultLossTolerance)

	if len(got) != len(window) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(window))
	}
	for i := range window {
		if got[i].TTL != window[i].TTL {
			t.Errorf("index %d: TTL = %d, want %d (order must be preserved)", i, got[i].TTL, window[i].TTL)
		}
		if got[i].Loss != window[i].Loss {
			t.Errorf("index %d: Loss = %v, want %v (raw value must be preserved)", i, got[i].Loss, window[i].Loss)
		}
	}
}

func TestAttributedLoss_DoesNotMutateInput(t *testing.T) {
	window := []HopLossSnapshot{
		snapshot(1, 10), snapshot(2, 10),
	}
	_ = AttributedLoss(window, DefaultLossTolerance)

	if window[0].Loss != 10 || window[1].Loss != 10 {
		t.Error("AttributedLoss mutated its input window")
	}
}

func TestAttributedLoss_EmptyWindow(t *testing.T) {
	got := AttributedLoss(nil, DefaultLossTolerance)
	if len(got) != 0 {
		t.Errorf("AttributedLoss(nil) returned %d entries, want 0", len(got))
	}
}

func TestAttributedLoss_SingleHop(t *testing.T) {
	// A single hop is its own final hop: suspect iff it has any loss.
	withLoss := AttributedLoss([]HopLossSnapshot{snapshot(1, 5)}, DefaultLossTolerance)
	if !withLoss[0].Suspect {
		t.Error("single hop with loss should be suspect")
	}
	noLoss := AttributedLoss([]HopLossSnapshot{snapshot(1, 0)}, DefaultLossTolerance)
	if noLoss[0].Suspect {
		t.Error("single hop with no loss should not be suspect")
	}
}
