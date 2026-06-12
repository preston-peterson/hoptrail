<script>
  // Bundles menu (step-36) — sits beside the "+" pill in the tab row.
  // Click "bundles ▾" to open a dropdown listing saved presets:
  //   - Each bundle row: click loads it (replaces current tab set),
  //     × on hover deletes it.
  //   - "save current as…" inline input at the bottom captures the
  //     current targetsStore under a name and POSTs to /api/bundles.
  //
  // Load semantic = REPLACE: remove tabs not in the bundle, add
  // missing tabs from the bundle, switch to the bundle's first
  // target. Operator gets a clean swap to the bundle's set.

  import { tick } from 'svelte'
  import { tabsStore, setActiveTab, dropTabState } from '../lib/stores.js'
  import { addTarget, removeTarget, fetchBundles, saveBundle, deleteBundle, createTab, deleteTab, reorderTabsApi } from '../lib/api.js'

  let open = false
  let bundles = []
  let bundlesLoading = false

  let saveDraft = ''
  let saving = false
  let saveError = null
  let saveInput

  let busy = null // bundle name currently being loaded, for spinner state

  async function toggleMenu() {
    open = !open
    if (open) {
      bundlesLoading = true
      try {
        const data = await fetchBundles()
        bundles = data.bundles ?? []
      } catch (err) {
        console.error('fetchBundles failed', err)
        bundles = []
      } finally {
        bundlesLoading = false
      }
    }
  }

  function closeMenu() {
    open = false
    saveDraft = ''
    saveError = null
  }

  async function loadBundle(bundle) {
    if (busy) return
    busy = bundle.name
    try {
      // Step-71: bundle.tabs is the canonical content. Legacy bundles
      // (no per-tab state) carry a tabs array synthesized from targets
      // by the server (bare entries — no label, no thresholds), so
      // this code path is uniform either way.
      const tabsToLoad = bundle.tabs ?? bundle.targets.map((t) => ({ target: t }))
      const wantTargets = new Set(tabsToLoad.map((tab) => tab.target))

      // Remove targets not represented in the bundle. Server cascades
      // the tab teardown; the next tabs-poll reconciles tabsStore.
      const currentTargets = new Set($tabsStore.map((tab) => tab.target))
      for (const target of currentTargets) {
        if (!wantTargets.has(target)) {
          try { await removeTarget(target) } catch (err) {
            console.error('loadBundle remove failed', target, err)
          }
        }
      }

      // Then for each bundle entry, ensure the target is monitored
      // and create the tab carrying the bundle's saved display state.
      // We DELETE any pre-existing tabs for the target first so the
      // bundle's tab set is authoritative (avoids accumulating extra
      // tabs across repeated loads).
      for (const target of wantTargets) {
        if (!currentTargets.has(target)) {
          try { await addTarget(target) } catch (err) {
            console.error('loadBundle add failed', target, err)
          }
        }
      }
      // Snapshot the current tabsStore AFTER the add/remove sweep so
      // we know which existing tabs to clean before recreating from
      // the bundle.
      const beforeCreate = $tabsStore.slice()
      const orderedIds = []
      for (const bt of tabsToLoad) {
        try {
          const created = await createTab({
            target: bt.target,
            label: bt.label,
            warningMs: bt.warning_ms,
            criticalMs: bt.critical_ms,
            probeId: bt.probe_id,
          })
          orderedIds.push(created.tab_id)
        } catch (err) {
          // Step-96: a bundle can reference a probe that has since
          // been unregistered — retry as local rather than dropping
          // the whole tab (the target/thresholds are still valuable).
          if (bt.probe_id && bt.probe_id !== 'local') {
            try {
              const created = await createTab({
                target: bt.target,
                label: bt.label,
                warningMs: bt.warning_ms,
                criticalMs: bt.critical_ms,
              })
              orderedIds.push(created.tab_id)
              console.warn('loadBundle: probe', bt.probe_id, 'unavailable; tab created on local', bt.target)
              continue
            } catch (retryErr) {
              console.error('loadBundle createTab retry failed', bt.target, retryErr)
              continue
            }
          }
          console.error('loadBundle createTab failed', bt.target, err)
        }
      }
      // Delete the prior tabs for the bundle's targets so the load is
      // a "replace" not an "append." Done after the new tabs are
      // created so the supervisor's target removal isn't triggered
      // mid-load.
      // Step-78: also drop per-tab localStorage state for the prior
      // tabs so bundle-load doesn't leak orphan focusArea/chartAnchor/
      // timeWindow entries for tab_ids that no longer exist. Only
      // prune on successful delete — if the server delete fails the
      // tab will come back on the next poll and the operator would
      // lose their customizations.
      const newIds = new Set(orderedIds)
      for (const old of beforeCreate) {
        if (wantTargets.has(old.target) && !newIds.has(old.tab_id)) {
          try {
            await deleteTab(old.tab_id)
            dropTabState(old.tab_id)
          } catch (err) {
            console.error('loadBundle deleteOld failed', old.tab_id, err)
          }
        }
      }
      // Server-side reorder so positions match the bundle's order.
      if (orderedIds.length > 0) {
        try { await reorderTabsApi(orderedIds) } catch (err) {
          console.error('loadBundle reorder failed', err)
        }
        setActiveTab(orderedIds[0])
      }
      closeMenu()
    } finally {
      busy = null
    }
  }

  async function removeBundle(name, e) {
    e.stopPropagation()
    try {
      await deleteBundle(name)
      bundles = bundles.filter((b) => b.name !== name)
    } catch (err) {
      console.error('deleteBundle failed', err)
    }
  }

  async function focusSaveInput() {
    await tick()
    saveInput?.focus()
  }

  async function submitSave() {
    const name = saveDraft.trim()
    if (!name) { saveError = 'name required'; return }
    if ($tabsStore.length === 0) { saveError = 'no tabs to save'; return }

    saving = true
    saveError = null
    try {
      // Step-71: save the full per-tab shape — target + label +
      // thresholds. Position is implicit in array order. Bundle
      // restore (loadBundle below) will recreate tabs from this
      // shape, preserving per-tab display state across save/restore.
      const tabs = $tabsStore.map((tab) => ({
        target: tab.target,
        label: tab.label,
        warning_ms: tab.warning_ms,
        critical_ms: tab.critical_ms,
        // Step-96: per-tab probe rides the bundle; omitted for local
        // so pre-step-96 centrals can still parse saved bundles.
        ...(tab.probe_id && tab.probe_id !== 'local' ? { probe_id: tab.probe_id } : {}),
      }))
      await saveBundle(name, { tabs })
      const data = await fetchBundles()
      bundles = data.bundles ?? []
      saveDraft = ''
    } catch (err) {
      saveError = err.message ?? String(err)
    } finally {
      saving = false
    }
  }

  function onSaveKeydown(e) {
    if (e.key === 'Enter') { e.preventDefault(); submitSave() }
    else if (e.key === 'Escape') { e.preventDefault(); saveDraft = ''; saveError = null }
  }

  // Click-outside close: when the menu is open and the user clicks
  // anywhere else, close it. Implemented via a window listener so
  // the click registers regardless of which descendant captured it.
  function handleWindowClick(e) {
    if (!open) return
    if (e.target.closest?.('.bundles-root')) return
    closeMenu()
  }
</script>

<svelte:window on:click={handleWindowClick} />

<div class="bundles-root">
  <button class="bundles-trigger" class:open on:click={toggleMenu} title="saved target bundles">
    bundles ▾
  </button>
  {#if open}
    <div class="bundles-menu">
      <div class="bundles-section-header">saved</div>
      {#if bundlesLoading}
        <div class="bundles-empty">loading…</div>
      {:else if bundles.length === 0}
        <div class="bundles-empty">no bundles yet</div>
      {:else}
        {#each bundles as b (b.name)}
          <div class="bundle-row" class:busy={busy === b.name}>
            <button class="bundle-name" on:click={() => loadBundle(b)} disabled={busy != null}>
              <span class="name">{b.name}</span>
              <span class="count">{(b.tabs ?? b.targets).length}</span>
            </button>
            <button
              class="bundle-delete"
              on:click={(e) => removeBundle(b.name, e)}
              title="delete bundle"
              disabled={busy != null}
            >×</button>
          </div>
        {/each}
      {/if}

      <div class="bundles-section-header">save current</div>
      <div class="bundle-save">
        <input
          bind:this={saveInput}
          bind:value={saveDraft}
          on:keydown={onSaveKeydown}
          on:focus={focusSaveInput}
          placeholder="bundle name"
          disabled={saving}
          maxlength="64"
          spellcheck="false"
        />
        <button class="bundle-save-btn" on:click={submitSave} disabled={saving}>
          {saving ? '…' : 'save'}
        </button>
      </div>
      {#if saveError}
        <div class="bundle-save-error" title={saveError}>{saveError}</div>
      {/if}
    </div>
  {/if}
</div>

<style>
  .bundles-root {
    position: relative;
    display: inline-flex;
  }

  .bundles-trigger {
    background: transparent;
    border: 1px dashed var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    padding: 4px 10px;
    font-family: var(--font-mono);
    font-size: 0.75rem;
    line-height: 1;
    cursor: pointer;
    transition: color 80ms ease-out, border-color 80ms ease-out, background 80ms ease-out;
  }
  .bundles-trigger:hover,
  .bundles-trigger.open {
    color: var(--fg);
    border-color: var(--border-strong);
    background: var(--bg-elevated);
  }

  .bundles-menu {
    position: absolute;
    top: calc(100% + 4px);
    /* Right-anchored: the trigger lives at the right end of the tab
       row, so left-anchoring would push the menu off-page. Anchoring
       the menu's right edge to the trigger's right edge keeps it
       fully on-screen and lets it grow leftward. */
    right: 0;
    min-width: 220px;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    box-shadow: 0 4px 16px rgba(0, 0, 0, 0.3);
    padding: 4px 0;
    z-index: 25;
    max-height: 320px;
    overflow-y: auto;
  }
  .bundles-section-header {
    color: var(--fg-subtle);
    font-family: var(--font-mono);
    font-size: 0.65rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    padding: 4px 10px;
    margin-top: 4px;
  }
  .bundles-section-header:first-child { margin-top: 0; }
  .bundles-empty {
    color: var(--fg-subtle);
    font-family: var(--font-mono);
    font-size: 0.75rem;
    font-style: italic;
    padding: 4px 10px 6px;
  }

  .bundle-row {
    display: flex;
    align-items: center;
    gap: 2px;
    padding: 0 4px;
  }
  .bundle-row.busy { opacity: 0.6; }
  .bundle-name {
    flex: 1;
    background: transparent;
    border: none;
    text-align: left;
    color: var(--fg);
    font-family: var(--font-mono);
    font-size: 0.85rem;
    padding: 4px 6px;
    cursor: pointer;
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-2);
  }
  .bundle-name:hover:not(:disabled) {
    background: var(--bg-sunken);
  }
  .bundle-name .name { color: var(--fg); }
  .bundle-name .count {
    color: var(--fg-subtle);
    font-size: 0.7rem;
  }
  .bundle-delete {
    background: transparent;
    border: none;
    color: var(--fg-subtle);
    font-size: 0.95rem;
    line-height: 1;
    padding: 0 6px;
    cursor: pointer;
    opacity: 0;
    transition: opacity 80ms ease-out, color 80ms ease-out;
  }
  .bundle-row:hover .bundle-delete { opacity: 0.7; }
  .bundle-delete:hover { opacity: 1; color: var(--danger); }

  .bundle-save {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: 4px 8px 6px;
  }
  .bundle-save input {
    flex: 1;
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-family: var(--font-mono);
    font-size: 0.8rem;
    padding: 2px 6px;
    outline: none;
  }
  .bundle-save input:focus {
    border-color: var(--accent);
  }
  .bundle-save-btn {
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    font-family: var(--font-mono);
    font-size: 0.7rem;
    padding: 2px 8px;
    cursor: pointer;
    font-weight: 600;
  }
  .bundle-save-btn:hover:not(:disabled) {
    color: var(--fg);
    border-color: var(--border-strong);
  }
  .bundle-save-btn:disabled { opacity: 0.5; cursor: wait; }
  .bundle-save-error {
    color: var(--danger);
    font-family: var(--font-mono);
    font-size: 0.7rem;
    padding: 0 10px 4px;
  }
</style>
