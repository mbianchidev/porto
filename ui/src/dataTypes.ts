export type FileResource = {
  kind: 'container' | 'image' | 'volume' | 'vm'
  name: string
  id: string
  createdAt?: string
  backend?: string
  readOnly: boolean
}

export type FileEntry = {
  path: string
  type: string
  size: number
  mode: number
  uid: number
  gid: number
  modifiedAt: string
  linkTarget?: string
}

export type FileListing = {
  resource: FileResource
  identity: string
  path: string
  entries: FileEntry[]
  truncated: boolean
  readOnly: boolean
  message?: string
  mounts?: Array<{ path: string; kind: string; source?: string; readOnly: boolean }>
}

export type FileContent = { path: string; text: string; sha256: string; readOnly: boolean }

export type StatsPoint = {
  read: string
  cpuMillicores?: number
  memoryBytes?: number
  memoryLimit?: number
  pids?: number
  networkRX?: number
  networkTX?: number
  blockRead?: number
  blockWrite?: number
}

export type InspectorStats = {
  available: boolean
  current?: StatsPoint
  history: StatsPoint[]
  message?: string
}

export type JSONValue = string | number | boolean | null | JSONValue[] | { [key: string]: JSONValue }

export type DataRequest = {
  action: string
  resource?: FileResource
  identity?: string
  destination?: string
  archive?: string
  directory?: string
  categories?: string[]
  selections?: Array<{ kind: string; name: string; id: string; destination?: string }>
  context?: string
  includeSensitive?: boolean
  allowSourceHelper?: boolean
  confirm?: boolean
  preview?: string
}

export type DataOperation = {
  id: number
  request: DataRequest
  status: string
  phase: string
  bytes: number
  startedAt: string
  completedAt?: string
  error?: string
  result: {
    archive?: VolumeArchive
    message?: string
    steps?: Array<{ kind: string; source: string; destination: string; status: string; message?: string }>
  }
}

export type VolumeArchive = {
  path: string
  sha256: string
  manifest: string
  bytes: number
  resource: FileResource
  consistency: string
}

export type BackupSchedule = {
  id: number
  resource: FileResource
  enabled: boolean
  intervalHours: number
  retention: number
  directory: string
  nextRunAt: string
}

export type StorageResource = {
  resource: { kind: string; name: string; id: string; createdAt?: string; readOnly: boolean }
  identity: string
  logicalBytes?: number
  allocatedBytes?: number
  sharedBytes?: number
  inUse: boolean
  dangling: boolean
  protected: boolean
  reason?: string
  owners: Array<{ id: string; name: string; state: string; composeProject?: string; composeService?: string }>
}

export type StorageUsage = {
  resources: StorageResource[]
  contentBytes: number
  snapshotBytes: number
  writableBytes: number
  volumeBytes?: number
  buildCacheBytes?: number
  metadataBytes?: number
  warnings: string[]
  accounting: string
}

export type PrunePreview = {
  request: DataRequest
  candidates: StorageResource[]
  excluded: StorageResource[]
  upperBoundBytes?: number
  token: string
  message: string
}

export type VolumePreview = {
  request: DataRequest
  resource: FileResource
  owners: StorageResource['owners']
  destination: string
  archive?: VolumeArchive
  files?: FileEntry[]
  token: string
  consistency: string
  consequences: string
}

export function bytesLabel(bytes: number | undefined): string {
  if (bytes === undefined || bytes < 0) return 'Unavailable'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) { value /= 1024; unit++ }
  return `${value.toFixed(unit === 0 ? 0 : 1)} ${units[unit]}`
}
