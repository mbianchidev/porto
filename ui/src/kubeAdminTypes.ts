export type AdminTarget = {
  context: string
  namespace: string
  group: string
  version: string
  resource: string
  name: string
  uid?: string
  resourceVersion?: string
}

export type AdminResource = {
  group: string
  version: string
  resource: string
  kind: string
  namespaced: boolean
  verbs: string[]
}

export type AdminDiscovery = { resources: AdminResource[]; partial: boolean; message?: string }
export type AdminAccess = { target: AdminTarget; verbs: Record<string, { allowed: boolean; reason?: string }> }
export type AdminObject = { target: AdminTarget; kind: string; manifest: Record<string, unknown>; secret: boolean; revealed: boolean }
export type AdminObjectList = { resource: AdminResource; items: AdminObject[]; continue?: string }
export type AdminSchema = { state: string; version?: string; definition?: Record<string, unknown>; message?: string }
export type AdminPreview = {
  token: string
  context: string
  namespace: string
  differences: Array<{ target: AdminTarget; kind: string; before: unknown; after: unknown; managers: string[] }>
  warnings: string[]
  expiresAt: string
}
export type RolloutRevision = { revision: number; createdAt: string; images: string[] }
export type HelmRepository = { name: string; url: string; credentialStored: boolean }
export type HelmRelease = { name: string; namespace: string; revision: number; status: string; chart: string; appVersion: string; updated: string; hooks?: Array<{ name: string; kind: string; phase: string }> }
export type HelmPreview = {
  token: string
  context: string
  namespace: string
  release: string
  action: string
  revision: number
  chartId?: string
  before: unknown
  after: unknown
  warnings: string[]
  storageConsequences: string[]
  expiresAt: string
}

export function adminQuery(target: AdminTarget): string {
  const query = new URLSearchParams()
  for (const [key, value] of Object.entries(target)) if (typeof value === 'string') query.set(key, value)
  return query.toString()
}

export function resourceKey(resource: AdminResource): string {
  return `${resource.group}/${resource.version}/${resource.resource}`
}

export function resourceActions(resource: AdminResource | undefined): string[] {
  if (!resource) return []
  if (resource.group === 'apps') {
    if (resource.resource === 'deployments') return ['scale', 'restart', 'pause', 'resume', 'rollback', 'delete']
    if (resource.resource === 'statefulsets') return ['scale', 'restart', 'rollback', 'delete']
    if (resource.resource === 'daemonsets') return ['restart', 'rollback', 'delete']
  }
  if (resource.group === 'batch') {
    if (resource.resource === 'jobs') return ['scale', 'suspend', 'resume', 'delete']
    if (resource.resource === 'cronjobs') return ['suspend', 'resume', 'delete']
  }
  return ['delete']
}
