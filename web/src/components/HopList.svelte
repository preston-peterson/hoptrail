<script>
  import {
    pathStore,
    selectedTTL,
    setSelectedTTL,
    displayHops,
    focusArea,
    routeChangesStore,
    activeTab,
    activeTarget,
    setShowRouteChanges,
    timeWindow,
    chartAnchor,
    TIME_WINDOWS,
    errorStore,
  } from '../lib/stores.js'
  import { clearRouteChanges } from '../lib/api.js'
  import HopCard from './HopCard.svelte'
  import DiagnosticBanner from './DiagnosticBanner.svelte'

  // displayHops swaps server-live hop data for focused-window stats
  // when focusArea is set (step-43). Shape is identical so HopCard
  // doesn't know or care; the small "focused" badge here lets the
  // operator know which mode they're reading.
  $: hopsToRender = $displayHops
  $: focusActive = $focusArea !== null

  function handleSelect(ttl) {
    setSelectedTTL(ttl)
  }

  // ---- inline route changes (step-130) ----
  //
  // Route changes shown WITH their hop instead of leaving the
  // TTL-to-hop join to the operator's head (their request). Scoped to
  // the chart's visible window, so scrolling back through an incident
  // lights the toggle for that era. The toggle persists on the tab
  // row server-side — it follows the operator across browsers.
  const MAX_INLINE = 5

  $: showRC = Boolean($activeTab?.show_route_changes)
  $: viewUntil = $chartAnchor ?? Date.now()
  $: viewSince = viewUntil - (TIME_WINDOWS[$timeWindow]?.ms ?? 300_000)
  $: changesByTTL = groupChanges($routeChangesStore, viewSince, viewUntil)
  $: rcCount = Object.values(changesByTTL).reduce((n, l) => n + l.length, 0)

  function groupChanges(changes, since, until) {
    const map = {}
    for (const c of changes) {
      if (c.ts < since || c.ts > until) continue
      ;(map[c.ttl] ??= []).push(c)
    }
    for (const ttl in map) map[ttl].sort((a, b) => b.ts - a.ts)
    return map
  }

  function fmtRcTime(ms) {
    return new Date(ms).toLocaleTimeString([], { hour12: false })
  }

  // Clear the route-change history for this target (step-133 — the
  // action lived in the retired section; two-click confirm because
  // it's a real delete, all probes, all time, not just this window).
  let clearArmed = false
  async function clearRC() {
    if (!clearArmed) {
      clearArmed = true
      setTimeout(() => (clearArmed = false), 3000)
      return
    }
    clearArmed = false
    try {
      await clearRouteChanges($activeTarget)
      routeChangesStore.set([])
    } catch (err) {
      errorStore.set(err.message ?? String(err))
    }
  }
</script>

<section class="hops">
  <header class="card-header">
    <h2>
      Hops
      {#if focusActive}
        <span class="focus-badge" title="stats are computed from the focused window — click × in the chart's focus area to return to live">
          focused
        </span>
      {/if}
    </h2>
    <div class="header-right">
      {#if $pathStore?.hops}
        <div class="meta">
          <!-- Step-94: a remote probe's path is a stored snapshot, not a
               live engine — say whose it is and how fresh. Absent (and
               the layout byte-identical) for the local probe. -->
          {#if $pathStore.probe_id}
            <span class="probe-meta" title="path as reported by this probe's last snapshot">
              via {$pathStore.probe_id}{#if $pathStore.snapshot_ts}&nbsp;· {new Date($pathStore.snapshot_ts).toLocaleTimeString()}{/if}
            </span>
          {/if}
          {$pathStore.hops.length} hops known
        </div>
      {/if}
      <button
        class="rc-toggle"
        class:on={showRC}
        class:lit={rcCount > 0}
        title={rcCount > 0
          ? `${rcCount} route change${rcCount === 1 ? '' : 's'} in this window — toggle inline display`
          : 'show route changes under their hop when they occur in this window'}
        on:click={() => setShowRouteChanges(!showRC)}
      >
        route changes{#if rcCount > 0}&nbsp;<span class="rc-count">{rcCount}</span>{/if}
      </button>
      {#if showRC && $routeChangesStore.length > 0}
        <button
          class="rc-clear"
          class:armed={clearArmed}
          title="delete this target's route-change history (all probes, all time)"
          on:click={clearRC}
        >{clearArmed ? 'really clear?' : 'clear'}</button>
      {/if}
    </div>
  </header>

  <DiagnosticBanner />

  {#if hopsToRender && hopsToRender.length > 0}
    <!-- Column headers. Grid template matches HopCard exactly so the
         label columns line up over their data columns. The dot column
         has no label (it's a visual cue, not data). -->
    <div class="column-headers">
      <span aria-hidden="true"></span>
      <span class="h-ttl">ttl</span>
      <span class="h-host">host</span>
      <span class="h-rtt">
        <span class="h-rtt-num h-rtt-cur">cur</span>
        <span class="h-rtt-num">avg</span>
        <span class="h-rtt-num">min</span>
      </span>
      <span class="h-loss">loss</span>
      <span class="h-trend">5m</span>
    </div>
  {/if}

  <div class="list">
    {#if hopsToRender && hopsToRender.length > 0}
      {#each hopsToRender as hop (hop.ttl)}
        <HopCard
          {hop}
          selected={hop.ttl === $selectedTTL}
          changed={Boolean(changesByTTL[hop.ttl]?.length)}
          on:select={(e) => handleSelect(e.detail)}
        />
        {#if showRC && changesByTTL[hop.ttl]?.length}
          <div class="rc-rows">
            {#each changesByTTL[hop.ttl].slice(0, MAX_INLINE) as c (c.ts + '-' + (c.new_ip ?? ''))}
              <div class="rc-row">
                <span class="rc-ts">{fmtRcTime(c.ts)}</span>
                <span class="rc-ips">{c.old_ip ?? '∅'} → {c.new_ip}</span>
              </div>
            {/each}
            {#if changesByTTL[hop.ttl].length > MAX_INLINE}
              <div class="rc-row rc-more">
                +{changesByTTL[hop.ttl].length - MAX_INLINE} earlier in this window
              </div>
            {/if}
          </div>
        {/if}
      {/each}
    {:else}
      <div class="placeholder">waiting for first discovery sweep…</div>
    {/if}
  </div>
</section>

<style>
  .header-right {
    display: flex;
    align-items: baseline;
    gap: 10px;
  }
  .rc-toggle {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-subtle);
    font-size: 0.68rem;
    padding: 2px 7px;
    cursor: pointer;
  }
  .rc-toggle:hover { color: var(--fg-muted); }
  .rc-toggle.on { color: var(--fg); border-color: var(--fg-subtle); }
  .rc-toggle.lit { border-color: var(--warning, #f5a524); color: var(--warning, #f5a524); }
  .rc-count { font-weight: 600; }
  .rc-clear {
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-subtle);
    font-size: 0.68rem;
    padding: 2px 7px;
    cursor: pointer;
  }
  .rc-clear:hover, .rc-clear.armed { border-color: var(--danger, #e5484d); color: var(--danger, #e5484d); }

  .rc-rows {
    margin: 0 0 2px 52px;
    padding-left: 8px;
    border-left: 2px solid var(--warning, #f5a524);
  }
  .rc-row {
    display: flex;
    gap: 8px;
    font-family: var(--font-mono);
    font-size: 0.68rem;
    line-height: 1.6;
    color: var(--fg-muted);
  }
  .rc-ts { color: var(--fg-subtle); }
  .rc-more { color: var(--fg-subtle); font-style: italic; }

  .hops {
    display: flex;
    flex-direction: column;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    overflow: hidden;
    min-height: 0;
  }
  .card-header {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--border);
  }
  .card-header h2 {
    margin: 0;
    font-size: 0.95rem;
    font-weight: 600;
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
  }
  .focus-badge {
    color: var(--accent);
    background: color-mix(in srgb, var(--accent) 12%, transparent);
    border: 1px solid color-mix(in srgb, var(--accent) 35%, transparent);
    border-radius: var(--radius-sm);
    padding: 1px 6px;
    font-size: 0.7rem;
    font-weight: 500;
    font-family: var(--font-mono);
    cursor: help;
  }
  .meta { color: var(--fg-muted); font-size: 0.85rem; font-family: var(--font-mono); }
  .probe-meta {
    color: var(--fg-subtle);
    margin-right: 10px;
  }

  /* Column header row. Must match HopCard's grid-template-columns and
     gap exactly so each label sits over its data column. Padding
     matches the .list wrapper's padding plus the HopCard's own padding
     so the visual alignment is pixel-clean. */
  .column-headers {
    display: grid;
    grid-template-columns: 12px 28px 1fr auto auto auto;
    align-items: end;
    gap: var(--space-3);
    /* Outer padding matches .list (var(--space-2)); inner matches
       HopCard's own padding (var(--space-2) var(--space-3)). Combined:
       full alignment with the data rows below. */
    padding: var(--space-3) calc(var(--space-2) + var(--space-3)) var(--space-2);
    border-bottom: 1px solid var(--border);
    font-family: var(--font-mono);
    font-size: 0.7rem;
    color: var(--fg-subtle);
    text-transform: lowercase;
    letter-spacing: 0.02em;
  }

  .h-ttl { text-align: right; }
  .h-host { text-align: left; }
  /* Header layout mirrors HopCard's .rtt — three columns, baseline-aligned,
     with the same gap and min-width per cell so labels sit directly over
     their data columns. */
  .h-rtt {
    display: flex;
    flex-direction: row;
    align-items: baseline;
    justify-content: flex-end;
    gap: var(--space-2);
    line-height: 1.2;
  }
  .h-rtt-num {
    min-width: 4em;
    text-align: right;
  }
  /* Visual hierarchy: cur is the primary stat (highest contrast in the
     data row); avg/min are secondary. Match that contrast in the labels. */
  .h-rtt-cur { color: var(--fg-muted); }
  .h-loss {
    text-align: right;
    min-width: 36px; /* matches .loss in HopCard so alignment is exact */
  }
  /* Sparkline column header. "5m" labels the time-window the sparkline
     covers — same window as the per-hop ring buffer the daemon
     summarizes as avg/cur/min. min-width matches the sparkline's
     intrinsic width so the column doesn't shift when populated. */
  .h-trend {
    min-width: 180px;
    text-align: center;
  }

  .list {
    flex: 1;
    overflow-y: auto;
    padding: var(--space-2);
  }
  .placeholder {
    padding: var(--space-4);
    color: var(--fg-subtle);
    font-style: italic;
    text-align: center;
  }
</style>
