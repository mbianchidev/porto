export type LocalOperation = {
  id: string
  resource: string
  action: string
  status: string
  phase: string
  error?: string
  startedAt: string
  completedAt?: string
}

export type DeliverySource = {
  kind: 'editor' | 'upload' | 'template' | 'git'
  git: { repository: string; ref: string; path: string }
  template?: string
  templateVersion?: number
}

export type DeliveryRequest = {
  projectId: number
  name: string
  target: string
  source: DeliverySource
  compose: string
  variables: Record<string, string>
  profiles?: string[]
  secrets?: Record<string, string>
  gitToken?: string
  pollSeconds: number
  webhookEnabled: boolean
  webhookSecret?: string
  detachGit?: boolean
  confirmDrift?: boolean
}

export type DeliveryConfiguration = {
  variables: Record<string, string>
  profiles?: string[]
}

export type DeliveryRevision = {
  id: string
  operationId: string
  commit?: string
  configDigest: string
  effectiveDigest: string
  images: Record<string, string>
  createdAt: string
  rollbackOf?: string
  warnings?: string[]
  profiles?: string[]
}

export type DeliveryApplication = {
  projectId: number
  name: string
  target: string
  source: DeliverySource
  profiles?: string[]
  currentRevision: string
  lastOperation?: string
  pollSeconds: number
  webhookEnabled: boolean
  webhookUrl?: string
  nextPollAt?: string
  failures: number
  lastError?: string
  drift: boolean
  revisions: DeliveryRevision[]
}

export type DeliveryPreview = {
  token: string
  projectId: number
  baseRevision: string
  commit?: string
  before: string
  after: string
  warnings: string[]
  profiles?: string[]
  expiresAt: string
}

export type DeliveryTemplate = {
  name: string
  version: number
  description: string
  compose: string
  required: string[]
  secretNames: string[]
}
