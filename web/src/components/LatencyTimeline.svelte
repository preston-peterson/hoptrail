<script>
  import { onMount } from 'svelte'
  import { get } from 'svelte/store'
  import uPlot from 'uplot'
  import 'uplot/dist/uPlot.min.css'
  import { samplesStore, selectedTTL, pathStore, chartAnchor, timeWindow, TIME_WINDOWS, TIME_WINDOW_KEYS, activeTarget, activeTabId, tabThresholds, annotationsStore, refreshAnnotations, focusArea, focusWidth, FOCUS_WIDTHS, setChartAnchor, setTimeWindow, setFocusArea, activeProbeId, bandwidthSamples } from '../lib/stores.js'
  import { addAnnotation, deleteAnnotation } from '../lib/api.js'
  import { computeBands, BAND_COLORS } from '../lib/bands.js'
  import { DEFAULT_PRESET as DEFAULT_THRESHOLDS } from '../lib/thresholds.js'
  import ThresholdsPicker from './ThresholdsPicker.svelte'
  import FinalHopOnlyToggle from './FinalHopOnlyToggle.svelte'
  import { tick } from 'svelte'

  // Design (v0.1, last refined in step-19):
  //   - Chart shows ONE hop at a time, the one selected in the hop list.
  //     Default selection is the path's target_ttl (set in stores.js).
  //   - Y axis defaults to log scale. The log/linear toggle stays;
  //     within a single hop the values mostly fit a tight band, but
  //     occasional outliers still benefit from log scaling.
  //   - Timeout samples render as gaps in the line (null in the data
  //     array), not zero values — preserves "this hop didn't respond"
  //     vs "this hop responded in 0ms" semantics.
  //   - Hover tooltip shows the snapped timestamp and the single value
  //     at that point. Much simpler than the multi-hop tooltip from
  //     step-18 — when there's only one series, sort and color-key
  //     don't carry information.
  //
  // The previous "all hops overlaid" view is removed for now. It was
  // visually busy without clear diagnostic value. A future
  // "correlation view" feature could re-introduce a multi-hop chart
  // with a more deliberate design.

  let chartContainer
  let chart // uPlot instance or null
  let lastShapeHash = ''
  let palette = []
  let hasData = false

  let legendOpen = false
  function legendWindowClick(e) {
    if (legendOpen && !e.target.closest?.('.legend-root')) legendOpen = false
  }

  // 'log' | 'linear'. Persisted to localStorage so the user's
  // preference survives reloads.
  let scaleMode = 'log'

  // ---- Timeline scroll-back (step-35) ----
  //
  // Live mode (chartAnchor = null) shows the trailing window ending
  // at now. Past mode (chartAnchor = unix ms) shows a stable window
  // ending at that timestamp. Operators navigate via the ← / now / →
  // controls in this card's header — each ← / → click pans by half
  // the current window width (familiar from grafana-class tools).
  //
  // The reactive boolean drives the visual "past mode" treatment +
  // disables the forward button when there's nothing forward to go to.
  $: isPast = $chartAnchor != null
  $: windowMs = TIME_WINDOWS[$timeWindow]?.ms ?? TIME_WINDOWS['5m'].ms

  function panBack() {
    const half = windowMs / 2
    const base = $chartAnchor ?? Date.now()
    setChartAnchor(base - half)
  }

  function panForward() {
    if (!isPast) return
    const half = windowMs / 2
    const next = $chartAnchor + half
    // Snap back to live mode if the next anchor would land at or
    // past now (any pan-forward in live mode is meaningless).
    if (next >= Date.now()) {
      setChartAnchor(null)
    } else {
      setChartAnchor(next)
    }
  }

  function returnToNow() {
    setChartAnchor(null)
  }

  // Format the anchor as a short timestamp for the past-mode badge.
  function formatAnchor(ms) {
    if (ms == null) return ''
    const d = new Date(ms)
    return d.toLocaleTimeString([], {
      hour: '2-digit',
      minute: '2-digit',
      hour12: false,
    })
  }

  // Tooltip state — single value now, not a list.
  let tooltipVisible = false
  let tooltipX = 0
  let tooltipY = 0
  let tooltipTime = ''
  let tooltipValue = null // ms, or null for timeout

  // Outage band state (step-21).
  //
  // bands is recomputed inside updateChart (not as a $: reactive) so
  // that the new values are guaranteed to be set BEFORE setData is
  // called and the chart redraws. Svelte's reactive declarations run
  // after store subscribers, which would cause band updates to lag
  // by one tick relative to the data being plotted.
  //
  // Each band is { start, end, color } where start/end are timestamps
  // in milliseconds and color is 'red' | 'orange' | 'yellow'. The
  // bandsPlugin (registered in buildOpts) reads this array via closure
  // on every chart draw.
  let bands = []

  // Step-39: per-tab latency thresholds in ms — drives the warning/
  // critical horizontal reference lines on the chart. Read by the
  // thresholdsPlugin via closure on each redraw, mirroring bands'
  // pattern. Step-70: re-keyed from target to tab_id via
  // $tabThresholds, so two tabs of the same target can show different
  // threshold lines.
  let thresholdsMs = { warning: DEFAULT_THRESHOLDS.warning, critical: DEFAULT_THRESHOLDS.critical }
  $: {
    const pair = $activeTabId != null ? $tabThresholds[$activeTabId] : null
    thresholdsMs = {
      warning: pair?.warning_ms ?? DEFAULT_THRESHOLDS.warning,
      critical: pair?.critical_ms ?? DEFAULT_THRESHOLDS.critical,
    }
    // Nudge the chart to redraw so the new lines paint without
    // waiting for the next sample tick. uPlot has no public "redraw"
    // method — re-setting the current data is the cheap idiomatic
    // way to trigger drawClear hooks.
    if (chart) chart.redraw(false, true)
  }

  // Step-42: annotation markers mirror annotationsStore as a closure
  // var the chart plugin can read on every draw. Same indirection as
  // bands/thresholds — Svelte reactive declarations would lag chart
  // redraws otherwise.
  let chartAnnotations = []
  $: {
    chartAnnotations = $annotationsStore
    if (chart) chart.redraw(false, true)
  }

  // Step-43: focus area mirrored for the plugin closure. Same pattern.
  let chartFocus = null
  $: {
    chartFocus = $focusArea
    if (chart) chart.redraw(false, true)
  }

  // Inline "+ note" form state. Open by clicking the header button,
  // submit by Enter or the apply button, cancel by Esc or ×.
  // Anchors the new note at the chart's current temporal anchor:
  //   - live mode (chartAnchor === null) → Date.now()
  //   - past mode (chartAnchor is unix-ms) → that anchor
  // That's the same time semantics the rest of the chart uses, so
  // the operator's mental model of "where the note lands" matches
  // what they're looking at.
  let noteFormOpen = false
  let noteDraft = ''
  let notePending = false
  let noteError = null
  let noteInputEl

  async function openNoteForm() {
    if (!$activeTarget) return
    noteFormOpen = true
    noteDraft = ''
    noteError = null
    await tick()
    noteInputEl?.focus()
  }

  function cancelNote() {
    noteFormOpen = false
    noteError = null
    notePending = false
  }

  function onNoteKeydown(e) {
    if (e.key === 'Escape') {
      e.preventDefault()
      cancelNote()
    }
    // Enter is handled by the form's submit.
  }

  async function submitNote() {
    const text = noteDraft.trim()
    if (!text || !$activeTarget) return
    notePending = true
    noteError = null
    const ts = $chartAnchor ?? Date.now()
    try {
      const created = await addAnnotation($activeTarget, ts, text)
      // Optimistic local add so the marker paints without waiting
      // for the next 5s annotations-poll tick.
      annotationsStore.update((list) => [...list, created].sort((a, b) => a.ts - b.ts))
      noteFormOpen = false
      noteDraft = ''
    } catch (err) {
      noteError = err.message ?? String(err)
    } finally {
      notePending = false
    }
  }

  // Marker hover state: which annotation the cursor is currently
  // hovering over (read off chartAnnotations + annotationHitboxes).
  // Drives the small overlay that shows the note text + delete
  // button. mousemove handler does a coarse box hit-test.
  let hoveredAnnotation = null
  function onChartMouseMove(e) {
    if (!annotationHitboxes.length || !chartContainer) return
    const rect = chartContainer.getBoundingClientRect()
    const x = e.clientX - rect.left
    const y = e.clientY - rect.top
    let hit = null
    for (const h of annotationHitboxes) {
      if (Math.abs(h.x - x) < 8 && Math.abs(h.y - y) < 10) {
        hit = h
        break
      }
    }
    hoveredAnnotation = hit
  }
  function onChartMouseLeave() {
    hoveredAnnotation = null
  }

  // Focus-window width — now operator-tunable via the focusWidth
  // store (step-47), replacing step-43's hardcoded 60s. Read from
  // the reactive store at click time so a width change made while
  // focus is active is reflected on the very next dblclick.

  // ---- Step-44: chart brush + scroll ----
  //
  // Wheel and drag gestures on the chart for fluid navigation.
  // Complements step-35's discrete pan buttons and the discrete
  // TimeWindowPicker presets — operators reach for these when
  // panning/zooming "by hand" feels faster than clicking.
  //
  //   - Wheel up   → cycle to a smaller time window (zoom in)
  //   - Wheel down → cycle to a larger time window (zoom out)
  //   - Drag       → pan chartAnchor by the pixel-delta * ms/pixel.
  //                  Starts in past mode if currently live. 5px
  //                  dead-zone so accidental cursor twitches don't
  //                  trigger panning.
  const DRAG_DEAD_ZONE_PX = 5
  let dragState = null // null | { startX, startAnchor, msPerPx, moved }

  // Step-52: shift+drag on the chart brushes a focus range. Plain
  // drag stays as pan (step-44); the modifier key is the only
  // disambiguator. brushState tracks pixel coords for the live
  // preview overlay + timestamps for the eventual setFocusArea call.
  // chartLeftPx / chartWidthPx are the plot-area bounds in CSS pixels
  // (chart.bbox is in canvas pixels — DPR-corrected at capture time).
  let brushState = null // null | { startX, startTsMs, currentX, chartLeftPx, chartWidthPx, moved }

  function onChartWheel(e) {
    // Only intercept when the cursor is over the chart canvas; the
    // page should still scroll if the wheel event hits the chart
    // card's margins. uPlot's bbox is in canvas-pixel space, so
    // convert the event coords the same way.
    if (!chart || !chartContainer) return
    const rect = chartContainer.getBoundingClientRect()
    const xPx = e.clientX - rect.left
    const yPx = e.clientY - rect.top
    const bb = chart.bbox
    const dpr = chart.ctx.canvas.width / rect.width
    const xC = xPx * dpr
    const yC = yPx * dpr
    if (xC < bb.left || xC > bb.left + bb.width || yC < bb.top || yC > bb.top + bb.height) return
    e.preventDefault()
    const current = get(timeWindow)
    const idx = TIME_WINDOW_KEYS.indexOf(current)
    if (idx < 0) return
    // deltaY > 0 = scroll down = zoom out = larger window.
    const dir = e.deltaY > 0 ? 1 : -1
    const next = Math.max(0, Math.min(TIME_WINDOW_KEYS.length - 1, idx + dir))
    if (next !== idx) setTimeWindow(TIME_WINDOW_KEYS[next])
  }

  function onChartMouseDown(e) {
    // Left-button only — let right-click / middle-click pass through
    // to native browser behavior.
    if (e.button !== 0) return
    if (!chart || !chartContainer) return
    const rect = chartContainer.getBoundingClientRect()
    const xPx = e.clientX - rect.left
    const bb = chart.bbox
    const dpr = chart.ctx.canvas.width / rect.width
    const xC = xPx * dpr
    if (xC < bb.left || xC > bb.left + bb.width) return

    // Step-52: shift-held → brush gesture (sets focus range), plain
    // → pan gesture (step-44, moves chart anchor). Locked at mousedown
    // so releasing shift mid-drag doesn't switch modes.
    if (e.shiftKey) {
      const tsSec = chart.posToVal(xPx, 'x')
      if (tsSec == null || !Number.isFinite(tsSec)) return
      // Coords stored in container-local pixels so the preview
      // overlay can use them in `style="left: …px"` directly.
      brushState = {
        startX: xPx,
        startTsMs: Math.round(tsSec * 1000),
        currentX: xPx,
        chartLeftPx: bb.left / dpr,
        chartWidthPx: bb.width / dpr,
        chartTopPx: bb.top / dpr,
        chartHeightPx: bb.height / dpr,
        moved: false,
      }
      // Don't let the browser's text-selection cursor kick in during a brush.
      e.preventDefault()
      return
    }

    const widthMs = (TIME_WINDOWS[get(timeWindow)] ?? TIME_WINDOWS['5m']).ms
    const plotPxWidth = bb.width / dpr
    const msPerPx = widthMs / plotPxWidth
    // Capture starting anchor — null in live mode means "anchored at now",
    // so we snapshot Date.now() as the starting point.
    const startAnchor = get(chartAnchor) ?? Date.now()
    dragState = { startX: e.clientX, startAnchor, msPerPx, moved: false }
  }

  function onChartMouseMoveDrag(e) {
    if (brushState) {
      if (!chartContainer) return
      const rect = chartContainer.getBoundingClientRect()
      const xLocal = e.clientX - rect.left
      const dx = xLocal - brushState.startX
      if (!brushState.moved && Math.abs(dx) < DRAG_DEAD_ZONE_PX) return
      brushState.moved = true
      // Clamp to the plot area so the preview overlay never bleeds
      // into the axis margins.
      const clamped = Math.max(
        brushState.chartLeftPx,
        Math.min(brushState.chartLeftPx + brushState.chartWidthPx, xLocal),
      )
      // Step-53: derive the brush's current end-timestamp here so the
      // range labels can render reactively without poking chart.posToVal
      // from the template. posToVal is cheap; calling it once per
      // mousemove costs nothing vs the smoothness gain on the label.
      let currentTsMs = null
      if (chart) {
        const tsSec = chart.posToVal(clamped, 'x')
        if (tsSec != null && Number.isFinite(tsSec)) {
          currentTsMs = Math.round(tsSec * 1000)
        }
      }
      brushState = { ...brushState, currentX: clamped, currentTsMs }
      return
    }
    if (!dragState) return
    const dx = e.clientX - dragState.startX
    if (!dragState.moved && Math.abs(dx) < DRAG_DEAD_ZONE_PX) return
    dragState.moved = true
    // Dragging right → going back in time → decrease anchor.
    // Dragging left → going forward → increase anchor (capped at now).
    const newAnchor = Math.min(Date.now(), dragState.startAnchor - dx * dragState.msPerPx)
    setChartAnchor(newAnchor)
  }

  function onChartMouseUp() {
    if (brushState) {
      if (brushState.moved && chart) {
        // currentX is already container-local — same coord space
        // posToVal expects.
        const endTsSec = chart.posToVal(brushState.currentX, 'x')
        if (endTsSec != null && Number.isFinite(endTsSec)) {
          const endTsMs = Math.round(endTsSec * 1000)
          const since = Math.min(brushState.startTsMs, endTsMs)
          const until = Math.max(brushState.startTsMs, endTsMs)
          // Reject degenerate selections; setFocusArea with since==until
          // would zero-out the focus window and confuse loss attribution.
          if (until - since >= 1000) {
            setFocusArea({ since, until })
          }
        }
      }
      brushState = null
      return
    }
    if (!dragState) return
    // If the drag never moved past the dead-zone, treat it as a
    // click and don't disturb the anchor — clears any spurious set
    // that mousedown might have done (we only set on actual move).
    // If we did move, the anchor is already updated; just release.
    // Special case: if drag was at the right edge (anchor === now()),
    // return to live mode by clearing chartAnchor.
    if (dragState.moved) {
      const anchor = get(chartAnchor)
      if (anchor != null && anchor >= Date.now() - 1000) {
        setChartAnchor(null)
      }
    }
    dragState = null
  }

  // Double-click on the chart sets focus to a window centered on
  // the clicked timestamp. Uses uPlot's posToVal to convert pixel
  // X into a timestamp in seconds (uPlot's time axis is seconds, so
  // we ×1000 to get unix-ms for the rest of the code).
  function onChartDblClick(e) {
    if (!chart || !chartContainer) return
    const rect = chartContainer.getBoundingClientRect()
    const xPx = e.clientX - rect.left
    // Only react to clicks inside the plot area, not in the axis margins.
    const bbox = chart.bbox
    const xCanvas = xPx * (chart.ctx.canvas.width / rect.width) // account for device pixel ratio
    if (xCanvas < bbox.left || xCanvas > bbox.left + bbox.width) return
    const tsSec = chart.posToVal(xPx, 'x')
    if (tsSec == null || !Number.isFinite(tsSec)) return
    const tsMs = Math.round(tsSec * 1000)
    setFocusArea({
      since: tsMs - $focusWidth / 2,
      until: tsMs + $focusWidth / 2,
    })
  }

  function clearFocus() {
    setFocusArea(null)
  }

  // Step-49: right-click anywhere on the chart pins a note at the
  // cursor's exact timestamp, bypassing step-42's "must scroll-back
  // the anchor first" two-step. The cursor location wins over the
  // chart anchor — operators reach for this when reviewing an
  // outage and want to mark "this is when the modem rebooted"
  // without first navigating their reference point to that spot.
  //
  // Opens a small inline popover at the click coords with a text
  // input. Browser context-menu suppressed. Esc / × cancels, Enter
  // submits at the captured timestamp (not Date.now() — we snapshot
  // ts at click time so a slow typist still pins to the moment they
  // clicked, not the moment they hit Enter).
  let contextNote = null // null | { ts, x, y, draft, pending, error }
  let contextNoteInputEl
  let contextNoteFormEl
  function onWindowMouseDownForContextNote(e) {
    if (!contextNote) return
    if (contextNoteFormEl && contextNoteFormEl.contains(e.target)) return
    contextNote = null
  }
  async function onChartContextMenu(e) {
    if (!chart || !chartContainer || !$activeTarget) return
    const rect = chartContainer.getBoundingClientRect()
    const xPx = e.clientX - rect.left
    const yPx = e.clientY - rect.top
    const bbox = chart.bbox
    const dpr = chart.ctx.canvas.width / rect.width
    const xCanvas = xPx * dpr
    const yCanvas = yPx * dpr
    if (xCanvas < bbox.left || xCanvas > bbox.left + bbox.width) return
    if (yCanvas < bbox.top || yCanvas > bbox.top + bbox.height) return
    const tsSec = chart.posToVal(xPx, 'x')
    if (tsSec == null || !Number.isFinite(tsSec)) return
    e.preventDefault()
    contextNote = {
      ts: Math.round(tsSec * 1000),
      x: xPx,
      y: yPx,
      draft: '',
      pending: false,
      error: null,
    }
    await tick()
    contextNoteInputEl?.focus()
  }

  function cancelContextNote() {
    contextNote = null
  }

  function onContextNoteKeydown(e) {
    if (e.key === 'Escape') {
      e.preventDefault()
      cancelContextNote()
    }
  }

  async function submitContextNote() {
    if (!contextNote) return
    const text = contextNote.draft.trim()
    if (!text || !$activeTarget) return
    contextNote.pending = true
    contextNote.error = null
    contextNote = contextNote // trigger reactivity on nested mutation
    const ts = contextNote.ts
    try {
      const created = await addAnnotation($activeTarget, ts, text)
      annotationsStore.update((list) => [...list, created].sort((a, b) => a.ts - b.ts))
      contextNote = null
    } catch (err) {
      contextNote.error = err.message ?? String(err)
      contextNote.pending = false
      contextNote = contextNote
    }
  }

  // Step-47: when the operator picks a new focus width while focus
  // is active, re-center the window on the existing midpoint with
  // the new width. Keeps "where am I looking" stable while changing
  // "how wide am I looking." Width sticks for future dblclicks too
  // since focusWidth is persisted globally.
  function onFocusWidthPick(e) {
    const newWidth = Number(e.target.value)
    if (!Number.isFinite(newWidth) || newWidth <= 0) return
    focusWidth.set(newWidth)
    const fa = $focusArea
    if (fa) {
      const center = (fa.since + fa.until) / 2
      setFocusArea({ since: center - newWidth / 2, until: center + newWidth / 2 })
    }
  }

  // Step-45: trigger a JSON-bundle download for the active target's
  // current chart window. Lets the operator's browser handle the
  // download via Content-Disposition (set server-side). We construct
  // the URL with the visible-window since/until so what gets exported
  // matches what they see — focus area is intentionally NOT used
  // here; focus is about which stats are displayed, not which data
  // is in scope.
  function exportView() {
    const t = $activeTarget
    if (!t) return
    const cfg = TIME_WINDOWS[$timeWindow] ?? TIME_WINDOWS['5m']
    // Step-57: round at the wire-edge (lesson #13). Integer-strict
    // server-side parser; pre-step-56 fractional anchors would 400.
    const until = Math.round($chartAnchor ?? Date.now())
    const since = until - cfg.ms
    let url = '/api/export?target=' + encodeURIComponent(t) +
      '&since=' + since + '&until=' + until
    // Step-94: export what the operator is looking at — including
    // which probe's measurements. Omitted for local (v0.2 URL shape).
    if ($activeProbeId && $activeProbeId !== 'local') {
      url += '&probe_id=' + encodeURIComponent($activeProbeId)
    }
    // window.location.href would navigate; using a temporary <a>
    // with download attribute triggers the download without losing
    // the SPA's current state.
    const a = document.createElement('a')
    a.href = url
    a.style.display = 'none'
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
  }

  // Format helper for the focus badge — operator-readable local time.
  function formatFocusCenter(focus) {
    if (!focus) return ''
    const center = new Date((focus.since + focus.until) / 2)
    return center.toLocaleTimeString()
  }

  async function deleteHoveredAnnotation() {
    if (!hoveredAnnotation) return
    const id = hoveredAnnotation.id
    // Optimistic remove.
    annotationsStore.update((list) => list.filter((a) => a.id !== id))
    hoveredAnnotation = null
    try {
      await deleteAnnotation(id)
    } catch (err) {
      console.error('deleteAnnotation failed', err)
      // Re-sync from server on failure.
      refreshAnnotations()
    }
  }

  // pathHopsSnapshot mirrors $pathStore.hops imperatively so updateChart
  // can access the current hop set without needing to subscribe to the
  // store inside it. Updated reactively from pathStore.
  let pathHopsSnapshot = []
  $: pathHopsSnapshot = $pathStore?.hops ?? []

  // Current selection drives both the data filter and the header text.
  // Re-derive whenever either store changes.
  $: currentTTL = $selectedTTL
  $: currentHop = $pathStore?.hops?.find((h) => h.ttl === currentTTL)
  $: currentLabel = currentHop
    ? `hop ${currentTTL} · ${currentHop.hostname || currentHop.current_ip || '*'}`
    : currentTTL != null
      ? `hop ${currentTTL}`
      : 'select a hop'
  $: currentColor = currentTTL != null
    ? `var(--hop-${((currentTTL - 1) % 10) + 1})`
    : 'var(--fg-subtle)'

  // Reactive rebuild trigger: changing the selected TTL means the
  // chart's series identity changes (label, color, data), which uPlot
  // needs to be rebuilt for. updateChart handles this via the shape
  // hash, but we have to call it when selectedTTL changes.
  $: if (currentTTL != null) {
    let current = []
    samplesStore.subscribe((v) => (current = v))()
    updateChart(current)
  }

  onMount(() => {
    palette = readPalette()

    const saved = localStorage.getItem('hoptrail-scale')
    if (saved === 'log' || saved === 'linear') {
      scaleMode = saved
    }

    const ro = new ResizeObserver(handleResize)
    ro.observe(chartContainer)

    const unsubscribe = samplesStore.subscribe(updateChart)

    return () => {
      ro.disconnect()
      unsubscribe()
      if (chart) {
        chart.destroy()
        chart = null
      }
    }
  })

  function readPalette() {
    const style = getComputedStyle(document.documentElement)
    const colors = []
    for (let i = 1; i <= 10; i++) {
      const raw = style.getPropertyValue(`--hop-${i}`).trim()
      colors.push(raw || '#888')
    }
    return colors
  }

  function handleResize() {
    if (chart && chartContainer) {
      chart.setSize({
        width: chartContainer.clientWidth,
        height: chartContainer.clientHeight,
      })
    }
  }

  function toggleScale() {
    scaleMode = scaleMode === 'log' ? 'linear' : 'log'
    localStorage.setItem('hoptrail-scale', scaleMode)
    lastShapeHash = ''
    if (chart) {
      chart.destroy()
      chart = null
    }
    tooltipVisible = false
    let current = []
    samplesStore.subscribe((v) => (current = v))()
    updateChart(current)
  }

  function updateChart(samples) {
    // No selection yet → no chart. The placeholder handles this case.
    if (currentTTL == null) {
      hasData = false
      tooltipVisible = false
      if (chart) {
        chart.destroy()
        chart = null
        lastShapeHash = ''
      }
      return
    }

    if (!samples || !samples.length) {
      hasData = false
      tooltipVisible = false
      if (chart) {
        chart.destroy()
        chart = null
        lastShapeHash = ''
      }
      return
    }

    const shaped = reshape(samples, currentTTL)
    if (!shaped || shaped.data[0].length === 0) {
      // Selected TTL has no samples in the window — empty chart.
      hasData = false
      tooltipVisible = false
      if (chart) {
        chart.destroy()
        chart = null
        lastShapeHash = ''
      }
      return
    }
    hasData = true

    // Recompute bands BEFORE the chart redraws so the plugin (which
    // reads `bands` via closure on each draw) sees current values.
    // See the comment on `bands` declaration for why this isn't a
    // $: reactive — Svelte's update order would make it lag by one
    // tick relative to the subscribe-driven chart redraw.
    bands = computeBands(samples, currentTTL, pathHopsSnapshot)

    // Shape hash includes selectedTTL and scaleMode. A change in either
    // triggers a chart rebuild because uPlot can't reconfigure series
    // or axes at runtime.
    const shapeHash = `ttl:${currentTTL}:${scaleMode}`
    if (!chart || shapeHash !== lastShapeHash) {
      if (chart) chart.destroy()
      tooltipVisible = false
      chart = new uPlot(buildOpts(currentTTL), shaped.data, chartContainer)
      lastShapeHash = shapeHash
    } else {
      chart.setData(shaped.data)
    }
  }

  function formatMs(ms) {
    if (ms == null) return '—'
    if (ms < 1) return ms.toFixed(2) + ' ms'
    if (ms < 100) return ms.toFixed(1) + ' ms'
    return Math.round(ms) + ' ms'
  }

  function formatTime(ms) {
    const d = new Date(ms)
    return d.toLocaleTimeString([], {
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hour12: false,
    })
  }

  // Step-53: human-readable elapsed duration for the brush's center
  // label. Picks the largest unit that keeps the number short.
  function formatDuration(ms) {
    if (ms < 60_000) return `${Math.round(ms / 1000)}s`
    if (ms < 3_600_000) {
      const m = Math.floor(ms / 60_000)
      const s = Math.round((ms % 60_000) / 1000)
      return s === 0 ? `${m}m` : `${m}m ${s}s`
    }
    const h = Math.floor(ms / 3_600_000)
    const m = Math.round((ms % 3_600_000) / 60_000)
    return m === 0 ? `${h}h` : `${h}h ${m}m`
  }

  // Step-59: position the tooltip ABOVE the cursor when possible.
  // The default arrow cursor extends down-right from its hotspot, so
  // a cursor+14,+14 offset placed the cursor's visual body right on
  // top of the tooltip's timestamp line (top-left of the box). Above-
  // positioning clears the cursor entirely. tooltipAbove drives a CSS
  // transform on the rendered overlay so we don't need to measure
  // the rendered height in JS — let layout do it. Fall back to below
  // when the cursor is near the chart's top edge.
  const TOOLTIP_OFFSET_PX = 14
  const TOOLTIP_ESTIMATED_HEIGHT_PX = 56 // ~ two lines + padding; conservative
  let tooltipAbove = true

  function makeUpdateTooltip() {
    return (u) => {
      const idx = u.cursor.idx
      if (idx == null) {
        tooltipVisible = false
        return
      }

      const chartW = chartContainer.clientWidth
      const left = u.cursor.left
      const top = u.cursor.top
      const tooltipW = 140 // matches min-width below
      const flipLeft = left + tooltipW + 20 > chartW
      tooltipX = flipLeft ? left - tooltipW - TOOLTIP_OFFSET_PX : left + TOOLTIP_OFFSET_PX
      // Default: position above. If there isn't room above (cursor
      // near the top of the chart), fall back to below.
      const roomAbove = top - TOOLTIP_OFFSET_PX - TOOLTIP_ESTIMATED_HEIGHT_PX > 0
      tooltipAbove = roomAbove
      tooltipY = roomAbove ? top - TOOLTIP_OFFSET_PX : top + TOOLTIP_OFFSET_PX + 8 // extra pad on fallback to clear the cursor

      const ts = u.data[0][idx] * 1000
      tooltipTime = formatTime(ts)
      tooltipValue = u.data[1][idx] // single series → index 1
      tooltipVisible = true
    }
  }

  function buildOpts(ttl) {
    const isLog = scaleMode === 'log'
    const colorIdx = (ttl - 1) % palette.length
    const color = palette[colorIdx] || '#888'
    return {
      width: chartContainer.clientWidth,
      height: chartContainer.clientHeight,
      scales: {
        x: { time: true },
        y: isLog
          ? { distr: 3, log: 10 }
          : { auto: true },
      },
      axes: [
        { stroke: '#888', grid: { stroke: 'rgba(136,136,136,0.15)' } },
        {
          stroke: '#888',
          label: isLog ? 'ms (log)' : 'ms',
          grid: { stroke: 'rgba(136,136,136,0.15)' },
        },
      ],
      legend: { show: false },
      cursor: { focus: { prox: 30 } },
      hooks: {
        setCursor: [makeUpdateTooltip()],
      },
      plugins: [bandsPlugin(), speedtestPlugin(), thresholdsPlugin(), annotationsPlugin(), focusPlugin()],
      series: [
        {}, // x-axis sentinel
        {
          label: `TTL ${ttl}`,
          stroke: color,
          width: 1.6, // slightly thicker than the multi-hop view since it's the only line
          points: { show: false },
          spanGaps: false,
        },
      ],
    }
  }

  // reshape filters samples to just the selected TTL and builds the
  // single-series uPlot data shape [xValues, yValues].
  //
  // Pre-step-19 this function built one Y series per TTL — but with
  // the chart now scoped to a single hop, the work simplifies down
  // to a single pass collecting timestamps and values for one TTL.
  function reshape(samples, ttl) {
    const xValues = []
    const yValues = []
    for (const s of samples) {
      if (s.ttl !== ttl) continue
      xValues.push(s.ts / 1000) // uPlot wants seconds for time-scale X
      yValues.push(s.ip === null ? null : s.rtt_ms)
    }
    return { data: [xValues, yValues] }
  }

  // Outage bands — the classifier itself lives in lib/bands.js so
  // both the main chart and the per-hop sparklines render the same
  // attribution-respecting bands (step-38 extraction).
  //
  // The uPlot plugin below paints the band rectangles behind the
  // data line. The closure reads the `bands` Svelte reactive variable
  // at draw time, so any reactive update flows through on the next
  // chart redraw without needing a uPlot rebuild.
  //
  // drawClear fires after the canvas is cleared but before series
  // are drawn, so the bands sit behind the line but in front of
  // nothing (they end up behind axes and gridlines too, which is
  // fine — both are subtle).
  function bandsPlugin() {
    return {
      hooks: {
        drawClear: [
          (u) => {
            if (!bands.length) return
            const ctx = u.ctx
            const yTop = u.bbox.top
            const yBot = u.bbox.top + u.bbox.height
            ctx.save()
            for (const band of bands) {
              const x1 = u.valToPos(band.start / 1000, 'x', true)
              const x2 = u.valToPos(band.end / 1000, 'x', true)
              // Minimum 2px width so single-sample bands stay visible;
              // valToPos can return a sub-pixel width when start≈end.
              const w = Math.max(2, x2 - x1)
              ctx.fillStyle = BAND_COLORS[band.color] || 'rgba(0,0,0,0)'
              ctx.fillRect(x1, yTop, w, yBot - yTop)
            }
            ctx.restore()
          },
        ],
      },
    }
  }

  // Speedtest-window markers (step-105, operator request): a subtle
  // accent band over the span of each bandwidth test, so latency
  // artifacts during a test explain themselves — locally as the
  // ICMP-pause gap, and on remote-probe tabs as the real bufferbloat
  // the saturated shared egress causes. Reads the already-polled
  // bandwidthSamples; the chart redraws every poll tick so plain
  // closure reads stay fresh.
  function speedtestPlugin() {
    return {
      hooks: {
        drawClear: [
          (u) => {
            const tests = $bandwidthSamples
            if (!tests?.length) return
            const ctx = u.ctx
            const yTop = u.bbox.top
            const yBot = u.bbox.top + u.bbox.height
            const accent = getComputedStyle(document.documentElement).getPropertyValue('--accent').trim() || '#4ea1ff'
            ctx.save()
            for (const t of tests) {
              const startS = t.ts / 1000
              const endS = (t.ts + Math.max(t.duration_ms ?? 45_000, 10_000)) / 1000
              const x1 = u.valToPos(startS, 'x', true)
              const x2 = u.valToPos(endS, 'x', true)
              if (x2 < u.bbox.left || x1 > u.bbox.left + u.bbox.width) continue
              ctx.fillStyle = accent
              ctx.globalAlpha = 0.08
              ctx.fillRect(x1, yTop, Math.max(2, x2 - x1), yBot - yTop)
              ctx.globalAlpha = 0.35
              ctx.fillRect(x1, yTop, 1, yBot - yTop)
            }
            ctx.restore()
          },
        ],
      },
    }
  }

  // Threshold reference lines (step-39). Two horizontal dashed lines
  // at the operator's warning (yellow) and critical (red) latencies,
  // giving an at-a-glance reference for what counts as bad RTT *for
  // this connection class* without forcing the operator to read the
  // exact Y-axis value. Dashed-and-low-contrast so they don't compete
  // with the data line; positioned at the Y-axis values via valToPos
  // so log/linear scale just works.
  //
  // Painted in `draw` (after series) rather than `drawClear` (before)
  // so the line sits on top of the data line — otherwise the trace
  // would obscure the threshold marker exactly when latency is at
  // the boundary, which is the most interesting case to surface.
  function thresholdsPlugin() {
    return {
      hooks: {
        draw: [
          (u) => {
            const ctx = u.ctx
            const xLeft = u.bbox.left
            const xRight = u.bbox.left + u.bbox.width
            ctx.save()
            ctx.setLineDash([4, 4])
            ctx.lineWidth = 1
            // Warning (yellow)
            const yWarn = u.valToPos(thresholdsMs.warning, 'y', true)
            if (yWarn >= u.bbox.top && yWarn <= u.bbox.top + u.bbox.height) {
              ctx.strokeStyle = 'rgba(234, 179, 8, 0.55)'
              ctx.beginPath()
              ctx.moveTo(xLeft, yWarn)
              ctx.lineTo(xRight, yWarn)
              ctx.stroke()
            }
            // Critical (red)
            const yCrit = u.valToPos(thresholdsMs.critical, 'y', true)
            if (yCrit >= u.bbox.top && yCrit <= u.bbox.top + u.bbox.height) {
              ctx.strokeStyle = 'rgba(239, 68, 68, 0.55)'
              ctx.beginPath()
              ctx.moveTo(xLeft, yCrit)
              ctx.lineTo(xRight, yCrit)
              ctx.stroke()
            }
            ctx.restore()
          },
        ],
      },
    }
  }

  // Step-43 focus-area overlay. Translucent blue rectangle from
  // chartFocus.since to chartFocus.until — visually pairs with the
  // "focused" badge in the HopList header so the operator can see
  // at a glance where the per-hop stats are being computed from.
  // Painted in drawClear so it sits behind the data line (and behind
  // the outage bands too — focus is meta-context about the view).
  function focusPlugin() {
    return {
      hooks: {
        drawClear: [
          (u) => {
            if (!chartFocus) return
            const ctx = u.ctx
            const yTop = u.bbox.top
            const yBot = u.bbox.top + u.bbox.height
            const x1 = u.valToPos(chartFocus.since / 1000, 'x', true)
            const x2 = u.valToPos(chartFocus.until / 1000, 'x', true)
            const w = Math.max(2, x2 - x1)
            ctx.save()
            ctx.fillStyle = 'rgba(109, 180, 255, 0.12)' // matches --accent
            ctx.fillRect(x1, yTop, w, yBot - yTop)
            // Edge guides at both bounds so the boundaries are
            // unambiguous even on top of busy data.
            ctx.strokeStyle = 'rgba(109, 180, 255, 0.55)'
            ctx.lineWidth = 1
            ctx.beginPath()
            ctx.moveTo(x1, yTop); ctx.lineTo(x1, yBot)
            ctx.moveTo(x2, yTop); ctx.lineTo(x2, yBot)
            ctx.stroke()
            ctx.restore()
          },
        ],
      },
    }
  }

  // Step-42 annotation markers. Paints a small ▲ at each note's
  // timestamp along the bottom of the plot area, plus a faint
  // vertical guide line so the operator can read off which sample
  // the note was pinned to even at zoomed-out scales. The mouseup
  // hit-test stores the ▲ position so DOM hovers (handled outside
  // the chart) can show the note text and a delete affordance.
  let annotationHitboxes = []
  function annotationsPlugin() {
    return {
      hooks: {
        draw: [
          (u) => {
            if (!chartAnnotations.length) return
            const ctx = u.ctx
            const yTop = u.bbox.top
            const yBot = u.bbox.top + u.bbox.height
            const xMin = u.scales.x.min
            const xMax = u.scales.x.max
            const hits = []
            ctx.save()
            for (const a of chartAnnotations) {
              const ts = a.ts / 1000 // uPlot X is in seconds
              if (ts < xMin || ts > xMax) continue
              const x = u.valToPos(ts, 'x', true)
              // Faint guide line behind the data — same alpha as
              // the threshold lines so it reads as supporting context.
              ctx.strokeStyle = 'rgba(140, 140, 160, 0.35)'
              ctx.setLineDash([2, 3])
              ctx.lineWidth = 1
              ctx.beginPath()
              ctx.moveTo(x, yTop)
              ctx.lineTo(x, yBot - 8)
              ctx.stroke()
              ctx.setLineDash([])
              // Triangle marker just inside the bottom edge.
              const tipY = yBot - 1
              ctx.fillStyle = 'rgba(220, 220, 240, 0.95)'
              ctx.beginPath()
              ctx.moveTo(x, tipY - 8)
              ctx.lineTo(x - 5, tipY)
              ctx.lineTo(x + 5, tipY)
              ctx.closePath()
              ctx.fill()
              ctx.strokeStyle = 'rgba(40, 44, 52, 0.9)'
              ctx.lineWidth = 1
              ctx.stroke()
              hits.push({ id: a.id, text: a.text, ts: a.ts, x, y: tipY - 4 })
            }
            ctx.restore()
            annotationHitboxes = hits
          },
        ],
      },
    }
  }
</script>

<!-- Window-level mouseup so a drag started on the chart ends even
     if the operator releases the button outside the chart area. -->
<svelte:window on:click={legendWindowClick} on:mouseup={onChartMouseUp} on:mousedown|capture={onWindowMouseDownForContextNote} />

<section class="timeline">
  <header class="card-header">
    <h2>
      Latency
      <span class="hop-label" style="--label-color: {currentColor};">
        — {currentLabel}
      </span>
    </h2>
    <div class="header-right">
      <!-- Timeline navigation. ← / → pan by half-window strides;
           "now" returns to live mode. The →-button is disabled when
           we're already at now (live mode). The past-mode badge
           shows the anchor's local time so the operator knows where
           in history they're looking. -->
      <div class="nav" class:active={isPast}>
        <button class="nav-btn" on:click={panBack} title="back {Math.round(windowMs / 60000 / 2)} min">←</button>
        <button
          class="nav-btn now"
          class:dim={!isPast}
          on:click={returnToNow}
          title={isPast ? 'return to live' : 'currently live'}
          disabled={!isPast}
        >now</button>
        <button class="nav-btn" on:click={panForward} disabled={!isPast} title="forward {Math.round(windowMs / 60000 / 2)} min">→</button>
        {#if isPast}
          <span class="past-badge" title="chart anchored at {formatAnchor($chartAnchor)}">
            {formatAnchor($chartAnchor)}
          </span>
        {/if}
      </div>
      <!-- Step-43 focus badge + step-47 width picker. Badge only
           shows when focus is active. Width dropdown re-centers the
           focus on the existing midpoint so changing width doesn't
           jump the operator's reference point. Width persists
           globally so subsequent dblclicks use the same width. -->
      {#if $focusArea}
        <span class="focus-area-badge" title="trace-grid stats computed from the highlighted window — dblclick to recenter, shift+drag to brush a custom range">
          focus: {formatFocusCenter($focusArea)}
          <select class="focus-width" bind:value={$focusWidth} on:change={onFocusWidthPick} title="focus window width">
            {#each FOCUS_WIDTHS as w (w.ms)}
              <option value={w.ms}>{w.label}</option>
            {/each}
          </select>
          <button class="focus-clear" on:click={clearFocus} title="clear focus (return stats to live)">×</button>
        </span>
      {/if}
      <ThresholdsPicker />
      <FinalHopOnlyToggle />
      <!-- Note-add affordance. Click to open inline form; submits
           a note at the chart's current anchor (now in live mode,
           the focused timestamp in past mode). For pinning a note
           at a specific past moment without moving the anchor,
           right-click on the chart itself (step-49). -->
      {#if !noteFormOpen}
        <button
          class="note-add"
          on:click={openNoteForm}
          disabled={!$activeTarget}
          title="add a note at this point on the timeline"
        >+ note</button>
      {:else}
        <form class="note-form" on:submit|preventDefault={submitNote}>
          <input
            bind:this={noteInputEl}
            bind:value={noteDraft}
            on:keydown={onNoteKeydown}
            placeholder="note (e.g. router reboot)"
            maxlength="280"
            spellcheck="false"
            disabled={notePending}
            aria-label="annotation text"
          />
          <button type="submit" class="note-apply" disabled={notePending || !noteDraft.trim()} title="save (Enter)">
            {notePending ? '…' : 'add'}
          </button>
          <button type="button" class="note-cancel" on:click={cancelNote} disabled={notePending} title="cancel (Esc)">×</button>
          {#if noteError}
            <span class="note-error" title={noteError}>{noteError}</span>
          {/if}
        </form>
      {/if}
      <!-- Step-116 (operator feedback: "I don't know what the
           different colors mean"): chart legend popover. Same
           collapsed-trigger pattern as the pickers. -->
      <div class="legend-root">
        <button class="scale-toggle" class:open={legendOpen}
                on:click|stopPropagation={() => (legendOpen = !legendOpen)}
                title="what the chart's colors and marks mean">?</button>
        {#if legendOpen}
          <div class="legend-menu" role="note" aria-label="chart legend">
            <div class="lg-row"><span class="lg-line" style="background: var(--hop-color, var(--accent))"></span>
              selected hop's round-trip time (color matches its row in the hop list)</div>
            <div class="lg-row"><span class="lg-band" style="background: rgba(234,179,8,0.5)"></span>
              ≥20% packet loss in this span (confirmed downstream — real loss, not ICMP rate-limiting)</div>
            <div class="lg-row"><span class="lg-band" style="background: rgba(249,115,22,0.55)"></span>
              ≥40% packet loss</div>
            <div class="lg-row"><span class="lg-band" style="background: rgba(239,68,68,0.55)"></span>
              ≥70% packet loss — outage-grade</div>
            <div class="lg-row"><span class="lg-dash" style="border-color: var(--warn, #eab308)"></span>
              warning latency threshold (the "band" preset for this tab)</div>
            <div class="lg-row"><span class="lg-dash" style="border-color: var(--danger, #ef4444)"></span>
              critical latency threshold</div>
            <div class="lg-row"><span class="lg-band" style="background: color-mix(in srgb, var(--accent) 25%, transparent)"></span>
              a bandwidth speed test was running — latency artifacts in this span are expected</div>
            <div class="lg-row"><span class="lg-mark">▲</span>
              your annotation — hover it for the note</div>
            <div class="lg-row"><span class="lg-band" style="background: color-mix(in srgb, var(--accent) 18%, transparent)"></span>
              focus area — the hop list's stats are computed from this span</div>
          </div>
        {/if}
      </div>
      <button
        class="scale-toggle"
        on:click={toggleScale}
        title="toggle Y axis: {scaleMode === 'log' ? 'log → linear' : 'linear → log'}"
      >
        {scaleMode === 'log' ? 'log' : 'linear'}
      </button>
      <!-- Step-45: export the current view's data as a JSON bundle.
           Window matches what the chart is showing (since/until
           derived from chartAnchor + timeWindow); target = active tab.
           Browser handles the download via Content-Disposition. -->
      <button
        class="export-btn"
        on:click={exportView}
        disabled={!$activeTarget}
        title="download samples + path + annotations for the visible window as a JSON bundle"
      >↓ export</button>
      <div class="meta">
        {#if $samplesStore.length}
          {$samplesStore.length} samples
        {:else}
          no data yet
        {/if}
      </div>
    </div>
  </header>
  <div
    class="chart-area"
    on:mousemove={(e) => { onChartMouseMove(e); onChartMouseMoveDrag(e); }}
    on:mouseleave={onChartMouseLeave}
    on:mousedown={onChartMouseDown}
    on:mouseup={onChartMouseUp}
    on:wheel|nonpassive={onChartWheel}
    on:dblclick={onChartDblClick}
    on:contextmenu={onChartContextMenu}
    role="presentation"
  >
    <div class="canvas-mount" bind:this={chartContainer}></div>
    {#if isPast}
      <!-- Step-58: prominent past-mode banner. The small "17:17" badge
           in the header isn't enough — operators twice mistook a tab
           in past mode for a stuck tab and didn't realize they'd
           accidentally entered it via a stray drag-pan. Banner sits
           inside the chart-area at the top so it's impossible to
           miss when looking at the chart, doubles as a click-to-clear
           affordance (same effect as clicking "now"), and the accent
           color makes the state visually distinct from live mode. -->
      <button
        type="button"
        class="past-banner"
        on:click={returnToNow}
        title="click to return to live"
      >
        <span class="past-banner-icon" aria-hidden="true">⏱</span>
        <span class="past-banner-text">
          Viewing history at <strong>{formatAnchor($chartAnchor)}</strong>
        </span>
        <span class="past-banner-cta">click to return to live</span>
      </button>
    {/if}
    {#if !hasData}
      <div class="placeholder">
        {#if currentTTL == null}
          select a hop from the list below
        {:else}
          waiting for samples…
        {/if}
      </div>
    {/if}
    {#if brushState && brushState.moved}
      <!-- Step-52: live brush preview while shift+drag is in progress.
           Translucent blue rect over the plot area showing the
           proposed focus range. On mouseup, this hands off to the
           focusPlugin which paints the committed focus rect. -->
      <div
        class="brush-preview"
        style="
          left: {Math.min(brushState.startX, brushState.currentX)}px;
          top: {brushState.chartTopPx}px;
          width: {Math.abs(brushState.currentX - brushState.startX)}px;
          height: {brushState.chartHeightPx}px;
        "
      ></div>
      <!-- Step-53: range labels above the brush rect. The cursor sits
           on the right-edge of the brush during the drag, so the
           regular hover tooltip got clobbered (cursor-over-text).
           Two anchored labels at the brush's left + right edges plus
           a center duration read read cleanly because the cursor only
           covers the right one (and the operator can read the left and
           center). Labels render above the plot area so they don't
           themselves get cursored over. -->
      {@const brushLeft = Math.min(brushState.startX, brushState.currentX)}
      {@const brushRight = Math.max(brushState.startX, brushState.currentX)}
      {@const brushEndTs = brushState.currentTsMs ?? brushState.startTsMs}
      {@const brushSince = Math.min(brushState.startTsMs, brushEndTs)}
      {@const brushUntil = Math.max(brushState.startTsMs, brushEndTs)}
      <div
        class="brush-label brush-label-left"
        style="left: {brushLeft}px; top: {brushState.chartTopPx - 22}px"
      >{formatTime(brushSince)}</div>
      <div
        class="brush-label brush-label-right"
        style="left: {brushRight}px; top: {brushState.chartTopPx - 22}px"
      >{formatTime(brushUntil)}</div>
      {#if brushRight - brushLeft > 70}
        <div
          class="brush-label brush-label-center"
          style="left: {(brushLeft + brushRight) / 2}px; top: {brushState.chartTopPx - 22}px"
        >{formatDuration(brushUntil - brushSince)}</div>
      {/if}
    {/if}
    <div
      class="tooltip"
      class:visible={tooltipVisible && !(brushState && brushState.moved)}
      class:above={tooltipAbove}
      style="left: {tooltipX}px; top: {tooltipY}px"
    >
      <div class="tooltip-time">{tooltipTime}</div>
      <div class="tooltip-value">{formatMs(tooltipValue)}</div>
    </div>
    {#if hoveredAnnotation}
      <div
        class="annotation-popover"
        style="left: {hoveredAnnotation.x}px; top: {hoveredAnnotation.y - 60}px"
      >
        <div class="annotation-text">{hoveredAnnotation.text}</div>
        <div class="annotation-meta">
          <span class="annotation-time">{new Date(hoveredAnnotation.ts).toLocaleString()}</span>
          <button class="annotation-delete" on:click={deleteHoveredAnnotation} title="delete this note">×</button>
        </div>
      </div>
    {/if}
    {#if contextNote}
      <!-- Step-49: right-click pin-at-cursor note popover. Anchors at
           click coords, shifted up so the input doesn't sit under the
           cursor. submit pins at contextNote.ts (the timestamp under
           the cursor when right-click landed). -->
      <form
        class="context-note"
        bind:this={contextNoteFormEl}
        style="left: {contextNote.x}px; top: {contextNote.y + 8}px"
        on:submit|preventDefault={submitContextNote}
      >
        <div class="context-note-time">
          {new Date(contextNote.ts).toLocaleString()}
        </div>
        <div class="context-note-row">
          <input
            bind:this={contextNoteInputEl}
            bind:value={contextNote.draft}
            on:keydown={onContextNoteKeydown}
            placeholder="note here (e.g. modem reboot)"
            maxlength="280"
            spellcheck="false"
            disabled={contextNote.pending}
            aria-label="annotation text"
          />
          <button
            type="submit"
            class="context-note-apply"
            disabled={contextNote.pending || !contextNote.draft.trim()}
            title="save (Enter)"
          >{contextNote.pending ? '…' : 'add'}</button>
          <button
            type="button"
            class="context-note-cancel"
            on:click={cancelContextNote}
            disabled={contextNote.pending}
            title="cancel (Esc)"
          >×</button>
        </div>
        {#if contextNote.error}
          <div class="context-note-error" title={contextNote.error}>{contextNote.error}</div>
        {/if}
      </form>
    {/if}
  </div>
</section>

<style>
  .timeline {
    display: flex;
    flex-direction: column;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    overflow: hidden;
  }
  .card-header {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--border);
  }
  .card-header h2 {
    margin: 0;
    font-size: 0.95rem;
    font-weight: 600;
    color: var(--fg);
    display: flex;
    align-items: baseline;
    gap: 0.5em;
  }
  .hop-label {
    color: var(--label-color, var(--fg-muted));
    font-family: var(--font-mono);
    font-weight: 400;
    font-size: 0.85rem;
  }
  .header-right {
    display: flex;
    align-items: baseline;
    gap: var(--space-3);
  }

  /* Nav controls. ← / now / → as a tight pill cluster matching the
     time-window picker style. The "past-mode badge" sits adjacent
     showing the current anchor's local time. */
  .nav {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 2px;
  }
  .nav.active {
    border-color: var(--accent);
  }
  .nav-btn {
    background: transparent;
    border: none;
    color: var(--fg-muted);
    font-family: var(--font-mono);
    font-size: 0.75rem;
    padding: 2px 8px;
    border-radius: calc(var(--radius-sm) - 2px);
    cursor: pointer;
    min-width: 24px;
    text-align: center;
    transition: background 80ms ease-out, color 80ms ease-out;
  }
  .nav-btn:hover:not(:disabled) { color: var(--fg); }
  .nav-btn:disabled { opacity: 0.4; cursor: default; }
  .nav-btn.now { font-weight: 600; }
  .nav-btn.now.dim { opacity: 0.6; }
  .past-badge {
    margin-left: 6px;
    padding: 1px 6px;
    color: var(--accent);
    font-family: var(--font-mono);
    font-size: 0.7rem;
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--accent) 12%, transparent);
  }

  /* Step-58: prominent past-mode banner. Top-center inside the
     chart-area, accent-tinted background with a thin accent border
     and a CTA that disambiguates "this is a state, not data." z-index
     above the canvas (15-25 range used by other overlays) so it sits
     on top, pointer-events:auto so the click registers. Width capped
     so it doesn't span the entire chart on wide displays. */
  .past-banner {
    position: absolute;
    top: 8px;
    left: 50%;
    transform: translateX(-50%);
    display: inline-flex;
    align-items: center;
    gap: 10px;
    padding: 6px 14px;
    background: color-mix(in srgb, var(--accent) 20%, var(--bg-elevated));
    border: 1px solid var(--accent);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-family: var(--font-mono);
    font-size: 0.8rem;
    cursor: pointer;
    z-index: 20;
    box-shadow: 0 2px 8px rgba(0, 0, 0, 0.25);
    transition: background 80ms ease-out;
  }
  .past-banner:hover {
    background: color-mix(in srgb, var(--accent) 30%, var(--bg-elevated));
  }
  .past-banner-icon {
    font-size: 0.95rem;
    line-height: 1;
  }
  .past-banner-text strong {
    color: var(--accent);
    font-weight: 600;
    margin-left: 2px;
  }
  .past-banner-cta {
    color: var(--fg-muted);
    font-size: 0.72rem;
    padding-left: 8px;
    border-left: 1px solid color-mix(in srgb, var(--accent) 40%, transparent);
  }

  .legend-root { position: relative; display: inline-flex; }
  .legend-menu {
    position: absolute;
    top: calc(100% + 4px);
    right: 0;
    width: 320px;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    box-shadow: 0 4px 16px rgba(0, 0, 0, 0.35);
    padding: 8px 10px;
    z-index: 25;
    display: flex;
    flex-direction: column;
    gap: 7px;
    font-size: 0.72rem;
    color: var(--fg-muted);
    text-align: left;
  }
  .lg-row { display: flex; align-items: center; gap: 8px; line-height: 1.35; }
  .lg-line { width: 18px; height: 2px; border-radius: 1px; flex-shrink: 0; }
  .lg-band { width: 18px; height: 12px; border-radius: 2px; flex-shrink: 0; }
  .lg-dash { width: 18px; height: 0; border-top: 2px dashed; flex-shrink: 0; }
  .lg-mark { width: 18px; text-align: center; color: var(--accent); flex-shrink: 0; }

  .scale-toggle {
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    padding: 2px 8px;
    font-family: var(--font-mono);
    font-size: 0.75rem;
  }
  .scale-toggle:hover {
    color: var(--fg);
    border-color: var(--border-strong);
  }
  .meta {
    color: var(--fg-muted);
    font-size: 0.85rem;
    font-family: var(--font-mono);
  }
  .chart-area {
    flex: 1;
    position: relative;
    min-height: 0;
  }
  .canvas-mount {
    width: 100%;
    height: 100%;
  }
  .placeholder {
    position: absolute;
    inset: 0;
    display: grid;
    place-items: center;
    color: var(--fg-subtle);
    font-family: var(--font-mono);
    font-size: 0.85rem;
    pointer-events: none;
  }

  /* Tooltip — much smaller now that it shows a single value. Keep the
     pointer-events: none and absolute positioning behavior from step-18. */
  .tooltip {
    position: absolute;
    pointer-events: none;
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-family: var(--font-mono);
    font-size: 0.8rem;
    box-shadow: 0 4px 16px rgba(0, 0, 0, 0.4);
    z-index: 10;
    opacity: 0;
    transition: opacity 80ms ease-out;
    min-width: 140px;
    width: 140px;
    text-align: center;
  }
  .tooltip.visible { opacity: 1; }
  /* Step-59: when there's room above the cursor, position the tooltip
     ABOVE it so the cursor's down-right body never sits on top of the
     timestamp text. translateY(-100%) anchors the bottom of the box
     at tooltipY; the JS already subtracts the offset, so the tooltip
     hangs cleanly above the cursor. */
  .tooltip.above { transform: translateY(-100%); }

  .tooltip-time {
    color: var(--fg-muted);
    font-size: 0.7rem;
    padding-bottom: var(--space-2);
    margin-bottom: var(--space-2);
    border-bottom: 1px solid var(--border);
  }
  .tooltip-value {
    color: var(--fg);
    font-size: 0.95rem;
  }

  :global(.uplot .u-legend) {
    color: var(--fg-muted);
    font-family: var(--font-mono);
    font-size: 0.8rem;
  }
  :global(.uplot .u-cursor-x),
  :global(.uplot .u-cursor-y) {
    border-color: var(--accent) !important;
  }

  /* Step-45: export button — same visual weight as scale-toggle. */
  .export-btn {
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    padding: 2px 8px;
    font-family: var(--font-mono);
    font-size: 0.75rem;
    line-height: 1;
    cursor: pointer;
    transition: color 80ms ease-out, border-color 80ms ease-out, background 80ms ease-out;
  }
  .export-btn:hover:not(:disabled) {
    color: var(--fg);
    border-color: var(--border-strong);
    background: var(--bg-elevated);
  }
  .export-btn:disabled { opacity: 0.4; cursor: not-allowed; }

  /* Step-43: focus-area badge in the chart header. Same visual
     vocabulary as the past-mode badge — accent-colored, low chrome,
     inline × dismiss. */
  .focus-area-badge {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    padding: 1px 4px 1px 8px;
    color: var(--accent);
    font-family: var(--font-mono);
    font-size: 0.72rem;
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--accent) 14%, transparent);
    border: 1px solid color-mix(in srgb, var(--accent) 35%, transparent);
  }
  .focus-clear {
    background: transparent;
    border: none;
    color: var(--accent);
    font-size: 0.9rem;
    line-height: 1;
    padding: 0 4px;
    cursor: pointer;
    opacity: 0.85;
  }
  .focus-clear:hover { opacity: 1; color: var(--danger); }
  /* Step-47: inline width select inside the focus badge. Native
     select for accessibility + zero menu-positioning code; minimal
     styling so it reads as part of the badge rather than a foreign
     form control. */
  .focus-width {
    background: transparent;
    border: 1px solid color-mix(in srgb, var(--accent) 30%, transparent);
    border-radius: var(--radius-sm);
    color: var(--accent);
    font-family: var(--font-mono);
    font-size: 0.7rem;
    padding: 0 2px;
    margin: 0 2px;
    cursor: pointer;
  }
  .focus-width:focus { outline: 1px solid var(--accent); outline-offset: 1px; }

  /* Step-42: + note button + inline form, matched visual weight to
     the surrounding header controls so the row reads as a single
     band of equivalent affordances. */
  .note-add {
    background: transparent;
    border: 1px dashed var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    padding: 2px 8px;
    font-family: var(--font-mono);
    font-size: 0.75rem;
    line-height: 1;
    cursor: pointer;
    transition: color 80ms ease-out, border-color 80ms ease-out, background 80ms ease-out;
  }
  .note-add:hover:not(:disabled) {
    color: var(--fg);
    border-color: var(--border-strong);
    background: var(--bg-elevated);
  }
  .note-add:disabled { opacity: 0.4; cursor: not-allowed; }
  .note-form {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    padding: 2px 4px;
    background: var(--bg-sunken);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm);
  }
  .note-form input {
    background: transparent;
    border: none;
    color: var(--fg);
    font-family: var(--font-mono);
    font-size: 0.8rem;
    padding: 2px 6px;
    width: 22ch;
    outline: none;
  }
  .note-apply, .note-cancel {
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-muted);
    font-family: var(--font-mono);
    font-size: 0.7rem;
    padding: 2px 6px;
    cursor: pointer;
  }
  .note-apply { font-weight: 600; }
  .note-apply:disabled, .note-cancel:disabled { opacity: 0.5; cursor: wait; }
  .note-error {
    color: var(--danger);
    font-size: 0.7rem;
    font-family: var(--font-mono);
    max-width: 16ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  /* Hover overlay for ▲ markers. Positioned absolutely inside
     .chart-area; pointer-events: auto so the × delete button is
     clickable. Centered above the marker via translate(-50%, 0). */
  .annotation-popover {
    position: absolute;
    transform: translate(-50%, 0);
    background: var(--bg-elevated);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    box-shadow: 0 4px 12px rgba(0, 0, 0, 0.4);
    padding: 6px 8px;
    z-index: 25;
    max-width: 280px;
    pointer-events: auto;
    font-family: var(--font-mono);
    font-size: 0.75rem;
  }
  .annotation-text {
    color: var(--fg);
    line-height: 1.4;
    word-wrap: break-word;
  }
  .annotation-meta {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    margin-top: 4px;
    padding-top: 4px;
    border-top: 1px solid var(--border);
    color: var(--fg-subtle);
    font-size: 0.65rem;
  }
  .annotation-delete {
    background: transparent;
    border: none;
    color: var(--fg-subtle);
    font-size: 0.9rem;
    padding: 0 4px;
    cursor: pointer;
    line-height: 1;
  }
  .annotation-delete:hover { color: var(--danger); }

  /* Step-49: right-click pin-at-cursor note popover. Anchored at the
     click coords (translate(-50%, 0) so the popover hangs below the
     cursor centered horizontally). Sits above the hover-popover in
     z-order so it wins when both happen to overlap. */
  .context-note {
    position: absolute;
    transform: translate(-50%, 0);
    background: var(--bg-elevated);
    border: 1px solid var(--accent);
    border-radius: var(--radius-sm);
    box-shadow: 0 4px 16px rgba(0, 0, 0, 0.5);
    padding: 6px 8px;
    z-index: 30;
    font-family: var(--font-mono);
    font-size: 0.75rem;
    pointer-events: auto;
    min-width: 260px;
  }
  .context-note-time {
    color: var(--fg-subtle);
    font-size: 0.65rem;
    margin-bottom: 4px;
  }
  .context-note-row {
    display: flex;
    align-items: center;
    gap: 4px;
  }
  .context-note input {
    flex: 1;
    min-width: 0;
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg);
    padding: 3px 6px;
    font-family: var(--font-mono);
    font-size: 0.75rem;
    outline: none;
  }
  .context-note input:focus { border-color: var(--accent); }
  .context-note-apply,
  .context-note-cancel {
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg-muted);
    padding: 2px 8px;
    border-radius: var(--radius-sm);
    cursor: pointer;
    font-family: var(--font-mono);
    font-size: 0.75rem;
  }
  .context-note-apply:hover:not(:disabled) {
    color: var(--accent);
    border-color: var(--accent);
  }
  .context-note-cancel:hover:not(:disabled) { color: var(--fg); }
  .context-note-apply:disabled,
  .context-note-cancel:disabled { opacity: 0.4; cursor: not-allowed; }
  .context-note-error {
    color: var(--danger);
    font-size: 0.7rem;
    margin-top: 4px;
  }

  /* Step-52: shift+drag brush preview overlay. Translucent accent
     fill matches the focusPlugin's committed-focus paint so the
     gesture feels like a single "preview → commit" transition.
     pointer-events: none so it doesn't intercept mousemove during
     the drag (chart-area handles the gesture). */
  .brush-preview {
    position: absolute;
    background: color-mix(in srgb, var(--accent) 18%, transparent);
    border-left: 1px dashed var(--accent);
    border-right: 1px dashed var(--accent);
    pointer-events: none;
    z-index: 15;
  }

  /* Step-53: brush range labels positioned above the plot area so
     they aren't cursored-over during the drag. Center label only
     renders when the brush is wide enough to fit it without overlap. */
  .brush-label {
    position: absolute;
    pointer-events: none;
    z-index: 16;
    background: var(--bg-elevated);
    border: 1px solid var(--accent);
    border-radius: var(--radius-sm);
    color: var(--fg);
    font-family: var(--font-mono);
    font-size: 0.7rem;
    padding: 1px 6px;
    white-space: nowrap;
    line-height: 1.4;
  }
  .brush-label-left { transform: translateX(-50%); }
  .brush-label-right { transform: translateX(-50%); }
  .brush-label-center {
    transform: translateX(-50%);
    background: var(--accent);
    color: var(--bg);
    border-color: var(--accent);
    font-weight: 600;
  }
</style>
