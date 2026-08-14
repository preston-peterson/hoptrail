<script>
  // Probes settings section (step-121, v0.5 design §5.3): the whole
  // distributed-probing story in one place. Add probe = type a name,
  // get a copy-paste one-liner for the remote host; the probe shows
  // up in the list below within one heartbeat. Revoke and forget are
  // buttons. No yaml, no restart, no SSH to the central.
  import { onMount } from 'svelte'
  import { probesStore, versionStore, refreshProbes } from '../lib/stores.js'
  import {
    fetchProbeTokens,
    createProbeToken,
    revokeProbeToken,
    forgetProbe,
    commandProbeUpdate,
    cancelProbeUpdate,
    setProbePin,
    startProbesUpdateAll,
    fetchProbesUpdateAll,
  } from '../lib/api.js'
  import ProbeUpdateGuide from './ProbeUpdateGuide.svelte'
  import { copyText } from '../lib/clipboard.js'
  import { onDestroy } from 'svelte'

  // Manual-instructions popout (#21) — now the fallback for probes
  // that can't self-update (no reported arch = pre-0.7).
  let updateGuideProbe = null

  // Central-driven updates (#22). The probesStore poll (10s) carries
  // each probe's update row, so state chips refresh on their own.
  // commandProbeUpdate can block 15-25s on the first update of a
  // version (the central downloads + verifies the release binary
  // before responding), so we flag the probe as "commanding" the
  // instant it's clicked — otherwise the button looks dead.
  let commanding = new Set()
  async function updateProbe(id) {
    error = ''
    commanding = new Set(commanding).add(id)
    try {
      await commandProbeUpdate(id)
      // Pull the new in-flight state in immediately so the row goes
      // "preparing…" → "updating…" with no gap that flashes the stale
      // "update available" chip (step-176).
      await refreshProbes()
    } catch (err) {
      error = err.message ?? String(err)
      commanding.delete(id)
      commanding = new Set(commanding)
    }
  }

  // Clear the "preparing…" flag only once the store confirms the probe
  // is actually in flight (or reached a terminal state) — so there's
  // never a window where the row falls back to "update available"
  // mid-command.
  $: if (commanding.size) {
    let changed = false
    for (const id of commanding) {
      const p = $probesStore.find((x) => x.probe_id === id)
      if (p && (inFlight(p) || p.update?.state === 'failed' || p.update?.state === 'applied' || !p.outdated)) {
        commanding.delete(id)
        changed = true
      }
    }
    if (changed) commanding = new Set(commanding)
  }

  async function cancelUpdate(id) {
    error = ''
    try {
      await cancelProbeUpdate(id)
    } catch (err) {
      error = err.message ?? String(err)
    }
  }

  async function togglePin(p) {
    error = ''
    try {
      await setProbePin(p.probe_id, !p.pin)
    } catch (err) {
      error = err.message ?? String(err)
    }
  }

  // Update-all rollout: poll its status while running.
  let rollout = null
  let rolloutTimer = null
  async function updateAll() {
    error = ''
    try {
      rollout = await startProbesUpdateAll()
      watchRollout()
    } catch (err) {
      error = err.message ?? String(err)
    }
  }
  function watchRollout() {
    if (rolloutTimer) return
    rolloutTimer = setInterval(async () => {
      try {
        rollout = await fetchProbesUpdateAll()
        if (!rollout.running) {
          clearInterval(rolloutTimer)
          rolloutTimer = null
        }
      } catch { /* transient */ }
    }, 3000)
  }
  onDestroy(() => { if (rolloutTimer) clearInterval(rolloutTimer) })

  // An in-flight update: command issued, not yet terminal.
  const inFlight = (p) =>
    p.update && (p.update.state === 'pending' || p.update.state === 'applying')
  $: updatableCount = $probesStore.filter(
    (p) => p.probe_id !== 'local' && p.outdated && p.online && p.arch && !p.pin && !inFlight(p)
  ).length

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
    copied = (await copyText(oneLiner(minted))) ? 'ok' : 'failed'
    setTimeout(() => (copied = false), 3000)
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
        <button class="copy" on:click={copyCommand}>{copied === 'ok' ? 'Copied ✓' : 'Copy command'}</button>
        <span class="once">This token is shown only once — if it's lost, revoke and add again.</span>
      </div>
      {#if copied === 'failed'}
        <p class="copy-warn">Couldn't write to the clipboard — click the command text to select it, then copy manually.</p>
      {/if}
      <p class="minted-foot">
        The probe appears in the list below within ~30 s of starting.
      </p>
    </div>
  {/if}

  <!-- Registered probes -->
  {#if remoteProbes.length > 0}
    <h4>Registered probes</h4>
    <ul class="rows probe-rows">
      {#each remoteProbes as p (p.probe_id)}
        <li>
          <div class="prow-head">
            <span class="dot" class:online={p.online} title={p.online ? 'online' : 'offline'}></span>
            <span class="name">{p.label || p.probe_id}</span>
            <span class="prow-spacer"></span>
            <button class="pin" class:pinned={p.pin}
              title={p.pin ? 'pinned: excluded from “Update all” — click to unpin' : 'pin: exclude from “Update all”'}
              on:click={() => togglePin(p)}>📌</button>
            <button class="danger" on:click={() => forget(p.probe_id)}>
              {armedProbe === p.probe_id ? 'Really remove?' : 'Remove'}
            </button>
          </div>
          <div class="prow-meta">
            {[p.ip, p.version, p.online ? '' : `last seen ${fmtWhen(p.last_seen_at)}`].filter(Boolean).join(' · ')}
          </div>
          {#if commanding.has(p.probe_id) || inFlight(p) || p.update?.state === 'failed' || p.outdated}
          <div class="prow-update">
            {#if commanding.has(p.probe_id)}
              <span class="busy">preparing update… (downloading to central)</span>
            {:else if inFlight(p)}
              <span class="busy">updating → v{p.update.target_version}…</span>
              {#if p.update.state === 'pending'}
                <button on:click={() => cancelUpdate(p.probe_id)}>Cancel</button>
              {/if}
            {:else if p.update?.state === 'failed'}
              <span class="failed" title={p.update.error}>update failed</span>
              {#if p.arch}
                <button class="update" on:click={() => updateProbe(p.probe_id)}>Retry</button>
              {:else}
                <button class="update" on:click={() => (updateGuideProbe = p)}>How to update</button>
              {/if}
            {:else if p.outdated}
              <span class="outdated" title="this probe runs an older release than the central">update available</span>
              {#if p.arch}
                <button class="update" title="download the latest release to this probe and apply it — no terminal needed"
                  on:click={() => updateProbe(p.probe_id)}>Update</button>
              {:else}
                <button class="update" title="this probe predates central-driven updates — one manual update enables them"
                  on:click={() => (updateGuideProbe = p)}>How to update</button>
              {/if}
            {/if}
          </div>
          {/if}
        </li>
      {/each}
    </ul>
    {#if updatableCount >= 2 || rollout?.running}
      <div class="rollout">
        <button class="update" disabled={rollout?.running} on:click={updateAll}>
          {rollout?.running ? 'Updating fleet…' : `Update all (${updatableCount})`}
        </button>
        {#if rollout?.running}
          <span class="meta">one at a time{rollout.current ? ` — now: ${rollout.current}` : ''}{rollout.done?.length ? ` · done: ${rollout.done.join(', ')}` : ''}</span>
        {:else if rollout && !rollout.running}
          {#if rollout.failed}
            <span class="failed">stopped at {rollout.failed} — see its row</span>
          {:else if rollout.done?.length}
            <span class="ok-inline">fleet updated ✓ ({rollout.done.join(', ')})</span>
          {/if}
        {/if}
      </div>
    {/if}
    {#if updateGuideProbe}
      <ProbeUpdateGuide
        probe={updateGuideProbe}
        centralVersion={$versionStore ?? ''}
        on:close={() => (updateGuideProbe = null)}
      />
    {/if}
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
  .copy-warn { color: var(--warning, #f5a524); font-size: 0.72rem; margin: 0; }

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

  /* Registered-probe rows: stacked title + meta + update line so the
     IP/version get full width and never truncate. */
  .probe-rows li { display: flex; flex-direction: column; align-items: stretch; gap: 2px; padding: 4px 0; }
  .prow-head { display: flex; align-items: center; gap: 7px; }
  .prow-spacer { flex: 1; min-width: 8px; }
  .prow-meta {
    color: var(--fg-muted);
    font-size: 0.7rem;
    font-family: var(--font-mono);
    margin-left: 15px; /* align under the name, past the status dot */
  }
  .prow-update { margin-left: 15px; display: flex; align-items: center; gap: 7px; flex-wrap: wrap; margin-top: 2px; }
  .dot {
    width: 8px; height: 8px; border-radius: 50%;
    background: var(--fg-subtle);
    flex: 0 0 auto;
  }
  .dot.online { background: var(--ok, #30a46c); }
  .outdated {
    color: var(--warning, #f5a524);
    font-size: 0.68rem;
    flex: 0 0 auto;
  }
  .update:hover:not(:disabled) { border-color: var(--ok, #30a46c); color: var(--ok, #30a46c); }
  .busy { color: var(--accent, #4f8ff7); font-size: 0.68rem; flex: 0 0 auto; }
  .failed { color: var(--danger, #e5484d); font-size: 0.68rem; flex: 0 0 auto; cursor: help; }
  .ok-inline { color: var(--ok, #30a46c); font-size: 0.68rem; }
  .pin {
    padding: 2px 5px;
    opacity: 0.35;
    filter: grayscale(1);
  }
  .pin.pinned { opacity: 1; filter: none; border-color: var(--warning, #f5a524); }
  .rollout { display: flex; align-items: center; gap: 8px; }
</style>
