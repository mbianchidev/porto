import { useState } from 'react'
import { Icon, type IconName } from './Icon'
import { StatusLamp } from './StatusLamp'
import type { RouteID } from '../types'

type NavItem = { id: RouteID; label: string; icon: IconName; badge?: string }
type NavGroup = { id: string; title: string; items: NavItem[]; collapsible?: boolean }

const NAV_GROUPS: NavGroup[] = [
  { id: 'local-development', title: 'Local development', items: [{ id: 'localhost-ing', label: 'localhost-ing', icon: 'localhost' }] },
  {
    id: 'containers',
    title: 'Containers',
    collapsible: true,
    items: [
      { id: 'containers', label: 'Containers', icon: 'containers' },
      { id: 'images', label: 'Images', icon: 'images' },
      { id: 'builds', label: 'Builds', icon: 'builds' },
      { id: 'volumes', label: 'Volumes', icon: 'volumes' },
      { id: 'networks', label: 'Networks', icon: 'networks' },
    ],
  },
  {
    id: 'kubernetes',
    title: 'Kubernetes',
    collapsible: true,
    items: [
      { id: 'kubernetes', label: 'Overview', icon: 'kubernetes' },
      { id: 'deployments', label: 'Deployments', icon: 'deployments' },
      { id: 'pods', label: 'Pods', icon: 'pods' },
      { id: 'services', label: 'Services', icon: 'services' },
      { id: 'jobs', label: 'Jobs', icon: 'jobs' },
      { id: 'cronjobs', label: 'CronJobs', icon: 'cronjobs' },
      { id: 'port-forwards', label: 'Port forwarding', icon: 'portForward' },
      { id: 'storage', label: 'Storage', icon: 'volumes' },
      { id: 'gateways', label: 'Gateway API', icon: 'networks' },
      { id: 'configs', label: 'Configs', icon: 'configs' },
      { id: 'secrets', label: 'Secrets', icon: 'secrets' },
      { id: 'nodes', label: 'Nodes', icon: 'nodes' },
    ],
  },
  { id: 'databases', title: 'Databases', collapsible: true, items: [{ id: 'databases', label: 'Databases', icon: 'databases', badge: 'Soon' }] },
  { id: 'virtual-machines', title: 'Virtual machines', collapsible: true, items: [{ id: 'machines', label: 'Machines', icon: 'machines' }] },
  {
    id: 'system',
    title: 'System',
    collapsible: true,
    items: [
      { id: 'activity', label: 'Activity', icon: 'activity' },
      { id: 'settings', label: 'Settings', icon: 'settings' },
    ],
  },
]

export function Rail({
  route,
  open,
  kubernetesRunningCount,
  onNavigate,
}: {
  route: RouteID
  open: boolean
  kubernetesRunningCount: number
  onNavigate: () => void
}) {
  const [collapsedGroups, setCollapsedGroups] = useState<Record<string, boolean>>({})

  return (
    <nav className={`rail ${open ? 'open' : ''}`} aria-label="Primary navigation">
      <a className="railBrand" href="#/localhost-ing" aria-label="Porto">
        <span className="brandMark" aria-hidden="true"><span /><span /><span /></span>
        <span className="railBrandText">
          <strong>Porto</strong>
          <small>Operate mode</small>
        </span>
      </a>
      <div className="railGroups">
        {NAV_GROUPS.map((group) => {
          const collapsed = group.collapsible && collapsedGroups[group.id]
          const groupContentID = `rail-group-${group.id}`
          const titleContent = (
            <>
              <span>{group.title}</span>
              <span className="railGroupMeta">
                {group.id === 'kubernetes' && (
                  <span className="railClusterSignal">
                    <StatusLamp state={kubernetesRunningCount > 0 ? 'running' : 'stopped'} />
                    {kubernetesRunningCount > 0 ? `${kubernetesRunningCount} running` : 'idle'}
                  </span>
                )}
                {group.collapsible && <span className="railGroupChevron"><Icon name="chevronDown" /></span>}
              </span>
            </>
          )
          return (
            <div className="railGroup" key={group.id}>
              {group.collapsible ? (
                <button
                  className="railGroupTitle railGroupToggle"
                  type="button"
                  aria-expanded={!collapsed}
                  aria-controls={groupContentID}
                  onClick={() => setCollapsedGroups((current) => ({ ...current, [group.id]: !current[group.id] }))}
                >
                  {titleContent}
                </button>
              ) : (
                <span className="railGroupTitle">{titleContent}</span>
              )}
              <div className="railGroupItems" id={groupContentID} hidden={collapsed}>
                {group.items.map((item) => (
                  <a
                    key={item.id}
                    href={`#/${item.id}`}
                    className={route === item.id ? 'active' : ''}
                    aria-current={route === item.id ? 'page' : undefined}
                    onClick={onNavigate}
                  >
                    <Icon name={item.icon} />
                    <span>{item.label}</span>
                    {item.badge && <small className="railItemBadge">{item.badge}</small>}
                  </a>
                ))}
              </div>
            </div>
          )
        })}
      </div>
    </nav>
  )
}
