/**
 * Calls `run` every `intervalMs` while the page is visible, assuming the caller
 * has just run it. Hidden pages (a window closed to the tray, minimized, or fully
 * covered) stop polling; when the page shows again, polling resumes on the same
 * cadence and runs immediately if a refresh is overdue. Returns a cleanup function.
 */
export function pollWhileVisible(run: () => void, intervalMs: number): () => void {
  if (intervalMs <= 0) return () => {}
  let lastRun = Date.now()
  let interval: number | undefined
  let resume: number | undefined
  const tick = () => {
    lastRun = Date.now()
    run()
  }
  const stop = () => {
    if (interval !== undefined) window.clearInterval(interval)
    if (resume !== undefined) window.clearTimeout(resume)
    interval = undefined
    resume = undefined
  }
  const start = () => {
    stop()
    resume = window.setTimeout(() => {
      resume = undefined
      tick()
      interval = window.setInterval(tick, intervalMs)
    }, Math.max(0, intervalMs - (Date.now() - lastRun)))
  }
  const onVisibilityChange = () => {
    if (document.visibilityState === 'hidden') stop()
    else start()
  }
  if (document.visibilityState !== 'hidden') interval = window.setInterval(tick, intervalMs)
  document.addEventListener('visibilitychange', onVisibilityChange)
  return () => {
    document.removeEventListener('visibilitychange', onVisibilityChange)
    stop()
  }
}

/** Structural equality for JSON-shaped API payloads; other objects never match. */
export function sameJSON(left: unknown, right: unknown): boolean {
  if (Object.is(left, right)) return true
  if (Array.isArray(left) || Array.isArray(right)) {
    if (!Array.isArray(left) || !Array.isArray(right) || left.length !== right.length) return false
    return left.every((value, index) => sameJSON(value, right[index]))
  }
  if (!isPlainObject(left) || !isPlainObject(right)) return false
  const leftKeys = Object.keys(left)
  if (leftKeys.length !== Object.keys(right).length) return false
  return leftKeys.every((key) => Object.prototype.hasOwnProperty.call(right, key) && sameJSON(left[key], right[key]))
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
  if (typeof value !== 'object' || value === null) return false
  const prototype = Object.getPrototypeOf(value)
  return prototype === Object.prototype || prototype === null
}
