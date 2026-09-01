import { useState } from 'react'
import type { FormEvent } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { Inspector } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { StatusLamp } from '../components/StatusLamp'
import { lampStateFor } from '../components/lampState'
import { RuntimeGate } from '../components/SectionChrome'
import type { DockerBuild, DockerBuildRequest, DockerStatus } from '../types'

const COLUMNS_TEMPLATE = '12px minmax(160px,1.2fr) minmax(110px,0.6fr) minmax(150px,0.9fr) minmax(90px,0.5fr) minmax(110px,0.6fr)'
const EMPTY_FORM: DockerBuildRequest = { context: '', dockerfile: '', tag: '', target: '', platform: '', noCache: false }

export function Builds() {
  const { notifyError, notifyNotice } = useMessages()
  const [selectedID, setSelectedID] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)
  const [form, setForm] = useState<DockerBuildRequest>(EMPTY_FORM)
  const [submitting, setSubmitting] = useState(false)

  const status = usePolledResource<DockerStatus>((signal) => apiGet('/api/docker/status', signal), 10000, [], 'docker:status')
  const builds = usePolledResource<DockerBuild[]>((signal) => apiGet('/api/docker/builds', signal), 6000, [], 'docker:builds')
  const items = builds.data ?? []
  const selected = items.find((build) => build.id === selectedID) ?? null
  const available = status.data?.available ?? false

  async function submitBuild(event: FormEvent) {
    event.preventDefault()
    if (form.context.trim() === '') {
      notifyError('builds', 'Build context path is required.')
      return
    }
    setSubmitting(true)
    try {
      const result = await apiSend<{ status: string; output: string }>('/api/docker/builds', 'POST', form)
      notifyNotice('builds', `Build ${result.status}${form.tag ? ` as ${form.tag}` : ''}.`)
      setForm(EMPTY_FORM)
      setCreating(false)
      builds.reload()
    } catch (err) {
      notifyError('builds', errorMessage(err, 'Build failed'))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <>
      <section className="fleetRail" aria-label="Docker status">
        <span className="fleetRailTitle">Build signal</span>
        <span className="fleetDatum"><StatusLamp state={available ? 'running' : 'crashed'} />{available ? 'Available' : 'Unavailable'}</span>
        <span className="fleetMessage">{items.length} build(s) in history</span>
      </section>
      <div className="controlBar">
        <span className="filterResultCount" aria-live="polite">{items.length} build(s)</span>
        <button className="refreshControl" type="button" onClick={builds.reload}>Refresh</button>
        <button type="button" disabled={!available} onClick={() => { setSelectedID(null); setCreating(true) }}>New build</button>
      </div>
      <div className="workArea">
        {!available ? (
          <RuntimeGate label="Docker" enabled={status.data?.enabled ?? false} message={status.data?.message || status.error} />
        ) : (
          <InventoryList
            items={items}
            getKey={(build) => build.id}
            columnsTemplate={COLUMNS_TEMPLATE}
            getLamp={(build) => lampStateFor(build.status)}
            getLampLabel={(build) => build.status}
            selectedKey={selectedID}
            onSelect={(build) => { setCreating(false); setSelectedID(build.id) }}
            ariaLabel="Docker build history"
            emptyMessage={builds.error || 'No build history yet. Start a new build to populate it.'}
            columns={[
              { header: 'Name', render: (build) => <strong>{build.name}</strong> },
              { header: 'Status', className: 'mono', render: (build) => build.status },
              { header: 'Created', className: 'mono', render: (build) => build.createdAt },
              { header: 'Duration', className: 'mono', render: (build) => build.duration || '—' },
              { header: 'Platform', className: 'mono', render: (build) => build.platform || '—' },
            ]}
          />
        )}

        {selected && !creating && (
          <Inspector title={selected.name} subtitle={selected.id} onClose={() => setSelectedID(null)}>
            <section className="drawerPanel">
              <h3>Build detail</h3>
              <dl className="runtimeGrid">
                <div><dt>Status</dt><dd>{selected.status}</dd></div>
                <div><dt>Created</dt><dd>{selected.createdAt}</dd></div>
                <div><dt>Duration</dt><dd>{selected.duration || '—'}</dd></div>
                <div><dt>Platform</dt><dd>{selected.platform || '—'}</dd></div>
              </dl>
            </section>
          </Inspector>
        )}

        {creating && (
          <Inspector title="New build" subtitle="docker build" onClose={() => setCreating(false)}>
            <form className="inspectorForm" onSubmit={submitBuild}>
              <label>
                <span>Build context path</span>
                <input type="text" value={form.context} placeholder="./services/api" onChange={(event) => setForm({ ...form, context: event.target.value })} required />
              </label>
              <label>
                <span>Dockerfile</span>
                <input type="text" value={form.dockerfile} placeholder="Dockerfile" onChange={(event) => setForm({ ...form, dockerfile: event.target.value })} />
              </label>
              <label>
                <span>Tag</span>
                <input type="text" value={form.tag} placeholder="repository:tag" onChange={(event) => setForm({ ...form, tag: event.target.value })} />
              </label>
              <label>
                <span>Target stage</span>
                <input type="text" value={form.target} placeholder="runtime" onChange={(event) => setForm({ ...form, target: event.target.value })} />
              </label>
              <label>
                <span>Platform</span>
                <input type="text" value={form.platform} placeholder="linux/amd64" onChange={(event) => setForm({ ...form, platform: event.target.value })} />
              </label>
              <label className="toggleRow">
                <span><strong>Disable build cache</strong></span>
                <input type="checkbox" checked={form.noCache} onChange={(event) => setForm({ ...form, noCache: event.target.checked })} />
              </label>
              <button type="submit" disabled={submitting}>{submitting ? 'Building…' : 'Start build'}</button>
            </form>
          </Inspector>
        )}
      </div>
    </>
  )
}
