import type { KubernetesCluster } from '../types'
import { InteractiveTerminal } from './VMTerminal'

function clusterTerminalSocketURL(clusterName: string): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${protocol}//${window.location.host}/api/kubernetes/clusters/${encodeURIComponent(clusterName)}/terminal`
}

export default function ClusterTerminal({ cluster }: { cluster: KubernetesCluster }) {
  const running = cluster.state.toLocaleLowerCase() === 'running'

  return (
    <>
      <section className="drawerPanel">
        <h3>k9s quick guide</h3>
        <p className="hintLine">This terminal is already scoped to {cluster.context} and all namespaces.</p>
        <p className="hintLine">If the embedded PTY is unavailable, run <span className="mono">porto kubernetes terminal {cluster.name}</span> in a local terminal.</p>
        <dl className="runtimeGrid">
          <div><dt>Help</dt><dd className="mono">?</dd></div>
          <div><dt>Contexts</dt><dd className="mono">:ctx</dd></div>
          <div><dt>Namespaces</dt><dd className="mono">:ns</dd></div>
          <div><dt>Pods</dt><dd className="mono">:pods</dd></div>
          <div><dt>Logs</dt><dd className="mono">l</dd></div>
          <div><dt>Shell</dt><dd className="mono">s</dd></div>
          <div><dt>Quit view</dt><dd className="mono">Esc</dd></div>
          <div><dt>Quit k9s</dt><dd className="mono">Ctrl+C</dd></div>
        </dl>
      </section>
      <InteractiveTerminal
        endpoint={clusterTerminalSocketURL(cluster.name)}
        title="k9s terminal"
        detail={cluster.context}
        running={running}
        ariaLabel={`k9s terminal for ${cluster.name}`}
        stoppedMessage="Start the cluster to open k9s."
      />
    </>
  )
}
