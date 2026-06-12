package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync/atomic"
	"time"
)

// PingerConfig configures the per-hop pinger loop.
type PingerConfig struct {
	// Target is the host being traced. Probes carry this as the
	// destination; each probe is differentiated by TTL.
	Target netip.Addr

	// Interval is the time between pinger ticks — i.e. how often each
	// known hop is pinged. This is the cadence the per-hop latency
	// timeline samples at.
	Interval time.Duration

	// Timeout is per-probe.
	Timeout time.Duration

	// Paused, when non-nil and true, makes ticks no-ops. Shared
	// across every pipeline by the daemon so the v0.4 bandwidth test
	// can quiet ICMP while it saturates the link (a probe storm under
	// saturation reads as a latency flap that's actually our own
	// measurement). Samples simply don't happen while paused — an
	// honest gap.
	Paused *atomic.Bool

	// FinalHopOnly (step-41) skips intermediate-TTL pings — only the
	// destination is probed. Discovery still runs (route changes are
	// detected) but per-hop sample density is sacrificed for ~95%
	// less probe traffic on long paths. Useful for casual sanity tabs.
	// When set without a known TargetTTL yet, ticks idle until
	// discovery lands the destination.
	FinalHopOnly bool
}

// Pinger runs the per-hop pinger loop. On each tick, it fetches the
// current set of known hops from the engine and sends one probe per
// hop in parallel, feeding each ProbeResult back to the engine.
//
// The loop is "passive" in the sense that it only probes hops that
// have already been discovered; new hops are discovered by the
// Discovery sweep. If discovery has produced no results yet, the
// pinger ticks idle until the first sweep lands.
type Pinger struct {
	cfg    PingerConfig
	prober ICMPProber
	engine *Engine
	log    *slog.Logger
}

// NewPinger validates the config and returns a Pinger ready to Run.
func NewPinger(cfg PingerConfig, prober ICMPProber, engine *Engine, log *slog.Logger) (*Pinger, error) {
	if !cfg.Target.IsValid() {
		return nil, errors.New("pinger: target must be a valid IP")
	}
	if cfg.Interval <= 0 {
		return nil, fmt.Errorf("pinger: interval must be positive, got %s", cfg.Interval)
	}
	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("pinger: timeout must be positive, got %s", cfg.Timeout)
	}
	if prober == nil {
		return nil, errors.New("pinger: prober must not be nil")
	}
	if engine == nil {
		return nil, errors.New("pinger: engine must not be nil")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &Pinger{cfg: cfg, prober: prober, engine: engine, log: log}, nil
}

// Run starts the pinger loop. Ticks every Interval until ctx is
// canceled. Blocks until cancellation.
func (p *Pinger) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if p.cfg.Paused != nil && p.cfg.Paused.Load() {
				continue
			}
			p.tick(ctx)
		}
	}
}

// tick fetches the current path snapshot from the engine and spawns one
// probe goroutine per known hop.
//
// Fire-and-forget: tick does NOT wait for the probe goroutines to
// finish. Waiting would cap the tick cadence to the slowest probe in
// the cohort — if any TTL times out (2s default), the next tick would
// be delayed by that long regardless of the configured Interval. With
// fire-and-forget, fast hops produce samples at Interval and slow hops
// produce samples at their natural rate; the two do not constrain
// each other.
//
// In-flight bound: each probe is bounded by Prober's per-call timeout,
// so the number of in-flight probe goroutines per TTL is at most
// ceil(Timeout / Interval). At the defaults (2s timeout, 1s interval)
// that's 2 in-flight per TTL — for 30 hops, a steady-state peak of
// ~60 goroutines. Cheap.
//
// Sample timestamping: each probe's Ts field is set when the probe is
// SENT, not when its response arrives. The engine's Sample stream is
// therefore correct on the time axis even when probes complete out of
// order or arrive at storage interleaved by completion time.
func (p *Pinger) tick(ctx context.Context) {
	snap, err := p.engine.PathSnapshot(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			p.log.Warn("pinger: failed to fetch path snapshot", "err", err)
		}
		return
	}
	if len(snap.Hops) == 0 {
		// No hops known yet (discovery hasn't produced a sweep). Skip
		// this tick; we'll try again next interval.
		return
	}

	// snap.Hops is already capped at TargetTTL by PathState.Snapshot
	// when the target is known, so this loop naturally restricts
	// itself once discovery has found the destination.
	//
	// FinalHopOnly (step-41): when set, skip everything except the
	// target TTL. Discovery is the only thing keeping intermediate
	// hops fresh in this mode — the operator accepts stale per-hop
	// data in exchange for ~95% less ping traffic. Until the target
	// TTL is known (snap.TargetTTL == 0), we tick idle rather than
	// probe every TTL — that'd defeat the bandwidth-saving point.
	if p.cfg.FinalHopOnly {
		if snap.TargetTTL == 0 {
			return
		}
		go p.probeAndSend(ctx, snap.TargetTTL)
		return
	}
	for _, hop := range snap.Hops {
		ttl := hop.TTL
		go p.probeAndSend(ctx, ttl)
	}
}

// probeAndSend issues one probe and ships the result to the engine.
func (p *Pinger) probeAndSend(ctx context.Context, ttl uint8) {
	ts := time.Now()
	res, err := p.prober.Probe(ctx, p.cfg.Target, ttl, p.cfg.Timeout)

	if errors.Is(err, context.Canceled) {
		// Shutting down — don't bother sending.
		return
	}

	pr := ProbeResult{
		Target: p.cfg.Target,
		TTL:    ttl,
		Ts:     ts,
	}
	switch {
	case errors.Is(err, ErrTimeout):
		pr.TimedOut = true
	case err != nil:
		p.log.Warn("pinger: probe error", "ttl", ttl, "err", err)
		pr.TimedOut = true
	default:
		pr.RespIP = res.RespIP
		pr.RTT = res.RTT
		pr.Reply = res.Type
	}

	if err := p.engine.SendProbe(ctx, pr); err != nil {
		if !errors.Is(err, context.Canceled) {
			p.log.Warn("pinger: failed to send probe to engine", "err", err)
		}
	}
}
