// Thin fetch helpers shared by every page. Every read defensively treats network
// failures, non-2xx responses, and aborted requests distinctly so pages can render
// clear unavailable/error states instead of inventing data.

import { dockerModeURL, isDockerModePath } from './dockerMode'

export class ApiError extends Error {
  status: number

  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

async function readErrorMessage(response: Response): Promise<string> {
  const text = await response.text()
  if (!text) return `Request failed with status ${response.status}`
  try {
    const parsed: unknown = JSON.parse(text)
    if (parsed && typeof parsed === 'object' && 'message' in parsed) {
      const message = (parsed as { message?: unknown }).message
      if (typeof message === 'string' && message !== '') return message
    }
  } catch {
    // Not JSON; fall through to the raw text body.
  }
  return text
}

async function fetchResponse(path: string, init?: RequestInit): Promise<Response> {
  path = dockerModeURL(path)
  const target = new URL(path, window.location.href)
  if (target.origin === window.location.origin && isDockerModePath(target.pathname)) {
    const headers = new Headers(init?.headers)
    if (target.searchParams.get('containerMode') === 'windows') headers.set('X-Porto-Container-Mode', 'windows')
    else headers.delete('X-Porto-Container-Mode')
    init = { ...init, headers }
  }
  let response: Response
  try {
    response = await fetch(path, init)
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') throw err
    throw new ApiError('Network request failed. Is the Porto daemon reachable?', 0)
  }
  if (!response.ok) {
    throw new ApiError(await readErrorMessage(response), response.status)
  }
  return response
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetchResponse(path, init)
  if (response.status === 204) return undefined as T
  const contentType = response.headers.get('content-type') ?? ''
  if (contentType.includes('application/json')) {
    return (await response.json()) as T
  }
  return (await response.text()) as unknown as T
}

export function apiGet<T>(path: string, signal?: AbortSignal): Promise<T> {
  return request<T>(path, { signal })
}

export async function apiDownload(path: string, signal?: AbortSignal): Promise<Blob> {
  return (await fetchResponse(path, { signal })).blob()
}

export function apiSend<T>(
  path: string,
  method: 'POST' | 'PUT' | 'DELETE',
  body?: unknown,
  signal?: AbortSignal,
): Promise<T> {
  return request<T>(path, {
    method,
    signal,
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
}

export function apiUpload<T>(path: string, body: Blob | string, signal?: AbortSignal): Promise<T> {
  return request<T>(path, {
    method: 'PUT', body, signal,
    headers: { 'Content-Type': 'application/octet-stream' },
  })
}

export function isAbortError(err: unknown): boolean {
  return err instanceof DOMException && err.name === 'AbortError'
}

export function errorMessage(err: unknown, fallback: string): string {
  if (err instanceof ApiError) return err.message
  if (err instanceof Error) return err.message
  return fallback
}
