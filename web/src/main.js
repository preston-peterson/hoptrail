import './app.css'
import App from './App.svelte'

// Svelte 4 entrypoint. Instantiates the App component against the
// #app div in index.html.
const app = new App({
  target: document.getElementById('app'),
})

export default app
