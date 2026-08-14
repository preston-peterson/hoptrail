package logring

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
)

func TestRing_AppendSinceEviction(t *testing.T) {
	r := New(4)

	if entries, latest := r.Since(-1, 100); len(entries) != 0 || latest != -1 {
		t.Fatalf("empty ring: %v latest %d", entries, latest)
	}

	for i := 0; i < 6; i++ {
		r.Append(Entry{Msg: fmt.Sprintf("m%d", i)})
	}
	// Capacity 4, six appends: 0 and 1 evicted; seqs 2..5 retained.
	entries, latest := r.Since(-1, 100)
	if latest != 5 {
		t.Errorf("latest = %d, want 5", latest)
	}
	if len(entries) != 4 || entries[0].Msg != "m2" || entries[3].Msg != "m5" {
		t.Errorf("entries = %+v", entries)
	}
	for i, e := range entries {
		if e.Seq != int64(i+2) {
			t.Errorf("entries[%d].Seq = %d, want %d", i, e.Seq, i+2)
		}
	}

	// Incremental poll: only what's new after seq 3.
	entries, _ = r.Since(3, 100)
	if len(entries) != 2 || entries[0].Msg != "m4" {
		t.Errorf("since 3: %+v", entries)
	}
	// Caught up: nothing.
	if entries, _ = r.Since(5, 100); len(entries) != 0 {
		t.Errorf("caught up: %+v", entries)
	}
	// Limit keeps the TAIL (freshest), not the head.
	entries, _ = r.Since(-1, 2)
	if len(entries) != 2 || entries[0].Msg != "m4" || entries[1].Msg != "m5" {
		t.Errorf("limited: %+v", entries)
	}
}

func TestHandler_CapturesAndForwards(t *testing.T) {
	ring := New(16)
	levelVar := new(slog.LevelVar) // info default
	inner := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: levelVar})
	logger := slog.New(NewHandler(inner, ring))

	logger.Info("hello", "k", "v", "n", 7)
	logger.Debug("invisible") // below level: not captured either
	logger.With("probe_id", "site-east").Warn("spilled", "batches", 3)

	entries, latest := ring.Since(-1, 100)
	if latest != 1 || len(entries) != 2 {
		t.Fatalf("entries = %+v latest %d", entries, latest)
	}
	if entries[0].Level != "info" || entries[0].Msg != "hello" || entries[0].Attrs != "k=v n=7" {
		t.Errorf("entry 0 = %+v", entries[0])
	}
	if entries[1].Level != "warn" || entries[1].Attrs != "probe_id=site-east batches=3" {
		t.Errorf("entry 1 = %+v", entries[1])
	}

	// Raising the live level captures debug from then on — the ring
	// follows the same LevelVar as the output handler.
	levelVar.Set(slog.LevelDebug)
	logger.Debug("now visible")
	entries, _ = ring.Since(latest, 100)
	if len(entries) != 1 || entries[0].Level != "debug" {
		t.Errorf("after level change: %+v", entries)
	}
}
