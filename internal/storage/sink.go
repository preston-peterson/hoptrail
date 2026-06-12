package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/preston-peterson/hoptrail/internal/probe"
)

// Default flush thresholds. Tuned for the probe engine's steady-state
// rate (~30 samples/sec at default cadence with 30 hops): the ticker
// keeps wall-clock latency under a frame, the size threshold caps
// memory under burst, and bufferCap caps the worst case if writes
// fall behind.
const (
	defaultFlushInterval = 250 * time.Millisecond
	defaultFlushSize     = 100
	defaultBufferCap     = 10_000 // ~5 min at 30 samples/sec
)

// BatchedSink implements probe.Sink by buffering writes and flushing
// them in periodic transactions. WriteSample and WriteRouteChange
// return immediately (just an append + signal); the actual SQLite I/O
// happens in the background Run goroutine.
//
// Trade-off baked in: on a hard kill (SIGKILL, power loss) we lose up
// to flushInterval-worth of data. A clean shutdown via SIGINT/SIGTERM
// flushes the final batch. That window is the price for not paying
// SQLite's per-transaction overhead on every probe; we'd be the
// bottleneck otherwise.
type BatchedSink struct {
	store *Store
	log   *slog.Logger

	flushInterval time.Duration
	flushSize     int
	bufferCap     int

	mu      sync.Mutex
	samples []probe.Sample
	changes []probe.RouteChange
	dropped int // samples dropped since last log

	// flushCh is a non-blocking signal to the Run loop that the buffer
	// has crossed flushSize and should flush immediately rather than
	// wait for the next tick.
	flushCh chan struct{}

	// doneCh closes when Run returns, so callers can wait for the
	// final flush to complete.
	doneCh chan struct{}
}

// NewBatchedSink constructs a sink wrapping the given store. log may
// be nil; a no-op default is used. Defaults are applied for flush
// timing.
func NewBatchedSink(store *Store, log *slog.Logger) *BatchedSink {
	if log == nil {
		log = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &BatchedSink{
		store:         store,
		log:           log,
		flushInterval: defaultFlushInterval,
		flushSize:     defaultFlushSize,
		bufferCap:     defaultBufferCap,
		flushCh:       make(chan struct{}, 1),
		doneCh:        make(chan struct{}),
	}
}

// Done returns a channel that closes when Run has performed its final
// flush and exited. Callers wait on this during shutdown.
func (s *BatchedSink) Done() <-chan struct{} { return s.doneCh }

// Run is the background flusher loop. It ticks at flushInterval and
// also responds to size-threshold-triggered signals. On ctx.Done it
// performs a final flush before exiting, so a clean shutdown
// preserves all buffered data.
func (s *BatchedSink) Run(ctx context.Context) {
	defer close(s.doneCh)
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	// A separate ticker for the dropped-samples logging — at most once
	// every 5 seconds, so we don't spam logs if we're under sustained
	// backpressure.
	dropTicker := time.NewTicker(5 * time.Second)
	defer dropTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			if err := s.flush(); err != nil {
				s.log.Error("storage: final flush failed", "err", err)
			}
			s.logDropped()
			return
		case <-ticker.C:
			if err := s.flush(); err != nil {
				s.log.Warn("storage: tick flush failed", "err", err)
			}
		case <-s.flushCh:
			if err := s.flush(); err != nil {
				s.log.Warn("storage: size-triggered flush failed", "err", err)
			}
		case <-dropTicker.C:
			s.logDropped()
		}
	}
}

// WriteSample queues a sample. Returns immediately. If the in-memory
// buffer has crossed flushSize, signals the flusher to drain
// immediately rather than wait for the next tick.
func (s *BatchedSink) WriteSample(sample probe.Sample) error {
	s.mu.Lock()
	if len(s.samples) >= s.bufferCap {
		// Backpressure: drop the oldest to bound memory. The drop
		// counter is reported periodically by Run.
		s.samples = s.samples[1:]
		s.dropped++
	}
	s.samples = append(s.samples, sample)
	needFlush := len(s.samples) >= s.flushSize
	s.mu.Unlock()

	if needFlush {
		select {
		case s.flushCh <- struct{}{}:
		default:
			// Signal already pending; that's fine.
		}
	}
	return nil
}

// WriteRouteChange queues a route change. Route changes are rare, so
// there's no immediate-flush trigger — they get picked up on the next
// tick or whenever a sample flush happens.
func (s *BatchedSink) WriteRouteChange(rc probe.RouteChange) error {
	s.mu.Lock()
	s.changes = append(s.changes, rc)
	s.mu.Unlock()
	return nil
}

// flush atomically drains the in-memory buffers and writes them to
// SQLite in one transaction. If no events are buffered, it returns
// nil without touching the database.
func (s *BatchedSink) flush() error {
	// Drain the buffers under the lock so concurrent writers see
	// fresh slices to append into.
	s.mu.Lock()
	samples := s.samples
	changes := s.changes
	s.samples = nil
	s.changes = nil
	s.mu.Unlock()

	if len(samples) == 0 && len(changes) == 0 {
		return nil
	}

	tx, err := s.store.db.Begin()
	if err != nil {
		// On error, put the events back so they'll be retried on the
		// next flush. This is the right call for transient SQLite
		// busy errors; persistent failures will just keep being
		// retried, which is appropriate (the data isn't lost).
		s.requeue(samples, changes)
		return fmt.Errorf("begin tx: %w", err)
	}

	if err := s.insertSamples(tx, samples); err != nil {
		_ = tx.Rollback()
		s.requeue(samples, changes)
		return err
	}
	if err := s.insertRouteChanges(tx, changes); err != nil {
		_ = tx.Rollback()
		s.requeue(samples, changes)
		return err
	}

	if err := tx.Commit(); err != nil {
		s.requeue(samples, changes)
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// insertSamples writes the sample batch using a prepared statement
// scoped to the transaction. Times become ms-since-epoch, RTT becomes
// microseconds (integer, exact), and a zero RespIP becomes SQL NULL.
func (s *BatchedSink) insertSamples(tx *sql.Tx, samples []probe.Sample) error {
	if len(samples) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`INSERT INTO samples (target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert sample: %w", err)
	}
	defer stmt.Close()

	for _, sample := range samples {
		var ip sql.NullString
		if sample.IP.IsValid() {
			ip = sql.NullString{String: sample.IP.String(), Valid: true}
		}
		// The `target` column stores the operator-typed identifier
		// (Sample.TargetID, e.g. "dns.google" or "8.8.8.8"), not the
		// engine's resolved IP. That decoupling — added in step-34 —
		// lets hostname-typed pipelines survive periodic IP
		// re-resolution: the resolved IP can rotate freely, but the
		// rows stay reachable under the same stable identifier.
		// Back-compat: tests and any legacy callers that don't set
		// TargetID fall through to Target.String().
		target := sample.TargetID
		if target == "" {
			target = sample.Target.String()
		}
		if _, err := stmt.Exec(
			target,
			int64(sample.TTL),
			sample.Ts.UnixMilli(),
			ip,
			sample.RTT.Microseconds(),
		); err != nil {
			return fmt.Errorf("insert sample (ttl=%d): %w", sample.TTL, err)
		}
	}
	return nil
}

// insertRouteChanges writes the route-change batch. OldIP may be
// invalid (the hop was anonymous before the change) — stored as NULL.
func (s *BatchedSink) insertRouteChanges(tx *sql.Tx, changes []probe.RouteChange) error {
	if len(changes) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`INSERT INTO route_changes (target, ttl, ts, old_ip, new_ip) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert route_change: %w", err)
	}
	defer stmt.Close()

	for _, rc := range changes {
		var old sql.NullString
		if rc.OldIP.IsValid() {
			old = sql.NullString{String: rc.OldIP.String(), Valid: true}
		}
		// Same target re-key as insertSamples — see comment there.
		target := rc.TargetID
		if target == "" {
			target = rc.Target.String()
		}
		if _, err := stmt.Exec(
			target,
			int64(rc.TTL),
			rc.Ts.UnixMilli(),
			old,
			rc.NewIP.String(),
		); err != nil {
			return fmt.Errorf("insert route_change (ttl=%d): %w", rc.TTL, err)
		}
	}
	return nil
}

// requeue prepends drained events back into the buffers, capping at
// bufferCap with drop-oldest. Called on transaction failure so events
// aren't lost when a transient SQLite error occurs.
func (s *BatchedSink) requeue(samples []probe.Sample, changes []probe.RouteChange) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Prepend the returned events. New samples that arrived during the
	// failed flush sit at the tail; the retried ones go in front so
	// they're written in original order on the next attempt.
	if len(samples) > 0 {
		merged := make([]probe.Sample, 0, len(samples)+len(s.samples))
		merged = append(merged, samples...)
		merged = append(merged, s.samples...)
		if len(merged) > s.bufferCap {
			over := len(merged) - s.bufferCap
			s.dropped += over
			merged = merged[over:]
		}
		s.samples = merged
	}
	if len(changes) > 0 {
		merged := make([]probe.RouteChange, 0, len(changes)+len(s.changes))
		merged = append(merged, changes...)
		merged = append(merged, s.changes...)
		s.changes = merged
	}
}

// logDropped emits a single log line summarizing any samples dropped
// since the last call. Done from the Run goroutine on a 5s ticker, so
// sustained backpressure surfaces without spamming.
func (s *BatchedSink) logDropped() {
	s.mu.Lock()
	dropped := s.dropped
	s.dropped = 0
	s.mu.Unlock()

	if dropped > 0 {
		s.log.Warn("storage: dropped samples due to write backpressure",
			"count", dropped,
			"buffer_cap", s.bufferCap,
			"advice", "investigate disk latency or sustained probe rate")
	}
}

// discardWriter is an io.Writer that throws everything away. Used as
// the slog default destination when the caller passes a nil logger.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
