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

## [0.7.4] — 2026-07-31

**Settings got a full redesign: one page per category.** The gear used
to open a narrow slide-out where every section hid behind a collapsible
header — usable, but cluttered as sections accumulated. It now opens a
proper settings window: categories down the left (Bandwidth, Probes,
Alerts, Data retention, System, Updates, About), each with its own full
page and a one-line description of what lives there. Links like "Enable
in settings" land directly on the relevant page, and Documentation and
the theme control keep their spots in the navigation. Changes still
apply immediately — there is no save button.

**Multi-gig lines can now set a "Min valid throughput" above 1000 Mbps.**
The field was capped at 1000 — a gigabit-era assumption that rejected
any floor a 2.5G/5G/10G connection would want, with
`health_check_floor_mbps: 2000 outside 1-1000`. The ceiling is now
100 Gbps; it still catches typos and garbage, it just no longer
pretends gigabit is the fastest link that exists. Note this field marks
a result *invalid/junk* (a failed test reporting 3 Mbps), not "good" —
to alert on a real slowdown, tune the derate threshold, not this floor.
*Under the hood:* the bound lived in two places that had to move
together — `bandwidth` config validation and the settings input's
`max` — plus validation tests that a real multi-gig floor (1500 / 2300
/ 10000 Mbps) passes and an absurd one (>100 Gbps) still fails.

**The "Host not allowed" error now names the right setting.** If you
reach the UI through a reverse proxy or a DNS name, the anti-rebinding
guard blocks state-changing requests (like "Check for updates") until
you allowlist that hostname — but the error told you to add it to
`ui.allowed_hosts`, which does not exist. The setting is the top-level
`allowed_hosts` in `config.yaml`. Anyone who followed the old message
got the same 403 back with no indication their edit had done nothing.
*Under the hood:* YAML silently ignores unknown keys, so a `ui:` block
parsed cleanly and was discarded; the message now names the real key,
and a test pins both that it names `allowed_hosts` and that it never
regresses to the phantom path. The guard also logs a warning with the
rejected Host, method, path, and remote address — the journal now
answers "which hostname do I need to allowlist" directly instead of
requiring referer archaeology in the access log.

## [0.7.3] — 2026-06-15

**Bandwidth baseline on the status card.** The status overlay's
Bandwidth tile now shows the rolling **baseline** it derates against —
down/up Mbps — so you can see what "normal" is, not just whether
you're below it. Reads "establishing…" until there are enough
successful tests (7) to set a baseline. *Under the hood:* the same
`ComputeBandwidthBaseline` the chart annotation and derate detection
already use, surfaced in `/api/status`.

**Disk and database-growth monitoring.** The central now watches its
own disk: the status card's Database tile shows free space, how fast
the database is growing (MB/day), and a **headroom** ratio that
projects whether it's on track to fill the volume at the current
retention — and a new **low disk / DB growth** alert (on by default,
high priority) pings your phone *before* the disk fills, not after.
Two triggers: free space dropping below a floor (the larger of 1 GB or
5% of the volume), or the growth projection eating your headroom
(below 1.2×). Thresholds are tunable under Settings → Alerts, and a
critical disk now also lights the top-bar health dot red even with
alerting off. *Under the hood:* a new dependency-free `internal/
capacity` package measures free/total disk via `statfs` and reads an
hourly database-size series (recorded by the retention worker, pruned
to 14 days) to fit the growth slope; the alert rides the existing
engine as one more provider — same sustain/cooldown/quiet-hours/paired-
recovery pipeline — with recovery hysteresis so a value hovering at the
line doesn't flap. Migration v20 adds the `db_size_samples` table.

**Roomier probe rows.** Registered probes now show the name and
controls on one line with the IP, version, and last-seen on a
full-width line beneath — no more truncated version strings.

**One-click central updates.** Checking for updates and applying them
is now a single flow: when a newer release is found, one **Download &
apply vX.Y.Z** button downloads it, verifies the checksum, swaps it in,
and restarts — no more downloading and then hunting down a separate
Apply button. Updating from a self-built local binary moved into a
collapsed "Update from a local binary instead" section so the common
path is front and center.

**Smoother probe-update feedback.** Starting a probe update no longer
briefly flashes "update available" between clicking and the update
showing as in progress — the row now goes straight from
"preparing…" to "updating…" to current.

## [0.7.2] — 2026-06-12

**Probe updates: no more false "failed".** A central-driven probe
update could report failure even though the probe successfully updated
— the probe's final restart terminates the probe itself, and that
expected death was mistaken for an error. Fixed: the restart is now
treated as best-effort (the central confirms success from the next
heartbeat), a probe reporting the target version self-heals a prior
false failure, and the Update button shows "preparing update…"
immediately instead of sitting silent while the central downloads the
release binary.

**Theme control moved to the foot of the gear menu** as a single
Auto / Light / Dark segmented strip — always visible when you open
settings, no longer tucked inside the System section.

**Expand all / Collapse all** button in the settings header — open or
close every section at once.

## [0.7.1] — 2026-06-12

**Security hardening (pre-disclosure audit).** A focused adversarial
audit of the now-public codebase found that the unauthenticated-by-
design API lacked cross-origin protection — a malicious page in the
operator's LAN browser could forge state-changing requests (CSRF),
the worst case being the binary-upload endpoint, which also executed
the upload while merely staging it. Fixed:
- **Cross-origin guard** on every mutating endpoint: same-origin +
  a custom header a drive-by page can't set, plus a `Host` allowlist
  against DNS rebinding (loopback/IP/`.local`/single-label always
  allowed; add proxy or public hostnames to the new `allowed_hosts`
  config). Reads and the bearer-authed probe-ingest surface are exempt.
- Staging an uploaded binary **no longer executes it** (the version
  probe moved to apply only, run with a clean environment).
- The ntfy **token is no longer returned** by the config endpoint.
- Probe **tokens are bound to their probe** — a token can't push or
  sabotage another probe's data.
- Plus: integer-safe clock-skew check, markdown link-scheme allowlist,
  owner-only database file/dir (0600/0700 + `UMask=0077`), validated
  path-snapshot ingest, bounded export window, and HTTP read/idle
  timeouts.
No operator action needed for direct-LAN installs; reverse-proxy
setups add their hostname to `allowed_hosts`.

## [0.7.0] — 2026-06-12

**The Update button updates the probe.** No more pasting commands on
remote hosts: when a probe runs an older release, clicking **Update**
makes the central download the release binary for that probe's
architecture (sha256-verified against the GitHub checksums), hand it
to the probe on its next heartbeat, and the probe verifies it again,
swaps itself with a backup kept, and restarts — wrong-architecture or
too-old-glibc binaries are refused before anything changes, and a
failed capability grant rolls back automatically. **Update all**
walks the fleet one probe at a time and stops at the first failure;
a 📌 pin keeps any probe out of fleet updates. Failures land in the
alert history with the reason. Probes too old to understand the
command keep the manual one-liner path — one last paste enables the
button forever. *Under the hood:* the command rides the heartbeat
reply like the target set does, the binary travels the bearer-token
ingest surface (probes need no GitHub egress), and the probe applies
in-process — mirroring the central's own updater, because update.sh
inside the unit's cgroup would be killed by its own restart. See
docs/probe-update-design.md.

**Copy buttons work on plain-http dashboards.** The browser clipboard
API only exists on https/localhost, so on a typical LAN install every
Copy button quietly did nothing. They now fall back to the legacy
copy path that works everywhere, show "Copied ✓" only when the text
actually landed, and tell you to select manually in the rare case
neither path works.

## [0.6.1] — 2026-06-12

**The installer has a real address.** The one-line install is now
`curl -fsSL https://get.hoptrail.net | bash` — a 301 on the project's
domain pointing at the same `get.sh` in this repository, so the
command is easier to remember and the script's home never moves out
from under it. The README, docs, and the UI's generated probe
commands all use it.

**Sections resize vertically.** Each dashboard section's bottom edge
is now a height handle: drag to shorten or grow it (charts resize to
follow), double-click to restore the natural height. Shorten the
latency timeline while you study the hop list, then put it back.
Heights live in the same server-side layout as order, dock, and the
dock-width splitter — one arrangement, every browser.

**Outdated probes announce themselves.** Probes already report their
version on every heartbeat; now the Probes panel and the status page
compare it against the central's release and flag stragglers — an
**update available** chip plus an **Update** button that opens a
step-by-step popout (one pasted command, config preserved, probe back
within a heartbeat). Release-version comparison only, so a central on
a dev build doesn't nag probes running the same release.

## [0.6.0] — 2026-06-12

**The dashboard can ring.** A sound master switch in the Alerts
settings, with per-event toggles beneath it: raises play a two-note
attention tone, recoveries a softer all-clear, straight from the
browser — no audio files, no extra setup. The policy is shared
server-side like every other setting; each browser arms itself with
its first click (the autoplay rule), and ♪ preview buttons let you
hear both tones. *Under the hood:* the trigger rides the existing
status poll's `latest_history_id`, WebAudio synthesizes the tones, and
a per-browser high-water mark stops reloads from replaying history.

**Install with one line, update from releases.** A new `get.sh` makes
`curl -fsSL …/get.sh | bash` the install path for centrals and probes
alike: it downloads the latest release's prebuilt binary for your
architecture (amd64/arm64), verifies it against the release's sha256
checksums, and runs the same installer — no Go or Node toolchain on
the box. The Probes panel's generated install command uses it too.
And the Updates panel now talks to GitHub releases directly: **Check
for updates** → **Download** → **Apply**, with a background check on
your cadence (daily / weekly / monthly, default monthly, or never)
that announces a new version on the status page but never installs
anything on its own. *Under the hood:* downloads stage the exact slot
the upload mode and `update.sh --staged` already consume, so apply
stays one gated code path; the new `internal/release` package keeps
GitHub-facing logic dependency-free and fully fake-able in tests.

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
