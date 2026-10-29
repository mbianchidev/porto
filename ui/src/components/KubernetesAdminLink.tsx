import type { AdminTarget } from '../kubeAdminTypes'
import { useKubernetesAdminTarget } from '../kubeAdminTarget'

export function KubernetesAdminLink({ target }: { target: AdminTarget }) {
  const { setTarget } = useKubernetesAdminTarget()
  return <a href="#/kubernetes-admin" onClick={() => setTarget(target)}>Administer this resource</a>
}
