import { apiGet } from './api'
import { usePolledResource } from './hooks'
import { adminQuery } from './kubeAdminTypes'
import type { AdminTarget } from './kubeAdminTypes'

type ResourceNames = { names: string[]; continue?: string }

export function useKubernetesResourceNames(target: Omit<AdminTarget, 'name'>, enabled: boolean) {
  const query = adminQuery({ ...target, name: '' })
  return usePolledResource<string[]>(async (signal) => {
    if (!enabled) return []
    const names: string[] = []
    const tokens = new Set<string>()
    let continuation = ''
    do {
      const page = await apiGet<ResourceNames>(`/api/kubernetes/admin/resource-names?${query}&continue=${encodeURIComponent(continuation)}`, signal)
      names.push(...page.names)
      continuation = page.continue ?? ''
      if (continuation && tokens.has(continuation)) throw new Error('Kubernetes returned repeated resource pages. Refresh suggestions or type the resource name manually.')
      tokens.add(continuation)
    } while (continuation)
    return names
  }, 6000, [query, enabled], enabled ? `kubernetes:resource-names:${query}` : undefined)
}
