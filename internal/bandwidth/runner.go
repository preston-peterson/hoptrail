// The bandwidth test runner: capability detection, the wall-clock
// scheduler, speedtest execution + JSON parsing, and derate-flag
// computation. One goroutine (Run), reconfigured live via
// Reconfigure, manually triggered via RunNow.

package bandwidth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

// Capability reports whether the speedtest CLI is usable.
type Capability struct {
	Available bool   `json:"available"`
	Version   string `json:"version"`
	Error     string `json:"error,omitempty"`
}

// CommandRunner executes the speedtest CLI and returns its stdout.
// Injectable so tests feed canned JSON instead of saturating links.
type CommandRunner func(ctx context.Context, args ...string) ([]byte, error)

// execRunner is the production CommandRunner.
func execRunner(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "speedtest", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("speedtest: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("speedtest: %w", err)
	}
	return out, nil
}

// DetectCapability runs `speedtest --version`. Called at startup and
// periodically so an operator who installs the CLI mid-flight sees
// the capability flip without restarting hoptrail.
func DetectCapability(ctx context.Context, run CommandRunner) Capability {
	if run == nil {
		run = execRunner
	}
	out, err := run(ctx, "--version")
	if err != nil {
		return Capability{Available: false, Error: err.Error()}
	}
	// First line reads like: "Speedtest by Ookla 1.2.0.84 (ea6b6773cf) ..."
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	version := line
	if fields := strings.Fields(line); len(fields) >= 4 {
		version = fields[3]
	}
	return Capability{Available: true, Version: version}
}

// Pauser is the hook that quiets the ICMP probe engine during a test
// (design §5: concurrent ICMP under a saturated link looks like a
// latency flap that's actually our own measurement). The supervisor
// implements it; nil means no pausing.
type Pauser interface {
	PauseProbing()
	ResumeProbing()
}

// speedtestResult is the documented `--format=json` shape — the
// subset hoptrail reads. bandwidth fields are BYTES per second.
type speedtestResult struct {
	Type string `json:"type"`
	Ping struct {
		Latency float64 `json:"latency"`
	} `json:"ping"`
	Download struct {
		Bandwidth float64 `json:"bandwidth"`
		Bytes     int64   `json:"bytes"`
		Elapsed   int64   `json:"elapsed"`
	} `json:"download"`
	Upload struct {
		Bandwidth float64 `json:"bandwidth"`
		Bytes     int64   `json:"bytes"`
		Elapsed   int64   `json:"elapsed"`
	} `json:"upload"`
	Server struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"server"`
}

// Runner owns the bandwidth schedule and test execution.
type Runner struct {
	store *storage.Store
	log   *slog.Logger
	run   CommandRunner
	pause Pauser

	// now is injectable for schedule tests.
	now func() time.Time

	mu       sync.Mutex
	cfg      Config
	inFlight bool

	reconfigCh chan Config
	manualCh   chan struct{}
}

// NewRunner builds a Runner. run may be nil (production exec); pause
// may be nil (no ICMP pausing); log may be nil.
func NewRunner(store *storage.Store, cfg Config, run CommandRunner, pause Pauser, log *slog.Logger) *Runner {
	if run == nil {
		run = execRunner
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(nopWriter{}, nil))
	}
	return &Runner{
		store:      store,
		log:        log,
		run:        run,
		pause:      pause,
		now:        time.Now,
		cfg:        cfg,
		reconfigCh: make(chan Config, 1),
		manualCh:   make(chan struct{}, 1),
	}
}

// Reconfigure applies a new config live (schedule recomputes on the
// next loop turn). Non-blocking; the latest config wins.
func (r *Runner) Reconfigure(cfg Config) {
	select {
	case <-r.reconfigCh:
	default:
	}
	r.reconfigCh <- cfg
}

// RunNow triggers a manual test. Returns false when one is already
// in flight (the API maps that to 409).
func (r *Runner) RunNow() bool {
	r.mu.Lock()
	busy := r.inFlight
	r.mu.Unlock()
	if busy {
		return false
	}
	select {
	case r.manualCh <- struct{}{}:
		return true
	default:
		return true // trigger already pending — that's still "accepted"
	}
}

// InFlight reports whether a test is currently running.
func (r *Runner) InFlight() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inFlight
}

// Run is the scheduler loop. Startup hygiene: clears a stale
// run_in_flight row (a crash mid-test must not leave the UI stuck on
// "test in progress" — design §7).
func (r *Runner) Run(ctx context.Context) {
	_ = r.store.DeleteConfig(ctx, KeyRunInFlight)

	for {
		r.mu.Lock()
		cfg := r.cfg
		r.mu.Unlock()

		// Per-iteration timer (stopped on every path — a deferred Stop
		// here would pile up across loop turns).
		var timer *time.Timer
		var timerCh <-chan time.Time
		if cfg.Enabled {
			next, ok := r.nextRun(ctx, cfg)
			if ok {
				timer = time.NewTimer(next.Sub(r.now()))
				timerCh = timer.C
				r.log.Debug("bandwidth: next scheduled test", "at", next.Format(time.RFC3339))
			}
		}
		stopTimer := func() {
			if timer != nil {
				timer.Stop()
			}
		}

		select {
		case <-ctx.Done():
			stopTimer()
			return
		case newCfg := <-r.reconfigCh:
			stopTimer()
			r.mu.Lock()
			r.cfg = newCfg
			r.mu.Unlock()
			continue // recompute the schedule
		case <-r.manualCh:
			stopTimer()
			r.execute(ctx, cfg)
		case <-timerCh:
			r.execute(ctx, cfg)
		}
	}
}

// execute runs one speedtest end-to-end: pause ICMP if configured,
// invoke the CLI, parse, compute the derate flag against the rolling
// baseline, store the sample (failures too), and maintain the
// run_in_flight + derate-dismissal state rows.
func (r *Runner) execute(ctx context.Context, cfg Config) {
	r.mu.Lock()
	if r.inFlight {
		r.mu.Unlock()
		return
	}
	r.inFlight = true
	r.mu.Unlock()
	_ = r.store.SetConfig(ctx, KeyRunInFlight, "true")
	defer func() {
		r.mu.Lock()
		r.inFlight = false
		r.mu.Unlock()
		_ = r.store.DeleteConfig(ctx, KeyRunInFlight)
	}()

	if cfg.PauseICMP && r.pause != nil {
		r.pause.PauseProbing()
		defer r.pause.ResumeProbing()
	}

	args := []string{"--format=json", "--accept-license", "--accept-gdpr"}
	if cfg.ServerMode == "pinned" && cfg.ServerID != nil {
		args = append(args, fmt.Sprintf("--server-id=%d", *cfg.ServerID))
	}

	started := r.now()
	// A full test runs ~40s; 3 minutes covers slow links without
	// letting a hung CLI wedge the scheduler.
	runCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	out, err := r.run(runCtx, args...)
	cancel()
	durationMs := r.now().Sub(started).Milliseconds()

	smp := storage.BandwidthSample{Ts: started.UnixMilli(), DurationMs: durationMs}
	if err != nil {
		msg := err.Error()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		smp.Error = &msg
		r.log.Error("bandwidth: test failed", "err", err)
	} else {
		var res speedtestResult
		if perr := json.Unmarshal(out, &res); perr != nil || res.Type != "result" {
			msg := fmt.Sprintf("unexpected speedtest output (parse: %v)", perr)
			smp.Error = &msg
			r.log.Error("bandwidth: output parse failed", "err", perr, "raw_prefix", firstN(string(out), 200))
		} else {
			smp.Ok = true
			smp.DownMbps = res.Download.Bandwidth * 8 / 1e6
			smp.UpMbps = res.Upload.Bandwidth * 8 / 1e6
			smp.PingMs = res.Ping.Latency
			smp.BytesDown = res.Download.Bytes
			smp.BytesUp = res.Upload.Bytes
			if res.Server.ID != 0 {
				id := res.Server.ID
				smp.ServerID = &id
			}
			if res.Server.Name != "" {
				name := res.Server.Name
				smp.ServerName = &name
			}
			smp.DerateFlag = r.derated(ctx, cfg, smp)
		}
	}

	if err := r.store.RecordBandwidthSample(ctx, smp); err != nil {
		r.log.Error("bandwidth: sample store failed", "err", err)
		return
	}
	if smp.Ok {
		r.log.Info("bandwidth: test complete",
			"down_mbps", fmt.Sprintf("%.1f", smp.DownMbps),
			"up_mbps", fmt.Sprintf("%.1f", smp.UpMbps),
			"derated", smp.DerateFlag,
			"duration_ms", durationMs)
		if !smp.DerateFlag {
			// Incident resolved (or never existed): clear any dismissal
			// so the NEXT incident's banner shows again (design §7).
			_ = r.store.DeleteConfig(ctx, KeyDerateDismissedTs)
		}
	}
}

// derated applies the headline rule: flag when the latest test drops
// below threshold × baseline in any direction the operator has opted
// into (directions re-scope, design §5). Dormant until the baseline
// bootstrap gate is met.
func (r *Runner) derated(ctx context.Context, cfg Config, smp storage.BandwidthSample) bool {
	base, err := r.store.ComputeBandwidthBaseline(ctx, cfg.BaselineMetric, cfg.BaselineDays, cfg.HealthFloor, smp.Ts, MinBaselineSamples)
	if err != nil {
		r.log.Error("bandwidth: baseline compute failed", "err", err)
		return false
	}
	if base == nil {
		return false // still bootstrapping
	}
	downBad := smp.DownMbps < base.DownMbps*cfg.DerateThresh
	upBad := smp.UpMbps < base.UpMbps*cfg.DerateThresh
	switch cfg.Directions {
	case "down_only":
		return downBad
	case "up_only":
		return upBad
	default:
		return downBad || upBad
	}
}

// nextRun picks the next test time for the active cadence mode.
// Interval mode anchors on the latest sample's ts — reboot-safe with
// zero extra state (step-108); with no samples yet the first run
// lands 30s out (matches the enable-toggle first-test promise).
func (r *Runner) nextRun(ctx context.Context, cfg Config) (time.Time, bool) {
	if cfg.CadenceMode == "interval" {
		latest, err := r.store.LatestBandwidthSample(ctx)
		if err != nil {
			r.log.Error("bandwidth: latest-sample lookup failed", "err", err)
			return time.Time{}, false
		}
		return NextIntervalRun(latest, cfg.IntervalMin, r.now()), true
	}
	// Catch-up (step-155, after a power outage ate the 02:00 slot):
	// if the most recent PAST slot has no test recorded after it, the
	// box was down (or hoptrail wasn't running) when it came due —
	// run promptly instead of silently waiting for the next day.
	// Failed tests are stored too, so this can't loop.
	if prev, ok := PrevScheduled(r.now(), cfg.ScheduledTimes, cfg.Location()); ok {
		latest, err := r.store.LatestBandwidthSample(ctx)
		if err == nil && (latest == nil || time.UnixMilli(latest.Ts).Before(prev)) {
			r.log.Info("bandwidth: missed scheduled test, catching up", "missed_slot", prev.Format(time.RFC3339))
			return r.now().Add(5 * time.Second), true
		}
	}
	return NextScheduled(r.now(), cfg.ScheduledTimes, cfg.Location())
}

// PrevScheduled returns the most recent scheduled occurrence at or
// before now (looking back up to two days), mirroring NextScheduled's
// parsing and DST-skip rules.
func PrevScheduled(now time.Time, times []string, loc *time.Location) (time.Time, bool) {
	nowLoc := now.In(loc)
	best := time.Time{}
	for dayOffset := 0; dayOffset >= -2; dayOffset-- {
		day := nowLoc.AddDate(0, 0, dayOffset)
		for _, entry := range times {
			var hh, mm int
			if _, err := fmt.Sscanf(entry, "%d:%d", &hh, &mm); err != nil {
				continue
			}
			t := time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, loc)
			if t.Hour() != hh || t.Minute() != mm {
				continue // DST-nonexistent: skip, same as NextScheduled
			}
			if !t.After(now) && t.After(best) {
				best = t
			}
		}
	}
	return best, !best.IsZero()
}

// NextIntervalRun computes latest.Ts + interval, clamped to no
// earlier than now+1s (a long-overdue test fires promptly, not in
// the past); nil latest → now+30s.
func NextIntervalRun(latest *storage.BandwidthSample, intervalMin int, now time.Time) time.Time {
	if latest == nil {
		return now.Add(30 * time.Second)
	}
	next := time.UnixMilli(latest.Ts).Add(time.Duration(intervalMin) * time.Minute)
	if next.Before(now.Add(time.Second)) {
		return now.Add(time.Second)
	}
	return next
}

// NextScheduled returns the soonest wall-clock occurrence of any
// scheduled HH:MM strictly after `now` in loc, looking up to 2 days
// out (covers the DST-skip day). Nonexistent local times (the
// spring-forward gap) are skipped for that day per the design: honest
// gap, no compensation. ok=false when no entries parse.
func NextScheduled(now time.Time, times []string, loc *time.Location) (time.Time, bool) {
	nowLoc := now.In(loc)
	best := time.Time{}
	for dayOffset := 0; dayOffset <= 2; dayOffset++ {
		day := nowLoc.AddDate(0, 0, dayOffset)
		for _, entry := range times {
			var hh, mm int
			if _, err := fmt.Sscanf(entry, "%d:%d", &hh, &mm); err != nil {
				continue
			}
			t := time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, loc)
			// time.Date normalizes nonexistent local times (02:30 on
			// spring-forward becomes 03:30). The design says skip, not
			// shift — detect the normalization and drop that day's entry.
			if t.Hour() != hh || t.Minute() != mm {
				continue
			}
			if t.After(now) && (best.IsZero() || t.Before(best)) {
				best = t
			}
		}
	}
	return best, !best.IsZero()
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// nopWriter discards log output when no logger is provided.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
