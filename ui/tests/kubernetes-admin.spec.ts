import { expect, test, type Page } from '@playwright/test'
import type { AdminObject, AdminPreview, AdminResource, HelmPreview } from '../src/kubeAdminTypes'
import type { LocalOperation } from '../src/deliveryTypes'

const context = 'porto-synthetic'
const secretValue = 'c3ludGhldGljLXByaXZhdGUtdmFsdWU='
const deployment: AdminResource = { group: 'apps', version: 'v1', resource: 'deployments', kind: 'Deployment', namespaced: true, verbs: ['get', 'list', 'create', 'patch', 'delete'] }
const widget: AdminResource = { group: 'fixtures.example.invalid', version: 'v1', resource: 'widgets', kind: 'Widget', namespaced: true, verbs: ['get', 'list', 'create', 'patch', 'delete'] }
const secret: AdminResource = { group: '', version: 'v1', resource: 'secrets', kind: 'Secret', namespaced: true, verbs: ['get', 'list', 'create', 'patch', 'delete'] }

function baseline(path: string): unknown {
  const values: Record<string, unknown> = {
    '/api/settings': { dockerEnabled: false, kubernetesEnabled: true, vmsEnabled: false, theme: 'dark' },
    '/api/kubernetes/contexts': [{ name: context, cluster: context, namespace: 'default' }],
    '/api/kubernetes/clusters': [{ name: 'synthetic', provider: 'kind', state: 'running', context }],
    '/api/kubernetes/status': { available: true, enabled: true, context },
    '/api/kubernetes/admin/resource-names': { names: ['default'] },
    '/api/docker/status': { enabled: false, available: false },
    '/api/docker/cleanup': { enabled: false, dockerEnabled: false, runs: [] },
    '/api/operations': [],
  }
  return values[path] ?? []
}

async function mockAdmin(page: Page, resource: AdminResource, writable = true) {
  const requests: Array<{ path: string; body: Record<string, unknown> }> = []
  const target = { context, namespace: 'default', group: resource.group, version: resource.version, resource: resource.resource, name: 'synthetic-resource', uid: 'synthetic-uid', resourceVersion: '17' }
  const object: AdminObject = {
    target, kind: resource.kind, secret: resource.kind === 'Secret', revealed: false,
    manifest: { apiVersion: resource.group ? `${resource.group}/${resource.version}` : resource.version, kind: resource.kind, metadata: { name: target.name, namespace: target.namespace, uid: target.uid, resourceVersion: target.resourceVersion }, ...(resource.kind === 'Secret' ? {} : { spec: { replicas: 1 } }) },
  }
  const preview: AdminPreview = { token: 'e'.repeat(32), context, namespace: 'default', differences: [{ target, kind: resource.kind, before: object.manifest, after: resource.kind === 'Secret' ? { kind: 'Secret', data: { token: '[redacted]' } } : { kind: resource.kind, spec: { count: 2 } }, managers: ['synthetic-controller'] }], warnings: ['Native resourceVersion and field ownership remain authoritative.'], expiresAt: '2099-01-01T00:00:00Z' }
  let operation: LocalOperation | null = null
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    let value: unknown = baseline(path)
    if (path === '/api/kubernetes/admin/discovery') value = { resources: [resource], partial: false }
    if (path === '/api/kubernetes/admin/resources') value = { resource, items: [object] }
    if (path === '/api/kubernetes/admin/resource-names') value = { names: url.searchParams.get('resource') === 'namespaces' ? ['default', 'synthetic-other'] : [target.name] }
    if (path === '/api/kubernetes/admin/resource') {
      const named = { ...object, target: { ...target, name: url.searchParams.get('name') || target.name } }
      value = url.searchParams.get('reveal') === 'true' ? { ...named, revealed: true, manifest: { ...object.manifest, data: { token: secretValue } } } : named
      if (url.searchParams.get('reveal') === 'true') requests.push({ path: `${path}?reveal=true`, body: {} })
    }
    if (path === '/api/kubernetes/admin/access') value = { target, verbs: Object.fromEntries(['get', 'list', 'create', 'patch', 'update', 'delete', 'scale'].map((verb) => [verb, { allowed: ['get', 'list'].includes(verb) || writable }])) }
    if (path === '/api/kubernetes/admin/schema') value = { state: 'available', version: '/synthetic/schema?hash=one', definition: { type: 'object', properties: { spec: { properties: { count: { type: 'integer', minimum: 0 } } } } } }
    if (path === '/api/operations') value = operation ? [operation] : []
    if (request.method() === 'POST' && path.startsWith('/api/kubernetes/admin/')) {
      const body: Record<string, unknown> = request.postDataJSON()
      requests.push({ path, body })
      if (path.endsWith('/previews')) value = preview
      else {
        operation = { id: 'f'.repeat(32), resource: `kubernetes:${context}`, action: path.endsWith('/actions') ? 'kubernetes.scale' : 'kubernetes.apply', status: 'partial', phase: 'complete', error: 'Synthetic rollout/admission failure. Inspect actual conditions; no force overwrite was used.', startedAt: '2026-01-01T00:00:00Z' }
        value = operation
      }
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) })
  })
  await page.goto('/#/kubernetes-admin')
  return { requests, target, preview }
}

test('read-only identities have disabled mutation controls and explicit effective capabilities', async ({ page }) => {
  const { requests } = await mockAdmin(page, deployment, false)
  await expect(page.getByRole('button', { name: 'New manifest', exact: true })).toBeDisabled()
  await page.getByText('synthetic-resource', { exact: true }).click()
  await expect(page.getByRole('button', { name: 'Edit manifest', exact: true })).toBeDisabled()
  await expect(page.getByRole('button', { name: 'Run resource action', exact: true })).toBeDisabled()
  await expect(page.getByText('Read-only or not authorized to mutate')).toBeVisible()
  expect(requests).toHaveLength(0)
})

test('unknown served resources use actual schemas and previewed context-bound apply', async ({ page }) => {
  const { requests, preview } = await mockAdmin(page, widget)
  await page.getByRole('button', { name: 'Show served schema', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Served API schema' })).toBeVisible()
  await expect(page.getByText('"integer"', { exact: false })).toBeVisible()
  await page.getByRole('button', { name: 'New manifest', exact: true }).click()
  await page.getByLabel('Kubernetes manifest editor').fill('apiVersion: fixtures.example.invalid/v1\nkind: Widget\nmetadata:\n  name: synthetic-widget\n  namespace: default\nspec:\n  count: 2\n')
  await page.getByRole('button', { name: 'Server dry run and diff', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Kubernetes manifest preview' })).toContainText('synthetic-controller')
  expect(requests[0].body).toMatchObject({ context, namespace: 'default', mode: 'create', secretConsent: false })
  page.once('dialog', (dialog) => dialog.dismiss())
  await page.getByRole('button', { name: 'Apply manifest preview', exact: true }).click()
  expect(requests).toHaveLength(1)
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Apply manifest preview', exact: true }).click()
  await expect.poll(() => requests.length).toBe(2)
  expect(requests[1].body).toEqual({ context, namespace: 'default', preview: preview.token, confirm: true })
  await expect(page.getByRole('table').filter({ hasText: 'kubernetes.apply' })).toContainText('partial')
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
})

test('Secret values require explicit reveal and are cleared after redacted preview', async ({ page }) => {
  const { requests } = await mockAdmin(page, secret)
  await page.getByText('synthetic-resource', { exact: true }).click()
  await expect(page.getByText(secretValue, { exact: false })).toHaveCount(0)
  page.once('dialog', (dialog) => dialog.dismiss())
  await page.getByRole('button', { name: 'Reveal Secret values', exact: true }).click()
  expect(requests).toHaveLength(0)
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Reveal Secret values', exact: true }).click()
  await expect(page.getByText(secretValue, { exact: false })).toBeVisible()
  await page.getByRole('button', { name: 'Edit revealed Secret', exact: true }).click()
  await page.getByLabel('Authorize Secret create/edit; values remain hidden in diffs and history').check()
  await page.getByRole('button', { name: 'Server dry run and diff', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Kubernetes manifest preview' })).toContainText('[redacted]')
  await expect(page.getByText(secretValue, { exact: false })).toHaveCount(0)
  await expect(page.getByLabel('Kubernetes manifest editor')).toHaveValue('')
  expect(requests.find((request) => request.path.endsWith('/previews'))?.body).toMatchObject({ context, namespace: 'default', secretConsent: true })
})

test('workload operations carry exact UID and resourceVersion instead of retargeting names', async ({ page }) => {
  const { requests, target } = await mockAdmin(page, deployment)
  await page.getByText('synthetic-resource', { exact: true }).click()
  await page.getByLabel('Replicas / Job parallelism').fill('2')
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Run resource action', exact: true }).click()
  await expect.poll(() => requests.length).toBe(1)
  expect(requests[0]).toEqual({ path: '/api/kubernetes/admin/actions', body: { ...target, action: 'scale', replicas: 2, confirm: true, acknowledgeStorage: false } })
})

test('resource dropdown follows every name page and permits an unlisted manual target', async ({ page }) => {
  const { requests, target } = await mockAdmin(page, deployment)
  await page.route('**/api/kubernetes/admin/resource-names?*', async (route) => {
    const query = new URL(route.request().url()).searchParams
    const value = query.get('resource') === 'namespaces' ? { names: ['default'] }
      : query.get('continue') === 'synthetic-next' ? { names: ['synthetic-last'] }
        : { names: ['synthetic-resource', 'synthetic-second'], continue: 'synthetic-next' }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) })
  })
  await page.getByRole('button', { name: 'Refresh discovery', exact: true }).click()
  const input = page.getByLabel('Resource name (optional)', { exact: true })
  await expect.poll(() => input.evaluate((element) => {
    if (!(element instanceof HTMLInputElement)) throw new Error('Expected resource-name input')
    return Array.from(element.list?.options ?? [], (option) => option.value)
  })).toEqual(['synthetic-last', 'synthetic-resource', 'synthetic-second'])
  await input.fill('synthetic-not-listed')
  await page.getByRole('button', { name: 'Inspect named resource', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'synthetic-not-listed', exact: true })).toBeVisible()
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Run resource action', exact: true }).click()
  await expect.poll(() => requests.length).toBe(1)
  expect(requests[0].body).toMatchObject({ ...target, name: 'synthetic-not-listed', action: 'scale' })
})

test('suggestion-list failures do not block inspection or authorized named actions', async ({ page }) => {
  const { requests } = await mockAdmin(page, deployment)
  await page.route('**/api/kubernetes/admin/resource-names?*', (route) => route.fulfill({ status: 403, body: 'Synthetic list permission denied; named get is still authorized.' }))
  await page.getByRole('button', { name: 'Refresh discovery', exact: true }).click()
  await expect(page.getByRole('alert').filter({ hasText: 'type a resource name manually' })).toBeVisible()
  await page.getByLabel('Resource name (optional)', { exact: true }).fill('synthetic-direct')
  await page.getByRole('button', { name: 'Inspect named resource', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'synthetic-direct', exact: true })).toBeVisible()
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Run resource action', exact: true }).click()
  await expect.poll(() => requests.length).toBe(1)
  expect(requests[0].body.name).toBe('synthetic-direct')
})

test('a failed manual lookup never leaves actions bound to the previously inspected resource', async ({ page }) => {
  await mockAdmin(page, deployment)
  await page.getByText('synthetic-resource', { exact: true }).click()
  await page.getByRole('button', { name: 'Close inspector', exact: true }).click()
  await page.route('**/api/kubernetes/admin/resource?*', (route) => route.fulfill({ status: 404, body: 'Synthetic named resource does not exist.' }))
  await page.getByLabel('Resource name (optional)', { exact: true }).fill('synthetic-missing')
  await page.getByRole('button', { name: 'Inspect named resource', exact: true }).click()
  await expect(page.getByRole('alert').filter({ hasText: 'Synthetic named resource does not exist.' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Run resource action', exact: true })).toHaveCount(0)
  await expect(page.getByRole('complementary', { name: 'synthetic-missing inspector' })).toBeVisible()
})

test('pod filters can inspect a typed namespace/name absent from the pod inventory', async ({ page }) => {
  const pod: AdminResource = { group: '', version: 'v1', resource: 'pods', kind: 'Pod', namespaced: true, verbs: ['get', 'list', 'delete'] }
  await mockAdmin(page, pod)
  await page.goto('/#/pods')
  await page.getByLabel('Filter pods', { exact: true }).fill('default/synthetic-unlisted-pod')
  const lookup = page.getByRole('link', { name: 'Inspect typed resource', exact: true })
  await lookup.focus()
  await page.keyboard.press('Enter')
  await expect(page.getByRole('heading', { name: 'synthetic-unlisted-pod', exact: true })).toBeVisible()
  await expect(page.getByLabel('Resource name (optional)', { exact: true })).toHaveValue('synthetic-unlisted-pod')
  await expect(page.getByRole('combobox', { name: 'Served API resource', exact: true })).toHaveValue('/v1/pods')
})

test('port forwarding suggests current pods and services while accepting unlisted names', async ({ page }) => {
  const requests: Record<string, unknown>[] = []
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    let value = baseline(url.pathname)
    if (url.pathname === '/api/kubernetes/admin/resource-names') {
      value = { names: url.searchParams.get('resource') === 'namespaces' ? ['default', 'synthetic-other']
        : url.searchParams.get('namespace') === 'synthetic-other' ? ['synthetic-other-pod']
          : url.searchParams.get('resource') === 'pods' ? ['synthetic-current-pod'] : ['synthetic-current-service'] }
    }
    if (url.pathname === '/api/kubernetes/port-forwards' && request.method() === 'POST') {
      const body: Record<string, unknown> = request.postDataJSON()
      requests.push(body)
      value = { ...body, id: 'synthetic-forward', context, address: '127.0.0.1', localPort: 41000, remotePort: 80, status: 'running', startedAt: '2026-01-01T00:00:00Z' }
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) })
  })
  await page.goto('/#/port-forwards')
  await page.getByRole('button', { name: 'New forward', exact: true }).click()
  const input = page.getByLabel('Resource name', { exact: true })
  const suggestions = () => input.evaluate((element) => {
    if (!(element instanceof HTMLInputElement)) throw new Error('Expected a port-forward target input')
    return Array.from(element.list?.options ?? [], (option) => option.value)
  })
  await expect.poll(suggestions).toEqual(['synthetic-current-service'])
  await page.getByRole('combobox', { name: 'Target type', exact: true }).selectOption('pod')
  await expect.poll(suggestions).toEqual(['synthetic-current-pod'])
  await page.getByLabel('Namespace', { exact: true }).fill('synthetic-other')
  await expect.poll(suggestions).toEqual(['synthetic-other-pod'])
  await input.fill('synthetic-unlisted-pod')
  await page.getByRole('button', { name: 'Start forward', exact: true }).click()
  await expect.poll(() => requests.length).toBe(1)
  expect(requests[0]).toMatchObject({ namespace: 'synthetic-other', resourceType: 'pod', resourceName: 'synthetic-unlisted-pod' })
})

test('Helm previews hide values and gate explicit storage consequences before activation', async ({ page }) => {
  const requests: Array<{ path: string; body: Record<string, unknown> }> = []
  let operation: LocalOperation | null = null
  const preview: HelmPreview = { token: '1'.repeat(32), context, namespace: 'default', release: 'synthetic-release', action: 'upgrade', revision: 1, chartId: '2'.repeat(64), before: [], after: [{ kind: 'Secret', data: { password: '[redacted]' } }], warnings: ['Failed hooks can leave partial resources.'], storageConsequences: ['PersistentVolumeClaim default/synthetic-data may affect persistent data.'], expiresAt: '2099-01-01T00:00:00Z' }
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    let value: unknown = baseline(path)
    if (path === '/api/kubernetes/helm/repositories') value = [{ name: 'synthetic', url: 'https://charts.example.invalid/', credentialStored: false }]
    if (path === '/api/kubernetes/helm/releases') value = [{ name: 'synthetic-release', namespace: 'default', revision: 1, status: 'deployed', chart: 'synthetic-1.0.0', appVersion: '1', updated: '2026-01-01T00:00:00Z' }]
    if (path === '/api/kubernetes/helm/release') value = { name: 'synthetic-release', namespace: 'default', revision: 1, status: operation ? 'failed' : 'deployed', chart: 'synthetic-1.0.0', appVersion: '1', updated: '2026-01-01T00:00:00Z', hooks: [] }
    if (path === '/api/kubernetes/admin/access') value = { target: {}, verbs: Object.fromEntries(['get', 'list', 'create', 'patch', 'update', 'delete'].map((verb) => [verb, { allowed: true }])) }
    if (path === '/api/operations') value = operation ? [operation] : []
    if (request.method() === 'POST') {
      requests.push({ path, body: request.postDataJSON() })
      if (path.endsWith('/previews')) value = preview
      else {
        operation = { id: '3'.repeat(32), resource: `kubernetes:${context}`, action: 'helm.upgrade', status: 'partial', phase: 'complete', error: 'Synthetic Helm hook failed; inspect native history.', startedAt: '2026-01-01T00:00:00Z' }
        value = operation
      }
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) })
  })
  await page.goto('/#/helm')
  await page.getByText('synthetic-release', { exact: true }).first().click()
  await page.getByRole('combobox', { name: 'Chart repository', exact: true }).selectOption('synthetic')
  await page.getByLabel('Chart name', { exact: true }).fill('synthetic')
  await page.getByLabel('Exact chart version', { exact: true }).fill('1.0.0')
  await page.getByLabel('Replacement Helm values (JSON, sensitive)').fill('{"password":"synthetic-private-value"}')
  await page.getByLabel('Authorize chart/release rendering and Secret changes; public diffs hide Kubernetes Secret values').check()
  await page.getByRole('button', { name: 'Render, dry run and preview Helm', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Helm lifecycle preview' })).not.toContainText('synthetic-private-value')
  await expect(page.getByLabel('Replacement Helm values (JSON, sensitive)')).toHaveValue('{}')
  const apply = page.getByRole('button', { name: 'Apply Helm preview', exact: true })
  await expect(apply).toBeDisabled()
  await page.getByLabel('I acknowledge rendered storage, namespace, CRD and dependent-data lifecycle consequences').check()
  page.once('dialog', (dialog) => dialog.accept())
  await apply.click()
  await expect.poll(() => requests.length).toBe(2)
  expect(requests[1].body).toEqual({ context, namespace: 'default', preview: preview.token, confirm: true, acknowledgeStorage: true })
  await expect(page.getByRole('table').filter({ hasText: 'helm.upgrade' })).toContainText('partial')
})
