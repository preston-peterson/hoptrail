// Clipboard helper (step-167): navigator.clipboard exists ONLY in
// secure contexts (https / localhost) — and hoptrail's UI is usually
// plain http on a LAN, where every Copy button silently did nothing
// (operator-reported). The textarea + execCommand('copy') path is the
// time-honored fallback that works on http; deprecated on paper,
// universally shipped in practice.
//
// Returns true when the text actually reached the clipboard — callers
// show "Copied ✓" only on success and fall back to telling the
// operator to select the (user-select: all) code block.

export async function copyText(text) {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // Permission denied or transient — try the legacy path.
    }
  }
  try {
    const ta = document.createElement('textarea')
    ta.value = text
    ta.setAttribute('readonly', '')
    // Off-screen but NOT display:none — hidden elements can't be
    // selected, and selection is what execCommand copies.
    ta.style.position = 'fixed'
    ta.style.top = '-1000px'
    document.body.appendChild(ta)
    ta.select()
    ta.setSelectionRange(0, text.length)
    const ok = document.execCommand('copy')
    document.body.removeChild(ta)
    return ok
  } catch {
    return false
  }
}
