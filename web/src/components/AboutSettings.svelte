<script>
  // About section (step-139, operator request — mirrors the other
  // project's About page: blurb + repo link, license, built-with
  // attribution grid). §9 note: the maintainer's given name lives in
  // LICENSE only — this section points there rather than repeating it.
  import { versionStore } from '../lib/stores.js'

  const DEPS = [
    { name: 'Go + golang.org/x/net/icmp', role: 'probe engine' },
    { name: 'SQLite (mattn/go-sqlite3)', role: 'storage' },
    { name: 'Svelte', role: 'web UI' },
    { name: 'uPlot', role: 'charts' },
    { name: 'ntfy', role: 'push notifications' },
    { name: 'Ookla® Speedtest® CLI', role: 'speed tests (operator-installed)' },
  ]
</script>

<div class="about">
  <p class="blurb">
    <strong>hoptrail {$versionStore ?? ''}</strong> — continuous
    traceroute and per-hop latency tracker for Linux. It traces the
    path to your targets around the clock, keeps per-hop latency and
    loss history, and answers "when did this start" and "which leg is
    degrading."
  </p>
  <p class="line">
    Source, issues, and releases:
    <a href="https://github.com/preston-peterson/hoptrail" target="_blank" rel="noopener noreferrer">
      github.com/preston-peterson/hoptrail
    </a>
  </p>

  <h4>License</h4>
  <p class="line">
    MIT — use it, fork it, modify it. See the <code>LICENSE</code> file
    in the source distribution for full terms and copyright.
  </p>

  <h4>Built with</h4>
  <div class="deps">
    {#each DEPS as d (d.name)}
      <div class="dep">
        <span class="name">{d.name}</span>
        <span class="role">{d.role}</span>
      </div>
    {/each}
  </div>
  <p class="fine">
    ntfy by Philipp C. Heckel. Speed tests run under
    <a href="https://www.speedtest.net/about/eula" target="_blank" rel="noopener noreferrer">Ookla's license terms</a>;
    hoptrail does not redistribute the CLI. No telemetry, no cloud
    account, no tracking — your data stays on your hardware.
  </p>
</div>

<style>
  .about { display: flex; flex-direction: column; gap: 8px; }
  .blurb { margin: 0; font-size: 0.8rem; line-height: 1.5; color: var(--fg); }
  .line { margin: 0; font-size: 0.75rem; color: var(--fg-muted); line-height: 1.5; }
  .line a, .fine a { color: var(--accent, #4f8ff7); }
  h4 {
    margin: 6px 0 0;
    font-size: 0.7rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--fg-subtle);
  }
  .deps { display: grid; grid-template-columns: 1fr; gap: 3px; }
  .dep {
    display: flex;
    justify-content: space-between;
    gap: 8px;
    font-size: 0.73rem;
    padding: 3px 6px;
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
  }
  .dep .name { color: var(--fg); }
  .dep .role { color: var(--fg-subtle); white-space: nowrap; }
  .fine { margin: 4px 0 0; font-size: 0.68rem; color: var(--fg-subtle); line-height: 1.5; }
</style>
