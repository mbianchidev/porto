import { useRef, useState } from 'react'
import { apiSend, errorMessage } from '../api'
import { setDockerMode, useDockerMode, type DockerMode } from '../dockerMode'
import { useMessages } from '../useMessages'
import { useWindowsContainerStatus, windowsRuntimeChanged } from '../windowsContainers'

export function WindowsContainersSettings() {
  const state = useWindowsContainerStatus()
  const { notifyError, notifyNotice } = useMessages()
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState('')
  const pending = useRef(false)
  const status = state.data
  const ready = status?.running && status.owned

  async function perform(action: 'install' | 'start' | 'stop' | 'remove' | 'context') {
    if (pending.current) return
    const descriptions = {
      install: 'Install or repair the separately owned native Windows containerd/runhcs runtime with administrator authorization? Windows containers have a separate store. Linux engines, workloads, WSL distributions and Docker contexts are not replaced. Missing Windows features require manual setup; Porto does not reboot the host.',
      start: 'Start only Porto\'s owned native Windows runtime? The Linux engine and its workloads remain unchanged.',
      stop: 'Stop only the native Windows runtime? Active native workloads must be stopped explicitly first. Linux workloads and all stored data remain unchanged.',
      remove: 'Remove only Porto\'s owned native Windows runtime integration? Native and Linux images, containers, volumes and WSL tooling are preserved. Active native workloads prevent removal.',
      context: 'Explicitly select the porto-windows Docker context for subsequent Windows Docker CLI commands? Existing contexts and Linux workloads are preserved. Opted-in WSL tooling continues using the separate Linux endpoint.',
    }
    if (!window.confirm(descriptions[action])) return
    pending.current = true
    setBusy(true)
    setFailure('')
    try {
      await apiSend(`/api/integrations/windows-containers/${action}`, 'POST', { confirm: true })
      state.reload()
      windowsRuntimeChanged()
      notifyNotice('windows-containers', `Native Windows runtime ${action} completed.`)
    } catch (error) {
      const message = errorMessage(error, `Native Windows runtime ${action} failed.`)
      setFailure(message)
      notifyError('windows-containers', message)
    } finally {
      pending.current = false
      setBusy(false)
    }
  }

  function view(mode: DockerMode) {
    try {
      setDockerMode(mode)
      window.location.hash = '#/containers'
    } catch (error) {
      notifyError('windows-containers', errorMessage(error, 'Unable to save the selected container view.'))
    }
  }

  return (
    <section className="integration platformSettings" aria-labelledby="windows-containers-title">
      <div className="hygieneIntro">
        <h2 id="windows-containers-title">Native Windows containers</h2>
        <p>An explicitly selected containerd/runhcs backend with its own protected Windows service, private named pipe and image/container store. It does not replace Lima/QEMU or WSL tooling.</p>
        <p className="hintLine">Windows edition, architecture, installed features, image build and isolation are checked before mutation. Host-process containers and incompatible image/host combinations are not fallback modes.</p>
      </div>
      <div className="hygieneControls">
        {(failure || state.error) && <p className="errorLine" role="alert">{failure || state.error}</p>}
        {state.loading && !status && <p role="status">Inspecting native Windows runtime capabilities...</p>}
        {status && (
          <>
            <p role="status">{status.message || (ready ? 'Native Windows runtime is ready.' : 'Native Windows runtime is not running.')}</p>
            <p className="hintLine">{status.host.edition || status.host.os}, {status.host.architecture}, build {status.host.build || 'unavailable'}.</p>
            {status.supported && <p className="hintLine">
              Process isolation: {status.capabilities.process.supported ? 'available' : status.capabilities.process.reason || 'unavailable'}.
              {' '}Hyper-V isolation: {status.capabilities.hyperv.supported ? 'available' : status.capabilities.hyperv.reason || 'unavailable'}.
            </p>}
            {status.endpoint && <p className="hintLine">Explicit Docker context: <code>{status.context}</code>; endpoint: <code>{status.endpoint}</code>.</p>}
          </>
        )}
        <div className="platformActions">
          <button type="button" disabled={busy || !status?.supported || (status.installed && !status.owned)} onClick={() => void perform('install')}>
            {status?.installed ? 'Repair native Windows runtime' : 'Install native Windows runtime'}
          </button>
          <button type="button" disabled={busy || !status?.installed || !status.owned || status.running} onClick={() => void perform('start')}>Start Windows runtime</button>
          <button type="button" disabled={busy || !ready} onClick={() => void perform('stop')}>Stop Windows runtime</button>
          <button type="button" className="destructiveAction" disabled={busy || !status?.installed || !status.owned} onClick={() => void perform('remove')}>Remove Windows runtime</button>
          <button type="button" disabled={busy || !ready} onClick={() => void perform('context')}>Select Windows Docker context</button>
          <button type="button" disabled={busy || !ready} onClick={() => view('windows')}>View Windows containers</button>
          <button type="button" disabled={busy} onClick={() => view('linux')}>View Linux containers</button>
          <button type="button" disabled={busy} onClick={state.reload}>Refresh Windows runtime</button>
        </div>
      </div>
    </section>
  )
}

export function ContainerModeSelector() {
  const mode = useDockerMode()
  const native = useWindowsContainerStatus()
  const { notifyError, notifyNotice } = useMessages()

  function select(next: string) {
    if ((next !== 'linux' && next !== 'windows') || next === mode) return
    if (!window.confirm(`Switch this dashboard to ${next === 'windows' ? 'Windows' : 'Linux'} containers? Open inspectors and terminals disconnect. Neither engine nor its workloads stop. CLI contexts and WSL selection remain unchanged.`)) return
    try {
      setDockerMode(next)
      notifyNotice('containers', `Viewing ${next === 'windows' ? 'native Windows' : 'Linux'} containers. Both stores are preserved.`)
    } catch (error) {
      notifyError('containers', errorMessage(error, 'Unable to save the selected container view.'))
    }
  }

  return (
    <section className="platformActions" aria-label="Container backend selection">
      <label className="settingsField">
        <span>Container backend view</span>
        <select value={mode} onChange={event => select(event.target.value)}>
          <option value="linux">Linux containers</option>
          <option value="windows" disabled={!native.data?.running || !native.data.owned}>Native Windows containers</option>
        </select>
      </label>
      <p className="hintLine">Separate stores and endpoints. <a href="#/settings">Configure native Windows support</a>.</p>
    </section>
  )
}
