// Client-side focused-window hop stats (step-43 / task #12).
//
// The hop-table's avg/min/cur/loss numbers can be re-computed against
// a specific historical window the operator picks, while the chart
// keeps its own view window. Decouples "what stats am I reading"
// from "what am I looking at on the chart" — critical for
// post-incident investigation ("what did hop 9 look like during the
// 6:23am spike?").
//
// Hoptrail computes this client-side: samplesStore already has all
// the raw samples for the active chart window, so we don't need a
// new API. When focusArea is set, derive per-hop stats from samples
// in [since, until] and return objects shaped identically to the
// server's path-snapshot hop JSON. HopCard doesn't need to know
// which source it's reading.
//
// Trade-off: the focus window is bounded by the chart's loaded
// samples. If the operator focuses on a moment outside what
// samplesStore contains (e.g. focus inside a 5m chart but pick a
// time 30m ago), the stats will be empty. The fix is to widen the
// chart's view window first — which is exactly what the operator
// would naturally do.

// attributeLoss is the JS port of internal/analysis.AttributedLoss.
// Same rule: a hop is "suspect" only if its loss also appears at
// every downstream hop. Otherwise the loss is rate-limiting at this
// hop and traffic is flowing fine through it — classify as
// "rate_limited". Returns the same enum strings the server emits
// for loss_state so HopCard's existing CSS classes Just Work.
//
// hopsWithLoss must be ordered by ttl ascending. tolerance lets
// downstream loss vary by a small ratio without breaking the
// suspect chain (matches the server's DefaultLossTolerance).
const LOSS_TOLERANCE = 0.05 // 5pp — matches analysis.DefaultLossTolerance

export function attributeLoss(hopsWithLoss) {
  return hopsWithLoss.map((h, i) => {
    if (h.loss === 0) return { ttl: h.ttl, loss: 0, state: 'ok' }
    // Suspect only if every downstream hop loses at least (this hop's
    // loss - tolerance). If any downstream is significantly healthier,
    // this hop is just rate-limiting its own ICMP.
    let suspect = true
    for (let j = i + 1; j < hopsWithLoss.length; j++) {
      if (hopsWithLoss[j].loss < h.loss - LOSS_TOLERANCE * 100) {
        suspect = false
        break
      }
    }
    return { ttl: h.ttl, loss: h.loss, state: suspect ? 'suspect' : 'rate_limited' }
  })
}

// computeFocusedHops returns per-hop summary objects derived from
// samples in [since, until]. Shape matches the server's path-snapshot
// hop JSON (current_rtt_ms / avg_rtt_ms / min_rtt_ms / loss_percent
// / loss_state / current_ip / hostname / ttl) so HopCard can swap
// between server-live data and focused data transparently.
//
// pathHops carries the per-hop identity (ip, hostname) the server
// has already resolved — we don't try to re-derive those from the
// raw samples (which only know the responding IP, not its rDNS).
//
// Empty samples for a hop's TTL in the window → all stats are 0 /
// loss = 100%. The HopCard already renders "0ms" / "—" sensibly
// for those edge cases.
export function computeFocusedHops(samples, pathHops, since, until) {
  // Bucket samples by TTL once for O(n) cost instead of per-hop scan.
  const byTtl = new Map()
  for (const s of samples) {
    if (s.ts < since || s.ts > until) continue
    let bucket = byTtl.get(s.ttl)
    if (!bucket) {
      bucket = []
      byTtl.set(s.ttl, bucket)
    }
    bucket.push(s)
  }

  // First pass: compute raw loss for every hop. Need that for the
  // attribution pass (which looks at downstream losses to classify).
  const withLoss = pathHops.map((h) => {
    const bucket = byTtl.get(h.ttl) ?? []
    let total = 0
    let timeouts = 0
    for (const s of bucket) {
      total++
      if (s.ip === null) timeouts++
    }
    const loss = total === 0 ? 0 : (timeouts / total) * 100
    return { ttl: h.ttl, loss }
  })
  const attributed = attributeLoss(withLoss)
  const stateByTtl = new Map(attributed.map((a) => [a.ttl, a.state]))

  // Second pass: assemble the focused hop objects.
  return pathHops.map((h) => {
    const bucket = byTtl.get(h.ttl) ?? []
    // Filter to non-timeout samples for rtt aggregations.
    let total = 0
    let timeouts = 0
    let minMs = Infinity
    let sumMs = 0
    let lastMs = 0
    let lastTs = 0
    for (const s of bucket) {
      total++
      if (s.ip === null) {
        timeouts++
        continue
      }
      if (s.rtt_ms < minMs) minMs = s.rtt_ms
      sumMs += s.rtt_ms
      if (s.ts > lastTs) {
        lastTs = s.ts
        lastMs = s.rtt_ms
      }
    }
    const responded = total - timeouts
    const loss = total === 0 ? 0 : (timeouts / total) * 100
    const lossState = stateByTtl.get(h.ttl) ?? 'ok'
    return {
      ttl: h.ttl,
      // Carry the server-side identity fields through unchanged —
      // they don't depend on the time window.
      current_ip: h.current_ip,
      hostname: h.hostname,
      current_rtt_ms: lastMs,
      avg_rtt_ms: responded === 0 ? 0 : sumMs / responded,
      min_rtt_ms: minMs === Infinity ? 0 : minMs,
      loss_percent: loss,
      loss_state: lossState,
      last_response: lastTs || null,
    }
  })
}
