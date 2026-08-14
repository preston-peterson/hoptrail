// Command hoptrail is a continuous traceroute and per-hop latency tracker
// for Linux.
//
// Subcommands:
//
//	hoptrail serve          run the central daemon (probe engine + web UI + ingest)
//	hoptrail probe          run as a remote probe reporting to a central
//	hoptrail version        print version and build information
//	hoptrail check-config   validate a config file without starting
//	hoptrail token gen      generate a probe bearer token (v0.3)
//
// Terminology (settled with the operator during step-95): "probe" is
// the user-facing word everywhere — a probe is a measurement vantage
// point, and a deployment is one central plus N probes, of which the
// central hosts one itself ('local'). The remote-probe PROCESS is
// internally implemented by the internal/agent package ("agent" stays
// as the code-level term because internal/probe is already the ICMP
// engine); operators never see that word.
//
// The subcommand structure exists from v0.1 so that later commands (the
// distributed-mode agent, a separate server role) can be added without
// breaking the CLI contract.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/preston-peterson/hoptrail/internal/alert"
	"github.com/preston-peterson/hoptrail/internal/bandwidth"
	"github.com/preston-peterson/hoptrail/internal/capacity"
	"github.com/preston-peterson/hoptrail/internal/config"
	"github.com/preston-peterson/hoptrail/internal/logring"
	"github.com/preston-peterson/hoptrail/internal/probe"
	"github.com/preston-peterson/hoptrail/internal/rdns"
	"github.com/preston-peterson/hoptrail/internal/release"
	"github.com/preston-peterson/hoptrail/internal/retention"
	"github.com/preston-peterson/hoptrail/internal/server"
	"github.com/preston-peterson/hoptrail/internal/storage"
	"github.com/preston-peterson/hoptrail/internal/web"
)

// version is the hoptrail version string. Production builds inject the
// real value via -ldflags "-X main.version=<v>" (the Makefile derives it
// from `git describe --tags --always --dirty`). The "dev" fallback below
// is what `go run` and unflagged `go build` see — useful during iteration
// where the version isn't load-bearing. Declared `var` (not `const`) so
// the linker can override it.
var version = "dev"

// defaultConfigPath is where `serve` and `check-config` look for the
// config file when --config is not given.
const defaultConfigPath = "/etc/hoptrail/config.yaml"

func main() {
	// os.Args[1] selects the subcommand. Everything after it is that
	// subcommand's own flag set.
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "serve":
		os.Exit(cmdServe(os.Args[2:]))
	case "probe":
		os.Exit(cmdAgent(os.Args[2:]))
	case "version":
		os.Exit(cmdVersion(os.Args[2:]))
	case "check-config":
		os.Exit(cmdCheckConfig(os.Args[2:]))
	case "token":
		os.Exit(cmdToken(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "hoptrail: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

// usage prints the top-level command summary.
func usage() {
	fmt.Fprint(os.Stderr, `hoptrail — continuous traceroute and per-hop latency tracker for Linux

usage:
  hoptrail <subcommand> [flags]

subcommands:
  serve          run the central daemon (probe engine + web UI + probe ingest)
  probe          run as a remote probe reporting to a central daemon
  version        print version and build information
  check-config   validate a config file without starting
  token gen      generate a probe bearer token (v0.3 distributed probing)

run "hoptrail <subcommand> -h" for subcommand flags.
`)
}

// cmdVersion implements `hoptrail version`. It returns the process exit
// code.
func cmdVersion(args []string) int {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	fs.Parse(args)

	fmt.Printf("hoptrail %s\n", version)
	fmt.Printf("  go:   %s\n", runtime.Version())
	fmt.Printf("  os:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
	return 0
}

// cmdCheckConfig implements `hoptrail check-config`. It loads and
// validates the config file, printing either a confirmation or the
// aggregated list of problems. Exit code 0 means valid, 1 means invalid
// or unreadable.
func cmdCheckConfig(args []string) int {
	fs := flag.NewFlagSet("check-config", flag.ExitOnError)
	path := fs.String("config", "", "path to the config file (default "+defaultConfigPath+", or "+defaultAgentConfigPath+" with --probe)")
	agentMode := fs.Bool("probe", false, "validate a probe config (probe.yaml) instead of a central config")
	fs.Parse(args)

	if *agentMode {
		p := *path
		if p == "" {
			p = defaultAgentConfigPath
		}
		acfg, err := config.LoadAgent(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
			return 1
		}
		fmt.Printf("probe config %s is valid\n", p)
		fmt.Printf("  probe_id: %s\n", acfg.ProbeID)
		fmt.Printf("  central:  %s (heartbeat %s, ingest %s)\n",
			acfg.Central.URL, acfg.Central.HeartbeatInterval.Std(), acfg.Central.IngestInterval.Std())
		fmt.Printf("  probe:    every %s, discovery every %s, max %d hops, %s timeout\n",
			acfg.Probe.Interval.Std(), acfg.Probe.DiscoveryInterval.Std(), acfg.Probe.MaxHops, acfg.Probe.Timeout.Std())
		fmt.Printf("  buffer:   %s (max %d MB)\n", acfg.Buffer.Path, acfg.Buffer.MaxSizeMB)
		fmt.Println("  targets:  owned by central, received via heartbeat")
		return 0
	}

	p := *path
	if p == "" {
		p = defaultConfigPath
	}
	cfg, err := config.Load(p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
		return 1
	}

	fmt.Printf("config %s is valid\n", p)
	fmt.Printf("  listen:   %s\n", cfg.Listen)
	fmt.Printf("  storage:  %s\n", cfg.Storage.Path)
	fmt.Printf("  probe:    every %s, discovery every %s, max %d hops, %s timeout\n",
		cfg.Probe.Interval.Std(),
		cfg.Probe.DiscoveryInterval.Std(),
		cfg.Probe.MaxHops,
		cfg.Probe.Timeout.Std(),
	)
	fmt.Println("  targets:  loaded from the active_targets table on daemon start")
	return 0
}

// cmdServe implements `hoptrail serve`. It builds the probe engine
// (ICMP prober + reducer + discovery loop + per-hop pinger loop), the
// SQLite-backed batched storage sink, optionally a human-readable
// stdout sink for development (--stream), wires them together, and
// runs until SIGINT/SIGTERM.
//
// The HTTP server is not yet wired (next step). The daemon currently
// produces a real `.db` file with samples and route changes; the UI
// will read from it.
func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("config", defaultConfigPath, "path to the config file")
	stream := fs.Bool("stream", false, "also write samples and route changes to stdout in human-readable form (development aid; default is SQLite-only)")
	fs.Parse(args)

	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
		return 1
	}

	// Last ~2000 log records for the web-UI viewer (step-128) —
	// bounded, restart-volatile; journald keeps the real history.
	logRing := logring.New(2000)
	logger, logLevelVar := newLogger(cfg.Log, logRing)

	// Step-32 removed the yaml-seeded initial target. The active tab
	// set is now sourced from the active_targets table; supervisor.
	// Hydrate brings up a pipeline for each one. A first-time install
	// boots into an empty state where the operator adds targets via
	// the UI; the daemon writes back on each successful Add so the
	// set survives subsequent restarts.

	// Open the storage layer. Creates the file and runs migrations on
	// first open; subsequent opens just verify schema.
	store, err := storage.Open(cfg.Storage.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
		return 1
	}
	defer store.Close()

	schemaVer, err := store.SchemaVersion()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
		return 1
	}
	logger.Info("hoptrail: storage ready",
		"path", cfg.Storage.Path,
		"schema_version", schemaVer,
	)

	// Settings-panel overrides (step-125): config KV rows win over
	// yaml, the same precedence retention.days established. The
	// listen override is test-bound first — an unbindable value falls
	// back to yaml LOUDLY instead of crash-looping the daemon with
	// the web UI (the only place the bad value can be fixed) dead.
	settingsCtx := context.Background()
	if v, ok, err := store.GetConfig(settingsCtx, "log.level"); err == nil && ok {
		if lv, valid := parseLogLevel(v); valid {
			logLevelVar.Set(lv)
			cfg.Log.Level = v
		}
	}
	if v, ok, err := store.GetConfig(settingsCtx, "rdns.enabled"); err == nil && ok {
		switch v {
		case "true":
			cfg.RDNS.Enabled = true
		case "false":
			cfg.RDNS.Enabled = false
		}
	}
	if v, ok, err := store.GetConfig(settingsCtx, "server.listen"); err == nil && ok && v != "" && v != cfg.Listen {
		if ln, lerr := net.Listen("tcp", v); lerr != nil {
			logger.Error("hoptrail: server.listen override is not bindable — falling back to the yaml value",
				"override", v, "yaml", cfg.Listen, "err", lerr)
		} else {
			ln.Close()
			cfg.Listen = v
		}
	}

	// Build the raw ICMP prober. The only piece requiring CAP_NET_RAW;
	// if it fails, bail with a useful message. Lives outside the
	// supervisor because the socket is target-agnostic — swaps reuse it.
	prober, err := probe.NewProber()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
		return 1
	}
	defer prober.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Supervisor owns one probe pipeline per monitored target.
	// Hydrate reads the persisted active_targets and brings up a
	// pipeline for each. An empty active_targets table is fine —
	// fresh installs boot with no tabs and the operator adds the
	// first one via the UI.
	sup := newSupervisor(ctx, prober, store, cfg, *stream, logger)
	if err := sup.Hydrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
		return 1
	}

	// v0.4 bandwidth engine (step-100). Always wired — when disabled
	// (the default) the runner idles with no timer armed; when the
	// speedtest CLI is absent the capability getter reports it and the
	// UI routes to the install guidance. Capability re-detects every
	// 60s so installing the CLI mid-flight flips availability without
	// a daemon restart.
	bwCfg, err := bandwidth.LoadConfig(ctx, store)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
		return 1
	}
	bwRunner := bandwidth.NewRunner(store, bwCfg, nil, sup, logger)
	go bwRunner.Run(ctx)

	// v0.6 alerting (step-135, docs/design/v0.6-alerting-design.md):
	// evaluator + persistent-queue sender. Providers feed primitives
	// from data the daemon already has; loss/latency rules are
	// local-probe-scoped in v1 (remote sites are covered by
	// probe_offline, which the central detects unilaterally).
	alertCfg, alertWarnings, err := alert.LoadConfig(ctx, store)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
		return 1
	}
	for _, w := range alertWarnings {
		logger.Warn("alert: config", "warning", w)
	}
	alertEngine := alert.NewEngine(store, alertCfg, alert.Providers{
		Probes: func(pctx context.Context) (map[string]time.Time, error) {
			probes, err := store.ListProbes(pctx)
			if err != nil {
				return nil, err
			}
			out := map[string]time.Time{}
			for _, p := range probes {
				out[p.ProbeID] = time.UnixMilli(p.LastSeenAt)
			}
			return out, nil
		},
		Targets: func() map[string]alert.Thresholds {
			out := map[string]alert.Thresholds{}
			for target, thr := range sup.Thresholds() {
				out[target] = alert.Thresholds{WarningMs: thr.WarningMs, CriticalMs: thr.CriticalMs}
			}
			return out
		},
		DerateActive: func(dctx context.Context) (bool, string, error) {
			smp, err := store.LatestBandwidthSample(dctx)
			if err != nil || smp == nil || !smp.Ok {
				return false, "", err
			}
			desc := fmt.Sprintf("latest test %.0f↓/%.0f↑ Mbps below baseline", smp.DownMbps, smp.UpMbps)
			return smp.DerateFlag, desc, nil
		},
		WindowStats: func(wctx context.Context, target string, since, until time.Time) (storage.TargetWindowStats, error) {
			return store.TargetWindowStats(wctx, storage.LocalProbeID, target, since, until)
		},
		Capacity: func(cctx context.Context, active bool) (alert.CapacityVerdict, error) {
			acfg, _, lerr := alert.LoadConfig(cctx, store)
			if lerr != nil {
				return alert.CapacityVerdict{}, lerr
			}
			days := capacity.EffectiveRetentionDays(cctx, store, cfg.Storage.RetentionDays)
			m, err := capacity.Measure(cctx, store, cfg.Storage.Path, days)
			if err != nil {
				return alert.CapacityVerdict{}, err
			}
			v := m.Evaluate(capacity.Thresholds{
				FreeFloorMB:  acfg.DiskFreeFloorMB,
				FreePctFloor: acfg.DiskFreePctFloor,
				HeadroomMin:  acfg.HeadroomThreshold,
			}, active)
			return alert.CapacityVerdict{
				Valid:      v.Health != "unknown",
				Tripped:    v.Tripped,
				AlertMsg:   m.AlertMessage(v.Reason),
				RecoverMsg: m.RecoverMessage(),
			}, nil
		},
	}, logger)
	go alertEngine.Run(ctx, 15*time.Second)
	alertSender := alert.NewSender(store, func() alert.Config {
		c, _, _ := alert.LoadConfig(context.Background(), store)
		return c
	}, nil, logger)
	go alertSender.Run(ctx)

	// Release-update wiring (#11): the client serves the manual
	// check/download endpoints; the checker is the background cadence
	// (operator-set interval, default monthly, "off" honored). Neither
	// ever applies anything.
	relClient := release.NewClient()
	go (&release.Checker{Store: store, Fetch: relClient.Latest, Log: logger}).Run(ctx)

	var bwCap atomic.Pointer[bandwidth.Capability]
	initialCap := bandwidth.DetectCapability(ctx, nil)
	bwCap.Store(&initialCap)
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c := bandwidth.DetectCapability(ctx, nil)
				bwCap.Store(&c)
			}
		}
	}()

	// HTTP server: serves the embedded Svelte UI at / and JSON
	// endpoints under /api/. Reads the engine through the supervisor's
	// atomic pointer so swaps are visible per-request, and exposes the
	// supervisor's swap method via POST /api/target.
	webFS, err := web.FS()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
		return 1
	}
	httpServer, err := server.New(server.Config{
		ListenAddr:          cfg.Listen,
		Supervisor:          sup,
		Store:               store,
		WebFS:               webFS,
		Version:             version,
		AgentTokens:         cfg.Agents.Tokens,
		RetentionDays:       cfg.Storage.RetentionDays,
		BandwidthRunner:     bwRunner,
		BandwidthCapability: func() bandwidth.Capability { return *bwCap.Load() },
		RecheckCapability: func() {
			c := bandwidth.DetectCapability(ctx, nil)
			bwCap.Store(&c)
		},
		LogLevel: cfg.Log.Level,
		ApplyLogLevel: func(level string) error {
			lv, ok := parseLogLevel(level)
			if !ok {
				return fmt.Errorf("invalid log level %q", level)
			}
			logLevelVar.Set(lv)
			return nil
		},
		RDNSEnabled: cfg.RDNS.Enabled,
		LogRing:     logRing,

		AlertReconfigure:  alertEngine.Reconfigure,
		AlertSenderStatus: alertSender.Status,
		ReleaseSource:     relClient,

		StartedAt:    time.Now(),
		DBPath:       cfg.Storage.Path,
		AllowedHosts: cfg.AllowedHosts,
	}, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hoptrail: %v\n", err)
		return 1
	}

	logger.Info("hoptrail: starting",
		"active_targets", len(sup.Targets()),
		"interval", cfg.Probe.Interval.Std(),
		"discovery_interval", cfg.Probe.DiscoveryInterval.Std(),
		"max_hops", cfg.Probe.MaxHops,
		"listen", cfg.Listen,
		"stream", *stream,
	)

	// The pipeline's four goroutines (engine/discovery/pinger/sink) are
	// owned by the supervisor and tracked via its own done channel,
	// not this wg. wg here covers the three lifetime-of-daemon
	// goroutines: HTTP server, rdns (optional), retention. They all
	// see the same SIGINT-driven ctx cancellation.
	//
	// httpFailed tracks whether the HTTP server died for a non-shutdown
	// reason (typically a bind failure when something else holds the
	// configured port). When that happens, we cancel the shared context
	// to take the rest of the daemon down with it, and return non-zero
	// so systemd marks the unit failed. The previous behavior — log
	// the error and let other goroutines keep running — produced a
	// "process is alive but UI is dead" state (lesson #9).
	var wg sync.WaitGroup
	var httpFailed atomic.Bool

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := httpServer.Run(ctx); err != nil {
			logger.Error("hoptrail: HTTP server failed", "err", err)
			httpFailed.Store(true)
			stop() // cancel the shared context; other goroutines drain
		}
	}()

	if cfg.RDNS.Enabled {
		resolver := rdns.New(rdns.DefaultConfig(), store, rdns.SystemResolver, logger)
		wg.Add(1)
		go func() { defer wg.Done(); resolver.Run(ctx) }()
		logger.Info("hoptrail: rdns worker enabled")
	} else {
		logger.Info("hoptrail: rdns worker disabled in config")
	}

	// Retention is always on — RetentionDays validation guarantees >= 1.
	// The worker runs an initial sweep at startup (in case the daemon
	// was down long enough for stale rows to accumulate) and then once
	// per hour. rdns table is deliberately excluded; see internal/
	// storage/retention.go for the rationale.
	rcfg := retention.DefaultConfig(cfg.Storage.RetentionDays)
	rcfg.DBPath = cfg.Storage.Path // enables hourly db-size sampling for capacity
	retentionWorker := retention.New(
		rcfg,
		store,
		logger,
	)
	wg.Add(1)
	go func() { defer wg.Done(); retentionWorker.Run(ctx) }()

	<-ctx.Done()
	logger.Info("hoptrail: shutdown signal received, stopping loops")

	// Drain the pipeline first. Use a fresh context so the wait can
	// extend past the parent ctx cancellation — the batched-sink final
	// flush is the slowest part and we want to give it room to land
	// the last samples before store.Close runs.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	sup.shutdown(shutdownCtx)

	wg.Wait()
	logger.Info("hoptrail: stopped")
	if httpFailed.Load() {
		return 1
	}
	return 0
}

// multiSink fans a Sink call out to multiple underlying sinks. The
// first sink's error is returned; later errors are logged but not
// surfaced. The reducer only ever looks at the first; if storage
// fails, the streamSink output still gets through (and vice versa).
type multiSink struct {
	sinks []probe.Sink
}

func (m *multiSink) WriteSample(s probe.Sample) error {
	var firstErr error
	for _, sink := range m.sinks {
		if err := sink.WriteSample(s); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *multiSink) WriteRouteChange(rc probe.RouteChange) error {
	var firstErr error
	for _, sink := range m.sinks {
		if err := sink.WriteRouteChange(rc); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// streamSink writes Samples and RouteChanges to an io.Writer in a
// human-readable, one-line-per-event form modeled on traceroute / the
// step-2 spike output. The intent is that piping `hoptrail serve` to
// `less` or `tail -f` is immediately useful for an operator.
//
// Output examples:
//
//	15:04:05.123 ttl=1  responder=192.0.2.1     rtt=1.2ms
//	15:04:05.456 ttl=4  responder=*               rtt=*       (timeout)
//	15:04:05.789 route change ttl=3: 203.0.113.12 -> 203.0.113.45
//
// One sink per running engine; the engine guarantees the WriteSample
// and WriteRouteChange methods are called from a single goroutine, so
// no mutex is needed inside the sink.
type streamSink struct {
	out io.Writer
}

func (s *streamSink) WriteSample(sample probe.Sample) error {
	ts := sample.Ts.Format("15:04:05.000")
	if sample.IP.IsValid() {
		_, err := fmt.Fprintf(s.out,
			"%s ttl=%-2d responder=%-15s rtt=%s\n",
			ts, sample.TTL, sample.IP.String(), sample.RTT.Round(100*time.Microsecond),
		)
		return err
	}
	_, err := fmt.Fprintf(s.out,
		"%s ttl=%-2d responder=*               rtt=*       (timeout)\n",
		ts, sample.TTL,
	)
	return err
}

func (s *streamSink) WriteRouteChange(rc probe.RouteChange) error {
	old := "*"
	if rc.OldIP.IsValid() {
		old = rc.OldIP.String()
	}
	_, err := fmt.Fprintf(s.out,
		"%s route change ttl=%d: %s -> %s\n",
		rc.Ts.Format("15:04:05.000"),
		rc.TTL,
		old,
		rc.NewIP.String(),
	)
	return err
}

// newLogger builds a slog.Logger from the config's log section. Output
// goes to stderr so it doesn't mix with the streamSink's stdout
// stream (operators can redirect them independently:
// `hoptrail serve >samples.log 2>daemon.log`).
//
// Step-125: the returned LevelVar is the live verbosity knob — the
// settings panel's log-level control adjusts it at runtime through
// server.Config.ApplyLogLevel, no restart.
//
// Step-128: a non-nil ring tees every emitted record into the
// in-memory buffer behind GET /api/logs (the web-UI log viewer).
// The probe role passes nil — it has no HTTP surface.
func newLogger(cfg config.LogConfig, ring *logring.Ring) (*slog.Logger, *slog.LevelVar) {
	levelVar := new(slog.LevelVar)
	if lv, ok := parseLogLevel(cfg.Level); ok {
		levelVar.Set(lv)
	}
	opts := &slog.HandlerOptions{Level: levelVar}
	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	if ring != nil {
		handler = logring.NewHandler(handler, ring)
	}
	return slog.New(handler), levelVar
}

// parseLogLevel maps the config/API level strings onto slog levels.
func parseLogLevel(s string) (slog.Level, bool) {
	switch s {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}
