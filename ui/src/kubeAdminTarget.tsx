import { useState } from 'react'
import type { ReactNode } from 'react'
import type { AdminTarget } from './kubeAdminTypes'
import { KubernetesAdminTargetContext } from './kubeAdminTargetContext'

export function KubernetesAdminTargetProvider({ children }: { children: ReactNode }) {
  const [target, setTarget] = useState<AdminTarget | null>(null)
  return <KubernetesAdminTargetContext.Provider value={{ target, setTarget }}>{children}</KubernetesAdminTargetContext.Provider>
}
