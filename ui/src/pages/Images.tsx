import { useState } from 'react'
import type { FormEvent } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { ActionButton } from '../components/ActionButton'
import { Inspector } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { StatusLamp } from '../components/StatusLamp'
import { RuntimeGate } from '../components/SectionChrome'
import type { DockerImage, DockerStatus } from '../types'

const COLUMNS_TEMPLATE = 'minmax(200px,1.4fr) minmax(90px,0.5fr) minmax(200px,1.3fr) minmax(90px,0.5fr)'

export function Images() {
  const { notifyError, notifyNotice } = useMessages()
  const [query, setQuery] = useState('')
  const [selectedID, setSelectedID] = useState<string | null>(null)
  const [pullReference, setPullReference] = useState('')
  const [pulling, setPulling] = useState(false)

  const status = usePolledResource<DockerStatus>((signal) => apiGet('/api/docker/status', signal), 10000, [], 'docker:status')
  const images = usePolledResource<DockerImage[]>((signal) => apiGet('/api/docker/images', signal), 8000, [], 'docker:images')
  const items = images.data ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filtered = items.filter((image) => normalizedQuery === '' || [image.repository, image.tag, image.digest]
    .some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
  const imageKey = (image: DockerImage) => `${image.repository}:${image.tag}:${image.digest || image.id}`
  const selected = items.find((image) => imageKey(image) === selectedID) ?? null
  const available = status.data?.available ?? false

  async function pullImage(event: FormEvent) {
    event.preventDefault()
    const reference = pullReference.trim()
    if (!reference) return
    setPulling(true)
    try {
      await apiSend('/api/docker/images/pull', 'POST', { reference })
      notifyNotice('images', `Pulled ${reference}.`)
      setPullReference('')
      images.reload()
    } catch (err) {
      notifyError('images', errorMessage(err, `Unable to pull ${reference}`))
    } finally {
      setPulling(false)
    }
  }

  async function removeImage(image: DockerImage, force: boolean) {
    const reference = image.repository && image.repository !== '<none>' && image.tag && image.tag !== '<none>'
      ? `${image.repository}:${image.tag}`
      : image.id
    if (!window.confirm(`${force ? 'Force-remove' : 'Remove'} image ${reference}?`)) return
    try {
      await apiSend(`/api/docker/images/${encodeURIComponent(reference)}${force ? '?force=true' : ''}`, 'DELETE')
      notifyNotice('images', `Removed ${reference}.`)
      if (selectedID === imageKey(image)) setSelectedID(null)
      images.reload()
    } catch (err) {
      notifyError('images', errorMessage(err, `Unable to remove ${image.repository}:${image.tag}`))
    }
  }

  return (
    <>
      <section className="fleetRail" aria-label="Docker status">
        <span className="fleetRailTitle">Registry signal</span>
        <span className="fleetDatum"><StatusLamp state={available ? 'running' : 'crashed'} />{available ? 'Available' : 'Unavailable'}</span>
        <span className="fleetMessage">{items.length} image(s)</span>
      </section>
      <div className="controlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter images</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
          <input type="search" value={query} placeholder="Filter images by repository or tag" onChange={(event) => setQuery(event.target.value)} />
        </label>
        <form className="pullForm" onSubmit={pullImage}>
          <input
            type="text"
            value={pullReference}
            placeholder="registry/repository:tag"
            aria-label="Image reference to pull"
            disabled={!available || pulling}
            onChange={(event) => setPullReference(event.target.value)}
          />
          <button type="submit" disabled={!available || pulling || pullReference.trim() === ''}>{pulling ? 'Pulling…' : 'Pull image'}</button>
        </form>
        <button className="refreshControl" type="button" onClick={images.reload}>Refresh</button>
      </div>
      <div className="workArea">
        {!available ? (
          <RuntimeGate label="Docker" enabled={status.data?.enabled ?? false} message={status.data?.message || status.error} />
        ) : (
          <InventoryList
            items={filtered}
            getKey={imageKey}
            columnsTemplate={COLUMNS_TEMPLATE}
            selectedKey={selectedID}
            onSelect={(image) => setSelectedID(imageKey(image))}
            ariaLabel="Docker images"
            emptyMessage={images.error || 'No images found.'}
            columns={[
              { header: 'Repository', render: (image) => <strong>{image.repository}</strong> },
              { header: 'Tag', className: 'mono', render: (image) => image.tag },
              { header: 'Digest', className: 'mono', render: (image) => image.digest ? `${image.digest.slice(0, 24)}…` : '—' },
              { header: 'Size', className: 'mono', render: (image) => image.size },
            ]}
            renderActions={(image) => (
              <ActionButton className="removeButton" label="Remove image" icon="remove" onClick={() => removeImage(image, false)} />
            )}
          />
        )}
        {selected && (
          <Inspector title={`${selected.repository}:${selected.tag}`} subtitle={selected.id} onClose={() => setSelectedID(null)}>
            <section className="drawerPanel">
              <h3>Image detail</h3>
              <dl className="runtimeGrid">
                <div><dt>Digest</dt><dd>{selected.digest || '—'}</dd></div>
                <div><dt>Size</dt><dd>{selected.size}</dd></div>
                <div><dt>Created</dt><dd>{selected.createdAt || '—'}</dd></div>
              </dl>
            </section>
            <div className="maintenanceBar">
              <span>Maintenance controls</span>
              <div className="actions">
                <ActionButton className="removeButton" label="Remove image" icon="remove" onClick={() => removeImage(selected, false)} />
                <ActionButton className="removeButton" label="Force remove image" icon="kill" onClick={() => removeImage(selected, true)} />
              </div>
            </div>
          </Inspector>
        )}
      </div>
    </>
  )
}
