// Cross-site request protection, client half (step-170, security
// audit). The daemon's crossOriginGuard requires an X-Hoptrail-CSRF
// header on every mutating request — a cross-origin page can't set a
// custom header without a CORS preflight the daemon refuses, so the
// header both proves same-origin and is the thing the server checks.
//
// Rather than thread it through ~40 call sites, we wrap window.fetch
// once: every same-origin, non-GET/HEAD request gets the header. The
// UI only ever talks to its own origin, so this is complete and safe;
// cross-origin requests (there are none) are left untouched.

export function installCsrfFetch() {
  if (typeof window === 'undefined' || window.__hoptrailCsrfInstalled) return
  window.__hoptrailCsrfInstalled = true

  const orig = window.fetch.bind(window)
  window.fetch = (input, init = {}) => {
    const method = (init.method || (typeof input !== 'string' && input.method) || 'GET').toUpperCase()
    if (method === 'GET' || method === 'HEAD') return orig(input, init)

    // Resolve the target URL and only add the header for same-origin
    // requests (defensive — the UI never calls elsewhere).
    let url
    try {
      url = new URL(typeof input === 'string' ? input : input.url, window.location.href)
    } catch {
      return orig(input, init)
    }
    if (url.origin !== window.location.origin) return orig(input, init)

    const headers = new Headers(init.headers || (typeof input !== 'string' && input.headers) || undefined)
    headers.set('X-Hoptrail-CSRF', '1')
    return orig(input, { ...init, headers })
  }
}
