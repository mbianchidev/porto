import type { DockerCleanupRun, DockerCleanupStep } from './types'

export const DOCKER_CLEANUP_WARNING = 'Cleanup permanently removes all unused image references, including unused tagged images, and unused BuildKit cache. Images used by running or stopped containers are preserved. Containers and volumes are never pruned. Later builds may require rebuilds or image downloads.'

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
}

export function cleanupRunTitle(run: DockerCleanupRun) {
  return `${run.trigger === 'scheduled' ? 'Scheduled' : 'Manual'} runtime cleanup · ${CLEANUP_RUN_LABELS[run.status]}`
}

export function cleanupStepSummary(step: DockerCleanupStep, images = false) {
  const item = images ? 'image reference' : 'cache item'
  const removed = `${step.itemsRemoved.toLocaleString()} ${item}${step.itemsRemoved === 1 ? '' : 's'} removed`
  const reclaimed = step.bytesReclaimed === undefined
    ? 'reclaimed bytes not reported'
    : `${step.bytesReclaimed.toLocaleString()} bytes reclaimed`
  return `${CLEANUP_STEP_LABELS[step.status]}: ${removed}; ${reclaimed}`
}

export function cleanupRunSummary(run: DockerCleanupRun) {
  const cache = run.result.buildCache
  const images = run.result.images
  return [
    `${cleanupRunTitle(run)}.`,
    `BuildKit cache: ${cleanupStepSummary(cache)}.${cache.error ? ` ${cache.error}` : ''}`,
    `Images: ${cleanupStepSummary(images, true)}.${images.error ? ` ${images.error}` : ''}`,
    run.error,
  ].filter(Boolean).join(' ')
}
