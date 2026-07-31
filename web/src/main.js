import './app.css'
import App from './App.svelte'
import { installCsrfFetch } from './lib/csrf.js'

// Install the X-Hoptrail-CSRF fetch wrapper before anything makes a
// request (step-170, security audit).
installCsrfFetch()

// Svelte 4 entrypoint. Instantiates the App component against the
// #app div in index.html.
const app = new App({
  target: document.getElementById('app'),
})

export default app
