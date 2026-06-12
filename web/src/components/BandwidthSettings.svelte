<script>
  // Bandwidth section of the settings panel (step-101; design §6.3).
  // Capability-routed: CLI missing → install guidance only;
  // available+disabled → enable toggle with fields dimmed;
  // available+enabled → full editor. Every change PATCHes
  // immediately on commit (no Save button — IntervalPicker pattern)
  // and re-fetches server truth.
  import { onDestroy } from 'svelte'
  import { bandwidthConfig, refreshBandwidthConfig, refreshBandwidth, errorStore } from '../lib/stores.js'
  import {
    patchBandwidthConfig,
    runBandwidthTest,
    startBandwidthCliInstall,
    fetchBandwidthCliInstall,
  } from '../lib/api.js'

  $: cfg = $bandwidthConfig

  let patchError = ''
  let runMsg = ''

  // Live slider preview (step-109, operator feedback): the % label
  // tracks the drag via on:input; the PATCH still commits on release
  // (on:change) so we don't spam the server mid-drag.
  let dragThresh = null

  // Timezone dropdown (step-154, mirroring the sibling project's
  // picker after the same trap: a UTC server made "02:00" mean 9 PM
  // local). Browser zone surfaced first; full IANA list from the
  // browser itself — no list to maintain.
  const browserTz = Intl.DateTimeFormat().resolvedOptions().timeZone ?? ''
  const allTimezones = (Intl.supportedValuesOf?.('timeZone') ?? [browserTz]).filter(Boolean)
  const serverTzHint = ''

  // Run-completion watcher (step-105, operator feedback: "the button
  // just stays at test running and there is no feedback"). While a
  // test is in flight, poll config every 3s; on completion announce
  // the result and refresh the chart/derate data immediately instead
  // of waiting for the 60s poll.
  let runWatch = null
  let sawInFlight = false
  $: if (cfg?.run_in_flight && !runWatch) {
    sawInFlight = true
    runWatch = setInterval(refreshBandwidthConfig, 3000)
  }
  $: if (cfg && !cfg.run_in_flight && runWatch) {
    clearInterval(runWatch)
    runWatch = null
    if (sawInFlight) {
      sawInFlight = false
      refreshBandwidth()
      runMsg = 'test complete — see the bandwidth chart'
    }
  }
  onDestroy(() => { if (runWatch) clearInterval(runWatch) })

  // CLI install flow (step-123). POST kicks off the sudoers-backed
  // helper; poll every 3s until it lands (same feedback discipline as
  // the run-now watcher above). On success the server re-detects
  // capability immediately, so the next config refresh swaps this
  // card for the real fields.
  let cliInstall = { status: 'idle', output: '' }
  let cliTimer = null
  async function installCli() {
    try {
      cliInstall = { status: 'running', output: '' }
      await startBandwidthCliInstall()
      watchCli()
    } catch (err) {
      cliInstall = { status: 'failed', output: err.message ?? String(err) }
    }
  }
  function watchCli() {
    if (cliTimer) return
    cliTimer = setInterval(async () => {
      try {
        const st = await fetchBandwidthCliInstall()
        cliInstall = st
        if (st.status !== 'running') {
          clearInterval(cliTimer)
          cliTimer = null
          if (st.status === 'ok') refreshBandwidthConfig()
        }
      } catch {
        // transient poll failure — keep polling
      }
    }, 3000)
  }
  // Panel reopened mid-install (or another browser started one):
  // resume the watcher instead of showing a stale idle button.
  $: if (cfg && !cfg.capability.available && cliInstall.status === 'idle' && !cliTimer) {
    fetchBandwidthCliInstall().then((st) => {
      if (st.status === 'running') {
        cliInstall = st
        watchCli()
      } else if (st.status === 'failed') {
        cliInstall = st
      }
    }).catch(() => {})
  }
  onDestroy(() => { if (cliTimer) clearInterval(cliTimer) })

  async function patch(p) {
    patchError = ''
    try {
      await patchBandwidthConfig(p)
      await refreshBandwidthConfig()
    } catch (err) {
      patchError = err.message ?? String(err)
      await refreshBandwidthConfig() // re-sync inputs to server truth
    }
  }

  // --- scheduled times editor (max 6; design §6.3) ---
  function setTime(i, value) {
    const times = [...cfg.scheduled_times]
    if (value === null) {
      times.splice(i, 1)
      if (times.length === 0) return // server enforces ≥1; don't send empties
    } else {
      times[i] = value
    }
    patch({ scheduled_times: times })
  }
  function addTime() {
    // 12:00 default so the operator clearly sees they should pick a
    // time, not silently duplicate an existing one (design call).
    patch({ scheduled_times: [...cfg.scheduled_times, '12:00'] })
  }
  function commitTime(i, e) {
    const v = e.target.value.trim()
    if (!v || v === cfg.scheduled_times[i]) {
      e.target.value = cfg.scheduled_times[i] // fall back to committed
      return
    }
    setTime(i, v)
  }

  async function runNow() {
    runMsg = ''
    try {
      await runBandwidthTest()
      runMsg = 'test running — takes about 40 seconds…'
      await refreshBandwidthConfig()
    } catch (err) {
      runMsg = err.message ?? String(err)
    }
  }

  function fmtThreshold(v) {
    return Math.round(v * 100) + '%'
  }
</script>

{#if !cfg}
  <div class="muted">loading…</div>
{:else if !cfg.capability.available}
  <!-- CLI missing: one-click install (step-123 — the sudoers-backed
       helper), with the CLI alternative as fallback. -->
  <div class="install-help">
    <p><strong>speedtest CLI not found.</strong></p>
    <p class="muted">
      Bandwidth monitoring measures your link with scheduled speed
      tests (~250&nbsp;MB of transfer per test on gigabit). Hoptrail
      can install the Ookla® Speedtest® CLI for you (Debian and RPM
      families).
    </p>
    <button class="run" on:click={installCli} disabled={cliInstall.status === 'running'}>
      {cliInstall.status === 'running' ? 'Installing…' : 'Install the speedtest CLI'}
    </button>
    {#if cliInstall.status === 'failed'}
      <p class="error">Install failed — output below. You can also install by hand
        (<code class="cmd">./install.sh --add-bandwidth</code> or
        speedtest.net/apps/cli); hoptrail detects it within a minute.</p>
      <pre class="install-out">{cliInstall.output}</pre>
    {:else}
      <p class="muted small">
        Or by hand: <code class="cmd">./install.sh --add-bandwidth</code>
      </p>
    {/if}
    {#if cfg.capability.error}
      <p class="muted small" title={cfg.capability.error}>detection: {cfg.capability.error}</p>
    {/if}
  </div>
{:else}
  <div class="status-line">
    <span class="ok-dot"></span>
    speedtest {cfg.capability.version}
    {#if cfg.run_in_flight}<span class="in-flight">· test running…</span>{/if}
  </div>

  <label class="enable">
    <input
      type="checkbox"
      checked={cfg.enabled}
      on:change={(e) => patch({ enabled: e.target.checked })}
    />
    Enable scheduled tests
  </label>
  <p class="eula muted small">
    Tests run via the Ookla® Speedtest® CLI. By enabling or running
    tests you accept Ookla's
    <a href="https://www.speedtest.net/about/eula" target="_blank" rel="noopener">EULA</a>,
    <a href="https://www.speedtest.net/about/terms" target="_blank" rel="noopener">terms</a>, and
    <a href="https://www.speedtest.net/about/privacy" target="_blank" rel="noopener">privacy policy</a>
    — hoptrail passes the acceptance flags on your behalf.
  </p>

  <div class="fields" class:dimmed={!cfg.enabled}>
    <div class="field">
      <span class="label">Cadence</span>
      <label class="radio">
        <input type="radio" name="cadence" checked={cfg.cadence_mode === 'times'}
               on:change={() => patch({ cadence_mode: 'times' })} />
        at scheduled times
      </label>
      <label class="radio">
        <input type="radio" name="cadence" checked={cfg.cadence_mode === 'interval'}
               on:change={() => patch({ cadence_mode: 'interval' })} />
        every
        <input
          class="floor" type="number" min="15" max="1440" step="5"
          value={cfg.interval_minutes}
          on:blur={(e) => { const v = Number(e.target.value); if (Number.isInteger(v) && v !== cfg.interval_minutes) patch({ interval_minutes: v }) }}
          on:keydown={(e) => { if (e.key === 'Enter') e.target.blur() }}
        /> minutes
      </label>
      {#if cfg.cadence_mode === 'interval'}
        <span class="muted small">
          ≈ {Math.round(1440 / cfg.interval_minutes)} tests/day —
          roughly {(1440 / cfg.interval_minutes * 0.25).toFixed(1)} GB/day of transfer on gigabit
        </span>
      {/if}
    </div>

    <div class="field" class:dimmed={cfg.cadence_mode !== 'times'}>
      <span class="label">Scheduled times <span class="muted small">(local, 24h)</span></span>
      {#each cfg.scheduled_times as t, i (i)}
        <div class="time-row">
          <input
            class="time"
            value={t}
            placeholder="HH:MM"
            maxlength="5"
            on:blur={(e) => commitTime(i, e)}
            on:keydown={(e) => { if (e.key === 'Enter') e.target.blur(); if (e.key === 'Escape') { e.target.value = t; e.target.blur() } }}
          />
          {#if cfg.scheduled_times.length > 1}
            <button class="mini" title="remove this time" on:click={() => setTime(i, null)}>×</button>
          {/if}
        </div>
      {/each}
      {#if cfg.scheduled_times.length < 6}
        <button class="mini add" on:click={addTime}>+ add another time</button>
      {/if}
    </div>

    <div class="field">
      <span class="label">Timezone</span>
      <select
        value={cfg.timezone}
        on:change={(e) => patch({ timezone: e.target.value })}
      >
        <option value="">server local{serverTzHint}</option>
        {#if browserTz}
          <option value={browserTz}>{browserTz} (your browser)</option>
        {/if}
        {#each allTimezones as tz (tz)}
          {#if tz !== browserTz}<option value={tz}>{tz}</option>{/if}
        {/each}
      </select>
      <p class="muted small" style="margin:4px 0 0">
        Scheduled times are interpreted in this zone. "Server local"
        is whatever the daemon's host clock says — a server on UTC
        makes 02:00 mean 02:00 UTC.
      </p>
    </div>

    <div class="field">
      <span class="label">Derate detection applies to</span>
      {#each [['both', 'both directions'], ['down_only', 'download only'], ['up_only', 'upload only']] as [val, text] (val)}
        <label class="radio">
          <input type="radio" name="directions" checked={cfg.directions === val}
                 on:change={() => patch({ directions: val })} />
          {text}
        </label>
      {/each}
      <span class="muted small">both directions are always measured and charted</span>
    </div>

    <div class="field">
      <span class="label">Server selection</span>
      <label class="radio">
        <input type="radio" name="server" checked={cfg.server_mode === 'auto'}
               on:change={() => patch({ server_mode: 'auto' })} />
        auto (closest)
      </label>
      <label class="radio">
        <input type="radio" name="server" checked={cfg.server_mode === 'pinned'} disabled={cfg.server_id == null}
               on:change={() => patch({ server_mode: 'pinned' })} />
        pinned to ID
        <input
          class="server-id"
          value={cfg.server_id ?? ''}
          placeholder="ID"
          on:blur={(e) => {
            const v = parseInt(e.target.value, 10)
            if (Number.isInteger(v) && v !== cfg.server_id) patch({ server_id: v, server_mode: 'pinned' })
          }}
          on:keydown={(e) => { if (e.key === 'Enter') e.target.blur() }}
        />
      </label>
    </div>

    <div class="field">
      <span class="label">Derate threshold: {fmtThreshold(dragThresh ?? cfg.derate_threshold)} of baseline</span>
      <input
        type="range" min="0.1" max="0.9" step="0.05"
        value={cfg.derate_threshold}
        on:input={(e) => (dragThresh = Number(e.target.value))}
        on:change={async (e) => { await patch({ derate_threshold: Number(e.target.value) }); dragThresh = null }}
      />
      <span class="muted small">alert when a test drops below this fraction of normal</span>
    </div>

    <div class="field inline">
      <span class="label">Baseline window</span>
      <select value={String(cfg.baseline_days)} on:change={(e) => patch({ baseline_days: Number(e.target.value) })}>
        {#each [1, 3, 7, 14, 30] as d (d)}
          <option value={String(d)}>{d} day{d === 1 ? '' : 's'}</option>
        {/each}
      </select>
    </div>

    <div class="field inline">
      <span class="label">Baseline metric</span>
      <select value={cfg.baseline_metric} on:change={(e) => patch({ baseline_metric: e.target.value })}>
        <option value="median">median</option>
        <option value="trimmed_mean">trimmed mean</option>
      </select>
    </div>

    <div class="field inline">
      <span class="label">Min valid throughput</span>
      <input
        class="floor" type="number" min="1" max="1000" step="1"
        value={cfg.health_check_floor_mbps}
        on:blur={(e) => { const v = Number(e.target.value); if (v !== cfg.health_check_floor_mbps) patch({ health_check_floor_mbps: v }) }}
        on:keydown={(e) => { if (e.key === 'Enter') e.target.blur() }}
      /> Mbps
    </div>

    <label class="enable">
      <input type="checkbox" checked={cfg.pause_icmp_during_test}
             on:change={(e) => patch({ pause_icmp_during_test: e.target.checked })} />
      Pause latency probing during tests
    </label>
  </div>

  <!-- Run-now lives OUTSIDE the dimmed field group (operator
       feedback, step-104): the enable toggle gates SCHEDULED tests
       only — an on-demand test needs nothing but the CLI. -->
  <button class="run" on:click={runNow} disabled={cfg.run_in_flight}>
    {cfg.run_in_flight ? 'test running…' : 'Run a test now'}
  </button>
  {#if runMsg}<div class="muted small">{runMsg}</div>{/if}

  {#if patchError}
    <div class="error">{patchError}</div>
  {/if}
{/if}

<style>
  .muted { color: var(--fg-muted); font-size: 0.8rem; }
  .eula { margin: -4px 0 10px; line-height: 1.5; }
  .eula a { color: var(--fg-muted); }
  .small { font-size: 0.7rem; }
  .error { color: var(--danger, #e5484d); font-size: 0.75rem; margin-top: 8px; }
  .install-out {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    font-family: var(--font-mono);
    font-size: 0.68rem;
    line-height: 1.4;
    margin: 6px 0 0;
    max-height: 180px;
    overflow: auto;
    padding: 6px;
    white-space: pre-wrap;
    word-break: break-word;
  }

  .install-help p { margin: 6px 0; font-size: 0.8rem; }
  .cmd {
    display: block;
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 6px 8px;
    font-family: var(--font-mono);
    font-size: 0.75rem;
    user-select: all;
  }

  .status-line {
    display: flex;
    align-items: center;
    gap: 6px;
    font-family: var(--font-mono);
    font-size: 0.75rem;
    color: var(--fg-muted);
    margin: 4px 0 10px;
  }
  .ok-dot {
    width: 7px; height: 7px; border-radius: 50%;
    background: var(--ok, #4caf82);
  }
  .in-flight { color: var(--accent); }

  .enable {
    display: flex;
    align-items: center;
    gap: 7px;
    font-size: 0.85rem;
    margin: 6px 0 10px;
    cursor: pointer;
  }

  .fields.dimmed { opacity: 0.45; pointer-events: none; }
  .field { margin-bottom: 12px; display: flex; flex-direction: column; gap: 4px; }
  .field.inline { flex-direction: row; align-items: center; gap: 8px; }
  .label { font-size: 0.78rem; font-weight: 600; }

  input, select {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-family: var(--font-mono);
    font-size: 0.78rem;
    padding: 4px 6px;
  }
  input[type='checkbox'], input[type='radio'] { width: auto; padding: 0; }
  input[type='range'] { padding: 0; }

  .time-row { display: flex; gap: 6px; align-items: center; }
  .time { width: 5.5em; }
  .server-id { width: 5em; margin-left: 6px; }
  .floor { width: 6em; }

  .radio { display: flex; align-items: center; gap: 6px; font-size: 0.8rem; }

  .mini {
    background: transparent;
    border: none;
    color: var(--fg-muted);
    font-size: 0.75rem;
    cursor: pointer;
    padding: 2px 4px;
    text-align: left;
  }
  .mini:hover { color: var(--fg); }
  .mini.add { color: var(--accent); }

  .run {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.8rem;
    padding: 6px 10px;
    cursor: pointer;
  }
  .run:hover:not(:disabled) { border-color: var(--border-strong); background: var(--bg-elevated); }
  .run:disabled { opacity: 0.6; cursor: default; }
</style>
