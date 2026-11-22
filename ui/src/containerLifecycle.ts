import { apiGet, apiSend, errorMessage, isAbortError } from './api'
import type { DockerContainer, DockerContainerAction, DockerNetwork, DockerVolume } from './types'

const COMPOSE_PROJECT_LABEL = 'com.docker.compose.project'
const COMPOSE_DEPENDS_ON_LABEL = 'com.docker.compose.depends_on'

export function containerState(container: DockerContainer) {
  return container.state.toLocaleLowerCase()
}

export function isPaused(container: DockerContainer) {
  return containerState(container) === 'paused'
}

export function isStartable(container: DockerContainer) {
  return ['stopped', 'exited', 'created'].includes(containerState(container))
}

export function isActive(container: DockerContainer) {
  return ['running', 'paused', 'pausing', 'restarting'].includes(containerState(container))
}

export function containerDisplayName(container: DockerContainer) {
  return container.name.replace(/^\//, '')
}

export type ComposeStackAction = 'start' | 'stop' | 'restart'
export type ComposeStackStep = { container: DockerContainer; action: DockerContainerAction }
export type ComposeStepFailure = { container: DockerContainer; message: string }
export type ComposeStepsResult = {
  completed: DockerContainer[]
  failures: ComposeStepFailure[]
  skipped: DockerContainer[]
}

function serviceKey(container: DockerContainer) {
  return container.composeService || container.id
}

// Compose records depends_on as comma-separated `service:condition:restart` entries.
function serviceDependencies(container: DockerContainer) {
  return (container.labels?.[COMPOSE_DEPENDS_ON_LABEL] ?? '')
    .split(',')
    .map((entry) => entry.split(':')[0].trim())
    .filter(Boolean)
}

/**
 * Groups containers into waves where every depends_on service in the set precedes its
 * dependents. Dependencies outside the set are ignored; a cycle runs its remaining
 * services together, as no valid order exists.
 */
export function composeDependencyWaves(containers: DockerContainer[]): DockerContainer[][] {
  const services = new Map<string, DockerContainer[]>()
  for (const container of containers) {
    const key = serviceKey(container)
    services.set(key, [...(services.get(key) ?? []), container])
  }
  const requires = new Map([...services].map(([service, members]) => [
    service,
    new Set(members.flatMap(serviceDependencies).filter((dependency) => dependency !== service && services.has(dependency))),
  ]))
  const placed = new Set<string>()
  let pending = [...services.keys()].sort()
  const waves: DockerContainer[][] = []
  while (pending.length > 0) {
    const ready = pending.filter((service) => [...(requires.get(service) ?? [])].every((dependency) => placed.has(dependency)))
    const wave = ready.length > 0 ? ready : pending
    for (const service of wave) placed.add(service)
    pending = pending.filter((service) => !placed.has(service))
    waves.push(wave.flatMap((service) => services.get(service) ?? []))
  }
  return waves
}

function plan(targets: DockerContainer[], action: (container: DockerContainer) => DockerContainerAction, dependentsFirst: boolean) {
  const waves = composeDependencyWaves(targets)
  if (dependentsFirst) waves.reverse()
  return waves.map((wave) => wave.map((container) => ({ container, action: action(container) })))
}

/** Start/resume and restart follow depends_on order; stop runs dependents first, like Compose. */
export function composeStackPlan(containers: DockerContainer[], action: ComposeStackAction): ComposeStackStep[][] {
  switch (action) {
    case 'start':
      return plan(containers.filter((container) => isStartable(container) || isPaused(container)), (container) => isPaused(container) ? 'unpause' : 'start', false)
    case 'stop':
      return plan(containers.filter(isActive), () => 'stop', true)
    case 'restart':
      return plan(containers.filter(isActive), () => 'restart', false)
  }
}

/**
 * Runs each wave concurrently through the per-container endpoint so every daemon guard
 * still applies. With skipDependents, services that depend on a failed or skipped
 * service are skipped; waves guarantee dependencies settle first.
 */
export async function runComposeSteps(waves: ComposeStackStep[][], skipDependents: boolean): Promise<ComposeStepsResult> {
  const result: ComposeStepsResult = { completed: [], failures: [], skipped: [] }
  const blocked = new Set<string>()
  for (const wave of waves) {
    const runnable = wave.filter((step) => {
      if (!skipDependents || !serviceDependencies(step.container).some((dependency) => blocked.has(dependency))) return true
      result.skipped.push(step.container)
      blocked.add(serviceKey(step.container))
      return false
    })
    const outcomes = await Promise.allSettled(runnable.map((step) =>
      apiSend(`/api/docker/containers/${encodeURIComponent(step.container.id)}/${step.action}`, 'POST')))
    outcomes.forEach((outcome, position) => {
      const step = runnable[position]
      if (outcome.status === 'fulfilled') {
        result.completed.push(step.container)
        return
      }
      result.failures.push({ container: step.container, message: errorMessage(outcome.reason, `Unable to ${step.action}`) })
      blocked.add(serviceKey(step.container))
    })
  }
  return result
}

export type ComposeStackResources = {
  networks: string[]
  volumes: string[]
  networkError?: string
  volumeError?: string
}

function projectResources(items: Array<DockerNetwork | DockerVolume>, project: string) {
  return items.filter((item) => item.labels?.[COMPOSE_PROJECT_LABEL] === project).map((item) => item.name).sort()
}

/** Lists networks and volumes Compose created for the project; external resources carry no project label. */
export async function composeStackResources(project: string, signal?: AbortSignal): Promise<ComposeStackResources> {
  const [networks, volumes] = await Promise.allSettled([
    apiGet<DockerNetwork[]>('/api/docker/networks', signal),
    apiGet<DockerVolume[]>('/api/docker/volumes', signal),
  ])
  for (const outcome of [networks, volumes]) {
    if (outcome.status === 'rejected' && isAbortError(outcome.reason)) throw outcome.reason
  }
  return {
    networks: networks.status === 'fulfilled' ? projectResources(networks.value ?? [], project) : [],
    volumes: volumes.status === 'fulfilled' ? projectResources(volumes.value ?? [], project) : [],
    ...(networks.status === 'rejected' ? { networkError: errorMessage(networks.reason, 'Unable to list networks') } : {}),
    ...(volumes.status === 'rejected' ? { volumeError: errorMessage(volumes.reason, 'Unable to list volumes') } : {}),
  }
}

export type ComposeStackRemoval = {
  stop: ComposeStepsResult
  remove: ComposeStepsResult
  removedNetworks: string[]
  removedVolumes: string[]
  resourceFailures: string[]
}

async function removeEach(names: string[], path: (name: string) => string, removed: string[], failures: string[], kind: string) {
  const outcomes = await Promise.allSettled(names.map((name) => apiSend(path(name), 'DELETE')))
  outcomes.forEach((outcome, index) => {
    if (outcome.status === 'fulfilled') removed.push(names[index])
    else failures.push(`${kind} ${names[index]}: ${errorMessage(outcome.reason, `Unable to remove ${kind}`)}`)
  })
}

/**
 * Mirrors `docker compose down`: stops active containers, removes every container, then the
 * previewed project networks. With deleteVolumes it also removes anonymous container volumes
 * and the previewed named volumes. Networks and volumes are kept while any container remains.
 */
export async function removeComposeStack(
  containers: DockerContainer[],
  resources: Pick<ComposeStackResources, 'networks' | 'volumes'>,
  deleteVolumes: boolean,
  onPhase?: (phase: string) => void,
): Promise<ComposeStackRemoval> {
  onPhase?.('Stopping')
  const stop = await runComposeSteps(plan(containers.filter(isActive), () => 'stop', true), false)
  const stopFailed = new Set(stop.failures.map((failure) => failure.container.id))
  onPhase?.('Removing containers')
  const remove = await runComposeSteps(
    plan(containers.filter((container) => !stopFailed.has(container.id)), () => deleteVolumes ? 'remove-volumes' : 'remove', true),
    false,
  )
  const removal: ComposeStackRemoval = { stop, remove, removedNetworks: [], removedVolumes: [], resourceFailures: [] }
  if (stop.failures.length > 0 || remove.failures.length > 0) return removal
  onPhase?.('Removing networks')
  await removeEach(resources.networks, (name) => `/api/docker/networks/${encodeURIComponent(name)}`, removal.removedNetworks, removal.resourceFailures, 'network')
  if (deleteVolumes) {
    onPhase?.('Deleting volumes')
    await removeEach(resources.volumes, (name) => `/api/docker/volumes/${encodeURIComponent(name)}`, removal.removedVolumes, removal.resourceFailures, 'volume')
  }
  return removal
}

export function describeStepFailures(failures: ComposeStepFailure[]) {
  return failures.map((failure) => `${containerDisplayName(failure.container)}: ${failure.message}`).join('; ')
}
