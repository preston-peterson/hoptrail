# Central-Driven Probe Updates — Design

Status: shipped (v0.7). This documents the design and its reasoning;
operator-facing usage lives in [operations.md](operations.md) and
[distributed-probing.md](distributed-probing.md).

## Goal

The **Update** button on an outdated probe *does the update*. No
terminal, no copy-paste, no inbound connection to the probe. Plus an
**Update all** that walks the fleet one probe at a time, and a
per-probe **pin** to keep a box out of fleet operations.

## Constraints that shaped it

1. **Probes have no inbound ports.** Everything they learn arrives in
   the reply to a heartbeat they initiated — so the update command
   rides the heartbeat reply, exactly like the target set does.
2. **Probes may lack GitHub egress.** The central downloads the
   release binary once per (version, architecture) — sha256-verified
   against the release's checksums — caches it, and serves it to
   probes over the existing bearer-token ingest surface.
3. **The probe re-verifies.** The heartbeat command carries the
   binary's sha256; the probe hashes what it downloaded before
   touching anything. Central verifies GitHub; probe verifies central.
4. **A bad update must not brick a remote site.** Before the live
   binary is touched, the probe runs the staged binary's `version` —
   wrong-architecture and too-old-glibc failures surface here, with
   nothing changed. The old binary is backed up; a failed `setcap`
   rolls back immediately. A binary that runs but can't probe lands
   in systemd's crash-loop — loud, visible, and reported by the
   central as a failed update when no healthy heartbeat arrives.

## Why in-process apply, not update.sh

`update.sh --staged` has the gold-standard rollback discipline, but it
cannot be invoked *by the probe itself*: the script runs inside the
probe unit's cgroup, and its first act — stopping the unit — makes
systemd kill the whole cgroup, script included, mid-update. The probe
therefore applies in-process, mirroring the central's own UI-update
path: verify staged → backup → atomic rename → `sudo -n setcap`
(rollback on failure) → `sudo -n systemctl restart --no-block`. The
swap is complete before the restart is requested. Everything needed is
already in sudoers v2 — no privilege changes shipped with this
feature.

## Lifecycle

```
operator clicks Update
  └─ central: download+cache release binary, write probe_updates row (pending)
heartbeat arrives
  └─ reply carries {version, sha256, path}        (delivery #1)
probe: POST update-status {applying}
probe: download → verify sha256+ELF → version-probe → backup → swap → setcap → restart
new binary's first heartbeat reports the target version
  └─ central marks applied
```

Failure detection, in order of information quality:

- **Probe-reported**: any dead end on the probe POSTs
  `{failed, why}` — sha mismatch, version-probe refusal, setcap
  rollback. Best story, immediate.
- **Never acknowledged**: the command was delivered on two heartbeat
  replies and the probe never reported `applying` → the probe predates
  the feature; failed with "update manually once."
- **Acknowledged, then silence**: `applying` but no heartbeat on the
  new version within 5 minutes → failed ("likely rolled back or
  wedged — check the probe's journal"). A rollback heartbeat (old
  version, post-timeout) lands here with a clear story.

Every failure appends to the alert history — the bell tells the tale.

## Update all

Strictly sequential: command one probe, wait for a terminal state,
proceed only on success. **A failure stops the rollout** — a bad
build never ships fleet-wide. Candidates: outdated + online +
architecture known + not pinned. The rollout state is in-memory; a
central restart mid-rollout abandons the remaining probes (the
commanded one finishes on its own) and the operator re-clicks —
deliberately simpler than a persistent rollout state machine.

## Bootstrap

A probe must already run a version that understands the heartbeat
command (and report its architecture) before it can be updated this
way — so each pre-existing probe needs **one final manual update**
(the get.sh one-liner). The Probes panel detects this case (no
reported arch) and shows the manual instructions instead of promising
a button it can't deliver.

## Deliberately deferred

- **Auto mode** ("probes follow the central") — the command machinery
  is identical; ships later as an opt-in toggle. Manual always works.
- **A dedicated ntfy event type** for failed updates — they currently
  reach the alert history and status surfaces.
