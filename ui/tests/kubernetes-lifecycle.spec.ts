import { expect, test, type Page } from '@playwright/test'
import type { KubernetesAutoscalingConfig, KubernetesCluster, KubernetesClusterRequest, RuntimeProviderStatus } from '../src/types'

const fixture: KubernetesCluster = {
  name: 'fixture-cluster', provider: 'kind', state: 'running', version: 'v1.35.2',
  context: 'porto-fixture-cluster', kubeconfigPath: '/synthetic/kubeconfig.yaml',
  server: 'https://127.0.0.1:54321', nodes: ['porto-fixture-cluster-control-plane'],
}

async function mockClusters(page: Page, initial: KubernetesCluster = fixture, providers: RuntimeProviderStatus[] = [], initialTab: 'upgrade' | 'overview' = 'upgrade') {
  let cluster = { ...initial }
  let scaling = {
    nodes: { enabled: false, maxNodes: 3, machine: { cpus: 2, memoryMiB: 2048, diskGiB: 20 } },
    keda: false, cluster: initial.name, nodeSupported: initial.provider === 'k3s' || initial.provider === 'k0s',
    running: false, paused: false, managedNodes: 0, kedaReady: false,
    karpenterVersion: '1.14.2', kedaVersion: '2.21.0',
  }
  const mutations: Array<{ path: string; body: unknown }> = []
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (request.method() !== 'GET' && (path === '/api/kubernetes/clusters' || path.startsWith('/api/kubernetes/clusters/'))) {
      const body: unknown = request.postData() ? request.postDataJSON() : undefined
      mutations.push({ path, body })
      if (path === '/api/kubernetes/clusters') {
        const spec: KubernetesClusterRequest = request.postDataJSON()
        cluster = { ...cluster, name: spec.name, provider: spec.provider, context: `porto-${spec.name}` }
      } else if (path.endsWith('/start') || path.endsWith('/restart')) {
        cluster = { ...cluster, state: 'running' }
      } else if (path.endsWith('/stop')) {
        cluster = { ...cluster, state: 'stopped' }
      } else if (path.endsWith('/upgrade')) {
        cluster = { ...cluster, version: 'v1.36.1' }
      } else if (path.endsWith('/upgrade-recover')) {
        cluster = { ...fixture }
      } else if (path.endsWith('/autoscaling')) {
        const next: KubernetesAutoscalingConfig = request.postDataJSON()
        scaling = { ...scaling, ...next, running: next.nodes.enabled, kedaReady: next.keda }
      }
      await route.fulfill({ contentType: 'application/json', body: JSON.stringify(path === '/api/kubernetes/clusters' ? cluster : { message: 'Synthetic cluster operation completed.' }) })
      return
    }
    const values: Record<string, unknown> = {
      '/api/settings': { dockerEnabled: false, kubernetesEnabled: true, vmsEnabled: false },
      '/api/kubernetes/status': { available: true, enabled: true, context: fixture.context, serverVersion: 'v1.35.2' },
      '/api/kubernetes/contexts': [{ name: fixture.context, cluster: fixture.context, namespace: 'default' }],
      '/api/kubernetes/clusters': [cluster],
      '/api/runtime/providers': providers,
      '/api/kubernetes/clusters/fixture-cluster/autoscaling': scaling,
      '/api/docker/status': { available: false, enabled: false },
      '/api/docker/cleanup': { enabled: false, dockerEnabled: false, runs: [] },
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(values[path] ?? []) })
  })
  await page.goto('/#/kubernetes')
  await page.getByText('fixture-cluster', { exact: true }).first().click()
  if (initialTab === 'upgrade') await page.getByRole('tab', { name: 'Upgrade', exact: true }).click()
  return mutations
}

test('kind upgrades require confirmation and explain persistent data restoration', async ({ page }, testInfo) => {
  const mutations = await mockClusters(page)
  await expect(page.getByText('kind upgrades replace the cluster.', { exact: false })).toBeVisible()
  await expect(page.getByLabel('Target kind release')).toBeVisible()
  await page.getByLabel('Target kind release').fill('v1.36.1')
  page.once('dialog', (dialog) => dialog.dismiss())
  await page.getByRole('button', { name: 'Upgrade cluster', exact: true }).click()
  expect(mutations).toHaveLength(0)
  let confirmation = ''
  page.once('dialog', async (dialog) => {
    confirmation = dialog.message()
    await dialog.accept()
  })
  await page.getByRole('button', { name: 'Upgrade cluster', exact: true }).click()
  await expect.poll(() => mutations.length).toBe(1)
  expect(confirmation).toContain('persistent volumes and application manifests')
  expect(mutations[0]).toEqual({
    path: '/api/kubernetes/clusters/fixture-cluster/upgrade',
    body: { version: 'v1.36.1', confirm: true },
  })
  await page.screenshot({ path: testInfo.outputPath('kind-upgrade-desktop.png'), fullPage: true })
  await page.setViewportSize({ width: 390, height: 844 })
  await expect(page.locator('.rail')).not.toBeInViewport()
  await expect(page.getByLabel('Target kind release')).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('kind-upgrade-mobile.png') })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  expect(await page.locator('.inspector').evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true)
})

test('failed kind upgrades expose recovery instead of destructive retries', async ({ page }) => {
  const mutations = await mockClusters(page, {
    ...fixture, state: 'upgrade-failed',
    upgrade: { version: 'v1.36.1', startedAt: '2026-01-01T00:00:00Z', backupPath: '/synthetic/private-backup', error: 'Synthetic restore failure.' },
  })
  await expect(page.getByRole('button', { name: 'Recover original cluster' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Upgrade cluster', exact: true })).toHaveCount(0)
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Recover original cluster' }).click()
  await expect.poll(() => mutations.length).toBe(1)
  expect(mutations[0].path).toContain('/upgrade-recover')
  expect(mutations[0].body).toEqual({ confirm: true })
})

test('VM upgrades expose the distribution-specific release and safe drain semantics', async ({ page }) => {
  await mockClusters(page, { ...fixture, provider: 'k0s', version: 'v1.35.2+k0s.0' })
  await expect(page.getByLabel('Target k0s release')).toHaveAttribute('placeholder', 'v1.36.4+k0s.1')
  await expect(page.getByText('Upgrade the control plane first, then drain and upgrade workers one at a time.', { exact: false })).toBeVisible()
})

test('kind gates node autoscaling while KEDA can configure and remove a real workload scaler', async ({ page }) => {
  const mutations = await mockClusters(page)
  await page.getByRole('tab', { name: 'Autoscaling', exact: true }).click()
  await expect(page.getByLabel('Autoscale worker nodes with Karpenter')).toBeDisabled()
  await page.getByLabel('Enable KEDA event-driven workload scaling').check()
  await page.getByRole('button', { name: 'Save autoscaling' }).click()
  await expect.poll(() => mutations.length).toBe(1)
  expect(mutations[0].body).toMatchObject({ keda: true, nodes: { enabled: false } })
  await expect(page.getByLabel('Target workload')).toBeEnabled()
  await page.getByLabel('Scaler name').fill('fixture-scaler')
  await page.getByLabel('Target workload').fill('fixture-app')
  await page.getByLabel('KEDA triggers (JSON array)').fill(JSON.stringify([
    { type: 'cron', metadata: { timezone: 'Etc/UTC', start: '0 8 * * *', end: '0 18 * * *', desiredReplicas: '2' } },
  ]))
  await page.getByRole('button', { name: 'Apply workload scaling' }).click()
  await expect.poll(() => mutations.length).toBe(2)
  expect(mutations[1].body).toMatchObject({
    namespace: 'default', name: 'fixture-scaler', targetName: 'fixture-app',
    targetKind: 'Deployment', minReplicas: 0, maxReplicas: 5,
  })
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Remove named scaler' }).click()
  await expect.poll(() => mutations.length).toBe(3)
  expect(mutations[2].path).toBe('/api/kubernetes/clusters/fixture-cluster/autoscaling/workloads/default/fixture-scaler')
})

test('k3s exposes explicit automatic-worker resource ceilings', async ({ page }) => {
  const mutations = await mockClusters(page, { ...fixture, provider: 'k3s', version: 'v1.35.2+k3s1' })
  await page.getByRole('tab', { name: 'Autoscaling', exact: true }).click()
  await page.getByLabel('Autoscale worker nodes with Karpenter').check()
  await page.getByLabel('Maximum autoscaled workers').fill('2')
  await page.getByLabel('CPUs per autoscaled worker').fill('1')
  await page.getByLabel('Worker memory (MiB)').fill('1024')
  await page.getByLabel('Worker disk (GiB)').fill('10')
  await expect(page.getByText('Worker ceiling: 2 CPUs, 2048 MiB RAM, 20 GiB disk.', { exact: false })).toBeVisible()
  await page.getByRole('button', { name: 'Save autoscaling' }).click()
  await expect.poll(() => mutations.length).toBe(1)
  expect(mutations[0].body).toMatchObject({ nodes: { enabled: true, maxNodes: 2, machine: { cpus: 1, memoryMiB: 1024, diskGiB: 10 } } })
})

for (const appleSilicon of [false, true]) {
  test(`Apple provider is ${appleSilicon ? 'available' : 'hidden'} without replacing Lima choices`, async ({ page }) => {
    const providers: RuntimeProviderStatus[] = [{ name: 'lima', command: 'limactl', installed: true, version: '2.2.0' }]
    if (appleSilicon) providers.push({ name: 'apple', command: 'container', installed: true, version: '1.7.1' })
    const mutations = await mockClusters(page, fixture, providers)
    await page.getByRole('button', { name: 'New cluster', exact: true }).click()
    const provider = page.getByRole('combobox', { name: 'Provider', exact: true })
    await expect(provider).toHaveValue('k3s')
    await expect(provider.locator('option[value="k3s"]')).toHaveCount(1)
    await expect(provider.locator('option[value="k0s"]')).toHaveCount(1)
    await expect(provider.locator('option[value="apple"]')).toHaveCount(appleSilicon ? 1 : 0)
    if (!appleSilicon) return
    await provider.selectOption('apple')
    await page.getByLabel('Name', { exact: true }).fill('native-fixture')
    await expect(page.getByLabel('Control-plane disk (GiB)', { exact: true })).toHaveCount(0)
    await page.getByRole('button', { name: 'Create cluster', exact: true }).click()
    await expect.poll(() => mutations.length).toBe(1)
    expect(mutations[0]).toMatchObject({
      path: '/api/kubernetes/clusters',
      body: { name: 'native-fixture', provider: 'apple', controlPlane: { diskGiB: 0 }, nodeGroups: [{ machine: { diskGiB: 0 } }] },
    })
  })
}

for (const state of ['running', 'stopped']) {
  test(`Legacy Apple ${state} clusters are not silently migrated into another lifecycle`, async ({ page }) => {
    const mutations = await mockClusters(page, { ...fixture, provider: 'apple', state })
    await expect(page.getByRole('button', { name: 'Upgrade cluster', exact: true })).toHaveCount(0)
    await expect(page.getByText('Apple Cluster does not support in-place upgrades.', { exact: false })).toBeVisible()
    await page.getByRole('tab', { name: 'Overview', exact: true }).click()
    const lifecycle = page.getByRole('button', { name: state === 'running' ? 'Stop cluster' : 'Start cluster', exact: true })
    await expect(lifecycle.first()).toBeDisabled()
    await expect(lifecycle.last()).toBeDisabled()
    await expect(page.getByRole('tab', { name: 'k9s terminal', exact: true })).toBeVisible()
    await page.getByRole('tab', { name: 'Node group', exact: true }).click()
    await expect(page.getByRole('button', { name: 'Scale node group', exact: true })).toHaveCount(0)
    await page.getByRole('tab', { name: 'Import image', exact: true }).click()
    await expect(page.getByText('Apple container image store', { exact: false })).toBeVisible()
    await page.getByRole('tab', { name: 'Autoscaling', exact: true }).click()
    await expect(page.getByLabel('Autoscale worker nodes with Karpenter')).toBeDisabled()
    expect(mutations).toHaveLength(0)
  })
}

const nativeCapabilities: NonNullable<KubernetesCluster['capabilities']> = {
  nativeNodes: true, start: true, stop: true, restart: true, imageLoading: true,
  nodeScaling: false, upgrade: false,
}

test('directly owned Apple nodes expose start stop and restart without deletion', async ({ page }) => {
  const mutations = await mockClusters(page, { ...fixture, provider: 'apple', capabilities: nativeCapabilities }, [], 'overview')
  const inspector = page.getByRole('complementary', { name: 'fixture-cluster inspector' })
  await expect(inspector.getByRole('button', { name: 'Stop cluster', exact: true })).toBeEnabled()
  await inspector.getByRole('button', { name: 'Stop cluster', exact: true }).click()
  await expect.poll(() => mutations.length).toBe(1)
  expect(mutations[0].path).toBe('/api/kubernetes/clusters/fixture-cluster/stop')
  await expect(inspector.getByRole('button', { name: 'Start cluster', exact: true })).toBeEnabled()
  await inspector.getByRole('button', { name: 'Start cluster', exact: true }).click()
  await expect.poll(() => mutations.length).toBe(2)
  expect(mutations[1].path).toBe('/api/kubernetes/clusters/fixture-cluster/start')
  await expect(inspector.getByRole('button', { name: 'Restart cluster', exact: true })).toBeEnabled()
  await inspector.getByRole('button', { name: 'Restart cluster', exact: true }).click()
  await expect.poll(() => mutations.length).toBe(3)
  expect(mutations[2].path).toBe('/api/kubernetes/clusters/fixture-cluster/restart')
  expect(mutations.every((mutation) => !mutation.path.endsWith('/delete'))).toBe(true)
})

test('native transitional and interrupted states retain visible retry progress', async ({ page }) => {
  await mockClusters(page, {
    ...fixture, provider: 'apple', state: 'stopping', capabilities: nativeCapabilities,
    lifecycle: { id: 'synthetic-operation', goal: 'stopped', stage: 'graceful-node-shutdown',
      startedAt: '2026-11-11T12:00:00Z', nodes: { 'synthetic-worker': 'stopped', 'synthetic-control': 'checking' } },
  }, [], 'overview')
  const inspector = page.getByRole('complementary', { name: 'fixture-cluster inspector' })
  await expect(inspector.getByText('stopped: graceful-node-shutdown', { exact: true })).toBeVisible()
  await expect(inspector.getByText('synthetic-worker', { exact: true })).toBeVisible()
  await expect(inspector.getByRole('button', { name: 'Start cluster', exact: true })).toBeDisabled()
  await expect(inspector.getByRole('button', { name: 'Stop cluster', exact: true })).toBeDisabled()
  await expect(inspector.getByRole('button', { name: 'Restart cluster', exact: true })).toBeDisabled()
})
