import { useSyncExternalStore } from 'react'

const storageKey = 'porto.environment.v1'
const listeners = new Set<() => void>()
const validID = /^(local|[a-f0-9]{32})$/

function storedEnvironment(): string {
  try {
    const value = window.localStorage.getItem(storageKey)
    if (value === null) return 'local'
    if (validID.test(value)) return value
    console.error('Invalid saved environment ID. No remote target was restored.')
  } catch (error) { console.error('Unable to read the selected environment', error) }
  return 'unavailable'
}

let environment = typeof window === 'undefined' ? 'local' : storedEnvironment()
export function getEnvironmentID() { return environment }
export function subscribeEnvironment(listener: () => void) {
  listeners.add(listener)
  return () => { listeners.delete(listener) }
}
export function setEnvironmentID(value: string) {
  if (!validID.test(value)) throw new Error('Select an explicit enrolled environment ID.')
  window.localStorage.setItem(storageKey, value)
  if (environment === value) return
  environment = value
  for (const listener of listeners) listener()
}
export function useEnvironmentID() {
  return useSyncExternalStore(subscribeEnvironment, getEnvironmentID, getEnvironmentID)
}
export function environmentStorageKey(key: string, id = environment) {
  return id === 'local' ? key : `${key}.${id}`
}
export function targetAPIPath(pathname: string) {
  return pathname.replace(/^\/api\/environments\/[^/]+(?=\/api\/)/, '')
}
export function environmentURL(input: string, selected = environment): string {
  const url = new URL(input, window.location.href)
  if (url.host !== window.location.host || !url.pathname.startsWith('/api/') ||
    /^\/api\/(environments|extensions)(\/|$)/.test(url.pathname) || selected === 'local') return input
  if (!validID.test(selected)) throw new Error('Invalid environment target. Local fallback is forbidden.')
  url.pathname = `/api/environments/${selected}${url.pathname}`
  return input.startsWith('/') ? `${url.pathname}${url.search}${url.hash}` : url.toString()
}

export const SOURCE_HOST_URL_NOTICE = 'This workload URL belongs to the source host. Use an explicit remote port forward or externally reachable ingress; Porto will not open a same-named local workload.'
export function workloadURL(input: string | undefined): string | undefined {
  if (!input) return undefined
  if (environment === 'local') return input
  try {
    const parsed = new URL(input, window.location.href)
    const host = parsed.hostname.toLowerCase().replace(/^\[|\]$/g, '')
    if (!['http:', 'https:'].includes(parsed.protocol) || parsed.origin === window.location.origin ||
      host === 'localhost' || host.endsWith('.localhost') || host.endsWith('.porto.local') ||
      /^127\./.test(host) || host === '0.0.0.0' || host === '::' || host === '::1' || host.startsWith('::ffff:') ||
      ['host.docker.internal', 'gateway.docker.internal', 'host.containers.internal'].includes(host)) return undefined
    return input
  } catch { return undefined }
}

function onStorage(event: StorageEvent) {
  if (event.key !== storageKey && event.key !== null) return
  const next = storedEnvironment()
  if (next === environment) return
  environment = next
  for (const listener of listeners) listener()
}
if (typeof window !== 'undefined') {
  window.addEventListener('storage', onStorage)
  import.meta.hot?.dispose(() => window.removeEventListener('storage', onStorage))
}
