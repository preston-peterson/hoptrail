// HTTPSink replaces the local BatchedSink on agents (§7): same
// probe.Sink interface, same buffer-and-tick shape, but the flush
// target is central's /api/ingest/samples instead of local SQLite.
// Failed deliveries spill to the Buffer; RunFlushLoop drains it.

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/preston-peterson/hoptrail/internal/probe"
)

const (
	defaultIngestInterval = 5 * time.Second
	defaultFlushSize      = 2000

	// Flush-loop backoff bounds (§8): start at the base, double per
	// consecutive failure, cap at the max.
	flushRetryBase = 5 * time.Second
	flushRetryMax  = 60 * time.Second
)

// SinkConfig controls the HTTPSink.
type SinkConfig struct {
	// ProbeID is this agent's identity, stamped into every batch.
	ProbeID string

	// IngestInterval is the live-flush cadence (config
	// central.ingest_interval, default 5s).
	IngestInterval time.Duration

	// FlushSize triggers an early flush when the in-memory buffer
	// crosses it, bounding memory under burst.
	FlushSize int
}

// wire shapes — must match the server's ingest contract (§3.2). The
// server's types are unexported; the contract is pinned by tests on
// both sides plus the two-host e2e.
type batchPayload struct {
	ProbeID      string            `json:"probe_id"`
	BatchID      string            `json:"batch_id"`
	Samples      []wireSample      `json:"samples"`
	RouteChanges []wireRouteChange `json:"route_changes"`
}

type wireSample struct {
	Target string  `json:"target"`
	TTL    int     `json:"ttl"`
	Ts     int64   `json:"ts"`
	IP     *string `json:"ip"`
	RTTms  float64 `json:"rtt_ms"`
}

type wireRouteChange struct {
	Target string  `json:"target"`
	TTL    int     `json:"ttl"`
	Ts     int64   `json:"ts"`
	OldIP  *string `json:"old_ip"`
	NewIP  string  `json:"new_ip"`
}

// HTTPSink implements probe.Sink for the agent role. WriteSample and
// WriteRouteChange return immediately; Run flushes on a ticker (and
// on size threshold), POSTing each batch to central and spilling to
// the Buffer on retryable failure. RunFlushLoop (a separate
// goroutine) drains the spill buffer with exponential backoff.
type HTTPSink struct {
	client *Client
	buffer *Buffer
	log    *slog.Logger
	cfg    SinkConfig

	mu      sync.Mutex
	samples []probe.Sample
	changes []probe.RouteChange

	flushCh chan struct{}
	doneCh  chan struct{}

	// fatalCh delivers the first 401 — the agent's main loop treats
	// it like lesson #9's bind failure: take everything down, exit
	// non-zero, let systemd surface the failed unit.
	fatalCh   chan error
	fatalOnce sync.Once
}

// NewHTTPSink builds the sink. buffer is required (partition recovery
// is not optional in the agent role); log may be nil.
func NewHTTPSink(client *Client, buffer *Buffer, cfg SinkConfig, log *slog.Logger) (*HTTPSink, error) {
	if client == nil || buffer == nil {
		return nil, fmt.Errorf("probe: sink needs a client and a buffer")
	}
	if cfg.ProbeID == "" {
		return nil, fmt.Errorf("probe: sink needs a probe id")
	}
	if cfg.IngestInterval <= 0 {
		cfg.IngestInterval = defaultIngestInterval
	}
	if cfg.FlushSize <= 0 {
		cfg.FlushSize = defaultFlushSize
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(nopWriter{}, nil))
	}
	return &HTTPSink{
		client:  client,
		buffer:  buffer,
		log:     log,
		cfg:     cfg,
		flushCh: make(chan struct{}, 1),
		doneCh:  make(chan struct{}),
		fatalCh: make(chan error, 1),
	}, nil
}

// Fatal delivers the first unrecoverable sink error (401 from
// central). The agent's run loop selects on it alongside ctx.
func (s *HTTPSink) Fatal() <-chan error { return s.fatalCh }

// Done closes after Run's final flush, mirroring BatchedSink.
func (s *HTTPSink) Done() <-chan struct{} { return s.doneCh }

// WriteSample queues a sample; immediate return, size-triggered flush
// signal past the threshold. Mirrors BatchedSink.
func (s *HTTPSink) WriteSample(sample probe.Sample) error {
	s.mu.Lock()
	s.samples = append(s.samples, sample)
	needFlush := len(s.samples) >= s.cfg.FlushSize
	s.mu.Unlock()
	if needFlush {
		select {
		case s.flushCh <- struct{}{}:
		default:
		}
	}
	return nil
}

// WriteRouteChange queues a route change; picked up on the next flush.
func (s *HTTPSink) WriteRouteChange(rc probe.RouteChange) error {
	s.mu.Lock()
	s.changes = append(s.changes, rc)
	s.mu.Unlock()
	return nil
}

// Run is the live-flush loop. On ctx cancellation it performs a final
// flush so a clean agent shutdown loses nothing — central-unreachable
// at shutdown spills to the buffer, picked up on next start.
func (s *HTTPSink) Run(ctx context.Context) {
	defer close(s.doneCh)
	ticker := time.NewTicker(s.cfg.IngestInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Final flush uses a fresh context: the run context is
			// already canceled and would abort the POST mid-flight.
			flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			s.flushOnce(flushCtx)
			cancel()
			return
		case <-ticker.C:
			s.flushOnce(ctx)
		case <-s.flushCh:
			s.flushOnce(ctx)
		}
	}
}

// flushOnce drains the in-memory buffers into one batch and delivers
// it: POST to central, spill to the buffer on retryable failure, drop
// with an ERROR log on 4xx.
func (s *HTTPSink) flushOnce(ctx context.Context) {
	s.mu.Lock()
	samples := s.samples
	changes := s.changes
	s.samples = nil
	s.changes = nil
	s.mu.Unlock()

	if len(samples) == 0 && len(changes) == 0 {
		return
	}

	batchID, err := NewBatchID()
	if err != nil {
		s.log.Error("probe: batch id generation failed; dropping batch", "err", err)
		return
	}
	payload, err := json.Marshal(buildPayload(s.cfg.ProbeID, batchID, samples, changes))
	if err != nil {
		s.log.Error("probe: batch marshal failed; dropping batch", "err", err)
		return
	}

	outcome, _, err := s.client.PostJSON(ctx, "/api/ingest/samples", payload)
	switch outcome {
	case OutcomeOK:
		return
	case OutcomeRetry:
		evicted, bufErr := s.buffer.Add(ctx, batchID, time.Now().UnixMilli(), payload)
		if bufErr != nil {
			s.log.Error("probe: central unreachable AND buffer write failed; batch lost",
				"batch_id", batchID, "post_err", err, "buffer_err", bufErr)
			return
		}
		if evicted > 0 {
			s.log.Warn("probe: buffer full; evicted oldest batches", "evicted", evicted)
		}
		s.log.Warn("probe: central unreachable; batch spilled to buffer",
			"batch_id", batchID, "samples", len(samples), "err", err)
	case OutcomeDrop:
		s.log.Error("probe: central rejected batch; dropping", "batch_id", batchID, "err", err)
	case OutcomeUnauthorized:
		s.deliverFatal(err)
	}
}

// RunFlushLoop drains the spill buffer oldest-first, one batch at a
// time (§8): on success delete and continue; on retryable failure
// back off exponentially (5s → 60s); on 4xx delete the poison batch
// so it can't wedge the queue (buffered data older than central's
// 24h skew bound lands here — accepted data loss after a >24h
// partition, logged loudly).
func (s *HTTPSink) RunFlushLoop(ctx context.Context) {
	delay := flushRetryBase
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		healthy := s.drainBuffer(ctx)
		if healthy {
			delay = flushRetryBase
		} else {
			delay *= 2
			if delay > flushRetryMax {
				delay = flushRetryMax
			}
		}
		timer.Reset(delay)
	}
}

// drainBuffer posts pending batches until the buffer is empty or a
// delivery fails. Returns false only on a retryable failure (the
// signal to back off).
func (s *HTTPSink) drainBuffer(ctx context.Context) bool {
	for {
		batchID, payload, ok, err := s.buffer.Oldest(ctx)
		if err != nil {
			s.log.Error("probe: buffer read failed", "err", err)
			return false
		}
		if !ok {
			return true
		}
		outcome, _, err := s.client.PostJSON(ctx, "/api/ingest/samples", payload)
		switch outcome {
		case OutcomeOK:
			if err := s.buffer.Delete(ctx, batchID); err != nil {
				// The batch was delivered but not deleted — it will be
				// re-posted and deduped by central's ingest_log. Safe,
				// just noisy; log and back off rather than hot-loop.
				s.log.Error("probe: buffer delete failed after delivery", "batch_id", batchID, "err", err)
				return false
			}
			s.log.Info("probe: buffered batch delivered", "batch_id", batchID)
		case OutcomeRetry:
			return false
		case OutcomeDrop:
			s.log.Error("probe: central rejected buffered batch; dropping it",
				"batch_id", batchID, "err", err)
			if err := s.buffer.Delete(ctx, batchID); err != nil {
				s.log.Error("probe: poison batch delete failed", "batch_id", batchID, "err", err)
				return false
			}
		case OutcomeUnauthorized:
			s.deliverFatal(err)
			return false
		}
	}
}

// deliverFatal pushes the first fatal error to Fatal(); later ones
// are dropped (one is enough to take the agent down).
func (s *HTTPSink) deliverFatal(err error) {
	s.fatalOnce.Do(func() { s.fatalCh <- err })
}

// buildPayload converts probe events to the wire shape. Target uses
// the operator-typed identifier when present (step-34 semantics),
// falling back to the resolved IP.
func buildPayload(probeID, batchID string, samples []probe.Sample, changes []probe.RouteChange) batchPayload {
	p := batchPayload{
		ProbeID:      probeID,
		BatchID:      batchID,
		Samples:      make([]wireSample, 0, len(samples)),
		RouteChanges: make([]wireRouteChange, 0, len(changes)),
	}
	for _, smp := range samples {
		ws := wireSample{
			Target: targetID(smp.TargetID, smp.Target),
			TTL:    int(smp.TTL),
			Ts:     smp.Ts.UnixMilli(),
			RTTms:  float64(smp.RTT.Microseconds()) / 1000.0,
		}
		if smp.IP.IsValid() {
			ip := smp.IP.String()
			ws.IP = &ip
		}
		p.Samples = append(p.Samples, ws)
	}
	for _, rc := range changes {
		wc := wireRouteChange{
			Target: targetID(rc.TargetID, rc.Target),
			TTL:    int(rc.TTL),
			Ts:     rc.Ts.UnixMilli(),
			NewIP:  rc.NewIP.String(),
		}
		if rc.OldIP.IsValid() {
			old := rc.OldIP.String()
			wc.OldIP = &old
		}
		p.RouteChanges = append(p.RouteChanges, wc)
	}
	return p
}

func targetID(typed string, resolved netip.Addr) string {
	if typed != "" {
		return typed
	}
	return resolved.String()
}

// nopWriter discards log output when no logger is provided.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
