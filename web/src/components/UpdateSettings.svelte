<script>
  // Updates settings section (step-124 upload mode + #11 release
  // mode): check GitHub for the latest release, download it verified,
  // or upload a binary by hand — every road stages the same slot and
  // the one apply path swaps/setcaps/restarts through the sudoers rule.
  import { onDestroy } from 'svelte'
  import {
    fetchUpdateStatus, uploadUpdateBinary, applyUpdate,
    checkForUpdates, downloadUpdate, patchUpdateSettings,
  } from '../lib/api.js'

  let status = null
  let error = ''
  let uploading = false
  let checking = false
  let downloading = false
  // applying: false → 'applying' → 'restarting' → 'done'
  let applying = false
  let restartTimer = null
  let newVersion = ''

  const INTERVALS = [
    { value: 'off', label: 'never' },
    { value: 'daily', label: 'daily' },
    { value: 'weekly', label: 'weekly' },
    { value: 'monthly', label: 'monthly' },
  ]

  async function refresh() {
    error = ''
    try {
      status = await fetchUpdateStatus()
    } catch (err) {
      error = err.message ?? String(err)
    }
  }
  refresh()

  async function check() {
    error = ''
    checking = true
    try {
      await checkForUpdates()
      await refresh()
    } catch (err) {
      error = err.message ?? String(err)
    } finally {
      checking = false
    }
  }

  async function download() {
    error = ''
    downloading = true
    try {
      await downloadUpdate()
      await refresh()
    } catch (err) {
      error = err.message ?? String(err)
    } finally {
      downloading = false
    }
  }

  async function onIntervalChange(e) {
    error = ''
    try {
      status = await patchUpdateSettings({ check_interval: e.target.value })
    } catch (err) {
      error = err.message ?? String(err)
      refresh()
    }
  }

  function ago(ms) {
    const s = Math.max(0, Math.round((Date.now() - ms) / 1000))
    if (s < 90) return 'just now'
    if (s < 5400) return `${Math.round(s / 60)}m ago`
    if (s < 129600) return `${Math.round(s / 3600)}h ago`
    return `${Math.round(s / 86400)}d ago`
  }

  async function onPick(e) {
    const file = e.target.files?.[0]
    e.target.value = ''
    if (!file) return
    error = ''
    uploading = true
    try {
      await uploadUpdateBinary(file)
      await refresh()
    } catch (err) {
      error = err.message ?? String(err)
    } finally {
      uploading = false
    }
  }

  async function apply() {
    error = ''
    applying = 'applying'
    try {
      const res = await applyUpdate()
      newVersion = res.new_version
      applying = 'restarting'
      watchRestart()
    } catch (err) {
      applying = false
      error = err.message ?? String(err)
      refresh()
    }
  }

  // After apply the daemon restarts (~1s + startup). Poll /api/version
  // until it answers with the new version, then reload the page so the
  // UI bundle matches the binary it came from.
  function watchRestart() {
    if (restartTimer) return
    restartTimer = setInterval(async () => {
      try {
        const res = await fetch('/api/version')
        if (!res.ok) return
        const v = await res.json()
        if (v.version && v.version !== status?.running_version) {
          clearInterval(restartTimer)
          restartTimer = null
          applying = 'done'
          setTimeout(() => location.reload(), 1200)
        }
      } catch {
        // daemon mid-restart — keep polling
      }
    }, 2000)
  }
  onDestroy(() => { if (restartTimer) clearInterval(restartTimer) })

  $: canApply =
    status?.staged?.present && !status?.staged?.error && status?.sudoers?.ok && !applying
</script>

<div class="updates">
  {#if !status}
    <p class="muted">loading…</p>
  {:else}
    <p class="row">
      <span class="label">Running version</span>
      <span class="mono">{status.running_version}</span>
    </p>

    {#if !status.sudoers.ok}
      <p class="warn">{status.sudoers.error}</p>
    {/if}

    {#if applying === 'applying'}
      <p class="muted">applying…</p>
    {:else if applying === 'restarting'}
      <p class="busy">Updating to {newVersion} — daemon restarting, this page will reload…</p>
    {:else if applying === 'done'}
      <p class="ok">Updated to {newVersion} ✓ — reloading…</p>
    {:else}
      {#if status.release_available}
        <div class="release">
          <div class="actions">
            <button class="check" disabled={checking || downloading} on:click={check}>
              {checking ? 'Checking…' : 'Check for updates'}
            </button>
            {#if status.last_check?.update_available}
              <button class="apply" disabled={downloading || checking} on:click={download}>
                {downloading ? 'Downloading…' : `Download v${status.last_check.latest_version}`}
              </button>
            {/if}
          </div>
          {#if status.last_check}
            <p class="muted small checkline">
              {#if status.last_check.error}
                <span class="warn">last check failed: {status.last_check.error}</span>
              {:else if status.last_check.update_available}
                <span class="avail">v{status.last_check.latest_version} is available</span>
                {#if status.last_check.release_url}
                  · <a href={status.last_check.release_url} target="_blank" rel="noreferrer">release notes</a>
                {/if}
              {:else}
                up to date (latest is v{status.last_check.latest_version})
              {/if}
              · checked {ago(status.last_check.checked_at)}
            </p>
          {/if}
          <label class="interval">
            <span class="label">Check automatically</span>
            <select value={status.check_interval} on:change={onIntervalChange}>
              {#each INTERVALS as iv}
                <option value={iv.value}>{iv.label}</option>
              {/each}
            </select>
          </label>
        </div>
      {/if}

      {#if status.staged.present}
        <p class="row">
          <span class="label">Staged binary</span>
          {#if status.staged.error}
            <span class="warn">unusable: {status.staged.error}</span>
          {:else}
            <span class="mono">{status.staged.version}</span>
            <span class="muted small">({Math.round(status.staged.size_bytes / 1048576)} MB)</span>
          {/if}
        </p>
      {/if}

      <div class="actions">
        <label class="upload-btn" class:disabled={uploading}>
          {uploading ? 'Uploading…' : status.staged.present ? 'Upload a different binary' : 'Upload a hoptrail binary'}
          <input type="file" on:change={onPick} disabled={uploading} />
        </label>
        {#if status.staged.present && !status.staged.error}
          <button class="apply" disabled={!canApply} on:click={apply}>
            Apply {status.staged.version}
          </button>
        {/if}
      </div>
      <p class="muted small">
        Applies in place: the current binary is backed up, the new one
        swapped in, <code>cap_net_raw</code> re-applied, and the
        service restarted — all from here. Downloads come from GitHub
        releases and are sha256-verified before staging; the upload
        button covers self-built binaries.
      </p>
    {/if}

    {#if error}<p class="error">{error}</p>{/if}
  {/if}
</div>

<style>
  .updates { display: flex; flex-direction: column; gap: 8px; }
  .row { display: flex; align-items: baseline; gap: 8px; margin: 0; font-size: 0.8rem; }
  .label { color: var(--fg-muted); font-size: 0.75rem; }
  .mono { font-family: var(--font-mono); font-size: 0.78rem; }
  .muted { color: var(--fg-muted); margin: 0; }
  .small { font-size: 0.7rem; }
  .warn { color: var(--warning, #f5a524); font-size: 0.75rem; margin: 0; }
  .busy { color: var(--fg); font-size: 0.8rem; margin: 0; }
  .ok { color: var(--ok, #30a46c); font-size: 0.8rem; margin: 0; }
  .error { color: var(--danger, #e5484d); font-size: 0.75rem; margin: 0; }

  .actions { display: flex; align-items: center; gap: 8px; }
  .release {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding-bottom: 8px;
    border-bottom: 1px solid var(--border);
  }
  .check {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.75rem;
    padding: 4px 8px;
    cursor: pointer;
  }
  .check:hover:not(:disabled) { border-color: var(--fg-subtle); }
  .check:disabled { opacity: 0.45; cursor: default; }
  .checkline { margin: 0; }
  .checkline a { color: var(--fg-muted); }
  .avail { color: var(--ok, #30a46c); }
  .interval { display: flex; align-items: center; gap: 8px; }
  .interval select {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.75rem;
    padding: 2px 6px;
  }
  .upload-btn {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.75rem;
    padding: 4px 8px;
    cursor: pointer;
  }
  .upload-btn.disabled { opacity: 0.45; cursor: default; }
  .upload-btn:hover:not(.disabled) { border-color: var(--fg-subtle); }
  .upload-btn input { display: none; }
  .apply {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.75rem;
    padding: 4px 8px;
    cursor: pointer;
  }
  .apply:hover:not(:disabled) { border-color: var(--ok, #30a46c); color: var(--ok, #30a46c); }
  .apply:disabled { opacity: 0.45; cursor: default; }
</style>
