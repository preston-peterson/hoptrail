<script>
  // Per-hop sparkline — small inline SVG showing recent RTT trend.
  //
  // Step-38: paints the same red/orange/yellow outage bands as the
  // main chart, applying the same attribution rule. The classifier
  // (lib/bands.js) needs the full samples stream + path snapshot so
  // it can read downstream loss when deciding whether this hop's
  // loss is real (band) or rate-limiting (no band).
  //
  // Design choices:
  //   - Per-hop Y auto-scaling: each sparkline scales to its own
  //     min/max RTT. Matters because RTT varies wildly across hops
  //     (sub-ms at the local gateway, 10-100ms at internet transit);
  //     a single global scale would crush most hops to flat lines.
  //   - Timeouts render as gaps, not zeros — same convention as the
  //     main chart (zero RTT is the wire signal for "no response").
  //   - One <path> with M/L commands; M re-opens a new segment after
  //     each timeout, which gives gap rendering for free.
  //   - Bands paint as <rect> elements behind the path so the line
  //     stays visible inside an outage period (you can still see the
  //     intermittent successful responses).

  import { computeBands, BAND_COLORS } from '../lib/bands.js'

  /** Full samples stream across all hops — needed so the band
      classifier can apply downstream attribution. */
  /** @type {{ ttl: number, ts: number, ip: string | null, rtt_ms: number }[]} */
  export let allSamples = []

  /** This sparkline's hop TTL. */
  export let ttl

  /** Current path snapshot hop list — only `ttl` is read; lets the
      classifier identify downstream TTLs for this hop. */
  /** @type {{ ttl: number }[]} */
  export let pathHops = []

  /** Stroke color — usually the hop's own color so it matches the
      dot, selection ring, and main chart line for the same hop. */
  export let color = 'var(--fg-subtle)'

  // Sparkline dimensions. 180px wide is enough horizontal resolution
  // to discern individual outliers across the 130-sample 5-minute
  // window; 30px tall gives the trace room to swing between min/max
  // for hops with small variance without forcing the row much taller.
  const W = 180
  const H = 30

  // Filter the global samples stream to this hop's TTL for the path
  // building. The band classifier still gets the full stream so it
  // can see downstream loss.
  $: ttlSamples = allSamples.filter((s) => s.ttl === ttl)
  $: path = buildPath(ttlSamples, W, H)
  $: bands = computeBands(allSamples, ttl, pathHops)
  $: bandRects = projectBands(bands, ttlSamples, W)

  function buildPath(s, w, h) {
    if (!s || s.length === 0) return ''

    // Valid = non-timeout. ip is null on timeout; rtt_ms is also 0.
    // Use either check; ip is the canonical wire signal per api-v0.1.md.
    const valid = s.filter(d => d.ip !== null && d.rtt_ms > 0)
    if (valid.length === 0) return ''

    // X spread uses the full sample window so gaps from timeouts
    // visually compress the line on either side of an outage,
    // which is the right cue ("nothing happened here for a while").
    const tsMin = s[0].ts
    const tsMax = s[s.length - 1].ts
    const tsRange = tsMax - tsMin || 1

    // Y spread uses valid samples only — timeouts have rtt_ms 0
    // which would otherwise crush everything against the top.
    let rttMin = Infinity, rttMax = -Infinity
    for (const d of valid) {
      if (d.rtt_ms < rttMin) rttMin = d.rtt_ms
      if (d.rtt_ms > rttMax) rttMax = d.rtt_ms
    }
    const rttRange = rttMax - rttMin || 1

    // Small inset so the stroke isn't clipped at the edges.
    const pad = 1.5
    const innerW = w - 2 * pad
    const innerH = h - 2 * pad

    let d = ''
    let pen = 'M'
    for (const sample of s) {
      if (sample.ip !== null && sample.rtt_ms > 0) {
        const x = pad + ((sample.ts - tsMin) / tsRange) * innerW
        const y = pad + innerH - ((sample.rtt_ms - rttMin) / rttRange) * innerH
        d += pen + x.toFixed(1) + ',' + y.toFixed(1) + ' '
        pen = 'L'
      } else {
        // Lift the pen — next valid sample starts a fresh segment.
        pen = 'M'
      }
    }
    return d.trim()
  }

  // Project the {start,end} timestamps from computeBands onto this
  // sparkline's X axis. The sparkline's X domain is the timestamps in
  // its own ttlSamples (same as buildPath uses), so the bands line up
  // with the trace even when this hop is missing samples that other
  // hops have.
  function projectBands(bs, s, w) {
    if (!bs.length || !s.length) return []
    const tsMin = s[0].ts
    const tsMax = s[s.length - 1].ts
    const tsRange = tsMax - tsMin || 1
    const pad = 1.5
    const innerW = w - 2 * pad
    const out = []
    for (const b of bs) {
      // Clip to the visible window — bands can extend past it if the
      // selected hop has samples outside this sparkline's range.
      const start = Math.max(b.start, tsMin)
      const end = Math.min(b.end, tsMax)
      if (end < tsMin || start > tsMax) continue
      const x1 = pad + ((start - tsMin) / tsRange) * innerW
      const x2 = pad + ((end - tsMin) / tsRange) * innerW
      out.push({
        x: x1,
        width: Math.max(1.5, x2 - x1),
        color: BAND_COLORS[b.color],
      })
    }
    return out
  }
</script>

<svg
  viewBox="0 0 {W} {H}"
  width={W}
  height={H}
  class="sparkline"
  aria-hidden="true"
  preserveAspectRatio="none"
>
  {#each bandRects as r}
    <rect x={r.x} y="0" width={r.width} height={H} fill={r.color} />
  {/each}
  {#if path}
    <path
      d={path}
      fill="none"
      stroke={color}
      stroke-width="1"
      stroke-linecap="round"
      stroke-linejoin="round"
    />
  {/if}
</svg>

<style>
  .sparkline {
    display: block;
  }
</style>
