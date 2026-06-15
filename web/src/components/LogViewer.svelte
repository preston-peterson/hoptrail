<script>
  // Web-UI log viewer (step-128): the daemon's recent log records
  // (in-memory ring; journald keeps the real history) as a dashboard
  // section — dockable/collapsible like the others. Incremental 2s
  // poll by last-seen seq; level filter is client-side; follow mode
  // keeps the view pinned to the newest line. The verbosity knob
  // itself lives in Settings → System (log level, applies live).
  import { onMount, onDestroy } from 'svelte'
  import { fetchLogs } from '../lib/api.js'

  const LEVEL_RANK = { debug: 0, info: 1, warn: 2, error: 3 }
  const MAX_KEPT = 2000

  let entries = []
  let latestSeq = -1
  let minLevel = 'info'
  let follow = true
  let error = ''
  let listEl
  let timer = null

  async function poll() {
    try {
      const res = await fetchLogs(latestSeq >= 0 ? { sinceSeq: latestSeq } : { limit: 500 })
      error = ''
      if (res.entries?.length) {
        entries = [...entries, ...res.entries].slice(-MAX_KEPT)
        if (follow) scrollToEnd()
      }
      latestSeq = res.latest_seq ?? latestSeq
    } catch (err) {
      error = err.message ?? String(err)
    }
  }

  function scrollToEnd() {
    // After the DOM updates.
    requestAnimationFrame(() => {
      if (listEl) listEl.scrollTop = listEl.scrollHeight
    })
  }

  // Turning follow back on snaps to the tail; scrolling up while
  // following turns it off (reading shouldn't fight the autoscroll).
  function onScroll() {
    if (!listEl) return
    const atEnd = listEl.scrollHeight - listEl.scrollTop - listEl.clientHeight < 8
    if (follow && !atEnd) follow = false
  }
  function toggleFollow() {
    follow = !follow
    if (follow) scrollToEnd()
  }

  $: shown = entries.filter((e) => (LEVEL_RANK[e.level] ?? 1) >= LEVEL_RANK[minLevel])

  function fmtTs(ms) {
    return new Date(ms).toLocaleTimeString([], { hour12: false })
  }

  onMount(() => {
    poll()
    timer = setInterval(poll, 2000)
  })
  onDestroy(() => { if (timer) clearInterval(timer) })
</script>

<section class="logs">
  <div class="bar">
    <span class="title">Daemon log</span>
    <label class="lvl">
      show
      <select bind:value={minLevel}>
        <option value="debug">debug+</option>
        <option value="info">info+</option>
        <option value="warn">warn+</option>
        <option value="error">error</option>
      </select>
    </label>
    <button class="follow" class:on={follow} on:click={toggleFollow} title="auto-scroll to the newest line">
      {follow ? '● following' : '○ follow'}
    </button>
    <span class="hint">last {entries.length} records · full history: journalctl -u hoptrail</span>
  </div>

  <div class="list" bind:this={listEl} on:scroll={onScroll}>
    {#if shown.length === 0}
      <div class="placeholder">
        {entries.length === 0 ? 'no log records yet' : `nothing at ${minLevel}+ — lower the filter`}
      </div>
    {:else}
      {#each shown as e (e.seq)}
        <div class="row {e.level}">
          <span class="ts">{fmtTs(e.ts)}</span>
          <span class="lv">{e.level}</span>
          <span class="msg">{e.msg}</span>
          {#if e.attrs}<span class="attrs">{e.attrs}</span>{/if}
        </div>
      {/each}
    {/if}
  </div>
  {#if error}<div class="error">{error}</div>{/if}
</section>

<style>
  .logs {
    display: flex;
    flex-direction: column;
    min-height: 0;
    height: 100%;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }
  .bar {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 6px 10px;
    border-bottom: 1px solid var(--border);
    flex: 0 0 auto;
  }
  .title { font-size: 0.8rem; font-weight: 600; }
  .lvl { display: flex; align-items: center; gap: 5px; font-size: 0.72rem; color: var(--fg-muted); }
  .lvl select {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-size: 0.72rem;
    padding: 2px 4px;
  }
  .follow {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    font-size: 0.7rem;
    padding: 2px 7px;
    cursor: pointer;
  }
  .follow.on { color: var(--ok, #30a46c); border-color: var(--ok, #30a46c); }
  .hint { margin-left: auto; color: var(--fg-subtle); font-size: 0.65rem; }

  .list {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    font-family: var(--font-mono);
    font-size: 0.7rem;
    line-height: 1.5;
    padding: 4px 10px;
  }
  .placeholder { color: var(--fg-subtle); padding: 12px 0; }
  .row { display: flex; gap: 8px; white-space: nowrap; }
  .ts { color: var(--fg-subtle); flex: 0 0 auto; }
  .lv { flex: 0 0 3.2em; }
  .row.debug .lv { color: var(--fg-subtle); }
  .row.info  .lv { color: var(--fg-muted); }
  .row.warn  .lv { color: var(--warning, #f5a524); }
  .row.error .lv { color: var(--danger, #e5484d); }
  .msg { color: var(--fg); flex: 0 0 auto; }
  .attrs { color: var(--fg-muted); overflow: hidden; text-overflow: ellipsis; }

  .error {
    color: var(--danger, #e5484d);
    font-size: 0.72rem;
    padding: 4px 10px;
    border-top: 1px solid var(--border);
    flex: 0 0 auto;
  }
</style>
