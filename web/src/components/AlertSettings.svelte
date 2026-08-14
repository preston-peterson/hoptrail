<script>
  // Alerts settings section (step-136, design §7). Transport setup +
  // test button live OUTSIDE the enabled-gated group (enable gates
  // automation only — manual test always works). The whole config
  // PATCHes as one object on every committed change.
  import { onDestroy } from 'svelte'
  import {
    fetchAlertConfig,
    patchAlertConfig,
    sendTestAlert,
    fetchAlertStatus,
    startNtfyInstall,
    fetchNtfyInstall,
    fetchSoundConfig,
  } from '../lib/api.js'
  import { soundConfig, setSoundConfig } from '../lib/stores.js'
  import { armAudio, playRaise, playRecover } from '../lib/sound.js'
  import PhoneSetupGuide from './PhoneSetupGuide.svelte'

  // Audible alerts (#20): master + per-event toggles, server-shared.
  const SOUND_EVENTS = [
    { key: 'probe_offline', label: 'probe offline / recovered' },
    { key: 'target_loss', label: 'sustained loss on a target' },
    { key: 'latency', label: 'latency over the threshold' },
    { key: 'derate', label: 'bandwidth derate' },
  ]

  // Re-sync the shared policy whenever the panel opens — another
  // browser may have changed it since this tab loaded.
  fetchSoundConfig()
    .then((c) => soundConfig.set(c))
    .catch(() => {})

  async function toggleSoundMaster(e) {
    error = ''
    armAudio() // the enabling click is this browser's autoplay gesture
    try {
      await setSoundConfig({ enabled: e.target.checked, events: {} })
    } catch (err) {
      error = err.message ?? String(err)
    }
  }

  async function toggleSoundEvent(key, e) {
    error = ''
    try {
      await setSoundConfig({
        enabled: $soundConfig?.enabled ?? false,
        events: { [key]: e.target.checked },
      })
    } catch (err) {
      error = err.message ?? String(err)
    }
  }

  // Phone setup guide modal (step-138, operator request).
  let guideOpen = false

  let cfg = null
  let status = null
  let error = ''
  let testMsg = ''

  async function refresh() {
    try {
      cfg = await fetchAlertConfig()
      status = await fetchAlertStatus()
    } catch (err) {
      error = err.message ?? String(err)
    }
  }
  refresh()
  const statusTimer = setInterval(async () => {
    try { status = await fetchAlertStatus() } catch { /* transient */ }
  }, 15000)
  onDestroy(() => clearInterval(statusTimer))

  async function commit() {
    error = ''
    try {
      cfg = await patchAlertConfig(cfg)
    } catch (err) {
      error = err.message ?? String(err)
      refresh() // re-sync to server truth
    }
  }

  // disk_free_pct_floor is a fraction (0–1) on the wire but shown as a
  // percentage; convert on the way out.
  function commitPct(e) {
    const pct = parseFloat(e.target.value)
    if (!isNaN(pct)) cfg.disk_free_pct_floor = Math.max(0, Math.min(1, pct / 100))
    commit()
  }

  async function test() {
    error = ''
    testMsg = 'sending…'
    try {
      await sendTestAlert()
      testMsg = 'delivered ✓ — check your phone'
    } catch (err) {
      testMsg = ''
      error = err.message ?? String(err)
    }
    setTimeout(() => (testMsg = ''), 6000)
  }

  $: transportReady = cfg?.server_url && cfg?.topic

  // Local-ntfy install flow (step-136): same watcher pattern as the
  // speedtest button. On success: prefill the transport — the browser
  // knows how it reaches this host, ntfy listens on :2586, and a
  // random topic suffix keeps drive-by subscribers guessing.
  let ntfy = { status: 'idle', output: '' }
  let ntfyTimer = null
  async function installNtfy() {
    error = ''
    try {
      ntfy = { status: 'running', output: '' }
      await startNtfyInstall()
      watchNtfy()
    } catch (err) {
      ntfy = { status: 'failed', output: err.message ?? String(err) }
    }
  }
  function watchNtfy() {
    if (ntfyTimer) return
    ntfyTimer = setInterval(async () => {
      try {
        const st = await fetchNtfyInstall()
        ntfy = st
        if (st.status !== 'running') {
          clearInterval(ntfyTimer)
          ntfyTimer = null
          if (st.status === 'ok') {
            cfg.server_url = `http://${location.hostname}:2586`
            if (!cfg.topic) {
              cfg.topic = 'hoptrail-' + Math.random().toString(36).slice(2, 8)
            }
            await commit()
          }
        }
      } catch {
        // transient poll failure — keep polling
      }
    }, 3000)
  }
  onDestroy(() => { if (ntfyTimer) clearInterval(ntfyTimer) })
</script>

{#if !cfg}
  <p class="muted">loading…</p>
{:else}
  <div class="alerts">
    <!-- Transport: where notifications go. -->
    <p class="hint">
      Alerts push to an <a href="https://ntfy.sh" target="_blank" rel="noreferrer">ntfy</a>
      server — install the ntfy app on your phone and subscribe to the
      topic below. Point hoptrail at a server you already run, or:
    </p>
    <div class="row">
      <button class="btn" disabled={ntfy.status === 'running'} on:click={installNtfy}>
        {ntfy.status === 'running' ? 'Installing…' : 'Install a local ntfy server'}
      </button>
      {#if ntfy.status === 'ok'}<span class="ok small">installed ✓ — transport prefilled below</span>{/if}
    </div>
    {#if ntfy.status === 'failed'}
      <pre class="install-out">{ntfy.output}</pre>
    {/if}
    <label class="row">
      <span class="label">ntfy server</span>
      <input class="mono wide" type="text" placeholder="https://ntfy.example.net"
        bind:value={cfg.server_url} on:blur={commit}
        on:keydown={(e) => e.key === 'Enter' && e.target.blur()} />
    </label>
    <label class="row">
      <span class="label">topic</span>
      <input class="mono" type="text" placeholder="hoptrail-alerts"
        bind:value={cfg.topic} on:blur={commit}
        on:keydown={(e) => e.key === 'Enter' && e.target.blur()} />
    </label>
    <label class="row">
      <span class="label">token</span>
      <input class="mono" type="password" placeholder="optional"
        bind:value={cfg.token} on:blur={commit}
        on:keydown={(e) => e.key === 'Enter' && e.target.blur()} />
    </label>
    <div class="row">
      <button class="btn" disabled={!transportReady} on:click={test}>Send test notification</button>
      <button class="btn" on:click={() => (guideOpen = true)}>Phone setup guide</button>
      {#if testMsg}<span class="ok small">{testMsg}</span>{/if}
    </div>
    {#if guideOpen}
      <PhoneSetupGuide
        serverUrl={cfg.server_url}
        topic={cfg.topic}
        on:close={() => (guideOpen = false)}
      />
    {/if}

    <label class="enable">
      <input type="checkbox" bind:checked={cfg.enabled} on:change={commit} />
      <strong>Enable alerts</strong>
    </label>

    <div class="group" class:dimmed={!cfg.enabled}>
      <span class="group-label">events</span>
      <label class="check"><input type="checkbox" bind:checked={cfg.event_probe_offline} on:change={commit} /> probe offline / recovered</label>
      <label class="check"><input type="checkbox" bind:checked={cfg.event_target_loss} on:change={commit} /> sustained loss on a target</label>
      <label class="check"><input type="checkbox" bind:checked={cfg.event_latency} on:change={commit} /> latency over the tab's
        <select bind:value={cfg.latency_level} on:change={commit}>
          <option value="warning">warning</option>
          <option value="critical">critical</option>
        </select> line</label>
      <label class="check"><input type="checkbox" bind:checked={cfg.event_derate} on:change={commit} /> bandwidth derate</label>
      <label class="check"><input type="checkbox" bind:checked={cfg.event_capacity} on:change={commit} /> low disk / DB growth</label>

      <span class="group-label">thresholds</span>
      <label class="row">
        <span class="label">loss counts as down at</span>
        <input class="num" type="number" min="1" max="100" bind:value={cfg.loss_pct} on:change={commit} /> %
      </label>
      <label class="row">
        <span class="label">condition must hold for</span>
        <input class="num" type="number" min="30" max="3600" step="30" bind:value={cfg.sustain_s} on:change={commit} /> s
      </label>
      <label class="row">
        <span class="label">warn when free disk below</span>
        <input class="num" type="number" min="0" max="1048576" bind:value={cfg.disk_free_floor_mb} on:change={commit} /> MB
      </label>
      <label class="row">
        <span class="label">…or below</span>
        <input class="num" type="number" min="0" max="100" step="1" value={Math.round((cfg.disk_free_pct_floor ?? 0) * 100)} on:change={commitPct} /> % of disk
      </label>
      <label class="row">
        <span class="label">warn when growth headroom below</span>
        <input class="num" type="number" min="1" max="100" step="0.1" bind:value={cfg.headroom_threshold} on:change={commit} /> ×
        <span class="muted small">projected size ÷ free space</span>
      </label>

      <span class="group-label">hygiene</span>
      <label class="row">
        <span class="label">quiet hours</span>
        <input class="mono time" type="text" placeholder="22:00" bind:value={cfg.quiet_start} on:blur={commit} />
        –
        <input class="mono time" type="text" placeholder="07:00" bind:value={cfg.quiet_end} on:blur={commit} />
        <span class="muted small">empty = none; alerts roll into one summary at window end</span>
      </label>
      <label class="row">
        <span class="label">re-alert cooldown</span>
        <input class="num" type="number" min="60" max="86400" step="60" bind:value={cfg.cooldown_s} on:change={commit} /> s
      </label>
      <label class="row">
        <span class="label">max alerts per hour</span>
        <input class="num" type="number" min="1" max="120" bind:value={cfg.rate_limit_per_h} on:change={commit} />
      </label>
      <p class="hint">Every alert gets a matching “recovered” message.</p>

      <span class="group-label">sound</span>
      <label class="check">
        <input type="checkbox" checked={$soundConfig?.enabled ?? false} on:change={toggleSoundMaster} />
        play a sound in the browser when an alert fires
      </label>
      <div class="sound-events" class:dimmed={!$soundConfig?.enabled}>
        {#each SOUND_EVENTS as ev}
          <label class="check">
            <input type="checkbox" checked={$soundConfig?.events?.[ev.key] ?? true}
              on:change={(e) => toggleSoundEvent(ev.key, e)} />
            {ev.label}
          </label>
        {/each}
        <div class="row">
          <button class="btn" on:click={() => playRaise()}>♪ alert tone</button>
          <button class="btn" on:click={() => playRecover()}>♪ recovery tone</button>
          <span class="muted small">recoveries play the softer all-clear</span>
        </div>
        <p class="hint">
          The setting is shared by every browser; each one can only ring
          after you've clicked somewhere in it once (a browser rule).
        </p>
      </div>
    </div>

    {#if status}
      <p class="status">
        {#if status.incidents.length > 0}
          <span class="warn">{status.incidents.length} active incident{status.incidents.length === 1 ? '' : 's'}</span> ·
        {/if}
        queue {status.queue_depth}
        {#if status.last_delivery_at}
          · last delivery {new Date(status.last_delivery_at).toLocaleTimeString()}
          {#if status.last_delivery_err}<span class="err-inline" title={status.last_delivery_err}>failed</span>{:else}ok{/if}
        {/if}
      </p>
    {/if}
    {#if error}<p class="error">{error}</p>{/if}
  </div>
{/if}

<style>
  .alerts { display: flex; flex-direction: column; gap: 8px; }
  .muted { color: var(--fg-muted); margin: 0; }
  .small { font-size: 0.7rem; }
  .hint { color: var(--fg-muted); font-size: 0.72rem; margin: 0; }
  .hint a { color: var(--accent, #4f8ff7); }
  .error { color: var(--danger, #e5484d); font-size: 0.75rem; margin: 0; }
  .ok { color: var(--ok, #30a46c); }
  .warn { color: var(--warning, #f5a524); }
  .err-inline { color: var(--danger, #e5484d); cursor: help; margin-left: 3px; }

  .row { display: flex; align-items: center; gap: 7px; font-size: 0.8rem; flex-wrap: wrap; }
  .label { color: var(--fg-muted); font-size: 0.75rem; min-width: 9em; }
  input[type='text'], input[type='password'], input[type='number'], select {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.78rem;
    padding: 4px 6px;
  }
  .mono { font-family: var(--font-mono); }
  .wide { flex: 1; min-width: 0; }
  .num { width: 5.5em; }
  .time { width: 4.5em; }

  .btn {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.75rem;
    padding: 4px 8px;
    cursor: pointer;
  }
  .btn:hover:not(:disabled) { border-color: var(--ok, #30a46c); color: var(--ok, #30a46c); }
  .btn:disabled { opacity: 0.45; cursor: default; }

  .enable { display: flex; align-items: center; gap: 7px; font-size: 0.85rem; margin-top: 4px; }
  .group { display: flex; flex-direction: column; gap: 7px; }
  .group.dimmed { opacity: 0.5; pointer-events: none; }
  .group-label {
    margin-top: 4px;
    font-size: 0.68rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--fg-subtle);
  }
  .check { display: flex; align-items: center; gap: 7px; font-size: 0.8rem; flex-wrap: wrap; }
  .sound-events {
    display: flex;
    flex-direction: column;
    gap: 7px;
    margin-left: 22px; /* indent under the master toggle */
  }
  .sound-events.dimmed { opacity: 0.5; pointer-events: none; }
  .install-out {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    font-family: var(--font-mono);
    font-size: 0.68rem;
    line-height: 1.4;
    margin: 0;
    max-height: 140px;
    overflow: auto;
    padding: 6px;
    white-space: pre-wrap;
    word-break: break-word;
  }
  .status { color: var(--fg-muted); font-size: 0.72rem; margin: 4px 0 0; border-top: 1px solid var(--border); padding-top: 8px; }
</style>
