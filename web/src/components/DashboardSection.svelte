<script>
  // One reorderable dashboard section (step-126): a slim chrome bar
  // (grip + label + collapse chevron) above the section's own card.
  // The bar — not the card — is the drag surface, so nothing inside
  // the section (chart pan/zoom, buttons, text selection) competes
  // with dragging. Drop indicators render as inset edges INSIDE the
  // section bounds (an overflow container clips anything negative).
  // All drag STATE lives in App.svelte; this component just reports.
  import { createEventDispatcher } from 'svelte'

  export let id
  export let label
  export let collapsed = false
  // 'before' | 'after' | null — where a drop would land relative to
  // this section while a drag is over it.
  export let dropEdge = null
  export let dragging = false
  // sized: an operator-dragged height is applied (step-164). The
  // content gets overflow:auto so a section shorter than its content
  // scrolls inside its bounds instead of spilling.
  export let sized = false

  const dispatch = createEventDispatcher()
  let rootEl

  function onDragStart(e) {
    // Firefox requires data for the drag to start at all.
    e.dataTransfer.setData('text/plain', id)
    e.dataTransfer.effectAllowed = 'move'
    dispatch('dragstart', { id })
  }

  // Height handle (step-164, the dock splitter's horizontal twin).
  // State lives in App.svelte, same as drag-to-reorder — this just
  // reports the gesture with the measured starting height.
  function onResizeStart(e) {
    e.preventDefault()
    dispatch('resizestart', { id, startY: e.clientY, startHeight: rootEl.offsetHeight })
  }
</script>

<div
  class="dash-section"
  class:collapsed
  class:dragging
  class:sized
  class:drop-before={dropEdge === 'before'}
  class:drop-after={dropEdge === 'after'}
  data-section-id={id}
  bind:this={rootEl}
>
  <div
    class="chrome"
    draggable="true"
    role="button"
    tabindex="-1"
    title="drag to reorder"
    on:dragstart={onDragStart}
    on:dragend={() => dispatch('dragend')}
  >
    <span class="grip" aria-hidden="true">⣿</span>
    <span class="label">{label}</span>
    <button
      class="chevron"
      title={collapsed ? 'expand section' : 'collapse section'}
      on:click={() => dispatch('toggle', { id })}
    >{collapsed ? '▸' : '▾'}</button>
  </div>
  {#if !collapsed}
    <div class="content">
      <slot />
    </div>
    <div
      class="h-handle"
      role="separator"
      aria-orientation="horizontal"
      title="drag to resize height · double-click to reset"
      on:mousedown={onResizeStart}
      on:dblclick={() => dispatch('resizereset', { id })}
    ></div>
  {/if}
</div>

<style>
  .dash-section {
    display: flex;
    flex-direction: column;
    min-height: 0;
    /* min-width:0 matters as much as min-height:0 (step-150): as a
       grid item this wrapper otherwise refuses to shrink below the
       chart canvas's current pixel width, so narrowing the pane
       clipped the section instead of resizing it — and the chart's
       ResizeObserver never fired because its container never shrank. */
    min-width: 0;
    border-radius: var(--radius-sm);
  }
  .dash-section.dragging { opacity: 0.45; }
  /* Drop indicators inside the bounds — inset shadows can't be
     clipped by the scroll container the way negative-offset
     absolutely-positioned bars can. */
  .dash-section.drop-before { box-shadow: inset 0 3px 0 0 var(--accent, #4f8ff7); }
  .dash-section.drop-after  { box-shadow: inset 0 -3px 0 0 var(--accent, #4f8ff7); }

  .chrome {
    display: flex;
    align-items: center;
    gap: 7px;
    padding: 1px 6px;
    cursor: grab;
    user-select: none;
    color: var(--fg-subtle);
  }
  .chrome:active { cursor: grabbing; }
  .chrome:hover { color: var(--fg-muted); }
  .grip { font-size: 0.7rem; letter-spacing: -1px; }
  .label {
    font-size: 0.65rem;
    text-transform: uppercase;
    letter-spacing: 0.06em;
  }
  .chevron {
    margin-left: auto;
    background: transparent;
    border: none;
    color: var(--fg-subtle);
    font-size: 0.7rem;
    cursor: pointer;
    padding: 0 4px;
  }
  .chevron:hover { color: var(--fg); }

  .content {
    flex: 1;
    min-height: 0;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .content > :global(*) { min-width: 0; }
  /* The slotted card fills the content box; cards that size
     themselves (bandwidth) just take their natural height. */
  .content > :global(*) { flex: 1; min-height: 0; }
  /* With an operator-dragged height, content shorter than the section
     scrolls inside it instead of spilling past the grid track. */
  .sized .content { overflow-y: auto; }

  .h-handle {
    flex: 0 0 auto;
    height: 7px;
    margin-top: 1px;
    cursor: ns-resize;
    border-radius: 3px;
  }
  .h-handle:hover, .h-handle:active {
    background: color-mix(in srgb, var(--accent, #4f8ff7) 35%, transparent);
  }
</style>
