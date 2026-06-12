// Outage-band classifier — extracted from LatencyTimeline in step-38
// so the main chart and the per-hop sparklines render the same red/
// orange/yellow bands. Both surfaces apply the same lesson #3
// attribution rule: a hop showing loss while every downstream hop is
// healthy is rate-limiting its own ICMP, not actually dropping
// traffic, and gets no band.
//
// Inputs (all live data, same shapes the components already consume):
//   samples — array of {ttl, ts, ip, rtt_ms}; ip === null is the wire
//             signal for a timeout, rtt_ms is 0 for those rows.
//   selectedTTL — the hop whose bands we want.
//   pathHops — current path snapshot's hops array; only `ttl` is read.
//
// Returns: array of {start, end, color} where color ∈ {red,orange,yellow}.
// Consecutive same-color classifications collapse into one band, so
// the caller can iterate and paint per band rather than per timestamp.

export const WINDOW_SIZE = 5 // samples per classification window
export const RATE_LIMIT_RATIO = 0.5 // downstream loss < sel * this → rate-limit

// Same hex values as LatencyTimeline's old colorMap. Translucent so
// the bands sit behind data without dominating. Exported so the
// sparkline plugin and the uPlot plugin both render identically.
export const BAND_COLORS = {
  red:    'rgba(239, 68, 68, 0.18)',
  orange: 'rgba(249, 115, 22, 0.18)',
  yellow: 'rgba(234, 179, 8, 0.14)',
}

export function computeBands(samples, selectedTTL, pathHops) {
  if (!samples?.length || selectedTTL == null) return []

  // Downstream TTLs relative to the selected hop. For the target hop
  // (last hop) this set is empty — no downstream attribution to apply,
  // so the band reflects the selected hop's loss directly (the target's
  // reachability IS the system's reachability).
  const downstreamTTLs = (pathHops ?? [])
    .map((h) => h.ttl)
    .filter((t) => t > selectedTTL)

  // Index samples by timestamp → ttl → sample for O(1) downstream
  // lookups inside the window scan.
  const byTs = new Map()
  for (const s of samples) {
    let inner = byTs.get(s.ts)
    if (!inner) {
      inner = new Map()
      byTs.set(s.ts, inner)
    }
    inner.set(s.ttl, s)
  }
  const tsList = [...byTs.keys()].sort((a, b) => a - b)

  const result = []
  let currentBand = null

  for (let i = 0; i < tsList.length; i++) {
    const windowStart = Math.max(0, i - WINDOW_SIZE + 1)

    let selTimeouts = 0
    let selTotal = 0
    let dsTimeouts = 0
    let dsTotal = 0

    for (let j = windowStart; j <= i; j++) {
      const m = byTs.get(tsList[j])
      const sel = m.get(selectedTTL)
      if (sel) {
        selTotal++
        if (sel.ip === null) selTimeouts++
      }
      for (const dt of downstreamTTLs) {
        const ds = m.get(dt)
        if (ds) {
          dsTotal++
          if (ds.ip === null) dsTimeouts++
        }
      }
    }

    if (selTotal === 0) {
      if (currentBand) { result.push(currentBand); currentBand = null }
      continue
    }

    const selLoss = selTimeouts / selTotal
    const dsLoss = dsTotal > 0 ? dsTimeouts / dsTotal : selLoss

    const effectiveLoss = downstreamTTLs.length > 0 && dsLoss < selLoss * RATE_LIMIT_RATIO
      ? 0
      : selLoss

    let color = null
    if (effectiveLoss >= 0.7) color = 'red'
    else if (effectiveLoss >= 0.4) color = 'orange'
    else if (effectiveLoss >= 0.2) color = 'yellow'

    const ts = tsList[i]
    if (color) {
      if (currentBand && currentBand.color === color) {
        currentBand.end = ts
      } else {
        if (currentBand) result.push(currentBand)
        currentBand = { start: ts, end: ts, color }
      }
    } else if (currentBand) {
      result.push(currentBand)
      currentBand = null
    }
  }
  if (currentBand) result.push(currentBand)
  return result
}
