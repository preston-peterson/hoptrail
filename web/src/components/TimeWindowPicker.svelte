<script>
  // Top-level time-window selector (step-24). Sits in the StatusBar
  // because it's a global control: changes drive both the main chart
  // and the per-hop sparklines via the shared samplesStore. Putting
  // it inside the chart card would imply it only affects the chart.
  //
  // The window options and polling cadences are defined in stores.js;
  // this component is purely a presentational picker.
  //
  // Step-38 (planned same-day follow-up): collapsed-by-default. Same
  // shape as IntervalPicker — single "view Nm ▾" trigger button that
  // opens a right-anchored popover. The expanded pill row + the
  // adjacent interval picker were starting to dominate the StatusBar
  // visually; collapsing both reclaims ~58% of the right cluster.

  import { timeWindow, TIME_WINDOW_KEYS, TIME_WINDOWS, setTimeWindow, retentionDays } from '../lib/stores.js'

  let open = false

  // Step-97: how far back stats actually go. Windows wider than the
  // retention policy still work — they just can't fill completely —
  // so they get a subtle marker rather than being disabled.
  $: retentionMs = $retentionDays != null ? $retentionDays * 86_400_000 : null
  function exceedsHistory(key) {
    return retentionMs != null && (TIME_WINDOWS[key]?.ms ?? 0) > retentionMs
  }

  function toggle() { open = !open }
  function close() { open = false }

  function pick(key) {
    setTimeWindow(key)
    close()
  }

  function onKeydown(e) {
    if (!open) return
    if (e.key === 'Escape') { e.preventDefault(); close() }
  }

  function handleWindowClick(e) {
    if (!open) return
    if (e.target.closest?.('.window-root')) return
    close()
  }
</script>

<svelte:window on:click={handleWindowClick} on:keydown={onKeydown} />

<div class="window-root">
  <button
    class="trigger"
    class:open
    on:click={toggle}
    title="time window — drives the chart's horizontal axis and the per-hop sparklines"
  >
    <span class="prefix">view</span>
    <span class="value">{$timeWindow}</span>
    <span class="caret">{open ? '▴' : '▾'}</span>
  </button>

  {#if open}
    <div class="menu" role="listbox" aria-label="time window">
      <div class="menu-header">show last</div>
      {#each TIME_WINDOW_KEYS as key (key)}
        <button
          class="option"
          class:active={$timeWindow === key}
          role="option"
          aria-selected={$timeWindow === key}
          title={exceedsHistory(key) ? `wider than the ${$retentionDays}-day retention window — older samples have been pruned` : undefined}
          on:click={() => pick(key)}
        >
          <span class="option-label">{key}</span>
          {#if exceedsHistory(key)}
            <span class="option-hist" aria-hidden="true">›hist</span>
          {/if}
          {#if $timeWindow === key}
            <span class="option-mark">●</span>
          {/if}
        </button>
      {/each}
      {#if $retentionDays != null}
        <!-- Step-97: answer "how far back can I scroll?" right where
             the operator is thinking about time. The setting itself
             stays in config.yaml until the v0.4 settings panel. -->
        <div class="menu-footer" title="storage.retention_days — samples older than this are pruned hourly; annotations are kept forever">
          history: {$retentionDays} day{$retentionDays === 1 ? '' : 's'}
        </div>
      {/if}
    </div>
  {/if}
</div>

<style>
  .window-root {
    position: relative;
    display: inline-flex;
  }

  .trigger {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-family: var(--font-mono);
    font-size: 0.75rem;
    line-height: 1;
    padding: 5px 8px;
    cursor: pointer;
    transition: color 80ms ease-out, border-color 80ms ease-out, background 80ms ease-out;
  }
  .trigger:hover,
  .trigger.open {
    border-color: var(--border-strong);
    background: var(--bg-elevated);
  }
  .trigger .prefix { color: var(--fg-muted); }
  .trigger .value {
    font-weight: 600;
    min-width: 2.4ch;
    text-align: right;
  }
  .trigger .caret {
    color: var(--fg-subtle);
    font-size: 0.7rem;
  }
  .trigger.open .caret { color: var(--accent); }
  .trigger:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: -1px;
  }
  .trigger:focus { outline: none; }

  /* Popover — right-anchored same as IntervalPicker. */
  .menu {
    position: absolute;
    top: calc(100% + 4px);
    right: 0;
    min-width: 120px;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    box-shadow: 0 4px 16px rgba(0, 0, 0, 0.35);
    padding: 4px 0;
    z-index: 25;
    max-height: 320px;
    overflow-y: auto;
  }
  .menu-header {
    color: var(--fg-subtle);
    font-family: var(--font-mono);
    font-size: 0.65rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    padding: 4px 10px;
  }

  .option {
    display: flex;
    align-items: center;
    justify-content: space-between;
    width: 100%;
    background: transparent;
    border: none;
    color: var(--fg-muted);
    font-family: var(--font-mono);
    font-size: 0.8rem;
    padding: 4px 10px;
    cursor: pointer;
    text-align: left;
  }
  .option:hover { background: var(--bg-sunken); color: var(--fg); }
  .option.active {
    color: var(--fg);
    background: var(--bg-sunken);
  }
  .option.active .option-label { font-weight: 600; }
  .option-mark {
    color: var(--accent);
    font-size: 0.7rem;
  }

  /* Step-97: marker for windows wider than the retained history. */
  .option-hist {
    color: var(--fg-subtle);
    font-size: 0.65rem;
    letter-spacing: 0.03em;
  }

  .menu-footer {
    border-top: 1px solid var(--border);
    margin-top: 4px;
    padding: 5px 10px 2px;
    color: var(--fg-subtle);
    font-family: var(--font-mono);
    font-size: 0.65rem;
    cursor: default;
  }
</style>
