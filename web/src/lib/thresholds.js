// Latency-threshold presets (step-39) — shared between ThresholdsPicker
// (the UI control) and LatencyTimeline (the chart's reference-line
// painter). Lives in lib/ so both components import the same source
// of truth; a mismatch between picker presets and chart fallback
// would let an operator pick "Cable" and see lines at a different
// pair of milliseconds.
//
// Cable is the default — covers the majority of home/SOHO operators.
// LAN for local-only targets (Tailscale, internal services); DSL for
// slower last-mile; Satellite for high-RTT links.

// Step-63: Fiber added between LAN and Cable. Operator on 1Gb fiber
// sits ~12ms baseline; Cable's 100ms warning never trips and LAN's
// 10ms is too tight for the small slack a real fiber link has. 30/100
// matches the ~3× multiplier shape used by the other presets (30ms ≈
// 2.5× baseline catches "starting to congest", 100ms catches
// "voice/gaming feel laggy"). Additive — Cable and DSL still distinct.
export const PRESETS = [
  { key: 'lan',       label: 'LAN',       warning:  10, critical:   30 },
  { key: 'fiber',     label: 'Fiber',     warning:  30, critical:  100 },
  { key: 'cable',     label: 'Cable',     warning: 100, critical:  300 },
  { key: 'dsl',       label: 'DSL',       warning: 200, critical:  500 },
  { key: 'satellite', label: 'Satellite', warning: 500, critical: 1000 },
]

export const DEFAULT_PRESET = PRESETS.find((p) => p.key === 'cable')
