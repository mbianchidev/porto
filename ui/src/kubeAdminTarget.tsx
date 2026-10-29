import { createContext, useContext, useState } from 'react'
import type { Dispatch, ReactNode, SetStateAction } from 'react'
import type { AdminTarget } from './kubeAdminTypes'

const TargetContext = createContext<{ target: AdminTarget | null; setTarget: Dispatch<SetStateAction<AdminTarget | null>> } | null>(null)

export function KubernetesAdminTargetProvider({ children }: { children: ReactNode }) {
  const [target, setTarget] = useState<AdminTarget | null>(null)
  return <TargetContext.Provider value={{ target, setTarget }}>{children}</TargetContext.Provider>
}

export function useKubernetesAdminTarget() {
  const value = useContext(TargetContext)
  if (!value) throw new Error('Kubernetes resource selection requires its in-memory provider.')
  return value
}
