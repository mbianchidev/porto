import type { ReactNode } from 'react'

/** Dark reporting strip fused above a section's control bar — mirrors the fleet rail. */
export function SectionRail({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="fleetRail" aria-label={`${title} status`}>
      <span className="fleetRailTitle">{title}</span>
      {children}
    </section>
  )
}

/** Search + filter + refresh strip fused beneath a SectionRail. */
export function ControlBar({ children }: { children: ReactNode }) {
  return <div className="controlBar">{children}</div>
}

/** Clear, explicit message for a runtime that is unreachable or not installed. */
export function UnavailableNotice({ title, message }: { title: string; message: string }) {
  return (
    <article className="empty unavailableNotice" role="status">
      <h2>{title}</h2>
      <p>{message}</p>
    </article>
  )
}

/**
 * Distinguishes a deliberately-off optional runtime (Docker/Kubernetes/VMs
 * default OFF to preserve native-only behavior) from one that is enabled but
 * unreachable, so the message never implies a live capability Porto can't
 * back up. A disabled runtime links straight to Settings to turn it on.
 */
export function RuntimeGate({
  label,
  enabled,
  message,
}: {
  label: string
  enabled: boolean
  message?: string
}) {
  if (!enabled) {
    return (
      <article className="empty unavailableNotice" role="status">
        <h2>{label} is turned off</h2>
        <p>{message || `${label} is an optional runtime and stays off by default so Porto's native project controls are unaffected.`}</p>
        <a className="buttonLink" href="#/settings">Enable in Settings</a>
      </article>
    )
  }
  return <UnavailableNotice title={`${label} is unavailable`} message={message || `Porto could not reach ${label}.`} />
}
