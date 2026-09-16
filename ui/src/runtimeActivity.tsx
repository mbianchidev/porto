import { useEffect, useRef } from 'react'
import { apiGet } from './api'
import { useContainerSnapshots } from './containerSnapshots'
import { cleanupRunSummary } from './dockerCleanup'
import { usePolledResource } from './hooks'
import { useMessages } from './useMessages'
import type {
  ActivityLevel,
  DockerCleanupState,
  DockerContainerLifecycleEvent,
  DockerContainerSnapshot,
} from './types'

const DOCKER_CURSOR_KEY = 'porto.activity.docker.v1'
const CLEANUP_CURSOR_KEY = 'porto.activity.docker-cleanup.v1'
const CLEANUP_CURSOR_LIMIT = 50
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

function loadCleanupCursor(): string[] {
  try {
    const raw = window.localStorage.getItem(CLEANUP_CURSOR_KEY)
    const value: unknown = raw ? JSON.parse(raw) : []
    return Array.isArray(value) ? value.filter((key): key is string => typeof key === 'string').slice(-CLEANUP_CURSOR_LIMIT) : []
  } catch (error) {
    console.error('Unable to load runtime cleanup activity cursor', error)
    return []
  }
}

function DockerCleanupActivity() {
  // Cleanup history stays readable with Docker disabled, including attempts that
  // completed while Settings was closed or before the runtime was turned off.
  const cleanup = usePolledResource<DockerCleanupState>(
    (signal) => apiGet('/api/docker/cleanup', signal),
    5000,
    [],
  )
  const { recordActivity } = useMessages()
  const cursor = useRef(loadCleanupCursor())
  const previousError = useRef('')

  useEffect(() => {
    if (!cleanup.data) return
    let changed = false
    for (const run of [...cleanup.data.runs].reverse()) {
      const key = `${run.id}:${run.startedAt}`
      if (run.status === 'running' || cursor.current.includes(key)) continue
      cursor.current = [...cursor.current, key].slice(-CLEANUP_CURSOR_LIMIT)
      changed = true
      const level = run.status === 'succeeded' ? 'notice' : run.status === 'skipped' ? 'info' : 'error'
      recordActivity(level, 'cleanup', cleanupRunSummary(run), run.completedAt ?? run.startedAt)
    }
    if (changed) {
      try {
        window.localStorage.setItem(CLEANUP_CURSOR_KEY, JSON.stringify(cursor.current))
      } catch (error) {
        console.error('Unable to save runtime cleanup activity cursor', error)
      }
    }
  }, [cleanup.data, recordActivity])

  useEffect(() => {
    if (!cleanup.error) {
      previousError.current = ''
      return
    }
    if (cleanup.error === previousError.current) return
    previousError.current = cleanup.error
    recordActivity('error', 'cleanup', `Unable to load runtime cleanup history: ${cleanup.error}`)
  }, [cleanup.error, recordActivity])

  return null
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

  return <DockerCleanupActivity />
}
