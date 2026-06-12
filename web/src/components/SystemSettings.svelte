<script>
  // System settings section (step-125, v0.5 design §6 F): the last
  // yaml-only knobs, in the panel. Log level applies live; listen and
  // rDNS persist as pending and the restart button (sudoers-backed)
  // makes them real.
  import { onDestroy } from 'svelte'
  import { fetchSystemSettings, patchSystemSettings, restartDaemon } from '../lib/api.js'
  import { logsOpen } from '../lib/stores.js'

  let st = null
  let error = ''
  let restarting = false
  let restartTimer = null
  let sawDown = false

  async function refresh() {
    try {
      st = await fetchSystemSettings()
    } catch (err) {
      error = err.message ?? String(err)
    }
  }
  refresh()

  async function patch(p) {
    error = ''
    try {
      st = await patchSystemSettings(p)
    } catch (err) {
      error = err.message ?? String(err)
      refresh()
    }
  }

  function commitListen(e) {
    const v = e.target.value.trim()
    if (!v || v === (st?.pending_listen ?? st?.listen)) return
    patch({ listen: v })
  }

  async function restart() {
    error = ''
    restarting = true
    sawDown = false
    try {
      await restartDaemon()
    } catch (err) {
      restarting = false
      error = err.message ?? String(err)
      return
    }
    // Poll the daemon down and back up, then reload (the bundle and
    // any changed settings come back fresh). If the listen address
    // changed, this origin may stop answering — the hint below warns.
    restartTimer = setInterval(async () => {
      try {
        const res = await fetch('/api/version')
        if (res.ok && sawDown) {
          clearInterval(restartTimer)
          restartTimer = null
          location.reload()
        }
      } catch {
        sawDown = true
      }
    }, 1000)
    // Safety: even if we never observe the down-window (fast restart),
    // reload after 15s rather than spinning forever.
    setTimeout(() => {
      if (restartTimer) {
        clearInterval(restartTimer)
        restartTimer = null
        location.reload()
      }
    }, 15000)
  }
  onDestroy(() => { if (restartTimer) clearInterval(restartTimer) })

  $: rdnsShown = st ? (st.pending_rdns_enabled ?? st.rdns_enabled) : true
</script>

<div class="system">
  {#if !st}
    <p class="muted">loading…</p>
  {:else}

    <label class="row">
      <span class="label">Listen address</span>
      <input
        type="text"
        class="mono"
        value={st.pending_listen ?? st.listen}
        on:blur={commitListen}
        on:keydown={(e) => { if (e.key === 'Enter') e.target.blur() }}
      />
    </label>
    <p class="hint">
      <code>:8080</code> = all interfaces; <code>127.0.0.1:8080</code> =
      this host only. Applies on restart — if you change it, reconnect
      at the new address afterwards. A value the daemon can't bind
      falls back to the config file's address (check the journal).
    </p>

    <label class="row">
      <span class="label">Log level</span>
      <select
        value={st.log_level}
        on:change={(e) => patch({ log_level: e.target.value })}
      >
        <option value="debug">debug</option>
        <option value="info">info</option>
        <option value="warn">warn</option>
        <option value="error">error</option>
      </select>
      <span class="muted small">applies immediately</span>
    </label>

    <label class="row check">
      <input
        type="checkbox"
        checked={rdnsShown}
        on:change={(e) => patch({ rdns_enabled: e.target.checked })}
      />
      Reverse-DNS lookups (hostnames in the hop table)
    </label>
    <p class="hint">
      Disable if you'd rather not leak intermediate-hop IPs to your DNS
      resolver. Applies on restart.
    </p>

    <div class="row">
      <span class="label">Daemon log</span>
      <button class="logs-btn" on:click={() => logsOpen.set(true)}>View logs</button>
      <span class="muted small">recent records, live</span>
    </div>

    {#if st.restart_required}
      <p class="warn">Changes pending — restart to apply.</p>
    {/if}
    <div class="actions">
      <button class="restart" on:click={restart} disabled={restarting}>
        {restarting ? 'Restarting…' : 'Restart hoptrail'}
      </button>
      {#if restarting}<span class="muted small">page reloads when it's back</span>{/if}
    </div>

    {#if error}<p class="error">{error}</p>{/if}
  {/if}
</div>

<style>
  .system { display: flex; flex-direction: column; gap: 8px; }
  .row { display: flex; align-items: center; gap: 8px; font-size: 0.8rem; }
  .row.check { font-size: 0.8rem; }
  .label { color: var(--fg-muted); font-size: 0.75rem; min-width: 7em; }
  .muted { color: var(--fg-muted); margin: 0; }
  .small { font-size: 0.7rem; }
  .hint { color: var(--fg-muted); font-size: 0.7rem; margin: -4px 0 0; }
  .warn { color: var(--warning, #f5a524); font-size: 0.75rem; margin: 0; }
  .error { color: var(--danger, #e5484d); font-size: 0.75rem; margin: 0; }

  input[type='text'], select {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.78rem;
    padding: 4px 6px;
  }
  .mono { font-family: var(--font-mono); width: 11em; }

  .actions { display: flex; align-items: center; gap: 8px; }
  .logs-btn {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.75rem;
    padding: 4px 8px;
    cursor: pointer;
  }
  .logs-btn:hover { border-color: var(--fg-subtle); }
  .restart {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.75rem;
    padding: 4px 8px;
    cursor: pointer;
  }
  .restart:hover:not(:disabled) { border-color: var(--warning, #f5a524); color: var(--warning, #f5a524); }
  .restart:disabled { opacity: 0.45; cursor: default; }
</style>
