import type { ReactNode } from 'react'
import { StatusLamp } from './StatusLamp'
import type { LampState } from '../types'

export type InventoryColumn<T> = {
  header: string
  render: (item: T) => ReactNode
  className?: string
}

type InventoryListProps<T> = {
  items: T[]
  getKey: (item: T) => string
  columns: InventoryColumn<T>[]
  columnsTemplate: string
  getLamp?: (item: T) => LampState
  getLampLabel?: (item: T) => string
  selectedKey?: string | null
  onSelect: (item: T) => void
  renderActions?: (item: T) => ReactNode
  emptyMessage: string
  ariaLabel: string
}

/**
 * Dense, single-column inventory list shared by every runtime section: containers,
 * images, builds, volumes, networks, pods, services, nodes, and machines. Rows are
 * ranked, not a card grid; selecting a row opens the right-hand inspector.
 */
export function InventoryList<T>({
  items,
  getKey,
  columns,
  columnsTemplate,
  getLamp,
  getLampLabel,
  selectedKey,
  onSelect,
  renderActions,
  emptyMessage,
  ariaLabel,
}: InventoryListProps<T>) {
  if (items.length === 0) {
    return (
      <article className="empty">
        <p>{emptyMessage}</p>
      </article>
    )
  }
  return (
    <div className="inventoryList" role="listbox" aria-label={ariaLabel}>
      <div className="inventoryHead" style={{ gridTemplateColumns: columnsTemplate }} aria-hidden="true">
        {getLamp && <span />}
        {columns.map((column) => (
          <span key={column.header}>{column.header}</span>
        ))}
        {renderActions && <span />}
      </div>
      {items.map((item) => {
        const key = getKey(item)
        const selected = selectedKey === key
        return (
          <div className={`inventoryRow ${selected ? 'selected' : ''}`} key={key}>
            <button
              type="button"
              role="option"
              aria-selected={selected}
              className="inventoryRowToggle"
              style={{ gridTemplateColumns: columnsTemplate }}
              onClick={() => onSelect(item)}
            >
              {getLamp && (
                <span className="inventoryLampCell">
                  <StatusLamp state={getLamp(item)} />
                  {getLampLabel && <span className="visuallyHidden">{getLampLabel(item)}</span>}
                </span>
              )}
              {columns.map((column) => (
                <span className={column.className ?? ''} key={column.header}>
                  {column.render(item)}
                </span>
              ))}
            </button>
            {renderActions && (
              <div className="inventoryRowActions" aria-label={`${key} actions`}>
                {renderActions(item)}
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}
