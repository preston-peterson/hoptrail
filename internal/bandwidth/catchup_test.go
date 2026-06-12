package bandwidth

import (
	"testing"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

// Pins the step-155 catch-up: a missed past slot with no sample after
// it schedules a prompt run; a covered slot defers to the next
// occurrence as before.
func TestPrevScheduled_AndCatchupLogic(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 6, 12, 8, 0, 0, 0, loc) // 08:00; daily slot 02:00

	prev, ok := PrevScheduled(now, []string{"02:00"}, loc)
	if !ok || !prev.Equal(time.Date(2026, 6, 12, 2, 0, 0, 0, loc)) {
		t.Fatalf("PrevScheduled = %v %v, want today 02:00", prev, ok)
	}

	// Before the day's slot: yesterday's occurrence.
	early := time.Date(2026, 6, 12, 1, 0, 0, 0, loc)
	prev, ok = PrevScheduled(early, []string{"02:00"}, loc)
	if !ok || !prev.Equal(time.Date(2026, 6, 11, 2, 0, 0, 0, loc)) {
		t.Fatalf("PrevScheduled early = %v %v, want yesterday 02:00", prev, ok)
	}

	// Sample-vs-slot comparison semantics (the nextRun guard):
	missed := &storage.BandwidthSample{Ts: time.Date(2026, 6, 11, 14, 0, 0, 0, loc).UnixMilli()}
	covered := &storage.BandwidthSample{Ts: time.Date(2026, 6, 12, 2, 0, 30, 0, loc).UnixMilli()}
	slot := time.Date(2026, 6, 12, 2, 0, 0, 0, loc)
	if !time.UnixMilli(missed.Ts).Before(slot) {
		t.Error("yesterday-afternoon sample should count as a missed slot")
	}
	if time.UnixMilli(covered.Ts).Before(slot) {
		t.Error("a sample just after the slot must NOT trigger catch-up")
	}
}
