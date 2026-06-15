package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// DiscoveryConfig configures the path discovery sweep loop. All fields
// required; NewDiscovery validates.
type DiscoveryConfig struct {
	// Target is the host to trace toward.
	Target netip.Addr

	// Interval is the time between sweeps. Slower than the per-hop
	// pinger's interval on purpose — discovery is about path shape, not
	// latency resolution.
	Interval time.Duration

	// Paused mirrors PingerConfig.Paused — sweeps skip while the
	// bandwidth test saturates the link.
	Paused *atomic.Bool

	// MaxHops is the largest TTL probed. Each sweep sends MaxHops
	// concurrent probes.
	MaxHops uint8

	// Timeout is per-probe (each TTL's probe waits up to this long for
	// a response before being recorded as a timeout).
	Timeout time.Duration
}

// Discovery runs the path discovery loop. It owns no persistent state:
// each sweep is independent and produces a complete SweepResult that
// the engine interprets to update its in-memory PathState.
//
// One Discovery per probe engine. Shares the Prober with the per-hop
// Pinger — both call ICMPProber.Probe concurrently; the prober
// demultiplexes responses by sequence number.
type Discovery struct {
	cfg    DiscoveryConfig
	prober ICMPProber
	engine *Engine
	log    *slog.Logger
}

// NewDiscovery validates the config and returns a Discovery ready to
// Run. Returns an error if any required field is missing or invalid.
func NewDiscovery(cfg DiscoveryConfig, prober ICMPProber, engine *Engine, log *slog.Logger) (*Discovery, error) {
	if !cfg.Target.IsValid() {
		return nil, errors.New("discovery: target must be a valid IP")
	}
	if cfg.Interval <= 0 {
		return nil, fmt.Errorf("discovery: interval must be positive, got %s", cfg.Interval)
	}
	if cfg.MaxHops == 0 || cfg.MaxHops > maxTTL {
		return nil, fmt.Errorf("discovery: max_hops must be 1..%d, got %d", maxTTL, cfg.MaxHops)
	}
	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("discovery: timeout must be positive, got %s", cfg.Timeout)
	}
	if prober == nil {
		return nil, errors.New("discovery: prober must not be nil")
	}
	if engine == nil {
		return nil, errors.New("discovery: engine must not be nil")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &Discovery{cfg: cfg, prober: prober, engine: engine, log: log}, nil
}

// Run starts the sweep loop. Runs one immediate sweep on entry, then
// ticks every Interval until ctx is canceled. Errors during individual
// sweeps are logged but do not stop the loop — losing one sweep is
// preferable to losing the daemon.
//
// Blocks until ctx is canceled.
func (d *Discovery) Run(ctx context.Context) {
	// Run an immediate sweep so the engine sees something before the
	// first Interval elapses. Without this, the first sweep is one
	// Interval (default 30s) after startup, which feels broken.
	d.sweep(ctx)

	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if d.cfg.Paused != nil && d.cfg.Paused.Load() {
				continue
			}
			d.sweep(ctx)
		}
	}
}

// sweep runs one TTL 1..maxTTL sweep, fanning out probes in parallel,
// then ships the assembled SweepResult to the engine.
//
// maxTTL is normally d.cfg.MaxHops, but if the engine has identified a
// target TTL (smallest TTL at which the destination responds with
// EchoReply), we cap the sweep at that TTL — probing higher is
// pointless since the destination responds at every higher TTL too.
//
// Full parallelism: all maxTTL probes are in flight at once. The
// Prober's mutex briefly serializes the SetTTL+WriteTo syscall pair,
// but the in-flight responses are demultiplexed by sequence number. A
// 30-hop sweep with a 2-second timeout completes in roughly 2 seconds
// (bounded by the slowest probe) rather than 60 seconds (sequential).
func (d *Discovery) sweep(ctx context.Context) {
	started := time.Now()

	// Cap the sweep at the engine's known target TTL, if any. The
	// snapshot query is cheap (~microseconds) and only blocks if the
	// reducer is mid-event; in steady state it's effectively free.
	maxTTL := d.cfg.MaxHops
	if snap, err := d.engine.PathSnapshot(ctx); err == nil {
		if snap.TargetTTL != 0 && snap.TargetTTL < maxTTL {
			maxTTL = snap.TargetTTL
		}
	}

	results := make([]ProbeResult, maxTTL)

	var wg sync.WaitGroup
	for i := uint8(0); i < maxTTL; i++ {
		ttl := i + 1
		idx := int(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[idx] = d.probeOne(ctx, ttl)
		}()
	}
	wg.Wait()

	// Find the terminal TTL: the smallest TTL at which the target
	// itself responded (EchoReply). Zero if no TTL reached the target.
	var terminalTTL uint8
	for _, r := range results {
		if r.Reply == ReplyEchoReply {
			terminalTTL = r.TTL
			break
		}
	}

	sweep := SweepResult{
		Target:      d.cfg.Target,
		Ts:          started,
		Results:     results,
		TerminalTTL: terminalTTL,
	}
	if err := d.engine.SendSweep(ctx, sweep); err != nil {
		if !errors.Is(err, context.Canceled) {
			d.log.Warn("discovery: failed to send sweep to engine", "err", err)
		}
	}
}

// probeOne issues one probe at a given TTL and translates the result
// into a ProbeResult. Returns a zero-value ProbeResult (TTL=0, which
// the engine skips) if the context was canceled — we don't want to
// record probes that didn't complete because we're shutting down.
func (d *Discovery) probeOne(ctx context.Context, ttl uint8) ProbeResult {
	ts := time.Now()
	res, err := d.prober.Probe(ctx, d.cfg.Target, ttl, d.cfg.Timeout)

	// Context cancellation means we're shutting down. Leave the slot
	// zero so the engine skips it.
	if errors.Is(err, context.Canceled) {
		return ProbeResult{}
	}

	pr := ProbeResult{
		Target: d.cfg.Target,
		TTL:    ttl,
		Ts:     ts,
	}
	switch {
	case errors.Is(err, ErrTimeout):
		pr.TimedOut = true
	case err != nil:
		// Something genuinely went wrong (socket error, marshal failure,
		// etc.). Log and treat as a timeout so the engine still gets a
		// slot for this TTL.
		d.log.Warn("discovery: probe error", "ttl", ttl, "err", err)
		pr.TimedOut = true
	default:
		pr.RespIP = res.RespIP
		pr.RTT = res.RTT
		pr.Reply = res.Type
	}
	return pr
}
