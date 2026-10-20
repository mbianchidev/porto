import { expect, test, type Page } from '@playwright/test'
import type { KubernetesCluster } from '../src/types'

const fixture: KubernetesCluster = {
  name: 'fixture-cluster', provider: 'kind', state: 'running', version: 'v1.35.2',
  context: 'porto-fixture-cluster', kubeconfigPath: '/synthetic/kubeconfig.yaml',
  server: 'https://127.0.0.1:54321', nodes: ['porto-fixture-cluster-control-plane'],
}

async function mockClusters(page: Page, initial: KubernetesCluster = fixture) {
  let cluster = { ...initial }
  const mutations: Array<{ path: string; body: unknown }> = []
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (request.method() === 'POST' && path.startsWith('/api/kubernetes/clusters/')) {
      const body: unknown = request.postDataJSON()
      mutations.push({ path, body })
      if (path.endsWith('/upgrade')) {
        cluster = { ...cluster, version: 'v1.36.1' }
      } else if (path.endsWith('/upgrade-recover')) {
        cluster = { ...fixture }
      }
      await route.fulfill({ contentType: 'application/json', body: JSON.stringify({ message: 'Synthetic cluster operation completed.' }) })
      return
    }
    const values: Record<string, unknown> = {
      '/api/settings': { dockerEnabled: false, kubernetesEnabled: true, vmsEnabled: false },
      '/api/kubernetes/status': { available: true, enabled: true, context: fixture.context, serverVersion: 'v1.35.2' },
      '/api/kubernetes/contexts': [{ name: fixture.context, cluster: fixture.context, namespace: 'default' }],
      '/api/kubernetes/clusters': [cluster],
      '/api/runtime/providers': [],
      '/api/docker/status': { available: false, enabled: false },
      '/api/docker/cleanup': { enabled: false, dockerEnabled: false, runs: [] },
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(values[path] ?? []) })
  })
  await page.goto('/#/kubernetes')
  await page.getByText('fixture-cluster', { exact: true }).first().click()
  await page.getByRole('tab', { name: 'Upgrade', exact: true }).click()
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
  await expect(page.getByLabel('Target kind release')).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('kind-upgrade-mobile.png'), fullPage: true })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
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
