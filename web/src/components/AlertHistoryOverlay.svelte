<script>
  // Alert history overlay (step-149, operator: "a running list of
  // alerts"). The append-only log of every raise/recovery the engine
  // accepted — independent of whether ntfy delivery succeeded, was
  // quiet-hours-buffered, or rate-limited. 90-day retention.
  import { alertHistoryOpen, markAlertsSeen } from '../lib/stores.js'
  import { fetchAlertHistory } from '../lib/api.js'

  let entries = []
  let error = ''
  let loaded = false

  $: if ($alertHistoryOpen && !loaded) refresh()

  async function refresh() {
    error = ''
    try {
      const data = await fetchAlertHistory(200)
      entries = data?.entries ?? []
      loaded = true
      if (entries.length) markAlertsSeen(entries[0].id)
    } catch (err) {
      error = err.message ?? String(err)
    }
  }

  function close() {
    alertHistoryOpen.set(false)
    loaded = false // re-fetch next open
  }
  function onKeydown(e) {
    if ($alertHistoryOpen && e.key === 'Escape') {
      e.preventDefault()
      e.stopImmediatePropagation()
      close()
    }
  }
  function fmtTs(ms) {
    return new Date(ms).toLocaleString([], { dateStyle: 'short', timeStyle: 'medium' })
  }
</script>

<svelte:window on:keydown|capture={onKeydown} />

{#if $alertHistoryOpen}
  <div class="backdrop" on:click={close} aria-hidden="true"></div>
  <div class="overlay" role="dialog" aria-label="Alert history">
    <header>
      <h2>Alerts</h2>
      <span class="muted">every raise and recovery, newest first · kept 90 days</span>
      <button class="close" on:click={close} title="close alert history">×</button>
    </header>
    <div class="list">
      {#if error}<p class="error">{error}</p>{/if}
      {#if loaded && entries.length === 0}
        <p class="placeholder">no alerts yet — that's a good thing</p>
      {:else}
        {#each entries as e (e.id)}
          <div class="row">
            <span class="ts">{fmtTs(e.ts)}</span>
            <span class="kind {e.kind}">{e.kind}</span>
            <span class="msg">{e.message}</span>
          </div>
        {/each}
      {/if}
    </div>
  </div>
{/if}

<style>
  .backdrop {
    position: fixed; inset: 0;
    background: rgba(0, 0, 0, 0.45);
    z-index: 60;
  }
  .overlay {
    position: fixed;
    inset: 7vh 12vw;
    background: var(--bg);
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
    align-items: baseline;
    gap: 12px;
    padding: 12px 16px;
    border-bottom: 1px solid var(--border);
    flex: 0 0 auto;
  }
  h2 { margin: 0; font-size: 0.95rem; font-weight: 600; }
  .muted { color: var(--fg-subtle); font-size: 0.68rem; }
  .close {
    margin-left: auto;
    background: transparent; border: none;
    color: var(--fg-muted); font-size: 1.2rem; line-height: 1;
    cursor: pointer; padding: 2px 6px;
  }
  .close:hover { color: var(--fg); }

  .list {
    flex: 1; min-height: 0;
    overflow-y: auto;
    padding: 8px 16px 20px;
    font-size: 0.78rem;
  }
  .placeholder { color: var(--fg-subtle); padding: 14px 0; }
  .error { color: var(--danger, #e5484d); }
  .row {
    display: flex;
    gap: 12px;
    align-items: baseline;
    padding: 5px 0;
    border-bottom: 1px solid var(--border);
  }
  .ts { color: var(--fg-subtle); font-family: var(--font-mono); font-size: 0.7rem; flex: 0 0 11.5em; }
  .kind { flex: 0 0 6em; font-family: var(--font-mono); font-size: 0.7rem; }
  .kind.alert { color: var(--danger, #e5484d); }
  .kind.recovered { color: var(--ok, #30a46c); }
  .msg { color: var(--fg); min-width: 0; }
</style>
