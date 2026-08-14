<script>
  // Per-tab latency-threshold picker (step-39). Sits in the chart-card
  // header — thresholds are a chart-context control (they paint the
  // reference lines on the latency timeline), not a global setting.
  //
  // Presets cover the operator's connection class. The supervisor
  // accepts any positive (warning < critical) pair, so a follow-up
  // could add a custom-input mode here without backend churn.
  //
  // Empty target / pre-active state: button disabled but visible.

  import { tick } from 'svelte'
  import { activeTabId, activeTab, tabThresholds, setTabThresholds } from '../lib/stores.js'
  import { PRESETS, DEFAULT_PRESET } from '../lib/thresholds.js'

  let open = false
  let pendingKey = null
  let error = null

  // Step-51: "Custom…" affordance for arbitrary (warning, critical)
  // pairs. Supervisor validates warning > 0, critical > 0, warning <
  // critical — no upper bound. UI mirrors the same checks so the
  // operator sees the error at the input, not after a round-trip.
  let customMode = false
  let customWarning = ''
  let customCritical = ''
  let customWarningEl

  // Step-70: thresholds are per-tab now (not per-target). Read by
  // tab_id; write via setTabThresholds which PATCHes /api/tabs/<id>.
  // The same target can have different thresholds in two tabs.
  $: pair = $activeTabId != null ? $tabThresholds[$activeTabId] : null
  $: effectiveWarning = pair?.warning_ms ?? DEFAULT_PRESET.warning
  $: effectiveCritical = pair?.critical_ms ?? DEFAULT_PRESET.critical

  // Trigger label: matched preset key when one matches, else 'Custom'.
  $: triggerLabel = labelFor(effectiveWarning, effectiveCritical, pair)

  function labelFor(warn, crit, pair) {
    const matched = PRESETS.find((p) => p.warning === warn && p.critical === crit)
    if (matched) return matched.label
    return 'Custom'
  }

  $: disabled = $activeTabId == null

  async function toggle() {
    if (disabled) return
    open = !open
    if (open) await tick()
  }

  function close() {
    open = false
    error = null
    customMode = false
    customWarning = ''
    customCritical = ''
  }

  async function pick(preset) {
    if ($activeTabId == null) return
    const sameAsEffective = preset.warning === effectiveWarning && preset.critical === effectiveCritical
    if (sameAsEffective && pendingKey == null) {
      close()
      return
    }
    pendingKey = preset.key
    error = null
    try {
      await setTabThresholds($activeTabId, preset.warning, preset.critical)
      close()
    } catch (err) {
      error = err.message ?? String(err)
    } finally {
      pendingKey = null
    }
  }

  async function enterCustomMode() {
    customMode = true
    customWarning = String(effectiveWarning)
    customCritical = String(effectiveCritical)
    error = null
    await tick()
    customWarningEl?.focus()
    customWarningEl?.select()
  }

  function exitCustomMode() {
    customMode = false
    customWarning = ''
    customCritical = ''
    error = null
  }

  function parsePositiveInt(raw) {
    // Step-74: defensive coercion. `type="number"` inputs with
    // Svelte's bind:value yield a number, not a string — so calling
    // raw.trim() directly throws TypeError (no .trim on Number).
    // Convert to string first so the rest of the parse works
    // regardless of whether the operator typed into a number-typed
    // input or a text-typed one.
    if (raw == null || raw === '') return null
    const s = String(raw).trim()
    if (!s) return null
    const n = Number(s)
    if (!Number.isFinite(n) || n <= 0 || !Number.isInteger(n)) return null
    return n
  }

  async function submitCustom() {
    const w = parsePositiveInt(customWarning)
    const c = parsePositiveInt(customCritical)
    if (w == null || c == null) {
      error = 'warning and critical must be positive whole ms'
      return
    }
    if (w >= c) {
      error = 'warning must be less than critical'
      return
    }
    if ($activeTabId == null) return
    pendingKey = 'custom'
    error = null
    try {
      await setTabThresholds($activeTabId, w, c)
      close()
    } catch (err) {
      error = err.message ?? String(err)
    } finally {
      pendingKey = null
    }
  }

  function onCustomKeydown(e) {
    if (e.key === 'Escape') {
      e.preventDefault()
      exitCustomMode()
    }
  }

  function onKeydown(e) {
    if (!open) return
    if (e.key === 'Escape') { e.preventDefault(); close() }
  }

  function handleWindowClick(e) {
    if (!open) return
    if (e.target.closest?.('.thresholds-root')) return
    close()
  }
</script>

<svelte:window on:click={handleWindowClick} on:keydown={onKeydown} />

<div class="thresholds-root">
  <button
    class="trigger"
    class:open
    {disabled}
    on:click={toggle}
    title={error ?? (disabled ? 'no active tab' : `latency thresholds: warning ${effectiveWarning}ms / critical ${effectiveCritical}ms`)}
  >
    <span class="prefix">band</span>
    <span class="value">{triggerLabel}</span>
    <span class="caret">{open ? '▴' : '▾'}</span>
  </button>

  {#if open}
    <div class="menu" role="listbox" aria-label="latency thresholds">
      <div class="menu-header">connection class</div>
      {#if !customMode}
        {#each PRESETS as p (p.key)}
          {@const isActive = p.warning === effectiveWarning && p.critical === effectiveCritical}
          <button
            class="option"
            class:active={isActive}
            class:pending={pendingKey === p.key}
            role="option"
            aria-selected={isActive}
            on:click={() => pick(p)}
          >
            <span class="option-label">{p.label}</span>
            <span class="option-range">{p.warning}/{p.critical}<span class="unit">ms</span></span>
            {#if isActive}
              <span class="option-mark">●</span>
            {/if}
          </button>
        {/each}
        {@const isCustom = !PRESETS.some((p) => p.warning === effectiveWarning && p.critical === effectiveCritical)}
        <button
          class="option custom-trigger"
          class:active={isCustom}
          on:click|stopPropagation={enterCustomMode}
        >
          <span class="option-label">Custom…</span>
          <span class="option-range">{effectiveWarning}/{effectiveCritical}<span class="unit">ms</span></span>
          {#if isCustom}
            <span class="option-mark">●</span>
          {/if}
        </button>
      {:else}
        <form class="custom-form" on:submit|preventDefault={submitCustom}>
          <label class="custom-field">
            <span class="custom-label">warning</span>
            <input
              bind:this={customWarningEl}
              bind:value={customWarning}
              on:keydown={onCustomKeydown}
              type="number"
              min="1"
              step="1"
              inputmode="numeric"
              disabled={pendingKey != null}
              aria-label="warning threshold in milliseconds"
            />
            <span class="custom-unit">ms</span>
          </label>
          <label class="custom-field">
            <span class="custom-label">critical</span>
            <input
              bind:value={customCritical}
              on:keydown={onCustomKeydown}
              type="number"
              min="1"
              step="1"
              inputmode="numeric"
              disabled={pendingKey != null}
              aria-label="critical threshold in milliseconds"
            />
            <span class="custom-unit">ms</span>
          </label>
          <div class="custom-actions">
            <button
              type="submit"
              class="custom-apply"
              disabled={pendingKey != null}
              title="apply (Enter)"
            >{pendingKey != null ? '…' : 'apply'}</button>
            <button
              type="button"
              class="custom-back"
              on:click={exitCustomMode}
              disabled={pendingKey != null}
              title="back to presets (Esc)"
            >back</button>
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
  /* Shares the visual weight of the chart-card's other header
     controls (linear/log toggle, scroll-back nav). Compact pill
     with a small dropdown — same shape as IntervalPicker so the
     two read as a matched set in the StatusBar / chart header. */
  .thresholds-root {
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
  .trigger:hover:not(:disabled),
  .trigger.open {
    border-color: var(--border-strong);
    background: var(--bg-elevated);
  }
  .trigger:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
  .trigger .prefix { color: var(--fg-muted); }
  .trigger .value {
    font-weight: 600;
    min-width: 3.5ch;
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

  /* Right-anchored popover same as IntervalPicker. */
  .menu {
    position: absolute;
    top: calc(100% + 4px);
    right: 0;
    min-width: 200px;
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
    display: grid;
    grid-template-columns: 1fr auto auto;
    align-items: center;
    column-gap: 10px;
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
  .option-range {
    color: var(--fg-subtle);
    font-size: 0.7rem;
  }
  .option-range .unit { opacity: 0.6; margin-left: 1px; }
  .option-mark {
    color: var(--accent);
    font-size: 0.7rem;
  }

  .error {
    color: var(--danger);
    font-family: var(--font-mono);
    font-size: 0.7rem;
    padding: 4px 10px 6px;
    max-width: 20ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  /* Step-51 custom-mode form. Same structure as IntervalPicker's
     custom mode — labeled fields stack, action row below. Numeric
     spinner buttons hidden because the up/down arrows feel coarse
     for ms entry (operator wants to type a value, not click). */
  .custom-trigger {
    border-top: 1px solid var(--border);
    margin-top: 2px;
    padding-top: 6px;
  }
  .custom-form {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 6px 10px 8px;
    min-width: 200px;
  }
  .custom-field {
    display: grid;
    grid-template-columns: 4.5em 1fr 2em;
    align-items: center;
    column-gap: 6px;
    font-family: var(--font-mono);
    font-size: 0.75rem;
  }
  .custom-label {
    color: var(--fg-muted);
    text-align: right;
  }
  .custom-field input {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-family: var(--font-mono);
    font-size: 0.8rem;
    padding: 3px 6px;
    outline: none;
    width: 100%;
    -moz-appearance: textfield;
  }
  .custom-field input::-webkit-outer-spin-button,
  .custom-field input::-webkit-inner-spin-button {
    -webkit-appearance: none;
    margin: 0;
  }
  .custom-field input:focus { border-color: var(--accent); }
  .custom-unit {
    color: var(--fg-subtle);
    font-size: 0.7rem;
  }
  .custom-actions {
    display: flex;
    gap: 4px;
    margin-top: 2px;
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
</style>
