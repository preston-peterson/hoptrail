# Operations Guide

Hoptrail is a continuous traceroute and per-hop latency tracker for Linux.
This guide covers running it as a service: install, update, and uninstall
flows; the systemd units for both roles; what to back up; the capability
bit that every binary swap silently strips; logs; token rotation;
retention behavior; and where the database lives and how big it gets.
For using the UI see [user-guide.md](user-guide.md); for multi-site
setups see [distributed-probing.md](distributed-probing.md).

## Layout

A scripted install puts everything in two places:

```
/opt/hoptrail/
├── bin/hoptrail        the binary (carries cap_net_raw+ep)
├── config.yaml         central config   (central role)
├── probe.yaml          probe config     (probe role)
├── update.sh           operator scripts, installed alongside
├── uninstall.sh
├── .backups/           timestamped binary backups (last 5 kept)
└── update/             staging dir for ./update.sh --staged

/var/lib/hoptrail/
├── hoptrail.db         the SQLite database (central; + -wal/-shm sidecars)
└── probe-buffer.db     partition spill buffer (probe role only)
```

Both directories are owned by the user who ran the installer; the daemon
runs as that user, not root.

## Install

The one-liner (recommended — downloads the latest release's prebuilt,
sha256-verified binary; no build tools needed):

```bash
curl -fsSL https://get.hoptrail.net | bash
```

Any flag `install.sh` understands passes straight through after `-s --`
(e.g. `| bash -s -- --probe --id site-east --central … --token …`).

From a source checkout:

```bash
make build       # optional — install.sh builds from source if no binary exists
./install.sh                 # central daemon (probe engine + web UI)
./install.sh --probe         # remote probe instead (see distributed-probing.md)
./install.sh --add-bandwidth # ONLY install the Ookla Speedtest CLI, then exit
./install.sh --help          # full usage
```

Run as a regular user; the script invokes `sudo` where needed (binary
placement, systemd, `setcap`). It is idempotent: re-running refreshes the
binary and scripts, preserves the config and database, and restarts the
service. Installing one role does not disturb the other's unit or
config — a box can host the central *and* a probe reporting to a
different central.

The probe role prompts for `probe_id`, the central URL, and the bearer
token on a fresh config; leaving any blank installs everything but
deliberately does **not** start the service (a probe with placeholder
config would crash-loop or sit "active" while doing nothing).

## Update

**From a GitHub release** (the easy path): gear icon → **Updates** →
**Check for updates** → **Download vX.Y.Z** → **Apply**. The daemon
fetches the release binary for its architecture, verifies it against
the release's sha256 checksums, stages it, and — on Apply — backs up
the old binary, swaps in the new one, re-applies `cap_net_raw`, and
restarts itself through the sudoers rule; the page reloads on the new
version. A background check runs on a cadence you pick in the same
panel (daily / weekly / monthly, default monthly, or never); a found
update is announced on the status page but **never** downloaded or
applied without your click.

**From an uploaded binary**: same panel — upload a self-built
`hoptrail` binary instead, then **Apply**. Both roads stage the same
slot and share the one apply path.

**Updating remote probes**: probes report their version on every
heartbeat; when one runs an older release than the central, the
Probes panel and the status page flag it (**update available** /
**outdated**), and the panel's **Update** button *does the update*:
the central downloads the release binary for the probe's
architecture (sha256-verified), hands it to the probe on its next
heartbeat, and the probe verifies, swaps, and restarts itself — with
the old binary backed up and automatic rollback if the new one can't
take the raw-ICMP capability. **Update all** walks every outdated
probe one at a time, stopping on the first failure; the 📌 pin on a
probe row keeps that box out of fleet updates. Failed updates land
in the alert history with the reason. See
[probe-update-design.md](probe-update-design.md) for the mechanics.

A probe too old to understand the command (it never reported its
architecture) gets **How to update** instead — one pasted command on
the probe host, after which the button works forever:

```bash
curl -fsSL https://get.hoptrail.net | bash -s -- --probe
```

The existing probe config is preserved (no name, address, or token to
re-enter). The outdated flag compares release versions, so a central
running a dev build doesn't nag probes on the same release.

Two terminal flows remain, both via `update.sh`:

**Local build** (dev box is the server):

```bash
make build
./update.sh
```

**Staged** (built elsewhere, copied to the server):

```bash
scp hoptrail server:/opt/hoptrail/update/hoptrail
ssh server /opt/hoptrail/update.sh --staged
```

Either way the script:

1. Stops every installed hoptrail unit on the box — `hoptrail`,
   `hoptrail-probe`, or both (they share the binary, so an update must
   bounce whichever roles exist).
2. Backs up the current binary to `/opt/hoptrail/.backups/<timestamp>/`
   (the 5 most recent backups are kept).
3. Atomically replaces the binary and **re-applies
   `cap_net_raw+ep`** (see the gotcha below), verifying with `getcap`.
4. Restarts only the units that were running before — an intentionally
   stopped unit (e.g. a probe awaiting config) stays stopped.

If a unit fails to come back up, the script automatically rolls back to
the backed-up binary and restarts. A successful `--staged` run consumes
the staged file. Config and database are never touched.

Database schema migrations run forward-only at daemon startup, so the
first start on a new binary may take a moment longer. There is no
automated downgrade path: rolling back the binary after a migration that
changed the schema is not supported — restore the database from backup
alongside the old binary instead.

## Uninstall

```bash
./uninstall.sh           # interactive — prompts per data file, with sizes
./uninstall.sh --purge   # remove everything including data, no prompts
./uninstall.sh --keep    # remove service + binary only, keep all data
```

The systemd units and binary always go. Config files, the database and
its sidecars, the probe spill buffer, and the directories themselves are
prompt-gated (or flag-driven). System packages (the libcap utilities)
are never removed.

## systemd units

| Role | Unit | ExecStart |
|---|---|---|
| central | `hoptrail.service` | `/opt/hoptrail/bin/hoptrail serve --config /opt/hoptrail/config.yaml` |
| probe | `hoptrail-probe.service` | `/opt/hoptrail/bin/hoptrail probe --config /opt/hoptrail/probe.yaml` |

Both run as the installing user with `Restart=on-failure`
(`RestartSec=15`) and log to the journal. The daemon is built to fail
loud: if a critical subsystem can't start (e.g. the HTTP port is taken),
the process exits non-zero rather than limping along "active" with a
dead UI — so `systemctl status` tells the truth.

```bash
sudo systemctl status hoptrail
sudo systemctl restart hoptrail
sudo systemctl stop hoptrail
```

## Backups

Back up two things:

- **The database**: `/var/lib/hoptrail/hoptrail.db`. It's a single
  SQLite file in WAL mode. The safe ways to copy it:

  ```bash
  # Online, consistent — preferred:
  sqlite3 /var/lib/hoptrail/hoptrail.db ".backup /tmp/hoptrail-backup.db"

  # Or stop the service and copy the file (plus -wal/-shm if present):
  sudo systemctl stop hoptrail
  cp /var/lib/hoptrail/hoptrail.db* /backup/location/
  sudo systemctl start hoptrail
  ```

  Copying the bare `.db` while the daemon is writing can capture a
  torn state — use one of the above.

- **The configs**: `/opt/hoptrail/config.yaml` (and `probe.yaml` on
  probe hosts). Small text files; everything else the installer lays
  down is regenerable.

Not worth backing up: `probe-buffer.db` (transient spill data),
`.backups/` (old binaries), the binary itself (rebuild or re-download).

Restore = install the same (or newer) version, put the config and
database back in place, start the service.

## The capability bit gotcha

Hoptrail needs `CAP_NET_RAW` to open the raw ICMP socket; it gets it via
a file capability on the binary instead of running as root:

```bash
sudo setcap cap_net_raw+ep /opt/hoptrail/bin/hoptrail
```

**Capabilities live on the inode. Every binary replacement — `install`,
`cp`, `go build`, a manual rollback — produces a new inode with zero
capabilities.** A daemon started from a freshly swapped binary without
re-applying the cap fails its ICMP socket open with "operation not
permitted." `install.sh` and `update.sh` both re-apply and verify the
cap after every swap; if you ever replace the binary by hand, run
`setcap` yourself and check:

```bash
getcap /opt/hoptrail/bin/hoptrail     # expect: cap_net_raw=ep
```

Related: filesystems mounted `nosuid` silently drop file capabilities.
`/opt` normally isn't, but the scripts verify with `getcap` after every
`setcap` and abort (or roll back) if the cap didn't stick — if you see
that failure, the install location is on a `nosuid` mount and you need a
different one.

## Logs

Everything goes to the journal:

```bash
sudo journalctl -u hoptrail -f          # central, follow
sudo journalctl -u hoptrail-probe -n 50 # probe, last 50 lines
```

Verbosity and format are config (`log.level`: debug|info|warn|error;
`log.format`: text|json). `info` is the right steady state — lifecycle
events, route changes, retention sweeps, probe ingest summaries, and one
line per API request (4xx/5xx enriched with the query string and
user agent). `debug` logs every probe and is for active troubleshooting
only. Each accepted ingest logs the probe id and the first 4 characters
of the token used, so you can audit which token a probe is on; the full
token never logs.

## Token rotation

Probe auth is a bearer token per probe (see
[distributed-probing.md](distributed-probing.md)). Multiple tokens are
valid simultaneously — rotation always has a clean overlap window:

1. On the central's UI: gear icon → **Probes** → add a token under the
   same probe name. (No restart — UI-minted tokens apply on the next
   request.)
2. On the probe: put the new token in `probe.yaml`'s `central.token`,
   restart `hoptrail-probe`.
3. On the central's UI: revoke the old token.

Yaml-managed tokens (`probes.tokens` in `config.yaml`) rotate the same
way but need a central restart per change; the accepted set is the
union of both sources.

To revoke a probe outright, revoke its token (UI button, instant). The
probe then 401s, exits non-zero, and visibly crash-loops under systemd
until you stop or reconfigure it — loud by design.

## The sudoers rule

`install.sh` writes `/etc/sudoers.d/hoptrail`: an exact-command
NOPASSWD whitelist (service restart, re-applying `cap_net_raw` after a
binary self-update, the speedtest-CLI and local-ntfy install helpers)
that lets the web UI drive those actions without a terminal. The file carries a
machine-readable `SUDOERS_VERSION` marker; when a future release needs
new rules, the UI tells you to re-run `install.sh` rather than editing
anything by hand. `uninstall.sh` always removes the file.

## Retention

A background worker runs hourly (plus once at startup) and deletes, per
the retention policy (default 7 days, editable live from the UI's
settings panel or `PATCH /api/retention`, range 1–3650):

- raw probe **samples**
- **route changes**

It also prunes the ingest dedup log on a fixed 24-hour horizon.

**Never pruned:**

- **annotations** — operator notes are kept forever (and survive even a
  per-target "start new" history wipe);
- **bandwidth samples** — a year of speed tests is ~88 KB; long-term
  trend is the value;
- the **rDNS cache** — bounded by unique IPs seen, not by time.

## Database location and size

Default `/var/lib/hoptrail/hoptrail.db` (configurable via
`storage.path`). Growth is linear in probe rate:
`hops × targets × probes / interval` rows per second. As a reference
point, one target on a ~30-hop budget at the default 1s interval stays
roughly 450 MB per 7 retained days per probed target at 1-second cadence (measured on a live deployment; scales linearly with targets, probes, and cadence). With retention at the default 7 days, the file grows for the first
week and then plateaus.

Two SQLite behaviors worth knowing:

- Deletes don't shrink the file — freed pages are reused, so the file
  stabilizes at "one retention window worth" of size. Reclaiming disk
  requires a manual `VACUUM` (blocking; stop the service first).
- WAL mode means `-wal`/`-shm` sidecars appear next to the file while
  the daemon runs; that's normal.

The biggest size levers, in order: probe interval (10s collects 10× less
than 1s), final-hop-only mode (~95% fewer rows on long paths), retention
days, and number of targets × probes.
