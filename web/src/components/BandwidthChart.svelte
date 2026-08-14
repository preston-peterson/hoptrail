<script>
  // Bandwidth chart card (step-102; design §6.2). Two lines —
  // download + upload — over the bandwidth test history, sharing the
  // latency chart's time anchor so scrolling back correlates
  // visually. Dashed reference lines mark derate_threshold × baseline
  // per direction; failed tests render as gaps.
  //
  // Visibility: hidden entirely when monitoring is disabled AND no
  // historical data exists (a v0.2-style deploy sees nothing). With
  // data-but-disabled it renders (operator can re-enable and pick up
  // where they left off); enabled-but-empty renders the hint state.
  import { onMount, onDestroy } from 'svelte'
  import uPlot from 'uplot'
  import 'uplot/dist/uPlot.min.css'
  import { bandwidthSamples, bandwidthWindow, bandwidthChartWindow, setBandwidthChartWindow, BW_WINDOWS, derateStatus, bandwidthConfig, openSettings, bandwidthSectionVisible } from '../lib/stores.js'

  let chartEl
  let plot = null
  let resizeObserver
  let tooltip = null

  // Read the window imperatively inside uPlot's range callback.
  let currentWindow = null
  $: currentWindow = $bandwidthWindow
  function windowNow() { return currentWindow }
  // Re-range when the window moves (poll tick / anchor change).
  $: if (plot && currentWindow) plot.redraw()

  $: cfg = $bandwidthConfig
  // Step-126: visibility moved to a shared derived store so the
  // App-level section stack and this card always agree.
  $: visible = $bandwidthSectionVisible
  $: hasData = $bandwidthSamples.some((s) => s.ok)

  // Latest + baseline summary line (design mockup).
  $: latest = $derateStatus?.last_test
  $: baseline = $derateStatus?.baseline

  function cssVar(name, fallback) {
    const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
    return v || fallback
  }

  function buildData(samples) {
    const ok = samples.filter((s) => s.ok)
    return [
      ok.map((s) => s.ts / 1000),
      ok.map((s) => s.down_mbps),
      ok.map((s) => s.up_mbps),
    ]
  }

  function makePlot() {
    if (!chartEl) return
    const downColor = cssVar('--accent', '#4ea1ff')
    const upColor = cssVar('--warn', '#e8a33d')

    const opts = {
      width: chartEl.clientWidth,
      height: 150,
      padding: [8, 8, 0, 0],
      legend: { show: false },
      cursor: { y: false },
      scales: {
        x: {
          time: true,
          // Pin the axis to the polled window: with one sample,
          // uPlot's auto-range padded the axis into a multi-year
          // span and the point vanished (step-105, operator shot).
          range: () => {
            const w = windowNow()
            return w ? [w.since / 1000, w.until / 1000] : [null, null]
          },
        },
      },
      axes: [
        { stroke: cssVar('--fg-subtle', '#888'), grid: { stroke: cssVar('--border', '#333'), width: 1 } },
        {
          stroke: cssVar('--fg-subtle', '#888'),
          grid: { stroke: cssVar('--border', '#333'), width: 1 },
          // 72px fits four-digit gigabit ticks ("1150 Mb") — at 56
          // the canvas clipped the leading digit and the axis read
          // "150 Mb" on a 1.15 Gbps link (step-106, operator shot).
          size: 72,
          values: (u, ticks) => ticks.map((v) => v + ' Mb'),
        },
      ],
      series: [
        {},
        { label: 'down', stroke: downColor, width: 1.5, points: { show: true, size: 5 } },
        { label: 'up', stroke: upColor, width: 1.5, points: { show: true, size: 5 } },
      ],
      hooks: {
        // Hover tooltip (step-106, operator request): nearest sample's
        // time + both values + server. Positioned ABOVE the cursor so
        // the pointer never occludes it.
        setCursor: [
          (u) => {
            const i = u.cursor.idx
            if (i == null || u.data[0][i] == null) {
              tooltip = null
              return
            }
            const okSamples = $bandwidthSamples.filter((x) => x.ok)
            const smp = okSamples[i]
            tooltip = {
              left: u.cursor.left + u.bbox.left / devicePixelRatio,
              top: u.cursor.top,
              ts: u.data[0][i] * 1000,
              down: u.data[1][i],
              up: u.data[2][i],
              server: smp?.server_name ?? null,
            }
          },
        ],
        // Dashed derate-threshold reference lines (threshold ×
        // baseline, per direction) — drawn after series so they stay
        // visible at boundary values.
        draw: [
          (u) => {
            if (!baseline || !cfg) return
            const ctx = u.ctx
            const refs = [
              [baseline.down_mbps * cfg.derate_threshold, downColor],
              [baseline.up_mbps * cfg.derate_threshold, upColor],
            ]
            ctx.save()
            ctx.setLineDash([4, 4])
            ctx.lineWidth = 1
            for (const [val, color] of refs) {
              const y = u.valToPos(val, 'y', true)
              if (y < u.bbox.top || y > u.bbox.top + u.bbox.height) continue
              ctx.strokeStyle = color
              ctx.globalAlpha = 0.5
              ctx.beginPath()
              ctx.moveTo(u.bbox.left, y)
              ctx.lineTo(u.bbox.left + u.bbox.width, y)
              ctx.stroke()
            }
            ctx.restore()
          },
        ],
      },
    }
    plot = new uPlot(opts, buildData($bandwidthSamples), chartEl)
  }

  $: if (plot && $bandwidthSamples) {
    plot.setData(buildData($bandwidthSamples))
  }

  onMount(() => {
    resizeObserver = new ResizeObserver(() => {
      if (plot && chartEl) plot.setSize({ width: chartEl.clientWidth, height: 150 })
    })
  })

  // (Re)create the plot when the card becomes visible with data.
  $: if (visible && hasData && chartEl && !plot) {
    makePlot()
    resizeObserver?.observe(chartEl)
  }
  // Tear the plot down when the data goes away (step-148): the
  // {#if hasData} block unmounts the canvas, and a stale non-null
  // `plot` handle blocked the rebuild above forever — one transit
  // through an empty window killed the chart until a page reload.
  $: if (!hasData && plot) {
    plot.destroy()
    plot = null
  }

  onDestroy(() => {
    resizeObserver?.disconnect()
    plot?.destroy()
    plot = null
  })

  function fmt(mbps) {
    return mbps >= 100 ? Math.round(mbps) : mbps.toFixed(1)
  }
</script>

{#if visible}
  <section class="card" id="bandwidth-card">
    <header>
      <h2>Bandwidth</h2>
      <div class="legend">
        <span class="swatch down"></span> download
        <span class="swatch up"></span> upload
        <select
          class="range"
          title="bandwidth chart range — 'view' follows the latency chart's window"
          value={$bandwidthChartWindow}
          on:change={(e) => setBandwidthChartWindow(e.target.value)}
        >
          {#each Object.keys(BW_WINDOWS) as k (k)}
            <option value={k}>{k}</option>
          {/each}
        </select>
      </div>
    </header>

    {#if hasData}
      <div class="chart-wrap">
        <div class="chart" role="presentation" bind:this={chartEl} on:mouseleave={() => (tooltip = null)}></div>
        {#if tooltip}
          <div class="tooltip" style="left: {tooltip.left}px; top: {tooltip.top}px;">
            <div class="tt-time">{new Date(tooltip.ts).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}</div>
            <div><span class="swatch down"></span> {fmt(tooltip.down)} Mbps down</div>
            <div><span class="swatch up"></span> {fmt(tooltip.up)} Mbps up</div>
            {#if tooltip.server}<div class="tt-server">{tooltip.server}</div>{/if}
          </div>
        {/if}
      </div>
      <div class="summary">
        <span class="attribution">tests via <a href="https://www.speedtest.net" target="_blank" rel="noopener">Ookla® Speedtest®</a></span>
        {#if latest?.ok}
          <span>current: <strong>{fmt(latest.down_mbps)} ↓ / {fmt(latest.up_mbps)} ↑</strong> Mbps</span>
        {/if}
        {#if baseline}
          <span class="muted">baseline ({cfg.baseline_days}d {cfg.baseline_metric}): {fmt(baseline.down_mbps)} ↓ / {fmt(baseline.up_mbps)} ↑ Mbps</span>
        {:else if cfg.enabled}
          <span class="muted">building baseline — derate detection starts after 7 successful tests</span>
        {/if}
      </div>
    {:else}
      <div class="empty muted">
        {#if $derateStatus?.last_test}
          no tests between
          {$bandwidthWindow ? new Date($bandwidthWindow.since).toLocaleString([], { dateStyle: 'short', timeStyle: 'short' }) : '…'}
          and
          {$bandwidthWindow ? new Date($bandwidthWindow.until).toLocaleString([], { dateStyle: 'short', timeStyle: 'short' }) : '…'}
          ({$bandwidthChartWindow}) — pick a wider range above
        {:else if cfg.enabled}
          no tests yet — the first scheduled run is coming, or
          <button class="linkish" on:click={() => openSettings('bandwidth')}>run one now</button>
        {:else}
          bandwidth monitoring is off —
          <button class="linkish" on:click={() => openSettings('bandwidth')}>enable it in settings</button>
        {/if}
      </div>
    {/if}
  </section>
{/if}

<style>
  .card {
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: var(--radius-md, 8px);
    padding: 10px 12px;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }
  h2 { margin: 0; font-size: 0.9rem; font-weight: 600; }

  .legend {
    display: flex;
    align-items: center;
    gap: 6px;
    font-family: var(--font-mono);
    font-size: 0.7rem;
    color: var(--fg-muted);
  }
  .swatch { width: 14px; height: 3px; border-radius: 2px; display: inline-block; }
  .swatch.down { background: var(--accent); }
  .swatch.up { background: var(--warn, #e8a33d); margin-left: 10px; }

  .chart-wrap { position: relative; }
  .chart { width: 100%; }
  .tooltip {
    position: absolute;
    transform: translate(-50%, -100%) translateY(-10px);
    background: var(--bg-elevated);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm);
    box-shadow: 0 4px 16px rgba(0, 0, 0, 0.4);
    padding: 6px 9px;
    font-family: var(--font-mono);
    font-size: 0.7rem;
    pointer-events: none;
    white-space: nowrap;
    z-index: 10;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .tt-time { color: var(--fg-muted); }
  .tt-server { color: var(--fg-subtle); }
  .range {
    margin-left: 12px;
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-family: var(--font-mono);
    font-size: 0.7rem;
    padding: 2px 4px;
  }

  .attribution { color: var(--fg-subtle); }
  .attribution a { color: var(--fg-subtle); text-decoration: underline; }
  .attribution a:hover { color: var(--fg-muted); }
  .summary {
    display: flex;
    gap: 16px;
    flex-wrap: wrap;
    font-family: var(--font-mono);
    font-size: 0.72rem;
  }
  .muted { color: var(--fg-muted); }

  .empty {
    padding: 18px 0 12px;
    font-size: 0.8rem;
    text-align: center;
  }
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
