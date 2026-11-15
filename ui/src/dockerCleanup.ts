import { bytesLabel } from './dataTypes'
import type { DockerCleanupRun, DockerCleanupStep } from './types'

export const DOCKER_CLEANUP_WARNING = 'Cleanup permanently removes standalone containers stopped for more than 24 hours, unused BuildKit cache, unused images (including tagged ones) pulled or built more than 24 hours ago, and unused anonymous volumes, then trims the engine disk so its host file shrinks. Compose service containers, Kubernetes nodes, Resource Saver workloads, named volumes, and images used by any container are kept. Later builds may require rebuilds or image downloads.'

export const CLEANUP_RUN_LABELS: Record<DockerCleanupRun['status'], string> = {
  running: 'Running',
  succeeded: 'Succeeded',
  failed: 'Failed',
  skipped: 'Skipped',
  interrupted: 'Interrupted',
}

export const CLEANUP_STEP_LABELS: Record<DockerCleanupStep['status'], string> = {
  not_run: 'Not run',
  succeeded: 'Succeeded',
  failed: 'Failed',
  skipped: 'Skipped',
}

type CleanupStepKey = keyof DockerCleanupRun['result']

export const CLEANUP_STEPS: { key: CleanupStepKey; label: string; item?: string }[] = [
  { key: 'containers', label: 'Stopped standalone containers', item: 'container' },
  { key: 'buildCache', label: 'BuildKit cache (builder / buildx, cleaned once)', item: 'cache item' },
  { key: 'images', label: 'Unused image references', item: 'image reference' },
  { key: 'volumes', label: 'Unused anonymous volumes', item: 'anonymous volume' },
  { key: 'hostDisk', label: 'Engine host disk' },
]

const NOT_RUN: DockerCleanupStep = { status: 'not_run', itemsRemoved: 0 }

export function cleanupStep(run: DockerCleanupRun, key: CleanupStepKey): DockerCleanupStep {
  return run.result[key] ?? NOT_RUN
}

export function cleanupRunTitle(run: DockerCleanupRun) {
  return `${run.trigger === 'scheduled' ? 'Scheduled' : 'Manual'} runtime cleanup · ${CLEANUP_RUN_LABELS[run.status]}`
}

export function cleanupStepSummary(step: DockerCleanupStep, item?: string) {
  const reclaimed = step.bytesReclaimed === undefined
    ? 'reclaimed bytes not reported'
    : `${bytesLabel(step.bytesReclaimed)} reclaimed`
  if (!item) return `${CLEANUP_STEP_LABELS[step.status]}: ${reclaimed}`
  const removed = `${step.itemsRemoved.toLocaleString()} ${item}${step.itemsRemoved === 1 ? '' : 's'} removed`
  return `${CLEANUP_STEP_LABELS[step.status]}: ${removed}; ${reclaimed}`
}

export function cleanupRunSummary(run: DockerCleanupRun) {
  return [
    `${cleanupRunTitle(run)}.`,
    ...CLEANUP_STEPS.map(({ key, label, item }) => {
      const step = cleanupStep(run, key)
      return `${label}: ${cleanupStepSummary(step, item)}.${step.error ? ` ${step.error}` : ''}`
    }),
    run.error,
  ].filter(Boolean).join(' ')
}
