<script>
  // Probes settings section (step-121, v0.5 design §5.3): the whole
  // distributed-probing story in one place. Add probe = type a name,
  // get a copy-paste one-liner for the remote host; the probe shows
  // up in the list below within one heartbeat. Revoke and forget are
  // buttons. No yaml, no restart, no SSH to the central.
  import { onMount } from 'svelte'
  import { probesStore } from '../lib/stores.js'
  import {
    fetchProbeTokens,
    createProbeToken,
    revokeProbeToken,
    forgetProbe,
  } from '../lib/api.js'

  let tokens = []
  let error = ''

  // Add-probe flow state. `minted` holds the one-time creation
  // response; the one-liner is built client-side from it.
  let newName = ''
  let minted = null
  let copied = false

  // Two-click destructive confirm (no window.confirm — keeps the
  // panel's look). Holds the id/probe_id armed for deletion.
  let armedToken = null
  let armedProbe = null

  // Client-side mirror of the server's probe_id shape, for inline
  // feedback before the POST. Server remains the authority.
  const nameRe = /^[a-z0-9][a-z0-9-]{1,31}$/
  $: nameValid = nameRe.test(newName) && newName !== 'local' && newName !== 'all'

  async function refreshTokens() {
    try {
      const data = await fetchProbeTokens()
      tokens = data?.tokens ?? []
    } catch (err) {
      error = err.message ?? String(err)
    }
  }
  onMount(refreshTokens)

  // The install command for the remote host. get.sh downloads the
  // latest release's prebuilt binary (sha256-verified) — no go/npm
  // needed on the probe box. The browser already knows how it reaches
  // this central — location is the right default for central_url
  // (overridable by editing the pasted line).
  function oneLiner(m) {
    const central = `${location.protocol}//${location.host}`
    return (
      'curl -fsSL https://raw.githubusercontent.com/preston-peterson/hoptrail/main/get.sh | ' +
      `bash -s -- --probe --id ${m.name} --central ${central} --token ${m.token}`
    )
  }

  async function addProbe() {
    error = ''
    copied = false
    try {
      minted = await createProbeToken(newName.trim())
      newName = ''
      await refreshTokens()
    } catch (err) {
      error = err.message ?? String(err)
    }
  }

  async function copyCommand() {
    try {
      await navigator.clipboard.writeText(oneLiner(minted))
      copied = true
      setTimeout(() => (copied = false), 2000)
    } catch {
      // Clipboard can fail on non-secure origins — the <code> block
      // below stays selectable as the fallback.
    }
  }

  async function revoke(id) {
    if (armedToken !== id) {
      armedToken = id
      return
    }
    armedToken = null
    error = ''
    try {
      await revokeProbeToken(id)
      if (minted && tokens.find((t) => t.id === id)?.token_prefix === minted.token.slice(0, 4)) minted = null
      await refreshTokens()
    } catch (err) {
      error = err.message ?? String(err)
    }
  }

  async function forget(probeId) {
    if (armedProbe !== probeId) {
      armedProbe = probeId
      return
    }
    armedProbe = null
    error = ''
    try {
      await forgetProbe(probeId)
      // probesStore refreshes on its own 10s poll; nothing to force.
    } catch (err) {
      error = err.message ?? String(err)
    }
  }

  function fmtWhen(ms) {
    if (ms == null) return 'never'
    return new Date(ms).toLocaleString([], { dateStyle: 'short', timeStyle: 'short' })
  }

  $: remoteProbes = $probesStore.filter((p) => p.probe_id !== 'local')
</script>

<div class="probes">
  <p class="hint">
    Probes are extra measurement points at other sites — each tab can
    show any probe's view of the path (the picker in the top bar).
    Adding one takes a name here and one pasted command on the remote
    Linux host.
  </p>

  <!-- Add probe -->
  <div class="add-row">
    <input
      type="text"
      placeholder="probe name, e.g. site-east"
      bind:value={newName}
      on:keydown={(e) => { if (e.key === 'Enter' && nameValid) addProbe() }}
    />
    <button class="add" disabled={!nameValid} on:click={addProbe}>Add probe</button>
  </div>
  {#if newName && !nameValid}
    <p class="field-hint">lowercase letters, digits, and dashes; 2–32 chars; not “local” or “all”</p>
  {/if}

  {#if minted}
    <div class="minted">
      <p class="minted-head">
        Run this on the remote host (as a regular user with sudo):
      </p>
      <code>{oneLiner(minted)}</code>
      <div class="minted-actions">
        <button class="copy" on:click={copyCommand}>{copied ? 'Copied ✓' : 'Copy command'}</button>
        <span class="once">This token is shown only once — if it's lost, revoke and add again.</span>
      </div>
      <p class="minted-foot">
        The probe appears in the list below within ~30 s of starting.
      </p>
    </div>
  {/if}

  <!-- Registered probes -->
  {#if remoteProbes.length > 0}
    <h4>Registered probes</h4>
    <ul class="rows">
      {#each remoteProbes as p (p.probe_id)}
        <li>
          <span class="dot" class:online={p.online} title={p.online ? 'online' : 'offline'}></span>
          <span class="name">{p.label || p.probe_id}</span>
          <span class="meta">
            {[p.ip, p.version, p.online ? '' : `last seen ${fmtWhen(p.last_seen_at)}`].filter(Boolean).join(' · ')}
          </span>
          <button class="danger" on:click={() => forget(p.probe_id)}>
            {armedProbe === p.probe_id ? 'Really remove?' : 'Remove'}
          </button>
        </li>
      {/each}
    </ul>
    <p class="hint">
      Removing a probe forgets its registration and repoints its tabs
      at this host. Revoke its token below too, or it will re-register
      on its next heartbeat.
    </p>
  {/if}

  <!-- Tokens -->
  {#if tokens.length > 0}
    <h4>Probe tokens</h4>
    <ul class="rows">
      {#each tokens as t (t.id)}
        <li>
          <span class="name">{t.name}</span>
          <span class="meta mono">{t.token_prefix}…</span>
          <span class="meta">last used {fmtWhen(t.last_used_at)}</span>
          <button class="danger" on:click={() => revoke(t.id)}>
            {armedToken === t.id ? 'Really revoke?' : 'Revoke'}
          </button>
        </li>
      {/each}
    </ul>
  {/if}

  {#if error}<p class="error">{error}</p>{/if}
</div>

<style>
  .probes { display: flex; flex-direction: column; gap: 8px; }
  .hint { color: var(--fg-muted); font-size: 0.72rem; margin: 0; }
  .field-hint { color: var(--fg-muted); font-size: 0.7rem; margin: 0; }
  .error { color: var(--danger, #e5484d); font-size: 0.75rem; margin: 0; }

  .add-row { display: flex; gap: 7px; }
  .add-row input {
    flex: 1;
    min-width: 0;
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-family: var(--font-mono);
    font-size: 0.78rem;
    padding: 4px 6px;
  }
  button {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.75rem;
    padding: 4px 8px;
    cursor: pointer;
    white-space: nowrap;
  }
  button:hover:not(:disabled) { border-color: var(--fg-subtle); }
  button:disabled { opacity: 0.45; cursor: default; }
  .danger:hover:not(:disabled) { border-color: var(--danger, #e5484d); color: var(--danger, #e5484d); }

  .minted {
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    background: var(--bg-sunken);
    padding: 8px;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .minted-head, .minted-foot { font-size: 0.72rem; color: var(--fg-muted); margin: 0; }
  .minted code {
    display: block;
    font-family: var(--font-mono);
    font-size: 0.7rem;
    line-height: 1.45;
    word-break: break-all;
    user-select: all;
    color: var(--fg);
  }
  .minted-actions { display: flex; align-items: center; gap: 8px; }
  .once { font-size: 0.68rem; color: var(--warning, #f5a524); }

  h4 {
    margin: 6px 0 0;
    font-size: 0.72rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--fg-subtle);
  }
  .rows { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 4px; }
  .rows li { display: flex; align-items: center; gap: 7px; font-size: 0.78rem; }
  .rows .name { font-weight: 600; }
  .rows .meta { color: var(--fg-muted); font-size: 0.7rem; flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .mono { font-family: var(--font-mono); flex: 0 0 auto; }
  .dot {
    width: 8px; height: 8px; border-radius: 50%;
    background: var(--fg-subtle);
    flex: 0 0 auto;
  }
  .dot.online { background: var(--ok, #30a46c); }
</style>
