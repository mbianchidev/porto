import { expect, test, type Page } from '@playwright/test'
import type { EnvironmentProfile, InstalledExtension } from '../src/managementTypes'

const alpha = 'a'.repeat(32)
const beta = 'b'.repeat(32)
const imported = 'c'.repeat(32)
const containerID = 'd'.repeat(64)
const profile = (id: string, name: string, backend: EnvironmentProfile['backend']): EnvironmentProfile => ({
  id, name, backend, address: backend === 'local' ? 'loopback' : 'https://synthetic.invalid',
  identity: `synthetic-identity-${id}`, capabilities: ['inventory.read', 'workload.operate', 'streams.read', 'terminal.open'],
  labels: { group: 'synthetic-fleet' }, state: 'online', revision: 1, revoked: false, remoteRevoked: false,
  owned: backend === 'local', ...(backend === 'kubernetes' ? { context: 'imported-synthetic' } : {}),
})
const profiles = [profile('local', 'This machine', 'local'), profile(alpha, 'Synthetic alpha', 'porto'), profile(beta, 'Synthetic beta', 'porto'), profile(imported, 'Imported synthetic cluster', 'kubernetes')]

function baseline(path: string, environment = 'local'): unknown {
  const container = {
    id: containerID, name: 'same-name', image: `synthetic/${environment === alpha ? 'alpha' : environment === beta ? 'beta' : 'local'}:one`,
    state: 'running', status: 'running', ports: '', networks: '', mounts: '', createdAt: '2026-01-01T00:00:00Z',
    taskPresent: true, health: { status: 'disabled', failingStreak: 0 }, resources: {}, restartCount: 0, oomKilled: false,
  }
  const values: Record<string, unknown> = {
    '/api/environments': profiles,
    '/api/environments/listener': null,
    '/api/environments/identities': [],
    '/api/environments/fleet/runs': [],
    '/api/extensions': [], '/api/extensions/catalog': [], '/api/extensions/publishers': {},
    '/api/settings': { dockerEnabled: environment !== imported, kubernetesEnabled: environment === imported, vmsEnabled: false, theme: 'dark', protectedBranches: [] },
    '/api/kubernetes/contexts': environment === imported ? [{ name: 'imported-synthetic', cluster: 'synthetic', authInfo: 'observer' }] : [],
    '/api/kubernetes/clusters': [],
    '/api/kubernetes/status': { enabled: true, available: true, context: 'imported-synthetic' },
    '/api/kubernetes/admin/discovery': { resources: [], partial: false },
    '/api/kubernetes/admin/resource-names': { names: ['default'] },
    '/api/docker/status': { enabled: true, available: true, context: `synthetic-${environment}` },
    '/api/docker/cleanup': { enabled: false, dockerEnabled: true, runs: [] },
    '/api/docker/containers/snapshot': { instanceId: `runtime-${environment}`, revision: 1, available: true, stale: false, containers: [container], events: [], capabilities: Object.fromEntries(['directInventory', 'lifecycleEvents', 'directCreation', 'taskRecreation', 'execLifecycle', 'healthUpdates', 'networkUpdates', 'checkpointRestore'].map((key) => [key, { supported: false, reason: 'Synthetic capability unavailable.' }])) },
    '/api/docker/images': [],
    '/api/operations': [],
  }
  return Object.hasOwn(values, path) ? values[path] : []
}

async function mockTargets(page: Page, offline = '') {
  const requests: Array<{ environment: string; path: string; method: string; body: unknown }> = []
  let environments = profiles
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const match = url.pathname.match(/^\/api\/environments\/([a-f0-9]{32})(\/api\/.*)$/)
    const environment = match?.[1] ?? 'local'
    const path = match?.[2] ?? url.pathname
    requests.push({ environment, path, method: request.method(), body: request.postData() ? request.postDataJSON() : undefined })
    if (environment === offline && path.startsWith('/api/docker/')) {
      await route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ message: 'Synthetic target offline. No local fallback or queued retry.' }) })
      return
    }
    if (path === '/api/docker/containers/events') {
      await route.fulfill({ contentType: 'text/event-stream', body: `event: snapshot\ndata: ${JSON.stringify(baseline('/api/docker/containers/snapshot', environment))}\n\n` })
      return
    }
    let value = path === '/api/environments' ? environments : baseline(path, environment)
    if (path.endsWith('/metadata') && request.method() === 'PUT') {
      const body: { name: string; labels: Record<string, string> } = request.postDataJSON()
      environments = environments.map((profile) => path === `/api/environments/${profile.id}/metadata`
        ? { ...profile, name: body.name, labels: body.labels, revision: profile.revision + 1 }
        : profile)
      value = {}
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) })
  })
  return requests
}

test('environment management keeps filters, metadata and explicit grants usable', async ({ page }) => {
  const requests = await mockTargets(page)
  await page.goto('/#/containers', { waitUntil: 'networkidle' })
  const manage = page.getByRole('link', { name: 'Manage environments', exact: true })
  await manage.focus()
  await page.keyboard.press('Enter')
  await expect(page.getByRole('heading', { name: 'Environments and fleets', level: 1 })).toBeVisible()
  const filter = page.getByRole('textbox', { name: 'Filter names, groups or labels', exact: true })
  await filter.fill('Synthetic alpha')
  const table = page.getByRole('table', { name: 'Authorized environments', exact: true })
  await expect(table.getByRole('row')).toHaveCount(2)
  await page.getByRole('button', { name: 'Edit labels for Synthetic alpha', exact: true }).click()
  const metadata = page.getByRole('region', { name: 'Environment metadata', exact: true })
  await metadata.getByLabel('Display name', { exact: true }).fill('Synthetic alpha updated')
  await metadata.getByRole('textbox', { name: 'Fleet labels and groups (JSON)', exact: true }).fill('{"group":"synthetic-updated"}')
  await metadata.getByRole('button', { name: 'Save environment metadata', exact: true }).click()
  await expect(metadata).toHaveCount(0)
  await expect(table).toContainText('Synthetic alpha updated')
  expect(requests.find((request) => request.method === 'PUT')?.body).toEqual({
    name: 'Synthetic alpha updated', labels: { group: 'synthetic-updated' }, revision: 1,
  })
  await page.getByText('Register a remote host, compatible Docker endpoint or external Kubernetes context', { exact: true }).click()
  await page.getByLabel('Environment name', { exact: true }).fill('Synthetic imported environment')
  await page.getByRole('combobox', { name: 'Backend', exact: true }).selectOption('kubernetes')
  await page.getByLabel('Imported context name', { exact: true }).fill('synthetic-import')
  await page.getByRole('textbox', { name: 'Kubeconfig content', exact: true }).fill('apiVersion: v1\nclusters: []\n')
  await page.getByLabel('Allow workload operations', { exact: false }).check()
  await page.getByLabel('Allow sensitive resource inspection', { exact: false }).check()
  await page.getByLabel('Allow explicit pod terminals', { exact: false }).check()
  await page.getByRole('button', { name: 'Register environment', exact: true }).click()
  await expect.poll(() => requests.filter((request) => request.path === '/api/environments' && request.method === 'POST').length).toBe(1)
  expect(requests.find((request) => request.path === '/api/environments' && request.method === 'POST')?.body).toMatchObject({
    backend: 'kubernetes', name: 'Synthetic imported environment', context: 'synthetic-import',
    operate: true, capabilities: ['secrets.read', 'terminal.open'], kubeconfig: 'apiVersion: v1\nclusters: []\n',
  })
})

test('environment controls and expanded management forms fit both themes and narrow screens', async ({ page }, testInfo) => {
  await mockTargets(page)
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/#/environments', { waitUntil: 'networkidle' })
  await page.screenshot({ path: testInfo.outputPath('environments.png'), animations: 'disabled' })
  const toolbar = page.getByRole('region', { name: 'Selected environment', exact: true })
  const selector = toolbar.getByRole('combobox', { name: 'Environment', exact: true })
  const heading = page.getByRole('heading', { name: 'Environments and fleets', level: 1 })
  await expect(heading).toBeVisible()
  const toolbarBox = await toolbar.boundingBox()
  const headingBox = await heading.boundingBox()
  if (!toolbarBox || !headingBox) throw new Error('Expected visible environment toolbar and heading')
  expect(headingBox.y - toolbarBox.y - toolbarBox.height).toBeGreaterThanOrEqual(16)
  await page.getByText('Register a remote host, compatible Docker endpoint or external Kubernetes context', { exact: true }).click()
  await page.getByRole('combobox', { name: 'Backend', exact: true }).selectOption('docker')
  await page.getByText('Secure listener and outbound Edge enrollment', { exact: true }).click()
  for (const theme of ['dark', 'light']) {
    await page.evaluate((value) => { document.documentElement.dataset.theme = value }, theme)
    const fieldStyle = await selector.evaluate((element) => {
      const style = getComputedStyle(element)
      const reference = document.querySelector('input[type="url"]')
      if (!reference) throw new Error('Expected a visible environment endpoint field')
      return {
        background: style.backgroundColor,
        radius: style.borderRadius,
        font: style.fontFamily,
        reference: getComputedStyle(reference).backgroundColor,
      }
    })
    expect(fieldStyle.background).toBe(fieldStyle.reference)
    expect(fieldStyle.radius).toBe('6px')
    expect(fieldStyle.font).not.toContain('Times')
    for (const width of [1440, 1024, 390]) {
      await page.setViewportSize({ width, height: 1000 })
      await expect(selector).toBeVisible()
      await expect(toolbar.getByRole('link', { name: 'Manage environments', exact: true })).toBeVisible()
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width)
      const overflowingControls = await page.locator('.environmentPanelBody').evaluateAll((panels) => panels.flatMap((panel) => {
        const bounds = panel.getBoundingClientRect()
        return Array.from(panel.querySelectorAll('input, select, textarea, button')).filter((control) => {
          const box = control.getBoundingClientRect()
          return box.width > 0 && (box.left < bounds.left - 1 || box.right > bounds.right + 1)
        }).map((control) => control.outerHTML)
      }))
      expect(overflowingControls).toEqual([])
      await toolbar.scrollIntoViewIfNeeded()
      await page.screenshot({ path: testInfo.outputPath(`environments-${theme}-${width}.png`), animations: 'disabled' })
      await page.getByText('Register a remote host, compatible Docker endpoint or external Kubernetes context', { exact: true }).scrollIntoViewIfNeeded()
      await page.screenshot({ path: testInfo.outputPath(`registration-${theme}-${width}.png`), animations: 'disabled' })
    }
  }
})

test('same-named resources, actions, caches and terminal streams stay on their explicit environment', async ({ page }) => {
  const requests = await mockTargets(page)
  const sockets: Array<{ url: string; closed: boolean }> = []
  await page.routeWebSocket('**/api/**', (socket) => {
    const record = { url: socket.url(), closed: false }
    sockets.push(record)
    socket.onClose(() => { record.closed = true })
  })
  await page.goto('/#/containers')
  const selector = page.getByRole('combobox', { name: 'Environment', exact: true })
  await selector.selectOption(alpha)
  await expect(page.getByRole('listbox', { name: 'Standalone Docker containers' })).toContainText('synthetic/alpha:one')
  const workloadURLs = await page.evaluate(async () => {
    const modulePath = '/src/environment.ts'
    const { workloadURL } = await import(modulePath)
    return [
      workloadURL('https://same-name.porto.localhost/'),
      workloadURL('http://127.0.0.1:41000/'),
      workloadURL('/api/projects/same-name/start'),
      workloadURL('https://synthetic.example.invalid/app'),
    ]
  })
  expect(workloadURLs).toEqual([undefined, undefined, undefined, 'https://synthetic.example.invalid/app'])
  await page.getByRole('listbox', { name: 'Standalone Docker containers' }).getByRole('button', { name: 'Stop container', exact: true }).click()
  await page.getByRole('option', { name: /same-name/ }).click()
  await page.getByRole('tab', { name: 'Terminal', exact: true }).click()
  await expect.poll(() => sockets.filter((socket) => !socket.closed).length).toBe(1)
  expect(sockets.every((socket) => socket.url.includes(`/api/environments/${alpha}/api/docker/containers/${containerID}/terminal`))).toBe(true)
  await selector.selectOption(beta)
  await expect.poll(() => sockets.every((socket) => socket.closed)).toBe(true)
  await expect(page.getByRole('listbox', { name: 'Standalone Docker containers' })).toContainText('synthetic/beta:one')
  await expect(page.getByRole('listbox', { name: 'Standalone Docker containers' })).not.toContainText('synthetic/alpha:one')
  await page.getByRole('listbox', { name: 'Standalone Docker containers' }).getByRole('button', { name: 'Stop container', exact: true }).click()
  await expect.poll(() => requests.filter((request) => request.method === 'POST' && request.path.endsWith('/stop')).map((request) => request.environment)).toEqual([alpha, beta])
  await selector.selectOption(imported)
  await page.goto('/#/kubernetes-admin')
  await expect(page.getByRole('link', { name: 'Virtual machines', exact: true })).toHaveCount(0)
  await expect.poll(() => requests.some((request) => request.environment === imported && request.path === '/api/kubernetes/admin/discovery')).toBe(true)
  expect(requests.filter((request) => request.method !== 'GET' && request.environment === 'local')).toHaveLength(0)
})

test('offline or invalid persisted targets never reuse local inventory or silently select local actions', async ({ page }) => {
  const requests = await mockTargets(page, alpha)
  await page.goto('/#/containers')
  await expect(page.getByRole('listbox', { name: 'Standalone Docker containers' })).toContainText('synthetic/local:one')
  await page.getByRole('combobox', { name: 'Environment', exact: true }).selectOption(alpha)
  await expect(page.getByText('Synthetic target offline. No local fallback or queued retry.', { exact: false }).first()).toBeVisible()
  await expect(page.getByRole('option', { name: /same-name/ })).toHaveCount(0)
  expect(requests.filter((request) => request.method !== 'GET')).toHaveLength(0)
  await page.evaluate(() => localStorage.setItem('porto.environment.v1', 'invalid-target'))
  await page.reload()
  await expect(page.getByRole('heading', { name: 'Unsupported target capability' })).toBeVisible()
  await expect(page.getByRole('combobox', { name: 'Environment', exact: true })).toHaveValue('unavailable')
})

test('extension packages require digest, developer consent and backend-bound explicit grants before invocation', async ({ page }) => {
  const requests: Array<{ path: string; body: Record<string, unknown> }> = []
  const manifest = {
    apiVersion: 'porto.extensions/v1', id: 'dev.synthetic.tool', name: 'Synthetic tool', version: '1.0.0', permissions: ['runtime.read'],
    views: [{ id: 'view', title: 'Synthetic static view', location: 'tools' as const }], actions: [{ id: 'inspect', title: 'Inspect synthetic inventory', destructive: false }],
  }
  let items: InstalledExtension[] = []
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    let value: unknown = baseline(path)
    if (path === '/api/extensions') value = items
    if (path.endsWith('/ui')) {
      await route.fulfill({
        contentType: 'text/html',
        headers: { 'Content-Security-Policy': "default-src 'none'; script-src 'none'; style-src 'unsafe-inline'; sandbox; frame-ancestors 'self'" },
        body: '<!doctype html><p>Synthetic isolated view</p><script>document.body.textContent="UNSAFE SCRIPT RAN";fetch("/api/projects/synthetic/start",{method:"POST"})</script>',
      })
      return
    }
    if (request.method() !== 'GET') {
      const body: Record<string, unknown> = request.postData() ? request.postDataJSON() : {}
      requests.push({ path, body })
      if (path === '/api/extensions') {
        items = [{ id: manifest.id, name: manifest.name, version: manifest.version, sha256: 'e'.repeat(64), source: 'developer', enabled: false, approved: false, compatible: true, generation: 1, state: 'disabled', manifest, grants: { permissions: [], environments: [], files: [], commands: [], secrets: [], destinations: [] } }]
        value = items[0]
      } else if (path.endsWith('/configuration')) {
        items = items.map((item) => ({ ...item, enabled: Boolean(body.enabled), approved: Boolean(body.approve), state: body.enabled ? 'ready' : 'disabled', generation: item.generation + 1, grants: body.grants as InstalledExtension['grants'] }))
        value = { status: 'configured' }
      } else if (path.endsWith('/invocations')) value = { environmentId: 'local', action: 'inspect', data: { synthetic: 'selected environment only' } }
      else if (request.method() === 'DELETE') { items = []; value = undefined }
    }
    if (request.method() === 'DELETE') await route.fulfill({ status: 204 })
    else await route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) })
  })
  await page.goto('/#/extensions')
  await page.getByText('Install or update a verified package', { exact: true }).click()
  await page.getByLabel('Extension package file').setInputFiles({ name: 'synthetic.porto-extension', mimeType: 'application/json', buffer: Buffer.from(JSON.stringify({ content: { manifest, ui: '<p>Synthetic</p>', module: 'AGFzbQEAAAA=' }, sha256: 'e'.repeat(64) })) })
  await page.getByLabel('Independently verified SHA-256').fill('e'.repeat(64))
  await page.getByLabel('I explicitly accept an unsigned local/developer package.', { exact: false }).check()
  await page.getByRole('button', { name: 'Install or update package', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Extension grants' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Inspect synthetic inventory', exact: true })).toHaveCount(0)
  await page.getByRole('checkbox', { name: 'runtime.read', exact: true }).check()
  await page.getByRole('checkbox', { name: 'This machine / local', exact: true }).check()
  await expect(page.getByRole('button', { name: 'Approve grants and enable' })).toBeDisabled()
  await page.getByLabel('I reviewed these capabilities', { exact: false }).check()
  await page.getByRole('button', { name: 'Approve grants and enable' }).click()
  await expect(page.getByRole('button', { name: 'Inspect synthetic inventory', exact: true })).toBeVisible()
  const iframe = page.locator('iframe')
  await expect(iframe).toHaveAttribute('sandbox', '')
  await expect(page.frameLocator('iframe').getByText('Synthetic isolated view')).toBeVisible()
  const frame = page.frames().find((frame) => frame.url().includes('/api/extensions/dev.synthetic.tool/ui'))
  if (!frame) throw new Error('Expected the enabled extension sandbox frame')
  expect(await frame.evaluate(() => {
    try { return window.parent.document.title } catch { return 'opaque-origin' }
  })).toBe('opaque-origin')
  await expect(page.frameLocator('iframe').getByText('UNSAFE SCRIPT RAN')).toHaveCount(0)
  await page.getByRole('button', { name: 'Inspect synthetic inventory', exact: true }).click()
  await expect(page.getByLabel('Extension action result')).toContainText('selected environment only')
  expect(requests.find((request) => request.path.endsWith('/invocations'))?.body).toMatchObject({ environmentId: 'local', action: 'inspect', confirm: false })
  expect(requests.find((request) => request.path.endsWith('/configuration'))?.body).toMatchObject({ enabled: true, approve: true, grants: { permissions: ['runtime.read'], environments: ['local'] } })
  expect(requests.some((request) => request.path.startsWith('/api/projects/'))).toBe(false)
  await page.getByRole('button', { name: 'Disable Synthetic tool', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Inspect synthetic inventory', exact: true })).toHaveCount(0)
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Remove Synthetic tool', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Review Synthetic tool', exact: true })).toHaveCount(0)
  await page.setViewportSize({ width: 390, height: 844 })
  const layout = await page.evaluate(() => ({
    width: document.documentElement.scrollWidth, viewport: window.innerWidth,
    overflow: Array.from(document.querySelectorAll('main *')).filter((element) => element.getBoundingClientRect().right > window.innerWidth + 1).map((element) => ({ tag: element.tagName, class: element.className, width: element.getBoundingClientRect().width })).slice(-10),
  }))
  expect(layout.width, JSON.stringify(layout.overflow)).toBeLessThanOrEqual(layout.viewport)
})
