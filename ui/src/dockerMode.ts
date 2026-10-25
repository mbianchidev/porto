import { useSyncExternalStore } from 'react'

export type DockerMode = 'linux' | 'windows'
const storageKey = 'porto.container-view.v1'
const listeners = new Set<() => void>()

function storedMode(): DockerMode {
  try {
    const value = window.localStorage.getItem(storageKey)
    if (value === null || value === 'linux' || value === 'windows') return value ?? 'linux'
    console.error('Invalid saved container view; using the Linux view without changing either engine.')
  } catch (error) {
    console.error('Unable to read the saved container view', error)
  }
  return 'linux'
}

let mode: DockerMode = typeof window === 'undefined' ? 'linux' : storedMode()

export function getDockerMode(): DockerMode { return mode }

export function subscribeDockerMode(listener: () => void) {
  listeners.add(listener)
  return () => { listeners.delete(listener) }
}

function updateMode(next: DockerMode) {
  if (mode === next) return
  mode = next
  for (const listener of listeners) listener()
}

export function setDockerMode(next: DockerMode) {
  if (next !== 'linux' && next !== 'windows') throw new Error('Container view must be Linux or Windows.')
  window.localStorage.setItem(storageKey, next)
  updateMode(next)
}

export function useDockerMode(): DockerMode {
  return useSyncExternalStore(subscribeDockerMode, getDockerMode, getDockerMode)
}

function onStorage(event: StorageEvent) {
  if (event.key === storageKey || event.key === null) updateMode(storedMode())
}
if (typeof window !== 'undefined') {
  window.addEventListener('storage', onStorage)
  import.meta.hot?.dispose(() => window.removeEventListener('storage', onStorage))
}

export function isDockerModePath(pathname: string) {
  return pathname === '/api/runtime' || pathname === '/api/docker' || pathname.startsWith('/api/docker/') ||
    pathname === '/api/networking' || pathname.startsWith('/api/networking/')
}

export function dockerModeURL(input: string, selected?: DockerMode): string {
  const url = new URL(input, window.location.href)
  if (url.host !== window.location.host || !isDockerModePath(url.pathname)) return input
  const existing = url.searchParams.get('containerMode')
  const requested = selected ?? (existing === 'linux' || existing === 'windows' ? existing : mode)
  if (requested === 'windows') url.searchParams.set('containerMode', 'windows')
  else url.searchParams.delete('containerMode')
  return input.startsWith('/') ? `${url.pathname}${url.search}${url.hash}` : url.toString()
}
