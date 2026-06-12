// Audible alerts (#20) — two small WebAudio tones, no audio assets.
//
// Browsers refuse to start audio until the page has seen a user
// gesture. The Sound master toggle and the preview buttons are
// gestures, so the browser that configures sound is armed on the
// spot; any OTHER browser (the policy is server-shared) arms on its
// first click/keypress anywhere in the page via installAutoArm().
// An alert arriving before that first interaction stays silent —
// that's the platform rule, not something to fight.

let ctx = null

function audioContext() {
  if (!ctx) {
    const AC = window.AudioContext ?? window.webkitAudioContext
    if (!AC) return null
    ctx = new AC()
  }
  return ctx
}

/** Resume the context if suspended. Call from a user-gesture handler. */
export function armAudio() {
  const ac = audioContext()
  if (ac && ac.state === 'suspended') ac.resume().catch(() => {})
  return ac
}

/** True when the browser would actually produce sound right now. */
export function audioArmed() {
  return Boolean(ctx && ctx.state === 'running')
}

// One-time page-wide listeners so a browser that never opens the
// settings panel still arms on its first natural interaction.
let autoArmInstalled = false
export function installAutoArm() {
  if (autoArmInstalled) return
  autoArmInstalled = true
  const arm = () => {
    armAudio()
    window.removeEventListener('pointerdown', arm)
    window.removeEventListener('keydown', arm)
  }
  window.addEventListener('pointerdown', arm)
  window.addEventListener('keydown', arm)
}

// note schedules one sine blip with a quick attack and exponential
// release — soft enough not to startle, distinct enough to register.
function note(ac, freq, at, dur, peak) {
  const osc = ac.createOscillator()
  const gain = ac.createGain()
  osc.type = 'sine'
  osc.frequency.value = freq
  gain.gain.setValueAtTime(0.0001, at)
  gain.gain.exponentialRampToValueAtTime(peak, at + 0.015)
  gain.gain.exponentialRampToValueAtTime(0.0001, at + dur)
  osc.connect(gain).connect(ac.destination)
  osc.start(at)
  osc.stop(at + dur + 0.05)
}

/** Attention tone for a raised alert: two rising blips. */
export function playRaise() {
  const ac = armAudio()
  if (!ac || ac.state !== 'running') return false
  const t = ac.currentTime + 0.02
  note(ac, 660, t, 0.14, 0.2)
  note(ac, 880, t + 0.17, 0.18, 0.2)
  return true
}

/** All-clear tone for a recovery: two gentler falling blips. */
export function playRecover() {
  const ac = armAudio()
  if (!ac || ac.state !== 'running') return false
  const t = ac.currentTime + 0.02
  note(ac, 660, t, 0.16, 0.11)
  note(ac, 520, t + 0.19, 0.22, 0.11)
  return true
}
