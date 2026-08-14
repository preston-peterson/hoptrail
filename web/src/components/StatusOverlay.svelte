<script>
  // Environment status overlay (step-140, operator request — the
  // sibling project's status page translated to hoptrail's overlay
  // idiom): a card grid, one card per subsystem, each with a status
  // light. Quick-glance answers, details live elsewhere.
  import { statusOpen, statusStore, alertHistoryOpen } from '../lib/stores.js'

  // Jump from a named incident to its full story (raise time, prior
  // occurrences, recoveries) in the alert-history overlay.
  function openHistory() {
    alertHistoryOpen.set(true)
    close()
  }

  $: s = $statusStore

  function close() {
    statusOpen.set(false)
  }
  function onKeydown(e) {
    if ($statusOpen && e.key === 'Escape') {
      e.preventDefault()
      e.stopImmediatePropagation()
      close()
    }
  }

  function fmtUptime(sec) {
    if (sec == null) return '—'
    const d = Math.floor(sec / 86400)
    const h = Math.floor((sec % 86400) / 3600)
    const m = Math.floor((sec % 3600) / 60)
    return d > 0 ? `${d}d ${h}h` : h > 0 ? `${h}h ${m}m` : `${m}m`
  }
  function fmtBytes(b) {
    if (b == null) return '—'
    if (b > 1 << 30) return (b / (1 << 30)).toFixed(1) + ' GB'
    if (b > 1 << 20) return Math.round(b / (1 << 20)) + ' MB'
    return Math.round(b / 1024) + ' KB'
  }
  function fmtWhen(ms) {
    return ms == null ? 'never' : new Date(ms).toLocaleString([], { dateStyle: 'short', timeStyle: 'short' })
  }

  // Per-card light classes.
  $: probesDown = (s?.probes ?? []).filter((p) => !p.online)
  $: alertLight = !s ? 'unknown'
    : s.alerts.active_incidents > 0 ? 'red'
    : !s.alerts.configured ? 'off'
    : s.alerts.queue_depth > 0 || s.alerts.last_delivery_err ? 'amber'
    : 'green'
  $: bwLight = !s ? 'unknown'
    : !s.bandwidth.capability_available ? 'off'
    : s.bandwidth.derate ? 'amber'
    : 'green'
  $: dbLight = !s ? 'unknown'
    : s.database.health === 'critical' ? 'red'
    : s.database.health === 'warn' ? 'amber'
    : s.database.health === 'ok' ? 'green'
    : 'unknown'
  // An available update is informational (amber would read as a
  // problem) — the card light stays green; the row says it.
  $: updLight = !s ? 'unknown' : s.update.sudoers_ok ? 'green' : 'red'
</script>

<svelte:window on:keydown|capture={onKeydown} />

{#if $statusOpen}
  <div class="backdrop" on:click={close} aria-hidden="true"></div>
  <div class="overlay" role="dialog" aria-label="Environment status">
    <header>
      <h2>Status</h2>
      {#if s}<span class="muted">hoptrail {s.version} · up {fmtUptime(s.uptime_s)} · listening {s.listen}</span>{/if}
      <button class="close" on:click={close} title="close status">×</button>
    </header>

    {#if !s}
      <p class="loading">loading…</p>
    {:else}
      <div class="grid">
        <div class="card">
          <div class="card-head">
            <span class="light green"></span>
            <span class="title">Probe engine</span>
            <span class="sub">central</span>
          </div>
          <div class="rows">
            <div><span>targets monitored</span><b>{s.engine.targets.length}</b></div>
            {#each s.engine.targets.slice(0, 6) as t (t)}
              <div class="mono dim"><span>{t}</span></div>
            {/each}
            {#if s.engine.targets.length > 6}<div class="dim"><span>+{s.engine.targets.length - 6} more</span></div>{/if}
          </div>
        </div>

        <div class="card">
          <div class="card-head">
            <span class="light {probesDown.length > 0 ? 'red' : 'green'}"></span>
            <span class="title">Probes</span>
            <span class="sub">{s.probes.length} registered</span>
          </div>
          <div class="rows">
            {#each s.probes as p (p.probe_id)}
              <div>
                <span class="mono">{p.probe_id}</span>
                {#if p.outdated}<span class="chip" title="runs an older release than the central — update from the Probes panel">outdated</span>{/if}
                <b class={p.online ? 'ok' : 'bad'}>{p.online ? 'online' : 'offline'}</b>
              </div>
              <div class="dim indent">
                <span>{[p.ip, p.version, p.last_seen_at ? `seen ${fmtWhen(p.last_seen_at)}` : ''].filter(Boolean).join(' · ')}</span>
              </div>
            {/each}
          </div>
        </div>

        <div class="card">
          <div class="card-head">
            <span class="light {dbLight}"></span>
            <span class="title">Database</span>
            <span class="sub">SQLite</span>
          </div>
          <div class="rows">
            <div><span>on disk</span><b>{fmtBytes(s.database.size_bytes)}</b></div>
            <div>
              <span>free disk</span>
              <b>{fmtBytes(s.database.free_bytes)}{s.database.total_bytes ? ` of ${fmtBytes(s.database.total_bytes)}` : ''}</b>
            </div>
            <div>
              <span>growth</span>
              <b>{#if s.database.days_of_data > 0}{Math.round(s.database.growth_mb_per_day)} MB/day{:else}collecting…{/if}</b>
            </div>
            {#if s.database.headroom_ratio != null}
              <div><span>headroom</span><b>{s.database.headroom_ratio.toFixed(2)}×</b></div>
            {/if}
            <div><span>retention</span><b>{s.database.retention_days} days</b></div>
            <div><span>schema</span><b>v{s.database.schema_version}</b></div>
          </div>
        </div>

        <div class="card">
          <div class="card-head">
            <span class="light {alertLight}"></span>
            <span class="title">Alerts</span>
            <span class="sub">{s.alerts.enabled ? 'enabled' : s.alerts.configured ? 'configured, off' : 'not set up'}</span>
          </div>
          <div class="rows">
            <div><span>active incidents</span><b class={s.alerts.active_incidents > 0 ? 'bad' : ''}>{s.alerts.active_incidents}</b></div>
            {#each s.alerts.active ?? [] as inc (inc.event + inc.subject)}
              <div class="incident">
                <button class="incident-btn" on:click={openHistory} title="open alert history">
                  <span class="inc-what">{inc.event.replaceAll('_', ' ')} · {inc.subject}</span>
                  <span class="inc-since">since {fmtWhen(inc.since)}</span>
                </button>
              </div>
            {/each}
            <div><span>delivery queue</span><b>{s.alerts.queue_depth}</b></div>
            <div><span>last delivery</span><b>{fmtWhen(s.alerts.last_delivery_at)}{s.alerts.last_delivery_err ? ' ✗' : ''}</b></div>
            {#if s.alerts.last_delivery_err}<div class="dim"><span>{s.alerts.last_delivery_err}</span></div>{/if}
          </div>
        </div>

        <div class="card">
          <div class="card-head">
            <span class="light {bwLight}"></span>
            <span class="title">Bandwidth</span>
            <span class="sub">{s.bandwidth.capability_available ? (s.bandwidth.enabled ? 'scheduled' : 'manual only') : 'CLI not installed'}</span>
          </div>
          <div class="rows">
            <div><span>last test</span><b>{fmtWhen(s.bandwidth.last_test_ts)}{s.bandwidth.last_test_ts != null && !s.bandwidth.last_test_ok ? ' (failed)' : ''}</b></div>
            <div>
              <span>baseline</span>
              <b>{#if s.bandwidth.baseline_down_mbps != null}{Math.round(s.bandwidth.baseline_down_mbps)} ↓ / {Math.round(s.bandwidth.baseline_up_mbps)} ↑ Mbps{:else}establishing…{/if}</b>
            </div>
            <div><span>derate</span><b class={s.bandwidth.derate ? 'bad' : ''}>{s.bandwidth.derate ? 'ACTIVE' : 'no'}</b></div>
          </div>
        </div>

        <div class="card">
          <div class="card-head">
            <span class="light {updLight}"></span>
            <span class="title">Updates</span>
            <span class="sub">self-update</span>
          </div>
          <div class="rows">
            <div><span>running</span><b class="mono">{s.version}</b></div>
            {#if s.update.update_available}
              <div><span>available</span><b class="ok mono">v{s.update.latest_version}</b></div>
            {:else if s.update.latest_version}
              <div><span>latest</span><b class="mono">v{s.update.latest_version} (up to date)</b></div>
            {/if}
            <div><span>staged</span><b class="mono">{s.update.staged_present ? (s.update.staged_version || 'yes') : 'none'}</b></div>
            <div><span>sudoers</span><b class={s.update.sudoers_ok ? 'ok' : 'bad'}>{s.update.sudoers_ok ? 'ok' : 'needs install.sh'}</b></div>
            {#if s.update.sudoers_err}<div class="dim"><span>{s.update.sudoers_err}</span></div>{/if}
          </div>
        </div>
      </div>
    {/if}
  </div>
{/if}

<style>
  .backdrop {
    position: fixed; inset: 0;
    background: rgba(0, 0, 0, 0.45);
    z-index: 60;
  }
  .overlay {
    position: fixed;
    inset: 7vh 8vw;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    box-shadow: 0 12px 40px rgba(0, 0, 0, 0.45);
    z-index: 61;
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }
  header {
    display: flex;
    align-items: baseline;
    gap: 12px;
    padding: 12px 16px;
    border-bottom: 1px solid var(--border);
    flex: 0 0 auto;
  }
  h2 { margin: 0; font-size: 0.95rem; font-weight: 600; }
  .muted { color: var(--fg-muted); font-size: 0.72rem; font-family: var(--font-mono); }
  .close {
    margin-left: auto;
    background: transparent; border: none;
    color: var(--fg-muted); font-size: 1.2rem; line-height: 1;
    cursor: pointer; padding: 2px 6px;
  }
  .close:hover { color: var(--fg); }
  .loading { color: var(--fg-muted); padding: 16px; }

  .grid {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(260px, 1fr));
    gap: 12px;
    padding: 14px 16px;
    align-content: start;
  }
  .card {
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 10px 12px;
  }
  .card-head {
    display: flex;
    align-items: baseline;
    gap: 8px;
    margin-bottom: 8px;
  }
  .light {
    width: 9px; height: 9px; border-radius: 50%;
    align-self: center;
    background: var(--fg-subtle);
  }
  .light.green { background: var(--ok, #30a46c); }
  .light.amber { background: var(--warning, #f5a524); }
  .light.red   { background: var(--danger, #e5484d); }
  .light.off   { background: var(--fg-subtle); opacity: 0.5; }
  .title { font-size: 0.82rem; font-weight: 600; }
  .sub { color: var(--fg-subtle); font-size: 0.68rem; margin-left: auto; }

  .rows { display: flex; flex-direction: column; gap: 3px; font-size: 0.75rem; }
  .rows > div { display: flex; justify-content: space-between; gap: 8px; color: var(--fg-muted); }
  .rows b { color: var(--fg); font-weight: 500; }
  .rows .ok { color: var(--ok, #30a46c); }
  .rows .bad { color: var(--danger, #e5484d); }
  .mono { font-family: var(--font-mono); }
  .dim { color: var(--fg-subtle); font-size: 0.68rem; }

  /* Named active incidents under the count — each is a jump into the
     alert-history overlay. The button is the row's sole flex child,
     stretched so the whole line is the click target. */
  .incident-btn {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 8px;
    flex: 1;
    min-width: 0;
    background: color-mix(in srgb, var(--danger, #e5484d) 8%, transparent);
    border: 1px solid color-mix(in srgb, var(--danger, #e5484d) 30%, transparent);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.72rem;
    text-align: left;
    padding: 3px 7px;
    cursor: pointer;
  }
  .incident-btn:hover { border-color: var(--danger, #e5484d); }
  .inc-what { font-weight: 500; }
  .inc-since { color: var(--fg-muted); font-size: 0.66rem; white-space: nowrap; }
  .chip {
    color: var(--warning, #f5a524);
    border: 1px solid color-mix(in srgb, var(--warning, #f5a524) 45%, transparent);
    border-radius: 999px;
    font-size: 0.62rem;
    padding: 0 6px;
    margin-left: 4px;
  }
  .indent { padding-left: 10px; }
</style>
