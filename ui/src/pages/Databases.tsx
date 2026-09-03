import { ControlBar, SectionRail } from '../components/SectionChrome'
import { StatusLamp } from '../components/StatusLamp'

export function Databases() {
  return (
    <>
      <SectionRail title="Database signal">
        <span className="fleetDatum"><StatusLamp state="stopped" />Coming soon</span>
        <span className="fleetDatum"><small>Sources</small><strong>Local + Kubernetes</strong></span>
        <span className="fleetMessage">Inventory planned</span>
      </SectionRail>
      <ControlBar>
        <span className="filterResultCount">Database discovery and operations are not enabled yet.</span>
      </ControlBar>
      <div className="workArea databasesWorkArea">
        <section className="channelBoard" aria-label="Planned database integration">
          <article className="empty databaseComingSoon" role="status">
            <h2>Database control is coming soon.</h2>
            <p>
              Porto will combine SQL Not So Lite project databases with operator-managed
              databases discovered in Kubernetes, starting with PostgreSQL clusters from CloudNativePG.
            </p>
            <dl className="runtimeGrid">
              <div>
                <dt>Local source</dt>
                <dd>SQL Not So Lite</dd>
              </div>
              <div>
                <dt>Kubernetes source</dt>
                <dd>CloudNativePG</dd>
              </div>
              <div>
                <dt>Planned visibility</dt>
                <dd>Health, roles, replicas, storage</dd>
              </div>
              <div>
                <dt>Planned operations</dt>
                <dd>Connection details and lifecycle controls</dd>
              </div>
            </dl>
          </article>
        </section>
      </div>
    </>
  )
}
