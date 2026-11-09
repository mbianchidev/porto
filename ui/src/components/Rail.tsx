import { useState, type Ref } from 'react'
import { Icon, type IconName } from './Icon'
import { StatusLamp } from './StatusLamp'
import type { RouteID } from '../types'
import { environmentSupportsRoute } from '../managementTypes'

type NavItem = { id: RouteID; label: string; icon?: IconName; badge?: string }
type NavSection = { id: string; title: string; items: NavItem[] }
type NavGroup = { id: string; title: string; icon: IconName; items: NavItem[]; sections?: NavSection[] }

const NAV_GROUPS: NavGroup[] = [
  {
    id: 'development',
    title: 'Development',
    icon: 'localhost',
    items: [
      { id: 'localhost-ing', label: 'localhost-ing', icon: 'localhost' },
      { id: 'applications', label: 'Applications', icon: 'deployments' },
      { id: 'databases', label: 'Databases', icon: 'databases', badge: 'Soon' },
    ],
  },
  {
    id: 'containers',
    title: 'Containers',
    icon: 'containers',
    items: [
      { id: 'containers', label: 'Containers', icon: 'containers' },
      { id: 'networks', label: 'Networks', icon: 'networks' },
    ],
    sections: [
      {
        id: 'containers-images', title: 'Images & builds',
        items: [
          { id: 'images', label: 'Images' },
          { id: 'builds', label: 'Builds' },
        ],
      },
      {
        id: 'containers-data', title: 'Storage & data',
        items: [
          { id: 'volumes', label: 'Volumes' },
          { id: 'docker-storage', label: 'Storage tools' },
          { id: 'migration', label: 'Migrate runtime data' },
        ],
      },
    ],
  },
  {
    id: 'kubernetes',
    title: 'Kubernetes',
    icon: 'kubernetes',
    items: [{ id: 'kubernetes', label: 'Overview', icon: 'kubernetes' }],
    sections: [
      {
        id: 'kubernetes-workloads', title: 'Workloads',
        items: [
          { id: 'deployments', label: 'Deployments' },
          { id: 'pods', label: 'Pods' },
          { id: 'jobs', label: 'Jobs' },
          { id: 'cronjobs', label: 'Scheduled jobs' },
        ],
      },
      {
        id: 'kubernetes-networking', title: 'Networking',
        items: [
          { id: 'services', label: 'Services' },
          { id: 'port-forwards', label: 'Port forwarding' },
          { id: 'gateways', label: 'Gateway API' },
        ],
      },
      {
        id: 'kubernetes-storage', title: 'Storage & config',
        items: [
          { id: 'storage', label: 'Storage' },
          { id: 'configs', label: 'ConfigMaps' },
          { id: 'secrets', label: 'Secrets' },
        ],
      },
      {
        id: 'kubernetes-delivery', title: 'Delivery',
        items: [
          { id: 'helm', label: 'Helm releases' },
          { id: 'gitops', label: 'GitOps applications' },
        ],
      },
      {
        id: 'kubernetes-cluster', title: 'Cluster tools',
        items: [
          { id: 'nodes', label: 'Nodes' },
          { id: 'kubernetes-admin', label: 'Resources and RBAC' },
        ],
      },
    ],
  },
  {
    id: 'system',
    title: 'System',
    icon: 'settings',
    items: [
      { id: 'environments', label: 'Environments and fleets', icon: 'networks' },
      { id: 'machines', label: 'Virtual machines', icon: 'machines' },
      { id: 'extensions', label: 'Extensions', icon: 'configs' },
      { id: 'diagnostics', label: 'Diagnostics', icon: 'diagnostics' },
      { id: 'activity', label: 'Activity', icon: 'activity' },
      { id: 'settings', label: 'Settings', icon: 'settings' },
    ],
  },
]

export function Rail({
  route,
  open,
  narrow,
  kubernetesRunningCount,
  onNavigate,
  backend = 'local',
  ref,
}: {
  route: RouteID
  open: boolean
  narrow: boolean
  kubernetesRunningCount: number
  onNavigate: () => void
  backend?: string
  ref?: Ref<HTMLElement>
}) {
  const groups = NAV_GROUPS.map((group) => ({
    ...group,
    items: group.items.filter((item) => environmentSupportsRoute(backend, item.id)),
    sections: (group.sections ?? []).map((section) => ({
      ...section,
      items: section.items.filter((item) => environmentSupportsRoute(backend, item.id)),
    })).filter((section) => section.items.length > 0),
  })).filter((group) => group.items.length > 0 || group.sections.length > 0)
  const activeGroup = groups.find((group) => group.items.some((item) => item.id === route)
    || group.sections.some((section) => section.items.some((item) => item.id === route)))
  const activeGroupID = activeGroup?.id ?? groups[0]?.id ?? null
  const activeSectionID = activeGroup?.sections.find((section) => section.items.some((item) => item.id === route))?.id ?? null
  const activeBranch = [activeGroupID, activeSectionID].filter((id): id is string => id !== null)
  const [expanded, setExpanded] = useState(() => ({ route, backend, ids: new Set(activeBranch) }))

  if (expanded.route !== route || expanded.backend !== backend) {
    setExpanded({ route, backend, ids: new Set([...expanded.ids, ...activeBranch]) })
  }

  function toggleExpansion(id: string) {
    setExpanded((current) => {
      const ids = new Set(current.ids)
      if (ids.has(id)) ids.delete(id)
      else ids.add(id)
      return { ...current, ids }
    })
  }

  function renderLink(item: NavItem) {
    return (
      <a
        key={item.id}
        href={`#/${item.id}`}
        className={route === item.id ? 'active' : undefined}
        aria-current={route === item.id ? 'page' : undefined}
        onClick={onNavigate}
      >
        {item.icon && <Icon name={item.icon} />}
        <span>{item.label}</span>
        {item.badge && <small className="railItemBadge">{item.badge}</small>}
      </a>
    )
  }

  return (
    <nav ref={ref} id="primary-navigation" className={`rail ${open ? 'open' : ''}`} aria-label="Primary navigation" inert={narrow && !open}>
      <a className="railBrand" href="#/localhost-ing" aria-label="Porto" onClick={onNavigate}>
        <span className="brandMark" aria-hidden="true"><span /><span /><span /></span>
        <span className="railBrandText">
          <strong>Porto</strong>
          <small>Operate mode</small>
        </span>
      </a>
      <div className="railExpansionControls">
        <button
          className="railExpansionButton"
          type="button"
          aria-controls="rail-groups"
          onClick={() => setExpanded((current) => ({
            ...current,
            ids: new Set(groups.flatMap((group) => [group.id, ...group.sections.map((section) => section.id)])),
          }))}
        >Expand all</button>
        <button
          className="railExpansionButton"
          type="button"
          aria-controls="rail-groups"
          onClick={() => setExpanded((current) => ({ ...current, ids: new Set<string>() }))}
        >Collapse all</button>
      </div>
      <div className="railGroups" id="rail-groups">
        {groups.map((group) => {
          const groupExpanded = expanded.ids.has(group.id)
          const groupContentID = `rail-group-${group.id}`
          const groupTitleID = `rail-title-${group.id}`
          return (
            <div className="railGroup" key={group.id}>
              <button
                className={`railGroupTitle railGroupToggle ${activeGroup?.id === group.id ? 'containsActive' : ''}`}
                type="button"
                aria-expanded={groupExpanded}
                aria-controls={groupContentID}
                aria-labelledby={groupTitleID}
                aria-describedby={group.id === 'kubernetes' ? 'rail-cluster-status' : undefined}
                onClick={() => toggleExpansion(group.id)}
              >
                <Icon name={group.icon} />
                <span className="railGroupName" id={groupTitleID}>{group.title}</span>
                <span className="railGroupMeta">
                  {group.id === 'kubernetes' && (
                    <span className="railClusterSignal" id="rail-cluster-status">
                      <StatusLamp state={kubernetesRunningCount > 0 ? 'running' : 'stopped'} />
                      {kubernetesRunningCount > 0 ? `${kubernetesRunningCount} running` : 'idle'}
                    </span>
                  )}
                  <span className="railGroupChevron"><Icon name="chevronDown" /></span>
                </span>
              </button>
              <div className="railGroupItems" id={groupContentID} hidden={!groupExpanded}>
                {group.items.map(renderLink)}
                {group.sections.map((section) => {
                  const sectionExpanded = expanded.ids.has(section.id)
                  const sectionContentID = `rail-section-${section.id}`
                  return (
                    <div className="railSection" key={section.id}>
                      <button
                        className={`railSectionToggle ${activeSectionID === section.id ? 'containsActive' : ''}`}
                        type="button"
                        aria-expanded={sectionExpanded}
                        aria-controls={sectionContentID}
                        onClick={() => toggleExpansion(section.id)}
                      >
                        <span>{section.title}</span>
                        <span className="railGroupChevron"><Icon name="chevronDown" /></span>
                      </button>
                      <div className="railSectionItems" id={sectionContentID} hidden={!sectionExpanded}>
                        {section.items.map(renderLink)}
                      </div>
                    </div>
                  )
                })}
              </div>
            </div>
          )
        })}
      </div>
    </nav>
  )
}
