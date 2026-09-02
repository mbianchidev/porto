import { useEffect, useState } from 'react'
import './App.css'
import { apiGet } from './api'
import { usePolledResource, useNarrowViewport } from './hooks'
import { MessagesProvider } from './messages'
import { useMessages } from './useMessages'
import { RuntimeActivity } from './runtimeActivity'
import { ActionButton } from './components/ActionButton'
import { Rail } from './components/Rail'
import { LocalhostIng } from './pages/LocalhostIng'
import { Containers } from './pages/Containers'
import { Images } from './pages/Images'
import { Builds } from './pages/Builds'
import { Volumes } from './pages/Volumes'
import { Networks } from './pages/Networks'
import { KubernetesOverview } from './pages/KubernetesOverview'
import { Pods } from './pages/Pods'
import { KubernetesServices } from './pages/KubernetesServices'
import { ConfigMaps } from './pages/ConfigMaps'
import { Secrets } from './pages/Secrets'
import { Nodes } from './pages/Nodes'
import { Machines } from './pages/Machines'
import { Activity } from './pages/Activity'
import { SettingsPage } from './pages/SettingsPage'
import type { IntegrationStatus, KillSwitchStatus, KubernetesCluster, KubernetesContext, RouteID, Settings } from './types'

const KNOWN_ROUTES: RouteID[] = [
  'localhost-ing', 'containers', 'images', 'builds', 'volumes', 'networks',
  'kubernetes', 'pods', 'services', 'configs', 'secrets', 'nodes', 'machines', 'activity', 'settings',
]

function routeFromHash(): RouteID {
  const raw = window.location.hash.replace(/^#\/?/, '')
  return (KNOWN_ROUTES as string[]).includes(raw) ? (raw as RouteID) : 'localhost-ing'
}

function AppShell() {
  const { errorBanner, noticeBanner } = useMessages()
  const [route, setRoute] = useState<RouteID>(routeFromHash)
  const [railOpen, setRailOpen] = useState(false)
  const [kubeContext, setKubeContext] = useState('')
  const [settingsOverride, setSettingsOverride] = useState<Settings | null>(null)
  const narrow = useNarrowViewport(860)

  useEffect(() => {
    if (window.location.hash === '' || window.location.hash === '#/') {
      window.location.hash = '#/localhost-ing'
    }
    const onHashChange = () => setRoute(routeFromHash())
    window.addEventListener('hashchange', onHashChange)
    return () => window.removeEventListener('hashchange', onHashChange)
  }, [])

  const settingsResource = usePolledResource<Settings>((signal) => apiGet('/api/settings', signal), 0, [])
  const settings = settingsOverride ?? settingsResource.data
  const sqlNotSoLite = usePolledResource<IntegrationStatus>((signal) => apiGet('/api/integrations/sql-not-so-lite', signal), 5000, [])
  const sendbox = usePolledResource<IntegrationStatus>((signal) => apiGet('/api/integrations/sendbox', signal), 5000, [])
  const killSwitch = usePolledResource<KillSwitchStatus>((signal) => apiGet('/api/integrations/kill-switch', signal), 5000, [])
  const kubernetesContexts = usePolledResource<KubernetesContext[]>((signal) => apiGet('/api/kubernetes/contexts', signal), 10000, [])
  const kubernetesClusters = usePolledResource<KubernetesCluster[]>((signal) => apiGet('/api/kubernetes/clusters', signal), 10000, [])
  const kubeContexts = kubernetesContexts.data ?? []
  const clusterItems = kubernetesClusters.data ?? []
  const clusterByContext = new Map(clusterItems.map((cluster) => [cluster.context, cluster]))
  const clustersLoaded = kubernetesClusters.data !== null
  const preferredKubeContext = kubeContexts.find((item) => clusterByContext.get(item.name)?.state === 'running')?.name
    || (clustersLoaded ? kubeContexts.find((item) => !clusterByContext.has(item.name))?.name : '')
  const activeKubeContext = kubeContexts.some((item) => item.name === kubeContext)
    ? kubeContext
    : preferredKubeContext || (clustersLoaded ? kubeContexts[0]?.name : '') || kubeContext
  const kubernetesRunningCount = clusterItems.filter((cluster) => cluster.state === 'running').length

  function reloadIntegrations() {
    sqlNotSoLite.reload()
    sendbox.reload()
    killSwitch.reload()
  }

  return (
    <div className={`appShell ${railOpen ? 'railOpen' : ''}`}>
      {/*
        THESIS: Porto is one dense operations desk, not a dashboard of cards, spanning
        local processes, containers, clusters, and VMs behind a single control surface.
        OWN-WORLD: Painted graphite/putty metal, engraved mono labels, status lamps, and
        a fixed left rail feeding a ranked inventory and a right inspector — olive healthy,
        amber attention, red fault, never a fourth hue.
        STORY: Pick a rail section, scan the section's signal rail and ranked inventory,
        select a row, then act from its inspector without leaving the surface.
        FIRST VIEWPORT: A dark rail, a fused signal-rail/control-bar instrument, and a
        dense ranked inventory fill the screen with no oversized hero or card grid.
        FORM: Broadcast patchbay operations desk extended into a three-pane infrastructure
        control board, distinctly Porto.
      */}
      <ActionButton
        className="railToggle"
        label={railOpen ? 'Close navigation' : 'Open navigation'}
        icon={railOpen ? 'close' : 'menu'}
        aria-expanded={railOpen}
        onClick={() => setRailOpen((value) => !value)}
      />
      <Rail route={route} open={railOpen} kubernetesRunningCount={kubernetesRunningCount} onNavigate={() => setRailOpen(false)} />
      {narrow && railOpen && <button type="button" className="railScrim" aria-label="Close navigation" onClick={() => setRailOpen(false)} />}
      <main className="appMain">
        <RuntimeActivity dockerEnabled={settings?.dockerEnabled ?? false} />
        {errorBanner && <div className="errorBanner banner" role="alert">{errorBanner}</div>}
        {noticeBanner && <div className="notice banner" role="status">{noticeBanner}</div>}

        {route === 'localhost-ing' && <LocalhostIng settings={settings} sendboxStatus={sendbox.data} kubeContext={activeKubeContext} />}
        {route === 'containers' && <Containers />}
        {route === 'images' && <Images />}
        {route === 'builds' && <Builds />}
        {route === 'volumes' && <Volumes />}
        {route === 'networks' && <Networks />}
        {route === 'kubernetes' && <KubernetesOverview context={activeKubeContext} contexts={kubeContexts} onContextChange={setKubeContext} />}
        {route === 'pods' && <Pods key={`pods:${activeKubeContext}`} context={activeKubeContext} contexts={kubeContexts} onContextChange={setKubeContext} />}
        {route === 'services' && <KubernetesServices key={`services:${activeKubeContext}`} context={activeKubeContext} contexts={kubeContexts} onContextChange={setKubeContext} />}
        {route === 'configs' && <ConfigMaps key={`configs:${activeKubeContext}`} context={activeKubeContext} contexts={kubeContexts} onContextChange={setKubeContext} />}
        {route === 'secrets' && <Secrets key={`secrets:${activeKubeContext}`} context={activeKubeContext} contexts={kubeContexts} onContextChange={setKubeContext} />}
        {route === 'nodes' && <Nodes key={`nodes:${activeKubeContext}`} context={activeKubeContext} contexts={kubeContexts} onContextChange={setKubeContext} />}
        {route === 'machines' && <Machines />}
        {route === 'activity' && <Activity />}
        {route === 'settings' && (
          <SettingsPage
            settings={settings}
            onSettingsSaved={setSettingsOverride}
            sqlNotSoLiteStatus={sqlNotSoLite.data}
            sendboxStatus={sendbox.data}
            killSwitchStatus={killSwitch.data}
            onIntegrationsChanged={reloadIntegrations}
          />
        )}
      </main>
    </div>
  )
}

function App() {
  return (
    <MessagesProvider>
      <AppShell />
    </MessagesProvider>
  )
}

export default App
