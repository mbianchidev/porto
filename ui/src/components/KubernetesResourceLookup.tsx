import type { AdminTarget } from '../kubeAdminTypes'
import { KubernetesAdminLink } from './KubernetesAdminLink'

export function KubernetesResourceLookup({ target, namespaced = true }: { target: AdminTarget; namespaced?: boolean }) {
  const parts = target.name.trim().split('/')
  const namespace = namespaced ? parts.length === 2 ? parts[0] : target.namespace.trim() : ''
  const name = namespaced && parts.length === 2 ? parts[1] : target.name.trim()
  if (!target.context || !name || namespaced && !namespace) {
    return <button type="button" disabled title="Enter an explicit namespace and resource name, or type namespace/name.">Inspect typed resource</button>
  }
  return <KubernetesAdminLink target={{ ...target, namespace, name }}>Inspect typed resource</KubernetesAdminLink>
}
