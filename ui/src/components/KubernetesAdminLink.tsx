import type { AdminTarget } from '../kubeAdminTypes'
import type { ReactNode } from 'react'
import { useKubernetesAdminTarget } from '../kubeAdminTargetContext'

export function KubernetesAdminLink({ target, children = 'Administer this resource' }: { target: AdminTarget; children?: ReactNode }) {
  const { setTarget } = useKubernetesAdminTarget()
  return <a href="#/kubernetes-admin" onClick={() => setTarget(target)}>{children}</a>
}
