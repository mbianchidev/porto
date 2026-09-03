// Shared API types mirrored from the Porto daemon (internal/daemon, internal/docker,
// internal/kubernetes, internal/vm). Keep field names and shapes in sync with the Go
// structs that back each endpoint.

export type Project = {
  id: number
  name: string
  path: string
  strategy: string
  command: string
  port: number
  pinnedPort: number
  hostname: string
  baseHostname: string
  httpsUrl: string
  sourcePath: string
  managedInstance: boolean
  defaultBranch: string
  pid: number
  status: string
  branch: string
  dirty: boolean
  autoStart: boolean
  lastStarted?: string
  updatedAt: string
  sendboxConfigured: boolean
  sendboxStatus: string
  sendboxMessage: string
}

export type Settings = {
  cleanupLocalMerged: boolean
  cleanupRemoteMerged: boolean
  pruneRemoteTracking: boolean
  protectedBranches: string[]
  sqlNotSoLiteEnabled: boolean
  killSwitchEnabled: boolean
  sendboxEnabled: boolean
  dockerEnabled: boolean
  kubernetesEnabled: boolean
  vmsEnabled: boolean
}

// Snapshot from GET/POST /api/runtime/features: whether each optional runtime
// gate is on. All three default OFF server-side to preserve native-only
// behavior until a user opts in from Settings.
export type RuntimeFeatures = {
  docker: boolean
  kubernetes: boolean
  vms: boolean
}

export type RuntimeFeatureName = keyof RuntimeFeatures

export type RuntimeProviderStatus = {
  name: 'lima' | 'qemu' | 'kind' | 'k9s' | 'k0s'
  command: string
  installed: boolean
  version?: string
  message?: string
}


export type IntegrationStatus = {
  state: 'disabled' | 'idle' | 'running' | 'ready' | 'error'
  message: string
  updatedAt: string
}

export type KillSwitchStatus = {
  state: 'disabled' | 'idle' | 'checking' | 'missing' | 'installing' | 'syncing' | 'cleaning' | 'ready' | 'error' | 'unsupported'
  message: string
  updatedAt: string
  supported: boolean
  installed: boolean
  binaryPath?: string
  version?: string
  autoKillEnabled: boolean | null
  userPorts: number[]
  syncedPorts: number[]
  effectivePorts: number[]
}

export type KillSwitchCleanupResult = {
  autoKillEnabled: boolean
  candidateCount: number
  killedCount: number
  killedProcesses: Array<{ pid: number }>
}

export type CleanupResult = {
  localDeleted: string[]
  remoteDeleted: string[]
  pruned: boolean
}

export type LogLine = {
  projectId: number
  stream: string
  line: string
  createdAt: string
}

// --- Docker -----------------------------------------------------------------

export type DockerStatus = {
  available: boolean
  enabled: boolean
  context: string
  endpoint: string
  clientVersion: string
  serverVersion: string
  proxySocket?: string
  canonicalPath?: string
  canonicalLink?: string
  canonical: boolean
  previousLink?: string
  message?: string
  namespace?: string
  inventory?: string
  revision?: number
  stale?: boolean
  updatedAt?: string
}

export type DockerContainer = {
  id: string
  name: string
  image: string
  state: string
  status: string
  ports: string
  networks: string
  mounts: string
  createdAt: string
  composeProject?: string
  composeService?: string
  taskPresent: boolean
  pid?: number
  exitCode?: number
  exitSignal?: number
  exitAt?: string
  exitReason?: string
  oomKilled: boolean
  restartPolicy?: string
  restartCount: number
  health: DockerContainerHealth
  resources: DockerContainerResources
  networkDetails?: DockerContainerNetworkState[]
  mountDetails?: DockerContainerMount[]
  annotations?: Record<string, string>
  stopSignal?: string
  stopTimeout?: number
  updatedAt?: string
  lastTransition?: string
  lastTransitionAt?: string
  history?: DockerContainerLifecycleEvent[]
  inventoryError?: string
}

export type DockerContainerHealth = {
  status: string
  failingStreak: number
  output?: string
  updatedAt?: string
}

export type DockerContainerResources = {
  cpuQuota?: number
  cpuPeriod?: number
  cpuShares?: number
  cpuSet?: string
  memoryLimit?: number
  memorySwap?: number
  pidsLimit?: number
}

export type DockerContainerNetworkState = {
  name: string
  hostIp?: string
  hostPort?: number
  containerPort?: number
  protocol?: string
}

export type DockerContainerMount = {
  type?: string
  source?: string
  destination: string
  options?: string[]
}

export type DockerContainerLifecycleEvent = {
  sequence: number
  topic: string
  type: string
  containerId?: string
  execId?: string
  timestamp: string
  exitCode?: number
  exitSignal?: number
  oom?: boolean
  reason?: string
}

export type DockerRuntimeCapability = {
  supported: boolean
  reason?: string
}

export type DockerContainerSnapshot = {
  instanceId: string
  revision: number
  available: boolean
  stale: boolean
  namespace?: string
  backend?: string
  message?: string
  connectedAt?: string
  lastEventAt?: string
  lastReconciledAt?: string
  containers: DockerContainer[]
  events?: DockerContainerLifecycleEvent[]
  capabilities: {
    directInventory: DockerRuntimeCapability
    lifecycleEvents: DockerRuntimeCapability
    checkpointRestore: DockerRuntimeCapability
  }
}

export type DockerImage = {
  id: string
  repository: string
  tag: string
  digest: string
  size: string
  createdAt: string
}

export type DockerContainerCreateRequest = {
  name: string
  image: string
  hostPort: number
  containerPort: number
  healthCommand: string
}

export type DockerContainerCreateResult = {
  id: string
  name: string
  status: string
}

export type DockerNetwork = {
  id: string
  name: string
  driver: string
  scope: string
  internal: string
  ipv6: string
  createdAt: string
}

export type DockerVolume = {
  name: string
  driver: string
  mountpoint: string
  scope: string
  createdAt: string
}

export type DockerBuild = {
  id: string
  name: string
  status: string
  createdAt: string
  duration: string
  platform: string
}

export type DockerBuildRequest = {
  context: string
  dockerfile: string
  tag: string
  target: string
  platform: string
  noCache: boolean
}

export type DockerCreateNetworkRequest = {
  name: string
  driver: string
  subnet: string
  gateway: string
  internal: boolean
}

export type DockerContainerAction = 'start' | 'stop' | 'restart' | 'pause' | 'unpause' | 'remove' | 'remove-force'

// --- Kubernetes ---------------------------------------------------------------

export type KubernetesStatus = {
  available: boolean
  enabled: boolean
  context: string
  clientVersion?: string
  serverVersion?: string
  message?: string
}

export type KubernetesContext = {
  name: string
  cluster: string
  authInfo: string
  namespace: string
  current: boolean
}

export type KubernetesContainer = {
  name: string
  image: string
  ready: boolean
  restartCount: number
  state: string
}

export type KubernetesContainerCapabilities = {
  shells: string[]
  fileInspection: boolean
  message?: string
}

export type KubernetesDebugContainer = {
  name: string
  image: string
  targetContainer: string
  podUID: string
  lifetimeSeconds: number
  ready: boolean
  state: string
  reason?: string
  message?: string
}

export type KubernetesPod = {
  name: string
  namespace: string
  uid: string
  phase: string
  ready: string
  podReady: boolean
  restarts: number
  node: string
  ip: string
  age: string
  containers: KubernetesContainer[]
}

export type KubernetesServicePort = {
  name: string
  protocol: string
  port: number
  targetPort: string
  nodePort?: number
  appProtocol?: string
  localPort?: number
  hostname?: string
  httpUrl?: string
  httpsUrl?: string
  gatewayReady: boolean
  gatewayError?: string
}

export type KubernetesService = {
  name: string
  namespace: string
  type: string
  clusterIP: string
  externalIPs: string[]
  ports: KubernetesServicePort[]
  age: string
}

export type KubernetesConfigMap = {
  name: string
  namespace: string
  immutable: boolean
  keys: string[]
  binaryKeys: string[]
  resourceVersion: string
  age: string
}

export type KubernetesConfigMapDetail = KubernetesConfigMap & {
  labels: Record<string, string>
  annotations: Record<string, string>
  data: Record<string, string>
  binaryData: Record<string, string>
}

export type KubernetesSecret = {
  name: string
  namespace: string
  type: string
  immutable: boolean
  keys: string[]
  age: string
}

export type KubernetesNode = {
  name: string
  ready: boolean
  roles: string[]
  version: string
  internalIP: string
  architecture: string
  capacity: Record<string, string>
  allocatable: Record<string, string>
  unschedulable: boolean
  age: string
}

export type KubernetesEvent = {
  type: string
  reason: string
  message: string
  source: string
  count: number
  firstSeen: string
  lastSeen: string
}

export type KubernetesPodStats = {
  container: string
  cpu: string
  memory: string
}

export type KubernetesFileEntry = {
  name: string
  type: string
  size: number
}

export type KubernetesFileListing = {
  path: string
  entries: KubernetesFileEntry[]
}

export type KubernetesFileContent = {
  path: string
  content: string
  truncated: boolean
}

export type KubernetesMachineSpec = {
  cpus: number
  memoryMiB: number
  diskGiB: number
}

export type KubernetesNodeGroupSpec = {
  name: string
  count: number
  machine: KubernetesMachineSpec
  labels: Record<string, string>
  taints: string[]
}

export type KubernetesClusterRequest = {
  name: string
  provider: 'kind' | 'k0s' | 'k3s'
  version: string
  controlPlane: KubernetesMachineSpec
  nodeGroups: KubernetesNodeGroupSpec[]
}

export type KubernetesScaleNodeGroupRequest = {
  version: string
  count: number
  machine: KubernetesMachineSpec
  labels: Record<string, string>
  taints: string[]
}

// A Porto-provisioned k3s cluster (VM-backed control plane + worker nodes).
// `nodes` is a flat list of Lima VM instance names, not grouped by node group.
export type KubernetesCluster = {
  name: string
  provider: 'kind' | 'k0s' | 'k3s'
  state: string
  context: string
  kubeconfigPath: string
  server: string
  nodes: string[]
}

// --- Virtual machines -----------------------------------------------------------

export type VMStatus = {
  available: boolean
  enabled: boolean
  provider: string
  version?: string
  message?: string
}

export type VMImage = {
  id: string
  distribution: string
  version: string
  template: string
  description: string
  architectures?: string[]
  available: boolean
  message?: string
}

export type VMInstance = {
  name: string
  status: string
  vmType: string
  architecture: string
  cpus: number
  memoryBytes: number
  diskBytes: number
  sshLocalPort: number
  directory: string
  addresses: string[]
  snapshotSupported: boolean
  snapshotMessage?: string
}

export type VMCreateRequest = {
  name: string
  image: string
  vmType: '' | 'qemu'
  cpus: number
  memoryMiB: number
  diskGiB: number
  architecture: string
  provision: string
  start: boolean
}

// --- Shell / navigation ---------------------------------------------------------

export type RouteID =
  | 'localhost-ing'
  | 'containers'
  | 'images'
  | 'builds'
  | 'volumes'
  | 'networks'
  | 'kubernetes'
  | 'pods'
  | 'services'
  | 'configs'
  | 'secrets'
  | 'nodes'
  | 'databases'
  | 'machines'
  | 'activity'
  | 'settings'

export type ActivityLevel = 'info' | 'notice' | 'error'

export type ActivityEntry = {
  id: number
  level: ActivityLevel
  message: string
  source: string
  at: string
}

export type ResourceUsage = {
  cpuMillicores: number
  memoryBytes: number
}

export type ActivityResourceItem = {
  id: string
  name: string
  detail?: string
  usage: ResourceUsage
  countedInTotal: boolean
}

export type ActivityResourceGroup = {
  id: string
  name: string
  total: ResourceUsage
  items: ActivityResourceItem[]
  error?: string
}

export type ActivityResourceSnapshot = {
  collectedAt: string
  total: ResourceUsage
  groups: ActivityResourceGroup[]
  partial: boolean
}

export type LampState = 'running' | 'starting' | 'stopped' | 'crashed' | 'neutral'
