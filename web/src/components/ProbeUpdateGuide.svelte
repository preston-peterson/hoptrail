<script>
  // Probe update guide (#21): a registered probe is running an older
  // release than this central — walk through updating it. Probes have
  // no self-update yet (central-driven updates are a future design
  // pass); the path is one pasted command on the probe host, same
  // get.sh as the original install. Existing probe.yaml is preserved,
  // so no id/central/token re-entry.
  import { createEventDispatcher } from 'svelte'
  import { copyText } from '../lib/clipboard.js'

  export let probe = null // { probe_id, label, version, ip, online }
  export let centralVersion = ''

  const dispatch = createEventDispatcher()
  let copied = '' // '' | 'ok' | 'failed'

  const updateCmd =
    'curl -fsSL https://get.hoptrail.net | bash -s -- --probe'

  async function copy() {
    copied = (await copyText(updateCmd)) ? 'ok' : 'failed'
    setTimeout(() => (copied = ''), 3000)
  }

  function close() {
    dispatch('close')
  }
  function onKeydown(e) {
    if (e.key === 'Escape') {
      e.preventDefault()
      e.stopImmediatePropagation()
      close()
    }
  }
</script>

<svelte:window on:keydown|capture={onKeydown} />

<div class="backdrop" on:click={close} aria-hidden="true"></div>
<div class="modal" role="dialog" aria-label="Probe update guide">
  <header>
    <h2>Update probe “{probe?.label || probe?.probe_id}”</h2>
    <button class="close" on:click={close} title="close">×</button>
  </header>

  <div class="body">
    <p class="versions">
      running <code class="inline">{probe?.version ?? '?'}</code>
      → this central is on <code class="inline">{centralVersion}</code>
    </p>

    <div class="step">
      <h3>1. Open a shell on the probe host</h3>
      <p>
        {#if probe?.ip}
          Its last heartbeat came from <code class="inline">{probe.ip}</code>.
        {:else}
          (The probe hasn't reported an address yet.)
        {/if}
        Log in as the same regular user that installed it.
      </p>
    </div>

    <div class="step">
      <h3>2. Run the updater</h3>
      <div class="copy-row">
        <code>{updateCmd}</code>
        <button on:click={copy}>{copied === 'ok' ? 'Copied ✓' : 'Copy'}</button>
      </div>
      {#if copied === 'failed'}
        <p class="copy-warn">Couldn't write to the clipboard — click the command text to select it, then copy manually.</p>
      {/if}
      <p>
        Same command family as the original install — it downloads the
        latest release's binary for the host's architecture, verifies
        the sha256, and restarts the probe service. The existing probe
        config is preserved: no name, address, or token to re-enter.
      </p>
    </div>

    <div class="step last">
      <h3>3. Watch it come back</h3>
      <p>
        The probe reappears in the list with the new version within one
        heartbeat (~60 s). Measurements buffered during the restart
        gap are not lost — anything unsent spills to the probe's local
        buffer and drains on reconnect.
      </p>
      <p class="muted">
        Running a self-built binary instead? Copy it to
        <code class="inline">/opt/hoptrail/update/hoptrail</code> on the
        probe host and run
        <code class="inline">sudo /opt/hoptrail/update.sh --staged</code>.
      </p>
    </div>
  </div>
</div>

<style>
  .backdrop {
    position: fixed; inset: 0;
    background: rgba(0, 0, 0, 0.55);
    z-index: 70;
  }
  .modal {
    position: fixed;
    top: 50%; left: 50%;
    transform: translate(-50%, -50%);
    width: min(560px, calc(100vw - 32px));
    max-height: 88vh;
    overflow-y: auto;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    box-shadow: 0 12px 40px rgba(0, 0, 0, 0.45);
    z-index: 71;
  }
  header {
    display: flex; align-items: center; justify-content: space-between;
    padding: 12px 16px;
    border-bottom: 1px solid var(--border);
  }
  h2 { margin: 0; font-size: 0.95rem; font-weight: 600; }
  .close {
    background: transparent; border: none; color: var(--fg-muted);
    font-size: 1.2rem; line-height: 1; cursor: pointer; padding: 2px 6px;
  }
  .close:hover { color: var(--fg); }

  .body { padding: 14px 16px; display: flex; flex-direction: column; gap: 14px; }
  .versions { margin: 0; font-size: 0.78rem; color: var(--fg-muted); }
  .step h3 { margin: 0 0 4px; font-size: 0.8rem; font-weight: 600; }
  .step p { margin: 0 0 6px; font-size: 0.75rem; color: var(--fg-muted); line-height: 1.5; }
  .step .muted { color: var(--fg-subtle); font-size: 0.7rem; }
  .step.last { border-top: 1px solid var(--border); padding-top: 12px; }

  .copy-row { display: flex; gap: 6px; align-items: stretch; margin-bottom: 6px; }
  .copy-warn { color: var(--warning, #f5a524); font-size: 0.72rem; margin: 0 0 6px; }
  .copy-row code {
    flex: 1; min-width: 0;
    padding: 6px 9px;
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    font-family: var(--font-mono);
    font-size: 0.74rem;
    word-break: break-all;
    user-select: all;
  }
  code.inline {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 0 4px;
    font-family: var(--font-mono);
  }
  .copy-row button {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.72rem;
    padding: 4px 10px;
    cursor: pointer;
    white-space: nowrap;
  }
  .copy-row button:hover { border-color: var(--fg-subtle); }
</style>
