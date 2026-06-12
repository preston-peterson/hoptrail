<script>
  // Full-screen-ish daemon-log overlay (step-129). Logs are a
  // diagnostic surface (operator: "it's only there for diagnostics")
  // — opened on demand from Settings → System → View daemon log, not
  // a standing dashboard tenant. Hosts the LogViewer, which polls
  // only while mounted, so a closed overlay costs nothing.
  import { logsOpen } from '../lib/stores.js'
  import LogViewer from './LogViewer.svelte'

  function close() {
    logsOpen.set(false)
  }
  function onKeydown(e) {
    if ($logsOpen && e.key === 'Escape') {
      e.preventDefault()
      // Stop the settings panel's own Esc handler from also firing —
      // one Esc dismisses one layer.
      e.stopImmediatePropagation()
      close()
    }
  }
</script>

<svelte:window on:keydown|capture={onKeydown} />

{#if $logsOpen}
  <div class="backdrop" on:click={close} aria-hidden="true"></div>
  <div class="overlay" role="dialog" aria-label="Daemon log">
    <header>
      <h2>Daemon log</h2>
      <button class="close" on:click={close} title="close log viewer">×</button>
    </header>
    <div class="body">
      <LogViewer />
    </div>
  </div>
{/if}

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.45);
    z-index: 60;
  }
  .overlay {
    position: fixed;
    inset: 5vh 5vw;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    box-shadow: 0 12px 40px rgba(0, 0, 0, 0.45);
    z-index: 61;
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 10px 14px;
    border-bottom: 1px solid var(--border);
    flex: 0 0 auto;
  }
  h2 { margin: 0; font-size: 0.95rem; font-weight: 600; }
  .close {
    background: transparent;
    border: none;
    color: var(--fg-muted);
    font-size: 1.2rem;
    line-height: 1;
    cursor: pointer;
    padding: 2px 6px;
  }
  .close:hover { color: var(--fg); }
  .body {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    padding: 10px;
  }
  .body :global(.logs) { flex: 1; min-height: 0; border: none; }
</style>
