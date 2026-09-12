import { KubernetesWorkloadDashboard } from '../components/KubernetesWorkloadDashboard'
import type { KubernetesContext, KubernetesCronJob, LampState } from '../types'

function cronJobLamp(cronJob: KubernetesCronJob): LampState {
  return cronJob.suspended ? 'neutral' : 'running'
}

export function KubernetesCronJobs({
  context,
  contexts,
  onContextChange,
}: {
  context: string
  contexts: KubernetesContext[]
  onContextChange: (context: string) => void
}) {
  return (
    <KubernetesWorkloadDashboard<KubernetesCronJob>
      context={context}
      contexts={contexts}
      onContextChange={onContextChange}
      endpoint="cronjobs"
      cacheKey="cronjobs"
      signalTitle="CronJob signal"
      singularLabel="CronJob"
      pluralLabel="CronJobs"
      ariaLabel="Kubernetes CronJobs"
      filterPlaceholder="Filter CronJobs by name, schedule, or state"
      emptyMessage="No CronJobs found in this namespace."
      columnsTemplate="12px minmax(150px,1fr) minmax(105px,0.65fr) minmax(150px,0.9fr) minmax(90px,0.5fr) minmax(70px,0.4fr) minmax(100px,0.6fr) minmax(65px,0.35fr)"
      getKey={(cronJob) => `${cronJob.namespace}/${cronJob.name}`}
      getLamp={cronJobLamp}
      getLampLabel={(cronJob) => cronJob.state}
      matchesQuery={(cronJob, query) => [
        cronJob.name,
        cronJob.namespace,
        cronJob.schedule,
        cronJob.state,
        cronJob.concurrencyPolicy,
      ].some((value) => value.toLocaleLowerCase().includes(query))}
      columns={[
        { header: 'Name', render: (cronJob) => <strong>{cronJob.name}</strong> },
        { header: 'Namespace', className: 'mono', render: (cronJob) => cronJob.namespace },
        { header: 'Schedule', className: 'mono', render: (cronJob) => cronJob.schedule },
        { header: 'State', className: 'mono', render: (cronJob) => cronJob.state },
        { header: 'Active', className: 'mono', render: (cronJob) => cronJob.active },
        { header: 'Last run', className: 'mono', render: (cronJob) => cronJob.lastSchedule || 'never' },
        { header: 'Age', className: 'mono', render: (cronJob) => cronJob.age },
      ]}
      getInspectorTitle={(cronJob) => cronJob.name}
      getInspectorSubtitle={(cronJob) => `${cronJob.namespace} · CronJob · ${cronJob.state}`}
      renderInspector={(cronJob) => (
        <section className="drawerPanel">
          <h3>CronJob detail</h3>
          <dl className="runtimeGrid">
            <div><dt>Schedule</dt><dd>{cronJob.schedule}</dd></div>
            <div><dt>State</dt><dd>{cronJob.state}</dd></div>
            <div><dt>Suspended</dt><dd>{cronJob.suspended ? 'yes' : 'no'}</dd></div>
            <div><dt>Active jobs</dt><dd>{cronJob.active}</dd></div>
            <div><dt>Concurrency</dt><dd>{cronJob.concurrencyPolicy}</dd></div>
            <div><dt>Last scheduled</dt><dd>{cronJob.lastSchedule || 'never'}</dd></div>
            <div><dt>Last successful</dt><dd>{cronJob.lastSuccessful || 'never'}</dd></div>
            <div><dt>Successful history</dt><dd>{cronJob.successfulHistory}</dd></div>
            <div><dt>Failed history</dt><dd>{cronJob.failedHistory}</dd></div>
            <div><dt>Age</dt><dd>{cronJob.age}</dd></div>
          </dl>
        </section>
      )}
    />
  )
}
