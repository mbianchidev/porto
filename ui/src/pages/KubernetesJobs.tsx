import { KubernetesWorkloadDashboard } from '../components/KubernetesWorkloadDashboard'
import type { KubernetesContext, KubernetesJob, LampState } from '../types'

function jobLamp(job: KubernetesJob): LampState {
  switch (job.state) {
    case 'complete':
    case 'running':
      return 'running'
    case 'pending':
      return 'starting'
    case 'failed':
      return 'crashed'
    default:
      return 'neutral'
  }
}

export function KubernetesJobs({
  context,
  contexts,
  onContextChange,
}: {
  context: string
  contexts: KubernetesContext[]
  onContextChange: (context: string) => void
}) {
  return (
    <KubernetesWorkloadDashboard<KubernetesJob>
      context={context}
      contexts={contexts}
      onContextChange={onContextChange}
      endpoint="jobs"
      cacheKey="jobs"
      signalTitle="Job signal"
      singularLabel="job"
      pluralLabel="jobs"
      ariaLabel="Kubernetes jobs"
      filterPlaceholder="Filter jobs by name or state"
      emptyMessage="No jobs found in this namespace."
      columnsTemplate="12px minmax(160px,1.2fr) minmax(110px,0.7fr) minmax(90px,0.5fr) minmax(100px,0.55fr) minmax(70px,0.4fr) minmax(70px,0.4fr) minmax(65px,0.35fr)"
      getKey={(job) => `${job.namespace}/${job.name}`}
      getLamp={jobLamp}
      getLampLabel={(job) => job.state}
      matchesQuery={(job, query) => [
        job.name,
        job.namespace,
        job.state,
        job.reason,
      ].some((value) => value?.toLocaleLowerCase().includes(query))}
      columns={[
        { header: 'Name', render: (job) => <strong>{job.name}</strong> },
        { header: 'Namespace', className: 'mono', render: (job) => job.namespace },
        { header: 'State', className: 'mono', render: (job) => job.state },
        { header: 'Complete', className: 'mono', render: (job) => `${job.succeeded}/${job.completions}` },
        { header: 'Active', className: 'mono', render: (job) => job.active },
        { header: 'Failed', className: 'mono', render: (job) => job.failed },
        { header: 'Age', className: 'mono', render: (job) => job.age },
      ]}
      getInspectorTitle={(job) => job.name}
      getInspectorSubtitle={(job) => `${job.namespace} · Job · ${job.state}`}
      renderInspector={(job) => (
        <section className="drawerPanel">
          <h3>Job detail</h3>
          <dl className="runtimeGrid">
            <div><dt>State</dt><dd>{job.state}</dd></div>
            <div><dt>Suspended</dt><dd>{job.suspended ? 'yes' : 'no'}</dd></div>
            <div><dt>Completions</dt><dd>{job.completions}</dd></div>
            <div><dt>Parallelism</dt><dd>{job.parallelism}</dd></div>
            <div><dt>Active</dt><dd>{job.active}</dd></div>
            <div><dt>Succeeded</dt><dd>{job.succeeded}</dd></div>
            <div><dt>Failed</dt><dd>{job.failed}</dd></div>
            <div><dt>Age</dt><dd>{job.age}</dd></div>
          </dl>
          {(job.reason || job.message) && (
            <>
              <h3>Latest condition</h3>
              <p className={job.state === 'failed' ? 'errorLine' : 'hintLine'}>
                {[job.reason, job.message].filter(Boolean).join(': ')}
              </p>
            </>
          )}
        </section>
      )}
    />
  )
}
