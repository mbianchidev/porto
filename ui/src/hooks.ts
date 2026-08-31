import { useCallback, useEffect, useRef, useState } from 'react'
import type { DependencyList } from 'react'
import { errorMessage, isAbortError } from './api'

type PolledResource<T> = {
  data: T | null
  error: string
  loading: boolean
  reload: () => void
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
): PolledResource<T> {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  const [reloadToken, setReloadToken] = useState(0)
  const fetcherRef = useRef(fetcher)
  fetcherRef.current = fetcher

  useEffect(() => {
    let active = true
    const controller = new AbortController()
    setLoading(true)
    const run = async () => {
      try {
        const result = await fetcherRef.current(controller.signal)
        if (active) {
          setData(result)
          setError('')
        }
      } catch (err) {
        if (active && !isAbortError(err)) {
          setError(errorMessage(err, 'Request failed'))
        }
      } finally {
        if (active) setLoading(false)
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
  }, [intervalMs, reloadToken, ...deps])

  const reload = useCallback(() => setReloadToken((value) => value + 1), [])

  return { data, error, loading, reload }
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
