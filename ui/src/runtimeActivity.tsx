import { useEffect, useRef } from 'react'
import { useContainerSnapshots } from './containerSnapshots'
import { useMessages } from './useMessages'
import type {
  ActivityLevel,
  DockerContainerLifecycleEvent,
  DockerContainerSnapshot,
} from './types'

const DOCKER_CURSOR_KEY = 'porto.activity.docker.v1'
const INITIAL_EVENT_LIMIT = 50

type DockerActivityCursor = {
  instanceId: string
  sequence: number
}

function loadCursor(): DockerActivityCursor | null {
  try {
    const raw = window.localStorage.getItem(DOCKER_CURSOR_KEY)
    if (!raw) return null
    const value: unknown = JSON.parse(raw)
    if (!value || typeof value !== 'object') return null
    const cursor = value as Partial<DockerActivityCursor>
    if (typeof cursor.instanceId !== 'string' || typeof cursor.sequence !== 'number' || !Number.isSafeInteger(cursor.sequence)) return null
    return { instanceId: cursor.instanceId, sequence: cursor.sequence }
  } catch (error) {
    console.error('Unable to load Docker activity cursor', error)
    return null
  }
}

function saveCursor(cursor: DockerActivityCursor) {
  try {
    window.localStorage.setItem(DOCKER_CURSOR_KEY, JSON.stringify(cursor))
  } catch (error) {
    console.error('Unable to save Docker activity cursor', error)
  }
}

function eventLevel(event: DockerContainerLifecycleEvent): ActivityLevel {
  if (event.oom || (event.exitCode !== undefined && event.exitCode !== 0)) return 'error'
  if (event.reason?.startsWith('partial event payload')) return 'error'
  return 'info'
}

function eventTarget(snapshot: DockerContainerSnapshot, event: DockerContainerLifecycleEvent) {
  const container = snapshot.containers.find((item) => item.id === event.containerId)
  const name = container?.name.replace(/^\//, '')
  if (name) return name
  if (event.containerId) return event.containerId.slice(0, 12)
  return 'container runtime'
}

function eventMessage(snapshot: DockerContainerSnapshot, event: DockerContainerLifecycleEvent) {
  const target = eventTarget(snapshot, event)
  const exitCode = event.exitCode !== undefined ? ` with code ${event.exitCode}` : ''
  switch (event.type) {
    case 'container-create': return `${target} created.`
    case 'container-delete': return `${target} deleted.`
    case 'task-create': return `${target} task created.`
    case 'task-start': return `${target} started.`
    case 'task-exit':
    case 'task-delete': return `${target} exited${exitCode}.`
    case 'task-oom': return `${target} was OOM-killed.`
    case 'task-paused': return `${target} paused.`
    case 'task-resumed': return `${target} resumed.`
    case 'state-transition': return `${target} changed state${event.reason ? ` (${event.reason})` : ''}.`
    case 'health-transition': return `${target} health changed${event.reason ? ` (${event.reason})` : ''}.`
    case 'restart': return `${target} restarted${event.reason ? ` (${event.reason})` : ''}.`
    default: {
      const action = event.type.replaceAll('-', ' ')
      return `${target}: ${action}${event.reason ? ` (${event.reason})` : ''}.`
    }
  }
}

function newEvents(snapshot: DockerContainerSnapshot, cursor: DockerActivityCursor | null) {
  const ordered = [...(snapshot.events ?? [])].sort((left, right) => left.sequence - right.sequence)
  if (cursor?.instanceId === snapshot.instanceId) {
    return ordered.filter((event) => event.sequence > cursor.sequence)
  }
  return ordered.slice(-INITIAL_EVENT_LIMIT)
}

export function RuntimeActivity({ dockerEnabled }: { dockerEnabled: boolean }) {
  const containers = useContainerSnapshots(dockerEnabled)
  const { recordActivity } = useMessages()
  const cursor = useRef<DockerActivityCursor | null>(loadCursor())
  const previousError = useRef('')

  useEffect(() => {
    const snapshot = containers.snapshot
    if (!snapshot) return
    const events = newEvents(snapshot, cursor.current)
    const sequence = Math.max(cursor.current?.instanceId === snapshot.instanceId ? cursor.current.sequence : 0,
      ...events.map((event) => event.sequence))
    const nextCursor = { instanceId: snapshot.instanceId, sequence }
    cursor.current = nextCursor
    saveCursor(nextCursor)
    for (const event of events) {
      recordActivity(eventLevel(event), 'containers', eventMessage(snapshot, event), event.timestamp)
    }
  }, [containers.snapshot, recordActivity])

  useEffect(() => {
    if (!dockerEnabled || !containers.error) {
      previousError.current = ''
      return
    }
    if (containers.error === previousError.current) return
    previousError.current = containers.error
    recordActivity('error', 'containers', containers.error)
  }, [containers.error, dockerEnabled, recordActivity])

  return null
}
