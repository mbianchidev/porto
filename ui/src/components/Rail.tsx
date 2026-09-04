import { Icon, type IconName } from './Icon'
import { StatusLamp } from './StatusLamp'
import type { RouteID } from '../types'

type NavItem = { id: RouteID; label: string; icon: IconName; badge?: string }
type NavGroup = { title: string; items: NavItem[] }

const NAV_GROUPS: NavGroup[] = [
  { title: 'Local development', items: [{ id: 'localhost-ing', label: 'localhost-ing', icon: 'localhost' }] },
  {
    title: 'Containers',
    items: [
      { id: 'containers', label: 'Containers', icon: 'containers' },
      { id: 'images', label: 'Images', icon: 'images' },
      { id: 'builds', label: 'Builds', icon: 'builds' },
      { id: 'volumes', label: 'Volumes', icon: 'volumes' },
      { id: 'networks', label: 'Networks', icon: 'networks' },
    ],
  },
  {
    title: 'Kubernetes',
    items: [
      { id: 'kubernetes', label: 'Overview', icon: 'kubernetes' },
      { id: 'pods', label: 'Pods', icon: 'pods' },
      { id: 'services', label: 'Services', icon: 'services' },
      { id: 'storage', label: 'Storage', icon: 'volumes' },
      { id: 'gateways', label: 'Gateway API', icon: 'networks' },
      { id: 'configs', label: 'Configs', icon: 'configs' },
      { id: 'secrets', label: 'Secrets', icon: 'secrets' },
      { id: 'nodes', label: 'Nodes', icon: 'nodes' },
    ],
  },
  { title: 'Databases', items: [{ id: 'databases', label: 'Databases', icon: 'databases', badge: 'Soon' }] },
  { title: 'Virtual machines', items: [{ id: 'machines', label: 'Machines', icon: 'machines' }] },
  {
    title: 'System',
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
        {NAV_GROUPS.map((group) => (
          <div className="railGroup" key={group.title}>
            <span className="railGroupTitle">
              <span>{group.title}</span>
              {group.title === 'Kubernetes' && (
                <span className="railClusterSignal">
                  <StatusLamp state={kubernetesRunningCount > 0 ? 'running' : 'stopped'} />
                  {kubernetesRunningCount > 0 ? `${kubernetesRunningCount} running` : 'idle'}
                </span>
              )}
            </span>
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
        ))}
      </div>
    </nav>
  )
}
