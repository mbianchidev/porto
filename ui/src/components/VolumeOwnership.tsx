import { bytesLabel, type StorageResource } from '../dataTypes'

export function VolumeOwnership({ volume }: { volume: StorageResource }) {
  return <section className="drawerPanel" aria-label="Volume allocation and ownership">
    <h3>Allocation and mount relationships</h3>
    <dl className="runtimeGrid">
      <div><dt>Logical data</dt><dd>{bytesLabel(volume.logicalBytes)}</dd></div>
      <div><dt>Allocated storage</dt><dd>{bytesLabel(volume.allocatedBytes)}</dd></div>
    </dl>
    {volume.reason && <p className="hintLine">{volume.reason}</p>}
    {!volume.owners.length ? <p>No container mounts reference this volume.</p> : <div className="dataTableScroll">
      <table className="dataTable" aria-label="Volume mount owners">
        <thead><tr><th>Container / application</th><th>State</th><th>Mounts</th></tr></thead>
        <tbody>{volume.owners.map((owner) => <tr key={owner.id}>
          <td><strong>{owner.name}</strong><br />{owner.composeProject ? `${owner.composeProject}/${owner.composeService || ''}` : 'Standalone'}{owner.managed && <p>Managed Kubernetes node</p>}</td>
          <td>{owner.state}</td>
          <td>{owner.mounts?.length ? owner.mounts.map((mount) => <p key={mount.path}><strong>{mount.path}</strong><br />{mount.readOnly ? 'Read-only' : 'Writable'}</p>) : 'Mount details unavailable'}</td>
        </tr>)}</tbody>
      </table>
    </div>}
  </section>
}
