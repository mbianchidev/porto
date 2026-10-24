export type ProxyConfig = {
  mode: 'system' | 'manual' | 'off'
  httpProxy: string
  httpsProxy: string
  socksProxy: string
  noProxy: string
  registryCAs: string[]
  clearCredentials?: boolean
}

export type ProxyStatus = ProxyConfig & {
  source: string
  credentialsStored: Record<string, boolean>
  message?: string
}

export type NetworkStatus = {
  platform: string
  https: { supported: boolean; installed: boolean; listening: boolean; trusted: boolean; message?: string }
  proxy: ProxyStatus
  proxyAvailable: boolean
  routes: Array<{ containerId: string; name: string; hostname: string; httpURL: string; httpsURL: string }>
  warnings: string[]
  engine: { configured: boolean; needsApply: boolean; backend: string } | null
  engineError: string
}

export type ConnectivityReport = {
  host: {
    subnets: Array<{ cidr: string; interface: string; source: string }>
    localhostAddresses: string[]
    warnings: string[]
  }
  containerDirectIP: { supported: boolean; message: string }
  vmDirectIP: { supported: boolean; message: string }
  networks: Array<{
    name: string
    subnets: string[]
    overlaps: Array<{ subnet: string; hostCIDR: string; interface: string }>
  }>
  virtualMachines: Array<{ name: string; addresses: string[]; sshLocalPort: number }> | null
}

export type WSLStatus = {
  supported: boolean
  message?: string
  distributions: Array<{ name: string; id: string; version: number; running: boolean; enabled: boolean; message?: string }>
  integrations: Array<{
    distribution: string
    id: string
    owner: string
    architecture: string
    home: string
    shares: Array<{ linuxPath: string; windowsPath: string; guestPath: string }>
    bridge: { distribution: string; owner: string; windowsExecutable: string; windowsHome: string; socket: string }
  }>
}
