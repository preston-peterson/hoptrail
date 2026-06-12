<script>
  import { onMount, onDestroy } from 'svelte'
  import {
    initStores,
    stopPolling,
    sectionLayout,
    saveSectionLayout,
    bandwidthSectionVisible,
    initTheme,
  } from './lib/stores.js'
  import StatusBar from './components/StatusBar.svelte'
  import TabRow from './components/TabRow.svelte'
  import LatencyTimeline from './components/LatencyTimeline.svelte'
  import HopList from './components/HopList.svelte'
  import SettingsPanel from './components/SettingsPanel.svelte'
  import BandwidthChart from './components/BandwidthChart.svelte'
  import BandwidthBanners from './components/BandwidthBanners.svelte'
  import DashboardSection from './components/DashboardSection.svelte'
  import LogOverlay from './components/LogOverlay.svelte'
  import StatusOverlay from './components/StatusOverlay.svelte'
  import DocsOverlay from './components/DocsOverlay.svelte'
  import AlertHistoryOverlay from './components/AlertHistoryOverlay.svelte'

  // Step-126/127: the four sections render in the operator's saved
  // layout — a full-width main stack (page-scrolls; sections take
  // their natural height) plus an optional vertical side dock pinned
  // left or right (the classic route-changes-sidebar look, now any
  // section's option). Bandwidth holds its slot but doesn't render
  // until it has something to show.
  const SECTION_LABELS = {
    latency: 'Latency timeline',
    bandwidth: 'Bandwidth',
    hops: 'Hops',
  }
  // Main-stack row sizing: the latency chart needs a definite height
  // (uPlot sizes from its container); everything else takes natural
  // height and the stack scrolls (operator: "make the sections full,
  // give scroll bars to go down the page").
  const SECTION_ROWS = {
    latency: 'minmax(280px, 38vh)',
    bandwidth: 'auto',
    hops: 'auto',
  }

  $: layout = $sectionLayout
  $: visibleMain = layout.order.filter(
    (id) => id !== 'bandwidth' || $bandwidthSectionVisible
  )
  $: visibleSide = (layout.side ?? []).filter(
    (id) => id !== 'bandwidth' || $bandwidthSectionVisible
  )
  $: gridRows = visibleMain
    .map((id) => (layout.collapsed[id] ? 'auto' : SECTION_ROWS[id]))
    .join(' ')
  $: dockRows = visibleSide
    .map((id) => (layout.collapsed[id] ? 'auto' : 'minmax(150px, 1fr)'))
    .join(' ')

  // The #app grid (status row + columns) lives in app.css, keyed by a
  // data attribute on the mount node.
  $: {
    const el = document.getElementById('app')
    if (el) {
      if (visibleSide.length > 0) el.dataset.dock = layout.side_position ?? 'right'
      else delete el.dataset.dock
      el.style.setProperty('--dock-w', (layout.side_width || 340) + 'px')
    }
  }

  // ---- dock splitter (step-150, operator request: drag the boundary
  // between dock and main to split the screen your way) ----
  let splitting = false
  function startSplit(e) {
    e.preventDefault()
    splitting = true
    const el = document.getElementById('app')
    const onMove = (ev) => {
      const w = (layout.side_position ?? 'right') === 'right'
        ? window.innerWidth - ev.clientX
        : ev.clientX
      const clamped = Math.max(240, Math.min(680, Math.round(w)))
      el?.style.setProperty('--dock-w', clamped + 'px')
      pendingWidth = clamped
    }
    const onUp = () => {
      splitting = false
      window.removeEventListener('mousemove', onMove)
      window.removeEventListener('mouseup', onUp)
      if (pendingWidth && pendingWidth !== layout.side_width) {
        saveSectionLayout({ ...layout, side_width: pendingWidth })
      }
      pendingWidth = null
    }
    window.addEventListener('mousemove', onMove)
    window.addEventListener('mouseup', onUp)
  }
  let pendingWidth = null

  // ---- drag-to-reorder (memory lesson: wire dragover/drop at the
  // CONTAINER, not per-element; indicators inset within bounds) ----
  let dragId = null
  // { list: 'order'|'side', id, edge } — hovering a section; or
  // { strip: 'left'|'right' } — hovering an edge dock strip.
  let dropTarget = null

  function sectionAt(e) {
    const el = e.target.closest?.('[data-section-id]')
    if (!el) return null
    const id = el.getAttribute('data-section-id')
    const rect = el.getBoundingClientRect()
    return { id, edge: e.clientY < rect.top + rect.height / 2 ? 'before' : 'after' }
  }

  function onDragOverStack(list) {
    return (e) => {
      if (!dragId) return
      e.preventDefault()
      e.dataTransfer.dropEffect = 'move'
      const t = sectionAt(e)
      if (t && t.id !== dragId) dropTarget = { list, ...t }
      else if (!t && list === 'side') dropTarget = { list, id: null, edge: 'after' }
      else dropTarget = null
    }
  }

  // Removes dragId from both lists, returning fresh copies.
  function without(id) {
    return {
      order: layout.order.filter((x) => x !== id),
      side: (layout.side ?? []).filter((x) => x !== id),
    }
  }

  function onDropStack(list) {
    return (e) => {
      if (!dragId) return
      e.preventDefault()
      if (dropTarget && dropTarget.list === list) {
        const { order, side } = without(dragId)
        const target = list === 'order' ? order : side
        let idx = dropTarget.id ? target.indexOf(dropTarget.id) : target.length
        if (dropTarget.id && dropTarget.edge === 'after') idx += 1
        target.splice(idx, 0, dragId)
        saveSectionLayout({ ...layout, order, side })
      }
      dragId = null
      dropTarget = null
    }
  }

  // Edge strips: while dragging, the window edges accept a drop to
  // dock the section there (creating the dock, or moving it to the
  // other side). The strip for the dock's current side is redundant
  // with the dock itself, so only the empty edge(s) show.
  $: stripEdges = dragId
    ? ['left', 'right'].filter(
        (edge) => visibleSide.length === 0 || (layout.side_position ?? 'right') !== edge
      )
    : []

  function onDropStrip(edge) {
    return (e) => {
      if (!dragId) return
      e.preventDefault()
      const { order, side } = without(dragId)
      side.push(dragId)
      saveSectionLayout({ ...layout, order, side, side_position: edge })
      dragId = null
      dropTarget = null
    }
  }

  function toggleCollapsed(id) {
    const collapsed = { ...layout.collapsed }
    if (collapsed[id]) delete collapsed[id]
    else collapsed[id] = true
    saveSectionLayout({ ...layout, collapsed })
  }

  function endDrag() {
    dragId = null
    dropTarget = null
  }

  onMount(() => {
    initTheme() // step-141: theme control lives under the gear now
    initStores()
  })

  onDestroy(() => {
    stopPolling()
  })
</script>

<div class="status-area">
  <!-- Step-102: derate/install banners render above everything —
       impossible to miss is the design requirement. -->
  <BandwidthBanners />
  <StatusBar />
  <TabRow />
</div>

<!-- svelte-ignore a11y-no-static-element-interactions -->
<div
  class="main-area"
  style="grid-template-rows: {gridRows}"
  on:dragover={onDragOverStack('order')}
  on:drop={onDropStack('order')}
  on:dragleave={(e) => { if (e.target === e.currentTarget) dropTarget = null }}
>
  {#each visibleMain as id (id)}
    <DashboardSection
      {id}
      label={SECTION_LABELS[id]}
      collapsed={Boolean(layout.collapsed[id])}
      dragging={dragId === id}
      dropEdge={dropTarget?.list === 'order' && dropTarget?.id === id ? dropTarget.edge : null}
      on:dragstart={(e) => (dragId = e.detail.id)}
      on:dragend={endDrag}
      on:toggle={(e) => toggleCollapsed(e.detail.id)}
    >
      {#if id === 'latency'}<LatencyTimeline />
      {:else if id === 'bandwidth'}<BandwidthChart />
      {:else if id === 'hops'}<HopList />{/if}
    </DashboardSection>
  {/each}
</div>

{#if visibleSide.length > 0}
  <!-- svelte-ignore a11y-no-static-element-interactions -->
  <div
    class="dock-area"
    class:dock-left={(layout.side_position ?? 'right') === 'left'}
    class:dock-hover={dropTarget?.list === 'side' && dropTarget?.id === null}
    style="grid-template-rows: {dockRows}"
    on:dragover={onDragOverStack('side')}
    on:drop={onDropStack('side')}
    on:dragleave={(e) => { if (e.target === e.currentTarget) dropTarget = null }}
  >
    <!-- svelte-ignore a11y-no-static-element-interactions -->
    <div class="splitter" class:active={splitting} on:mousedown={startSplit} title="drag to resize"></div>
    {#each visibleSide as id (id)}
      <DashboardSection
        {id}
        label={SECTION_LABELS[id]}
        collapsed={Boolean(layout.collapsed[id])}
        dragging={dragId === id}
        dropEdge={dropTarget?.list === 'side' && dropTarget?.id === id ? dropTarget.edge : null}
        on:dragstart={(e) => (dragId = e.detail.id)}
        on:dragend={endDrag}
        on:toggle={(e) => toggleCollapsed(e.detail.id)}
      >
        {#if id === 'latency'}<LatencyTimeline />
        {:else if id === 'bandwidth'}<BandwidthChart />
        {:else if id === 'hops'}<HopList />{/if}
      </DashboardSection>
    {/each}
  </div>
{/if}

<!-- Edge dock strips: visible only mid-drag; dropping docks the
     section to that side of the screen. -->
{#each stripEdges as edge (edge)}
  <!-- svelte-ignore a11y-no-static-element-interactions -->
  <div
    class="dock-strip {edge}"
    on:dragover={(e) => { e.preventDefault(); e.dataTransfer.dropEffect = 'move' }}
    on:drop={onDropStrip(edge)}
  >
    <span>dock {edge}</span>
  </div>
{/each}

<!-- Step-101: settings slide-out; fixed-position overlay, renders
     nothing while closed. -->
<SettingsPanel />

<!-- Step-129: full-screen daemon-log overlay, opened from Settings ->
     System. Renders nothing while closed. -->
<LogOverlay />

<!-- Step-140: environment status overlay, opened from the StatusBar
     health dot. Renders nothing while closed. -->
<StatusOverlay />

<!-- Step-143: in-app documentation overlay (gear → Documentation). -->
<DocsOverlay />

<!-- Step-149: alert history overlay (StatusBar bell). -->
<AlertHistoryOverlay />

<style>
  .status-area  { grid-area: status; }
  .main-area {
    grid-area: main;
    /* Grid items default to min-width:auto — the content's natural
       width would stop the 1fr column shrinking and shove it off
       screen when the dock grows (step-150 operator shot). */
    min-width: 0;
    /* Step-127: the main stack page-scrolls; sections take natural
       (or vh-bounded) heights instead of being squeezed into the
       viewport. grid-template-rows set inline from the layout. */
    overflow-y: auto;
    overflow-x: hidden;
    display: grid;
    align-content: start;
    gap: var(--space-2);
    padding: var(--space-3);
  }
  .dock-area {
    grid-area: dock;
    min-width: 0;
    overflow: hidden;
    display: grid;
    /* rows set inline */
    gap: var(--space-2);
    padding: var(--space-3) var(--space-2);
    border-left: 1px solid var(--border);
  }
  :global(#app[data-dock='left']) .dock-area {
    border-left: none;
    border-right: 1px solid var(--border);
  }
  .dock-area.dock-hover { box-shadow: inset 0 0 0 2px var(--accent, #4f8ff7); }
  .dock-area { position: relative; }
  .splitter {
    position: absolute;
    top: 0;
    bottom: 0;
    left: -3px;
    width: 7px;
    cursor: col-resize;
    z-index: 5;
  }
  .dock-area.dock-left .splitter { left: auto; right: -3px; }
  .splitter:hover, .splitter.active {
    background: color-mix(in srgb, var(--accent, #4f8ff7) 35%, transparent);
  }

  .dock-strip {
    position: fixed;
    top: 0;
    bottom: 0;
    width: 34px;
    z-index: 50;
    display: flex;
    align-items: center;
    justify-content: center;
    background: color-mix(in srgb, var(--accent, #4f8ff7) 18%, transparent);
    border: 1px dashed var(--accent, #4f8ff7);
  }
  .dock-strip.left  { left: 0; }
  .dock-strip.right { right: 0; }
  .dock-strip span {
    writing-mode: vertical-rl;
    font-size: 0.65rem;
    text-transform: uppercase;
    letter-spacing: 0.08em;
    color: var(--accent, #4f8ff7);
    pointer-events: none;
  }
</style>
