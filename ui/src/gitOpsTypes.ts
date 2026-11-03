import type { AdminTarget } from './kubeAdminTypes'

export type GitOpsScope = { context: string; namespace: string; controller: 'flux' | 'argocd'; controllerNamespace: string }
export type GitOpsController = GitOpsScope & {
  installed: boolean; supported: boolean; ready: boolean; managed: boolean; protected: boolean
  canWrite: boolean; canBootstrap: boolean; bootstrapVersion: string; capabilities: string[]; message: string
}
export type GitOpsSource = GitOpsScope & { name: string; kind: 'git' | 'helm'; url: string; credentialStored: boolean }
export type GitOpsConfiguration = { path?: string; chart?: string; renderer?: string; serviceAccount?: string; project?: string; dependencies: string[] }
export type GitOpsApplication = GitOpsScope & {
  target: AdminTarget; name: string; kind: string; source?: string
  desiredRevision: string; sourceRevision: string; appliedRevision: string
  phase: string; health: string; drift: string; failureStage?: string
  conditions: Array<{ type: string; status: string; reason: string; transitionAt?: string }>
  dependenciesReady: boolean; lastSuccess?: string; automatic: boolean; selfHeal: boolean; prune: boolean
  managed: boolean; adoption?: string; message: string; configuration: GitOpsConfiguration
  inventory: Array<{ group: string; version: string; kind: string; namespace: string; name: string }>
}
export type GitOpsPreview = GitOpsScope & {
  token: string; action: string; name: string; kind: string; expiresAt: string
  differences: Array<{ target: AdminTarget; kind: string; before: unknown; after: unknown; managers: string[] }>
  warnings: string[]; consequences: string[]
}

export function gitOpsQuery(scope: GitOpsScope) {
  return new URLSearchParams(scope).toString()
}
