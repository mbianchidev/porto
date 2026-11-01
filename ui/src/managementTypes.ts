export type EnvironmentProfile = {
  id: string
  name: string
  backend: 'local' | 'porto' | 'docker' | 'kubernetes' | 'edge'
  address: string
  identity: string
  certificate?: string
  capabilities: string[]
  labels: Record<string, string>
  state: string
  lastSeen?: string
  lastError?: string
  revision: number
  revoked: boolean
  remoteRevoked: boolean
  context?: string
  owned: boolean
  version?: string
  unsupported?: string[]
}
export type FleetTarget = { environmentId: string; resourceId: string; environmentName?: string; revision: number; state?: string }
export type FleetPreview = { id: string; action: string; targets: FleetTarget[]; confirmation: string; expiresAt: string }
export type FleetRun = { id: string; operation: string; action: string; results: Array<{ target: FleetTarget; status: string; error?: string }> }
export type ExtensionAction = { id: string; title: string; resourceKind?: string; destructive: boolean }
export type ExtensionManifest = {
  apiVersion: string; id: string; name: string; version: string; description?: string; permissions: string[]
  views: Array<{ id: string; title: string; location: 'tools' | 'resource'; resourceKind?: string }>
  actions: ExtensionAction[]
}
export type ExtensionGrants = {
  permissions: string[]; environments: string[]; files: string[]
  commands: Array<{ executable: string; args: string[] }>; secrets: string[]; destinations: string[]
}
export type InstalledExtension = {
  id: string; name: string; version: string; sha256: string; publisher?: string; source: string; enabled: boolean
  approved: boolean; compatible: boolean; generation: number; manifest: ExtensionManifest; grants: ExtensionGrants
  state: string; lastError?: string; lastRun?: string; secretNames?: string[]
}
export type ExtensionPackage = {
  content: { manifest: ExtensionManifest; ui: string; module: string }
  sha256: string; publicKey?: string; signature?: string
}
export type ExtensionCatalogEntry = { id: string; name: string; version: string; description: string; url: string; sha256: string; publisher: string }

export function environmentSupportsRoute(backend: string, route: string) {
  if (['environments', 'extensions', 'activity'].includes(route)) return true
  if (backend === 'local') return true
  if (backend === 'kubernetes') return ['kubernetes-admin', 'helm'].includes(route)
  if (backend === 'docker') return route === 'containers'
  if (backend === 'porto' || backend === 'edge') return !['settings', 'diagnostics', 'docker-storage', 'migration', 'databases'].includes(route)
  return false
}
