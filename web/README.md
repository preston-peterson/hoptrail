# Hoptrail web UI

Svelte 4 + Vite 5 + uPlot 1.6 frontend for the Hoptrail daemon.

## Layout

```
web/
├── package.json          npm metadata + scripts
├── vite.config.js        Vite + Svelte plugin config; outputs to ../internal/web/dist/
├── svelte.config.js      Svelte preprocess config
├── index.html            Vite entrypoint (mounts <App/> into #app)
└── src/
    ├── main.js           Svelte app instantiation
    ├── app.css           Design tokens (CSS variables for light/dark themes)
    ├── App.svelte        Top-level layout + theme management
    ├── lib/
    │   ├── api.js        Fetch wrappers for /api/path, /api/samples, /api/route_changes
    │   └── stores.js     Svelte stores backed by polling
    └── components/
        ├── StatusBar.svelte
        ├── LatencyTimeline.svelte
        ├── HopList.svelte
        ├── HopCard.svelte
        └── RouteChangesLog.svelte

internal/web/dist/        Build output — embedded into the Go binary via //go:embed
```

## Dev workflow

Two terminals, one for the daemon, one for Vite.

```
# Terminal 1: the daemon (API on :8080)
make go-dev

# Terminal 2: Vite dev server (UI on :5173 with HMR, proxies /api to :8080)
make web-dev
```

Open http://localhost:5173 in a browser. Changes to `.svelte` / `.js` /
`.css` files hot-reload without losing UI state.

## Production build

```
make build
```

This runs `npm run build` to produce `web/dist/`, then `go build` to
produce the daemon binary with the bundle embedded via `//go:embed`. The
resulting binary is a single file — no separate static-asset deploy.

## API contract

See [`docs/api-v0.1.md`](../docs/api-v0.1.md) for the JSON shapes the
UI consumes. The handlers are implemented in step-10.
