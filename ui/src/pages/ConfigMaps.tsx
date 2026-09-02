import { useState } from 'react'
import { apiGet, errorMessage } from '../api'
import { writeClipboard } from '../clipboard'
import { usePolledResource } from '../hooks'
import { useKubernetesStatus } from '../kubernetes'
import { useMessages } from '../useMessages'
import { ActionButton } from '../components/ActionButton'
import { Inspector } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { KubernetesContextSelect } from '../components/KubernetesContextSelect'
import { StatusLamp } from '../components/StatusLamp'
import { RuntimeGate } from '../components/SectionChrome'
import type { KubernetesConfigMap, KubernetesConfigMapDetail, KubernetesContext } from '../types'

const COLUMNS_TEMPLATE = 'minmax(160px,1.2fr) minmax(110px,0.7fr) minmax(80px,0.4fr) minmax(80px,0.4fr) minmax(90px,0.5fr) minmax(70px,0.4fr)'

export function ConfigMaps({
  context,
  contexts,
  onContextChange,
}: {
  context: string
  contexts: KubernetesContext[]
  onContextChange: (context: string) => void
}) {
  const { notifyError, notifyNotice } = useMessages()
  const [namespace, setNamespace] = useState('')
  const [query, setQuery] = useState('')
  const [selectedKey, setSelectedKey] = useState<string | null>(null)

  const status = useKubernetesStatus(context)
  const available = !status.loading && !status.error && (status.data?.available ?? false)
  const configMaps = usePolledResource<KubernetesConfigMap[]>(
    (signal) => available
      ? apiGet(`/api/kubernetes/configmaps?context=${encodeURIComponent(context)}&namespace=${encodeURIComponent(namespace)}`, signal)
      : Promise.resolve([]),
    6000,
    [context, namespace, available],
    available ? `kubernetes:${context}:configmaps:${namespace}` : undefined,
  )
  const items = configMaps.data ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filtered = items.filter((configMap) => normalizedQuery === '' || [
    configMap.name,
    configMap.namespace,
    ...configMap.keys,
    ...configMap.binaryKeys,
  ].some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
  const key = (configMap: KubernetesConfigMap) => `${configMap.namespace}/${configMap.name}`
  const selected = items.find((configMap) => key(configMap) === selectedKey) ?? null
  const detail = usePolledResource<KubernetesConfigMapDetail | null>(
    (signal) => available && selected
      ? apiGet(`/api/kubernetes/configmaps/${encodeURIComponent(selected.namespace)}/${encodeURIComponent(selected.name)}?context=${encodeURIComponent(context)}`, signal)
      : Promise.resolve(null),
    0,
    [context, selectedKey, selected?.resourceVersion, available],
  )
  const ready = available && !configMaps.loading && !configMaps.error
  const selectedDetail = !detail.loading
    && !detail.error
    && detail.data?.namespace === selected?.namespace
    && detail.data?.name === selected?.name
    && detail.data?.resourceVersion === selected?.resourceVersion
    ? detail.data
    : null

  async function copyConfig(value: string, message: string) {
    try {
      await writeClipboard(value)
      notifyNotice('kubernetes', message)
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, 'Unable to copy config data'))
    }
  }

  function copyConfigMap() {
    if (!selectedDetail) return
    const metadata: {
      name: string
      namespace: string
      labels?: Record<string, string>
      annotations?: Record<string, string>
    } = {
      name: selectedDetail.name,
      namespace: selectedDetail.namespace,
    }
    if (Object.keys(selectedDetail.labels ?? {}).length > 0) metadata.labels = selectedDetail.labels
    if (Object.keys(selectedDetail.annotations ?? {}).length > 0) metadata.annotations = selectedDetail.annotations
    const payload = JSON.stringify({
      apiVersion: 'v1',
      kind: 'ConfigMap',
      metadata,
      immutable: selectedDetail.immutable,
      data: selectedDetail.data,
      binaryData: selectedDetail.binaryData ?? {},
    }, null, 2)
    void copyConfig(payload, `Copied the ${selectedDetail.name} ConfigMap.`)
  }

  return (
    <>
      <section className="fleetRail" aria-label="Kubernetes status">
        <span className="fleetRailTitle">Config signal</span>
        <span className="fleetDatum"><StatusLamp state={available ? 'running' : 'crashed'} />{available ? 'Available' : 'Unavailable'}</span>
        <span className="fleetDatum"><small>Context</small><strong>{context || 'default'}</strong></span>
        <span className="fleetMessage">{items.length} config map(s)</span>
      </section>
      <div className="controlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter config maps</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
          <input type="search" value={query} placeholder="Filter configs by name or key" onChange={(event) => setQuery(event.target.value)} />
        </label>
        <label className="namespaceField">
          <span>Namespace</span>
          <input type="text" value={namespace} placeholder="all namespaces" onChange={(event) => setNamespace(event.target.value)} />
        </label>
        <KubernetesContextSelect contexts={contexts} value={context} onChange={onContextChange} />
        <span className="filterResultCount" aria-live="polite">{filtered.length} / {items.length} configs</span>
        <button className="refreshControl" type="button" onClick={() => {
          status.reload()
          configMaps.reload()
          detail.reload()
        }}>Refresh</button>
      </div>
      <div className="workArea">
        {!ready ? (
          <RuntimeGate
            label="Kubernetes ConfigMaps"
            enabled={status.data?.enabled ?? false}
            message={configMaps.error || status.data?.message || status.error}
          />
        ) : (
          <InventoryList
            items={filtered}
            getKey={key}
            columnsTemplate={COLUMNS_TEMPLATE}
            selectedKey={selectedKey}
            onSelect={(configMap) => setSelectedKey(key(configMap))}
            ariaLabel="Kubernetes config maps"
            emptyMessage={configMaps.error || 'No config maps found in this namespace.'}
            columns={[
              { header: 'Name', render: (configMap) => <strong>{configMap.name}</strong> },
              { header: 'Namespace', className: 'mono', render: (configMap) => configMap.namespace },
              { header: 'Text keys', className: 'mono', render: (configMap) => configMap.keys.length },
              { header: 'Binary keys', className: 'mono', render: (configMap) => configMap.binaryKeys.length },
              { header: 'Immutable', className: 'mono', render: (configMap) => configMap.immutable ? 'yes' : 'no' },
              { header: 'Age', className: 'mono', render: (configMap) => configMap.age },
            ]}
          />
        )}

        {ready && selected && (
          <Inspector title={selected.name} subtitle={`${selected.namespace} · ConfigMap`} onClose={() => setSelectedKey(null)}>
            <section className="drawerPanel">
              <div className="drawerPanelHeading">
                <h3>Config map detail</h3>
                <ActionButton label="Copy complete ConfigMap" icon="copy" disabled={!selectedDetail} onClick={copyConfigMap} />
              </div>
              <dl className="runtimeGrid">
                <div><dt>Text keys</dt><dd>{selected.keys.length}</dd></div>
                <div><dt>Binary keys</dt><dd>{selected.binaryKeys.length}</dd></div>
                <div><dt>Immutable</dt><dd>{selected.immutable ? 'yes' : 'no'}</dd></div>
                <div><dt>Age</dt><dd>{selected.age}</dd></div>
              </dl>
              <h3>Text data</h3>
              {detail.error && <p className="errorLine">{detail.error}</p>}
              {!selectedDetail && !detail.error && <p className="hintLine">Loading config data...</p>}
              {selectedDetail && selectedDetail.keys.length === 0 ? (
                <p className="hintLine">No text data is defined.</p>
              ) : selectedDetail ? (
                <div className="resourceDataList">
                  {selectedDetail.keys.map((dataKey) => (
                    <div className="resourceDataEntry" key={dataKey}>
                      <div className="resourceDataEntryHeader">
                        <strong className="mono">{dataKey}</strong>
                        <ActionButton
                          label={`Copy ${dataKey}`}
                          icon="copy"
                          onClick={() => void copyConfig(selectedDetail.data[dataKey], `Copied ${dataKey}.`)}
                        />
                      </div>
                      <pre className="logRaw">{selectedDetail.data[dataKey]}</pre>
                    </div>
                  ))}
                </div>
              ) : null}
              <h3>Binary data</h3>
              {selectedDetail && selectedDetail.binaryKeys.length === 0 ? (
                <p className="hintLine">No binary data is defined.</p>
              ) : selectedDetail ? (
                <div className="resourceDataList">
                  {selectedDetail.binaryKeys.map((dataKey) => (
                    <div className="resourceDataEntry" key={dataKey}>
                      <div className="resourceDataEntryHeader">
                        <strong className="mono">{dataKey}</strong>
                        <ActionButton
                          label={`Copy ${dataKey}`}
                          icon="copy"
                          onClick={() => void copyConfig(selectedDetail.binaryData?.[dataKey] ?? '', `Copied ${dataKey}.`)}
                        />
                      </div>
                      <pre className="logRaw">{selectedDetail.binaryData?.[dataKey] ?? ''}</pre>
                    </div>
                  ))}
                </div>
              ) : (
                <p className="hintLine">Loading binary data...</p>
              )}
            </section>
          </Inspector>
        )}
      </div>
    </>
  )
}
