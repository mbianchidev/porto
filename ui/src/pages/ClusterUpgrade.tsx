import { useState, type FormEvent } from 'react'
import { apiSend, errorMessage } from '../api'
import { useMessages } from '../useMessages'
import type { KubernetesCluster } from '../types'

export function ClusterUpgrade({
  cluster, disabled, begin, end, onChanged,
}: {
  cluster: KubernetesCluster
  disabled: boolean
  begin: () => boolean
  end: () => void
  onChanged: () => void
}) {
  const { notifyError, notifyNotice } = useMessages()
  const [version, setVersion] = useState(cluster.upgrade?.version ?? '')
  const [working, setWorking] = useState(false)
  const failed = cluster.state === 'upgrade-failed'
  const kind = cluster.provider === 'kind'
  const recoveryRequired = kind && failed && Boolean(cluster.upgrade?.backupPath)
  const locked = disabled || working || !['running', 'upgrade-failed'].includes(cluster.state)
  const example = kind ? 'v1.37.0' : cluster.provider === 'k0s' ? 'v1.36.4+k0s.1' : 'v1.37.1+k3s1'

  async function perform(recover: boolean) {
    const description = recover
      ? `Stop the failed replacement and restart the original ${cluster.name} cluster? Changes made in the replacement will not be merged.`
      : kind
        ? `Upgrade ${cluster.name} to ${version.trim()}? Porto will stop the cluster, preserve supported persistent volumes and application manifests, and restore them in a replacement cluster. There will be downtime. Unsupported storage blocks the upgrade before nodes stop.`
        : `Upgrade ${cluster.name} to ${version.trim()}? The control plane restarts first, followed by one worker at a time. There will be a brief API outage. Drains respect disruption budgets and local data.`
    if (!window.confirm(description) || !begin()) return
    setWorking(true)
    try {
      const result = await apiSend<{ message?: string }>(
        `/api/kubernetes/clusters/${encodeURIComponent(cluster.name)}/${recover ? 'upgrade-recover' : 'upgrade'}`,
        'POST', recover ? { confirm: true } : { version: version.trim(), confirm: true },
      )
      notifyNotice('kubernetes', result.message || `${cluster.name} recovered.`)
      onChanged()
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, `Unable to ${recover ? 'recover' : 'upgrade'} ${cluster.name}`))
      onChanged()
    } finally {
      setWorking(false)
      end()
    }
  }

  function upgrade(event: FormEvent) {
    event.preventDefault()
    void perform(false)
  }

  return (
    <section className="drawerPanel clusterMaintenance">
      <h3>Upgrade Kubernetes</h3>
      <p className="hintLine">Configured version: {cluster.version || 'Unpinned; Porto checks the installed version before upgrading.'}</p>
      <p>{kind
        ? 'kind upgrades replace the cluster. Porto preserves local-path persistent volumes, reattaches claims, and redeploys applications. The original stopped nodes and a private recovery backup remain available.'
        : 'Upgrade the control plane first, then drain and upgrade workers one at a time. Service configuration and persistent disks stay in place. Minor versions cannot be skipped.'}</p>
      {cluster.upgrade?.error && <p className="errorLine" role="alert">{cluster.upgrade.error}</p>}
      {cluster.upgrade?.backupPath && <p className="hintLine">Private recovery backup: <code>{cluster.upgrade.backupPath}</code></p>}
      {recoveryRequired ? (
        <>
          <p>Recover the original cluster before attempting another upgrade.</p>
          <button type="button" disabled={locked} onClick={() => void perform(true)}>Recover original cluster</button>
        </>
      ) : (
        <form className="inspectorForm" onSubmit={upgrade}>
          <label>
            <span>Target {cluster.provider} release</span>
            <input
              type="text" value={version} placeholder={example} required disabled={locked}
              onChange={(event) => setVersion(event.target.value)}
              aria-describedby="cluster-upgrade-version-help"
            />
          </label>
          <p id="cluster-upgrade-version-help" className="hintLine">Use a full distribution release, for example {example}. Downgrades and moving channels are not supported.</p>
          <button type="submit" disabled={locked || version.trim() === ''}>{working ? 'Upgrading cluster...' : failed ? 'Retry upgrade' : 'Upgrade cluster'}</button>
        </form>
      )}
      {working && <p className="hintLine" role="status">Upgrade continues in the daemon if this inspector closes. Other cluster mutations are locked until it finishes.</p>}
    </section>
  )
}
