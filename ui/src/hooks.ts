import { useCallback, useEffect, useRef, useState } from 'react'
import type { DependencyList } from 'react'
import { errorMessage, isAbortError } from './api'

type PolledResource<T> = {
  data: T | null
  error: string
  loading: boolean
  reload: () => void
  update: (updater: (current: T | null) => T | null) => void
}

type ResourceState<T> = {
  cacheKey?: string
  data: T | null
}

const resourceCache = new Map<string, unknown>()
const resourceCacheLimit = 100

function cachedResource<T>(cacheKey?: string): T | null {
  if (!cacheKey || !resourceCache.has(cacheKey)) return null
  return resourceCache.get(cacheKey) as T
}

function cacheResource<T>(cacheKey: string, value: T) {
  resourceCache.delete(cacheKey)
  resourceCache.set(cacheKey, value)
  while (resourceCache.size > resourceCacheLimit) {
    const oldest = resourceCache.keys().next().value
    if (oldest === undefined) break
    resourceCache.delete(oldest)
  }
}

/**
 * Fetches a resource once, then re-fetches on an interval, aborting the in-flight
 * request on unmount or when a dependency changes so stale responses never land.
 * Pass intervalMs=0 to fetch only once (still abortable, still reloadable).
 */
export function usePolledResource<T>(
  fetcher: (signal: AbortSignal) => Promise<T>,
  intervalMs: number,
  deps: DependencyList,
  cacheKey?: string,
): PolledResource<T> {
  const initialData = cachedResource<T>(cacheKey)
  const [resource, setResource] = useState<ResourceState<T>>({ cacheKey, data: initialData })
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(initialData === null)
  const [reloadToken, setReloadToken] = useState(0)
  const fetcherRef = useRef(fetcher)
  const generationRef = useRef(0)
  const data = resource.cacheKey === cacheKey ? resource.data : cachedResource<T>(cacheKey)
  const currentError = resource.cacheKey === cacheKey ? error : ''
  const currentLoading = resource.cacheKey === cacheKey ? loading : data === null
  const dataRef = useRef(data)
  fetcherRef.current = fetcher
  dataRef.current = data

  useEffect(() => {
    let active = true
    let running = false
    const controller = new AbortController()
    setError('')
    setLoading(dataRef.current === null)
    const run = async () => {
      if (running) return
      running = true
      const generation = generationRef.current
      try {
        const result = await fetcherRef.current(controller.signal)
        if (active && generation === generationRef.current) {
          if (cacheKey) cacheResource(cacheKey, result)
          dataRef.current = result
          setResource({ cacheKey, data: result })
          setError('')
        }
      } catch (err) {
        if (active && !isAbortError(err)) {
          setError(errorMessage(err, 'Request failed'))
        }
      } finally {
        if (active) setLoading(false)
        running = false
      }
    }
    run()
    const timer = intervalMs > 0 ? window.setInterval(run, intervalMs) : undefined
    return () => {
      active = false
      controller.abort()
      if (timer !== undefined) window.clearInterval(timer)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cacheKey, intervalMs, reloadToken, ...deps])

  const reload = useCallback(() => setReloadToken((value) => value + 1), [])
  const update = useCallback((updater: (current: T | null) => T | null) => {
    generationRef.current += 1
    const next = updater(dataRef.current)
    dataRef.current = next
    if (cacheKey) {
      if (next === null) resourceCache.delete(cacheKey)
      else cacheResource(cacheKey, next)
    }
    setResource({ cacheKey, data: next })
  }, [cacheKey])

  return { data, error: currentError, loading: currentLoading, reload, update }
}

/** Tracks whether the viewport is at or below a breakpoint, for rail/inspector collapse. */
export function useNarrowViewport(maxWidthPx: number): boolean {
  const query = `(max-width: ${maxWidthPx}px)`
  const [narrow, setNarrow] = useState(() => window.matchMedia(query).matches)
  useEffect(() => {
    const media = window.matchMedia(query)
    const listener = () => setNarrow(media.matches)
    media.addEventListener('change', listener)
    return () => media.removeEventListener('change', listener)
  }, [query])
  return narrow
}
