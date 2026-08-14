<script>
  // Per-target probe-interval picker (step-37). Sits in the StatusBar
  // alongside the time-window picker. Scoped to $activeTarget — each
  // tab carries its own cadence, persisted server-side on every click
  // via PATCH /api/targets/<id>.
  //
  // Step-38 (planned same-day follow-up): collapsed-by-default. The
  // expanded pill row (combined with the time-window picker) put 13
  // chips across the StatusBar's right cluster and read as visual
  // noise. The new shape: a single compact "probe Ns ▾" button that
  // opens a small popover with the option list. Same right-anchored
  // pattern as BundlesMenu (lesson from step-36: left-anchoring at
  // the right edge of the page clips off the viewport).
  //
  // Disabled when no target is active (e.g. an empty tab set) — the
  // button stays visible so the affordance is discoverable, just
  // greyed-out and non-interactive.

  import { tick } from 'svelte'
  import { activeTarget, targetIntervals } from '../lib/stores.js'
  import { setTargetInterval } from '../lib/api.js'

  // Presets cover the operator's likely range. 2.5s was added by
  // operator request shortly after the initial set. The supervisor
  // accepts anything inside [200ms, 60s]; the picker doesn't surface
  // every value — operators wanting a custom point can PATCH directly.
  const PRESETS = [
    { label: '0.5s', ms:    500 },
    { label: '1s',   ms:   1000 },
    { label: '2s',   ms:   2000 },
    { label: '2.5s', ms:   2500 },
    { label: '5s',   ms:   5000 },
    { label: '10s',  ms:  10000 },
  ]

  // Bounds match the supervisor's MinProbeInterval / MaxProbeInterval.
  // Validation happens server-side too — the UI guard is a courtesy that
  // surfaces the bound at the source of the typing, not via a round-trip.
  const MIN_INTERVAL_MS = 200
  const MAX_INTERVAL_MS = 60_000

  let open = false
  let pendingMs = null
  let error = null
  let triggerEl

  // Step-50: "Custom…" affordance. When the operator picks Custom, the
  // menu swaps from the preset list to a small inline form. Submitting
  // funnels through pick() so the optimistic-update + error-display
  // paths are identical to a preset pick.
  let customMode = false
  let customInput = ''
  let customInputEl

  $: currentMs = $activeTarget ? $targetIntervals[$activeTarget] : null
  // What the trigger button reflects. While a pick is in flight, show
  // the optimistic value; otherwise show what the daemon reports.
  $: displayMs = pendingMs ?? currentMs
  $: displayLabel = labelFor(displayMs)
  $: disabled = !$activeTarget

  function labelFor(ms) {
    if (ms == null) return '—'
    const preset = PRESETS.find((p) => p.ms === ms)
    if (preset) return preset.label
    // Custom value set via direct PATCH — render as a plain s/ms label
    // so the trigger still tells the truth.
    return ms >= 1000 ? `${(ms / 1000).toFixed(ms % 1000 ? 1 : 0)}s` : `${ms}ms`
  }

  async function toggle() {
    if (disabled) return
    open = !open
    if (open) {
      await tick()
    }
  }

  function close() {
    open = false
    error = null
    customMode = false
    customInput = ''
  }

  async function enterCustomMode() {
    customMode = true
    // Seed with the current value in seconds so the operator can tweak
    // from where they already are. Trim trailing zeros for readability.
    if (displayMs != null) {
      customInput = String(displayMs / 1000)
    } else {
      customInput = ''
    }
    error = null
    await tick()
    customInputEl?.focus()
    customInputEl?.select()
  }

  function exitCustomMode() {
    customMode = false
    customInput = ''
    error = null
  }

  // Custom input is always in seconds; matches the preset labeling
  // convention (0.5s / 1s / 2.5s / …). Decimal is supported for sub-
  // second values. Single semantic = no ambiguity vs an auto-detect
  // seconds-vs-ms heuristic.
  function parseCustomMs(raw) {
    const trimmed = raw.trim()
    if (!trimmed) return null
    const n = Number(trimmed)
    if (!Number.isFinite(n) || n <= 0) return null
    return Math.round(n * 1000)
  }

  async function submitCustom() {
    const ms = parseCustomMs(customInput)
    if (ms == null) {
      error = 'enter a number of seconds (e.g. 0.5)'
      return
    }
    if (ms < MIN_INTERVAL_MS || ms > MAX_INTERVAL_MS) {
      error = `must be between ${MIN_INTERVAL_MS / 1000}s and ${MAX_INTERVAL_MS / 1000}s`
      return
    }
    await pick(ms)
  }

  function onCustomKeydown(e) {
    if (e.key === 'Escape') {
      e.preventDefault()
      exitCustomMode()
    }
  }

  async function pick(ms) {
    if (!$activeTarget) return
    if (ms === currentMs && pendingMs == null) {
      close()
      return
    }
    pendingMs = ms
    error = null
    try {
      await setTargetInterval($activeTarget, ms)
      targetIntervals.update((map) => ({ ...map, [$activeTarget]: ms }))
      close()
    } catch (err) {
      error = err.message ?? String(err)
    } finally {
      pendingMs = null
    }
  }

  function onKeydown(e) {
    if (!open) return
    if (e.key === 'Escape') { e.preventDefault(); close() }
  }

  // Click-outside dismiss: same pattern as BundlesMenu. Window-level
  // listener so any descendant of the root short-circuits, anything
  // else closes.
  function handleWindowClick(e) {
    if (!open) return
    if (e.target.closest?.('.interval-root')) return
    close()
  }
</script>

<svelte:window on:click={handleWindowClick} on:keydown={onKeydown} />

<div class="interval-root">
  <button
    class="trigger"
    class:open
    {disabled}
    bind:this={triggerEl}
    on:click={toggle}
    title={error ?? (disabled ? 'no active target' : `probe interval${$activeTarget ? ` for ${$activeTarget}` : ''}`)}
  >
    <span class="prefix">probe</span>
    <span class="value">{displayLabel}</span>
    <span class="caret">{open ? '▴' : '▾'}</span>
  </button>

  {#if open}
    <div class="menu" role="listbox" aria-label="probe interval">
      <div class="menu-header">probe every</div>
      {#if !customMode}
        {#each PRESETS as p (p.ms)}
          <button
            class="option"
            class:active={displayMs === p.ms}
            class:pending={pendingMs === p.ms}
            role="option"
            aria-selected={displayMs === p.ms}
            on:click={() => pick(p.ms)}
          >
            <span class="option-label">{p.label}</span>
            {#if displayMs === p.ms}
              <span class="option-mark">●</span>
            {/if}
          </button>
        {/each}
        <button
          class="option custom-trigger"
          class:active={displayMs != null && !PRESETS.some((p) => p.ms === displayMs)}
          on:click|stopPropagation={enterCustomMode}
        >
          <span class="option-label">Custom…</span>
          {#if displayMs != null && !PRESETS.some((p) => p.ms === displayMs)}
            <span class="option-mark">●</span>
          {/if}
        </button>
      {:else}
        <form class="custom-form" on:submit|preventDefault={submitCustom}>
          <input
            bind:this={customInputEl}
            bind:value={customInput}
            on:keydown={onCustomKeydown}
            placeholder="seconds (e.g. 1.5)"
            inputmode="decimal"
            disabled={pendingMs != null}
            aria-label="custom probe interval in seconds"
          />
          <div class="custom-actions">
            <button
              type="submit"
              class="custom-apply"
              disabled={pendingMs != null || !customInput.trim()}
              title="apply (Enter)"
            >{pendingMs != null ? '…' : 'apply'}</button>
            <button
              type="button"
              class="custom-back"
              on:click={exitCustomMode}
              disabled={pendingMs != null}
              title="back to presets (Esc)"
            >back</button>
          </div>
          <div class="custom-hint">
            seconds, {MIN_INTERVAL_MS / 1000} — {MAX_INTERVAL_MS / 1000}
          </div>
        </form>
      {/if}
      {#if error}
        <div class="error" title={error}>{error}</div>
      {/if}
    </div>
  {/if}
</div>

<style>
  .interval-root {
    position: relative;
    display: inline-flex;
  }

  /* Trigger button — compact, label + value + caret. Mirrors the
     visual weight of BundlesMenu's trigger so they read as siblings
     when both are present. */
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
  .trigger:hover:not(:disabled),
  .trigger.open {
    border-color: var(--border-strong);
    background: var(--bg-elevated);
  }
  .trigger:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
  .trigger .prefix {
    color: var(--fg-muted);
  }
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

  /* Popover — right-anchored so it grows leftward into the page,
     never off the viewport's right edge (lesson #12-shaped: the
     StatusBar's right cluster is at the very edge). Same z-index
     +max-height pattern as BundlesMenu so they coexist cleanly. */
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
  .option.pending { opacity: 0.6; }
  .option-mark {
    color: var(--accent);
    font-size: 0.7rem;
  }

  .error {
    color: var(--danger);
    font-family: var(--font-mono);
    font-size: 0.7rem;
    padding: 4px 10px 6px;
    max-width: 18ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  /* Step-50 custom-mode form. Same vertical rhythm as the preset
     list — input on top, action row below, fine-print hint last. */
  .custom-trigger {
    border-top: 1px solid var(--border);
    margin-top: 2px;
    padding-top: 6px;
  }
  .custom-form {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 4px 10px 6px;
    min-width: 180px;
  }
  .custom-form input {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-family: var(--font-mono);
    font-size: 0.8rem;
    padding: 4px 6px;
    outline: none;
  }
  .custom-form input:focus { border-color: var(--accent); }
  .custom-actions {
    display: flex;
    gap: 4px;
  }
  .custom-apply,
  .custom-back {
    flex: 1;
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    font-family: var(--font-mono);
    font-size: 0.75rem;
    padding: 3px 8px;
    cursor: pointer;
  }
  .custom-apply:hover:not(:disabled) {
    color: var(--accent);
    border-color: var(--accent);
  }
  .custom-back:hover:not(:disabled) { color: var(--fg); }
  .custom-apply:disabled,
  .custom-back:disabled { opacity: 0.5; cursor: not-allowed; }
  .custom-hint {
    color: var(--fg-subtle);
    font-family: var(--font-mono);
    font-size: 0.65rem;
    line-height: 1.4;
  }
</style>
