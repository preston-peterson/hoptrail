<script>
  import { createEventDispatcher } from 'svelte'
  import { samplesStore, pathStore } from '../lib/stores.js'
  import HopSparkline from './HopSparkline.svelte'

  /** @type {{ ttl: number, current_ip: string | null, hostname: string | null,
                 current_rtt_ms: number, avg_rtt_ms: number, min_rtt_ms: number,
                 loss_percent: number,
                 loss_state: 'ok' | 'suspect' | 'rate_limited',
                 last_response: string | null }} */
  export let hop
  // Step-130: this hop has route changes in the visible window — a
  // small marker on the TTL cell, shown regardless of the inline
  // toggle so the row itself hints where to look.
  export let changed = false

  // The sparkline needs the full samples stream + path snapshot so it
  // can apply downstream attribution when classifying outage bands —
  // a hop showing loss while downstream is healthy gets no band,
  // matching the main chart's behavior (step-38).
  $: pathHops = $pathStore?.hops ?? []

  // selected drives the highlighted state — the row gets a tinted
  // background and a 1.5px ring in the hop's color (matching its
  // line color on the chart), making the link between the row and
  // the chart visually obvious.
  export let selected = false

  const dispatch = createEventDispatcher()

  // Cycle through the 10 hop colors defined in app.css so neighboring
  // TTLs are visually distinct. The same N is used for the chart's
  // series stroke, so the selection ring and the chart line agree.
  $: hopColorVar = `var(--hop-${((hop.ttl - 1) % 10) + 1})`

  // The badge class comes from the server's loss_state classification.
  $: lossClass = (() => {
    if (hop.loss_percent === 0 || hop.loss_state === 'ok') return 'none'
    if (hop.loss_state === 'rate_limited') return 'rate_limited'
    return hop.loss_percent >= 10 ? 'high' : 'some'
  })()

  $: lossTitle = hop.loss_state === 'rate_limited'
    ? 'This hop is declining to respond to ICMP, but downstream hops are healthy — traffic is flowing fine.'
    : ''

  function formatRtt(ms) {
    if (ms == null || ms === 0) return '—'
    if (ms < 1) return ms.toFixed(2) + 'ms'
    if (ms < 100) return ms.toFixed(1) + 'ms'
    return Math.round(ms) + 'ms'
  }

  $: ip = hop.current_ip || '*'
  $: hasHostname = !!hop.hostname

  function handleClick() {
    dispatch('select', hop.ttl)
  }

  function handleKeydown(e) {
    // Treat Enter and Space as activation keys so keyboard users
    // can navigate the list (via tab) and pick a hop without a mouse.
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      dispatch('select', hop.ttl)
    }
  }
</script>

<!-- The card is a button semantically — clicking selects this hop. -->
<!-- svelte-ignore a11y-click-events-have-key-events (handled below) -->
<div
  class="card"
  class:selected
  style="--hop-color: {hopColorVar};"
  role="button"
  tabindex="0"
  aria-pressed={selected}
  on:click={handleClick}
  on:keydown={handleKeydown}
>
  <span class="dot" style="background: {hopColorVar}" aria-hidden="true"></span>
  <span class="ttl">{hop.ttl}{#if changed}<span class="rc-dot" title="route changed in this window"></span>{/if}</span>
  <span class="name-col" class:anon={!hop.current_ip}>
    {#if hasHostname}
      <span class="hostname">{hop.hostname}</span>
      <span class="ip-subtle">{ip}</span>
    {:else}
      <span class="ip">{ip}</span>
    {/if}
  </span>
  <span class="rtt">
    <span class="rtt-num rtt-current">{formatRtt(hop.current_rtt_ms)}</span>
    <span class="rtt-num rtt-secondary">{formatRtt(hop.avg_rtt_ms)}</span>
    <span class="rtt-num rtt-secondary">{formatRtt(hop.min_rtt_ms)}</span>
  </span>
  <span class="loss {lossClass}" title={lossTitle}>
    {hop.loss_percent.toFixed(0)}%
  </span>
  <span class="trend">
    <HopSparkline allSamples={$samplesStore} ttl={hop.ttl} pathHops={pathHops} color={hopColorVar} />
  </span>
</div>

<style>
  .card {
    display: grid;
    grid-template-columns: 12px 28px 1fr auto auto auto;
    align-items: center;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm);
    font-size: 0.9rem;
    cursor: pointer;
    /* The outline reserves no layout space, so applying the box-shadow
       ring on selection doesn't shift other rows. transition keeps the
       state change feeling intentional rather than abrupt. */
    transition: background 80ms ease-out, box-shadow 80ms ease-out;
  }
  /* Hover applies only when not selected — the selected state has its
     own background that we don't want to override on hover. */
  .card:not(.selected):hover { background: var(--bg-sunken); }
  .card:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: -2px;
  }
  .card:focus { outline: none; } /* hide the default; use focus-visible above */

  /* Selected state — matches the step-19 mockup:
     - subtle background tint using the hop's color at low alpha
     - 1.5px ring in the hop's color (box-shadow, so no layout shift)
     - slightly brighter hostname for emphasis
     - slightly larger hop dot with a soft ring around it */
  .card.selected {
    background: color-mix(in srgb, var(--hop-color) 8%, transparent);
    box-shadow: 0 0 0 1.5px var(--hop-color);
  }
  .card.selected .hostname,
  .card.selected .ip { color: var(--fg-bright, var(--fg)); }
  .card.selected .dot {
    width: 11px;
    height: 11px;
    box-shadow: 0 0 0 2px color-mix(in srgb, var(--hop-color) 25%, transparent);
  }

  .dot {
    width: 10px;
    height: 10px;
    border-radius: 50%;
    display: inline-block;
    /* Match the .card transition so dot size animates with the row. */
    transition: width 80ms ease-out, height 80ms ease-out, box-shadow 80ms ease-out;
  }
  .ttl {
    font-family: var(--font-mono);
    color: var(--fg-muted);
    font-size: 0.8rem;
    text-align: right;
  }

  .name-col {
    display: flex;
    flex-direction: column;
    line-height: 1.2;
    overflow: hidden;
  }
  .name-col.anon { color: var(--fg-subtle); }

  .hostname {
    font-family: var(--font-mono);
    color: var(--fg);
    font-size: 0.9rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .ip {
    font-family: var(--font-mono);
    color: var(--fg);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .ip-subtle {
    font-family: var(--font-mono);
    color: var(--fg-subtle);
    font-size: 0.7rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .rtt {
    display: flex;
    flex-direction: row;
    align-items: baseline;
    justify-content: flex-end;
    gap: var(--space-2);
    font-family: var(--font-mono);
    line-height: 1.2;
  }
  /* Each numeric cell gets a min-width so the three columns align
     across rows even when individual values shift between 1-3 digits. */
  .rtt-num {
    min-width: 4em;
    text-align: right;
    font-size: 0.85rem;
  }
  .rtt-current { color: var(--fg); }
  .rtt-secondary { color: var(--fg-subtle); }

  .loss {
    font-family: var(--font-mono);
    font-size: 0.8rem;
    min-width: 36px;
    text-align: right;
    cursor: default;
  }
  .loss.none { color: var(--fg-subtle); }
  .loss.some { color: var(--warn); }
  .loss.high { color: var(--danger); }
  .loss.rate_limited {
    color: var(--fg-subtle);
    text-decoration: underline dotted;
    text-underline-offset: 3px;
  }

  /* The sparkline column. Width comes from the svg's own attributes;
     this wrapper just exists so the grid cell aligns and the sparkline
     can be vertically centered next to the loss percentage. */
  .trend {
    display: flex;
    align-items: center;
  }
  .rc-dot {
    display: inline-block;
    width: 5px;
    height: 5px;
    border-radius: 50%;
    background: var(--warning, #f5a524);
    margin-left: 3px;
    vertical-align: super;
  }
</style>
