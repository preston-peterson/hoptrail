// Package logring is a bounded in-memory ring of recent log records,
// teed off the daemon's slog pipeline so the web UI can show logs
// without SSH (step-128). journald remains the durable source of
// truth — the ring holds the last N records, loses everything on
// restart, and that's fine: the UI viewer is for "what's happening
// right now," not forensics.
//
// The tee handler respects the same level as the stderr handler (the
// settings panel's live log-level control raises/lowers both), so
// the viewer always mirrors what the journal shows.
package logring

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// Entry is one captured record. Seq is a monotonic id — clients poll
// with their last-seen seq and get only what's new.
type Entry struct {
	Seq   int64  `json:"seq"`
	Ts    int64  `json:"ts"` // unix ms
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Attrs string `json:"attrs,omitempty"` // "key=val key=val" — preformatted
}

// Ring is the fixed-capacity buffer. Safe for concurrent use.
type Ring struct {
	mu   sync.Mutex
	buf  []Entry // circular; len == cap once full
	cap  int
	next int64 // next seq to assign; buf head derivable from it
}

// New returns a Ring holding at most capacity entries.
func New(capacity int) *Ring {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring{buf: make([]Entry, 0, capacity), cap: capacity}
}

// Append stores one entry, assigning its Seq. Oldest entry is
// overwritten once the ring is full.
func (r *Ring) Append(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.Seq = r.next
	if len(r.buf) < r.cap {
		r.buf = append(r.buf, e)
	} else {
		r.buf[int(e.Seq)%r.cap] = e
	}
	r.next++
}

// Since returns up to limit entries with Seq > afterSeq, oldest
// first, plus the latest assigned seq (-1 when nothing has ever been
// logged). afterSeq -1 returns from the oldest retained entry.
func (r *Ring) Since(afterSeq int64, limit int) (entries []Entry, latest int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	latest = r.next - 1
	if limit < 1 || len(r.buf) == 0 {
		return []Entry{}, latest
	}
	oldest := r.next - int64(len(r.buf))
	start := afterSeq + 1
	if start < oldest {
		start = oldest
	}
	n := r.next - start
	if n <= 0 {
		return []Entry{}, latest
	}
	if n > int64(limit) {
		// Tail wins: the freshest records matter most in a viewer.
		start = r.next - int64(limit)
	}
	out := make([]Entry, 0, r.next-start)
	for s := start; s < r.next; s++ {
		out = append(out, r.buf[int(s)%r.cap])
	}
	return out, latest
}

// Handler tees slog records into a Ring and forwards them to the
// real output handler. Enabled defers to the inner handler, so the
// live LevelVar governs both surfaces identically.
type Handler struct {
	inner slog.Handler
	ring  *Ring
	// preformatted attrs from WithAttrs/WithGroup chains
	prefix string
}

// NewHandler wraps inner with capture into ring.
func NewHandler(inner slog.Handler, ring *Ring) *Handler {
	return &Handler{inner: inner, ring: ring}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(h.prefix)
	rec.Attrs(func(a slog.Attr) bool {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%v", a.Key, a.Value)
		return true
	})
	h.ring.Append(Entry{
		Ts:    rec.Time.UnixMilli(),
		Level: strings.ToLower(rec.Level.String()),
		Msg:   rec.Message,
		Attrs: b.String(),
	})
	return h.inner.Handle(ctx, rec)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.prefix)
	for _, a := range attrs {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%v", a.Key, a.Value)
	}
	return &Handler{inner: h.inner.WithAttrs(attrs), ring: h.ring, prefix: b.String()}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	// Group nesting is flattened for display purposes — the ring's
	// attrs string is a viewer convenience, not a parseable format.
	return &Handler{inner: h.inner.WithGroup(name), ring: h.ring, prefix: h.prefix}
}
