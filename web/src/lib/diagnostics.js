// Path-level diagnostic detectors (step-40 / task #9). Encode the
// common diagnostic patterns operators look for in a continuous
// traceroute as pattern detectors that scan the current path snapshot
// and emit short, actionable advisories. Pure functions of (path
// hops): no I/O, no state, easy to unit-test.
//
// Each detector returns either null (pattern not present) or an
// advisory object: { id, severity, title, detail }.
//   - id: stable string for keyed-each and dismissal tracking.
//   - severity: 'info' | 'warn' — drives the banner's accent color.
//   - title: one-line summary the operator scans at a glance.
//   - detail: a sentence or two of action-oriented guidance.
//
// `analyzePath` runs every detector and returns the array of fired
// advisories (in priority order — local-hardware before border-cross
// before tail-noise so the most actionable one is first).

// Thresholds tuned conservatively: false positives are worse than
// missed detections because every false advisory burns operator
// trust. If a pattern looks ambiguous, the detector stays silent.

// All-hops-uniform-loss: when every responsive hop is dropping a
// similar percentage of packets and the loss is non-trivial, the
// shared upstream component is the most likely cause — typically
// the operator's own router/modem/NIC/cable. Wireless near the
// range limit produces a similar signature.
const MIN_HOPS_FOR_UNIFORM = 4 // need enough rows for the pattern to mean anything
const MIN_UNIFORM_LOSS_PCT = 5 // below this we don't bother — could be noise
const UNIFORM_LOSS_SPREAD = 5 // max delta between min/max hop loss% to count as "uniform"

export function detectUniformLoss(hops) {
  if (!hops || hops.length < MIN_HOPS_FOR_UNIFORM) return null

  // Only consider hops that responded enough to have meaningful loss
  // — anonymous (no IP) hops have undefined loss_percent and skew the
  // calculation. Rate-limited hops are excluded too (their loss isn't
  // real per the attribution rule, so they shouldn't drag the "all
  // hops match" signal).
  const real = hops.filter(
    (h) => h.current_ip && h.loss_state !== 'rate_limited' && typeof h.loss_percent === 'number',
  )
  if (real.length < MIN_HOPS_FOR_UNIFORM) return null

  const losses = real.map((h) => h.loss_percent)
  const min = Math.min(...losses)
  const max = Math.max(...losses)
  const avg = losses.reduce((a, b) => a + b, 0) / losses.length

  if (avg < MIN_UNIFORM_LOSS_PCT) return null
  if (max - min > UNIFORM_LOSS_SPREAD) return null

  return {
    id: 'uniform-loss',
    severity: 'warn',
    title: `~${Math.round(avg)}% packet loss across the entire path`,
    detail:
      'Identical loss at every hop usually means the issue is local — your router, modem, NIC, or cable, ' +
      'or a wireless link near its range limit. Try cycling that gear before suspecting upstream.',
  }
}

// Border-crossing loss: loss starts at a specific hop AND persists at
// every downstream hop, AND that hop crosses into a new DNS domain
// from the previous hop. Strongly suggests the peering link between
// the two providers is oversubscribed. Operator should contact the
// PRIOR provider (whose customer they are), not the next one.
const MIN_BORDER_LOSS_PCT = 8 // below this the signal is too weak to act on

export function detectBorderCrossing(hops) {
  if (!hops || hops.length < 3) return null

  for (let i = 1; i < hops.length; i++) {
    const prev = hops[i - 1]
    const here = hops[i]

    // Require: prev was clean OR very low loss; here suddenly shows
    // meaningful loss; that loss is "suspect" (i.e. real per attribution);
    // and every downstream hop also shows similar loss.
    if (!prev.current_ip || !here.current_ip) continue
    if (here.loss_state !== 'suspect') continue
    if (here.loss_percent < MIN_BORDER_LOSS_PCT) continue
    if (prev.loss_percent >= here.loss_percent * 0.5) continue // prev was already lossy → not a clean boundary

    // Loss must persist downstream — otherwise it's just one bad hop
    // (caught by other detectors / the per-hop badge).
    const downstream = hops.slice(i + 1).filter((h) => h.current_ip)
    if (downstream.length === 0) continue
    const allDownstreamLossy = downstream.every(
      (h) => h.loss_percent >= MIN_BORDER_LOSS_PCT * 0.5,
    )
    if (!allDownstreamLossy) continue

    // Domain change at the boundary.
    const prevDomain = registrableDomain(prev.hostname)
    const hereDomain = registrableDomain(here.hostname)
    if (!prevDomain || !hereDomain) continue
    if (prevDomain === hereDomain) continue

    return {
      id: `border-${i}`,
      severity: 'warn',
      title: `Loss starts at the ${prevDomain} → ${hereDomain} boundary`,
      detail:
        `Hop ${here.ttl} (${here.hostname || here.current_ip}) is the first to drop packets, and every ` +
        `downstream hop is affected too. The peering link between ${prevDomain} and ${hereDomain} is the ` +
        `likely culprit — contact ${prevDomain} (your upstream provider) about congestion at this hop.`,
    }
  }
  return null
}

// registrableDomain returns the rightmost two labels of a hostname
// (e.g. "lag-7.foo.rr.com" → "rr.com"). Good enough for the border-
// crossing detector's "did we cross networks?" check; doesn't try to
// handle the public suffix list edge cases (.co.uk etc) because
// false positives there just suppress an advisory, not corrupt one.
function registrableDomain(hostname) {
  if (!hostname) return null
  const parts = hostname.split('.').filter(Boolean)
  if (parts.length < 2) return null
  return parts.slice(-2).join('.')
}

// Sawtooth latency pattern on the destination — the classic
// bandwidth-saturation signature. Latency climbs gradually as buffers
// fill, then snaps back to baseline as backpressure relieves,
// repeated. Operationally: a download, P2P, video upload, or some
// other heavy flow is filling the operator's uplink/downlink, and
// the RTT of the destination measures the queue depth.
//
// Detection: in the recent (last 5 min) sample stream for the
// destination TTL, count "down-spikes" where RTT drops by ≥50% from
// the previous sample AND the previous sample was meaningfully
// above the median (so we're not flagging routine noise at a flat
// baseline). Three or more such cycles → sawtooth. The median
// guard suppresses noise from healthy fast links where micro-RTT
// fluctuations look saw-like in absolute terms but aren't.
const SAWTOOTH_WINDOW_MS = 5 * 60 * 1000
const SAWTOOTH_MIN_SAMPLES = 30
const SAWTOOTH_MIN_MEDIAN_MS = 20    // below this we're at the speed-of-light floor; no saturation pattern
const SAWTOOTH_PEAK_MULTIPLIER = 1.3 // a "peak" must be at least 1.3× median
const SAWTOOTH_DROP_RATIO = 0.5      // a drop counts if next < prev * this
const SAWTOOTH_MIN_CYCLES = 3        // ≥3 drops in window = sawtooth

export function detectSawtooth(samples, pathHops) {
  if (!samples?.length || !pathHops?.length) return null
  const target = pathHops[pathHops.length - 1]
  if (!target) return null

  const targetSamples = samples
    .filter((s) => s.ttl === target.ttl && s.ip !== null && s.rtt_ms > 0)
    .sort((a, b) => a.ts - b.ts)
  if (targetSamples.length < SAWTOOTH_MIN_SAMPLES) return null

  const latestTs = targetSamples[targetSamples.length - 1].ts
  const recent = targetSamples.filter((s) => s.ts >= latestTs - SAWTOOTH_WINDOW_MS)
  if (recent.length < SAWTOOTH_MIN_SAMPLES) return null

  const rtts = recent.map((s) => s.rtt_ms).sort((a, b) => a - b)
  const median = rtts[Math.floor(rtts.length / 2)]
  if (median < SAWTOOTH_MIN_MEDIAN_MS) return null

  let drops = 0
  for (let i = 1; i < recent.length; i++) {
    const prev = recent[i - 1].rtt_ms
    const curr = recent[i].rtt_ms
    if (prev > median * SAWTOOTH_PEAK_MULTIPLIER && curr < prev * SAWTOOTH_DROP_RATIO) {
      drops++
    }
  }
  if (drops < SAWTOOTH_MIN_CYCLES) return null

  return {
    id: 'sawtooth',
    severity: 'warn',
    title: 'Sawtooth latency pattern on the destination',
    detail:
      'Repeated rising-then-falling RTT cycles on the destination hop are the classic bandwidth-saturation ' +
      'signature. Something — a download, an upload, a video conference, P2P — is filling your uplink or ' +
      'downlink and the latency climbs as queues build, then snaps back as they drain. Find the heavy flow; ' +
      'consider QoS / traffic shaping if you can\'t throttle the source directly.',
  }
}

// analyzePath runs every detector and returns advisories in priority
// order. Uniform-loss wins over border-crossing because it points at
// something the operator can fix themselves immediately (cycle the
// router) — actionable trumps informative. Sawtooth can co-occur
// with either since it diagnoses a different layer (bandwidth vs
// hardware vs peering).
export function analyzePath(hops, samples) {
  const out = []
  const uniform = detectUniformLoss(hops)
  if (uniform) out.push(uniform)
  // Suppress border-crossing when uniform-loss already fires:
  // every hop is lossy, so the "starts at hop N" framing is misleading.
  if (!uniform) {
    const border = detectBorderCrossing(hops)
    if (border) out.push(border)
  }
  // Sawtooth is orthogonal — independent diagnostic. Add if it fires.
  if (samples) {
    const sawtooth = detectSawtooth(samples, hops)
    if (sawtooth) out.push(sawtooth)
  }
  return out
}
