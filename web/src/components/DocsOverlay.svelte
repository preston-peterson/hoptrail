<script>
  // In-app documentation viewer (step-143, operator request: "gear →
  // Documentation → tab for each doc", mirroring the sibling
  // project's docs page). Sidebar tab per guide, markdown rendered
  // client-side; the docs come embedded in the running binary, so
  // they always match the version you're on.
  import { docsOpen } from '../lib/stores.js'
  import { fetchDocsIndex, fetchDoc } from '../lib/api.js'
  import { renderMarkdown } from '../lib/markdown.js'

  let index = []
  let active = null
  let html = ''
  let error = ''
  let bodyEl

  $: if ($docsOpen && index.length === 0) load()

  async function load() {
    try {
      const data = await fetchDocsIndex()
      index = data?.docs ?? []
      if (index.length && !active) open(index[0].slug)
    } catch (err) {
      error = err.message ?? String(err)
    }
  }

  async function open(slug) {
    error = ''
    active = slug
    try {
      html = renderMarkdown(await fetchDoc(slug))
      if (bodyEl) bodyEl.scrollTop = 0
    } catch (err) {
      error = err.message ?? String(err)
      html = ''
    }
  }

  // Intercept intra-doc links ([user guide](user-guide.md)) — switch
  // tabs instead of navigating.
  function onBodyClick(e) {
    const a = e.target.closest?.('a[data-doc]')
    if (!a) return
    e.preventDefault()
    const slug = a.getAttribute('data-doc')
    if (index.some((d) => d.slug === slug)) open(slug)
  }

  function close() {
    docsOpen.set(false)
  }
  function onKeydown(e) {
    if ($docsOpen && e.key === 'Escape') {
      e.preventDefault()
      e.stopImmediatePropagation()
      close()
    }
  }
</script>

<svelte:window on:keydown|capture={onKeydown} />

{#if $docsOpen}
  <div class="backdrop" on:click={close} aria-hidden="true"></div>
  <div class="overlay" role="dialog" aria-label="Documentation">
    <header>
      <h2>Documentation</h2>
      <span class="muted">embedded with this build — always matches the running version</span>
      <button class="close" on:click={close} title="close documentation">×</button>
    </header>
    <div class="split">
      <nav>
        {#each index as d (d.slug)}
          <button class="tab" class:active={active === d.slug} on:click={() => open(d.slug)}>
            {d.title}
          </button>
        {/each}
      </nav>
      <!-- svelte-ignore a11y-no-static-element-interactions a11y-click-events-have-key-events -->
      <div class="body md-body" bind:this={bodyEl} on:click={onBodyClick}>
        {#if error}<p class="error">{error}</p>{/if}
        {@html html}
      </div>
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
    inset: 4vh 6vw;
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
  .error { color: var(--danger, #e5484d); }

  .split { display: flex; flex: 1; min-height: 0; }
  nav {
    flex: 0 0 190px;
    border-right: 1px solid var(--border);
    padding: 10px 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
    overflow-y: auto;
  }
  .tab {
    background: transparent;
    border: none;
    border-left: 2px solid transparent;
    color: var(--fg-muted);
    font-size: 0.8rem;
    text-align: left;
    padding: 7px 14px;
    cursor: pointer;
  }
  .tab:hover { color: var(--fg); background: var(--bg-sunken); }
  .tab.active { color: var(--fg); border-left-color: var(--accent, #4f8ff7); background: var(--bg-sunken); }

  .body {
    flex: 1;
    min-width: 0;
    overflow-y: auto;
    padding: 18px 26px 60px;
  }

  /* Rendered markdown — scoped under .md-body via :global since the
     HTML arrives through {@html}. */
  .md-body { color: var(--fg); font-size: 0.84rem; line-height: 1.6; }
  .md-body :global(h1) { font-size: 1.35rem; margin: 0 0 14px; font-weight: 600; }
  .md-body :global(h2) { font-size: 1.05rem; margin: 26px 0 10px; font-weight: 600; border-bottom: 1px solid var(--border); padding-bottom: 5px; }
  .md-body :global(h3) { font-size: 0.92rem; margin: 20px 0 8px; font-weight: 600; }
  .md-body :global(h4) { font-size: 0.84rem; margin: 16px 0 6px; font-weight: 600; }
  .md-body :global(p) { margin: 0 0 10px; max-width: 78ch; }
  .md-body :global(a) { color: var(--accent, #4f8ff7); text-decoration: none; }
  .md-body :global(a:hover) { text-decoration: underline; }
  .md-body :global(code) {
    font-family: var(--font-mono);
    font-size: 0.78rem;
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: 3px;
    padding: 0 4px;
  }
  .md-body :global(pre) {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 10px 12px;
    overflow-x: auto;
    margin: 0 0 12px;
  }
  .md-body :global(pre code) { background: transparent; border: none; padding: 0; font-size: 0.76rem; }
  .md-body :global(ul), .md-body :global(ol) { margin: 0 0 10px; padding-left: 22px; max-width: 78ch; }
  .md-body :global(li) { margin-bottom: 3px; }
  .md-body :global(blockquote) {
    border-left: 3px solid var(--border);
    margin: 0 0 10px;
    padding: 2px 12px;
    color: var(--fg-muted);
  }
  .md-body :global(table) { border-collapse: collapse; margin: 0 0 12px; font-size: 0.78rem; }
  .md-body :global(th), .md-body :global(td) {
    border: 1px solid var(--border);
    padding: 4px 9px;
    text-align: left;
  }
  .md-body :global(th) { background: var(--bg-sunken); }
  .md-body :global(hr) { border: none; border-top: 1px solid var(--border); margin: 18px 0; }
</style>
