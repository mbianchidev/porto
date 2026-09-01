import { Component, useEffect, useRef } from 'react'
import type { ErrorInfo, ReactNode } from 'react'
import { ActionButton } from './ActionButton'

type InspectorProps = {
  title: string
  subtitle?: string
  onClose: () => void
  children: ReactNode
}

export class InspectorErrorBoundary extends Component<
  { children: ReactNode },
  { failed: boolean }
> {
  state = { failed: false }

  static getDerivedStateFromError() {
    return { failed: true }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('Pod inspector view failed', error, info)
  }

  render() {
    if (this.state.failed) {
      return (
        <section className="drawerPanel" role="alert">
          <h3>Inspector view unavailable</h3>
          <p className="errorLine">This view could not be rendered. Switch tabs or refresh the pod inventory.</p>
        </section>
      )
    }
    return this.props.children
  }
}

/**
 * Right-hand inspector pane. On desktop it sits fixed beside the central inventory;
 * on narrow widths it becomes an accessible full-screen overlay (Escape closes it,
 * focus moves to the panel heading on open, and focus returns to the triggering
 * control is left to the caller via onClose).
 */
export function Inspector({ title, subtitle, onClose, children }: InspectorProps) {
  const headingRef = useRef<HTMLHeadingElement>(null)
  const onCloseRef = useRef(onClose)

  useEffect(() => {
    onCloseRef.current = onClose
  }, [onClose])

  useEffect(() => {
    headingRef.current?.focus()
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.defaultPrevented) return
      if (event.key === 'Escape') onCloseRef.current()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [])

  return (
    <aside className="inspector" aria-label={`${title} inspector`}>
      <div className="inspectorHeader">
        <div>
          <h2 tabIndex={-1} ref={headingRef}>{title}</h2>
          {subtitle && <p>{subtitle}</p>}
        </div>
        <ActionButton label="Close inspector" icon="close" className="inspectorClose" onClick={onClose} />
      </div>
      <div className="inspectorBody">{children}</div>
    </aside>
  )
}

export function InspectorTabs({
  tabs,
  activeID,
  onSelect,
}: {
  tabs: Array<{ id: string; label: string }>
  activeID: string
  onSelect: (id: string) => void
}) {
  return (
    <div className="inspectorTabs" role="tablist" aria-label="Inspector sections">
      {tabs.map((tab) => (
        <button
          type="button"
          role="tab"
          key={tab.id}
          aria-selected={activeID === tab.id}
          className={activeID === tab.id ? 'active' : ''}
          onClick={() => onSelect(tab.id)}
        >
          {tab.label}
        </button>
      ))}
    </div>
  )
}
