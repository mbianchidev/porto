import type { LampState } from '../types'

/**
 * The status lamp: a 9px circular indicator whose color encodes state
 * (olive running, amber starting/attention, fault red crashed, mid-gray
 * stopped/neutral). Always rendered alongside a text state label by callers —
 * never used as the sole signal of state.
 */
export function StatusLamp({ state }: { state: LampState }) {
  return <span className={`statusLamp ${state}`} aria-hidden="true" />
}
