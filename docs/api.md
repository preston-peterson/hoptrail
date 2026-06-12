# Hoptrail HTTP API

Complete reference for the JSON API served by the central daemon
(`hoptrail serve`) alongside the embedded web UI. It supersedes the v0.1
document (`api-v0.1.md`). Endpoints are grouped: read endpoints,
target/tab management, bundles, annotations, probes + ingest (the
probe-to-central wire protocol), bandwidth, and misc. Operator-facing
context for these features lives in [user-guide.md](user-guide.md) and
[distributed-probing.md](distributed-probing.md).

## Conventions

- All request and response bodies are JSON; fields are snake_case.
- Timestamps are unix **milliseconds** (integers). RTTs are
  floating-point milliseconds on the wire.
- `null`-able IP/hostname fields signal "timed out" / "not resolved";
  the UI renders gaps and bare IPs accordingly.
- Responses always use concrete empty arrays/objects (`[]`, `{}`), never
  `null`, for collection fields.
- No authentication, **except** `/api/ingest/*`, which requires a bearer
  token. Hoptrail is a single-user tool for trusted networks; put a
  reverse proxy in front if you must expose it.
- Wrong method on any route → `405`. JSON responses carry
  `Cache-Control: no-store`.
- Two common query parameters on read endpoints:
  - `target` — the operator-typed target identifier (IP or hostname).
    May be omitted only when exactly one target is active (then it
    defaults to it); with zero active targets the request gets `503`,
    with several it gets `400`.
  - `probe_id` — which probe's data to read. Omitted or empty defaults
    to `local` (the central's own probe). Unknown ids → `404`;
    `probe_id=all` is reserved for a future merged view and currently →
    `400`.

---

## Read endpoints

### `GET /api/path`

Current state of the path to a target, as seen by one probe.

Query: `target`, `probe_id`.

```json
{
  "target": "8.8.8.8",
  "started_at": 1716412800123,
  "hop_count": 11,
  "target_ttl": 11,
  "hops": [
    {
      "ttl": 1,
      "current_ip": "192.168.1.1",
      "hostname": "router.lan",
      "current_rtt_ms": 0.4,
      "avg_rtt_ms": 0.5,
      "min_rtt_ms": 0.3,
      "loss_percent": 0.0,
      "loss_state": "ok",
      "last_response": 1716412803456
    }
  ]
}
```

- `target_ttl` — smallest TTL at which the destination replied; `0` if
  not yet reached. `hops` is capped at it once known.
- `current_ip` / `hostname` — `null` for anonymous hops / unresolved
  IPs.
- `current_rtt_ms` / `avg_rtt_ms` / `min_rtt_ms` — over the recent
  window (~5 min at 1 Hz), excluding timeouts; `0` when everything
  recent timed out.
- `loss_state` — `"ok"` | `"suspect"` | `"rate_limited"`. Server-side
  classification via the downstream-persistence rule; `loss_percent` is
  the raw number in all states.
- For a **remote probe** (`probe_id` ≠ `local`) the response is served
  from the probe's last reported snapshot and carries two extra fields:
  `probe_id` and `snapshot_ts` (when the probe took the snapshot — the
  staleness signal). `started_at` is the snapshot time.

Status: `200`; `400` (ambiguous target / `probe_id=all`); `404` (target
not monitored, unknown probe, or remote probe has no snapshot yet for
this target); `503` (no targets, or engine snapshot unavailable during
startup/shutdown).

### `GET /api/samples`

Historical per-probe samples for the latency chart.

Query: `target`, `probe_id`, `since` (default now−5m), `until` (default
now), `bucket_ms` (optional, ≥0; when positive the server downsamples to
one representative sample per (TTL, bucket) — the earliest in each
bucket).

```json
{
  "since": 1716412500000,
  "until": 1716412800000,
  "samples": [
    { "ttl": 1, "ts": 1716412800123, "ip": "192.168.1.1",  "rtt_ms": 0.4 },
    { "ttl": 2, "ts": 1716412800124, "ip": "203.0.113.10", "rtt_ms": 6.4 },
    { "ttl": 6, "ts": 1716412800126, "ip": null,           "rtt_ms": 0 }
  ]
}
```

Ordered by `ts` ascending, then `ttl`. Timeouts have `ip: null` and
`rtt_ms: 0`; distinguish by `ip === null`, not by the RTT.

Status: `200`; `400` (unparseable `since`/`until`/`bucket_ms`, negative
`bucket_ms`, ambiguous target); `404`/`503` as above.

### `GET /api/route_changes`

Recent route-change events, newest first.

Query: `target`, `probe_id`, `since` (optional; default all history),
`limit` (default 50, hard cap 500).

```json
{
  "since": null,
  "limit": 50,
  "changes": [
    { "ttl": 3, "ts": 1716412800123, "old_ip": "203.0.113.12", "new_ip": "203.0.113.45" },
    { "ttl": 6, "ts": 1716412700456, "old_ip": null,           "new_ip": "203.0.113.206" }
  ]
}
```

`old_ip: null` means a previously-anonymous hop gained an identity.

### `DELETE /api/route_changes?target=<id>`

Clears the route-change log for one target (no global wipe). → `204`.

### `GET /api/target_history`

Recently added target identifiers, newest first — feeds the add-form's
dropdown. Query: `limit` (default 10, cap 100). →
`{ "targets": ["8.8.8.8", "dns.google"] }`.

---

## Target and tab management

A **target** is a probe stream; a **tab** is a view of one. Probe-
affecting settings (interval, final-hop-only) live on targets; display
settings (label, thresholds, probe selection) live on tabs.

### `GET /api/targets`

```json
{
  "targets": ["8.8.8.8", "dns.google"],
  "intervals_ms": { "8.8.8.8": 1000, "dns.google": 2500 },
  "thresholds": { "8.8.8.8": { "warning_ms": null, "critical_ms": null } },
  "final_hop_only": { "8.8.8.8": false, "dns.google": true }
}
```

Per-target threshold fields are `null` when the operator hasn't
overridden (UI falls back to its default preset). Note: per-**tab**
thresholds from `/api/tabs` are what the UI actually paints; this map is
the legacy per-target layer.

### `POST /api/targets`

Body: `{ "target": "8.8.8.8" }` (IP or hostname; resolved at add time;
IPv4 only). → `200` `{ "target": "8.8.8.8" }`.

Status: `400` (empty, unresolvable, IPv6, not a valid traceroute
target); `409` (already monitored); `500` (pipeline build failure).

### `DELETE /api/targets/<id>`

Stops monitoring a target (the UI normally removes targets by closing
their last tab instead). → `200` `{ "target": ... }`; `404` if not
monitored.

### `PATCH /api/targets/<id>`

Partial update; at least one field required.

```json
{ "interval_ms": 2500, "warning_ms": 30, "critical_ms": 100, "final_hop_only": true }
```

- `interval_ms` — per-hop pinger cadence; positive, server-enforced
  bounds (200 ms – 60 s).
- `warning_ms` + `critical_ms` — must be sent **together**; both `null`
  clears the override; values must be positive with warning < critical.
- `final_hop_only` — boolean; triggers a pipeline rebuild.

→ `200` echoing the fields that changed. `400` on validation failure,
`404` unknown target.

### `GET /api/target` / `POST /api/target` (legacy)

Single-target back-compat surface. GET returns the only active target
(errors when zero/multiple are active); POST `{ "target": ... }`
*replaces* the whole active set with the one target (swap semantic).
Prefer `/api/targets` + `/api/tabs`.

### `GET /api/tabs`

Every tab, ordered by position:

```json
{
  "tabs": [
    {
      "tab_id": 1,
      "target": "8.8.8.8",
      "label": null,
      "warning_ms": 100,
      "critical_ms": 300,
      "position": 0,
      "created_at": 1716412800000,
      "probe_id": "local",
      "show_route_changes": false
    }
  ]
}
```

### `POST /api/tabs`

```json
{ "target": "8.8.8.8", "label": "1h view", "warning_ms": 30,
  "critical_ms": 100, "copy_from": 1, "probe_id": "site-east-pi" }
```

Only `target` is required. `copy_from` clones label/thresholds/probe
from an existing tab unless explicitly overridden; `probe_id` defaults
to `local` and must name a registered probe (`all` is rejected). The
target must already be monitored — otherwise `400` with guidance to
`POST /api/targets` first. → `200` with the full tab object.

### `PATCH /api/tabs/<id>`

Partial update; at least one of `label`, `warning_ms`+`critical_ms`,
`probe_id`, `show_route_changes`. `label: null` clears the label;
thresholds follow the same sent-together / both-`null`-clears rule as
the target PATCH; `probe_id` is plain-optional (switch to local is
`"local"`, never `null`); `show_route_changes` (bool) is the inline
route-changes toggle in the hops header — tab-persisted so it follows
the operator across browsers. → `200` with the updated tab; `400`
validation; `404` unknown tab.

### `PATCH /api/tabs/order`

Bulk reorder: `{ "order": [2, 1, 5, 3] }` (tab_ids in new position
order). → `204`.

### `DELETE /api/tabs/<id>`

Removes a tab. If it was the last tab for its target, the target is
removed too (pipeline torn down). → `204`; `404` unknown tab.

---

## Bundles

Named tab-set presets.

### `GET /api/bundles`

```json
{
  "bundles": [
    {
      "name": "debug-set",
      "created_at": 1716412800000,
      "targets": ["8.8.8.8", "dns.google"],
      "tabs": [
        { "target": "8.8.8.8", "label": "dns", "warning_ms": 30, "critical_ms": 100 },
        { "target": "dns.google" }
      ]
    }
  ]
}
```

`targets` is the bare legacy list; `tabs` is the full per-tab shape.

### `POST /api/bundles`

Body: `{ "name": "debug-set", "tabs": [ ... ] }` (or legacy
`"targets": [ ... ]`; `tabs` wins when both are present). Name ≤ 64
chars. Saving an existing name overwrites it. → `200` with the saved
bundle; `400` on missing/long name or a tab without a target.

### `DELETE /api/bundles/<name>` → `204`.

---

## Annotations

Operator notes pinned to timeline moments. Always target-scoped —
`target` is required, never defaulted.

### `GET /api/annotations?target=<id>&since=<ms>&until=<ms>`

`since`/`until` optional (0 = unbounded).

```json
{
  "annotations": [
    { "id": 7, "target": "8.8.8.8", "ts": 1716412800000,
      "text": "rebooted the router", "created_at": 1716412805000 }
  ]
}
```

### `POST /api/annotations`

Body: `{ "target": "8.8.8.8", "ts": 1716412800000, "text": "..." }`.
`text` ≤ 280 characters; `ts` must be positive. → `200` echoing the full
inserted row (with its `id`); `400` validation.

### `DELETE /api/annotations/<id>` → `204`.

---

## Probes + ingest

### `GET /api/probes`

Registered probes for the probe picker. `local` is synthesized first
(always online; carries the central's version).

```json
{
  "probes": [
    { "probe_id": "local", "label": null, "version": "v0.4.2",
      "online": true, "last_seen_at": null, "started_at": null },
    { "probe_id": "site-east-pi", "label": null, "version": "v0.4.2",
      "online": true, "last_seen_at": 1716412800000, "started_at": 1716400000000 }
  ]
}
```

`online` = heartbeat within 3× the default 60s interval.

### `DELETE /api/probes/<probe_id>` → `204`

Forgets a registered probe: its registration row and path snapshots
are deleted, and any tabs pointed at it are repointed to `local`. Its
samples age out via retention. Tokens are not touched — a probe whose
token is still valid re-registers on its next heartbeat, so revoke
first. `local` and `all` → `400`; unknown probe → `404`.

### `GET /api/probe-tokens`

Ingest bearer tokens minted from the UI (Settings → Probes). The full
token is never listed — only its first 4 characters.

```json
{
  "tokens": [
    { "id": 3, "name": "site-east-pi", "token_prefix": "kf3Q",
      "created_at": 1716412800000, "last_used_at": 1716412860000 }
  ]
}
```

`last_used_at` is stamped by heartbeat auth (null until the probe's
first beat). Yaml-configured tokens don't appear here.

### `POST /api/probe-tokens`

Body: `{ "name": "site-east-pi" }` — the intended probe_id (same shape
rules, `local`/`all` reserved). → `200` with
`{ "id": 3, "name": "site-east-pi", "token": "<43 chars>" }`. **The
only response that ever contains the full token.** `400` validation.

### `DELETE /api/probe-tokens/<id>` → `204`

Revokes the token; effective on the probe's next request (it 401s and
spills to its local buffer). Unknown id → `404`.

### `/api/ingest/*` — the probe-to-central wire protocol

These three POST endpoints are what a remote `hoptrail probe` process
speaks to its central. They are not meant for browsers or ad-hoc
clients, but they're plain HTTP+JSON and stable.

**Auth:** every request needs `Authorization: Bearer <token>` matching
the central's accepted set — the union of UI-minted tokens (the
`/api/probe-tokens` endpoints above) and the yaml `probes.tokens`
list. With no tokens configured the surface is disabled — everything
`401`s. Bodies are capped at 4 MiB.

**Response-code contract:** `4xx` tells the probe the batch is
config-shaped/unfixable (drop it and log); `5xx`, timeouts, and
connection failures mean retry with backoff. Duplicate batches are acked
`200` so a retry after a lost ack stops cleanly.

`probe_id` must match `^[a-z0-9][a-z0-9-]{1,31}$`; `local` and `all`
are reserved (→ `400`). Sample/route-change timestamps more than 24h
from the central's clock are rejected `400` (broken-NTP tripwire).

#### `POST /api/ingest/heartbeat`

Sent at startup and every `heartbeat_interval` (default 60s).

```json
{ "probe_id": "site-east-pi", "version": "v0.4.2",
  "started_at": 1716412800000, "targets": ["8.8.8.8"] }
```

→ `200`:

```json
{ "registered_at": 1716412800123, "central_target_set": ["8.8.8.8", "1.1.1.1"] }
```

`central_target_set` is authoritative — the probe reshapes its local
probing to it. Re-registration with the same `probe_id` is idempotent.

#### `POST /api/ingest/samples`

Sent every `ingest_interval` (default 5s) — and during partition
recovery, one buffered batch at a time.

```json
{
  "probe_id": "site-east-pi",
  "batch_id": "01HZX...",
  "samples": [
    { "target": "8.8.8.8", "ttl": 2, "ts": 1716412800124,
      "ip": "203.0.113.10", "rtt_ms": 6.4 }
  ],
  "route_changes": [
    { "target": "8.8.8.8", "ttl": 3, "ts": 1716412800123,
      "old_ip": "203.0.113.12", "new_ip": "203.0.113.45" }
  ]
}
```

`batch_id` (1–128 chars, client-generated, time-sortable, opaque to the
central) is the at-least-once dedup key. TTLs 1–64; `rtt_ms` ≥ 0;
`ip: null` = timeout. → `200`
`{ "received_at": ..., "batch_id": "<echoed>" }` — for fresh *and*
duplicate batches alike.

#### `POST /api/ingest/path`

Sent after each discovery sweep (default 30s); the central keeps only
the latest snapshot per (probe, target).

```json
{ "probe_id": "site-east-pi", "target": "8.8.8.8", "ts": 1716412800000,
  "hop_count": 11, "target_ttl": 11,
  "hops": [ { "ttl": 1, "current_ip": "192.168.1.1", "current_rtt_ms": 0.4,
              "avg_rtt_ms": 0.5, "min_rtt_ms": 0.3, "loss_percent": 0,
              "last_response": 1716412800000 } ] }
```

Hop elements are the `/api/path` hop shape minus `hostname` and
`loss_state`, which the central computes at read time. → `200`
`{ "received_at": ... }`.

---

## Bandwidth

### `GET /api/bandwidth/config`

Every tunable, the UI-state rows, and the capability snapshot:

```json
{
  "capability": { "available": true, "version": "1.2.0", "error": "" },
  "enabled": false,
  "cadence_mode": "times",
  "interval_minutes": 60,
  "scheduled_times": ["02:00"],
  "timezone": "America/Chicago",
  "directions": "both",
  "server_mode": "auto",
  "server_id": null,
  "derate_threshold": 0.5,
  "baseline_days": 7,
  "baseline_metric": "median",
  "health_check_floor_mbps": 10,
  "pause_icmp_during_test": true,
  "install_banner_dismissed_for_version": null,
  "derate_banner_dismissed_incident_ts": null,
  "run_in_flight": false
}
```

### `PATCH /api/bandwidth/config`

Body: any subset of the GET keys except `capability` and
`run_in_flight` (read-only). The composite result is validated, then
persisted and live-applied to the bandwidth engine — no restart. →
`204`; `400` on empty patch or invalid combination (e.g.
`server_mode: "pinned"` with no `server_id`).

### `GET /api/bandwidth/history?since=<ms>&until=<ms>`

Default window: last 7 days. (`bucket_ms` is accepted for shape-compat
but unused — the table holds a handful of rows per day.)

```json
{
  "samples": [
    { "ts": 1716412800000, "down_mbps": 1004.2, "up_mbps": 187.3,
      "ping_ms": 11.8, "duration_ms": 28000, "server_name": "Example ISP",
      "ok": true, "error": null, "derate_flag": true }
  ]
}
```

Failed tests appear with `ok: false` and an `error` string.

### `GET /api/bandwidth/derate-status`

Cheap dedicated endpoint for the banner (UI polls ~60s):

```json
{
  "derated": true,
  "last_test": { "ts": 1716412800000, "down_mbps": 1004.2, "up_mbps": 187.3,
                 "ping_ms": 11.8, "duration_ms": 28000, "server_name": "Example ISP",
                 "ok": true, "error": null, "derate_flag": true },
  "baseline": { "down_mbps": 1024.0, "up_mbps": 974.0,
                "computed_at": 1716412800000, "n": 14 },
  "since": 1716380000000,
  "dismissed_ts": null
}
```

`since` is the start of the current consecutive derate run; `baseline`
is `null` until enough healthy samples exist.

### `POST /api/bandwidth/run`

Manual test trigger (works regardless of `enabled`). → `202` scheduled;
`409` a test is already in flight; `503` engine not wired or speedtest
CLI unavailable.

### `POST /api/bandwidth/install-cli` / `GET .../install-cli`

POST starts the speedtest-CLI install (the root-owned helper via the
sudoers rule) → `202 {"status":"running"}`; `409` one is already
running. GET polls: `{"status": "idle|running|ok|failed", "output"}` —
`output` is the helper's combined output once done (shown in the UI on
failure; the unsupported-distro manual pointer arrives there too). A
successful install flips capability immediately.

---

## Self-update

### `GET /api/update`

```json
{
  "running_version": "v0.5.0",
  "staged": { "present": true, "version": "v0.5.1",
              "size_bytes": 17000000, "modified_at": 1716412800000 },
  "sudoers": { "ok": true, "version": 1, "expected": 1 }
}
```

`staged.error` replaces `version` when the staged file won't run.
`sudoers.error` carries the blocking reason (missing rule, version
drift) — apply refuses until install.sh is re-run.

### `POST /api/update/upload`

Body: the raw binary (`application/octet-stream`, ≤ 100 MB). Must be a
Linux ELF executable → `400` otherwise. Stages to
`/opt/hoptrail/update/hoptrail` (the same path `update.sh --staged`
consumes). → `200` with the staged info.

### `POST /api/update/apply`

Backs up the live binary to `.backups/ui-update-<ts>/`, swaps the
staged one in, re-applies `cap_net_raw` (rolls the binary back if that
fails), then restarts the service(s) ~1 s after responding. → `200
{"applied":true,"new_version":...,"restarting":true}`; `409` nothing
staged / staged unusable / sudoers drift.

---

## Environment status

### `GET /api/status`

The one-call environment overview behind the StatusBar health dot and
the status overlay: `version`, `started_at`/`uptime_s`, `listen`,
`engine.targets`, `probes` (incl. the synthesized local, with
online/last-seen/version), `database` (size on disk, schema version,
retention), `alerts` (enabled/configured, queue depth, active
incidents, last delivery), `bandwidth` (capability, enabled, last
test, derate), and `update` (staged binary, sudoers check). Polled at
30 s by the UI.

## Alerts

### `GET /api/alerts/config` / `PATCH /api/alerts/config`

Full config object both ways (PATCH sends every field):
`enabled`, `server_url`, `topic`, `token`, `event_probe_offline`,
`event_target_loss`, `event_latency`, `event_derate`, `loss_pct`
(1–100), `sustain_s` (30–3600), `latency_level` (warning|critical),
`cooldown_s` (60–86400), `rate_limit_per_h` (1–120), `quiet_start`/
`quiet_end` ("HH:MM" pair or both empty). Composite-validated → `400`;
applies live, no restart.

### `POST /api/alerts/test`

Sends a test notification immediately (bypasses the queue; works with
alerts disabled). → `200 {"delivered":true}`; `409` transport not
configured; `502` with the ntfy error text.

### `GET /api/alerts/status`

`{ "queue_depth": 0, "last_delivery_at": ms, "last_delivery_err": "",
"incidents": [{event_type, subject, state, since, notified_at}] }` —
the settings panel's status line.

### `GET /api/alerts/history?limit=<1-1000>`

The append-only alert log, newest first — every raise and recovery
the engine accepted (recorded regardless of delivery path; the queue
is "what was sent," this is "what happened"). Kept 90 days.
`{ "entries": [{id, ts, event_type, subject, kind, message}] }`,
`kind` ∈ `alert` | `recovered`.

### `POST /api/alerts/install-ntfy` / `GET .../install-ntfy`

Installs a local ntfy server via the sudoers helper — same shape as
the speedtest installer (`202` running / `409` already running; GET
polls `{status, output}`). Refusal because an ntfy already exists
arrives as `failed` with a point-at-it message in `output`.

## Dashboard layout

### `GET /api/layout` / `PATCH /api/layout`

```json
{
  "order": ["latency", "bandwidth", "hops"],
  "side": [],
  "side_position": "right",
  "collapsed": { "bandwidth": true }
}
```

The operator's section arrangement — one global layout, stored
server-side. `order` is the full-width main stack; `side` the optional
vertical dock pinned to `side_position` (`right`|`left`); `collapsed`
maps section ids to true. PATCH never rejects: unknown ids are
dropped, missing ids appended to the main stack, duplicates deduped —
the normalized result is returned and stored.

## Logs

### `GET /api/logs?since_seq=<n>&limit=<1-2000>`

```json
{
  "entries": [
    { "seq": 41, "ts": 1716412800000, "level": "info",
      "msg": "ingest: heartbeat", "attrs": "probe_id=site-east" }
  ],
  "latest_seq": 41
}
```

The web-UI log viewer's feed: the daemon's last ~2000 records from an
in-memory ring (restart-volatile; journald keeps the durable history).
Incremental — pass your last-seen `latest_seq` as `since_seq` to get
only newer records; when more than `limit` (default 500) are pending,
the newest win. Levels follow the live log-level setting.

## System settings

### `GET /api/system` / `PATCH /api/system`

```json
{
  "listen": ":8080",
  "pending_listen": "127.0.0.1:9090",
  "log_level": "info",
  "rdns_enabled": true,
  "pending_rdns_enabled": false,
  "restart_required": true
}
```

PATCH accepts any of `{ "listen", "log_level", "rdns_enabled" }` and
returns the refreshed state. `log_level` (debug|info|warn|error)
applies live; `listen` and `rdns_enabled` persist as overrides (they
win over the yaml at startup) and take effect on restart —
`pending_*` fields show what differs from the running values. Listen
is shape-validated (`[ip]:port`); a value the daemon can't actually
bind at boot falls back to the yaml address with a loud log line
rather than crash-looping. → `400` validation.

### `POST /api/system/restart`

Restarts the central daemon through the sudoers rule, ~1 s after
responding `{"restarting": true}`.

---

## Misc

### `GET /api/version`

→ `{ "version": "v0.4.2" }` (build-injected; `"dev"` for unflagged
builds). Always `200`.

### `GET /api/retention` / `PATCH /api/retention`

GET → `{ "retention_days": 7 }`. PATCH body
`{ "retention_days": 30 }` (1–3650) → `204`; picked up live by the
hourly sweep. `400` out of range or malformed.

### `GET /api/export`

Query: `target`, `probe_id`, `since`/`until` (default: last 1 hour).
Returns a downloadable JSON bundle (`Content-Disposition: attachment`,
filename `hoptrail-<target>-<until>.json`):

```json
{
  "schema_version": 1,
  "generated_at": 1716412800000,
  "target": "8.8.8.8",
  "probe_id": "local",
  "window": { "since": 1716409200000, "until": 1716412800000 },
  "path": { "...": "same shape as /api/path; omitted if unavailable" },
  "samples": [ ],
  "route_changes": [ ],
  "annotations": [ ]
}
```

### `GET /api/target_stats?target=<id>`

Does history exist for this target (across all probes)? Feeds the
resume-vs-new prompt. →
`{ "samples": 123456, "oldest_ts": 1716000000000, "newest_ts": 1716412800000 }`.
`400` without `target`.

### `DELETE /api/target_data?target=<id>`

Wipes a target's samples and route changes across all probes ("start
new"). Annotations survive. → `204`; `400` without `target`.
