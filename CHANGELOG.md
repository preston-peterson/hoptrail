# Changelog

All notable changes to Hoptrail are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
follows [Semantic Versioning](https://semver.org/).

## How to read this changelog

Each entry opens with a **bold lead** that names what changed in plain language —
what an operator would see, in vocabulary an operator uses. The first paragraph
stays in user voice: what's new, how to use it, where it appears, what to watch
for. An "Under the hood" transition then hands off to dev voice — design
pivots and technical detail useful to someone reading the code.

Skim the bolds for an upgrade summary; read the first paragraph of any entry
that catches your eye; continue into the dev paragraph only if you want the
implementation story.

## [0.5.0] — 2026-06-12

**Alerting.** Hoptrail can now push notifications to your phone through
[ntfy](https://ntfy.sh): a remote probe going offline (and recovering),
sustained loss on a target, latency held above a tab's threshold line, and
bandwidth derates — every alert paired with a recovery message. Setup lives in
the settings panel: install a local ntfy server with one button or point at
one you already run, send a test notification, and follow the phone setup
guide. Quiet hours roll overnight alerts into a single summary message.
*Under the hood:* a sustain-before-raise state machine persisted across
restarts, per-incident cooldowns, a global rate limit that exempts
recoveries, and a durable SQLite delivery queue — an ntfy outage delays
notifications, never loses them.

**Everything is in the web UI now — installers stay dead simple.** Remote
probes are added from the settings panel (type a name, paste one generated
command on the remote host — no config editing, no restart); the daemon
updates itself from an uploaded binary; the speedtest CLI and a local ntfy
server install from buttons; listen address, log level, reverse-DNS,
retention, and theme are all settings-panel controls, with a restart button
to match. *Under the hood:* a narrowly-scoped `/etc/sudoers.d/hoptrail`
whitelist (exact commands, no wildcards, version-marked so updates detect
drift) lets the UI drive the few root-requiring actions; probe bearer
tokens moved from yaml into SQLite.

**Arrange the dashboard your way.** Sections (latency timeline, bandwidth,
hops) drag to reorder, dock to either screen edge as a vertical column,
and collapse to slim bars. One layout, saved server-side, shared by every
browser.

**Route changes moved in with their hops.** Instead of a separate log you
had to cross-reference, a "route changes" button in the Hops header lights
up with a count when hops changed IP in the visible time window; toggled
on, each affected hop shows its changes inline (time, old IP → new IP).

**A status page and health dot.** A dot in the top bar distills the whole
environment (green/amber/red); clicking it opens a card grid — probe
engine, every probe with IP/version/last-heartbeat, database, alerts,
bandwidth, update state.

**Logs and docs without leaving the browser.** A live log viewer (level
filter, follow mode) under Settings → System, and this documentation set
embedded in the binary under the gear's Documentation entry — what you
read always matches what you run.

## [0.4.0] — 2026-06-10

**Bandwidth monitoring.** Scheduled or interval speed tests via the
operator-installed Ookla® Speedtest® CLI, charted alongside latency with
test windows marked on the timeline, and automatic derate detection against
a rolling baseline — including the asymmetric case where only upload
collapses. Tests are off by default and enabled from the settings panel,
which also discloses the data cost (~250 MB per test on gigabit).
*Under the hood:* a wall-clock scheduler that skips (not shifts) across
DST gaps, an injectable CLI runner, capability re-detection every 60s,
and per-test rows kept forever (they're tiny and long-term valuable).

## [0.3.0] — 2026-06-09

**Distributed probing.** Install lightweight probes at other sites; they
push measurements to the central over HTTP with automatic buffering
through outages, and every tab can display any probe's vantage point. The
diagnostic question shifts from *something is degrading* to *which leg is
degrading*. *Under the hood:* bearer-token ingest endpoints with
batch-level dedup (at-least-once delivery, transactional writes), a local
SQLite spill buffer on each probe that drains oldest-first on reconnect,
heartbeat-driven target-set propagation, and a ±24h clock-skew tripwire.

## [0.2.x] — 2026-06-08

**The investigation UI.** Multi-target tabs, hostname targets that
survive CDN rotation, historical scroll-back with wheel zoom, a brushable
focus window that recomputes hop statistics for past incidents, pinned
annotations, per-tab latency thresholds, loss attribution that separates
real packet loss from routers deprioritizing ICMP, ECMP-aware route-change
detection, diagnostic banners for known path patterns, and one-click JSON
export — the "send this to my ISP" artifact.

## [0.1.0] — 2026-05

**The core loop.** Continuous per-hop ICMP tracing (raw socket,
`cap_net_raw`, no root), SQLite/WAL storage built for one sample per hop
per second, and the embedded single-binary web UI with the hop table and
latency timeline.
