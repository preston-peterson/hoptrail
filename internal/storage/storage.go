// Package storage is hoptrail's SQLite persistence layer. It defines
// the on-disk schema, applies migrations, and provides a Sink
// implementation that buffers writes and flushes them in transactions
// to keep up with the probe engine's per-second sample rate.
//
// The package is intentionally narrow: it knows nothing about probes,
// network paths, or hops as concepts — it just receives Sample and
// RouteChange values via the probe.Sink interface and writes them.
// Read-side queries for the HTTP API land in step-8+.
//
// CGO and SQLite: this package uses mattn/go-sqlite3, which embeds the
// SQLite amalgamation and links it via CGO. Build hosts need a C
// toolchain (gcc, clang); the runtime binary is statically linked and
// needs nothing beyond glibc.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Registers the "sqlite3" driver with database/sql. The blank
	// import is intentional — we never call this package directly.
	_ "github.com/mattn/go-sqlite3"
)

// Store wraps a *sql.DB and provides the storage layer's lifecycle
// methods. It is safe for concurrent use (database/sql is); the Sink
// type below adds batching on top.
type Store struct {
	db *sql.DB
}

// DSN is the connection string format we use. WAL mode enables
// concurrent reads while writes are in progress (critical for the
// future HTTP server reading while the sink writes). busy_timeout
// turns short transient locks into waits rather than errors —
// 5 seconds is comfortably longer than any single transaction we
// expect to take.
// Step-69: foreign_keys=1 enables SQLite FK enforcement (off by default
// in mattn/go-sqlite3). Required for the cascade-on-target-delete
// semantic in the new tabs table (v9). Pre-v9 schema had no FKs, so
// turning this on is a no-op for existing data.
const dsnParams = "?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL&_foreign_keys=1"

// Open returns a Store ready to use. The database file is created if
// it doesn't exist, and its parent directory is created (recursively,
// mode 0755) if absent — so a fresh install can point at a path like
// /var/lib/hoptrail/hoptrail.db without an operator-side mkdir step.
// Pending migrations are applied automatically. The caller must call
// Close when finished.
//
// path may be ":memory:" for an ephemeral in-process database, useful
// in tests. Be aware that with the default database/sql connection
// pool, ":memory:" produces a different DB per connection — pass
// "file::memory:?cache=shared" if you need test code to share state
// across connections, or set MaxOpenConns(1) on the returned Store's
// underlying DB.
func Open(path string) (*Store, error) {
	if err := ensureParentDir(path); err != nil {
		return nil, err
	}

	dsn := path + dsnParams
	if path == ":memory:" {
		// In-memory databases are per-connection by default. Force a
		// single connection so tests using ":memory:" see consistent
		// state.
		dsn = path
	}

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: sql.Open(%q): %w", path, err)
	}

	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}

	// Ping to verify the connection works before returning a Store.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("storage: ping %q: %w", path, err)
	}

	// SECURITY (step-170, audit #16): the DB holds bearer tokens (probe
	// + ntfy) in plaintext. Tighten the file to owner-only so a local
	// non-service user can't read the secrets. Best-effort: a no-op for
	// :memory:, and chmod failure (e.g. an odd filesystem) is logged by
	// the caller path, not fatal.
	if path != ":memory:" && !strings.HasPrefix(path, "file::memory:") {
		_ = os.Chmod(path, 0o600)
	}

	s := &Store{db: db}
	if err := s.runMigrations(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// ensureParentDir creates the parent directory of a database file
// path, recursively if needed. Skipped for in-memory paths and for
// bare filenames in the current directory (where the parent already
// exists by definition). Mode 0755 — owner full access, others read
// and traverse, which matches the convention for /var/lib/<service>/
// data directories. The daemon's user must own the path it's pointed
// at; this method does not chown.
func ensureParentDir(path string) error {
	if path == ":memory:" || strings.HasPrefix(path, "file::memory:") {
		return nil
	}
	dir := filepath.Dir(path)
	if dir == "." || dir == "/" || dir == "" {
		// Current directory and filesystem root always exist; bare
		// filename in PWD needs no work.
		return nil
	}
	// 0700: the data dir holds the secrets-bearing DB (audit #16). Only
	// the service user needs in. MkdirAll won't tighten an existing dir;
	// install.sh creates /var/lib/hoptrail with the right mode.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("storage: create parent directory %q: %w", dir, err)
	}
	return nil
}

// Close releases the underlying database connections. Safe to call
// multiple times.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// DB returns the underlying *sql.DB. The HTTP read handlers in a later
// step will use this for queries; the sink uses it internally.
func (s *Store) DB() *sql.DB { return s.db }

// SchemaVersion returns the highest migration version applied to this
// database. Useful in tests and for diagnostic output. Returns 0 if no
// migrations have been applied yet.
func (s *Store) SchemaVersion() (int, error) {
	var v int
	row := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`)
	if err := row.Scan(&v); err != nil {
		return 0, fmt.Errorf("storage: read schema_version: %w", err)
	}
	return v, nil
}

// migration is one schema change. Migrations run in order of version,
// each in its own transaction together with the schema_version row
// that records it.
type migration struct {
	version int
	sql     string
}

// migrations is the ordered list of all migrations the storage layer
// has ever defined. Future schema changes append new entries here;
// existing entries are immutable. Never edit a past migration —
// existing databases have already applied it and won't re-run it.
var migrations = []migration{
	{
		version: 1,
		sql: `
CREATE TABLE samples (
    target TEXT NOT NULL,
    ttl INTEGER NOT NULL,
    ts INTEGER NOT NULL,
    ip TEXT,
    rtt_us INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_samples_query ON samples(target, ttl, ts);

CREATE TABLE route_changes (
    target TEXT NOT NULL,
    ttl INTEGER NOT NULL,
    ts INTEGER NOT NULL,
    old_ip TEXT,
    new_ip TEXT NOT NULL
);
CREATE INDEX idx_route_changes_query ON route_changes(target, ttl, ts);

CREATE TABLE rdns (
    ip TEXT NOT NULL PRIMARY KEY,
    hostname TEXT,
    resolved_at INTEGER NOT NULL
);
`,
	},
	{
		// Step-30: persistent recent-target history. Previously lived
		// in localStorage which is per-browser and lost on data-clear;
		// moving it to SQLite makes it durable across browsers,
		// machines, and incognito sessions. Single-row-per-target via
		// PRIMARY KEY so re-adding the same target just updates
		// last_added_at — no de-dup pass needed at read time.
		version: 2,
		sql: `
CREATE TABLE target_history (
    target TEXT NOT NULL PRIMARY KEY,
    last_added_at INTEGER NOT NULL
);
CREATE INDEX idx_target_history_recency ON target_history(last_added_at DESC);
`,
	},
	{
		// Step-32: persistent active-target set. The list of tabs the
		// daemon is currently monitoring used to live in yaml's
		// probe.target (single target, pre-step-26) and then in
		// supervisor memory only (multi-target, step-26+). yaml is a
		// poor fit for operationally-managed state (operators don't
		// want to edit it for routine changes) and in-memory means
		// every restart wiped the tab set. This table is the canonical
		// store; supervisor.Hydrate reads it on startup, and
		// supervisor.Add/Remove keep it in sync.
		version: 3,
		sql: `
CREATE TABLE active_targets (
    target TEXT NOT NULL PRIMARY KEY,
    added_at INTEGER NOT NULL
);
`,
	},
	{
		// Step-36: named target bundles ("WAN sanity", "ISP path", etc.)
		// that operators can load with one click to replace the
		// active tab set. Targets are stored as a JSON array since
		// bundles are always read/written as a unit and we never
		// query across bundles by target. SQLite handles small JSON
		// strings fine — no need for the JSON1 extension's operators.
		version: 4,
		sql: `
CREATE TABLE bundles (
    name TEXT NOT NULL PRIMARY KEY,
    created_at INTEGER NOT NULL,
    targets TEXT NOT NULL
);
`,
	},
	{
		// Step-37: per-target probe interval. Operators can dial the
		// per-hop pinger cadence per tab from the UI (fast for flaky
		// ISP-path tabs, slow for casual sanity tabs). NULL means
		// "fall back to cfg.Probe.Interval" — pre-migration rows and
		// any future add that doesn't specify an interval. Set via
		// SetActiveTargetInterval; the supervisor reads it on Hydrate
		// and applies it on every pipeline build / rebuild.
		version: 5,
		sql: `
ALTER TABLE active_targets ADD COLUMN interval_ms INTEGER;
`,
	},
	{
		// Step-39: per-target latency thresholds. Operators tune the
		// "warning" (yellow) and "critical" (red) RTT boundaries per
		// tab so a satellite-link tab and a LAN tab can each set a
		// reasonable visual budget. NULL on either column means "use
		// the daemon defaults" (cable-friendly 100/300). Pure display
		// metadata — never read by the probe engine itself, only by
		// the API + UI for chart band coloring.
		version: 6,
		sql: `
ALTER TABLE active_targets ADD COLUMN warning_ms INTEGER;
ALTER TABLE active_targets ADD COLUMN critical_ms INTEGER;
`,
	},
	{
		// Step-41: per-target final-hop-only mode. When set, the
		// pinger skips intermediate TTLs and only probes the
		// destination — drops outgoing probe traffic by ~95% on
		// long paths. Discovery still runs so route changes are
		// detected, but per-hop sample density is sacrificed.
		// Useful for casual-sanity tabs at scale (operator
		// monitoring 100+ targets). 0/NULL = off, 1 = on.
		version: 7,
		sql: `
ALTER TABLE active_targets ADD COLUMN final_hop_only INTEGER NOT NULL DEFAULT 0;
`,
	},
	{
		// Step-42: timeline annotations. Operator-typed notes pinned
		// to specific moments on the chart — "router reboot",
		// "ISP outage started", "called support" — so the chart's
		// raw data is paired with the operator's narration of what
		// happened. Without notes, a graph shipped weeks later to an
		// ISP or vendor doesn't communicate the story; with notes,
		// it does. Scoped per-target so each tab's chart shows only
		// its own notes. `ts` is unix ms (matches samples.ts). text
		// is the operator's note (untrusted; HTML-escape at render).
		// Notes are NOT pruned by retention — they're tiny and
		// historically significant well past the sample-data window.
		version: 8,
		sql: `
CREATE TABLE annotations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    target TEXT NOT NULL,
    ts INTEGER NOT NULL,
    text TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX idx_annotations_query ON annotations(target, ts);
`,
	},
	{
		// Step-69: multi-tab-per-target foundation. The `tabs` table
		// introduces a one-to-many between target (what's being probed)
		// and tab (the operator-visible view). Each tab carries its
		// own display-only state — label, thresholds, position — so
		// two tabs of the same target can show different threshold
		// reference lines, different labels, and live at different
		// positions in the tab bar without affecting probe traffic.
		//
		// Additive in this migration: active_targets stays unchanged
		// (existing supervisor + handlers + UI continue to read it).
		// Backfill creates exactly one tab per existing target,
		// inheriting that target's thresholds. Future migrations will
		// strip the now-redundant threshold columns from active_targets
		// and rename the table — but only after the frontend has
		// migrated to /api/tabs as its canonical source.
		//
		// tab_id is INTEGER PRIMARY KEY AUTOINCREMENT — small integer
		// handle suitable for URL paths (/api/tabs/<id>) without ULID
		// machinery. position is an INTEGER with the operator-defined
		// tab-bar order; the seed assigns positions in active_targets'
		// added_at order so the first load looks identical to today.
		version: 9,
		sql: `
CREATE TABLE tabs (
    tab_id      INTEGER PRIMARY KEY AUTOINCREMENT,
    target      TEXT NOT NULL,
    label       TEXT,
    warning_ms  INTEGER,
    critical_ms INTEGER,
    position    INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    FOREIGN KEY (target) REFERENCES active_targets(target) ON DELETE CASCADE
);
CREATE INDEX idx_tabs_target ON tabs(target);
CREATE INDEX idx_tabs_position ON tabs(position);

INSERT INTO tabs (target, label, warning_ms, critical_ms, position, created_at)
SELECT
    target,
    NULL,
    warning_ms,
    critical_ms,
    ROW_NUMBER() OVER (ORDER BY added_at) - 1,
    added_at
FROM active_targets;
`,
	},
	{
		// Step-71: bundles grow a nullable `tabs` JSON column that
		// carries the full per-tab display state (target, label,
		// thresholds). The existing `targets` column stays NOT NULL
		// for backward compat: when tabs is non-null, it's the
		// authoritative source; when tabs is NULL (legacy bundle
		// saved pre-step-71), the reader converts targets into
		// default-tab entries (Label nil, thresholds nil). New
		// bundles always write both columns so a downgrade still
		// reads them as target lists.
		version: 10,
		sql: `
ALTER TABLE bundles ADD COLUMN tabs TEXT;
`,
	},
	{
		// Step-88: v0.3 per-probe foundation (docs/v0.3-protocol-design.md
		// §4). Every sample and route change is now attributed to the
		// probe that produced it. 'local' is the reserved probe_id for
		// the central daemon's own on-host engine — the DEFAULT both
		// backfills existing v0.2 rows at migration time and covers the
		// in-process BatchedSink's inserts (which don't name the column),
		// so a zero-agent deploy behaves exactly as before. Remote-agent
		// ingest (a later step) names probe_id explicitly.
		//
		// The two query indexes are rebuilt to lead with probe_id; the
		// read handlers gain a probe_id filter in the same step so the
		// hot /api/samples poll keeps its covering index (SQLite has no
		// skip-scan — an index leading with probe_id is useless to a
		// query that only filters on target).
		//
		// probes: registered agents, upserted by heartbeat. Stale agents
		// (last_seen_at old) get marked offline in the UI; rows stay.
		// path_snapshots: most recent path per (probe, target),
		// overwritten on each ingest — current state, not a time-series.
		// ingest_log: batch_id dedup for at-least-once delivery; rows
		// older than 24h are pruned by the retention worker.
		version: 11,
		sql: `
ALTER TABLE samples       ADD COLUMN probe_id TEXT NOT NULL DEFAULT 'local';
ALTER TABLE route_changes ADD COLUMN probe_id TEXT NOT NULL DEFAULT 'local';

DROP INDEX idx_samples_query;
CREATE INDEX idx_samples_query ON samples(probe_id, target, ttl, ts);

DROP INDEX idx_route_changes_query;
CREATE INDEX idx_route_changes_query ON route_changes(probe_id, target, ttl, ts);

CREATE TABLE probes (
    probe_id      TEXT PRIMARY KEY,
    version       TEXT,
    started_at    INTEGER,
    last_seen_at  INTEGER NOT NULL,
    label         TEXT
);

CREATE TABLE path_snapshots (
    probe_id   TEXT NOT NULL,
    target     TEXT NOT NULL,
    ts         INTEGER NOT NULL,
    hop_count  INTEGER NOT NULL,
    target_ttl INTEGER NOT NULL,
    hops_json  TEXT NOT NULL,
    PRIMARY KEY (probe_id, target)
);

CREATE TABLE ingest_log (
    batch_id     TEXT PRIMARY KEY,
    probe_id     TEXT NOT NULL,
    received_at  INTEGER NOT NULL
);
CREATE INDEX idx_ingest_log_received ON ingest_log(received_at);
`,
	},
	{
		// Step-96: per-tab probe selection. Each tab displays one
		// probe's measurements (v0.3 design §10's "(probe, target)"
		// tab model, operator-requested after the two-host e2e made
		// it concrete: a "WAN from the cabin" tab pinned to a remote
		// probe next to a "WAN from home" tab on local). DEFAULT
		// 'local' backfills every existing tab with the v0.2-identical
		// behavior. Display-only — probe selection never touches the
		// central's own probe pipelines.
		version: 12,
		sql: `
ALTER TABLE tabs ADD COLUMN probe_id TEXT NOT NULL DEFAULT 'local';
`,
	},
	{
		// Step-98: v0.4 bandwidth-monitoring foundation
		// (docs/v0.4-bandwidth-design.md). Two pieces:
		//
		// config — the general key/value store the design's §7
		// persistence policy calls for ("every piece of bandwidth-
		// feature state lives in SQLite"); first use is the
		// bandwidth.* rows but future settings-panel sections store
		// here too. Values are TEXT, JSON-encoded where the logical
		// type needs it. Absent key = use the compiled default, so
		// the table starts empty and only operator overrides and
		// state flags ever get rows.
		//
		// bandwidth_samples — one row per speedtest run (ok or
		// failed; failures keep the chart honest about gaps).
		// derate_flag is computed at write time against the rolling
		// baseline so the banner query stays a cheap LIMIT 1. At 1-6
		// tests/day the table is a few hundred KB/year — never
		// retention-pruned.
		version: 13,
		sql: `
CREATE TABLE config (
    key   TEXT NOT NULL PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE bandwidth_samples (
    ts               INTEGER NOT NULL,
    down_mbps        REAL NOT NULL,
    up_mbps          REAL NOT NULL,
    ping_ms          REAL NOT NULL,
    bytes_down       INTEGER NOT NULL,
    bytes_up         INTEGER NOT NULL,
    duration_ms      INTEGER NOT NULL,
    server_id        INTEGER,
    server_name      TEXT,
    ok               INTEGER NOT NULL,
    error            TEXT,
    derate_flag      INTEGER NOT NULL,
    PRIMARY KEY (ts)
);
CREATE INDEX bandwidth_samples_ts_idx ON bandwidth_samples (ts DESC);
`,
	},
	{
		// Step-120: v0.5 probes-managed-in-the-UI. Bearer tokens for
		// the /api/ingest/* surface move from the yaml-only
		// probes.tokens list into this table so the settings panel can
		// mint and revoke them with no config edit and no restart.
		// The yaml list remains honored (auth = yaml ∪ table).
		//
		// token is the full secret, stored plaintext — the same trust
		// level as the yaml file it replaces and the rest of this DB.
		// name is the intended probe_id (kebab-validated at the API
		// layer), letting the operator correlate token ↔ probe in the
		// UI list. last_used_at is touched on heartbeat auth only, so
		// write churn stays at heartbeat cadence, not batch cadence.
		version: 14,
		sql: `
CREATE TABLE probe_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    token        TEXT NOT NULL UNIQUE,
    name         TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER
);
`,
	},
	{
		// Step-130: per-tab "show route changes inline in the hop
		// list" toggle. A display preference, but server-persisted so
		// it follows the operator across browsers (same rationale as
		// tabs.probe_id).
		version: 15,
		sql: `
ALTER TABLE tabs ADD COLUMN show_route_changes INTEGER NOT NULL DEFAULT 0;
`,
	},
	{
		// Step-134: v0.6 alerting (docs/design/v0.6-alerting-design.md
		// §3). alert_state persists each incident's state machine so a
		// daemon restart doesn't re-fire active alerts; alert_queue is
		// the persistent delivery queue — undelivered notifications
		// survive restarts and ntfy outages (the operator's "at least
		// I have the alerts eventually").
		version: 16,
		sql: `
CREATE TABLE alert_state (
    event_type  TEXT NOT NULL,
    subject     TEXT NOT NULL,
    state       TEXT NOT NULL,
    since       INTEGER NOT NULL,
    notified_at INTEGER,
    PRIMARY KEY (event_type, subject)
);
CREATE TABLE alert_queue (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at INTEGER NOT NULL,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL,
    priority   TEXT NOT NULL DEFAULT 'default',
    attempts   INTEGER NOT NULL DEFAULT 0
);
`,
	},
	{
		// Step-142: the probe's source address as seen by the central
		// on its last heartbeat — operator request: "for probes ...
		// anywhere they are shown we should include their IP too."
		version: 17,
		sql: `
ALTER TABLE probes ADD COLUMN last_ip TEXT;
`,
	},
	{
		// Step-149: append-only alert history (task #19 — "a running
		// list of alerts" in the UI). One row per raise/recovery the
		// engine accepted, independent of delivery mechanics (the
		// queue is "what was sent"; this is "what happened").
		version: 18,
		sql: `
CREATE TABLE alert_history (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ts         INTEGER NOT NULL,
    event_type TEXT NOT NULL,
    subject    TEXT NOT NULL,
    kind       TEXT NOT NULL,
    message    TEXT NOT NULL
);
CREATE INDEX idx_alert_history_ts ON alert_history(ts DESC);
`,
	},
	{
		// Step-168 (#22, central-driven probe updates): probes report
		// their architecture (needed to pick the release binary) and
		// can be pinned out of fleet updates; probe_updates is the
		// per-probe update command + its lifecycle. States:
		// pending (commanded, not yet acknowledged) → applying (probe
		// reported it started) → applied | failed. deliveries counts
		// how many heartbeat replies carried the command — a probe
		// that's seen it twice without acknowledging is too old to
		// understand it.
		version: 19,
		sql: `
ALTER TABLE probes ADD COLUMN arch TEXT;
ALTER TABLE probes ADD COLUMN pin INTEGER NOT NULL DEFAULT 0;
CREATE TABLE probe_updates (
    probe_id       TEXT PRIMARY KEY,
    target_version TEXT NOT NULL,
    arch           TEXT NOT NULL,
    sha256         TEXT NOT NULL,
    state          TEXT NOT NULL,
    error          TEXT NOT NULL DEFAULT '',
    deliveries     INTEGER NOT NULL DEFAULT 0,
    requested_at   INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);
`,
	},
	{
		// db_size_samples: an hourly time-series of the database file
		// size, written by the retention worker after each sweep. The
		// capacity monitor reads it to estimate growth (MB/day) and
		// project where the database lands at the current retention,
		// so it can warn before the disk fills rather than after. A
		// cache, not telemetry — pruned to a short fixed window.
		version: 20,
		sql: `
CREATE TABLE db_size_samples (
    ts    INTEGER NOT NULL,
    bytes INTEGER NOT NULL
);
CREATE INDEX idx_db_size_samples_ts ON db_size_samples(ts);
`,
	},
}

// runMigrations ensures the schema_version table exists, then applies
// any migration whose version is greater than the highest recorded.
// Each migration runs in its own transaction together with the
// schema_version row that records it — so a half-applied migration
// rolls back and is retried on next startup.
func (s *Store) runMigrations() error {
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER NOT NULL PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("storage: create schema_version: %w", err)
	}

	current, err := s.SchemaVersion()
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := s.applyMigration(m); err != nil {
			return err
		}
	}
	return nil
}

// RememberTarget records that `target` was added at this moment.
// Idempotent — re-adding the same target updates last_added_at so
// it floats to the top of RecentTargets. Called from the
// /api/targets POST handler after a successful supervisor.Add.
func (s *Store) RememberTarget(ctx context.Context, target string) error {
	if target == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO target_history (target, last_added_at)
		 VALUES (?, ?)
		 ON CONFLICT(target) DO UPDATE SET last_added_at = excluded.last_added_at`,
		target, time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("storage: remember target: %w", err)
	}
	return nil
}

// RecentTargets returns up to `limit` target strings, newest first
// by last_added_at. Used by the /api/target_history endpoint to
// populate the add-form dropdown across browsers.
func (s *Store) RecentTargets(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT target FROM target_history
		 ORDER BY last_added_at DESC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: recent targets: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0, limit)
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("storage: scan target: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: rows: %w", err)
	}
	return out, nil
}

// ActiveTarget is one row of the active_targets table. Pointer fields
// are nil when the operator hasn't picked a custom value — the
// supervisor / UI then falls back to its defaults. Step-37 added
// IntervalMs; step-39 added WarningMs + CriticalMs; step-41 added
// FinalHopOnly (a plain bool, not pointer — it has a NOT NULL DEFAULT
// 0 column so every row has a definite value). Pre-migration rows
// hydrate with nils + false and keep prior defaults.
type ActiveTarget struct {
	Target       string
	IntervalMs   *int64
	WarningMs    *int64
	CriticalMs   *int64
	FinalHopOnly bool
}

// ActiveTargets returns the persisted list of targets the supervisor
// should be monitoring. Called from supervisor.Hydrate at daemon
// startup. Order is insertion-time ascending (oldest first), which
// gives a stable tab order across restarts.
func (s *Store) ActiveTargets(ctx context.Context) ([]ActiveTarget, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT target, interval_ms, warning_ms, critical_ms, final_hop_only
		 FROM active_targets ORDER BY added_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("storage: active targets: %w", err)
	}
	defer rows.Close()
	out := []ActiveTarget{}
	for rows.Next() {
		var t string
		var ivl, warn, crit sql.NullInt64
		var finalHop int64
		if err := rows.Scan(&t, &ivl, &warn, &crit, &finalHop); err != nil {
			return nil, fmt.Errorf("storage: scan active target: %w", err)
		}
		at := ActiveTarget{Target: t, FinalHopOnly: finalHop != 0}
		if ivl.Valid {
			v := ivl.Int64
			at.IntervalMs = &v
		}
		if warn.Valid {
			v := warn.Int64
			at.WarningMs = &v
		}
		if crit.Valid {
			v := crit.Int64
			at.CriticalMs = &v
		}
		out = append(out, at)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: rows: %w", err)
	}
	return out, nil
}

// AddActiveTarget records that the supervisor is now monitoring
// `target`. Idempotent — re-adding refreshes the added_at without
// failing or touching the existing interval_ms. Called from
// supervisor.Add on success; the interval defaults to NULL (fall back
// to cfg.Probe.Interval) and the operator can promote it later via
// SetActiveTargetInterval.
func (s *Store) AddActiveTarget(ctx context.Context, target string) error {
	if target == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO active_targets (target, added_at)
		 VALUES (?, ?)
		 ON CONFLICT(target) DO UPDATE SET added_at = excluded.added_at`,
		target, time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("storage: add active target: %w", err)
	}
	return nil
}

// SetActiveTargetInterval updates the persisted per-target probe
// interval. intervalMs nil clears the override (fall back to
// cfg.Probe.Interval). The target must already be an active row;
// missing rows produce a "not active" error so callers don't
// silently no-op against a typo. Range-checking is the caller's job
// — the storage layer takes whatever int64 it's given.
func (s *Store) SetActiveTargetInterval(ctx context.Context, target string, intervalMs *int64) error {
	if target == "" {
		return fmt.Errorf("storage: set interval: target must not be empty")
	}
	var (
		res sql.Result
		err error
	)
	if intervalMs == nil {
		res, err = s.db.ExecContext(ctx,
			`UPDATE active_targets SET interval_ms = NULL WHERE target = ?`,
			target,
		)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE active_targets SET interval_ms = ? WHERE target = ?`,
			*intervalMs, target,
		)
	}
	if err != nil {
		return fmt.Errorf("storage: set interval: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: set interval: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("storage: set interval: target %q is not active", target)
	}
	return nil
}

// SetActiveTargetThresholds updates the persisted per-target latency
// thresholds — the green→yellow and yellow→red breakpoints used by
// the UI's chart band coloring. Either pointer being nil clears that
// override (the UI then falls back to the global default). The
// target must already be an active row; missing rows produce a
// "not active" error so callers don't silently no-op against a typo.
// Range-checking + ordering (warning < critical) is the caller's
// job; storage takes whatever int64s it's given.
func (s *Store) SetActiveTargetThresholds(ctx context.Context, target string, warningMs, criticalMs *int64) error {
	if target == "" {
		return fmt.Errorf("storage: set thresholds: target must not be empty")
	}
	var (
		warnArg any
		critArg any
	)
	if warningMs == nil {
		warnArg = nil
	} else {
		warnArg = *warningMs
	}
	if criticalMs == nil {
		critArg = nil
	} else {
		critArg = *criticalMs
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE active_targets SET warning_ms = ?, critical_ms = ? WHERE target = ?`,
		warnArg, critArg, target,
	)
	if err != nil {
		return fmt.Errorf("storage: set thresholds: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: set thresholds: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("storage: set thresholds: target %q is not active", target)
	}
	return nil
}

// SetActiveTargetFinalHopOnly updates the per-target final-hop-only
// flag (step-41). Same pattern as the other Set methods: requires
// the row to exist (errors on typo) and writes through one column.
func (s *Store) SetActiveTargetFinalHopOnly(ctx context.Context, target string, finalHopOnly bool) error {
	if target == "" {
		return fmt.Errorf("storage: set final_hop_only: target must not be empty")
	}
	v := int64(0)
	if finalHopOnly {
		v = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE active_targets SET final_hop_only = ? WHERE target = ?`,
		v, target,
	)
	if err != nil {
		return fmt.Errorf("storage: set final_hop_only: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: set final_hop_only: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("storage: set final_hop_only: target %q is not active", target)
	}
	return nil
}

// RemoveActiveTarget removes `target` from the active set. Safe to
// call for a target that isn't present (no-op). Called from
// supervisor.Remove on success.
func (s *Store) RemoveActiveTarget(ctx context.Context, target string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM active_targets WHERE target = ?`, target)
	if err != nil {
		return fmt.Errorf("storage: remove active target: %w", err)
	}
	return nil
}

// Bundle is a named preset operators can load with one click. Tabs
// carries the full per-tab display state (the step-71 shape); Targets
// is the legacy-shape mirror retained so a downgrade can still read
// the bundle. When loaded from disk, the two are always consistent
// (Targets is derived from Tabs).
type Bundle struct {
	Name      string
	CreatedAt int64
	Targets   []string
	Tabs      []BundleTab
}

// BundleTab is one entry inside a Bundle — a target plus its
// display-only state. All fields except Target are nullable, mapping
// to the existing tabs(label, warning_ms, critical_ms) columns'
// "unset" semantic. JSON tags match the API's JSON shape so the
// stored on-disk JSON is wire-compatible with /api/bundles responses.
type BundleTab struct {
	Target     string  `json:"target"`
	Label      *string `json:"label,omitempty"`
	WarningMs  *int64  `json:"warning_ms,omitempty"`
	CriticalMs *int64  `json:"critical_ms,omitempty"`
	// ProbeID rides the bundle's JSON column verbatim (step-96).
	// Empty/absent = local, so pre-step-96 bundles load unchanged.
	ProbeID string `json:"probe_id,omitempty"`
}

// SaveBundle creates or replaces the bundle named `name`. Always
// writes both `targets` (legacy column) and `tabs` (new column);
// Targets is derived from Tabs so a downgrade still reads the bundle
// as a target list. Empty tab list is allowed (a saved "blank"
// preset).
func (s *Store) SaveBundle(ctx context.Context, name string, tabs []BundleTab) error {
	if name == "" {
		return fmt.Errorf("storage: bundle name must not be empty")
	}
	if tabs == nil {
		tabs = []BundleTab{}
	}
	targets := make([]string, 0, len(tabs))
	for _, t := range tabs {
		targets = append(targets, t.Target)
	}
	targetsJSON, err := json.Marshal(targets)
	if err != nil {
		return fmt.Errorf("storage: encode bundle targets: %w", err)
	}
	tabsJSON, err := json.Marshal(tabs)
	if err != nil {
		return fmt.Errorf("storage: encode bundle tabs: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO bundles (name, created_at, targets, tabs)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   created_at = excluded.created_at,
		   targets = excluded.targets,
		   tabs = excluded.tabs`,
		name, time.Now().UnixMilli(), string(targetsJSON), string(tabsJSON),
	)
	if err != nil {
		return fmt.Errorf("storage: save bundle: %w", err)
	}
	return nil
}

// ListBundles returns every bundle ordered by created_at descending
// (newest first). Legacy bundles (tabs column is NULL — saved before
// step-71) are decoded from the targets column with each target
// turned into a default tab entry (Label nil, thresholds nil).
func (s *Store) ListBundles(ctx context.Context) ([]Bundle, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, created_at, targets, tabs FROM bundles ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("storage: list bundles: %w", err)
	}
	defer rows.Close()
	out := []Bundle{}
	for rows.Next() {
		var b Bundle
		var targets string
		var tabs sql.NullString
		if err := rows.Scan(&b.Name, &b.CreatedAt, &targets, &tabs); err != nil {
			return nil, fmt.Errorf("storage: scan bundle: %w", err)
		}
		if err := json.Unmarshal([]byte(targets), &b.Targets); err != nil {
			return nil, fmt.Errorf("storage: decode bundle %q targets: %w", b.Name, err)
		}
		if b.Targets == nil {
			b.Targets = []string{}
		}
		if tabs.Valid && tabs.String != "" {
			if err := json.Unmarshal([]byte(tabs.String), &b.Tabs); err != nil {
				return nil, fmt.Errorf("storage: decode bundle %q tabs: %w", b.Name, err)
			}
		}
		if b.Tabs == nil {
			// Legacy bundle — synthesize default tabs from the target list.
			b.Tabs = make([]BundleTab, 0, len(b.Targets))
			for _, t := range b.Targets {
				b.Tabs = append(b.Tabs, BundleTab{Target: t})
			}
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: rows: %w", err)
	}
	return out, nil
}

// DeleteBundle removes the bundle by name. Safe to call for a
// non-existent bundle (no-op).
func (s *Store) DeleteBundle(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM bundles WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("storage: delete bundle: %w", err)
	}
	return nil
}

// Annotation is a single operator-typed note pinned to a moment on
// a target's timeline (step-42). ID is server-assigned (autoincrement).
// Ts is the timestamp the note refers to in unix ms (matches samples.ts).
// CreatedAt is when the operator added the note.
type Annotation struct {
	ID        int64
	Target    string
	Ts        int64
	Text      string
	CreatedAt int64
}

// AddAnnotation inserts a new note for `target` at `ts`. text is
// stored as-given; the API layer caps length and HTML-escapes at
// render. Returns the inserted row's assigned ID so the caller can
// surface it to the UI for optimistic-add-then-reconcile.
func (s *Store) AddAnnotation(ctx context.Context, target string, ts int64, text string) (int64, error) {
	if target == "" {
		return 0, fmt.Errorf("storage: add annotation: target must not be empty")
	}
	if text == "" {
		return 0, fmt.Errorf("storage: add annotation: text must not be empty")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO annotations (target, ts, text, created_at) VALUES (?, ?, ?, ?)`,
		target, ts, text, time.Now().UnixMilli(),
	)
	if err != nil {
		return 0, fmt.Errorf("storage: add annotation: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("storage: add annotation: last insert id: %w", err)
	}
	return id, nil
}

// ListAnnotations returns annotations for `target` in [since, until]
// (inclusive, unix ms). The window matches how the chart polls
// samples, so the same set of bounds Wikipedia the visible chart
// window and the annotations layered on it. Order is ts ascending —
// stable for keyed-each renders. since == 0 means "from the start
// of time"; until == 0 means "until now."
func (s *Store) ListAnnotations(ctx context.Context, target string, since, until int64) ([]Annotation, error) {
	if target == "" {
		return nil, fmt.Errorf("storage: list annotations: target must not be empty")
	}
	if until == 0 {
		until = time.Now().UnixMilli()
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, target, ts, text, created_at
		 FROM annotations
		 WHERE target = ? AND ts >= ? AND ts <= ?
		 ORDER BY ts ASC`,
		target, since, until,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: list annotations: %w", err)
	}
	defer rows.Close()
	out := []Annotation{}
	for rows.Next() {
		var a Annotation
		if err := rows.Scan(&a.ID, &a.Target, &a.Ts, &a.Text, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("storage: scan annotation: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: rows: %w", err)
	}
	return out, nil
}

// DeleteAnnotation removes a single note by ID. No-op when the ID
// doesn't exist (matches the rest of the storage layer's "deletion
// is idempotent" pattern).
func (s *Store) DeleteAnnotation(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM annotations WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("storage: delete annotation: %w", err)
	}
	return nil
}

// Step-75: ClearRouteChanges wipes the route_changes log for a given
// target. Operator-initiated: invoked when the panel's "clear" button
// is clicked after a noisy flap that's no longer worth seeing. Empty
// target means "all targets" — kept narrow to per-target use by the
// handler, which always passes a non-empty target.
func (s *Store) ClearRouteChanges(ctx context.Context, target string) error {
	if target == "" {
		return fmt.Errorf("storage: clear route_changes: target must not be empty")
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM route_changes WHERE target = ?`, target)
	if err != nil {
		return fmt.Errorf("storage: clear route_changes: %w", err)
	}
	return nil
}

// ----- step-69: tabs (multi-tab-per-target foundation) -----
//
// Tab is the operator-visible view of a probed target. Two tabs can
// share one target; the supervisor's pipeline-per-target shape is
// unchanged (display-only divergence between tabs of the same target).
// See docs/multi-tab-per-target-design.md.
//
// WarningMs / CriticalMs are pointers so they can express "unset"
// (fall back to daemon default) distinctly from any concrete value.
// Label is similarly nullable: NULL means "render the target string."
type Tab struct {
	TabID      int64
	Target     string
	Label      *string
	WarningMs  *int64
	CriticalMs *int64
	Position   int64
	CreatedAt  int64
	// ProbeID is whose measurements this tab displays (step-96).
	// Always non-empty; 'local' is the default and the v0.2 behavior.
	ProbeID string
	// ShowRouteChanges is the per-tab inline-route-changes toggle
	// (step-130) — display state, server-persisted so it follows the
	// operator across browsers.
	ShowRouteChanges bool
}

// ListTabs returns every tab ordered by position ASC then tab_id ASC
// (stable secondary sort for tabs at the same position — shouldn't
// happen, but defensive).
func (s *Store) ListTabs(ctx context.Context) ([]Tab, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tab_id, target, label, warning_ms, critical_ms, position, created_at, probe_id, show_route_changes
		   FROM tabs
		   ORDER BY position ASC, tab_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("storage: list tabs: %w", err)
	}
	defer rows.Close()

	out := []Tab{}
	for rows.Next() {
		var t Tab
		var label, warn, crit sql.NullString
		var warnI, critI sql.NullInt64
		var showRC int64
		if err := rows.Scan(&t.TabID, &t.Target, &label, &warnI, &critI, &t.Position, &t.CreatedAt, &t.ProbeID, &showRC); err != nil {
			_ = warn // unused; keeping the parallel structure visible
			_ = crit
			return nil, fmt.Errorf("storage: scan tab: %w", err)
		}
		if label.Valid {
			val := label.String
			t.Label = &val
		}
		if warnI.Valid {
			v := warnI.Int64
			t.WarningMs = &v
		}
		if critI.Valid {
			v := critI.Int64
			t.CriticalMs = &v
		}
		t.ShowRouteChanges = showRC != 0
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: tabs rows: %w", err)
	}
	return out, nil
}

// CreateTab inserts a new tab pointing at `target` and returns the
// assigned tab_id. Position is appended (max+1). The target must
// already exist in active_targets — the FK will reject otherwise.
func (s *Store) CreateTab(ctx context.Context, target string, label *string, warningMs, criticalMs *int64, probeID string) (int64, error) {
	if target == "" {
		return 0, fmt.Errorf("storage: create tab: target is required")
	}
	if probeID == "" {
		probeID = LocalProbeID
	}
	// Compute next position. New tabs land at the end of the bar.
	var maxPos sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(position) FROM tabs`).Scan(&maxPos); err != nil {
		return 0, fmt.Errorf("storage: max position: %w", err)
	}
	nextPos := int64(0)
	if maxPos.Valid {
		nextPos = maxPos.Int64 + 1
	}

	now := time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tabs (target, label, warning_ms, critical_ms, position, created_at, probe_id)
		   VALUES (?, ?, ?, ?, ?, ?, ?)`,
		target, nullableString(label), nullableInt(warningMs), nullableInt(criticalMs), nextPos, now, probeID,
	)
	if err != nil {
		return 0, fmt.Errorf("storage: insert tab: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("storage: tab last-insert-id: %w", err)
	}
	return id, nil
}

// UpdateTab applies a partial update. Nil pointers mean "don't touch
// that field"; explicit-but-zero pointers (e.g. setting label to "")
// mean "set it." Distinguishing "absent" from "explicit null" is
// handled at the API layer via jsonNullableInt — the storage method
// trusts the caller to have already resolved that.
//
// To clear a field (set back to NULL), pass a tristate that maps to
// SQL NULL. The clearLabel / clearThresholds bools handle that.
func (s *Store) UpdateTab(ctx context.Context, tabID int64, label *string, clearLabel bool, warningMs, criticalMs *int64, clearThresholds bool, probeID *string, showRouteChanges *bool) error {
	// Build a dynamic SET clause. Each field gets its own branch so
	// "nothing changed" produces a no-op (which is a 200, not a 304).
	parts := []string{}
	args := []any{}
	if probeID != nil {
		parts = append(parts, "probe_id = ?")
		args = append(args, *probeID)
	}
	if showRouteChanges != nil {
		parts = append(parts, "show_route_changes = ?")
		v := int64(0)
		if *showRouteChanges {
			v = 1
		}
		args = append(args, v)
	}
	if label != nil {
		parts = append(parts, "label = ?")
		args = append(args, *label)
	} else if clearLabel {
		parts = append(parts, "label = NULL")
	}
	if clearThresholds {
		parts = append(parts, "warning_ms = NULL", "critical_ms = NULL")
	} else {
		if warningMs != nil {
			parts = append(parts, "warning_ms = ?")
			args = append(args, *warningMs)
		}
		if criticalMs != nil {
			parts = append(parts, "critical_ms = ?")
			args = append(args, *criticalMs)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	args = append(args, tabID)
	q := "UPDATE tabs SET " + strings.Join(parts, ", ") + " WHERE tab_id = ?"
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("storage: update tab: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: update tab rows: %w", err)
	}
	if n == 0 {
		return ErrTabNotFound
	}
	return nil
}

// DeleteTab removes a single tab by ID. Returns ErrTabNotFound if no
// such tab exists. Caller is responsible for the "last-tab-cascades-
// to-target-delete" semantic — that logic lives one layer up because
// it involves the supervisor's pipeline teardown.
func (s *Store) DeleteTab(ctx context.Context, tabID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tabs WHERE tab_id = ?`, tabID)
	if err != nil {
		return fmt.Errorf("storage: delete tab: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: delete tab rows: %w", err)
	}
	if n == 0 {
		return ErrTabNotFound
	}
	return nil
}

// ReorderTabs applies a bulk position update. The `order` slice lists
// tab_ids in their new order; position N is set to slice index N. Any
// tabs not in the slice are left alone (positions un-shifted).
// Wrapped in a transaction so a partial reorder doesn't leave the bar
// in a mixed state. Unknown tab_ids in the slice are silently skipped
// — the caller is responsible for sending a sensible list.
func (s *Store) ReorderTabs(ctx context.Context, order []int64) error {
	if len(order) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: reorder begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for i, id := range order {
		if _, err := tx.ExecContext(ctx, `UPDATE tabs SET position = ? WHERE tab_id = ?`, int64(i), id); err != nil {
			return fmt.Errorf("storage: reorder set tab %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: reorder commit: %w", err)
	}
	return nil
}

// CountTabsForTarget reports how many tabs currently point at a given
// target. The handler uses this to implement the "last tab for a
// target cascades to target delete" semantic.
func (s *Store) CountTabsForTarget(ctx context.Context, target string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tabs WHERE target = ?`, target).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count tabs for target: %w", err)
	}
	return n, nil
}

// ErrTabNotFound is returned by UpdateTab / DeleteTab when the tab_id
// isn't in the table. Handlers map it to 404.
var ErrTabNotFound = fmt.Errorf("tab not found")

// Small helpers for nullable values — keep the INSERT/UPDATE call
// sites readable.
func nullableString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
func nullableInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// applyMigration runs one migration's SQL and records it in
// schema_version, both inside a transaction.
func (s *Store) applyMigration(m migration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: begin migration v%d tx: %w", m.version, err)
	}
	if _, err := tx.Exec(m.sql); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("storage: apply migration v%d: %w", m.version, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`,
		m.version, time.Now().UnixMilli(),
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("storage: record migration v%d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit migration v%d: %w", m.version, err)
	}
	return nil
}
