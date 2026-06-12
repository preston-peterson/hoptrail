<script>
  // Settings slide-out (step-101) — the design's §6.3 "new UX
  // infrastructure": gear icon top-right, panel slides in from the
  // right edge, collapsible sections. Bandwidth is the only section
  // in v0.4; future settings (retention editor, per-tab defaults)
  // get sections here instead of new one-off surfaces.
  import { settingsOpen, refreshBandwidthConfig, retentionDays, docsOpen } from '../lib/stores.js'
  import { patchRetention, fetchRetention } from '../lib/api.js'
  import BandwidthSettings from './BandwidthSettings.svelte'
  import ProbeSettings from './ProbeSettings.svelte'
  import UpdateSettings from './UpdateSettings.svelte'
  import SystemSettings from './SystemSettings.svelte'
  import AlertSettings from './AlertSettings.svelte'
  import AboutSettings from './AboutSettings.svelte'

  // Sections collapse independently — ALL closed by default (operator
  // feedback: bandwidth opening expanded made the panel feel like the
  // bandwidth panel).
  let bandwidthOpen = false
  let probesOpen = false
  let alertsOpen = false
  let retentionOpen = false
  let systemOpen = false
  let updatesOpen = false
  let aboutOpen = false
  let retentionError = ''

  async function commitRetention(e) {
    retentionError = ''
    const v = Number(e.target.value)
    if (!Number.isInteger(v) || v === $retentionDays) return
    try {
      await patchRetention(v)
      const data = await fetchRetention()
      retentionDays.set(data?.retention_days ?? v)
    } catch (err) {
      retentionError = err.message ?? String(err)
      e.target.value = $retentionDays
    }
  }

  // Fetch fresh config every time the panel opens — server is the
  // single source of truth (no localStorage for any bandwidth state).
  $: if ($settingsOpen) refreshBandwidthConfig()

  function close() {
    settingsOpen.set(false)
  }
  function onKeydown(e) {
    if ($settingsOpen && e.key === 'Escape') {
      e.preventDefault()
      close()
    }
  }
</script>

<svelte:window on:keydown={onKeydown} />

{#if $settingsOpen}
  <!-- Backdrop: click closes. Subtle dim so the dashboard stays
       readable behind the panel (the design wants context preserved). -->
  <div class="backdrop" on:click={close} aria-hidden="true"></div>

  <aside class="panel" aria-label="Settings">
    <header>
      <h2>Settings</h2>
      <button class="close" on:click={close} title="close settings">×</button>
    </header>

    <section>
      <button class="section-head" on:click={() => (bandwidthOpen = !bandwidthOpen)}>
        <span class="disclosure">{bandwidthOpen ? '▾' : '▸'}</span>
        Bandwidth monitoring
      </button>
      {#if bandwidthOpen}
        <div class="section-body">
          <BandwidthSettings />
        </div>
      {/if}
    </section>

    <section>
      <button class="section-head" on:click={() => (probesOpen = !probesOpen)}>
        <span class="disclosure">{probesOpen ? '▾' : '▸'}</span>
        Probes (other sites)
      </button>
      {#if probesOpen}
        <div class="section-body">
          <ProbeSettings />
        </div>
      {/if}
    </section>

    <section>
      <button class="section-head" on:click={() => (alertsOpen = !alertsOpen)}>
        <span class="disclosure">{alertsOpen ? '▾' : '▸'}</span>
        Alerts
      </button>
      {#if alertsOpen}
        <div class="section-body">
          <AlertSettings />
        </div>
      {/if}
    </section>

    <section>
      <button class="section-head" on:click={() => (retentionOpen = !retentionOpen)}>
        <span class="disclosure">{retentionOpen ? '▾' : '▸'}</span>
        Data retention
      </button>
      {#if retentionOpen}
        <div class="section-body retention">
          <label class="ret-row">
            Keep samples for
            <input
              type="number" min="1" max="3650" step="1"
              value={$retentionDays ?? ''}
              on:blur={commitRetention}
              on:keydown={(e) => { if (e.key === 'Enter') e.target.blur() }}
            /> days
          </label>
          <p class="ret-hint">
            Older latency samples and route changes are pruned hourly.
            Annotations and speed test results are kept forever (tiny —
            even years of tests stay under a few MB). Applies live — no
            restart.
            Rough disk math: ~450&nbsp;MB per 7 days per probed target
            at 1s cadence.
          </p>
          {#if retentionError}<p class="ret-error">{retentionError}</p>{/if}
        </div>
      {/if}
    </section>

    <section>
      <button class="section-head" on:click={() => (systemOpen = !systemOpen)}>
        <span class="disclosure">{systemOpen ? '▾' : '▸'}</span>
        System
      </button>
      {#if systemOpen}
        <div class="section-body">
          <SystemSettings />
        </div>
      {/if}
    </section>

    <section>
      <button class="section-head" on:click={() => (updatesOpen = !updatesOpen)}>
        <span class="disclosure">{updatesOpen ? '▾' : '▸'}</span>
        Updates
      </button>
      {#if updatesOpen}
        <div class="section-body">
          <UpdateSettings />
        </div>
      {/if}
    </section>

    <section>
      <!-- Step-143: not a collapsible section — the head itself opens
           the full documentation overlay. -->
      <button class="section-head" on:click={() => { docsOpen.set(true); close() }}>
        <span class="disclosure">↗</span>
        Documentation
      </button>
    </section>

    <section>
      <button class="section-head" on:click={() => (aboutOpen = !aboutOpen)}>
        <span class="disclosure">{aboutOpen ? '▾' : '▸'}</span>
        About
      </button>
      {#if aboutOpen}
        <div class="section-body">
          <AboutSettings />
        </div>
      {/if}
    </section>
  </aside>
{/if}

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.25);
    z-index: 40;
  }

  .panel {
    position: fixed;
    top: 0;
    right: 0;
    bottom: 0;
    width: min(340px, 100vw);
    background: var(--bg-elevated);
    border-left: 1px solid var(--border);
    box-shadow: -8px 0 24px rgba(0, 0, 0, 0.3);
    z-index: 41;
    display: flex;
    flex-direction: column;
    overflow-y: auto;
    animation: slide-in 200ms ease-out;
  }
  @keyframes slide-in {
    from { transform: translateX(100%); }
    to   { transform: translateX(0); }
  }

  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 12px 14px;
    border-bottom: 1px solid var(--border);
  }
  h2 {
    margin: 0;
    font-size: 0.95rem;
    font-weight: 600;
  }
  .close {
    background: transparent;
    border: none;
    color: var(--fg-muted);
    font-size: 1.2rem;
    line-height: 1;
    cursor: pointer;
    padding: 2px 6px;
  }
  .close:hover { color: var(--fg); }

  section { border-bottom: 1px solid var(--border); }
  .section-head {
    display: flex;
    align-items: center;
    gap: 7px;
    width: 100%;
    background: transparent;
    border: none;
    color: var(--fg);
    font-size: 0.85rem;
    font-weight: 600;
    text-align: left;
    padding: 10px 14px;
    cursor: pointer;
  }
  .section-head:hover { background: var(--bg-sunken); }
  .disclosure {
    color: var(--fg-subtle);
    font-size: 0.75rem;
    width: 1em;
  }
  .section-body { padding: 4px 14px 14px; }
  .ret-row {
    display: flex;
    align-items: center;
    gap: 7px;
    font-size: 0.85rem;
  }
  .ret-row input {
    width: 5.5em;
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-family: var(--font-mono);
    font-size: 0.78rem;
    padding: 4px 6px;
  }
  .ret-hint { color: var(--fg-muted); font-size: 0.72rem; margin: 8px 0 0; }
  .ret-error { color: var(--danger, #e5484d); font-size: 0.75rem; margin: 6px 0 0; }
</style>
