// The alerting evaluator: ticks against injected data providers, runs
// the per-incident state machine (sustain before raise, paired
// recovery on clear), and pushes through the gating pipeline into the
// persistent queue. Quiet-hours raises/clears accumulate into one
// summary message delivered when the window ends.

package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

// summaryKey is the config row buffering quiet-hours events.
const summaryKey = "alert.summary_pending"

// probeOfflineAfter mirrors the UI's offline rule (3× the 60s default
// heartbeat).
const probeOfflineAfter = 180 * time.Second

// Thresholds is a target's latency lines in ms (nil = no override —
// no latency alert for that target; alerting deliberately does not
// invent thresholds the operator never set).
type Thresholds struct {
	WarningMs  *int64
	CriticalMs *int64
}

// Providers are the engine's data inputs, injected so the package
// stays free of probe/supervisor imports and tests feed primitives.
type Providers struct {
	// Probes returns (probe_id, last_seen) for every REMOTE probe.
	Probes func(ctx context.Context) (map[string]time.Time, error)
	// Targets returns the per-target latency thresholds for every
	// monitored target (locally probed; loss/latency rules are
	// local-probe-scoped in v1 — remote sites are covered by
	// probe_offline).
	Targets func() map[string]Thresholds
	// DerateActive reports whether a bandwidth derate is in effect
	// and a short description ("down 612 of 940 Mbps baseline").
	DerateActive func(ctx context.Context) (bool, string, error)
	// WindowStats returns destination-hop stats for the local probe.
	WindowStats func(ctx context.Context, target string, since, until time.Time) (storage.TargetWindowStats, error)
}

// Engine evaluates rules on Tick and enqueues notifications.
type Engine struct {
	store *storage.Store
	prov  Providers
	log   *slog.Logger

	mu  sync.Mutex
	cfg Config
	// lastRaised: cooldown memory per (event|subject). In-memory only —
	// a restart forgetting a cooldown re-alerts at worst, never loses.
	lastRaised map[string]time.Time
	// sent timestamps within the last hour (global rate limit).
	sent []time.Time
	now  func() time.Time
}

func NewEngine(store *storage.Store, cfg Config, prov Providers, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.New(slog.NewTextHandler(nopWriter{}, nil))
	}
	return &Engine{
		store: store, prov: prov, log: log, cfg: cfg,
		lastRaised: map[string]time.Time{}, now: time.Now,
	}
}

// Reconfigure swaps the live config (PATCH endpoint).
func (e *Engine) Reconfigure(cfg Config) {
	e.mu.Lock()
	e.cfg = cfg
	e.mu.Unlock()
}

func (e *Engine) config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// Run ticks every interval until ctx ends. 15s default.
func (e *Engine) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.Tick(ctx); err != nil {
				e.log.Warn("alert: tick failed", "err", err)
			}
		}
	}
}

// Tick is one evaluation pass. Exported for tests.
func (e *Engine) Tick(ctx context.Context) error {
	cfg := e.config()
	now := e.now()

	// Quiet-hours window just ended? Flush the summary regardless of
	// what else this tick finds.
	if !inQuietWindow(now, cfg.QuietStart, cfg.QuietEnd) {
		if err := e.flushSummary(ctx, cfg, now); err != nil {
			e.log.Warn("alert: summary flush failed", "err", err)
		}
	}
	if !cfg.Enabled {
		return nil
	}

	states, err := e.store.ListAlertStates(ctx)
	if err != nil {
		return err
	}
	stateOf := map[string]*storage.AlertState{}
	for i := range states {
		s := states[i]
		stateOf[s.EventType+"|"+s.Subject] = &s
	}
	sustain := time.Duration(cfg.SustainS) * time.Second

	// ---- probe offline ----
	if cfg.EventProbeOffline && e.prov.Probes != nil {
		probes, err := e.prov.Probes(ctx)
		if err != nil {
			return err
		}
		for id, lastSeen := range probes {
			down := now.Sub(lastSeen) > probeOfflineAfter
			e.step(ctx, cfg, now, stateOf, EventProbeOffline, id, down, 0,
				fmt.Sprintf("probe %s offline — last heartbeat %s", id, lastSeen.Format("15:04:05")),
				fmt.Sprintf("probe %s recovered", id), "high")
		}
	}

	// ---- target loss + latency (local probe) ----
	targets := map[string]Thresholds{}
	if e.prov.Targets != nil {
		targets = e.prov.Targets()
	}
	for target, thr := range targets {
		if e.prov.WindowStats == nil {
			break
		}
		st, err := e.prov.WindowStats(ctx, target, now.Add(-sustain), now)
		if err != nil {
			return err
		}
		// Too few samples = no verdict either way (daemon just started,
		// target just added). Existing incidents persist untouched.
		const minSamples = 10
		if st.Sent >= minSamples {
			if cfg.EventTargetLoss {
				lossPct := 100 * float64(st.Sent-st.Received) / float64(st.Sent)
				down := lossPct >= cfg.LossPct
				e.step(ctx, cfg, now, stateOf, EventTargetLoss, target, down, sustain,
					fmt.Sprintf("%s unreachable — %.0f%% loss over the last %s", target, lossPct, sustain),
					fmt.Sprintf("%s reachable again", target), "high")
			}
			if cfg.EventLatency && st.Received > 0 {
				var limit *int64
				if cfg.LatencyLevel == "warning" {
					limit = thr.WarningMs
				} else {
					limit = thr.CriticalMs
				}
				if limit != nil {
					avgMs := st.AvgRTTUs / 1000
					over := avgMs > *limit
					e.step(ctx, cfg, now, stateOf, EventLatency, target, over, sustain,
						fmt.Sprintf("%s latency %dms — above the %dms %s line for %s", target, avgMs, *limit, cfg.LatencyLevel, sustain),
						fmt.Sprintf("%s latency back under %dms", target, *limit), "default")
				}
			}
		}
	}

	// ---- bandwidth derate ----
	if cfg.EventDerate && e.prov.DerateActive != nil {
		active, desc, err := e.prov.DerateActive(ctx)
		if err != nil {
			return err
		}
		e.step(ctx, cfg, now, stateOf, EventDerate, "bandwidth", active, 0,
			"bandwidth derate: "+desc, "bandwidth back at baseline", "default")
	}
	return nil
}

// step runs one incident's state machine. sustain 0 = the condition
// is already debounced upstream (heartbeat staleness, derate flag).
func (e *Engine) step(ctx context.Context, cfg Config, now time.Time,
	stateOf map[string]*storage.AlertState, event, subject string,
	condition bool, sustain time.Duration, alertMsg, recoverMsg, priority string) {

	key := event + "|" + subject
	st := stateOf[key]

	switch {
	case condition && st == nil:
		// First sighting: start (or immediately fire when no sustain).
		ns := storage.AlertState{EventType: event, Subject: subject, State: "raising", Since: now.UnixMilli()}
		if sustain == 0 {
			e.raise(ctx, cfg, now, &ns, event, subject, alertMsg, priority)
		}
		if err := e.store.UpsertAlertState(ctx, ns); err != nil {
			e.log.Warn("alert: state write failed", "err", err)
		}

	case condition && st.State == "raising":
		if now.Sub(time.UnixMilli(st.Since)) >= sustain {
			e.raise(ctx, cfg, now, st, event, subject, alertMsg, priority)
			if err := e.store.UpsertAlertState(ctx, *st); err != nil {
				e.log.Warn("alert: state write failed", "err", err)
			}
		}

	case !condition && st != nil:
		// Cleared. Recovery message only if the alert actually went out.
		if st.State == "active" && st.NotifiedAt != nil {
			e.deliver(ctx, cfg, now, event, subject, recoverMsg, "default", true)
		}
		if err := e.store.DeleteAlertState(ctx, event, subject); err != nil {
			e.log.Warn("alert: state delete failed", "err", err)
		}
	}
}

// raise marks the state active and pushes the alert through gating.
func (e *Engine) raise(ctx context.Context, cfg Config, now time.Time,
	st *storage.AlertState, event, subject, msg, priority string) {
	st.State = "active"
	// Cooldown: a re-raise of the same incident shortly after the last
	// one is tracked (state active, recovery suppressed-pairing intact)
	// but not re-notified.
	key := event + "|" + subject
	e.mu.Lock()
	last, seen := e.lastRaised[key]
	e.mu.Unlock()
	if seen && now.Sub(last) < time.Duration(cfg.CooldownS)*time.Second {
		e.log.Info("alert: raise suppressed by cooldown", "event", event, "subject", subject)
		return
	}
	if e.deliver(ctx, cfg, now, event, subject, msg, priority, false) {
		ts := now.UnixMilli()
		st.NotifiedAt = &ts
		e.mu.Lock()
		e.lastRaised[key] = now
		e.mu.Unlock()
	}
}

// deliver applies quiet hours + the rate limit, then enqueues. Returns
// whether the event was accepted (queued or summarized).
func (e *Engine) deliver(ctx context.Context, cfg Config, now time.Time,
	event, subject, msg, priority string, recovery bool) bool {

	// History first (step-149): the log records what HAPPENED —
	// quiet-hours buffering and rate limiting affect delivery, not
	// history. Failure is log-and-continue.
	kind := "alert"
	if recovery {
		kind = "recovered"
	}
	if err := e.store.AppendAlertHistory(ctx, storage.AlertHistoryEntry{
		Ts: now.UnixMilli(), EventType: event, Subject: subject, Kind: kind, Message: msg,
	}); err != nil {
		e.log.Warn("alert: history write failed", "err", err)
	}

	if inQuietWindow(now, cfg.QuietStart, cfg.QuietEnd) {
		if err := e.appendSummary(ctx, summaryEntry{
			Ts: now.UnixMilli(), Event: event, Subject: subject, Msg: msg, Recovery: recovery,
		}); err != nil {
			e.log.Warn("alert: summary append failed", "err", err)
			return false
		}
		return true
	}

	// Global rate limit (sliding hour). Recoveries ride free — a
	// recovery you don't get is worse than one extra message.
	if !recovery {
		e.mu.Lock()
		cut := now.Add(-time.Hour)
		kept := e.sent[:0]
		for _, t := range e.sent {
			if t.After(cut) {
				kept = append(kept, t)
			}
		}
		e.sent = kept
		if len(e.sent) >= cfg.RateLimitPerH {
			e.mu.Unlock()
			e.log.Warn("alert: rate limit hit, dropping", "event", event, "subject", subject)
			return false
		}
		e.sent = append(e.sent, now)
		e.mu.Unlock()
	}

	title := "hoptrail: " + event
	if recovery {
		title = "hoptrail: recovered"
		priority = "default"
	}
	if err := e.store.EnqueueAlert(ctx, sanitizeLatin1(title), msg, priority, now); err != nil {
		e.log.Error("alert: enqueue failed", "err", err)
		return false
	}
	e.log.Info("alert: queued", "event", event, "subject", subject, "recovery", recovery)
	return true
}

// ---- quiet hours + summary ----

type summaryEntry struct {
	Ts       int64  `json:"ts"`
	Event    string `json:"event"`
	Subject  string `json:"subject"`
	Msg      string `json:"msg"`
	Recovery bool   `json:"recovery"`
}

// inQuietWindow handles windows that wrap midnight ("22:00"-"07:00").
// Empty strings = no quiet hours. Local time.
func inQuietWindow(now time.Time, start, end string) bool {
	if start == "" || end == "" {
		return false
	}
	sh, sm, err1 := parseHHMM(start)
	eh, em, err2 := parseHHMM(end)
	if err1 != nil || err2 != nil {
		return false
	}
	cur := now.Hour()*60 + now.Minute()
	s, e := sh*60+sm, eh*60+em
	if s == e {
		return false
	}
	if s < e {
		return cur >= s && cur < e
	}
	return cur >= s || cur < e // wraps midnight
}

func (e *Engine) appendSummary(ctx context.Context, entry summaryEntry) error {
	entries, _ := e.loadSummary(ctx)
	entries = append(entries, entry)
	raw, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return e.store.SetConfig(ctx, summaryKey, string(raw))
}

func (e *Engine) loadSummary(ctx context.Context) ([]summaryEntry, error) {
	v, ok, err := e.store.GetConfig(ctx, summaryKey)
	if err != nil || !ok {
		return nil, err
	}
	var entries []summaryEntry
	if err := json.Unmarshal([]byte(v), &entries); err != nil {
		return nil, nil // corrupt buffer: drop rather than wedge
	}
	return entries, nil
}

// flushSummary coalesces quiet-hours events into one message: a
// raise+recovery pair for the same incident folds to a single line.
func (e *Engine) flushSummary(ctx context.Context, cfg Config, now time.Time) error {
	entries, err := e.loadSummary(ctx)
	if err != nil || len(entries) == 0 {
		return err
	}
	type pair struct {
		raise   *summaryEntry
		recover *summaryEntry
	}
	order := []string{}
	pairs := map[string]*pair{}
	for i := range entries {
		en := entries[i]
		k := en.Event + "|" + en.Subject
		p, seen := pairs[k]
		if !seen {
			p = &pair{}
			pairs[k] = p
			order = append(order, k)
		}
		if en.Recovery {
			p.recover = &en
		} else if p.raise == nil {
			p.raise = &en
		}
	}
	fmtT := func(ms int64) string { return time.UnixMilli(ms).Format("15:04") }
	lines := []string{}
	for _, k := range order {
		p := pairs[k]
		switch {
		case p.raise != nil && p.recover != nil:
			lines = append(lines, fmt.Sprintf("%s %s — recovered %s", fmtT(p.raise.Ts), p.raise.Msg, fmtT(p.recover.Ts)))
		case p.raise != nil:
			lines = append(lines, fmt.Sprintf("%s %s — STILL ACTIVE", fmtT(p.raise.Ts), p.raise.Msg))
		case p.recover != nil:
			lines = append(lines, fmt.Sprintf("%s %s", fmtT(p.recover.Ts), p.recover.Msg))
		}
	}
	sort.Strings(lines) // chronological — lines start with HH:MM
	title := fmt.Sprintf("hoptrail: %d alert(s) during quiet hours", len(order))
	if err := e.store.EnqueueAlert(ctx, sanitizeLatin1(title), strings.Join(lines, "\n"), "default", now); err != nil {
		return err
	}
	return e.store.DeleteConfig(ctx, summaryKey)
}

// nopWriter mirrors the retention package's discard logger shim
// (slog.DiscardHandler needs go1.24; the module is go1.22).
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// sanitizeLatin1 replaces runes outside latin-1 — ntfy transmits the
// title as an HTTP header, which rejects anything beyond (the
// other-project lesson: an em-dash in a title broke delivery).
func sanitizeLatin1(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r > 0xFF {
			b.WriteByte('?')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
