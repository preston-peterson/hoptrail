package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"
)

// EngineConfig configures the reducer.
type EngineConfig struct {
	// Target is the resolved IPv4 to probe.
	Target netip.Addr

	// TargetID is the operator-typed identifier (an IP string or a
	// hostname like "dns.google"). Carried into Sample.TargetID /
	// RouteChange.TargetID so storage rows can be queried by the
	// stable typed identifier rather than by the (potentially-
	// re-resolved) IP. If empty, the sink falls back to Target.String().
	TargetID string

	// RouteChangeThreshold is how many consecutive observations of a
	// new IP-at-TTL must accumulate before a route change is flagged.
	// Forwarded to analysis.DetectRouteChange on every observation.
	RouteChangeThreshold int

	// ProbeBufferSize and SweepBufferSize are the input channel
	// capacities. Sensible defaults are applied if zero.
	ProbeBufferSize int
	SweepBufferSize int
}

// Engine wraps the reducer goroutine, the input channels probe loops
// send on, and the query channel API handlers use to read PathState
// without taking locks.
//
// The reducer is a single goroutine that owns PathState. Every read or
// mutation of PathState — from probe loops, from API handlers — flows
// through the engine's channels and is processed serially by the
// reducer. This is the actor model from the design doc §2.
type Engine struct {
	cfg       EngineConfig
	sink      Sink
	log       *slog.Logger
	startedAt time.Time

	probes  chan ProbeResult
	sweeps  chan SweepResult
	queries chan queryRequest

	doneCh chan struct{}
	wg     sync.WaitGroup
}

// queryRequest is sent on Engine.queries by API handlers wanting a
// point-in-time view of PathState. The reducer replies on the channel
// in the request — no shared state involved.
type queryRequest struct {
	reply chan Snapshot
}

// Default channel capacities. Large enough to absorb short bursts
// without blocking the probe loops; small enough that backpressure
// reaches them under sustained overload.
const (
	defaultProbeBufferSize = 256
	defaultSweepBufferSize = 16
	defaultQueryBufferSize = 8
)

// NewEngine constructs an Engine. The reducer goroutine is not started
// until Run is called.
//
// log may be nil; a no-op default is used in that case (useful in
// tests that don't care about log output).
func NewEngine(cfg EngineConfig, sink Sink, log *slog.Logger) (*Engine, error) {
	if !cfg.Target.IsValid() {
		return nil, errors.New("engine: target must be a valid IP")
	}
	if cfg.RouteChangeThreshold < 1 {
		return nil, fmt.Errorf("engine: RouteChangeThreshold must be >= 1, got %d", cfg.RouteChangeThreshold)
	}
	if sink == nil {
		return nil, errors.New("engine: sink must not be nil")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}

	probeBuf := cfg.ProbeBufferSize
	if probeBuf == 0 {
		probeBuf = defaultProbeBufferSize
	}
	sweepBuf := cfg.SweepBufferSize
	if sweepBuf == 0 {
		sweepBuf = defaultSweepBufferSize
	}

	return &Engine{
		cfg:       cfg,
		sink:      sink,
		log:       log,
		startedAt: time.Now(),
		probes:    make(chan ProbeResult, probeBuf),
		sweeps:    make(chan SweepResult, sweepBuf),
		queries:   make(chan queryRequest, defaultQueryBufferSize),
		doneCh:    make(chan struct{}),
	}, nil
}

// Target returns the IP address this engine is tracing toward. Useful
// for HTTP handlers that need to surface it in API responses without
// duplicating the value at the call site.
func (e *Engine) Target() netip.Addr { return e.cfg.Target }

// StartedAt returns the timestamp at which the engine was constructed.
// Surfaced in /api/path so the UI can show "tracking for X minutes."
func (e *Engine) StartedAt() time.Time { return e.startedAt }

// Run starts the reducer goroutine and blocks until ctx is canceled.
// It returns nil after the reducer exits and all pending events have
// been drained. Returns an error only if the reducer was never started
// (always nil in current implementation).
//
// Call from a single goroutine; not safe to call Run more than once.
func (e *Engine) Run(ctx context.Context) error {
	e.wg.Add(1)
	go e.reducer(ctx)
	e.wg.Wait()
	close(e.doneCh)
	return nil
}

// Done returns a channel that is closed when Run has exited. Useful for
// callers that want to coordinate shutdown timing.
func (e *Engine) Done() <-chan struct{} { return e.doneCh }

// SendProbe feeds a single probe observation to the reducer. Called by
// the per-hop pinger loop. Blocks if the input channel is full; in
// steady state the reducer drains faster than probes arrive, so this is
// typically instantaneous.
//
// Returns ctx.Err() if ctx is canceled while waiting for buffer space.
func (e *Engine) SendProbe(ctx context.Context, r ProbeResult) error {
	select {
	case e.probes <- r:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SendSweep feeds a full path-discovery sweep to the reducer. Called by
// the discovery loop. The reducer processes the sweep's ProbeResults in
// TTL order; route-change detection sees the full path.
func (e *Engine) SendSweep(ctx context.Context, r SweepResult) error {
	select {
	case e.sweeps <- r:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PathSnapshot returns a point-in-time view of the path. Called by API
// handlers serving /api/path. Synchronous: blocks until the reducer
// answers the query, which it does between event processing steps.
func (e *Engine) PathSnapshot(ctx context.Context) (Snapshot, error) {
	reply := make(chan Snapshot, 1)
	select {
	case e.queries <- queryRequest{reply: reply}:
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
	select {
	case snap := <-reply:
		return snap, nil
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
}

// reducer is the single goroutine that owns PathState. All access to
// the state happens here, serialized by the select.
func (e *Engine) reducer(ctx context.Context) {
	defer e.wg.Done()
	state := NewPathState(e.cfg.Target, e.cfg.TargetID)

	for {
		select {
		case <-ctx.Done():
			return

		case r := <-e.probes:
			e.applyProbe(state, r)

		case sw := <-e.sweeps:
			for _, r := range sw.Results {
				// Skip zero-value slots (sweep slots past the terminal
				// where no probe was sent).
				if r.TTL == 0 {
					continue
				}
				e.applyProbe(state, r)
			}

		case q := <-e.queries:
			q.reply <- state.Snapshot()
		}
	}
}

// applyProbe runs one observation through PathState and writes the
// resulting Sample and (if present) RouteChange to the sink. Sink
// errors are logged but do not crash the reducer — losing a sample
// row is preferable to losing the daemon.
func (e *Engine) applyProbe(state *PathState, r ProbeResult) {
	if r.TTL == 0 || r.TTL > maxTTL {
		e.log.Warn("engine: ignoring probe with invalid TTL",
			"ttl", r.TTL, "target", r.Target)
		return
	}
	sample, rc := state.ApplyProbeResult(r, e.cfg.RouteChangeThreshold)
	if err := e.sink.WriteSample(sample); err != nil {
		e.log.Error("engine: sink WriteSample failed",
			"err", err, "ttl", r.TTL, "ts", r.Ts)
	}
	if rc != nil {
		if err := e.sink.WriteRouteChange(*rc); err != nil {
			e.log.Error("engine: sink WriteRouteChange failed",
				"err", err, "ttl", rc.TTL, "old", rc.OldIP, "new", rc.NewIP)
		} else {
			e.log.Info("engine: route change at hop",
				"ttl", rc.TTL, "old", rc.OldIP.String(), "new", rc.NewIP.String())
		}
	}
}

// discardWriter is an io.Writer that throws everything away. Used as
// the default slog destination when the caller passes a nil logger.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
