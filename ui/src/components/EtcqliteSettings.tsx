import { useRef, useState, type FormEvent } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import type { EtcqliteBackup, EtcqliteInstance, EtcqliteStatus } from '../types'

export function EtcqliteSettings({
  enabled, savedEnabled, disabled, saveDisabled, onChange, onSave,
}: {
  enabled: boolean
  savedEnabled: boolean
  disabled: boolean
  saveDisabled: boolean
  onChange: (enabled: boolean) => void
  onSave: () => void
}) {
  const { notifyError, notifyNotice } = useMessages()
  const status = usePolledResource<EtcqliteStatus>((signal) => apiGet('/api/integrations/etcqlite', signal), 10000, [savedEnabled])
  const backups = usePolledResource<EtcqliteBackup[]>(
    (signal) => savedEnabled ? apiGet('/api/integrations/etcqlite/backups', signal) : Promise.resolve([]),
    10000, [savedEnabled],
  )
  const [name, setName] = useState('')
  const [members, setMembers] = useState(1)
  const [advertiseHost, setAdvertiseHost] = useState('127.0.0.1')
  const [listenHost, setListenHost] = useState('127.0.0.1')
  const [busy, setBusy] = useState('')
  const pending = useRef(false)
  const instances = status.data?.instances ?? []
  const manageDisabled = !savedEnabled || !enabled || disabled || !status.data?.supported || status.data.state === 'disabled' || !!status.error || busy !== ''
  const retainedBackups = (backups.data ?? []).filter((backup, index, all) => (
    !instances.some((instance) => instance.name === backup.name) &&
    all.findIndex((other) => other.name === backup.name && other.clusterID === backup.clusterID) === index
  ))

  function reload() {
    status.reload()
    backups.reload()
  }

  async function initialize(event: FormEvent) {
    event.preventDefault()
    if (pending.current) return
    pending.current = true
    setBusy('initialize')
    try {
      await apiSend<EtcqliteInstance>('/api/integrations/etcqlite/instances', 'POST', {
        name: name.trim(), members, advertiseHost: advertiseHost.trim(), listenHost: listenHost.trim(),
      })
      setName('')
      notifyNotice('settings', 'Etcqlite initialized with private node-local data. Start it explicitly.')
      reload()
    } catch (err) {
      notifyError('settings', errorMessage(err, 'Etcqlite initialization failed'))
      reload()
    } finally {
      pending.current = false
      setBusy('')
    }
  }

  async function action(instance: Pick<EtcqliteInstance, 'name' | 'clusterID'>, verb: 'start' | 'stop' | 'backup' | 'delete' | 'restore') {
    if (pending.current) return
    const confirm = verb === 'delete' || verb === 'restore'
    if (confirm && !window.confirm(verb === 'delete'
      ? `Verify native recovery images, stop ${instance.name}, and retain its node data for reversible removal? k3s bindings prevent removal.`
      : `Restore retained ${instance.name} with its original identity? Existing instances are never overwritten and members are not started.`)) return
    pending.current = true
    setBusy(`${verb}:${instance.name}`)
    try {
      const path = `/api/integrations/etcqlite/instances/${encodeURIComponent(instance.name)}${verb === 'delete' ? '' : `/${verb}`}`
      const result = await apiSend<{ path?: string; message?: string }>(path, verb === 'delete' ? 'DELETE' : 'POST', {
        clusterID: instance.clusterID, ...(confirm ? { confirm: true } : {}),
      })
      notifyNotice('settings', verb === 'backup' && result.path
        ? `Verified native recovery images saved privately at ${result.path}.`
        : result.message || `Etcqlite ${verb} completed for ${instance.name}.`)
      reload()
    } catch (err) {
      notifyError('settings', errorMessage(err, `Etcqlite ${verb} failed`))
      reload()
    } finally {
      pending.current = false
      setBusy('')
    }
  }

  return (
    <section className="integration etcqliteIntegration" aria-labelledby="etcqlite-integration-title">
      <div className="hygieneIntro">
        <h2 id="etcqlite-integration-title">Optional Etcqlite datastore</h2>
        <p>
          Explicitly manage <a className="integrationRepoLink" href="https://github.com/mbianchidev/etcqlite" target="_blank" rel="noreferrer">Etcqlite</a> members
          and select their external-etcd endpoint only for new k3s clusters. Existing clusters and datastore defaults stay unchanged.
        </p>
        <p>Requires separately installed, matching etcqlite and etcqlite-snapshot binaries on macOS or Linux. No automatic install or project scanning.</p>
        <p>Members use private mTLS and node-local SQLite data. Three members on the same host are a local quorum, not independent failure domains.</p>
      </div>
      <div className="hygieneControls">
        <label className="toggleRow">
          <span><strong>Enable Etcqlite</strong><small>Saving this gate does not initialize or start a datastore.</small></span>
          <input type="checkbox" checked={enabled} disabled={disabled || status.data?.supported === false || busy !== ''} onChange={(event) => onChange(event.target.checked)} />
        </label>
        <div className={`integrationStatus ${status.error ? 'error' : status.data?.state ?? 'idle'}`} role="status">
          <strong>{busy || status.data?.state || 'loading'}</strong>
          <span>{status.error || status.data?.message || 'Loading explicit datastore state.'}</span>
        </div>
        <button type="button" disabled={saveDisabled || busy !== ''} onClick={onSave}>Save integration setting</button>
        <form className="inspectorForm" onSubmit={initialize}>
          <label><span>Etcqlite name</span><input type="text" value={name} pattern="[a-z0-9][a-z0-9-]{0,39}" required disabled={manageDisabled} onChange={(event) => setName(event.target.value)} /></label>
          <label><span>Local members</span><select value={members} disabled={manageDisabled} onChange={(event) => setMembers(Number(event.target.value))}>
            <option value={1}>1 standalone member</option><option value={3}>3 local quorum members</option>
          </select></label>
          <label><span>Advertised client hostname</span><input type="text" value={advertiseHost} required disabled={manageDisabled} onChange={(event) => setAdvertiseHost(event.target.value)} /></label>
          <label><span>Client listen address</span><input type="text" value={listenHost} required disabled={manageDisabled} onChange={(event) => setListenHost(event.target.value)} /></label>
          <p className="hintLine">Loopback stays local. For Lima k3s, explicitly choose host.lima.internal and 0.0.0.0, subject to host firewall/reachability. Peers remain loopback mTLS.</p>
          <button type="submit" disabled={manageDisabled || name.trim() === ''}>{busy === 'initialize' ? 'Initializing...' : 'Initialize Etcqlite'}</button>
        </form>
        {instances.map((instance) => (
          <div className={`integrationStatus ${instance.state}`} key={instance.clusterID}>
            <strong>{instance.name}</strong>
            <span>{instance.state}: {instance.message}</span>
            <span>Cluster identity: <code>{instance.clusterID}</code></span>
            {instance.members.map((member) => <span key={member.id}><code>{member.name} {member.clientURL}</code>{member.message && `: ${member.message}`}</span>)}
            <div className="integrationActions">
              <button type="button" aria-label={`Start ${instance.name}`} disabled={manageDisabled || instance.state === 'ready'} onClick={() => action(instance, 'start')}>Start</button>
              <button type="button" aria-label={`Stop ${instance.name}`} disabled={manageDisabled || instance.state === 'stopped'} onClick={() => action(instance, 'stop')}>Stop</button>
              <button type="button" aria-label={`Back up ${instance.name}`} disabled={manageDisabled} onClick={() => action(instance, 'backup')}>Back up</button>
              <button type="button" className="destructiveAction" aria-label={`Retain and remove ${instance.name}`} disabled={manageDisabled} onClick={() => action(instance, 'delete')}>Retain and remove</button>
            </div>
          </div>
        ))}
        {savedEnabled && (
          <div aria-label="Verified native backups">
            <h3>Verified native backups</h3>
            {backups.error && <p className="errorLine" role="alert">{backups.error}</p>}
            {(backups.data ?? []).map((backup) => (
              <div className="integrationStatus" key={backup.id}>
                <strong>{backup.name}</strong><code>{backup.path}</code>
                {backup.snapshots.map((snapshot) => <span key={snapshot.member}>Native revision {snapshot.status.revision}, payload SHA-256 <code>{snapshot.status.sha256}</code></span>)}
              </div>
            ))}
            {retainedBackups.map((backup) => <button type="button" key={backup.id} disabled={manageDisabled} onClick={() => action(backup, 'restore')}>Restore retained {backup.name}</button>)}
          </div>
        )}
      </div>
    </section>
  )
}
