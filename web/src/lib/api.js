// Hoptrail v0.1 API client.
//
// One small fetch wrapper per endpoint. No retries, no exponential
// backoff, no caching — those concerns live in the stores layer if
// they're needed at all. These functions return parsed JSON or throw.
//
// The contract these target is documented in docs/api-v0.1.md.
// Implementation lands in step-10; for now these are aimed at the
// proxy target from Vite's dev server, but they also work against
// the embedded production server because the paths are absolute.

const BASE = '/api'

async function getJSON(path) {
  const res = await fetch(BASE + path)
  if (!res.ok) {
    const text = (await res.text()).trim()
    throw new Error(text || `HTTP ${res.status} ${res.statusText} on GET ${path}`)
  }
  return res.json()
}

// All three data endpoints accept an optional ?target= query param to
// scope their response to one monitored target. When the multi-target
// UI is active (step-26+), every poll passes the active tab's target.
// When only one target is configured, the server defaults to that one
// and the param is unnecessary — but harmless.

// Step-94: the three data endpoints (and export) also accept an
// optional probeId. Omitted or 'local' sends no param at all — the
// server defaults to the local probe, and v0.2-identical URLs keep
// the access log clean for the overwhelmingly-common case.
function setProbeParam(params, probeId) {
  if (probeId && probeId !== 'local') params.set('probe_id', probeId)
}

/**
 * Returns the current path snapshot for `target` (optional — defaults
 * to the single active target if exactly one is monitored), as seen
 * by `probeId` (optional — defaults to the local probe).
 */
export function fetchPath({ target, probeId } = {}) {
  const params = new URLSearchParams()
  if (target) params.set('target', target)
  setProbeParam(params, probeId)
  const q = params.toString()
  return getJSON('/path' + (q ? '?' + q : ''))
}

/**
 * Returns a window of historical samples for charting, scoped to
 * `target` if provided. `since` and `until` are unix milliseconds.
 */
export function fetchSamples({ since, until, target, bucketMs, probeId } = {}) {
  const params = new URLSearchParams()
  // Step-57: round at the wire-edge. The server's parseTimeMs is
  // integer-strict (strconv.ParseInt rejects decimals). Lesson #13 —
  // any JS-computed ms that crosses the wire as an integer query param
  // gets rounded at the boundary, not at the call site.
  if (since != null) params.set('since', String(Math.round(since)))
  if (until != null) params.set('until', String(Math.round(until)))
  if (target) params.set('target', target)
  setProbeParam(params, probeId)
  // Step-65: server-side downsampling for long windows. When bucketMs
  // is set, server returns one representative sample per (TTL, bucket).
  if (bucketMs != null && bucketMs > 0) params.set('bucket_ms', String(Math.round(bucketMs)))
  const q = params.toString()
  return getJSON('/samples' + (q ? '?' + q : ''))
}

/**
 * Returns recent route changes, scoped to `target` if provided.
 */
/**
 * Step-75: clear the route_changes log for a target. Server wipes
 * the table for that target; no soft-delete. Idempotent (no error
 * when there's nothing to clear).
 */
export async function clearRouteChanges(target) {
  const params = new URLSearchParams()
  if (target) params.set('target', target)
  const res = await fetch(BASE + '/route_changes?' + params.toString(), {
    method: 'DELETE',
  })
  if (!res.ok && res.status !== 404) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
}

export function fetchRouteChanges({ since, limit, target, probeId } = {}) {
  const params = new URLSearchParams()
  // Step-57: same integer-strict rounding as fetchSamples (lesson #13).
  if (since != null) params.set('since', String(Math.round(since)))
  if (limit != null) params.set('limit', String(limit))
  if (target) params.set('target', target)
  setProbeParam(params, probeId)
  const q = params.toString()
  return getJSON('/route_changes' + (q ? '?' + q : ''))
}

// ---------- /api/probes (step-94) ----------

/**
 * Returns the registered probe list for the ProbePicker: the local
 * probe first (always online), then any remote probes with their
 * online/offline state.
 */
export function fetchProbes() {
  return getJSON('/probes')
}

// ---------- /api/probe-tokens (step-121, v0.5 probes-in-the-UI) ----------

/** Token list for the Probes settings section. Prefixes only — the
 *  full token appears exactly once, in createProbeToken's response. */
export function fetchProbeTokens() {
  return getJSON('/probe-tokens')
}

/**
 * Mints a probe token. `name` is the intended probe_id (kebab-case;
 * server-validated). Returns { id, name, token } — the only time the
 * full token crosses the wire, so the caller must surface it
 * immediately (the UI builds the install one-liner from it).
 */
export async function createProbeToken(name) {
  const res = await fetch(BASE + '/probe-tokens', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

/** Revokes a token by id. The probe using it 401s on its next push
 *  and spills to its local buffer. 204 on success. */
export async function revokeProbeToken(id) {
  const res = await fetch(BASE + '/probe-tokens/' + id, { method: 'DELETE' })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
}

/** Forgets a registered probe (row + path snapshots; tabs repointed
 *  to local). Does not revoke tokens — a probe with a live token
 *  re-registers on its next heartbeat. 204 on success. */
export async function forgetProbe(probeId) {
  const res = await fetch(BASE + '/probes/' + encodeURIComponent(probeId), { method: 'DELETE' })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
}

/**
 * Step-97: the active retention policy — how many days of samples
 * the daemon keeps. Display-only for now (the setting moves to the
 * settings panel in v0.4).
 */
export function fetchRetention() {
  return getJSON('/retention')
}

/** Step-110: operator-editable retention. 204 on success. */
export async function patchRetention(days) {
  const res = await fetch(BASE + '/retention', {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ retention_days: days }),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
}

function targetQuery(target) {
  return target ? `?target=${encodeURIComponent(target)}` : ''
}

// ---------- /api/targets (step-26) ----------

/**
 * Returns the list of currently-monitored targets.
 */
export function fetchTargets() {
  return getJSON('/targets')
}

// Step-85: build-time version string for display next to the wordmark.
// Cheap; fetched once at startup, no polling.
export function fetchVersion() {
  return getJSON('/version')
}

/**
 * Returns up to `limit` recently-added target identifiers, newest
 * first. Backed by SQLite's target_history table so the result is
 * durable across browsers and reloads (step-30).
 */
export function fetchTargetHistory({ limit } = {}) {
  const q = limit ? '?limit=' + encodeURIComponent(String(limit)) : ''
  return getJSON('/target_history' + q)
}

// ---------- /api/bundles (step-36) ----------

/**
 * Returns all saved target bundles, newest first.
 */
export function fetchBundles() {
  return getJSON('/bundles')
}

/**
 * Save (or replace) a bundle by name. `targets` is the array of
 * operator-typed target identifiers the bundle should contain.
 */
export async function saveBundle(name, payload) {
  // Step-71: payload may be `{tabs: [{target, label, warning_ms, critical_ms}]}`
  // (the new shape) or `{targets: [<string>]}` (the legacy shape).
  // Callers should pass one; server accepts either.
  const res = await fetch(BASE + '/bundles', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name, ...payload }),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

/**
 * Delete a bundle by name. Safe to call for a non-existent bundle.
 */
export async function deleteBundle(name) {
  const res = await fetch(BASE + '/bundles/' + encodeURIComponent(name), {
    method: 'DELETE',
  })
  if (!res.ok && res.status !== 404) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
}

/**
 * Adds a new monitored target. The supervisor builds a fresh probe
 * pipeline; returns when the engine is live.
 */
export async function addTarget(target) {
  const res = await fetch(BASE + '/targets', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ target }),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

/**
 * Removes a monitored target. The supervisor drains its pipeline
 * before this returns.
 */
export async function removeTarget(target) {
  const res = await fetch(BASE + '/targets/' + encodeURIComponent(target), {
    method: 'DELETE',
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

/**
 * Updates the per-hop pinger cadence for a monitored target (step-37).
 * intervalMs must be a positive integer inside the supervisor's
 * range (200–60000 at present); the daemon enforces and returns 400
 * for out-of-range values.
 */
export async function setTargetInterval(target, intervalMs) {
  const res = await fetch(BASE + '/targets/' + encodeURIComponent(target), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ interval_ms: intervalMs }),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

/**
 * Updates the per-tab latency thresholds (step-39). warningMs and
 * criticalMs must be positive with warning < critical, OR both null
 * to clear the operator's override (UI then falls back to its
 * default preset). The daemon enforces validation and returns 400 on
 * bad input / non-monitored target.
 */
export async function setTargetThresholds(target, warningMs, criticalMs) {
  const res = await fetch(BASE + '/targets/' + encodeURIComponent(target), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ warning_ms: warningMs, critical_ms: criticalMs }),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

/**
 * Toggles per-tab final-hop-only mode (step-41). When true, the
 * daemon's pinger only probes the destination TTL for this target —
 * intermediate hops are skipped. Discovery still runs so route
 * changes are detected. Triggers a brief pipeline rebuild server-side.
 */
export async function setTargetFinalHopOnly(target, finalHopOnly) {
  const res = await fetch(BASE + '/targets/' + encodeURIComponent(target), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ final_hop_only: finalHopOnly }),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

// ---------- /api/annotations (step-42) ----------

/**
 * Returns annotations for `target` in [since, until] (unix ms,
 * inclusive). since/until both optional — omit since to get the
 * full history; omit until to fetch up to "now."
 */
export function fetchAnnotations({ target, since, until } = {}) {
  const params = new URLSearchParams()
  if (target) params.set('target', target)
  // Step-57: same integer-strict rounding as fetchSamples (lesson #13).
  if (since != null) params.set('since', String(Math.round(since)))
  if (until != null) params.set('until', String(Math.round(until)))
  return getJSON('/annotations?' + params.toString())
}

/**
 * Creates a new annotation pinned to (target, ts). text is capped
 * server-side at 280 chars. Returns the inserted row with the
 * server-assigned id + created_at so the caller can stash it in the
 * store without an extra GET.
 */
export async function addAnnotation(target, ts, text) {
  const res = await fetch(BASE + '/annotations', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ target, ts, text }),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

/**
 * Deletes an annotation by id. Idempotent server-side (no error
 * when the id is already gone).
 */
export async function deleteAnnotation(id) {
  const res = await fetch(BASE + '/annotations/' + encodeURIComponent(id), {
    method: 'DELETE',
  })
  if (!res.ok && res.status !== 404) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
}

/**
 * Swap the monitored target (legacy single-target endpoint). Body
 * is `{ target }` with an IPv4 string. Kept for compat until the UI
 * shifts entirely to tab-based affordances; multi-target callers
 * should use addTarget/removeTarget above.
 */
export async function postTarget(target) {
  const res = await fetch(BASE + '/target', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ target }),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

// ---------- /api/tabs (step-69 backend, step-70 frontend) ----------

/** Returns every tab ordered by position. */
export function fetchTabs() {
  return getJSON('/tabs')
}

/**
 * Create a new tab. `target` is required; `label`, `warning_ms`,
 * `critical_ms`, `copy_from` are optional. When `copy_from` is set
 * the server inherits label + thresholds from the source tab (unless
 * the request overrides them explicitly).
 */
export async function createTab({ target, label, warningMs, criticalMs, copyFrom, probeId } = {}) {
  const body = { target }
  if (label !== undefined) body.label = label
  if (warningMs !== undefined) body.warning_ms = warningMs
  if (criticalMs !== undefined) body.critical_ms = criticalMs
  if (copyFrom !== undefined) body.copy_from = copyFrom
  if (probeId !== undefined && probeId !== 'local') body.probe_id = probeId
  const res = await fetch(BASE + '/tabs', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

/**
 * Partial update of one tab. Pass `null` for label/warning_ms/critical_ms
 * to explicitly clear those fields back to default. Threshold pair must
 * be sent together (server rejects half-pair updates).
 */
export async function updateTab(tabId, { label, warningMs, criticalMs, probeId, showRouteChanges } = {}) {
  const body = {}
  if (label !== undefined) body.label = label
  if (warningMs !== undefined) body.warning_ms = warningMs
  if (criticalMs !== undefined) body.critical_ms = criticalMs
  if (probeId !== undefined) body.probe_id = probeId
  if (showRouteChanges !== undefined) body.show_route_changes = showRouteChanges
  const res = await fetch(BASE + '/tabs/' + encodeURIComponent(String(tabId)), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

/**
 * Delete one tab. Server cascades the target removal if this was the
 * last tab for that target.
 */
export async function deleteTab(tabId) {
  const res = await fetch(BASE + '/tabs/' + encodeURIComponent(String(tabId)), {
    method: 'DELETE',
  })
  if (!res.ok && res.status !== 404) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
}

/**
 * Bulk-reorder tabs. `orderedIds` is the new position-order. Returns
 * void on success.
 */
export async function reorderTabsApi(orderedIds) {
  const res = await fetch(BASE + '/tabs/order', {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ order: orderedIds }),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
}

// ---------- /api/bandwidth/* (v0.4, step-101) ----------

/**
 * Full bandwidth config: every tunable, the dismissal/state rows,
 * capability, and run_in_flight.
 */
export function fetchBandwidthConfig() {
  return getJSON('/bandwidth/config')
}

/**
 * PATCH any subset of the writable bandwidth config keys. Body keys
 * use the wire names (snake_case). 204 on success.
 */
export async function patchBandwidthConfig(patch) {
  const res = await fetch(BASE + '/bandwidth/config', {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
}

/**
 * Bandwidth test history. since/until are unix ms (lesson #13
 * rounding applies).
 */
export function fetchBandwidthHistory({ since, until } = {}) {
  const params = new URLSearchParams()
  if (since != null) params.set('since', String(Math.round(since)))
  if (until != null) params.set('until', String(Math.round(until)))
  const q = params.toString()
  return getJSON('/bandwidth/history' + (q ? '?' + q : ''))
}

/**
 * The cheap banner endpoint: latest test, baseline, incident start,
 * dismissal state.
 */
export function fetchBandwidthDerateStatus() {
  return getJSON('/bandwidth/derate-status')
}

/**
 * Manual "run a test now". Resolves on 202; throws with the server's
 * message on 409 (in flight) / 503 (no capability).
 */
export async function runBandwidthTest() {
  const res = await fetch(BASE + '/bandwidth/run', { method: 'POST' })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
}

/**
 * Step-123: kicks off the speedtest-CLI install (the root-owned
 * helper via the sudoers rule). Returns { status: "running" }; throws
 * the server's message on 409 (already running).
 */
export async function startBandwidthCliInstall() {
  const res = await fetch(BASE + '/bandwidth/install-cli', { method: 'POST' })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

/** Step-123: install progress — { status: idle|running|ok|failed, output }. */
export function fetchBandwidthCliInstall() {
  return getJSON('/bandwidth/install-cli')
}

// ---------- self-update (step-124) ----------

/** { running_version, staged: {present, version, ...}, sudoers: {ok, ...} } */
export function fetchUpdateStatus() {
  return getJSON('/update')
}

/** Stages an uploaded binary (raw bytes). Returns the staged info. */
export async function uploadUpdateBinary(file) {
  const res = await fetch(BASE + '/update/upload', {
    method: 'POST',
    headers: { 'Content-Type': 'application/octet-stream' },
    body: file,
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

/** Applies the staged binary: backup → swap → setcap → restart. The
 *  daemon restarts ~1s after this resolves. */
export async function applyUpdate() {
  const res = await fetch(BASE + '/update/apply', { method: 'POST' })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

// ---------- system settings (step-125) ----------

/** { listen, pending_listen?, log_level, rdns_enabled,
 *    pending_rdns_enabled?, restart_required } */
export function fetchSystemSettings() {
  return getJSON('/system')
}

/** PATCH any of { listen, log_level, rdns_enabled }. Log level
 *  applies live; the others are pending until restart. Returns the
 *  refreshed settings. */
export async function patchSystemSettings(patch) {
  const res = await fetch(BASE + '/system', {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

// ---------- log viewer (step-128) ----------

/**
 * Incremental log feed from the daemon's in-memory ring.
 * { entries: [{seq, ts, level, msg, attrs}], latest_seq }.
 */
export function fetchLogs({ sinceSeq, limit } = {}) {
  const params = new URLSearchParams()
  if (sinceSeq != null) params.set('since_seq', String(sinceSeq))
  if (limit != null) params.set('limit', String(limit))
  const q = params.toString()
  return getJSON('/logs' + (q ? '?' + q : ''))
}

// ---------- in-app documentation (step-143) ----------

/** { docs: [{slug, title}] } — the embedded guide index. */
export function fetchDocsIndex() {
  return getJSON('/docs')
}

/** Raw markdown for one guide. */
export async function fetchDoc(slug) {
  const res = await fetch(BASE + '/docs/' + encodeURIComponent(slug))
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.text()
}

// ---------- environment status (step-140) ----------

/** Aggregate environment overview for the status page + health dot. */
export function fetchStatus() {
  return getJSON('/status')
}

// ---------- alerting (step-136) ----------

/** Full alert config (all fields, every time). */
export function fetchAlertConfig() {
  return getJSON('/alerts/config')
}

/** PATCHes the FULL config object; returns the stored config. */
export async function patchAlertConfig(cfg) {
  const res = await fetch(BASE + '/alerts/config', {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(cfg),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

/** Sends a test notification NOW (works with alerts disabled). */
export async function sendTestAlert() {
  const res = await fetch(BASE + '/alerts/test', { method: 'POST' })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
}

/** { queue_depth, last_delivery_at, last_delivery_err, incidents }. */
export function fetchAlertStatus() {
  return getJSON('/alerts/status')
}

/** Append-only alert log, newest first: { entries: [...] }. */
export function fetchAlertHistory(limit = 200) {
  return getJSON('/alerts/history?limit=' + limit)
}

/** Kicks off the local-ntfy install (sudoers helper). */
export async function startNtfyInstall() {
  const res = await fetch(BASE + '/alerts/install-ntfy', { method: 'POST' })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

/** ntfy install progress — { status: idle|running|ok|failed, output }. */
export function fetchNtfyInstall() {
  return getJSON('/alerts/install-ntfy')
}

// ---------- dashboard layout (step-126) ----------

/** { order: [section ids], collapsed: {id: true} } — global. */
export function fetchLayout() {
  return getJSON('/layout')
}

/** Persists the full layout; returns the server-normalized version. */
export async function patchLayout(layout) {
  const res = await fetch(BASE + '/layout', {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(layout),
  })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return res.json()
}

/** Restarts the daemon via the sudoers rule (~1s after resolving). */
export async function restartDaemon() {
  const res = await fetch(BASE + '/system/restart', { method: 'POST' })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
}

// ---------- resume-vs-new (step-111) ----------

/** History footprint for a target across all probes. */
export function fetchTargetStats(target) {
  return getJSON('/target_stats?target=' + encodeURIComponent(target))
}

/** "Start new": wipe a target's samples + route changes (annotations kept). */
export async function deleteTargetData(target) {
  const res = await fetch(BASE + '/target_data?target=' + encodeURIComponent(target), { method: 'DELETE' })
  if (!res.ok) {
    const msg = (await res.text()).trim() || `HTTP ${res.status}`
    throw new Error(msg)
  }
}
