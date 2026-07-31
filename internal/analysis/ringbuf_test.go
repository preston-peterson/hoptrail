package analysis

import (
	"reflect"
	"testing"
)

func TestRingBuffer_EmptyState(t *testing.T) {
	r := NewRingBuffer[int](3)

	if r.Len() != 0 {
		t.Errorf("Len() on empty buffer = %d, want 0", r.Len())
	}
	if r.Cap() != 3 {
		t.Errorf("Cap() = %d, want 3", r.Cap())
	}
	if _, ok := r.Newest(); ok {
		t.Error("Newest() on empty buffer returned ok=true, want false")
	}
	if _, ok := r.Oldest(); ok {
		t.Error("Oldest() on empty buffer returned ok=true, want false")
	}
	if got := r.NewestFirst(); got != nil {
		t.Errorf("NewestFirst() on empty buffer = %v, want nil", got)
	}
}

func TestRingBuffer_CapacityClamped(t *testing.T) {
	// A capacity below 1 must clamp to 1 rather than panic — a degenerate
	// buffer is recoverable, a crash in the reducer is not.
	for _, capacity := range []int{0, -1, -100} {
		r := NewRingBuffer[int](capacity)
		if r.Cap() != 1 {
			t.Errorf("NewRingBuffer(%d).Cap() = %d, want 1", capacity, r.Cap())
		}
	}
}

func TestRingBuffer_PartialFill(t *testing.T) {
	r := NewRingBuffer[int](5)
	r.Push(10)
	r.Push(20)
	r.Push(30)

	if r.Len() != 3 {
		t.Errorf("Len() = %d, want 3", r.Len())
	}
	if v, ok := r.Newest(); !ok || v != 30 {
		t.Errorf("Newest() = %d, %v; want 30, true", v, ok)
	}
	if v, ok := r.Oldest(); !ok || v != 10 {
		t.Errorf("Oldest() = %d, %v; want 10, true", v, ok)
	}
	if got, want := r.NewestFirst(), []int{30, 20, 10}; !reflect.DeepEqual(got, want) {
		t.Errorf("NewestFirst() = %v, want %v", got, want)
	}
	if got, want := r.OldestFirst(), []int{10, 20, 30}; !reflect.DeepEqual(got, want) {
		t.Errorf("OldestFirst() = %v, want %v", got, want)
	}
}

func TestRingBuffer_WrapAround(t *testing.T) {
	// Push more than capacity; the oldest values must be evicted.
	r := NewRingBuffer[int](3)
	for _, v := range []int{1, 2, 3, 4, 5} {
		r.Push(v)
	}

	if r.Len() != 3 {
		t.Errorf("Len() after overflow = %d, want 3 (capacity)", r.Len())
	}
	if v, ok := r.Newest(); !ok || v != 5 {
		t.Errorf("Newest() = %d, %v; want 5, true", v, ok)
	}
	if v, ok := r.Oldest(); !ok || v != 3 {
		t.Errorf("Oldest() = %d, %v; want 3, true (1 and 2 evicted)", v, ok)
	}
	if got, want := r.NewestFirst(), []int{5, 4, 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("NewestFirst() after wrap = %v, want %v", got, want)
	}
	if got, want := r.OldestFirst(), []int{3, 4, 5}; !reflect.DeepEqual(got, want) {
		t.Errorf("OldestFirst() after wrap = %v, want %v", got, want)
	}
}

func TestRingBuffer_ExactlyFull(t *testing.T) {
	// The size == capacity boundary is where Oldest() switches which
	// index it reads; exercise it directly.
	r := NewRingBuffer[string](2)
	r.Push("a")
	r.Push("b")

	if v, ok := r.Oldest(); !ok || v != "a" {
		t.Errorf("Oldest() at exactly-full = %q, %v; want \"a\", true", v, ok)
	}
	r.Push("c") // evicts "a"
	if v, ok := r.Oldest(); !ok || v != "b" {
		t.Errorf("Oldest() after one eviction = %q, %v; want \"b\", true", v, ok)
	}
}

func TestRingBuffer_SnapshotIsCopy(t *testing.T) {
	// Mutating a returned snapshot must not affect the buffer.
	r := NewRingBuffer[int](3)
	r.Push(1)
	r.Push(2)

	snap := r.NewestFirst()
	snap[0] = 999

	if v, _ := r.Newest(); v != 2 {
		t.Errorf("buffer mutated through snapshot: Newest() = %d, want 2", v)
	}
}

func TestRingBuffer_CapacityOne(t *testing.T) {
	// The clamped-degenerate case must still behave like a ring buffer.
	r := NewRingBuffer[int](1)
	r.Push(7)
	r.Push(8)
	r.Push(9)

	if r.Len() != 1 {
		t.Errorf("Len() = %d, want 1", r.Len())
	}
	if v, _ := r.Newest(); v != 9 {
		t.Errorf("Newest() = %d, want 9", v)
	}
	if v, _ := r.Oldest(); v != 9 {
		t.Errorf("Oldest() = %d, want 9 (same as newest at capacity 1)", v)
	}
}
