import type { LampState } from '../types'

export function lampStateFor(status: string): LampState {
  const normalized = status.toLowerCase()
  if (normalized === 'running' || normalized === 'ready' || normalized === 'active' || normalized === 'bound') return 'running'
  if (normalized === 'starting' || normalized === 'pending' || normalized === 'creating' || normalized === 'created' || normalized === 'restarting') return 'starting'
  if (normalized === 'stopped' || normalized === 'exited' || normalized === 'paused' || normalized === 'succeeded') return 'stopped'
  if (normalized === '' || normalized === 'unknown') return 'neutral'
  if (normalized === 'error' || normalized === 'failed' || normalized === 'crashed' || normalized === 'dead') return 'crashed'
  return 'crashed'
}
