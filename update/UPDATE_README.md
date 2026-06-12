# Update staging folder

This folder is where update.sh looks for a newly-built hoptrail binary
when invoked with `--staged`. The flow exists so you can rebuild on a
dev box, rsync the binary to the server, and apply it without copying
the whole project tree.

## How to stage an update

From your dev box, after `make build` produces `./hoptrail`:

```bash
rsync -av ./hoptrail user@your-server:/opt/hoptrail/update/hoptrail
```

Then on the server:

```bash
/opt/hoptrail/update.sh --staged
```

The script will:

1. Stop the running service
2. Back up the current binary to `/opt/hoptrail/.backups/<timestamp>/hoptrail`
3. Move `update/hoptrail` into place at `/opt/hoptrail/bin/hoptrail`
4. Re-apply `cap_net_raw+ep` (every binary swap strips capability bits;
   this is non-optional, see lesson #7 in the project handoff)
5. Restart the service and verify it came up
6. Delete the staged binary so the staging slot is empty for the next update

If the new binary fails to start, the script automatically rolls back
to the previous binary from the backup. The previous five backups are
retained at `/opt/hoptrail/.backups/`.

## What's preserved across updates

- `/opt/hoptrail/config.yaml` — operator-edited config, never overwritten
- `/var/lib/hoptrail/*` — the SQLite database and its WAL/SHM sidecars
- `/opt/hoptrail/.backups/*` — prior binary versions, for manual rollback

## What's replaced

- `/opt/hoptrail/bin/hoptrail` — the binary itself
