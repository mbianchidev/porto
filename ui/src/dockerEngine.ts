// Rest/operate switch for Porto's Docker engine. The daemon owns the engine;
// this only reads its status and calls the start/stop routes.

import { useState } from 'react'
import { apiGet, apiSend, errorMessage } from './api'
import { usePolledResource } from './hooks'
import { useMessages } from './useMessages'
import type { DockerContainer, DockerStatus } from './types'

export type DockerEnginePhase = 'unknown' | 'running' | 'resting' | 'starting' | 'stopping'

const START_WAIT_MS = 60000

async function runningContainerCount(): Promise<number | null> {
  try {
    const containers = await apiGet<DockerContainer[]>('/api/docker/containers')
    return containers.filter((container) => container.state === 'running').length
  } catch {
    return null
  }
}

async function waitForEngine() {
  const deadline = Date.now() + START_WAIT_MS
  while (Date.now() < deadline) {
    const status = await apiGet<DockerStatus>('/api/docker/status')
    if (status.available) return
    await new Promise((resolve) => setTimeout(resolve, 1000))
  }
  throw new Error(`The Docker engine started but did not report ready within ${START_WAIT_MS / 1000}s. Check Diagnostics.`)
}

export function useDockerEngine(controllable: boolean) {
  const { notifyError, notifyNotice } = useMessages()
  const [transition, setTransition] = useState<'starting' | 'stopping' | null>(null)
  const status = usePolledResource<DockerStatus | null>(
    (signal) => controllable ? apiGet('/api/docker/status', signal) : Promise.resolve(null),
    10000,
    [controllable],
    controllable ? 'docker:status' : undefined,
  )
  const engine = controllable && status.data?.enabled ? status.data : null
  const phase: DockerEnginePhase = transition ?? (engine === null ? 'unknown' : engine.available ? 'running' : 'resting')

  async function toggle() {
    if (phase !== 'running' && phase !== 'resting') return
    const starting = phase === 'resting'
    if (!starting) {
      const running = await runningContainerCount()
      const impact = running === null
        ? 'Porto could not list running containers, so any that are running will stop.'
        : `${running} running container${running === 1 ? '' : 's'} will stop.`
      if (running !== 0 && !window.confirm(`Stop the Docker engine? ${impact} Containers, images and volumes are kept.`)) return
    }
    setTransition(starting ? 'starting' : 'stopping')
    try {
      await apiSend(`/api/docker/engine/${starting ? 'start' : 'stop'}`, 'POST')
      if (starting) await waitForEngine()
      notifyNotice('containers', starting
        ? 'Docker engine started. Porto is back in operate mode.'
        : 'Docker engine stopped. Porto is in rest mode; containers, images and volumes are kept.')
    } catch (error) {
      notifyError('containers', errorMessage(error, `Unable to ${starting ? 'start' : 'stop'} the Docker engine`))
    } finally {
      setTransition(null)
      status.reload()
    }
  }

  return { phase, toggle }
}
