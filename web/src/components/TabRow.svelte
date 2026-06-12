<script>
  // Tab row (step-26 → step-70 multi-tab refactor) — one pill per
  // operator-visible tab. A tab is a (target, label?, thresholds?)
  // triple; multiple tabs may share a target. Clicking a pill switches
  // activeTabId, which drives every downstream poll via the derived
  // activeTarget store. The "+" pill opens an inline target editor
  // that POSTs /api/targets THEN /api/tabs (so every monitored target
  // always has at least one default tab — the design's invariant).
  // Hover affordances: × close, ⧉ duplicate.
  //
  // Step-70 internal model:
  //   $tabsStore   — array of tab objects ordered by server position
  //   $activeTabId — number | null, persisted to localStorage
  //   $activeTab   — derived: the active tab object
  //   $activeTarget — derived: the active tab's target string
  //   tab.label    — null = render tab.target; otherwise operator label
  //   tab.tab_id   — integer; primary key for everything tab-related

  import { tick } from 'svelte'
  import {
    tabsStore,
    activeTabId,
    activeTab,
    tabHostnames,
    targetHistory,
    refreshTargetHistory,
    setActiveTab,
    createTabAndActivate,
    deleteTabById,
    reorderTabs,
    setTabLabel,
  } from '../lib/stores.js'
  import { addTarget , fetchTargetStats, deleteTargetData } from '../lib/api.js'
  import BundlesMenu from './BundlesMenu.svelte'

  let adding = false
  let addInput
  let addDraft = ''
  let addError = null
  let addPending = false

  function selectTab(tabId) {
    setActiveTab(tabId)
  }

  async function beginAdd() {
    addDraft = ''
    addError = null
    adding = true
    refreshTargetHistory()
    await tick()
    addInput?.focus()
  }

  function cancelAdd() {
    adding = false
    addError = null
    addPending = false
  }

  // Resume-vs-new (step-111): when the daemon has prior history for
  // a typed target, ask before adding — resume keeps it (the chart
  // shows an honest gap for the unmonitored span) or start new wipes
  // samples + route changes (annotations survive). Operator chose
  // clean deletion over generation-keying.
  let resumePrompt = null // { target, samples, oldestTs }

  async function submitAdd(value) {
    const t = (value ?? addDraft).trim()
    if (!t) { addError = 'enter an IP or hostname'; return }
    addPending = true
    addError = null
    try {
      const existing = $tabsStore.find((tab) => tab.target === t)
      if (existing) {
        setActiveTab(existing.tab_id)
        adding = false
        return
      }
      // Seen before? Offer resume-vs-new instead of silently resuming.
      try {
        const stats = await fetchTargetStats(t)
        if (stats.samples > 0) {
          resumePrompt = { target: t, samples: stats.samples, oldestTs: stats.oldest_ts }
          return
        }
      } catch { /* stats are advisory — fall through to a plain add */ }
      await finishAdd(t)
    } catch (err) {
      addError = err.message ?? String(err)
    } finally {
      addPending = false
    }
  }

  async function finishAdd(t) {
    await addTarget(t)
    await createTabAndActivate({ target: t })
    refreshTargetHistory()
    adding = false
    resumePrompt = null
  }

  async function chooseResume() {
    addPending = true
    try { await finishAdd(resumePrompt.target) }
    catch (err) { addError = err.message ?? String(err) }
    finally { addPending = false }
  }

  async function chooseNew() {
    addPending = true
    try {
      await deleteTargetData(resumePrompt.target)
      await finishAdd(resumePrompt.target)
    } catch (err) { addError = err.message ?? String(err) }
    finally { addPending = false }
  }

  function historySpan(oldestTs) {
    const days = Math.max(1, Math.round((Date.now() - oldestTs) / 86_400_000))
    return days === 1 ? '1 day' : `${days} days`
  }

  // Filter history for the dropdown: drop any target that already
  // has a live tab (so the operator doesn't re-add and double up),
  // then substring-match against the current input.
  $: activeTargets = new Set($tabsStore.map((tab) => tab.target))
  $: dropdownItems = $targetHistory
    .filter((t) => !activeTargets.has(t))
    .filter((t) => !addDraft || t.toLowerCase().includes(addDraft.toLowerCase().trim()))

  function pickFromHistory(t) {
    addDraft = t
    submitAdd(t)
  }

  function onAddKeydown(e) {
    if (e.key === 'Enter') { e.preventDefault(); submitAdd() }
    else if (e.key === 'Escape') { e.preventDefault(); cancelAdd() }
  }

  // Step-61: drag-to-reorder. Container-level dragover walks all
  // tab rects to compute the drop position from cursor X — handles
  // the edge cases (drag past leftmost, drag past rightmost, etc).
  // Indicators sit INSIDE the tab edge (left:0 / right:0) so the
  // overflow-x:auto on .tabs-scroll doesn't clip them.
  //
  // Step-70: keyed by tab_id, not target. reorderTabs takes the full
  // ordered tab_id list and PATCHes /api/tabs/order in one call.
  let dragSource = null // tab_id | null
  let dropIndicator = null // null | { tabId, side: 'left' | 'right' }
  let tabsScrollEl

  function onTabDragStart(tabId, e) {
    dragSource = tabId
    e.dataTransfer.effectAllowed = 'move'
    e.dataTransfer.setData('text/plain', String(tabId))
  }

  function computeDropPosition(clientX) {
    if (!tabsScrollEl) return null
    const tabEls = tabsScrollEl.querySelectorAll('.tab')
    if (tabEls.length === 0) return null
    const list = $tabsStore
    for (let i = 0; i < tabEls.length; i++) {
      const rect = tabEls[i].getBoundingClientRect()
      const center = rect.left + rect.width / 2
      if (clientX < center) {
        const tab = list[i]
        if (!tab) return null
        // Suppress no-op drop indicators (drop-on-self, drop-after-self).
        if (tab.tab_id === dragSource) {
          const prev = list[i - 1]
          if (prev) return { tabId: prev.tab_id, side: 'right' }
          return null
        }
        if (list[i - 1]?.tab_id === dragSource) return null
        return { tabId: tab.tab_id, side: 'left' }
      }
    }
    const last = list[list.length - 1]
    if (!last || last.tab_id === dragSource) return null
    return { tabId: last.tab_id, side: 'right' }
  }

  function onTabsDragOver(e) {
    if (dragSource == null) return
    e.preventDefault()
    e.dataTransfer.dropEffect = 'move'
    const next = computeDropPosition(e.clientX)
    if (!next) {
      if (dropIndicator !== null) dropIndicator = null
      return
    }
    if (
      !dropIndicator ||
      dropIndicator.tabId !== next.tabId ||
      dropIndicator.side !== next.side
    ) {
      dropIndicator = next
    }
  }

  function onTabsDragLeave(e) {
    if (!e.relatedTarget || !tabsScrollEl.contains(e.relatedTarget)) {
      dropIndicator = null
    }
  }

  function onTabsDrop(e) {
    if (dragSource == null) return
    e.preventDefault()
    const pos = computeDropPosition(e.clientX)
    if (pos) {
      // Build the new full ordered list of tab_ids.
      const list = $tabsStore
      const remaining = list.filter((tab) => tab.tab_id !== dragSource)
      let insertAt
      if (pos.side === 'left') {
        const targetIdx = remaining.findIndex((tab) => tab.tab_id === pos.tabId)
        insertAt = targetIdx < 0 ? remaining.length : targetIdx
      } else {
        const targetIdx = remaining.findIndex((tab) => tab.tab_id === pos.tabId)
        insertAt = targetIdx < 0 ? remaining.length : targetIdx + 1
      }
      const orderedIds = remaining.map((tab) => tab.tab_id)
      orderedIds.splice(insertAt, 0, dragSource)
      reorderTabs(orderedIds)
    }
    dropIndicator = null
    dragSource = null
  }

  function onTabDragEnd() {
    dragSource = null
    dropIndicator = null
  }

  async function closeTab(tabId, e) {
    e.stopPropagation()
    try {
      await deleteTabById(tabId)
    } catch (err) {
      console.error('deleteTab failed', err)
    }
  }

  // Step-70: duplicate affordance. Calls /api/tabs with copy_from so
  // the new tab inherits label + thresholds from the source. Lands
  // right after the source in position (server appends; the next
  // tabs-poll reconciles to canonical ordering).
  async function duplicateTab(tab, e) {
    e.stopPropagation()
    try {
      await createTabAndActivate({ target: tab.target, copyFrom: tab.tab_id })
    } catch (err) {
      console.error('duplicateTab failed', err)
    }
  }

  // Resolve the operator-facing pill label. Operator label wins;
  // otherwise the raw target string. Hostname-discovered names live in
  // the title tooltip rather than the pill text so operator-set labels
  // can't be silently overwritten by an rDNS round.
  function pillLabel(tab) {
    if (tab.label) return tab.label
    return tab.target
  }

  // Step-72: per-tab label rename. Double-clicking a pill enters
  // inline-edit mode for that tab's label. Enter saves via
  // setTabLabel; Esc cancels. Empty input clears the label (so the
  // pill falls back to displaying the target). Only one tab is in
  // edit mode at a time.
  let renamingTabId = null
  let renameDraft = ''
  let renameInputEl
  async function beginRename(tab) {
    renamingTabId = tab.tab_id
    renameDraft = tab.label ?? ''
    await tick()
    renameInputEl?.focus()
    renameInputEl?.select()
  }
  async function commitRename() {
    if (renamingTabId == null) return
    const id = renamingTabId
    const value = renameDraft.trim()
    renamingTabId = null
    renameDraft = ''
    await setTabLabel(id, value)
  }

  // Step-76: close-all-tabs affordance next to the bundles dropdown.
  // Two-step arm: first click arms (button shows "really? ×N" in
  // danger color); second click within DISARM_MS wipes every tab.
  // No modal — homelab single-operator UI shouldn't gate on a dialog
  // the operator confirms reflexively. Tabs delete in sequence; the
  // server cascades target removal when each target's last tab dies.
  let closeAllArmed = false
  let closeAllArmTimer = null
  const CLOSE_ALL_DISARM_MS = 3000
  function armCloseAll() {
    if (closeAllArmed) return
    closeAllArmed = true
    closeAllArmTimer = setTimeout(() => {
      closeAllArmed = false
    }, CLOSE_ALL_DISARM_MS)
  }
  function disarmCloseAll() {
    closeAllArmed = false
    if (closeAllArmTimer) {
      clearTimeout(closeAllArmTimer)
      closeAllArmTimer = null
    }
  }
  async function confirmCloseAll() {
    disarmCloseAll()
    // Snapshot tab_ids before the loop so the optimistic deletes
    // don't disturb the iteration.
    const tabIds = $tabsStore.map((tab) => tab.tab_id)
    for (const id of tabIds) {
      try {
        await deleteTabById(id)
      } catch (err) {
        console.error('closeAllTabs: deleteTabById failed', id, err)
      }
    }
  }
  function onCloseAllClick() {
    if (closeAllArmed) {
      confirmCloseAll()
    } else {
      armCloseAll()
    }
  }
  function cancelRename() {
    renamingTabId = null
    renameDraft = ''
  }
  function onRenameKeydown(e) {
    if (e.key === 'Enter') { e.preventDefault(); commitRename() }
    else if (e.key === 'Escape') { e.preventDefault(); cancelRename() }
  }
</script>

<div class="tab-row" role="tablist" aria-label="monitored targets">
  <!-- Tabs are wrapped in their own horizontally-scrollable container
       so the add-form's dropdown (positioned absolutely beneath it)
       can escape vertically without being clipped by the tab row's
       overflow. -->
  <!-- Inner scroll wrapper. Visual container only — the parent
       carries role="tablist" and the children carry role="tab", so
       this gets role="presentation" to stay transparent to ARIA
       traversal while still satisfying the a11y linter's rule that
       drag-handler-bearing elements declare a role. -->
  <div
    class="tabs-scroll"
    role="presentation"
    bind:this={tabsScrollEl}
    on:dragover={onTabsDragOver}
    on:dragleave={onTabsDragLeave}
    on:drop={onTabsDrop}
  >
  {#each $tabsStore as tab (tab.tab_id)}
    {@const hostname = $tabHostnames[tab.target]}
    {@const isActive = tab.tab_id === $activeTabId}
    {@const label = pillLabel(tab)}
    <div
      class="tab"
      class:active={isActive}
      class:dragging={dragSource === tab.tab_id}
      class:drop-before={dropIndicator?.tabId === tab.tab_id && dropIndicator.side === 'left'}
      class:drop-after={dropIndicator?.tabId === tab.tab_id && dropIndicator.side === 'right'}
      role="tab"
      tabindex="0"
      aria-selected={isActive}
      title={hostname && hostname !== tab.target ? `${tab.target} → ${hostname}` : tab.target}
      draggable="true"
      on:dragstart={(e) => onTabDragStart(tab.tab_id, e)}
      on:dragend={onTabDragEnd}
      on:click={() => selectTab(tab.tab_id)}
      on:dblclick={() => beginRename(tab)}
      on:keydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); selectTab(tab.tab_id) } }}
    >
      {#if renamingTabId === tab.tab_id}
        <input
          class="tab-label-input"
          bind:this={renameInputEl}
          bind:value={renameDraft}
          on:keydown={onRenameKeydown}
          on:blur={commitRename}
          on:click|stopPropagation
          on:mousedown|stopPropagation
          on:dblclick|stopPropagation
          placeholder={tab.target}
          maxlength="64"
          spellcheck="false"
          aria-label="rename tab"
        />
      {:else}
        <span class="tab-label">{label}</span>
      {/if}
      <button
        class="dup"
        draggable="false"
        on:click={(e) => duplicateTab(tab, e)}
        on:mousedown={(e) => e.stopPropagation()}
        title="duplicate this tab"
        aria-label="duplicate tab {label}"
      >⧉</button>
      <button
        class="close"
        draggable="false"
        on:click={(e) => closeTab(tab.tab_id, e)}
        on:mousedown={(e) => e.stopPropagation()}
        title="close this tab"
        aria-label="close tab {label}"
      >×</button>
    </div>
  {/each}
  </div>

  {#if adding}
    <div class="tab-add-wrap">
      <div class="tab-add-form">
        <input
          bind:this={addInput}
          bind:value={addDraft}
          on:keydown={onAddKeydown}
          placeholder="1.2.3.4 or dns.google"
          disabled={addPending}
          spellcheck="false"
          autocomplete="off"
          aria-label="new target IP or hostname"
        />
        <button class="add-apply" on:click={() => submitAdd()} disabled={addPending} title="apply (Enter)">
          {addPending ? '…' : 'add'}
        </button>
        <button class="add-cancel" on:click={cancelAdd} disabled={addPending} title="cancel (Esc)">×</button>
        {#if addError}
          <span class="add-error" title={addError}>{addError}</span>
        {/if}
      </div>
      {#if resumePrompt}
        <!-- Step-111: this target has prior history — resume or wipe. -->
        <div class="resume-prompt" role="alertdialog" aria-label="resume or start new">
          <span class="rp-text">
            <strong>{resumePrompt.target}</strong> has {historySpan(resumePrompt.oldestTs)} of history
            ({resumePrompt.samples.toLocaleString()} samples).
          </span>
          <button class="rp-resume" on:click={chooseResume} disabled={addPending}
                  title="keep the history — the chart shows a gap for the unmonitored span">
            resume
          </button>
          <button class="rp-new" on:click={chooseNew} disabled={addPending}
                  title="permanently delete this target's samples and route changes (annotations are kept)">
            start new — delete history
          </button>
          <button class="add-cancel" on:click={() => { resumePrompt = null }} disabled={addPending} title="cancel">×</button>
        </div>
      {/if}
      <!-- Recent-target dropdown. Always rendered while adding so
           the affordance is discoverable from the first open; shows
           an empty-state hint until the user has built up history. -->
      <div class="add-dropdown" role="listbox" aria-label="recent targets">
        <div class="add-dropdown-header">recent</div>
        {#if dropdownItems.length > 0}
          {#each dropdownItems as item (item)}
            <button
              class="add-dropdown-item"
              role="option"
              aria-selected="false"
              on:click={() => pickFromHistory(item)}
              disabled={addPending}
            >
              {item}
            </button>
          {/each}
        {:else if $targetHistory.length === 0}
          <div class="add-dropdown-empty">targets you add will appear here</div>
        {:else if $targetHistory.every((t) => activeTargets.has(t))}
          <div class="add-dropdown-empty">all recent targets are already active</div>
        {:else}
          <div class="add-dropdown-empty">no matches</div>
        {/if}
      </div>
    </div>
  {:else}
    <button class="tab-add" on:click={beginAdd} title="add a new target">+</button>
  {/if}

  <BundlesMenu />

  {#if $tabsStore.length > 0}
    <!-- Step-76: close-all-tabs. Sits next to BundlesMenu since they're
         conceptually a pair (one loads a tab set, the other clears it).
         Two-step arm pattern: click once to arm, again to commit. -->
    <button
      class="close-all"
      class:armed={closeAllArmed}
      on:click={onCloseAllClick}
      on:blur={disarmCloseAll}
      title={closeAllArmed ? `click again within 3s to close all ${$tabsStore.length} tabs` : 'close all tabs'}
    >
      {#if closeAllArmed}
        really? ×{$tabsStore.length}
      {:else}
        close all
      {/if}
    </button>
  {/if}

  {#if $tabsStore.length === 0 && !adding}
    <span class="empty">no tabs — click + to add a target</span>
  {/if}
</div>

<style>
  .tab-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    background: var(--bg-sunken);
    border-bottom: 1px solid var(--border);
    /* No overflow here — the dropdown beneath the add-form needs to
       escape vertically. Horizontal overflow handling lives on the
       .tabs-scroll child wrapper around the tab pills only. */
  }
  .tabs-scroll {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: nowrap;
    overflow-x: auto;
    /* min-width: 0 lets this flex item actually shrink when the row
       is narrower than the sum of tab widths, instead of refusing to
       shrink and forcing the parent to overflow. */
    min-width: 0;
    flex: 1 1 auto;
  }

  .tab {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    padding: 4px 8px 4px 10px;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    font-family: var(--font-mono);
    font-size: 0.85rem;
    cursor: pointer;
    white-space: nowrap;
    transition: background 80ms ease-out, border-color 80ms ease-out, color 80ms ease-out;
  }
  .tab:hover { color: var(--fg); border-color: var(--border-strong); }
  .tab.active {
    background: var(--accent-soft, var(--bg-elevated));
    border-color: var(--accent);
    color: var(--fg);
  }
  .tab:focus-visible { outline: 2px solid var(--accent); outline-offset: -2px; }
  .tab:focus { outline: none; }

  /* Step-60 drag-to-reorder. The dragged tab dims while in-flight; the
     drop target shows a thin accent bar on the side the drop will land.
     Position is `relative` on .tab so the absolute-positioned indicators
     anchor to the tab's box.

     Step-61: indicators live INSIDE the tab edge (left:0 / right:0)
     rather than just outside (left:-5px / right:-5px). The outside
     placement got clipped by .tabs-scroll's overflow-x:auto on the
     leftmost / rightmost tab, so dropping into either edge gave no
     feedback. Inside placement is always visible. */
  .tab { position: relative; }
  .tab.dragging { opacity: 0.4; }
  .tab.drop-before::before,
  .tab.drop-after::after {
    content: '';
    position: absolute;
    top: 2px;
    bottom: 2px;
    width: 3px;
    background: var(--accent);
    border-radius: 2px;
    pointer-events: none;
    box-shadow: 0 0 6px var(--accent);
  }
  .tab.drop-before::before { left: 0; }
  .tab.drop-after::after { right: 0; }

  .tab-label { font-weight: 500; }

  /* Step-72: inline rename input. Visually replaces .tab-label while
     in rename mode; sized to feel continuous with the surrounding
     pill so the tab doesn't jump on entry. The input keeps the pill's
     existing height by inheriting font + padding. */
  .tab-label-input {
    background: var(--bg-sunken);
    border: 1px solid var(--accent);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-family: var(--font-mono);
    font-size: 0.85rem;
    font-weight: 500;
    padding: 0 4px;
    margin: -1px 0;
    outline: none;
    min-width: 8ch;
    max-width: 24ch;
  }

  /* × close button — visible by default at low opacity so users see
     the affordance, brightens on hover of either the tab or the
     button itself. */
  .close {
    background: transparent;
    border: none;
    color: var(--fg-subtle);
    font-size: 0.95rem;
    line-height: 1;
    padding: 0 4px;
    cursor: pointer;
    opacity: 0.6;
    transition: opacity 80ms ease-out, color 80ms ease-out;
  }
  .tab:hover .close { opacity: 1; }
  .close:hover { color: var(--danger); }

  /* Step-70: ⧉ duplicate button — same shape as .close but accents to
     the brand color on hover. Hidden until tab hover (so single-tab
     pills don't get visually busy) but always present so keyboard
     navigation reaches it. */
  .dup {
    background: transparent;
    border: none;
    color: var(--fg-subtle);
    font-size: 0.9rem;
    line-height: 1;
    padding: 0 4px;
    cursor: pointer;
    opacity: 0;
    transition: opacity 80ms ease-out, color 80ms ease-out;
  }
  .tab:hover .dup,
  .dup:focus-visible { opacity: 0.85; }
  .dup:hover { color: var(--accent); opacity: 1; }

  /* + pill — distinct from tab pills so the add affordance is obvious. */
  .tab-add {
    background: transparent;
    border: 1px dashed var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    padding: 4px 10px;
    font-size: 0.95rem;
    line-height: 1;
    cursor: pointer;
    transition: color 80ms ease-out, border-color 80ms ease-out, background 80ms ease-out;
  }
  .tab-add:hover {
    color: var(--fg);
    border-color: var(--border-strong);
    background: var(--bg-elevated);
  }

  /* Step-76: close-all pill. Mirrors BundlesMenu's trigger weight so
     they read as a matched cluster on the right edge of the tab row.
     Armed state flips to danger-tinted background + accent border so
     the operator sees their commit window unambiguously. */
  .close-all {
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    font-family: var(--font-mono);
    font-size: 0.75rem;
    padding: 3px 8px;
    cursor: pointer;
    white-space: nowrap;
    transition: color 80ms ease-out, border-color 80ms ease-out, background 80ms ease-out;
  }
  .close-all:hover:not(.armed) {
    color: var(--fg);
    border-color: var(--border-strong);
  }
  .close-all.armed {
    color: var(--bg);
    background: var(--danger);
    border-color: var(--danger);
    font-weight: 600;
  }

  /* Wrapper provides the positioning context for the dropdown,
     which is absolutely positioned beneath the form. */
  .tab-add-wrap {
    position: relative;
    display: inline-flex;
  }

  /* Inline add form replaces the + pill while adding. Same visual
     weight so the row's height doesn't shift on transition. */
  .tab-add-form {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    padding: 2px 4px;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
  }

  /* Recent-target dropdown — sits beneath the add form. Each item
     is a button (so keyboard tab-through works) labeled with the
     recent target string. Click submits with that value. */
  .add-dropdown {
    position: absolute;
    top: calc(100% + 4px);
    left: 0;
    min-width: 100%;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    box-shadow: 0 4px 16px rgba(0, 0, 0, 0.3);
    padding: 4px 0;
    z-index: 20;
    max-height: 240px;
    overflow-y: auto;
  }
  .add-dropdown-header {
    color: var(--fg-subtle);
    font-family: var(--font-mono);
    font-size: 0.65rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    padding: 2px 10px 4px;
  }
  .add-dropdown-item {
    display: block;
    width: 100%;
    background: transparent;
    border: none;
    text-align: left;
    color: var(--fg);
    font-family: var(--font-mono);
    font-size: 0.85rem;
    padding: 4px 10px;
    cursor: pointer;
  }
  .add-dropdown-item:hover:not(:disabled) {
    background: var(--bg-sunken);
  }
  .add-dropdown-item:disabled {
    opacity: 0.5;
    cursor: wait;
  }
  /* Empty-state hint: dropdown is rendered even with no items so
     the operator sees the affordance. The hint explains the
     mechanic and disappears as soon as items exist. */
  .add-dropdown-empty {
    color: var(--fg-subtle);
    font-family: var(--font-mono);
    font-size: 0.75rem;
    font-style: italic;
    padding: 4px 10px 6px;
  }
  .tab-add-form input {
    background: var(--bg-sunken);
    border: none;
    color: var(--fg);
    font-family: var(--font-mono);
    font-size: 0.85rem;
    padding: 2px 6px;
    width: 13ch;
    outline: none;
  }
  .add-apply, .add-cancel {
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    font-family: var(--font-mono);
    font-size: 0.7rem;
    padding: 2px 6px;
    cursor: pointer;
  }
  .add-apply { font-weight: 600; }
  .add-apply:disabled, .add-cancel:disabled { opacity: 0.5; cursor: wait; }
  .resume-prompt {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-top: 6px;
    padding: 6px 9px;
    background: var(--bg-elevated);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm);
    font-size: 0.78rem;
  }
  .rp-text { color: var(--fg-muted); }
  .rp-resume, .rp-new {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.72rem;
    padding: 3px 8px;
    cursor: pointer;
    white-space: nowrap;
  }
  .rp-resume:hover { border-color: var(--accent); }
  .rp-new { color: var(--danger, #e5484d); }
  .rp-new:hover { border-color: var(--danger, #e5484d); }

  .add-error {
    color: var(--danger);
    font-size: 0.7rem;
    font-family: var(--font-mono);
    max-width: 18ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .empty {
    color: var(--fg-subtle);
    font-style: italic;
    font-size: 0.85rem;
    padding-left: var(--space-2);
  }
</style>
