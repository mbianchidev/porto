import { useCallback, useEffect, useState } from 'react'
import { apiGet, errorMessage, isAbortError } from './api'
import type { KubernetesStatus } from './types'

type KubernetesStatusState = {
  key: string
  data: KubernetesStatus | null
  error: string
  loading: boolean
}

function inactiveStatus(context: string, enabled: boolean): KubernetesStatus {
  return {
    enabled,
    available: false,
    context,
    message: enabled ? 'Select a Kubernetes context' : 'Kubernetes runtime is disabled',
  }
}

export function useKubernetesStatus(context: string, enabled = true) {
  const active = enabled && context.trim() !== ''
  const key = active ? context : `${enabled ? 'select-context' : 'disabled'}:${context}`
  const [state, setState] = useState<KubernetesStatusState>({
    key,
    data: active ? null : inactiveStatus(context, enabled),
    error: '',
    loading: active,
  })
  const [reloadToken, setReloadToken] = useState(0)
  const current = state.key === key
    ? state
    : { key, data: active ? null : inactiveStatus(context, enabled), error: '', loading: active }

  useEffect(() => {
    if (!active) return
    let mounted = true
    let running = false
    const controller = new AbortController()
    const run = async () => {
      if (running) return
      running = true
      try {
        const data = await apiGet<KubernetesStatus>(
          `/api/kubernetes/status?context=${encodeURIComponent(context)}`,
          controller.signal,
        )
        if (mounted) setState({ key, data, error: '', loading: false })
      } catch (err) {
        if (mounted && !isAbortError(err)) {
          const message = errorMessage(err, 'Kubernetes status request failed')
          setState({
            key,
            data: { enabled: true, available: false, context, message },
            error: message,
            loading: false,
          })
        }
      } finally {
        running = false
      }
    }
    void run()
    const timer = window.setInterval(run, 10000)
    return () => {
      mounted = false
      controller.abort()
      window.clearInterval(timer)
    }
  }, [active, context, enabled, key, reloadToken])

  const reload = useCallback(() => setReloadToken((value) => value + 1), [])
  return {
    data: current.data,
    error: current.error,
    loading: current.loading,
    reload,
  }
}
