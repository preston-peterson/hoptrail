<script>
  // Diagnostic-pattern banner (step-40 / task #9). Renders advisories
  // from lib/diagnostics.js — short, actionable hints encoding the
  // common diagnostic patterns operators reach for when reading a
  // continuous traceroute (uniform loss → local hardware; clean→bad
  // at a network boundary → upstream peering; sawtooth latency on the
  // destination → bandwidth saturation).
  //
  // Lives above the HopList because it summarizes the path as a
  // whole. Silent (no DOM) when no patterns are detected — operators
  // shouldn't have to scan past an "all clear" banner every time.
  //
  // Updates reactively with pathStore; advisories may appear / vanish
  // as the path evolves. No dismissal in v1: silence is preferable
  // to a sticky banner the operator has to keep clicking away.

  import { pathStore, samplesStore } from '../lib/stores.js'
  import { analyzePath } from '../lib/diagnostics.js'

  // Step-48: pass samples so the sawtooth detector can read timeline
  // history, not just the current path snapshot. analyzePath ignores
  // samples for the path-only detectors (uniform-loss, border-crossing).
  $: advisories = analyzePath($pathStore?.hops, $samplesStore)
</script>

{#if advisories.length > 0}
  <div class="banner-stack">
    {#each advisories as a (a.id)}
      <div class="banner sev-{a.severity}">
        <span class="icon" aria-hidden="true">{a.severity === 'warn' ? '⚠' : 'ⓘ'}</span>
        <div class="text">
          <div class="title">{a.title}</div>
          <div class="detail">{a.detail}</div>
        </div>
      </div>
    {/each}
  </div>
{/if}

<style>
  .banner-stack {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-3);
    border-bottom: 1px solid var(--border);
  }
  .banner {
    display: grid;
    grid-template-columns: auto 1fr;
    column-gap: var(--space-3);
    align-items: start;
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm);
    border-left: 3px solid;
    background: var(--bg-sunken);
  }
  .banner.sev-warn {
    border-left-color: rgba(234, 179, 8, 0.7);
    background: color-mix(in srgb, rgba(234, 179, 8, 0.18) 35%, var(--bg-sunken));
  }
  .banner.sev-info {
    border-left-color: var(--accent);
    background: color-mix(in srgb, var(--accent) 8%, var(--bg-sunken));
  }
  .icon {
    font-size: 1rem;
    line-height: 1.4;
    color: var(--fg);
  }
  .text {
    min-width: 0;
  }
  .title {
    font-weight: 600;
    color: var(--fg);
    font-size: 0.85rem;
    line-height: 1.3;
  }
  .detail {
    color: var(--fg-muted);
    font-size: 0.78rem;
    line-height: 1.4;
    margin-top: 2px;
  }
</style>
