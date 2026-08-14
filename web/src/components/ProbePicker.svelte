<script>
  // Probe picker (step-94; per-tab since step-96). Edits the ACTIVE
  // tab's probe — whose measurements that tab displays (chart, hop
  // list, route changes, export). The choice is stored on the tab row
  // server-side, so it survives browsers/devices and rides bundles,
  // like labels and thresholds. Same collapsed-trigger +
  // right-anchored-popover shape as the other pickers (step-38).
  //
  // Renders nothing until a second probe exists — a zero-probe deploy
  // sees a byte-identical StatusBar. Online state comes from the
  // server (last heartbeat within 3× the default interval).

  import { probesStore, activeProbeId, setActiveProbe } from '../lib/stores.js'

  let open = false

  function toggle() { open = !open }
  function close() { open = false }

  function pick(id) {
    setActiveProbe(id)
    close()
  }

  function onKeydown(e) {
    if (!open) return
    if (e.key === 'Escape') { e.preventDefault(); close() }
  }

  function handleWindowClick(e) {
    if (!open) return
    if (e.target.closest?.('.probe-root')) return
    close()
  }

  function displayName(p) {
    return p.label || p.probe_id
  }

  function probeTitle(p) {
    const bits = [p.probe_id]
    if (p.ip) bits.push(p.ip)
    if (p.version) bits.push(p.version)
    if (p.last_seen_at) bits.push('last seen ' + new Date(p.last_seen_at).toLocaleTimeString())
    bits.push(p.online ? 'online' : 'offline')
    return bits.join(' · ')
  }

  $: active = $probesStore.find((p) => p.probe_id === $activeProbeId)
</script>

<svelte:window on:click={handleWindowClick} on:keydown={onKeydown} />

{#if $probesStore.length > 1}
  <div class="probe-root">
    <button
      class="trigger"
      class:open
      on:click={toggle}
      title="probe — which site's measurements this tab displays (per-tab, saved with the tab)"
    >
      <span class="dot" class:online={active?.online ?? true}></span>
      <span class="prefix">probe</span>
      <span class="value">{active ? displayName(active) : $activeProbeId}</span>
      <span class="caret">{open ? '▴' : '▾'}</span>
    </button>

    {#if open}
      <div class="menu" role="listbox" aria-label="probe">
        <div class="menu-header">this tab measures from</div>
        {#each $probesStore as p (p.probe_id)}
          <button
            class="option"
            class:active={$activeProbeId === p.probe_id}
            role="option"
            aria-selected={$activeProbeId === p.probe_id}
            title={probeTitle(p)}
            on:click={() => pick(p.probe_id)}
          >
            <span class="dot" class:online={p.online}></span>
            <span class="option-label">{displayName(p)}</span>
            {#if $activeProbeId === p.probe_id}
              <span class="option-mark">●</span>
            {/if}
          </button>
        {/each}
      </div>
    {/if}
  </div>
{/if}

<style>
  .probe-root {
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
  .trigger .value { font-weight: 600; }
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

  /* Online/offline indicator. Gray (offline) is deliberately quiet —
     an offline agent isn't an alarm, just information. */
  .dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--fg-subtle);
    flex-shrink: 0;
  }
  .dot.online { background: var(--ok, #4caf82); }

  .menu {
    position: absolute;
    top: calc(100% + 4px);
    right: 0;
    min-width: 160px;
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
    gap: 7px;
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
  .option-label {
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .option-mark {
    color: var(--accent);
    font-size: 0.7rem;
  }
</style>
