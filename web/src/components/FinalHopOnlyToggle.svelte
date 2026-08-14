<script>
  // Per-tab final-hop-only toggle (step-41). Sits in the chart card
  // header next to the linear/log toggle and ThresholdsPicker. When
  // enabled, the daemon's pinger skips intermediate TTLs and only
  // probes the destination — drops outgoing probe traffic by ~95%
  // on long paths in exchange for losing per-hop sample density.
  // Discovery still runs so route changes are detected.
  //
  // Same compact-toggle shape as the linear/log button. Trigger a
  // brief pipeline rebuild server-side so we keep the success state
  // optimistic and roll back on error.

  import { activeTarget, targetFinalHopOnly } from '../lib/stores.js'
  import { setTargetFinalHopOnly } from '../lib/api.js'

  let pending = false
  let error = null

  $: enabled = $activeTarget ? ($targetFinalHopOnly[$activeTarget] ?? false) : false
  $: disabled = !$activeTarget || pending

  async function toggle() {
    if (!$activeTarget) return
    const next = !enabled
    pending = true
    error = null
    // Optimistic flip so the button label changes immediately.
    targetFinalHopOnly.update((map) => ({ ...map, [$activeTarget]: next }))
    try {
      await setTargetFinalHopOnly($activeTarget, next)
    } catch (err) {
      // Roll back on failure.
      targetFinalHopOnly.update((map) => ({ ...map, [$activeTarget]: !next }))
      error = err.message ?? String(err)
    } finally {
      pending = false
    }
  }
</script>

<button
  class="toggle"
  class:on={enabled}
  class:pending
  {disabled}
  on:click={toggle}
  title={error ?? (enabled
    ? 'final-hop-only ON — only the destination is being pinged. Click to resume per-hop probing.'
    : 'click to switch this tab to final-hop-only mode (saves bandwidth; sacrifices per-hop sample density)')}
>
  {enabled ? '◉ dst only' : '◯ all hops'}
</button>

<style>
  /* Matches the visual weight of LatencyTimeline's .scale-toggle so
     the three header buttons read as a row of equivalent controls. */
  .toggle {
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    padding: 2px 8px;
    font-family: var(--font-mono);
    font-size: 0.75rem;
    line-height: 1;
    cursor: pointer;
    transition: color 80ms ease-out, border-color 80ms ease-out, background 80ms ease-out;
  }
  .toggle:hover:not(:disabled) {
    color: var(--fg);
    border-color: var(--border-strong);
  }
  .toggle.on {
    color: var(--accent);
    border-color: var(--accent);
    background: color-mix(in srgb, var(--accent) 8%, transparent);
  }
  .toggle.pending { opacity: 0.7; }
  .toggle:disabled { opacity: 0.4; cursor: not-allowed; }
  .toggle:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: -1px;
  }
  .toggle:focus { outline: none; }
</style>
