// Package analysis holds hoptrail's interpretation logic: the rules that
// turn raw probe observations into meaning. It is deliberately separate
// from the probe engine so these rules can be unit-tested with synthetic
// input rather than real ICMP traffic — and because they are the parts of
// the system most likely to evolve.
//
// Nothing in this package performs I/O or touches the network. Everything
// here is a pure function or a plain data structure.
package analysis

// RingBuffer is a fixed-capacity buffer that retains the most recent N
// values pushed into it. When full, the oldest value is evicted to make
// room. It is not safe for concurrent use; in hoptrail every RingBuffer
// is owned by a single goroutine (the reducer).
//
// It is used for per-hop observation history: recent IP-at-TTL sightings
// (for route-change detection), recent RTTs, and recent hit/miss results
// (for loss percentage).
type RingBuffer[T any] struct {
	buf  []T
	head int // index where the next Push writes
	size int // number of valid elements currently stored
}

// NewRingBuffer returns a RingBuffer that retains the most recent
// capacity values. capacity must be >= 1; a smaller value is clamped to 1
// rather than panicking, since a degenerate buffer is recoverable but a
// crash in the reducer is not.
func NewRingBuffer[T any](capacity int) *RingBuffer[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &RingBuffer[T]{buf: make([]T, capacity)}
}

// Push appends v, evicting the oldest value if the buffer is full.
func (r *RingBuffer[T]) Push(v T) {
	r.buf[r.head] = v
	r.head = (r.head + 1) % len(r.buf)
	if r.size < len(r.buf) {
		r.size++
	}
}

// Len returns the number of values currently stored (0..Cap).
func (r *RingBuffer[T]) Len() int { return r.size }

// Cap returns the maximum number of values the buffer retains.
func (r *RingBuffer[T]) Cap() int { return len(r.buf) }

// Newest returns the most recently pushed value. ok is false if the
// buffer is empty.
func (r *RingBuffer[T]) Newest() (value T, ok bool) {
	if r.size == 0 {
		var zero T
		return zero, false
	}
	// head points one past the newest write; step back one (mod cap).
	idx := (r.head - 1 + len(r.buf)) % len(r.buf)
	return r.buf[idx], true
}

// Oldest returns the least recently pushed value still retained. ok is
// false if the buffer is empty.
func (r *RingBuffer[T]) Oldest() (value T, ok bool) {
	if r.size == 0 {
		var zero T
		return zero, false
	}
	// When not full, the oldest is at index 0. When full, head points at
	// the oldest (it is about to be overwritten next).
	idx := 0
	if r.size == len(r.buf) {
		idx = r.head
	}
	return r.buf[idx], true
}

// NewestFirst returns a snapshot of all retained values ordered from most
// recent to least recent. The returned slice is a copy; mutating it does
// not affect the buffer. An empty buffer yields a nil slice.
//
// This is the primitive route-change detection uses: it walks
// observations newest-first, counting a consecutive run.
func (r *RingBuffer[T]) NewestFirst() []T {
	if r.size == 0 {
		return nil
	}
	out := make([]T, 0, r.size)
	// Start at the newest and walk backward r.size steps.
	idx := (r.head - 1 + len(r.buf)) % len(r.buf)
	for i := 0; i < r.size; i++ {
		out = append(out, r.buf[idx])
		idx = (idx - 1 + len(r.buf)) % len(r.buf)
	}
	return out
}

// OldestFirst returns a snapshot of all retained values in chronological
// order (least recent first). The returned slice is a copy. An empty
// buffer yields a nil slice.
func (r *RingBuffer[T]) OldestFirst() []T {
	nf := r.NewestFirst()
	// Reverse in place.
	for i, j := 0, len(nf)-1; i < j; i, j = i+1, j-1 {
		nf[i], nf[j] = nf[j], nf[i]
	}
	return nf
}
