export type AnalysisDatabase = {
  state: string
  schemaVersion?: string
  built?: string
  checksum?: string
  location: string
  bytes: number
  maxAgeHours: number
  offline: boolean
  message?: string
}

export type AnalysisPackage = {
  id: string
  name: string
  version: string
  type: string
  purl?: string
  licenses: string[]
  locations: string[]
  layers: string[]
}

export type AnalysisFinding = {
  id: string
  packageId: string
  package: string
  version: string
  severity: string
  fixState: string
  fixedVersions: string[]
  advisory?: string
  namespace?: string
  matchTypes: string[]
  remediation: string
}

export type ImageAnalysisReport = {
  id: string
  cacheKey?: string
  sbomId?: string
  image: { digest: string; platform: string; manifestDigest?: string }
  status: string
  phase: string
  startedAt: string
  completedAt?: string
  syftVersion?: string
  grypeVersion?: string
  database: AnalysisDatabase
  packages?: AnalysisPackage[]
  findings?: AnalysisFinding[]
  layers?: string[]
  distribution?: string
  baseImage: string
  warnings?: string[]
  error?: string
  cached: boolean
}
