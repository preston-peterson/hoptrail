<script>
  // The two bandwidth banners (step-102; design §6.1 + §4.2), one
  // component since they're mutually exclusive in practice and share
  // the dismissal plumbing.
  //
  // Derate banner — the flagship: shows while the latest test is
  // derate-flagged, unless the operator dismissed THIS incident
  // (dismissals are per-incident-start-ts, stored server-side, and
  // the backend clears them automatically on resolution so the next
  // incident's banner always reappears).
  //
  // Install banner — upgrade-discovery: bandwidth exists but isn't
  // enabled and the operator hasn't dismissed the prompt for this
  // hoptrail version (a newer version re-prompts once).
  import { derateStatus, bandwidthConfig, versionStore, openSettings, refreshBandwidthConfig } from '../lib/stores.js'
  import { patchBandwidthConfig } from '../lib/api.js'

  $: status = $derateStatus
  $: cfg = $bandwidthConfig

  $: derateVisible =
    status?.derated &&
    status?.since != null &&
    status?.dismissed_ts !== status?.since

  $: installVisible =
    !derateVisible &&
    cfg != null &&
    !cfg.enabled &&
    $versionStore != null &&
    cfg.install_banner_dismissed_for_version !== $versionStore

  function heldFor(sinceMs) {
    const mins = Math.max(0, Math.round((Date.now() - sinceMs) / 60_000))
    if (mins < 60) return `${mins}m`
    const h = Math.floor(mins / 60)
    return `${h}h ${mins % 60}m`
  }
  function at(ts) {
    return new Date(ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  }
  function fmt(mbps) {
    return mbps >= 100 ? Math.round(mbps) : mbps.toFixed(1)
  }

  // Which direction(s) actually tripped — the message names the
  // culprit instead of a generic "throughput derated".
  $: culprit = (() => {
    if (!status?.last_test || !status?.baseline || !cfg) return null
    const t = status.last_test
    const b = status.baseline
    const downBad = t.down_mbps < b.down_mbps * cfg.derate_threshold
    const upBad = t.up_mbps < b.up_mbps * cfg.derate_threshold
    if (upBad && !downBad) return { dir: 'Upload', now: t.up_mbps, base: b.up_mbps }
    if (downBad && !upBad) return { dir: 'Download', now: t.down_mbps, base: b.down_mbps }
    return { dir: 'Throughput', now: t.down_mbps, base: b.down_mbps }
  })()

  async function dismissDerate() {
    try {
      await patchBandwidthConfig({ derate_banner_dismissed_incident_ts: status.since })
      await refreshBandwidthConfig()
      // The 60s derate poll will pick up dismissed_ts; reflect it now.
      derateStatus.update((s) => (s ? { ...s, dismissed_ts: status.since } : s))
    } catch { /* next poll reconciles */ }
  }

  async function dismissInstall() {
    try {
      await patchBandwidthConfig({ install_banner_dismissed_for_version: $versionStore })
      await refreshBandwidthConfig()
    } catch { /* next open reconciles */ }
  }

  function toChart() {
    document.getElementById('bandwidth-card')?.scrollIntoView({ behavior: 'smooth', block: 'center' })
  }
</script>

{#if derateVisible && culprit}
  <div class="banner derate" role="alert">
    <span class="icon" aria-hidden="true">⚠</span>
    <span class="text">
      <strong>{culprit.dir} throughput derated: {fmt(culprit.now)} Mbps</strong>
      <span class="detail">(baseline {fmt(culprit.base)} Mbps) · detected {at(status.since)}, held {heldFor(status.since)}</span>
    </span>
    <span class="actions">
      <button on:click={toChart}>chart</button>
      <button on:click={dismissDerate}>dismiss</button>
    </span>
  </div>
{:else if installVisible}
  <div class="banner install">
    <span class="text">
      <strong>New: bandwidth monitoring.</strong>
      <span class="detail">
        Scheduled speed tests with automatic derate alerts.
        {#if cfg.capability.available}
          <button class="linkish" on:click={() => openSettings('bandwidth')}>Enable in settings</button>
        {:else}
          <button class="linkish" on:click={() => openSettings('bandwidth')}>Install it from settings</button>
        {/if}
      </span>
    </span>
    <span class="actions">
      <button on:click={dismissInstall}>dismiss</button>
    </span>
  </div>
{/if}

<style>
  .banner {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 7px 14px;
    font-size: 0.82rem;
    border-bottom: 1px solid var(--border);
  }
  .banner.derate {
    background: color-mix(in srgb, var(--danger, #e5484d) 12%, var(--bg-elevated));
    color: var(--fg);
  }
  .banner.install {
    background: color-mix(in srgb, var(--accent) 10%, var(--bg-elevated));
  }
  .icon { color: var(--danger, #e5484d); }
  .text { flex: 1; min-width: 0; }
  .detail { color: var(--fg-muted); margin-left: 6px; }
  code {
    font-family: var(--font-mono);
    font-size: 0.75rem;
    background: var(--bg-sunken);
    padding: 1px 5px;
    border-radius: var(--radius-sm);
    user-select: all;
  }
  .actions { display: flex; gap: 6px; }
  .actions button {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    font-size: 0.72rem;
    padding: 3px 8px;
    cursor: pointer;
  }
  .actions button:hover { color: var(--fg); border-color: var(--border-strong); }
  .linkish {
    background: transparent;
    border: none;
    color: var(--accent);
    cursor: pointer;
    font-size: inherit;
    padding: 0;
    text-decoration: underline;
  }
</style>
