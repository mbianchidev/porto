import { lazy, Suspense } from 'react'

const Contributions = lazy(async () => ({ default: (await import('./ExtensionContributions')).ExtensionContributions }))

export function ExtensionContributions(props: { resourceKind: string; resourceID: string }) {
  return <Suspense fallback={<p role="status">Loading extension contributions...</p>}><Contributions {...props} /></Suspense>
}
