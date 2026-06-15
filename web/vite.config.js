import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// Vite config for the Hoptrail web UI.
//
// Production: `npm run build` produces web/dist/, which is embedded
// into the Go binary via //go:embed in internal/web/web.go. The Go
// HTTP server then serves the bundle as static assets at /.
//
// Development: `npm run dev` runs Vite's dev server on :5173 with HMR.
// The proxy below sends /api/* requests to the Go daemon on :8080, so
// during frontend dev you run `make go-dev` (daemon) and `make web-dev`
// (Vite) in two terminals, point your browser at http://localhost:5173,
// and get hot module reload on the frontend with live API data from
// the daemon.

export default defineConfig({
  plugins: [svelte()],
  build: {
    // Output to internal/web/dist/ so the Go //go:embed directive in
    // internal/web/web.go can reach it. Vite requires emptyOutDir
    // explicitly when the target is outside the project root.
    outDir: '../internal/web/dist',
    emptyOutDir: true,
    assetsDir: 'assets',
    sourcemap: true,
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      '/api': 'http://localhost:8080',
    },
  },
})
