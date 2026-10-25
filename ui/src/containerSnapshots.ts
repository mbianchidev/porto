import { useEffect, useSyncExternalStore } from 'react'
import { apiGet, errorMessage, isAbortError } from './api'
import { dockerModeURL, getDockerMode, subscribeDockerMode } from './dockerMode'
import type { DockerContainer, DockerContainerSnapshot } from './types'

type ContainerSnapshotState = {
  snapshot: DockerContainerSnapshot | null
  error: string
  loading: boolean
  connected: boolean
}

type ContainerSnapshotResource = {
  data: DockerContainer[] | null
  error: string
  loading: boolean
  connected: boolean
  snapshot: DockerContainerSnapshot | null
  reload: () => void
}

const listeners = new Set<() => void>()
let state: ContainerSnapshotState = {
  snapshot: null,
  error: '',
  loading: true,
  connected: false,
}
let consumers = 0
let source: EventSource | null = null
let request: AbortController | null = null
let reconnectTimer: number | undefined
let fallbackTimer: number | undefined
let reconnectDelay = 1000
let activeMode = getDockerMode()

function emit(next: ContainerSnapshotState) {
  state = next
  for (const listener of listeners) listener()
}

function acceptSnapshot(snapshot: DockerContainerSnapshot) {
  if (
    state.snapshot
    && snapshot.instanceId === state.snapshot.instanceId
    && snapshot.revision < state.snapshot.revision
  ) return
  emit({
    snapshot,
    error: snapshot.available ? '' : snapshot.message || 'Container inventory is unavailable',
    loading: false,
    connected: state.connected,
  })
}

async function refresh() {
  request?.abort()
  const controller = new AbortController()
  const requestMode = activeMode
  request = controller
  if (!state.snapshot) {
    emit({ ...state, loading: true, error: '' })
  }
  try {
    const snapshot = await apiGet<DockerContainerSnapshot>(dockerModeURL('/api/docker/containers/snapshot', requestMode), controller.signal)
    if (!controller.signal.aborted && requestMode === activeMode) acceptSnapshot(snapshot)
  } catch (err) {
    if (!isAbortError(err) && requestMode === activeMode) {
      emit({
        ...state,
        error: errorMessage(err, 'Unable to load container inventory'),
        loading: false,
      })
    }
  } finally {
    if (request === controller) request = null
  }
}

function clearReconnectTimer() {
  if (reconnectTimer !== undefined) {
    window.clearTimeout(reconnectTimer)
    reconnectTimer = undefined
  }
}

function clearFallbackTimer() {
  if (fallbackTimer !== undefined) {
    window.clearInterval(fallbackTimer)
    fallbackTimer = undefined
  }
}

function startFallbackPolling() {
  if (fallbackTimer !== undefined || consumers === 0) return
  fallbackTimer = window.setInterval(refresh, 30000)
}

function scheduleReconnect() {
  if (reconnectTimer !== undefined || consumers === 0) return
  const delay = reconnectDelay
  reconnectDelay = Math.min(reconnectDelay * 2, 30000)
  reconnectTimer = window.setTimeout(() => {
    reconnectTimer = undefined
    connect()
  }, delay)
}

function connect() {
  if (source || consumers === 0) return
  const nextSource = new EventSource(dockerModeURL('/api/docker/containers/events', activeMode))
  source = nextSource
  nextSource.onopen = () => {
    if (source !== nextSource) return
    reconnectDelay = 1000
    clearReconnectTimer()
    clearFallbackTimer()
    emit({ ...state, connected: true })
  }
  nextSource.addEventListener('snapshot', (event) => {
    if (source !== nextSource || !(event instanceof MessageEvent)) return
    try {
      acceptSnapshot(JSON.parse(event.data) as DockerContainerSnapshot)
    } catch (err) {
      emit({
        ...state,
        error: errorMessage(err, 'Container update was invalid'),
        loading: false,
      })
    }
  })
  nextSource.onerror = () => {
    if (source !== nextSource) return
    nextSource.close()
    source = null
    emit({
      ...state,
      connected: false,
      error: state.snapshot?.message || 'Live container updates disconnected; using fallback refreshes',
    })
    startFallbackPolling()
    scheduleReconnect()
  }
}

function start() {
  consumers += 1
  if (consumers > 1) return
  void refresh()
  connect()
}

function stop() {
  consumers = Math.max(0, consumers - 1)
  if (consumers > 0) return
  source?.close()
  source = null
  request?.abort()
  request = null
  clearReconnectTimer()
  clearFallbackTimer()
  reconnectDelay = 1000
  if (state.connected) emit({ ...state, connected: false })
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

function getSnapshot() {
  return state
}

const unsubscribeMode = subscribeDockerMode(() => {
  source?.close()
  source = null
  request?.abort()
  request = null
  clearReconnectTimer()
  clearFallbackTimer()
  reconnectDelay = 1000
  activeMode = getDockerMode()
  emit({ snapshot: null, error: '', loading: true, connected: false })
  if (consumers > 0) {
    void refresh()
    connect()
  }
})
import.meta.hot?.dispose(unsubscribeMode)

export function useContainerSnapshots(enabled = true): ContainerSnapshotResource {
  const current = useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
  useEffect(() => {
    if (!enabled) return
    start()
    return stop
  }, [enabled])
  if (!enabled) {
    return {
      data: [],
      error: '',
      loading: false,
      connected: false,
      snapshot: null,
      reload: refresh,
    }
  }
  return {
    data: current.snapshot?.containers ?? null,
    error: current.error,
    loading: current.loading,
    connected: current.connected,
    snapshot: current.snapshot,
    reload: refresh,
  }
}
