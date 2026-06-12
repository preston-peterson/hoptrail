// Hoptrail Svelte stores — three observable values backed by polling
// the v0.1 API. Components subscribe via $store syntax; updates push
// to all subscribers automatically.
//
// Polling cadences chosen to match the daemon's natural rhythm:
//   - path snapshot: 1s (matches per-hop pinger cadence)
//   - samples:       1s (each tick produces ~30 new samples)
//   - route changes: 5s (rare events; lower cadence reduces load)
//
// WebSockets are a v0.2 upgrade if polling latency starts to feel
// wrong. For v0.1 polling keeps the API and the server-side code
// simple — no connection state, no fan-out, no reconnect logic.

import { writable, get, derived } from 'svelte/store'
import { fetchPath, fetchSamples, fetchRouteChanges, fetchTargets, fetchTargetHistory, fetchAnnotations, fetchTabs, createTab, updateTab, deleteTab, reorderTabsApi, fetchVersion, fetchProbes, fetchRetention, fetchBandwidthConfig, fetchBandwidthHistory, fetchBandwidthDerateStatus, fetchLayout, patchLayout, fetchStatus, patchBandwidthConfig } from './api.js'
import { computeFocusedHops } from './focus.js'

export const pathStore         = writable(null)
export const samplesStore      = writable([])
export const routeChangesStore = writable([])
export const errorStore        = writable(null)

// ---------- probes registry (step-94, v0.3) ----------
//
// probesStore mirrors GET /api/probes: the local probe plus any
// registered remote probes. The ACTIVE probe became per-tab in
// step-96 — see activeProbeId below (defined after activeTab, which
// it derives from).
export const probesStore = writable([])

// Step-85: hoptrail build version, fetched once from /api/version at
// startup so the StatusBar can render it next to the wordmark. null
// while the fetch is in flight; an empty/error fetch is silently
// ignored (version display is purely informational).
export const versionStore      = writable(null)
// Step-97: retention_days from /api/retention — how far back stats
// go. null until fetched; display-only.
export const retentionDays     = writable(null)

// ---------- v0.4 bandwidth + settings panel (step-101) ----------

// settingsOpen drives the slide-out settings panel (gear icon).
export const settingsOpen = writable(false)

// bandwidthConfig mirrors GET /api/bandwidth/config. null until the
// first fetch; refreshed on panel open and after every PATCH so all
// state (including dismissals and run_in_flight) is server-truth —
// the design's no-localStorage rule.
export const bandwidthConfig = writable(null)

/** Re-fetches /api/bandwidth/config into bandwidthConfig. */
export async function refreshBandwidthConfig() {
  try {
    bandwidthConfig.set(await fetchBandwidthConfig())
  } catch (err) {
    errorStore.set(err.message ?? String(err))
  }
}

// derateStatus mirrors /api/bandwidth/derate-status — the cheap
// banner endpoint (latest test, baseline, incident start, dismissal).
// Polled at 60s: bandwidth changes on test cadence, not seconds.
export const derateStatus = writable(null)

// bandwidthSamples backs the bandwidth chart card. Window follows the
// SAME timeWindow/chartAnchor the latency chart uses (visual
// correlation is the point — design §6.2). Sparse data (1-6 rows/day)
// so a full refetch per poll is cheap.
export const bandwidthSamples = writable([])

// ---------- dashboard section layout (step-126) ----------

// One global arrangement for the dashboard's four sections,
// server-persisted (`ui.layout` config row) so it follows the
// operator across browsers. v2 shape (step-127): `order` is the
// full-width main stack, `side` the vertical dock pinned to
// `side_position`. Default matches the classic look: route changes
// docked right.
export const SECTION_IDS = ['latency', 'bandwidth', 'hops']
export const sectionLayout = writable({
  order: ['latency', 'bandwidth', 'hops'],
  side: [],
  side_position: 'right',
  side_width: 340,
  collapsed: {},
})

// ---------- theme (step-141: moved under the gear) ----------

// 'auto' | 'light' | 'dark'. localStorage-persisted (a per-browser
// display preference, deliberately NOT server-side — your laptop can
// be dark while the wall monitor is light).
export const theme = writable('auto')
export function initTheme() {
  const saved = localStorage.getItem('hoptrail-theme')
  if (saved === 'light' || saved === 'dark') theme.set(saved)
  applyTheme(get(theme))
}
export function setTheme(t) {
  if (!['auto', 'light', 'dark'].includes(t)) return
  theme.set(t)
  applyTheme(t)
}
function applyTheme(t) {
  if (t === 'auto') document.documentElement.removeAttribute('data-theme')
  else document.documentElement.setAttribute('data-theme', t)
  localStorage.setItem('hoptrail-theme', t)
}

// ---------- environment status (step-140) ----------

// statusStore mirrors GET /api/status, polled at 30s — drives the
// StatusBar health dot and the status overlay. statusOpen drives the
// overlay itself.
export const statusStore = writable(null)
export const statusOpen = writable(false)

// docsOpen drives the in-app documentation overlay (step-143).
export const docsOpen = writable(false)

// alertHistoryOpen drives the alert-history overlay (step-149).
export const alertHistoryOpen = writable(false)

// Unseen-alert badge (step-152): the bell lights when the newest
// alert_history id outruns what this browser has viewed. Seen-ness is
// per-browser by nature → localStorage is correct here.
export const alertsSeenId = writable(Number(localStorage.getItem('hoptrail-alerts-seen') ?? 0))
export function markAlertsSeen(id) {
  if (id == null) return
  alertsSeenId.set(id)
  localStorage.setItem('hoptrail-alerts-seen', String(id))
}
export const unseenAlerts = derived([statusStore, alertsSeenId], ([$s, $seen]) =>
  Boolean($s && ($s.alerts?.latest_history_id ?? 0) > $seen))

// envHealth distills the aggregate into one light: 'red' (probe
// offline or active incident or sudoers drift), 'amber' (derate, or
// alert queue backing up), 'green' otherwise. The dot is a summary,
// not a diagnosis — the overlay has the details.
export const envHealth = derived(statusStore, ($s) => {
  if (!$s) return 'unknown'
  const probeDown = ($s.probes ?? []).some((p) => !p.online)
  if (probeDown || ($s.alerts?.active_incidents ?? 0) > 0 || $s.update?.sudoers_ok === false) return 'red'
  if ($s.bandwidth?.derate || ($s.alerts?.queue_depth ?? 0) > 0) return 'amber'
  return 'green'
})

let statusTimer = null
function startStatusPoll() {
  if (statusTimer != null) clearInterval(statusTimer)
  const tick = async () => {
    try {
      statusStore.set(await fetchStatus())
    } catch {
      // Unreachable daemon: the main pollers surface that; keep the
      // last-known snapshot rather than flashing the dot.
    }
  }
  tick()
  statusTimer = setInterval(tick, 30_000)
}

// logsOpen drives the full-screen log overlay (step-129: logs are a
// diagnostic, opened from Settings -> System -> View daemon log, not
// a dashboard section).
export const logsOpen = writable(false)

/** Persists a new layout; optimistic local set, server-normalized on ack. */
export async function saveSectionLayout(layout) {
  sectionLayout.set(layout)
  try {
    sectionLayout.set(await patchLayout(layout))
  } catch (err) {
    errorStore.set(err.message ?? String(err))
  }
}

// The bandwidth section renders only when there's something to show
// (enabled, or historical samples exist) — the operator's "don't show
// it unless speedtest is set up" rule. Shared by the chart card and
// the App-level section stack so both agree.
export const bandwidthSectionVisible = derived(
  [bandwidthConfig, bandwidthSamples],
  ([$cfg, $samples]) => Boolean($cfg && ($cfg.enabled || $samples.length > 0))
)

// The [since, until] the bandwidth poll last fetched — the chart pins
// its x-range to this so a single sample doesn't let uPlot auto-range
// the axis into absurdity (step-105 fix: one point produced a
// multi-year x-axis).
export const bandwidthWindow = writable(null)

// Operator-selectable bandwidth chart range (step-106): 'view'
// follows the latency window+anchor exactly and is the DEFAULT —
// correlation with the latency chart is the point (operator call).
// The fixed spans are for stepping back to trend views.
export const BW_WINDOWS = { view: 0, '1h': 3_600_000, '6h': 21_600_000, '24h': 86_400_000, '7d': 604_800_000, '30d': 2_592_000_000 }
// Step-145: server-persisted (bandwidth.chart_window config row) —
// the localStorage version was per-origin, so reaching the same
// daemon via a new DNS name silently reset the picker and "lost" the
// chart (operator-reported). Synced from every config poll; the
// setter PATCHes and applies optimistically.
export const bandwidthChartWindow = writable('view')
bandwidthConfig.subscribe(($cfg) => {
  if ($cfg?.chart_window && $cfg.chart_window in BW_WINDOWS && $cfg.chart_window !== get(bandwidthChartWindow)) {
    bandwidthChartWindow.set($cfg.chart_window)
    startBandwidthPoll()
  }
})
export function setBandwidthChartWindow(key) {
  if (!(key in BW_WINDOWS)) return
  bandwidthChartWindow.set(key)
  startBandwidthPoll()
  // The bandwidth PATCH answers 204 — re-fetch for server truth
  // rather than trusting a body that isn't there (the step-145 first
  // cut set the store to undefined and hid the whole section).
  patchBandwidthConfig({ chart_window: key })
    .then(() => refreshBandwidthConfig())
    .catch(() => {})
}

let bandwidthTimer = null
// Generation guard (step-147): restarting the poll (range change)
// must invalidate in-flight fetches — a stale response landing last
// would clobber the fresh window's samples (operator hit it: re-pick
// a range and the graph vanished under a late empty 'view' fetch).
let bandwidthPollGen = 0
function startBandwidthPoll() {
  if (bandwidthTimer != null) {
    clearInterval(bandwidthTimer)
    bandwidthTimer = null
  }
  const gen = ++bandwidthPollGen
  const tick = async () => {
    if (gen !== bandwidthPollGen) return
    try {
      const status = await fetchBandwidthDerateStatus()
      if (gen !== bandwidthPollGen) return
      derateStatus.set(status)
    } catch {
      // Bandwidth telemetry is auxiliary — never surface its poll
      // failures in the main error line.
    }
    try {
      const key = get(bandwidthChartWindow)
      const cfg = TIME_WINDOWS[get(timeWindow)] ?? TIME_WINDOWS['5m']
      // 'view' follows the latency window AND its scroll-back anchor
      // (correlation is its job); fixed spans are always "the last X
      // from now" — step-148: they previously inherited the latency
      // anchor too, so a tab anchored in the past could make "24h"
      // quietly mean a day that contained no tests.
      const until = key === 'view' ? (get(chartAnchor) ?? Date.now()) : Date.now()
      const windowMs = key === 'view' ? cfg.ms : (BW_WINDOWS[key] ?? 86_400_000)
      const data = await fetchBandwidthHistory({ since: until - windowMs, until })
      if (gen !== bandwidthPollGen) return // superseded mid-flight
      bandwidthSamples.set(data.samples ?? [])
      bandwidthWindow.set({ since: until - windowMs, until })
    } catch {
      // Same: silent.
    }
  }
  tick()
  bandwidthTimer = setInterval(tick, 60_000)
}

/** Immediate bandwidth refetch — used after a manual run completes. */
export function refreshBandwidth() {
  startBandwidthPoll()
}

// Step-70: multi-tab-per-target state. tabsStore holds the full
// daemon-reported tab set (one row per tab, with target + thresholds
// + position + label), polled from /api/tabs every 5s. activeTabId
// is the operator's tab selection — what gets persisted across
// reloads.
//
// activeTarget and targetsStore are kept as DERIVED views of the tab
// set for backward compatibility — consumers that read
// `$activeTarget` (e.g. /api/path?target=...) keep working unchanged,
// they just resolve to the active tab's target instead of being the
// canonical writable. Same with targetsStore (unique targets across
// all tabs); BundlesMenu still reads it until step-71.
//
// Shape of a tab object:
//   { tab_id, target, label, warning_ms, critical_ms, position, created_at }
export const tabsStore   = writable([])
export const activeTabId = writable(null) // number | null

export const activeTab = derived(
  [tabsStore, activeTabId],
  ([$tabs, $id]) => $tabs.find((t) => t.tab_id === $id) ?? null,
)
export const activeTarget = derived(activeTab, ($tab) => $tab?.target ?? null)

// Step-96: probe selection is per-tab, stored server-side on the tab
// row (migration v12) like label and thresholds — it survives
// browsers/devices and rides bundles. The picker edits the ACTIVE
// tab's probe; 'local' is every tab's default and the v0.2 behavior.
export const activeProbeId = derived(activeTab, ($tab) => $tab?.probe_id ?? 'local')

/**
 * Sets the ACTIVE tab's probe. Optimistic tabsStore update for
 * instant poll restart (via the activeProbeId subscription in
 * initStores); the PATCH persists it server-side. On failure the
 * next tabs poll reconciles back to server truth and the error
 * surfaces inline.
 */
export async function setActiveProbe(id) {
  const tab = get(activeTab)
  if (!tab || !id || id === (tab.probe_id ?? 'local')) return
  tabsStore.update((tabs) =>
    tabs.map((t) => (t.tab_id === tab.tab_id ? { ...t, probe_id: id } : t))
  )
  try {
    await updateTab(tab.tab_id, { probeId: id })
  } catch (err) {
    errorStore.set(err.message ?? String(err))
  }
}
// Step-130: per-tab inline-route-changes toggle. Server-persisted on
// the tab row (operator requirement: follows them across browsers).
// Same optimistic-update-then-PATCH shape as setActiveProbe.
export async function setShowRouteChanges(value) {
  const tab = get(activeTab)
  if (!tab || Boolean(tab.show_route_changes) === Boolean(value)) return
  tabsStore.update((tabs) =>
    tabs.map((t) => (t.tab_id === tab.tab_id ? { ...t, show_route_changes: value } : t))
  )
  try {
    await updateTab(tab.tab_id, { showRouteChanges: value })
  } catch (err) {
    errorStore.set(err.message ?? String(err))
  }
}

export const targetsStore = derived(tabsStore, ($tabs) => {
  // Order targets by first-tab-position — matches how the tab bar
  // shows them. Distinct so each target appears once even when there
  // are multiple tabs for it.
  const seen = new Set()
  const out = []
  for (const t of $tabs) {
    if (!seen.has(t.target)) {
      seen.add(t.target)
      out.push(t.target)
    }
  }
  return out
})

// targetIntervals (step-37) mirrors the supervisor's per-target
// pinger cadence as a {target: intervalMs} map. Populated from
// GET /api/targets's intervals_ms field on each targets-poll tick
// and updated optimistically when the operator picks a new cadence
// in IntervalPicker. The UI uses this so the picker's "current"
// pill reflects the live state without an extra round-trip per
// tab switch.
export const targetIntervals = writable({})

// targetThresholds is the BACKWARD-COMPAT view: a derived map of
// {target: {warning_ms, critical_ms}} computed from the first tab
// per target. Step-39's design (per-tab thresholds) is now realized
// as actual per-tab thresholds via tabThresholds below; this derived
// alias is retained so the existing ThresholdsPicker / LatencyTimeline
// keep working through the rename, and gets removed once those
// components migrate to tabThresholds directly.
export const tabThresholds = derived(tabsStore, ($tabs) => {
  const out = {}
  for (const t of $tabs) {
    out[t.tab_id] = { warning_ms: t.warning_ms, critical_ms: t.critical_ms }
  }
  return out
})
export const targetThresholds = derived(tabsStore, ($tabs) => {
  // For each target, return the *first* tab's thresholds — the
  // legacy semantic of "one threshold pair per target." With multi-
  // tab, the per-target view is fundamentally ambiguous; the picker
  // and chart should migrate to reading tabThresholds by tab_id.
  const out = {}
  for (const t of $tabs) {
    if (!(t.target in out)) {
      out[t.target] = { warning_ms: t.warning_ms, critical_ms: t.critical_ms }
    }
  }
  return out
})

// targetFinalHopOnly (step-41) mirrors the per-tab final-hop-only
// flag. Shape: {target: bool} — defaults to false. Drives the
// FinalHopOnlyToggle in the chart card header.
export const targetFinalHopOnly = writable({})

// focusArea (step-43) is the per-tab focused-stats window. null
// means "live stats" — the hop list shows what the server's
// PathState rolling buffer summarizes. {since, until} (unix ms)
// means "compute stats client-side from samples in this range" —
// the hop list shows what hops looked like during that historical
// moment. Independent of chartAnchor (which controls what the chart
// *views*, not what the trace grid summarizes).
//
// Set by double-clicking the chart; cleared by the × on the focus
// badge. Step-46: per-tab via focusAreaByTab — focus survives tab
// switches. Step-70: re-keyed from target to tab_id so two tabs of
// the same target carry independent focus windows.
export const focusAreaByTab = writable({}) // { tab_id: {since, until} }
export const focusArea = derived(
  [focusAreaByTab, activeTabId],
  ([$map, $id]) => $id != null ? ($map[$id] ?? null) : null,
)
export function setFocusArea(area) {
  const id = get(activeTabId)
  if (id == null) return
  focusAreaByTab.update((m) => {
    const next = { ...m }
    if (area == null) {
      delete next[id]
    } else {
      next[id] = area
    }
    persistTabState()
    return next
  })
}

// displayHops is the derived store HopList renders from: server-
// reported hops when focusArea is null (live mode), or focused-window
// stats when set. The shape is identical so HopCard doesn't branch.
//
// Performance: computeFocusedHops scans samplesStore once per
// recompute (≤ a few thousand samples in a typical chart window).
// The derived only fires when one of its inputs changes — focus
// changes are rare, and the samples-poll already throttles per the
// time-window picker's pollMs.
export const displayHops = derived(
  [pathStore, samplesStore, focusArea],
  ([$path, $samples, $focus]) => {
    const baseHops = $path?.hops ?? []
    if (!$focus) return baseHops
    return computeFocusedHops($samples, baseHops, $focus.since, $focus.until)
  },
)

// Step-47: focus-window width (ms). Operator-tunable so a 5-min
// incident doesn't have to be investigated through a hardcoded 60s
// lens. Global (not per-tab) since this is a "how do I prefer to
// investigate" preference rather than a target-specific property.
// Persisted to localStorage alongside the tab-state blob.
export const FOCUS_WIDTHS = [
  { label: '15s',  ms:   15_000 },
  { label: '60s',  ms:   60_000 },
  { label: '5m',   ms:  300_000 },
  { label: '30m',  ms: 1_800_000 },
  { label: '1h',   ms: 3_600_000 },
]
export const focusWidth = writable(60_000)

// annotationsStore (step-42) holds the visible-window annotations
// for the active tab. Polled at a low cadence (5s — annotations are
// rare, operator-created events) and re-fetched whenever the active
// target / chart window changes so the on-chart ▲ markers track the
// current view. Empty array when no notes exist for the window.
export const annotationsStore = writable([])

// chartAnchor (step-35) is the chart's temporal anchor. null means
// "follow now" — the chart shows the trailing time-window ending at
// the current moment, samples poll incrementally as live data
// arrives. A number is a unix-ms timestamp: the chart shows the
// time-window ending at that anchor (so width = TIME_WINDOWS[key].ms,
// since = anchor - width, until = anchor). In past mode polling
// pauses — a historical window is stable, no need to re-fetch.
// Operators navigate via the latency timeline's ← / now / → controls.
//
// ---- Step-46 per-tab persistence helpers ----
//
// Three per-tab maps (timeWindow, chartAnchor, focusArea) all live
// in a single localStorage JSON blob so the writes are batched and
// the schema is easy to evolve in one place. Each map is sparse:
// missing tab keys mean "use the store's default" (e.g. '5m' for
// timeWindow, null for chartAnchor/focusArea).
const TAB_STATE_KEY = 'hoptrail-tab-state'

function persistTabState() {
  // Read current map values without subscribing — this is called
  // from setter functions that just mutated the store, so values
  // are already current. Uses queueMicrotask to defer until after
  // the writable's update callback returns (otherwise we'd capture
  // pre-update values).
  //
  // Step-70: tabOrder field is gone — server-side tabs.position is
  // canonical now. The per-tab maps are keyed by tab_id (integer),
  // serialized as string keys per JSON convention.
  queueMicrotask(() => {
    try {
      const blob = {
        timeWindow: get(timeWindowByTab),
        chartAnchor: get(chartAnchorByTab),
        focusArea: get(focusAreaByTab),
        focusWidth: get(focusWidth), // step-47 — global preference
      }
      localStorage.setItem(TAB_STATE_KEY, JSON.stringify(blob))
    } catch {
      // localStorage may be unavailable (private browsing, quota);
      // silently drop persistence rather than crash the UI.
    }
  })
}

// Step-70: legacy per-tab maps from the pre-multi-tab world. When the
// stored map uses target strings as keys, we hold the data here and
// run a one-shot migration on the first non-empty tabsStore update,
// re-keying each entry to the first matching tab's tab_id. After
// migration these slots are cleared and persistTabState writes the
// new shape going forward.
let legacyTimeWindowByTarget = null
let legacyChartAnchorByTarget = null
let legacyFocusAreaByTarget = null
let legacyMigrationDone = false

// A map looks "new shape" if every key is a numeric string (tab_id).
// "old shape" otherwise (target strings like "8.8.8.8" or "google.com").
// Edge case: an empty map is treated as new shape (nothing to migrate).
function looksNewShape(map) {
  if (!map || typeof map !== 'object') return true
  for (const k of Object.keys(map)) {
    if (!/^\d+$/.test(k)) return false
  }
  return true
}

function restoreTabState() {
  try {
    const raw = localStorage.getItem(TAB_STATE_KEY)
    if (!raw) return
    const blob = JSON.parse(raw)
    if (blob && typeof blob === 'object') {
      if (blob.timeWindow) {
        if (looksNewShape(blob.timeWindow)) {
          timeWindowByTab.set(coerceIntegerKeys(blob.timeWindow))
        } else {
          legacyTimeWindowByTarget = blob.timeWindow
        }
      }
      if (blob.chartAnchor) {
        // Step-57: any pre-step-56 persisted anchors are float-valued
        // (the drag-pan bug from lesson #13). Round on restore so a
        // stale localStorage doesn't keep firing 400s on /api/samples
        // after the wire-edge fix landed.
        const sanitized = {}
        for (const [k, v] of Object.entries(blob.chartAnchor)) {
          if (typeof v === 'number' && Number.isFinite(v)) {
            sanitized[k] = Math.round(v)
          }
        }
        if (looksNewShape(sanitized)) {
          chartAnchorByTab.set(coerceIntegerKeys(sanitized))
        } else {
          legacyChartAnchorByTarget = sanitized
        }
      }
      if (blob.focusArea) {
        if (looksNewShape(blob.focusArea)) {
          focusAreaByTab.set(coerceIntegerKeys(blob.focusArea))
        } else {
          legacyFocusAreaByTarget = blob.focusArea
        }
      }
      if (typeof blob.focusWidth === 'number' && blob.focusWidth > 0) {
        focusWidth.set(blob.focusWidth)
      }
    }
  } catch {
    // Corrupted blob — start fresh.
  }
}

// JSON object keys are always strings; the per-tab maps are
// semantically keyed by tab_id (integer). Coerce so lookups against
// `get(activeTabId)` (number) match the persisted key (string).
function coerceIntegerKeys(map) {
  const out = {}
  for (const [k, v] of Object.entries(map)) {
    const n = Number(k)
    if (Number.isInteger(n)) out[n] = v
  }
  return out
}

// One-shot migration: walk the legacy target-keyed maps and re-key
// each entry to the first matching tab's tab_id from the freshly-
// loaded tabsStore. Called from the tabsStore subscriber after the
// first non-empty update. Idempotent: subsequent invocations are
// no-ops (legacyMigrationDone flag).
function migrateLegacyTabState(tabs) {
  if (legacyMigrationDone) return
  if (!tabs || tabs.length === 0) return
  legacyMigrationDone = true

  // Map target → first tab_id for that target (lowest position).
  const firstTabForTarget = {}
  for (const t of tabs) {
    if (!(t.target in firstTabForTarget)) {
      firstTabForTarget[t.target] = t.tab_id
    }
  }
  const migrateOne = (legacy) => {
    if (!legacy) return {}
    const next = {}
    for (const [target, value] of Object.entries(legacy)) {
      const id = firstTabForTarget[target]
      if (id != null) next[id] = value
    }
    return next
  }
  if (legacyTimeWindowByTarget) {
    const migrated = migrateOne(legacyTimeWindowByTarget)
    timeWindowByTab.update((m) => ({ ...migrated, ...m }))
    legacyTimeWindowByTarget = null
  }
  if (legacyChartAnchorByTarget) {
    const migrated = migrateOne(legacyChartAnchorByTarget)
    chartAnchorByTab.update((m) => ({ ...migrated, ...m }))
    legacyChartAnchorByTarget = null
  }
  if (legacyFocusAreaByTarget) {
    const migrated = migrateOne(legacyFocusAreaByTarget)
    focusAreaByTab.update((m) => ({ ...migrated, ...m }))
    legacyFocusAreaByTarget = null
  }
  // Persist the migrated shape immediately so a reload doesn't re-
  // trigger the migration.
  persistTabState()
}

// Step-47: focusWidth needs its own subscribe-to-persist wiring,
// but it can't run at module-load time because persistTabState reads
// other stores (timeWindowByTab etc) that are declared further down
// the file — the synchronous-immediate-fire of writable.subscribe
// would hit a temporal-dead-zone error. Wired inside initStores
// instead so it fires after all module bindings are live.

// Step-46: per-tab. Each tab carries its own anchor so scrolling
// back to investigate something on tab A doesn't reset when the
// operator switches to tab B. Step-70: re-keyed from target to
// tab_id so two tabs of the same target carry independent anchors.
//
// chartAnchor is read-only (.subscribe / $-syntax); writes go
// through setChartAnchor.
export const chartAnchorByTab = writable({})  // { tab_id: anchor }
export const chartAnchor = derived(
  [chartAnchorByTab, activeTabId],
  ([$map, $id]) => $id != null ? ($map[$id] ?? null) : null,
)

// Step-70: API-backed setActiveTab. Replaces the activeTarget.set
// pattern from steps 26-69. Persisted to localStorage as
// `hoptrail-active-tab-id`.
export function setActiveTab(tabId) {
  activeTabId.set(tabId == null ? null : Number(tabId))
}

// Step-70: API-backed create. Returns the created tab object; the
// caller usually wants to activate it (TabRow does so).
export async function createTabAndActivate({ target, label, copyFrom } = {}) {
  const created = await createTab({ target, label, copyFrom })
  // Optimistically slot into tabsStore so the new tab pill appears
  // without waiting for the next 5s tabs poll. The next poll will
  // reconcile to the canonical server-side ordering.
  tabsStore.update((list) => {
    if (list.some((t) => t.tab_id === created.tab_id)) return list
    return [...list, created]
  })
  activeTabId.set(created.tab_id)
  return created
}

// Step-78: drop a tab's entries from the three per-tab localStorage
// maps (timeWindow, chartAnchor, focusArea). Without this, every
// close+create cycle leaves orphan numeric keys keyed by dead
// tab_ids — the maps grow unbounded over a long session. Idempotent
// so callers can fire it from multiple delete paths without
// double-checking.
export function dropTabState(tabId) {
  if (tabId == null) return
  let touched = false
  timeWindowByTab.update((m) => {
    if (!(tabId in m)) return m
    const next = { ...m }
    delete next[tabId]
    touched = true
    return next
  })
  chartAnchorByTab.update((m) => {
    if (!(tabId in m)) return m
    const next = { ...m }
    delete next[tabId]
    touched = true
    return next
  })
  focusAreaByTab.update((m) => {
    if (!(tabId in m)) return m
    const next = { ...m }
    delete next[tabId]
    touched = true
    return next
  })
  if (touched) persistTabState()
}

// Step-70: API-backed delete. Server cascades target removal when
// this was the last tab. Caller is responsible for switching the
// active tab to another if the active one was deleted.
//
// Step-78: also prune per-tab localStorage maps so we don't leak
// orphan entries.
export async function deleteTabById(tabId) {
  await deleteTab(tabId)
  tabsStore.update((list) => list.filter((t) => t.tab_id !== tabId))
  dropTabState(tabId)
}

// Step-70: API-backed reorder. `orderedIds` is the new position
// order (top → bottom of the tab bar). Optimistically reorders
// tabsStore so the bar updates immediately; the next /api/tabs
// poll will reconcile.
export async function reorderTabs(orderedIds) {
  if (!Array.isArray(orderedIds) || orderedIds.length === 0) return
  // Reorder tabsStore locally to match the requested order.
  tabsStore.update((list) => {
    const byId = new Map(list.map((t) => [t.tab_id, t]))
    const next = []
    for (const id of orderedIds) {
      const t = byId.get(id)
      if (t) {
        next.push({ ...t, position: next.length })
        byId.delete(id)
      }
    }
    // Append any tabs not mentioned in the order list (defensive).
    for (const t of byId.values()) {
      next.push({ ...t, position: next.length })
    }
    return next
  })
  try {
    await reorderTabsApi(orderedIds)
  } catch (err) {
    errorStore.set(err.message ?? String(err))
  }
}

// Step-70 backward-compat: setTabOrder used to write a tab order
// derived from a target list (step-62, for bundle restore). With
// tab_id-keyed reordering, map each target to its first matching
// tab and call reorderTabs. Tabs of the same target that aren't
// represented in the input fall to the end of the order. Bundles
// will get a richer per-tab wire shape in step-71.
export async function setTabOrder(orderedTargets) {
  if (!Array.isArray(orderedTargets)) return
  const tabs = get(tabsStore)
  const byTarget = new Map()
  for (const t of tabs) {
    if (!byTarget.has(t.target)) byTarget.set(t.target, [])
    byTarget.get(t.target).push(t.tab_id)
  }
  const orderedIds = []
  const seenIds = new Set()
  for (const target of orderedTargets) {
    const ids = byTarget.get(target)
    if (!ids) continue
    for (const id of ids) {
      if (!seenIds.has(id)) {
        seenIds.add(id)
        orderedIds.push(id)
      }
    }
  }
  for (const t of tabs) {
    if (!seenIds.has(t.tab_id)) orderedIds.push(t.tab_id)
  }
  await reorderTabs(orderedIds)
}

// Step-72: PATCH a tab's label. null/empty-string clears the label
// back to "display the target." Optimistic update so the pill text
// flips immediately.
export async function setTabLabel(tabId, label) {
  const normalized = label && label.length > 0 ? label : null
  tabsStore.update((list) =>
    list.map((t) => (t.tab_id === tabId ? { ...t, label: normalized } : t)),
  )
  try {
    await updateTab(tabId, { label: normalized })
  } catch (err) {
    errorStore.set(err.message ?? String(err))
  }
}

// Step-70: PATCH a tab's threshold pair. Pass null/null to clear back
// to the active preset default. Tabs poll reconciles on next tick;
// optimistic update applied here so the chart picks up the new
// reference lines immediately.
export async function setTabThresholds(tabId, warningMs, criticalMs) {
  tabsStore.update((list) =>
    list.map((t) => (t.tab_id === tabId ? { ...t, warning_ms: warningMs, critical_ms: criticalMs } : t)),
  )
  try {
    await updateTab(tabId, { warningMs, criticalMs })
  } catch (err) {
    errorStore.set(err.message ?? String(err))
  }
}

export function setChartAnchor(anchor) {
  const id = get(activeTabId)
  if (id == null) return
  // Step-56: round to integer ms. Drag-pan computes anchor as
  // `start - dx * msPerPx` which is float-valued; passing the
  // fractional value to /api/samples?until=... was rejected by the
  // server (strconv.ParseInt) as "not a unix-ms integer", which
  // surfaced as the stuck-empty chart the operator caught. Fix at
  // the setter so every caller is safe by default.
  if (anchor != null) anchor = Math.round(anchor)
  chartAnchorByTab.update((m) => {
    const next = { ...m }
    if (anchor == null) {
      delete next[id] // keep the map sparse; null is the default
    } else {
      next[id] = anchor
    }
    persistTabState()
    return next
  })
}

// Per-target destination-hostname cache (step-28). Populated from
// pathStore each time a tab is active and its discovery has reached
// the destination; tabHostnames[target] becomes the human-readable
// label the tab shows (e.g. "dns.google" instead of "8.8.8.8").
// In-memory only — a reload resets the cache, and inactive tabs
// for never-visited targets just show their IP until first visit.
// Adding localStorage persistence is a small follow-up if reload
// regression becomes a real annoyance.
export const tabHostnames  = writable({})

// Recent-target history (step-30). Backed by SQLite's target_history
// table — every successful POST /api/targets writes to it, and
// GET /api/target_history returns the most recent N entries.
// Durable across browsers/devices/data-clears, unlike the original
// localStorage-only implementation.
//
// The store holds the last list fetched from the daemon. TabRow
// calls refreshTargetHistory() when the add-form opens so the
// dropdown is fresh; we don't poll continuously since the history
// rarely changes and a stale list for a few seconds is harmless.
export const targetHistory = writable([])

/**
 * Refresh targetHistory from /api/target_history. Called by TabRow
 * when the add-form opens. Silently no-ops on error — the dropdown
 * just shows whatever was last fetched (or empty on first call).
 */
export async function refreshTargetHistory() {
  try {
    const data = await fetchTargetHistory()
    targetHistory.set(data.targets ?? [])
  } catch {
    // Backend transient error; keep showing last-known list.
  }
}

// Time-window picker (step-24). Controls both the main chart's
// horizontal axis and the per-hop sparklines' window — they share
// samplesStore so a single selector drives both views.
//
// Polling cadence scales with window length: short windows want
// near-real-time updates (the chart's tail is what's interesting);
// long windows mostly show historical context and don't need
// per-second refresh. Re-polling the full window each tick is also
// wasteful for longer windows — until incremental polling lands
// (banked follow-up), slowing the cadence is the simplest mitigation.
//
// 24h and 7d are deliberately omitted from this initial set: at the
// current ~2.5s probe cadence those windows return 400K-3M samples
// per poll, which needs server-side downsampling or incremental
// polling to be practical. Both are banked as follow-ups.
// Step-33 made the samples poll incremental (fetch only samples
// since the last seen ts), so the response per tick is small even
// for long windows. That lets us run noticeably tighter polling
// cadences than the step-24 full-window-refetch math required, and
// it unblocks 24h — the per-tick payload at 24h is now ~30-100
// samples (not 400K), so we can poll it every 15s with no strain.
//
// Step-65 unblocked 7d via server-side downsampling. `bucketMs`,
// when present, asks /api/samples to return one representative sample
// per (TTL, time-bucket) instead of raw samples. At 7d × 11 hops × 1
// sample/sec the raw count is 6.6M; with 5min buckets that drops to
// ~22k, fast over the wire and fast to render. Bucketing only kicks
// in on the initial full-window fetch; incremental polling for the
// recent tail still uses raw samples (small tail = no need to bucket).
export const TIME_WINDOWS = {
  '5m':  { ms:     5 * 60 * 1000, pollMs:  1000 },
  '15m': { ms:    15 * 60 * 1000, pollMs:  1000 },
  '30m': { ms:    30 * 60 * 1000, pollMs:  1000 },
  '1h':  { ms:    60 * 60 * 1000, pollMs:  2000 },
  '6h':  { ms: 6 * 60 * 60 * 1000, pollMs:  5000 },
  '12h': { ms:12 * 60 * 60 * 1000, pollMs: 10000 },
  '24h': { ms:24 * 60 * 60 * 1000, pollMs: 15000 },
  '7d':  { ms: 7 * 24 * 60 * 60 * 1000, pollMs: 60000, bucketMs: 5 * 60 * 1000 },
}
export const TIME_WINDOW_KEYS = ['5m', '15m', '30m', '1h', '6h', '12h', '24h', '7d']

// Step-46: timeWindow is per-tab. timeWindowByTab is the source of
// truth; timeWindow is a derived view of the active tab's slot
// (defaulting to '5m' when the tab has no override). Writes go
// through setTimeWindow which mutates the right slot.
// Step-70: re-keyed from target to tab_id.
export const timeWindowByTab = writable({}) // { tab_id: '5m' }
export const timeWindow = derived(
  [timeWindowByTab, activeTabId],
  ([$map, $id]) => $id != null ? ($map[$id] ?? '5m') : '5m',
)
export function setTimeWindow(key) {
  const id = get(activeTabId)
  if (id == null) return
  timeWindowByTab.update((m) => {
    const next = { ...m, [id]: key }
    persistTabState()
    return next
  })
}

// selectedTTL drives the latency chart: it shows samples for exactly
// this TTL. Default is null, then gets set to the path's target_ttl
// on the first path response that surfaces one. User clicks in the
// hop list overwrite this; the auto-default never fires again after
// the first non-null value.
//
// "User picked something" vs "still on default" is tracked by the
// `userSelectedTTL` flag below — the auto-default check is gated on
// that flag, so a route change that resets target_ttl never clobbers
// a user-chosen hop. If the user-chosen TTL later disappears from the
// path (route shrank), the chart simply shows no data; the user can
// click a different hop. v0.2 may want to handle this more gracefully.
export const selectedTTL = writable(null)
let userSelectedTTL = false

/**
 * Mark the selected TTL as user-chosen. Called from HopList when a
 * user click sets the selection. Subsequent auto-defaults from the
 * path poll won't overwrite the user's choice.
 */
export function setSelectedTTL(ttl) {
  userSelectedTTL = true
  selectedTTL.set(ttl)
}

// Track timers so stopPolling can clear them on app teardown.
let timers = []

// Per-target polls and the targets-list poll each live in their own
// timer slot so they can be restarted independently:
//   - samplesTimer restarts when timeWindow OR activeTarget changes
//   - pathTimer / routeChangesTimer restart when activeTarget changes
//   - targetsTimer is constant cadence; its tick reconciles the
//     server's lexically-sorted list against our locally-ordered
//     tab row, preserving user-driven order
let samplesTimer = null
let pathTimer = null
let routeChangesTimer = null
let targetsTimer = null
let annotationsTimer = null
let probesTimer = null

// lastPolledProbe is the probe the data polls were last started with.
// Lets the activeTarget and activeProbeId subscriptions coordinate so
// a tab switch (which changes both) restarts the polls exactly once.
let lastPolledProbe = null

// Step-94: probes poll. 10s — probe membership changes on the
// timescale of heartbeats (60s), so this is generous; the payload is
// a handful of rows. Also reconciles the active probe: if the
// operator's persisted selection disappears from the registry (row
// deleted server-side), fall back to local rather than polling a
// probe the server will 404.
function startProbesPoll() {
  if (probesTimer != null) {
    clearInterval(probesTimer)
    probesTimer = null
  if (statusTimer != null) clearInterval(statusTimer)
  statusTimer = null
  }
  const tick = async () => {
    try {
      const data = await fetchProbes()
      probesStore.set(data.probes ?? [])
      // No active-probe reconciliation here (step-96): the selection
      // lives on the tab row server-side and writes are validated, so
      // a tab can only reference a probe that existed when it was set.
    } catch {
      // Transient — the picker just keeps its last-known list.
    }
  }
  tick()
  probesTimer = setInterval(tick, 10_000)
}

// Step-70: tabs poll. Replaces step-26's role of populating the
// canonical tab set. Re-keyed: tabsStore is the source of truth;
// targetsStore is now derived. activeTabId migrates to the first
// surviving tab when the previously-active one disappears.
let tabsTimer = null
function startTabsPoll() {
  if (tabsTimer != null) {
    clearInterval(tabsTimer)
    tabsTimer = null
  }
  const tick = async () => {
    try {
      const data = await fetchTabs()
      const serverTabs = data.tabs ?? []
      tabsStore.set(serverTabs)
      // Migrate activeTabId if the selected tab disappeared.
      const currentId = get(activeTabId)
      if (serverTabs.length === 0) {
        activeTabId.set(null)
      } else if (currentId == null || !serverTabs.some((t) => t.tab_id === currentId)) {
        activeTabId.set(serverTabs[0].tab_id)
      }
      // Run the one-shot legacy-localStorage migration once tabsStore
      // has its first non-empty value.
      migrateLegacyTabState(serverTabs)
      errorStore.set(null)
    } catch (err) {
      errorStore.set(err.message ?? String(err))
    }
  }
  tick()
  tabsTimer = setInterval(tick, 5000)
}

// Step-70: targets poll trimmed. tabsStore drives the tab bar now;
// /api/targets is still the source of truth for per-target probe
// settings (interval, final-hop-only) since those don't move to tabs
// in the design. Threshold map from this endpoint is ignored — the
// canonical thresholds come from /api/tabs.
function startTargetsPoll() {
  if (targetsTimer != null) {
    clearInterval(targetsTimer)
    targetsTimer = null
  }
  const tick = async () => {
    try {
      const data = await fetchTargets()
      targetIntervals.set(data.intervals_ms ?? {})
      targetFinalHopOnly.set(data.final_hop_only ?? {})
      errorStore.set(null)
    } catch (err) {
      errorStore.set(err.message ?? String(err))
    }
  }
  tick()
  targetsTimer = setInterval(tick, 5000)
}

/**
 * Starts polling all endpoints. Idempotent — calling twice doesn't
 * double-poll because the previous timers are cleared first.
 */
export function initStores() {
  stopPolling()
  // Reset selection state so a re-init (e.g. SPA navigation) doesn't
  // carry over a stale "user picked X" flag from a previous session.
  userSelectedTTL = false
  selectedTTL.set(null)

  // Step-85: fire-and-forget /api/version fetch so the wordmark can
  // render the build version once it arrives. Errors are swallowed —
  // missing version display is not a state worth surfacing.
  fetchVersion()
    .then((data) => versionStore.set(data?.version ?? null))
    .catch(() => {})

  // Step-97: same fire-and-forget shape — the retention display is
  // informational; a failed fetch just leaves the footer absent.
  fetchRetention()
    .then((data) => retentionDays.set(data?.retention_days ?? null))
    .catch(() => {})

  // Step-140: environment status poll (health dot + status overlay).
  startStatusPoll()

  // Step-126: section layout. A failed fetch leaves the default
  // order — the dashboard still renders.
  fetchLayout()
    .then((data) => { if (data?.order?.length) sectionLayout.set(data) })
    .catch(() => {})

  // Restore persisted choices before kicking off polls so the first
  // fetches use the right window + tab. Step-46: per-tab state lives
  // in a single JSON blob (hoptrail-tab-state). Step-70: active tab
  // persisted under hoptrail-active-tab-id (number) instead of
  // hoptrail-active-target (string).
  restoreTabState()
  const savedTabId = localStorage.getItem('hoptrail-active-tab-id')
  if (savedTabId) {
    const n = Number(savedTabId)
    if (Number.isInteger(n)) activeTabId.set(n)
  }
  // Step-70 transition: an operator coming from a pre-step-70 bundle
  // has `hoptrail-active-target` in localStorage. Stash it so the
  // first tabs poll can map it to the right tab_id (lookup happens
  // in migrateActiveTargetSelection below).
  const legacyActiveTarget = localStorage.getItem('hoptrail-active-target')

  // Tabs poll: canonical source of truth for the tab bar.
  startTabsPoll()
  // Targets poll: still runs for per-target probe settings
  // (interval, final-hop-only). Thresholds + tab set come from
  // /api/tabs now.
  startTargetsPoll()
  // Probes poll: feeds the ProbePicker; hidden on zero-agent deploys.
  startProbesPoll()
  // Step-102: bandwidth chart + banners. Config fetched once at
  // startup (banners need enabled/dismissal state before the panel
  // ever opens); history + derate-status poll at 60s.
  refreshBandwidthConfig()
  startBandwidthPoll()

  // If the operator landed without an activeTabId but had a legacy
  // activeTarget pointer, resolve it once tabs come in.
  if (savedTabId == null && legacyActiveTarget) {
    const unsubscribe = tabsStore.subscribe((tabs) => {
      if (!tabs || tabs.length === 0) return
      const match = tabs.find((t) => t.target === legacyActiveTarget)
      if (match) activeTabId.set(match.tab_id)
      localStorage.removeItem('hoptrail-active-target')
      unsubscribe()
    })
  }

  // Restart the per-target polls whenever the active tab changes.
  // activeTarget is derived from activeTab so it fires after tabsStore
  // updates settle; subscribing to that lets the path/samples polls
  // re-target without needing to know about tab_id.
  activeTabId.subscribe((id) => {
    if (id != null) {
      localStorage.setItem('hoptrail-active-tab-id', String(id))
    }
  })
  activeTarget.subscribe((target) => {
    // Step-46: focus is per-tab now — survives tab switches.
    // Step-96: note the probe these polls will read so the
    // activeProbeId subscription below doesn't double-restart them
    // on a tab switch that also changes the probe.
    lastPolledProbe = get(activeProbeId)
    startPathPoll(target)
    startRouteChangesPoll(target)
    startAnnotationsPoll(target)
    // Samples poll also needs to restart when target changes; it
    // reads the current target inside startSamplesPoll via get().
    const w = get(timeWindow)
    startSamplesPoll(w)
  })

  timeWindow.subscribe((key) => {
    // No per-key localStorage write here — persistTabState (called
    // from the setter) handles persistence across all three per-tab
    // stores in one batched JSON blob.
    startSamplesPoll(key)
  })

  // Step-94/96: a probe change on the CURRENT tab restarts the data
  // polls exactly like a target switch — full samples refetch
  // (incremental state resets inside startSamplesPoll), fresh path +
  // route changes. Tab switches are handled by the activeTarget
  // subscription above (which records lastPolledProbe so this one
  // stays quiet); annotations stay target-scoped.
  activeProbeId.subscribe((id) => {
    if (id === lastPolledProbe) return
    lastPolledProbe = id
    startPathPoll(get(activeTarget))
    startRouteChangesPoll(get(activeTarget))
    startSamplesPoll(get(timeWindow))
  })

  // Step-47: persist focusWidth on every change. Safe to subscribe
  // here because every module binding is live by the time initStores
  // is called from App.svelte's onMount.
  focusWidth.subscribe(() => persistTabState())

  // Restart the samples poll whenever the chart anchor changes —
  // live↔past transitions need a full fetch, and a different past
  // anchor needs a different historical window.
  chartAnchor.subscribe(() => {
    startSamplesPoll(get(timeWindow))
    startBandwidthPoll() // bandwidth chart follows the same anchor
  })

  // Mirror the destination hostname from pathStore into tabHostnames.
  // The path snapshot's last hop is the destination once target_ttl
  // is known; if its hostname is resolved, cache it keyed by the
  // path's target so the TabRow can render a friendly label.
  pathStore.subscribe((path) => {
    if (!path?.hops?.length || !path.target) return
    const last = path.hops[path.hops.length - 1]
    if (last?.hostname) {
      tabHostnames.update((map) =>
        map[path.target] === last.hostname ? map : { ...map, [path.target]: last.hostname }
      )
    }
  })
}

function startPathPoll(target) {
  if (pathTimer != null) {
    clearInterval(pathTimer)
    pathTimer = null
  }
  if (!target) {
    pathStore.set(null)
    return
  }
  const tick = async () => {
    try {
      const data = await fetchPath({ target, probeId: get(activeProbeId) })
      if (!userSelectedTTL && data?.target_ttl > 0) {
        selectedTTL.set(data.target_ttl)
      }
      pathStore.set(data)
      errorStore.set(null)
    } catch (err) {
      errorStore.set(err.message ?? String(err))
    }
  }
  tick()
  pathTimer = setInterval(tick, 1000)
}

function startRouteChangesPoll(target) {
  if (routeChangesTimer != null) {
    clearInterval(routeChangesTimer)
    routeChangesTimer = null
  }
  if (!target) {
    routeChangesStore.set([])
    return
  }
  const tick = async () => {
    try {
      const data = await fetchRouteChanges({ target, probeId: get(activeProbeId) })
      routeChangesStore.set(data.changes ?? [])
      errorStore.set(null)
    } catch (err) {
      errorStore.set(err.message ?? String(err))
    }
  }
  tick()
  routeChangesTimer = setInterval(tick, 5000)
}

// Annotations poll (step-42). Fetches the full annotation set for
// the active target — there are usually only a handful of operator-
// typed notes per target, so window-filtering doesn't pay for the
// extra round-trip complexity. 5s cadence matches route_changes;
// annotations are operator-initiated events with the same rarity.
function startAnnotationsPoll(target) {
  if (annotationsTimer != null) {
    clearInterval(annotationsTimer)
    annotationsTimer = null
  }
  if (!target) {
    annotationsStore.set([])
    return
  }
  const tick = async () => {
    try {
      const data = await fetchAnnotations({ target })
      annotationsStore.set(data.annotations ?? [])
      errorStore.set(null)
    } catch (err) {
      errorStore.set(err.message ?? String(err))
    }
  }
  tick()
  annotationsTimer = setInterval(tick, 5000)
}

/**
 * Triggers an immediate refetch of annotations for the active target.
 * Called by components that just mutated the annotation set (add /
 * delete) so the chart updates without waiting for the next poll tick.
 */
export async function refreshAnnotations() {
  const t = get(activeTarget)
  if (!t) return
  try {
    const data = await fetchAnnotations({ target: t })
    annotationsStore.set(data.annotations ?? [])
  } catch {
    // Transient — next poll tick will reconcile.
  }
}

// lastSampleTs is the highest ts we've seen in the current
// (window, target) polling instance. Reset whenever the timer
// restarts (window change, target change). Used for the
// incremental fetch path so each tick only pulls new samples.
let lastSampleTs = 0

function startSamplesPoll(windowKey) {
  if (samplesTimer != null) {
    clearInterval(samplesTimer)
    samplesTimer = null
  }
  // Reset incremental state — this is either initial start or a
  // window/target/anchor change, all of which require a full reload.
  lastSampleTs = 0

  const cfg = TIME_WINDOWS[windowKey] ?? TIME_WINDOWS['5m']

  // Past-mode tick: anchor is set. Fetch exactly the [anchor-ms, anchor]
  // window once and stop. No polling — a historical window is stable.
  const pastTick = async (anchor) => {
    const target = get(activeTarget)
    if (!target) {
      samplesStore.set([])
      return
    }
    try {
      const data = await fetchSamples({
        since: anchor - cfg.ms,
        until: anchor,
        target,
        bucketMs: cfg.bucketMs, // step-65: only set for 7d+
        probeId: get(activeProbeId),
      })
      samplesStore.set(data.samples ?? [])
      errorStore.set(null)
    } catch (err) {
      errorStore.set(err.message ?? String(err))
    }
  }

  // Live-mode tick: incremental polling. Initial: full window from
  // now-ms. Subsequent ticks: samples newer than lastSampleTs.
  //
  // Step-65: at long windows (7d), the *initial* full-window fetch is
  // bucketed (server returns ~22k representative samples instead of
  // 6.6M raw). Incremental ticks for the recent tail stay raw — small
  // tail = no need to bucket, and raw samples interleave cleanly with
  // bucketed history because the bucket boundaries are deterministic.
  const liveTick = async () => {
    const target = get(activeTarget)
    if (!target) {
      samplesStore.set([])
      lastSampleTs = 0
      return
    }
    try {
      const windowStart = Date.now() - cfg.ms
      const isInitialFetch = lastSampleTs === 0
      const since = isInitialFetch ? windowStart : lastSampleTs + 1
      // Initial fetch of a bucketed window goes through the bucketing
      // path; subsequent incremental polls stay raw (the tail is small).
      const bucketMs = isInitialFetch ? cfg.bucketMs : undefined

      const data = await fetchSamples({ since, target, bucketMs, probeId: get(activeProbeId) })
      const newSamples = data.samples ?? []

      if (isInitialFetch) {
        samplesStore.set(newSamples)
      } else if (newSamples.length > 0) {
        samplesStore.update((existing) => {
          const merged = existing.concat(newSamples)
          return merged.filter((s) => s.ts >= windowStart)
        })
      } else {
        samplesStore.update((existing) => existing.filter((s) => s.ts >= windowStart))
      }

      for (const s of newSamples) {
        if (s.ts > lastSampleTs) lastSampleTs = s.ts
      }

      errorStore.set(null)
    } catch (err) {
      errorStore.set(err.message ?? String(err))
    }
  }

  const anchor = get(chartAnchor)
  if (anchor != null) {
    // Past mode — single fetch, no recurring poll.
    pastTick(anchor)
  } else {
    // Live mode — incremental polling.
    liveTick()
    samplesTimer = setInterval(liveTick, cfg.pollMs)
  }
}

/**
 * Clears all poll timers. Called from App.svelte's onDestroy.
 */
export function stopPolling() {
  for (const t of timers) clearInterval(t)
  timers = []
  for (const slot of [samplesTimer, pathTimer, routeChangesTimer, targetsTimer, tabsTimer, annotationsTimer, probesTimer]) {
    if (slot != null) clearInterval(slot)
  }
  samplesTimer = null
  pathTimer = null
  routeChangesTimer = null
  targetsTimer = null
  tabsTimer = null
  annotationsTimer = null
  probesTimer = null
  if (bandwidthTimer != null) clearInterval(bandwidthTimer)
  bandwidthTimer = null
}

// poll runs an initial fetch then schedules recurring ones, updating
// the given store with each result. `extract` optionally pulls the
// useful field out of the API response shape.
function poll(fetcher, store, intervalMs, extract = (x) => x) {
  const tick = async () => {
    try {
      const data = await fetcher()
      store.set(extract(data))
      errorStore.set(null)
    } catch (err) {
      errorStore.set(err.message ?? String(err))
    }
  }
  tick()
  timers.push(setInterval(tick, intervalMs))
}
