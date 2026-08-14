// supervisor owns N concurrent probe pipelines, one per monitored
// target. Each pipeline bundles engine + discovery + pinger + batched
// sink and runs independently; the supervisor's job is lifecycle
// (add / remove / swap a target) and serving HTTP-layer lookups
// ("give me the engine for target X" / "list all active targets").
//
// Step-29 lifted the target-identity type from netip.Addr to string
// so operators can monitor hostnames (dns.google) as well as raw IPs.
// The supervisor preserves the operator-typed string as the target's
// identity throughout — it's the map key, the storage column value,
// and the tab label — while resolving it once at add-time to the
// actual IP the prober uses internally.
//
// Thread-safety: the pipelines map is guarded by mu. add/remove
// serialize the heavyweight work (building/draining a pipeline)
// inside the critical section so concurrent /api/targets POSTs
// from a misbehaving client queue rather than racing.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/preston-peterson/hoptrail/internal/config"
	"github.com/preston-peterson/hoptrail/internal/probe"
	"github.com/preston-peterson/hoptrail/internal/server"
	"github.com/preston-peterson/hoptrail/internal/storage"
)

// reresolveInterval is how often a hostname-typed pipeline re-checks
// its target's DNS resolution. CDN-fronted hostnames (cloudflare.com,
// fastly-backed sites, etc.) rotate IPs across DNS TTLs; without
// periodic re-resolution the engine would probe a stale IP forever.
// 5 min is a balance: most A-record TTLs are 60s to a few hours, so
// we catch rotations within an acceptable window without hammering
// the system resolver.
const reresolveInterval = 5 * time.Minute

// Per-target probe-interval bounds (step-37). Below MinInterval the
// pinger would pile up backpressure on the prober's mutex faster than
// the network can drain; above MaxInterval the chart would feel dead
// and there's no operator scenario that needs less than one sample
// per minute. The UI's picker offers a handful of presets inside
// this range; the API enforces it.
const (
	MinProbeInterval = 200 * time.Millisecond
	MaxProbeInterval = 60 * time.Second
)

// ErrIntervalOutOfRange is returned by SetInterval / addPipeline when
// the requested interval is outside [MinProbeInterval, MaxProbeInterval].
// Surfaced as 400 by the handler.
var ErrIntervalOutOfRange = fmt.Errorf("probe interval must be between %s and %s",
	MinProbeInterval, MaxProbeInterval)

type supervisor struct {
	parentCtx context.Context
	logger    *slog.Logger

	prober *probe.Prober
	store  *storage.Store
	cfg    config.Config
	stream bool

	mu        sync.RWMutex
	pipelines map[string]*pipeline

	// probingPaused quiets every pipeline's pinger + discovery ticks
	// while the v0.4 bandwidth test saturates the link (shared into
	// each PingerConfig/DiscoveryConfig at build). Step-100.
	probingPaused atomic.Bool
}

// PauseProbing / ResumeProbing implement bandwidth.Pauser. Pausing is
// a flag flip, not a pipeline teardown — ticks skip while paused and
// resume on the next interval, leaving an honest gap in the samples.
func (s *supervisor) PauseProbing() {
	s.probingPaused.Store(true)
	s.logger.Info("supervisor: probing paused (bandwidth test in progress)")
}

func (s *supervisor) ResumeProbing() {
	s.probingPaused.Store(false)
	s.logger.Info("supervisor: probing resumed")
}

// pipeline bundles everything one target produces. Cancel closes the
// local ctx (all goroutines exit); done is closed once every
// goroutine has actually returned and the final sink flush has
// completed.
//
// target is the operator-supplied identity (IP string or hostname)
// — what the API surfaces and storage records. addr is the resolved
// IP the prober actually uses. For IP-typed targets the two are
// equal; for hostname-typed targets addr is what LookupHost returned
// at add-time and target is what the operator typed.
//
// interval is the per-hop pinger cadence this pipeline was built
// with — step-37 made it per-target so different tabs can run at
// different rates. SetInterval triggers a rebuild via rebuildPipeline
// whenever the operator picks a different value.
//
// warningMs / criticalMs (step-39) are pure display metadata — the
// per-tab latency thresholds the UI uses to paint green/yellow/red
// reference lines on the chart. Nil means "use the daemon defaults."
// They're guarded by the supervisor's mu (same lock the map uses)
// so SetThresholds + concurrent reads don't tear.
type pipeline struct {
	target       string
	addr         netip.Addr
	interval     time.Duration
	warningMs    *int64
	criticalMs   *int64
	finalHopOnly bool
	engine       *probe.Engine
	cancel       context.CancelFunc
	done         chan struct{}
}

func newSupervisor(parentCtx context.Context, prober *probe.Prober, store *storage.Store, cfg config.Config, stream bool, logger *slog.Logger) *supervisor {
	return &supervisor{
		parentCtx: parentCtx,
		logger:    logger,
		prober:    prober,
		store:     store,
		cfg:       cfg,
		stream:    stream,
		pipelines: make(map[string]*pipeline),
	}
}

// EngineFor returns the engine currently monitoring `target`, or nil
// if no such target is active.
func (s *supervisor) EngineFor(target string) *probe.Engine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.pipelines[target]
	if p == nil {
		return nil
	}
	return p.engine
}

// IntervalFor returns the active per-hop pinger interval for `target`,
// or 0 if the target isn't monitored. The UI uses this to surface
// the current cadence in the interval picker.
func (s *supervisor) IntervalFor(target string) time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.pipelines[target]
	if p == nil {
		return 0
	}
	return p.interval
}

// Intervals returns a snapshot of every active target's current
// pinger interval. Used by the GET /api/targets handler so a single
// call gives the UI both the target set and their cadences.
func (s *supervisor) Intervals() map[string]time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]time.Duration, len(s.pipelines))
	for k, p := range s.pipelines {
		out[k] = p.interval
	}
	return out
}

// Thresholds returns a snapshot of every active target's current
// latency-threshold overrides. Same one-call ergonomics as Intervals
// — the GET /api/targets handler bundles both into one response.
// The pair type lives in the server package (single source of truth
// for the wire shape); supervisor imports it.
func (s *supervisor) Thresholds() map[string]server.ThresholdPair {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]server.ThresholdPair, len(s.pipelines))
	for k, p := range s.pipelines {
		out[k] = server.ThresholdPair{WarningMs: p.warningMs, CriticalMs: p.criticalMs}
	}
	return out
}

// FinalHopOnlys returns a snapshot of every active target's
// final-hop-only flag. Same one-call pattern as Intervals /
// Thresholds — bundled into GET /api/targets.
func (s *supervisor) FinalHopOnlys() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]bool, len(s.pipelines))
	for k, p := range s.pipelines {
		out[k] = p.finalHopOnly
	}
	return out
}

// SetFinalHopOnly toggles the per-tab final-hop-only mode (step-41).
// Triggers a pipeline rebuild — the pinger config is at-construction,
// so an in-place flip would require restructuring NewPinger. Rebuild
// is brief (~1-2s gap) and matches the SetInterval pattern.
// Persists through to active_targets.
func (s *supervisor) SetFinalHopOnly(ctx context.Context, target string, finalHopOnly bool) error {
	changed, err := s.rebuildPipeline(ctx, target, nil, nil, &finalHopOnly)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := s.store.SetActiveTargetFinalHopOnly(ctx, target, finalHopOnly); err != nil {
		s.logger.Warn("supervisor: final_hop_only write failed",
			"target", target, "err", err)
	}
	s.logger.Info("supervisor: final_hop_only set",
		"target", target, "final_hop_only", finalHopOnly)
	return nil
}

// SetThresholds updates the per-tab latency thresholds for `target`.
// Unlike SetInterval, no pipeline rebuild — thresholds are pure
// display metadata, the probe engine doesn't read them. Writes
// through to active_targets so the change survives restart. Either
// pointer being nil clears the override (the UI falls back to its
// default preset). Returns an error if target isn't monitored or
// (warning, critical) are non-positive / out of ordering.
func (s *supervisor) SetThresholds(ctx context.Context, target string, warningMs, criticalMs *int64) error {
	if warningMs != nil && *warningMs <= 0 {
		return fmt.Errorf("warning_ms must be positive, got %d", *warningMs)
	}
	if criticalMs != nil && *criticalMs <= 0 {
		return fmt.Errorf("critical_ms must be positive, got %d", *criticalMs)
	}
	if warningMs != nil && criticalMs != nil && *warningMs >= *criticalMs {
		return fmt.Errorf("warning_ms (%d) must be less than critical_ms (%d)", *warningMs, *criticalMs)
	}

	s.mu.Lock()
	p, ok := s.pipelines[target]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("target %s not monitored", target)
	}
	p.warningMs = warningMs
	p.criticalMs = criticalMs
	s.mu.Unlock()

	if err := s.store.SetActiveTargetThresholds(ctx, target, warningMs, criticalMs); err != nil {
		s.logger.Warn("supervisor: thresholds write failed",
			"target", target, "err", err)
	}
	s.logger.Info("supervisor: thresholds set",
		"target", target, "warning_ms", warningMs, "critical_ms", criticalMs)
	return nil
}

// Targets returns the active target identifiers sorted lexically.
func (s *supervisor) Targets() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.pipelines))
	for k := range s.pipelines {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Add starts a new pipeline targeting `target`. The string may be
// either a raw IPv4 address or a hostname — hostnames are resolved
// to their first usable IPv4 A record. Returns an error if the
// target is already monitored, parses as neither, or resolves to
// no usable IPv4 record.
//
// On success, mirrors the new target into both active_targets (so
// Hydrate sees it on next startup) and target_history (so the UI's
// recent-targets dropdown surfaces it). Storage failures here are
// non-fatal — the pipeline is up and serving; persistence is a
// durability nicety, not a correctness gate.
//
// Operator-driven adds always use the default cadence (cfg.Probe.Interval),
// no threshold overrides (UI presets apply), and final-hop-only off
// (the operator opted into the tab to see hops). Per-target overrides
// are explicit follow-ups via SetInterval / SetThresholds /
// SetFinalHopOnly — keeping the add path simple keeps the "+" form
// a one-field input.
func (s *supervisor) Add(ctx context.Context, target string) error {
	id, addr, err := s.addPipeline(ctx, target, s.cfg.Probe.Interval.Std(), nil, nil, false)
	if err != nil {
		return err
	}

	if err := s.store.AddActiveTarget(ctx, id); err != nil {
		s.logger.Warn("supervisor: active-targets write failed",
			"target", id, "err", err)
	}
	if err := s.store.RememberTarget(ctx, id); err != nil {
		s.logger.Warn("supervisor: history write failed",
			"target", id, "err", err)
	}
	s.mu.RLock()
	count := len(s.pipelines)
	s.mu.RUnlock()
	s.logger.Info("supervisor: target added",
		"target", id, "addr", addr, "interval", s.cfg.Probe.Interval.Std(), "active_count", count)
	return nil
}

// addPipeline is the unified "bring up a new pipeline" path used by
// Add and Hydrate (and indirectly the re-resolve loop via
// rebuildPipeline). interval must be a validated, in-range duration
// — callers handle defaulting. warningMs / criticalMs are pure
// display metadata stored on the pipeline; nil means "no override."
// finalHopOnly (step-41) is a probe-behavior flag passed into the
// pinger config and also stashed on the pipeline for query/restore.
// Returns the resolved identity and addr so the caller can log
// meaningfully.
func (s *supervisor) addPipeline(ctx context.Context, target string, interval time.Duration, warningMs, criticalMs *int64, finalHopOnly bool) (string, netip.Addr, error) {
	id, addr, err := resolveTarget(ctx, target)
	if err != nil {
		return "", netip.Addr{}, err
	}
	if interval < MinProbeInterval || interval > MaxProbeInterval {
		return "", netip.Addr{}, ErrIntervalOutOfRange
	}

	s.mu.Lock()
	if _, ok := s.pipelines[id]; ok {
		s.mu.Unlock()
		return "", netip.Addr{}, fmt.Errorf("target %s already monitored", id)
	}
	newP, err := s.buildPipeline(id, addr, interval, finalHopOnly)
	if err != nil {
		s.mu.Unlock()
		return "", netip.Addr{}, fmt.Errorf("build pipeline for %s: %w", id, err)
	}
	newP.warningMs = warningMs
	newP.criticalMs = criticalMs
	s.pipelines[id] = newP
	s.mu.Unlock()
	return id, addr, nil
}

// Hydrate reads the persisted active_targets list from storage and
// brings up a pipeline for each. Called once from cmdServe at
// daemon startup, before the HTTP server starts accepting requests.
// Per-target resolution failures are logged but don't abort the
// hydrate — a stale entry (e.g. a hostname whose A record went away)
// shouldn't keep the rest of the tab set from coming back.
//
// Each row's persisted interval_ms (step-37) + warning_ms/critical_ms
// (step-39) are threaded through to the pipeline; NULL falls back to
// defaults so pre-migration databases continue to work.
func (s *supervisor) Hydrate(ctx context.Context) error {
	targets, err := s.store.ActiveTargets(ctx)
	if err != nil {
		return fmt.Errorf("supervisor: read active targets: %w", err)
	}
	for _, at := range targets {
		interval := s.cfg.Probe.Interval.Std()
		if at.IntervalMs != nil {
			interval = time.Duration(*at.IntervalMs) * time.Millisecond
		}
		// Clamp persisted values that pre-date the current bounds so
		// a row written by a future build doesn't keep the daemon
		// from coming back up.
		if interval < MinProbeInterval {
			interval = MinProbeInterval
		} else if interval > MaxProbeInterval {
			interval = MaxProbeInterval
		}
		id, addr, err := s.addPipeline(ctx, at.Target, interval, at.WarningMs, at.CriticalMs, at.FinalHopOnly)
		if err != nil {
			// "already monitored" can't happen during hydrate (we
			// start from an empty map). Log everything else.
			s.logger.Warn("supervisor: hydrate skip",
				"target", at.Target, "err", err)
			continue
		}
		s.logger.Info("supervisor: target hydrated",
			"target", id, "addr", addr, "interval", interval, "final_hop_only", at.FinalHopOnly)
	}
	s.logger.Info("supervisor: hydrated", "count", len(s.pipelines))
	return nil
}

// SetInterval changes the per-hop pinger cadence for `target`. The
// pipeline is rebuilt atomically: a new pinger is constructed at the
// new interval, the supervisor's map flips to it, then the old
// pipeline drains outside the lock. The active_targets row's
// interval_ms is updated so the change survives restart.
//
// Returns an error if target isn't monitored, or if interval is
// outside [MinProbeInterval, MaxProbeInterval]. Same-interval calls
// are a no-op (no rebuild, no DB write) — the picker calls SetInterval
// on every click, and clicks-without-change shouldn't churn a pipeline.
func (s *supervisor) SetInterval(ctx context.Context, target string, interval time.Duration) error {
	if interval < MinProbeInterval || interval > MaxProbeInterval {
		return ErrIntervalOutOfRange
	}
	changed, err := s.rebuildPipeline(ctx, target, nil, &interval, nil)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	ms := interval.Milliseconds()
	if err := s.store.SetActiveTargetInterval(ctx, target, &ms); err != nil {
		s.logger.Warn("supervisor: interval write failed",
			"target", target, "err", err)
	}
	s.logger.Info("supervisor: interval set",
		"target", target, "interval", interval)
	return nil
}

// rebuildPipeline atomically swaps the pipeline for `target`,
// optionally overriding its resolved addr and/or pinger interval.
// nil overrides preserve the existing value. Returns (changed, err):
// changed is false when the requested (addr, interval, finalHopOnly)
// all match the current pipeline (no rebuild was done).
//
// Locking: holds s.mu only for the snapshot + map swap; the old
// pipeline drains outside the lock so concurrent EngineFor / Targets
// readers don't block on the drain.
func (s *supervisor) rebuildPipeline(ctx context.Context, target string, newAddr *netip.Addr, newInterval *time.Duration, newFinalHopOnly *bool) (bool, error) {
	s.mu.Lock()
	old, ok := s.pipelines[target]
	if !ok {
		s.mu.Unlock()
		return false, fmt.Errorf("target %s not monitored", target)
	}
	addr := old.addr
	if newAddr != nil {
		addr = *newAddr
	}
	interval := old.interval
	if newInterval != nil {
		interval = *newInterval
	}
	finalHopOnly := old.finalHopOnly
	if newFinalHopOnly != nil {
		finalHopOnly = *newFinalHopOnly
	}
	if addr == old.addr && interval == old.interval && finalHopOnly == old.finalHopOnly {
		s.mu.Unlock()
		return false, nil
	}
	newP, err := s.buildPipeline(target, addr, interval, finalHopOnly)
	if err != nil {
		s.mu.Unlock()
		return false, fmt.Errorf("build pipeline for %s: %w", target, err)
	}
	// Preserve display-metadata across the rebuild — thresholds are
	// independent of probe behavior, so a SetInterval shouldn't reset
	// the operator's color choices (and a re-resolve shouldn't either).
	newP.warningMs = old.warningMs
	newP.criticalMs = old.criticalMs
	s.pipelines[target] = newP
	s.mu.Unlock()

	old.cancel()
	select {
	case <-old.done:
	case <-ctx.Done():
		s.logger.Warn("supervisor: rebuild drain exceeded ctx",
			"target", target, "err", ctx.Err())
	}
	return true, nil
}

// Remove stops monitoring `target`. Drains the pipeline (including
// final sink flush) before returning, bounded by ctx. Also clears
// the active_targets row so Hydrate on next startup doesn't bring
// the target back.
func (s *supervisor) Remove(ctx context.Context, target string) error {
	s.mu.Lock()
	p, ok := s.pipelines[target]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("target %s not monitored", target)
	}
	delete(s.pipelines, target)
	remainingCount := len(s.pipelines)
	s.mu.Unlock()

	p.cancel()
	select {
	case <-p.done:
	case <-ctx.Done():
		s.logger.Warn("supervisor: remove drain exceeded ctx",
			"target", target, "err", ctx.Err())
	}
	if err := s.store.RemoveActiveTarget(ctx, target); err != nil {
		s.logger.Warn("supervisor: active-targets delete failed",
			"target", target, "err", err)
	}
	s.logger.Info("supervisor: target removed",
		"target", target, "active_count", remainingCount)
	return nil
}

// Swap retains step-25's "replace THE target" semantic for the
// legacy POST /api/target endpoint. Resolves the input first (so
// hostnames work here too), then adds the new target and removes
// every other existing one.
func (s *supervisor) Swap(ctx context.Context, target string) error {
	id, _, err := resolveTarget(ctx, target)
	if err != nil {
		return err
	}

	s.mu.RLock()
	_, alreadyHave := s.pipelines[id]
	existing := make([]string, 0, len(s.pipelines))
	for k := range s.pipelines {
		existing = append(existing, k)
	}
	s.mu.RUnlock()

	if !alreadyHave {
		if err := s.Add(ctx, id); err != nil {
			return err
		}
	}

	for _, t := range existing {
		if t == id {
			continue
		}
		if err := s.Remove(ctx, t); err != nil {
			s.logger.Warn("supervisor: swap: failed to remove old target",
				"target", t, "err", err)
		}
	}
	return nil
}

// shutdown drains every active pipeline.
func (s *supervisor) shutdown(ctx context.Context) {
	s.mu.Lock()
	ps := s.pipelines
	s.pipelines = make(map[string]*pipeline)
	s.mu.Unlock()

	for _, p := range ps {
		p.cancel()
	}
	for target, p := range ps {
		select {
		case <-p.done:
		case <-ctx.Done():
			s.logger.Warn("supervisor: shutdown drain exceeded context",
				"target", target, "err", ctx.Err())
		}
	}
}

func (s *supervisor) buildPipeline(target string, addr netip.Addr, interval time.Duration, finalHopOnly bool) (*pipeline, error) {
	ctx, cancel := context.WithCancel(s.parentCtx)

	batchedSink := storage.NewBatchedSink(s.store, s.logger)
	var sink probe.Sink = batchedSink
	if s.stream {
		sink = &multiSink{sinks: []probe.Sink{batchedSink, &streamSink{out: os.Stdout}}}
	}

	// The engine + discovery + pinger probe the resolved IP. The
	// operator-typed `target` string is the API-level identity (map
	// key, tab label, API parameter) but doesn't reach storage — the
	// storage target column continues to be the resolved IP. Handlers
	// translate the typed identity to the IP for storage queries via
	// EngineFor(typed).Target().
	engine, err := probe.NewEngine(probe.EngineConfig{
		Target:               addr,
		TargetID:             target,
		RouteChangeThreshold: s.cfg.Probe.RouteChangeThreshold,
	}, sink, s.logger)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("engine: %w", err)
	}

	discovery, err := probe.NewDiscovery(probe.DiscoveryConfig{
		Target:   addr,
		Interval: s.cfg.Probe.DiscoveryInterval.Std(),
		MaxHops:  uint8(s.cfg.Probe.MaxHops),
		Timeout:  s.cfg.Probe.Timeout.Std(),
		Paused:   &s.probingPaused,
	}, s.prober, engine, s.logger)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("discovery: %w", err)
	}

	pinger, err := probe.NewPinger(probe.PingerConfig{
		Target:       addr,
		Interval:     interval,
		Timeout:      s.cfg.Probe.Timeout.Std(),
		FinalHopOnly: finalHopOnly,
		Paused:       &s.probingPaused,
	}, s.prober, engine, s.logger)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("pinger: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); _ = engine.Run(ctx) }()
	go func() { defer wg.Done(); discovery.Run(ctx) }()
	go func() { defer wg.Done(); pinger.Run(ctx) }()
	go func() { defer wg.Done(); batchedSink.Run(ctx) }()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	// If the target is hostname-typed (the typed string isn't itself
	// a parseable IP), spawn a re-resolution goroutine that watches
	// for the DNS answer to change and rebuilds the pipeline when it
	// does. The new pipeline (from rebuildPipeline) will spawn its own
	// re-resolve goroutine, so the chain continues naturally; the
	// goroutine here returns once it triggers a rebuild, since the
	// old pipeline ctx gets canceled inside rebuildPipeline.
	if _, parseErr := netip.ParseAddr(target); parseErr != nil {
		go s.runReresolve(ctx, target, addr)
	}

	return &pipeline{
		target:       target,
		addr:         addr,
		interval:     interval,
		finalHopOnly: finalHopOnly,
		engine:       engine,
		cancel:       cancel,
		done:         done,
	}, nil
}

// runReresolve is the per-hostname-pipeline re-resolution loop. Every
// reresolveInterval, it asks the system resolver for the current
// IPv4 for target. If the answer differs from currentAddr, it
// triggers an atomic pipeline rebuild via rebuildPipeline — the
// active_targets row is untouched (so the per-target interval and
// target_history row survive the rotation), and the new pipeline
// spawns its own re-resolve goroutine.
//
// Storage history survives because the storage `target` column is
// keyed by the operator-typed identifier (step-34), not by the
// resolved IP — so all rows for "cloudflare.com" stay reachable even
// as the underlying IP rotates.
//
// Failures here are intentionally non-fatal:
//   - Transient DNS errors → log, keep the old pipeline running
//   - rebuildPipeline failures → log, keep trying on the next tick
//
// The loop exits when ctx fires (parent pipeline canceled — meaning
// either the daemon's shutting down, or a rebuild is in progress and
// the next pipeline iteration will spawn its own re-resolve loop).
func (s *supervisor) runReresolve(ctx context.Context, target string, currentAddr netip.Addr) {
	ticker := time.NewTicker(reresolveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		_, newAddr, err := resolveTarget(ctx, target)
		if err != nil {
			s.logger.Warn("re-resolve failed", "target", target, "err", err)
			continue
		}
		if newAddr == currentAddr {
			continue
		}
		s.logger.Info("re-resolve: IP changed, rebuilding pipeline",
			"target", target, "old", currentAddr, "new", newAddr)

		// Fresh context for the rebuild — the original ctx is tied
		// to the pipeline we're about to drain. Bounded so a hung
		// supervisor mutex doesn't pin us forever.
		swapCtx, cancel := context.WithTimeout(s.parentCtx, 30*time.Second)
		if _, err := s.rebuildPipeline(swapCtx, target, &newAddr, nil, nil); err != nil {
			s.logger.Warn("re-resolve: rebuild failed", "target", target, "err", err)
			cancel()
			continue
		}
		cancel()
		// The new pipeline's buildPipeline spawned its own re-resolve
		// loop. Return so this goroutine exits cleanly.
		return
	}
}

// resolveTarget translates an operator-supplied target string into
// (typed identity, resolved IPv4 address). When `typed` parses as
// an IPv4 address it's used directly. Otherwise it's treated as a
// hostname and resolved via the system resolver; the first usable
// IPv4 A record becomes the probed address. The typed string is
// preserved as the returned identity so storage entries, tab labels,
// and API responses all reflect what the operator entered.
//
// Rejects unspecified (0.0.0.0) and multicast addresses regardless
// of how they were obtained; same rule the IP-only validator applied
// before step-29.
func resolveTarget(ctx context.Context, typed string) (string, netip.Addr, error) {
	if typed == "" {
		return "", netip.Addr{}, errors.New("target is empty")
	}

	if addr, err := netip.ParseAddr(typed); err == nil {
		if !addr.Is4() {
			return "", netip.Addr{}, fmt.Errorf("target %q is IPv6; only IPv4 is supported", typed)
		}
		if addr.IsUnspecified() || addr.IsMulticast() {
			return "", netip.Addr{}, fmt.Errorf("target %q is not a valid traceroute target", typed)
		}
		return typed, addr, nil
	}

	ips, err := net.DefaultResolver.LookupHost(ctx, typed)
	if err != nil {
		return "", netip.Addr{}, fmt.Errorf("resolve %q: %w", typed, err)
	}
	for _, ip := range ips {
		addr, perr := netip.ParseAddr(ip)
		if perr != nil {
			continue
		}
		if !addr.Is4() {
			continue
		}
		if addr.IsUnspecified() || addr.IsMulticast() {
			continue
		}
		return typed, addr, nil
	}
	return "", netip.Addr{}, fmt.Errorf("resolve %q: no usable IPv4 address found", typed)
}
