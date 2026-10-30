import { createContext, useContext } from 'react'
import type { Dispatch, SetStateAction } from 'react'
import type { AdminTarget } from './kubeAdminTypes'

export const KubernetesAdminTargetContext = createContext<{ target: AdminTarget | null; setTarget: Dispatch<SetStateAction<AdminTarget | null>> } | null>(null)

export function useKubernetesAdminTarget() {
  const value = useContext(KubernetesAdminTargetContext)
  if (!value) throw new Error('Kubernetes resource selection requires its in-memory provider.')
  return value
}
