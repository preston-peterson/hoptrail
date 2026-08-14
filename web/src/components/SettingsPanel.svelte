<script>
  // Settings overlay (step-101 slide-out → step-186 tabbed redesign,
  // operator: "the gear is too cluttered — every configuration area
  // should have its own mini page"). Same chrome as DocsOverlay so
  // the two big surfaces feel like one app: backdrop, centered
  // overlay, left nav, content pane. One nav entry per category; the
  // active category renders alone in a roomy page instead of the old
  // 340px accordion strip. settingsTab is the deep-link hook —
  // "Enable in settings" banners land directly on the Bandwidth page.
  import {
    settingsOpen, settingsTab, refreshBandwidthConfig, retentionDays,
    docsOpen, theme, setTheme,
  } from '../lib/stores.js'
  import { patchRetention, fetchRetention } from '../lib/api.js'
  import BandwidthSettings from './BandwidthSettings.svelte'
  import ProbeSettings from './ProbeSettings.svelte'
  import UpdateSettings from './UpdateSettings.svelte'
  import SystemSettings from './SystemSettings.svelte'
  import AlertSettings from './AlertSettings.svelte'
  import AboutSettings from './AboutSettings.svelte'

  // One entry per category page. `component: null` marks the pages
  // whose markup lives in this file (retention is a two-field form —
  // not worth its own component).
  const SECTIONS = [
    { id: 'bandwidth', label: 'Bandwidth', component: BandwidthSettings,
      desc: 'Scheduled speed tests, rolling baseline, and degradation detection.' },
    { id: 'probes', label: 'Probes', component: ProbeSettings,
      desc: 'Remote probe sites reporting into this server — add, revoke, forget.' },
    { id: 'alerts', label: 'Alerts', component: AlertSettings,
      desc: 'Where and when hoptrail notifies you about path problems.' },
    { id: 'retention', label: 'Data retention', component: null,
      desc: 'How long latency samples and route changes are kept on disk.' },
    { id: 'system', label: 'System', component: SystemSettings,
      desc: 'Log level, listen address, reverse DNS — the last daemon knobs.' },
    { id: 'updates', label: 'Updates', component: UpdateSettings,
      desc: 'Check for new releases, download or upload, apply with rollback.' },
    { id: 'about', label: 'About', component: AboutSettings,
      desc: 'Version, license, and what hoptrail is built with.' },
  ]

  // Unknown tab id (stale deep-link) falls back to the first page.
  $: active = SECTIONS.find((s) => s.id === $settingsTab) ?? SECTIONS[0]

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
  <div class="backdrop" on:click={close} aria-hidden="true"></div>

  <div class="overlay" role="dialog" aria-label="Settings">
    <header>
      <h2>Settings</h2>
      <span class="muted">changes apply immediately — no save button</span>
      <button class="close" on:click={close} title="close settings">×</button>
    </header>

    <div class="split">
      <nav aria-label="Settings sections">
        {#each SECTIONS as s (s.id)}
          <button
            class="tab"
            class:active={active.id === s.id}
            on:click={() => settingsTab.set(s.id)}
          >{s.label}</button>
        {/each}

        <button class="tab external" on:click={() => { docsOpen.set(true); close() }}>
          Documentation <span class="ext-mark">↗</span>
        </button>

        <!-- Theme: pinned at the foot of the nav as a segmented
             control. Per-browser; no server round trip. Sole theme
             control — not duplicated elsewhere. -->
        <div class="theme-foot">
          <span class="theme-label">Theme</span>
          <div class="seg" role="group" aria-label="Theme">
            {#each [['auto', 'Auto'], ['light', 'Light'], ['dark', 'Dark']] as [value, label]}
              <button
                class="seg-btn"
                class:active={$theme === value}
                aria-pressed={$theme === value}
                on:click={() => setTheme(value)}
              >{label}</button>
            {/each}
          </div>
        </div>
      </nav>

      <div class="page">
        <div class="page-head">
          <h3>{active.label}</h3>
          <p class="page-desc">{active.desc}</p>
        </div>

        {#if active.id === 'retention'}
          <div class="retention">
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
        {:else}
          <svelte:component this={active.component} />
        {/if}
      </div>
    </div>
  </div>
{/if}

<style>
  .backdrop {
    position: fixed; inset: 0;
    background: rgba(0, 0, 0, 0.45);
    z-index: 40;
  }
  .overlay {
    position: fixed;
    inset: 5vh 0;
    margin: 0 auto;
    width: min(880px, 92vw);
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    box-shadow: 0 12px 40px rgba(0, 0, 0, 0.45);
    z-index: 41;
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
  .muted { color: var(--fg-subtle); font-size: 0.68rem; }
  .close {
    margin-left: auto;
    background: transparent; border: none;
    color: var(--fg-muted); font-size: 1.2rem; line-height: 1;
    cursor: pointer; padding: 2px 6px;
  }
  .close:hover { color: var(--fg); }

  .split { display: flex; flex: 1; min-height: 0; }

  nav {
    flex: 0 0 180px;
    border-right: 1px solid var(--border);
    padding: 10px 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
    overflow-y: auto;
  }
  .tab {
    background: transparent;
    border: none;
    border-left: 2px solid transparent;
    color: var(--fg-muted);
    font-size: 0.8rem;
    text-align: left;
    padding: 7px 14px;
    cursor: pointer;
  }
  .tab:hover { color: var(--fg); background: var(--bg-sunken); }
  .tab.active { color: var(--fg); border-left-color: var(--accent, #4f8ff7); background: var(--bg-sunken); }
  .tab.external { margin-top: 6px; border-top: 1px solid var(--border); padding-top: 12px; }
  .ext-mark { color: var(--fg-subtle); font-size: 0.72rem; }

  .page {
    flex: 1;
    min-width: 0;
    overflow-y: auto;
    padding: 16px 22px 40px;
  }
  .page-head {
    margin-bottom: 14px;
    padding-bottom: 10px;
    border-bottom: 1px solid var(--border);
  }
  h3 { margin: 0 0 3px; font-size: 1rem; font-weight: 600; }
  .page-desc { margin: 0; color: var(--fg-muted); font-size: 0.75rem; }

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
  .ret-hint { color: var(--fg-muted); font-size: 0.72rem; margin: 8px 0 0; max-width: 52ch; }
  .ret-error { color: var(--danger, #e5484d); font-size: 0.75rem; margin: 6px 0 0; }

  /* Theme footer — pinned to the bottom of the nav column. */
  .theme-foot {
    margin-top: auto;
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 12px 14px 4px;
    border-top: 1px solid var(--border);
  }
  .theme-label {
    font-size: 0.68rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--fg-subtle);
  }
  .seg {
    display: inline-flex;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    overflow: hidden;
    align-self: flex-start;
  }
  .seg-btn {
    background: transparent;
    border: none;
    color: var(--fg-muted);
    font-size: 0.75rem;
    padding: 4px 10px;
    cursor: pointer;
  }
  .seg-btn:not(:last-child) { border-right: 1px solid var(--border); }
  .seg-btn:hover:not(.active) { background: var(--bg-sunken); color: var(--fg); }
  .seg-btn.active {
    background: var(--accent, #4f8ff7);
    color: #fff;
  }

  /* Narrow screens: nav folds into a horizontal scroll row on top so
     the page keeps full width. Theme foot joins the row inline. */
  @media (max-width: 640px) {
    .overlay { inset: 2vh 0; width: 96vw; }
    .split { flex-direction: column; }
    nav {
      flex: 0 0 auto;
      flex-direction: row;
      align-items: center;
      overflow-x: auto;
      border-right: none;
      border-bottom: 1px solid var(--border);
      padding: 6px 8px;
    }
    .tab { border-left: none; border-bottom: 2px solid transparent; white-space: nowrap; }
    .tab.active { border-left-color: transparent; border-bottom-color: var(--accent, #4f8ff7); }
    .tab.external { margin-top: 0; border-top: none; padding-top: 7px; }
    .theme-foot { margin-top: 0; margin-left: auto; border-top: none; padding: 0 6px; flex-direction: row; align-items: center; }
    .theme-label { display: none; }
  }
</style>
