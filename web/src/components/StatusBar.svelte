<script>
  import { errorStore, versionStore, settingsOpen, statusOpen, envHealth, alertHistoryOpen, unseenAlerts } from '../lib/stores.js'
  import TimeWindowPicker from './TimeWindowPicker.svelte'
  import IntervalPicker from './IntervalPicker.svelte'
  import ProbePicker from './ProbePicker.svelte'




  // dot to indicate "follow system."

  // Step-54: surface the actual error message inline instead of a
  // generic "connection error" badge. Operators caught a real bug
  // ("target query param required (active: ...)") with the old badge
  // hiding the diagnostic behind a hover tooltip — by the time you
  // notice the badge, you don't reach for the mouse to read it.
  // Truncate to keep the header line from wrapping; full message
  // still in the title attribute for the curious.
  const ERROR_MAX_LEN = 80
  $: errorShort = $errorStore
    ? ($errorStore.length > ERROR_MAX_LEN
        ? $errorStore.slice(0, ERROR_MAX_LEN - 1) + '…'
        : $errorStore)
    : ''

  // The target text + click-to-edit affordance that used to live here
  // moved into the TabRow below when step-26 introduced multi-target
  // monitoring. The active tab IS the current target; editing/adding
  // happens via the tab pills, not from the global header.

  // Step-87: clamp the displayed version to just the closest tag
  // (vX.Y.Z) and signal "post-tag build" with a trailing `+`. The
  // raw git-describe string (e.g. v0.2.2-1-g6ee10a3-dirty) stays in
  // the title attribute for when an operator does want the precise
  // commit / dirty info.
  //
  //   v0.2.2                         → "v0.2.2"
  //   v0.2.2-1-g6ee10a3              → "v0.2.2+"
  //   v0.2.2-1-g6ee10a3-dirty        → "v0.2.2+"
  //   dev                            → "dev"
  //   <empty / null>                 → ""
  function shortVersion(raw) {
    if (!raw) return ''
    if (raw === 'dev') return 'dev'
    const m = raw.match(/^v\d+\.\d+\.\d+/)
    if (!m) return raw
    return m[0] === raw ? raw : `${m[0]}+`
  }
  $: versionLabel = shortVersion($versionStore)
</script>

<header>
  <div class="brand">
    <!-- Logo mark: trail of hops ending at the destination, with a
         continuous sonar ping radiating from the final node. Same
         shape as the favicon; inherits color from the surrounding
         wordmark via currentColor so it follows the brand color. -->
    <svg class="logo-mark" viewBox="0 0 32 32" width="26" height="26" aria-hidden="true">
      <polyline points="5,21 12,11 19,23 25,13"
                fill="none"
                stroke="currentColor"
                stroke-width="2.4"
                stroke-linecap="round"
                stroke-linejoin="round" />
      <circle cx="5"  cy="21" r="2.6" fill="currentColor" />
      <circle cx="12" cy="11" r="2.6" fill="currentColor" />
      <circle cx="19" cy="23" r="2.6" fill="currentColor" />
      <circle cx="25" cy="13" r="2.6" fill="currentColor" />
      <!-- Sonar ping — 1.5s cycle, ring expands from the destination
           node and fades. Stroke + radius dialed to be visible at
           small UI sizes; an earlier 1.2px / 7.5 radius / 2s pass was
           too subtle to read as a pulse. -->
      <circle cx="25" cy="13" r="2.6" fill="none" stroke="currentColor" stroke-width="1.8">
        <animate attributeName="r" values="2.6;9.5" dur="1.5s" repeatCount="indefinite" />
        <animate attributeName="opacity" values="1;0" dur="1.5s" repeatCount="indefinite" />
      </circle>
    </svg>
    <span class="logo">hoptrail</span>
    <!-- Step-85: build version from /api/version, dim mono next to the
         wordmark. Suppressed until fetched so we don't render a "ghost"
         element that pops in. -->
    {#if versionLabel}
      <span class="version" title={$versionStore}>{versionLabel}</span>
    {/if}
  </div>

  <div class="right">
    {#if $errorStore}
      <span class="error" title={$errorStore}>
        <span class="error-prefix">error:</span> {errorShort}
      </span>
    {/if}
    <!-- Step-94: probe-view picker. Self-hiding on zero-agent deploys
         (renders nothing until a second probe registers). Leftmost of
         the pickers: it scopes WHAT is measured, the others scope how
         it's probed/viewed. -->
    <ProbePicker />
    <IntervalPicker />
    <TimeWindowPicker />
    <!-- Step-140: environment health dot — green/amber/red distilled
         from /api/status; click opens the status overlay. -->
    <button
      class="theme-toggle health-btn"
      on:click={() => statusOpen.update((v) => !v)}
      title="status"
    >
      <span class="health-dot {$envHealth}" aria-hidden="true"></span>
      <span class="sr-only">status: {$envHealth}</span>
    </button>
    <!-- Step-149: alert history. -->
    <button class="theme-toggle bell-btn" class:lit={$unseenAlerts} on:click={() => alertHistoryOpen.update((v) => !v)} title="alerts">
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor"
           stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9"></path>
        <path d="M13.73 21a2 2 0 0 1-3.46 0"></path>
      </svg>
      <span class="sr-only">alerts</span>
    </button>
    <!-- Step-101: settings panel toggle. SVG gear (design call: crisp
         at any DPI vs the Unicode glyph). -->
    <button class="theme-toggle" on:click={() => settingsOpen.update((v) => !v)} title="settings">
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor"
           stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <circle cx="12" cy="12" r="3"></circle>
        <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"></path>
      </svg>
      <span class="sr-only">settings</span>
    </button>
  </div>
</header>

<style>
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: var(--space-3) var(--space-5);
    background: var(--bg-elevated);
    border-bottom: 1px solid var(--border);
    box-shadow: var(--shadow-sm);
  }
  .brand { display: flex; align-items: center; gap: var(--space-2); color: var(--accent); }
  .logo-mark {
    /* color flows from .brand via currentColor on the SVG elements. */
    flex-shrink: 0;
  }
  .logo {
    font-weight: 600;
    font-size: 1.05rem;
    color: var(--fg);
    letter-spacing: -0.01em;
  }
  /* Step-85: build version. Small, dim, mono — present but not
     competitive with the wordmark. Sits flush after the logo with a
     small gap. No `cursor: help` (operator reported the ? + arrow
     pointer hybrid that browsers render for that rule felt jarring
     over a passive informational element). */
  .version {
    font-family: var(--font-mono);
    font-size: 0.7rem;
    color: var(--fg-subtle);
    letter-spacing: 0;
    margin-left: 2px;
  }

  .right { display: flex; align-items: center; gap: var(--space-3); }
  .error {
    color: var(--danger);
    font-size: 0.8rem;
    cursor: help;
    font-family: var(--font-mono);
    max-width: 60ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .error-prefix {
    font-weight: 600;
    margin-right: 4px;
  }
  .theme-toggle {
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    padding: var(--space-1) var(--space-3);
    font-size: 1rem;
  }
  .theme-toggle:hover { color: var(--fg); border-color: var(--border-strong); }
  .sr-only {
    position: absolute;
    width: 1px; height: 1px;
    padding: 0; margin: -1px; overflow: hidden;
    clip: rect(0, 0, 0, 0); white-space: nowrap; border: 0;
  }
  .health-dot {
    display: inline-block;
    width: 9px;
    height: 9px;
    border-radius: 50%;
    background: var(--fg-subtle);
  }
  .health-dot.green { background: var(--ok, #30a46c); }
  .health-dot.amber { background: var(--warning, #f5a524); }
  .bell-btn { position: relative; }
  .bell-btn.lit { color: var(--warning, #f5a524); }
  .bell-btn.lit::after {
    content: '';
    position: absolute;
    top: 4px;
    right: 4px;
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--warning, #f5a524);
  }
  .health-dot.red {
    background: var(--danger, #e5484d);
    animation: health-pulse 1.6s ease-in-out infinite;
  }
  @keyframes health-pulse {
    50% { opacity: 0.35; }
  }
</style>
