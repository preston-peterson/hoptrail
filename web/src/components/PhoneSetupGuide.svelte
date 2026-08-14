<script>
  // Phone setup guide (step-138, operator request — mirrors the
  // 4-step walkthrough pattern from their other project): install the
  // ntfy app, point it at the server, subscribe to the topic, test —
  // personalized with the actual configured values, copy buttons,
  // and a can't-reach-localhost tripwire.
  import { createEventDispatcher } from 'svelte'
  import { sendTestAlert } from '../lib/api.js'
  import { copyText } from '../lib/clipboard.js'

  export let serverUrl = ''
  export let topic = ''

  const dispatch = createEventDispatcher()
  let copied = '' // 'server' | 'sub'
  let testMsg = ''

  $: subscriptionUrl = serverUrl && topic
    ? serverUrl.replace(/\/+$/, '') + '/' + topic
    : ''
  $: localhostWarn = /\/\/(localhost|127\.0\.0\.1)([:/]|$)/.test(serverUrl)

  async function copy(text, which) {
    copied = (await copyText(text)) ? which : 'failed'
    setTimeout(() => (copied = ''), 3000)
  }

  async function test() {
    testMsg = 'sending…'
    try {
      await sendTestAlert()
      testMsg = 'delivered ✓ — your phone should buzz'
    } catch (err) {
      testMsg = '✗ ' + (err.message ?? String(err))
    }
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
<div class="modal" role="dialog" aria-label="Phone setup guide">
  <header>
    <h2>Get alerts on your phone</h2>
    <button class="close" on:click={close} title="close">×</button>
  </header>

  <div class="body">
    <div class="step">
      <h3>1. Install the ntfy app</h3>
      <p>ntfy is free and open-source. Pick your phone's store:</p>
      <div class="links">
        <a href="https://play.google.com/store/apps/details?id=io.heckel.ntfy" target="_blank" rel="noopener noreferrer">↗ Google Play (Android)</a>
        <a href="https://apps.apple.com/us/app/ntfy/id1625396347" target="_blank" rel="noopener noreferrer">↗ App Store (iOS)</a>
        <a href="https://f-droid.org/en/packages/io.heckel.ntfy/" target="_blank" rel="noopener noreferrer">↗ F-Droid</a>
      </div>
    </div>

    <div class="step">
      <h3>2. Add the server</h3>
      {#if serverUrl}
        <p>In the app: Settings → Default server → set it to:</p>
        <div class="copy-row">
          <code>{serverUrl}</code>
          <button on:click={() => copy(serverUrl, 'server')}>{copied === 'server' ? 'Copied ✓' : 'Copy'}</button>
        </div>
        {#if localhostWarn}
          <p class="warn">⚠ The server URL points at localhost — your phone can't reach
            that. Use this host's LAN address in the Alerts settings.</p>
        {/if}
      {:else}
        <p class="warn">No ntfy server configured yet — set one up in the Alerts
          section first (install a local one or enter your server's URL).</p>
      {/if}
    </div>

    <div class="step">
      <h3>3. Subscribe to the topic</h3>
      {#if subscriptionUrl}
        <p>In the app: + (Add subscription) → topic <code class="inline">{topic}</code> —
          or paste the full URL:</p>
        <div class="copy-row">
          <code>{subscriptionUrl}</code>
          <button on:click={() => copy(subscriptionUrl, 'sub')}>{copied === 'sub' ? 'Copied ✓' : 'Copy'}</button>
        </div>
        <p class="muted">Anyone who knows the topic name can read these alerts —
          that's the only secret on a token-less setup.</p>
      {:else}
        <p class="warn">Set a topic in the Alerts section first.</p>
      {/if}
    </div>

    {#if copied === 'failed'}
      <p class="copy-warn">Couldn't write to the clipboard — click the text to select it, then copy manually.</p>
    {/if}

    <div class="step last">
      <h3>4. Test it</h3>
      <p>With the app subscribed, verify the whole pipeline:</p>
      <div class="copy-row">
        <button class="primary" disabled={!subscriptionUrl} on:click={test}>Send test notification</button>
        {#if testMsg}<span class="status">{testMsg}</span>{/if}
      </div>
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
  .step h3 { margin: 0 0 4px; font-size: 0.8rem; font-weight: 600; }
  .step p { margin: 0 0 6px; font-size: 0.75rem; color: var(--fg-muted); line-height: 1.5; }
  .step .warn { color: var(--warning, #f5a524); }
  .step .muted { color: var(--fg-subtle); font-size: 0.7rem; }
  .step.last { border-top: 1px solid var(--border); padding-top: 12px; }

  .links { display: flex; gap: 8px; flex-wrap: wrap; }
  .links a {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.72rem;
    padding: 4px 9px;
    text-decoration: none;
  }
  .links a:hover { border-color: var(--accent, #4f8ff7); color: var(--accent, #4f8ff7); }

  .copy-row { display: flex; gap: 6px; align-items: stretch; }
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
  .copy-row button, .primary {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.72rem;
    padding: 4px 10px;
    cursor: pointer;
    white-space: nowrap;
  }
  .copy-row button:hover:not(:disabled) { border-color: var(--fg-subtle); }
  .primary:hover:not(:disabled) { border-color: var(--ok, #30a46c); color: var(--ok, #30a46c); }
  .primary:disabled { opacity: 0.45; cursor: default; }
  .status { font-size: 0.72rem; color: var(--fg-muted); align-self: center; }
  .copy-warn { color: var(--warning, #f5a524); font-size: 0.72rem; margin: 0; }
</style>
