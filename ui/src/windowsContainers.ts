import { useEffect } from 'react'
import { apiGet } from './api'
import { usePolledResource } from './hooks'

export type WindowsContainerStatus = {
  supported: boolean
  installed: boolean
  running: boolean
  owned: boolean
  context: string
  endpoint: string
  message?: string
  host: {
    os: string
    architecture: string
    build: number
    edition: string
    server: boolean
    containers: boolean
    hyperV: boolean
    virtualization: boolean
    problem?: string
  }
  capabilities: {
    process: { supported: boolean; reason?: string }
    hyperv: { supported: boolean; reason?: string }
  }
}

const changeEvent = 'porto:windows-runtime-changed'

export function windowsRuntimeChanged() { window.dispatchEvent(new Event(changeEvent)) }

export function useWindowsContainerStatus() {
  const resource = usePolledResource(async signal => {
    const status = await apiGet<WindowsContainerStatus>('/api/integrations/windows-containers', signal)
    if (!status || typeof status.supported !== 'boolean' || typeof status.installed !== 'boolean' ||
        typeof status.running !== 'boolean' || typeof status.owned !== 'boolean' || !status.host ||
        typeof status.capabilities?.process?.supported !== 'boolean' || typeof status.capabilities?.hyperv?.supported !== 'boolean') {
      throw new Error('Native Windows runtime state is unavailable. Restart the matching Porto daemon and refresh.')
    }
    return status
  }, 15000, [], 'platform:windows-containers')
  useEffect(() => {
    window.addEventListener(changeEvent, resource.reload)
    return () => window.removeEventListener(changeEvent, resource.reload)
  }, [resource.reload])
  return resource
}
