// `hoptrail probe` — the remote-probe role (v0.3 design §7). Same
// binary as `hoptrail serve`; this file is the probe's run loop:
// probe pipelines (engine + discovery + pinger, reused verbatim from
// the serve role) feeding an HTTPSink instead of local SQLite, a
// heartbeat loop that owns the target set, and fail-loud handling
// for auth failures per §12.1 + lesson #9.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/preston-peterson/hoptrail/internal/agent"
	"github.com/preston-peterson/hoptrail/internal/config"
	"github.com/preston-peterson/hoptrail/internal/probe"
)

// defaultAgentConfigPath is where `hoptrail probe` looks when --config is not
// given.
const defaultAgentConfigPath = "/etc/hoptrail/probe.yaml"

// cmdAgent implements `hoptrail probe`. Returns the process exit code.
// (The func keeps the internal "agent" name — see the terminology note
// in this package's doc comment in main.go.)
func cmdAgent(args []string) int {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	path := fs.String("config", defaultAgentConfigPath, "path to the probe config file")
	fs.Parse(args)

	cfg, err := config.LoadAgent(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
		return 1
	}
	logger, _ := newLogger(cfg.Log, nil)

	buffer, err := agent.OpenBuffer(cfg.Buffer.Path, cfg.Buffer.MaxSizeMB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
		return 1
	}
	defer buffer.Close()

	// The raw ICMP socket — the only piece requiring CAP_NET_RAW,
	// shared across all pipelines exactly like the serve role.
	prober, err := probe.NewProber()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
		return 1
	}
	defer prober.Close()

	client := agent.NewClient(cfg.Central.URL, cfg.Central.Token)
	sink, err := agent.NewHTTPSink(client, buffer, agent.SinkConfig{
		ProbeID:        cfg.ProbeID,
		IngestInterval: cfg.Central.IngestInterval.Std(),
	}, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	logger.Info("hoptrail probe: starting",
		"probe_id", cfg.ProbeID,
		"central", cfg.Central.URL,
		"version", version,
		"buffer", cfg.Buffer.Path,
	)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); sink.Run(runCtx) }()
	go func() { defer wg.Done(); sink.RunFlushLoop(runCtx) }()

	runner := &agentRunner{
		parentCtx: runCtx,
		prober:    prober,
		sink:      sink,
		client:    client,
		cfg:       cfg,
		logger:    logger,
	}
	defer runner.shutdown()

	// Heartbeat loop in a goroutine; its error return (401/400 —
	// config-shaped, unfixable by retry) is the agent's fatal signal
	// alongside the sink's 401 channel.
	hbErr := make(chan error, 1)
	go func() {
		hbErr <- agent.RunHeartbeat(runCtx, client, agent.HeartbeatConfig{
			ProbeID:     cfg.ProbeID,
			Version:     version,
			StartedAt:   time.Now(),
			Interval:    cfg.Central.HeartbeatInterval.Std(),
			Targets:     runner.targets,
			OnTargetSet: runner.reconcile,
		}, logger)
	}()

	exit := 0
	select {
	case <-ctx.Done():
		logger.Info("hoptrail probe: shutdown signal received")
	case err := <-hbErr:
		if err != nil {
			logger.Error("hoptrail probe: fatal heartbeat failure", "err", err)
			exit = 1
		}
	case err := <-sink.Fatal():
		logger.Error("hoptrail probe: fatal ingest failure", "err", err)
		exit = 1
	}

	// Take everything down: pipelines first (they feed the sink),
	// then the sink (its final flush delivers or spills what's left).
	runner.shutdown()
	cancel()
	wg.Wait()
	logger.Info("hoptrail probe: shutdown complete")
	return exit
}

// agentRunner owns the per-target probe pipelines, reconciled against
// whatever target set the last heartbeat returned. Until the first
// heartbeat succeeds the map is empty and the agent probes nothing
// (§7 — better than probing the wrong set).
type agentRunner struct {
	parentCtx context.Context
	prober    *probe.Prober
	sink      probe.Sink
	client    *agent.Client
	cfg       config.AgentConfig
	logger    *slog.Logger

	mu        sync.Mutex
	pipelines map[string]*agentPipeline
	stopped   bool
}

type agentPipeline struct {
	target string
	addr   netip.Addr
	engine *probe.Engine
	cancel context.CancelFunc
	done   chan struct{}
}

// targets returns the current local target set, announced in each
// heartbeat (informational; central owns the set).
func (r *agentRunner) targets() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.pipelines))
	for t := range r.pipelines {
		out = append(out, t)
	}
	return out
}

// reconcile diffs the local pipelines against central's authoritative
// set: missing targets get a pipeline, absent ones are torn down.
// Called from the heartbeat goroutine after every successful beat.
func (r *agentRunner) reconcile(targets []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	if r.pipelines == nil {
		r.pipelines = map[string]*agentPipeline{}
	}

	want := make(map[string]bool, len(targets))
	for _, t := range targets {
		if t != "" {
			want[t] = true
		}
	}

	for t, p := range r.pipelines {
		if !want[t] {
			r.logger.Info("probe: target removed by central; stopping pipeline", "target", t)
			p.cancel()
			delete(r.pipelines, t)
		}
	}
	for t := range want {
		if _, ok := r.pipelines[t]; ok {
			continue
		}
		p, err := r.buildPipeline(t)
		if err != nil {
			// Unresolvable now (DNS hiccup, bad name) — log and skip;
			// the next heartbeat's reconcile retries.
			r.logger.Warn("probe: pipeline build failed; will retry on next heartbeat", "target", t, "err", err)
			continue
		}
		r.logger.Info("probe: target added by central; pipeline started", "target", t, "addr", p.addr)
		r.pipelines[t] = p
	}
}

// shutdown cancels every pipeline and waits for them to drain.
// Idempotent — called both on the normal exit path and via defer.
func (r *agentRunner) shutdown() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	ps := r.pipelines
	r.pipelines = nil
	r.mu.Unlock()

	for _, p := range ps {
		p.cancel()
	}
	for t, p := range ps {
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			r.logger.Warn("probe: pipeline drain timed out", "target", t)
		}
	}
}

// buildPipeline mirrors the supervisor's: engine + discovery + pinger
// over the shared prober, but feeding the HTTPSink, plus an agent-only
// path-snapshot poster. Caller holds r.mu.
func (r *agentRunner) buildPipeline(target string) (*agentPipeline, error) {
	_, addr, err := resolveTarget(r.parentCtx, target)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(r.parentCtx)
	engine, err := probe.NewEngine(probe.EngineConfig{
		Target:               addr,
		TargetID:             target,
		RouteChangeThreshold: r.cfg.Probe.RouteChangeThreshold,
	}, r.sink, r.logger)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("engine: %w", err)
	}
	discovery, err := probe.NewDiscovery(probe.DiscoveryConfig{
		Target:   addr,
		Interval: r.cfg.Probe.DiscoveryInterval.Std(),
		MaxHops:  uint8(r.cfg.Probe.MaxHops),
		Timeout:  r.cfg.Probe.Timeout.Std(),
	}, r.prober, engine, r.logger)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("discovery: %w", err)
	}
	pinger, err := probe.NewPinger(probe.PingerConfig{
		Target:   addr,
		Interval: r.cfg.Probe.Interval.Std(),
		Timeout:  r.cfg.Probe.Timeout.Std(),
	}, r.prober, engine, r.logger)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("pinger: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); _ = engine.Run(ctx) }()
	go func() { defer wg.Done(); discovery.Run(ctx) }()
	go func() { defer wg.Done(); pinger.Run(ctx) }()
	go func() { defer wg.Done(); r.runSnapshotPoster(ctx, target, engine) }()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	// Hostname targets re-resolve periodically (same rationale as the
	// supervisor's step-34 loop): on IP change, tear down and rebuild
	// this pipeline so probing follows the DNS answer.
	if _, parseErr := netip.ParseAddr(target); parseErr != nil {
		go r.runReresolve(ctx, target, addr)
	}

	return &agentPipeline{
		target: target, addr: addr, engine: engine,
		cancel: cancel, done: done,
	}, nil
}

// wireHop is the hops_json element shape — the agent-side subset of
// the central's /api/path hopJSON (no hostname: rdns runs centrally;
// no loss_state: attribution is computed where the data is read).
type wireHop struct {
	TTL          int     `json:"ttl"`
	CurrentIP    *string `json:"current_ip"`
	CurrentRTTms float64 `json:"current_rtt_ms"`
	AvgRTTms     float64 `json:"avg_rtt_ms"`
	MinRTTms     float64 `json:"min_rtt_ms"`
	LossPercent  float64 `json:"loss_percent"`
	LastResponse *int64  `json:"last_response"`
}

type pathPostBody struct {
	ProbeID   string    `json:"probe_id"`
	Target    string    `json:"target"`
	Ts        int64     `json:"ts"`
	HopCount  int       `json:"hop_count"`
	TargetTTL int       `json:"target_ttl"`
	Hops      []wireHop `json:"hops"`
}

// runSnapshotPoster POSTs the engine's current path snapshot to
// central at the discovery cadence (§3.3). Failures are non-fatal —
// the snapshot is current-state, so a missed post is fully repaired
// by the next one; only 401 escalates (via the sink's fatal channel
// being the canonical path, heartbeat will hit it too).
func (r *agentRunner) runSnapshotPoster(ctx context.Context, target string, engine *probe.Engine) {
	ticker := time.NewTicker(r.cfg.Probe.DiscoveryInterval.Std())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		snap, err := engine.PathSnapshot(ctx)
		if err != nil {
			continue // engine shutting down
		}
		hops := make([]wireHop, 0, len(snap.Hops))
		for _, h := range snap.Hops {
			wh := wireHop{
				TTL:          int(h.TTL),
				CurrentRTTms: float64(h.CurrentRTT.Microseconds()) / 1000.0,
				AvgRTTms:     float64(h.AvgRTT.Microseconds()) / 1000.0,
				MinRTTms:     float64(h.MinRTT.Microseconds()) / 1000.0,
				LossPercent:  h.LossPercent,
			}
			if h.CurrentIP.IsValid() {
				ip := h.CurrentIP.String()
				wh.CurrentIP = &ip
			}
			if !h.LastResponse.IsZero() {
				ms := h.LastResponse.UnixMilli()
				wh.LastResponse = &ms
			}
			hops = append(hops, wh)
		}
		body, err := json.Marshal(pathPostBody{
			ProbeID:   r.cfg.ProbeID,
			Target:    target,
			Ts:        time.Now().UnixMilli(),
			HopCount:  len(snap.Hops),
			TargetTTL: int(snap.TargetTTL),
			Hops:      hops,
		})
		if err != nil {
			r.logger.Error("probe: path snapshot marshal failed", "target", target, "err", err)
			continue
		}
		if outcome, _, err := r.client.PostJSON(ctx, "/api/ingest/path", body); outcome != agent.OutcomeOK && err != nil {
			r.logger.Warn("probe: path snapshot post failed", "target", target, "err", err)
		}
	}
}

// runReresolve watches a hostname target's DNS answer; on change it
// rebuilds the pipeline via remove+re-add through the reconciler's
// lock. Exits when its pipeline's ctx fires or after triggering a
// rebuild (the new pipeline spawns its own loop).
func (r *agentRunner) runReresolve(ctx context.Context, target string, currentAddr netip.Addr) {
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
			r.logger.Warn("probe: re-resolve failed", "target", target, "err", err)
			continue
		}
		if newAddr == currentAddr {
			continue
		}
		r.logger.Info("probe: re-resolve IP changed, rebuilding pipeline",
			"target", target, "old", currentAddr, "new", newAddr)

		r.mu.Lock()
		if r.stopped {
			r.mu.Unlock()
			return
		}
		if p, ok := r.pipelines[target]; ok {
			p.cancel()
			delete(r.pipelines, target)
			if np, err := r.buildPipeline(target); err != nil {
				r.logger.Warn("probe: re-resolve rebuild failed; next heartbeat retries", "target", target, "err", err)
			} else {
				r.pipelines[target] = np
			}
		}
		r.mu.Unlock()
		return // the new pipeline owns re-resolution now
	}
}
