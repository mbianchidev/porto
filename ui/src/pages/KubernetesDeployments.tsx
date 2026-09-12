import { KubernetesWorkloadDashboard } from '../components/KubernetesWorkloadDashboard'
import type { KubernetesContext, KubernetesDeployment, LampState } from '../types'

function deploymentLamp(deployment: KubernetesDeployment): LampState {
  switch (deployment.state) {
    case 'available':
      return 'running'
    case 'progressing':
    case 'pending':
      return 'starting'
    case 'degraded':
      return 'crashed'
    default:
      return 'neutral'
  }
}

export function KubernetesDeployments({
  context,
  contexts,
  onContextChange,
}: {
  context: string
  contexts: KubernetesContext[]
  onContextChange: (context: string) => void
}) {
  return (
    <KubernetesWorkloadDashboard<KubernetesDeployment>
      context={context}
      contexts={contexts}
      onContextChange={onContextChange}
      endpoint="deployments"
      cacheKey="deployments"
      signalTitle="Deployment signal"
      singularLabel="deployment"
      pluralLabel="deployments"
      ariaLabel="Kubernetes deployments"
      filterPlaceholder="Filter deployments by name, state, or strategy"
      emptyMessage="No deployments found in this namespace."
      columnsTemplate="12px minmax(150px,1.1fr) minmax(110px,0.7fr) minmax(75px,0.4fr) minmax(75px,0.4fr) minmax(80px,0.45fr) minmax(110px,0.65fr) minmax(65px,0.35fr)"
      getKey={(deployment) => `${deployment.namespace}/${deployment.name}`}
      getLamp={deploymentLamp}
      getLampLabel={(deployment) => deployment.state}
      matchesQuery={(deployment, query) => [
        deployment.name,
        deployment.namespace,
        deployment.state,
        deployment.strategy,
        deployment.reason,
      ].some((value) => value?.toLocaleLowerCase().includes(query))}
      columns={[
        { header: 'Name', render: (deployment) => <strong>{deployment.name}</strong> },
        { header: 'Namespace', className: 'mono', render: (deployment) => deployment.namespace },
        { header: 'Ready', className: 'mono', render: (deployment) => `${deployment.ready}/${deployment.desired}` },
        { header: 'Updated', className: 'mono', render: (deployment) => deployment.updated },
        { header: 'Available', className: 'mono', render: (deployment) => deployment.available },
        { header: 'Strategy', className: 'mono', render: (deployment) => deployment.strategy },
        { header: 'Age', className: 'mono', render: (deployment) => deployment.age },
      ]}
      getInspectorTitle={(deployment) => deployment.name}
      getInspectorSubtitle={(deployment) => `${deployment.namespace} · Deployment · ${deployment.state}`}
      renderInspector={(deployment) => (
        <section className="drawerPanel">
          <h3>Deployment detail</h3>
          <dl className="runtimeGrid">
            <div><dt>State</dt><dd>{deployment.state}</dd></div>
            <div><dt>Strategy</dt><dd>{deployment.strategy}</dd></div>
            <div><dt>Desired</dt><dd>{deployment.desired}</dd></div>
            <div><dt>Current</dt><dd>{deployment.current}</dd></div>
            <div><dt>Updated</dt><dd>{deployment.updated}</dd></div>
            <div><dt>Ready</dt><dd>{deployment.ready}</dd></div>
            <div><dt>Available</dt><dd>{deployment.available}</dd></div>
            <div><dt>Unavailable</dt><dd>{deployment.unavailable}</dd></div>
            <div><dt>Age</dt><dd>{deployment.age}</dd></div>
          </dl>
          {(deployment.reason || deployment.message) && (
            <>
              <h3>Rollout condition</h3>
              <p className={deployment.state === 'degraded' ? 'errorLine' : 'hintLine'}>
                {[deployment.reason, deployment.message].filter(Boolean).join(': ')}
              </p>
            </>
          )}
        </section>
      )}
    />
  )
}
