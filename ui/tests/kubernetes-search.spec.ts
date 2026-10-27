import { expect, test } from '@playwright/test'

const namespaces = ['app-1', 'app2', 'application', 'Application', 'my-app-team', 'unrelated']
const screens = [
  { route: 'pods', endpoint: 'pods', list: 'Kubernetes pods', filter: 'Filter pods' },
  { route: 'deployments', endpoint: 'deployments', list: 'Kubernetes deployments', filter: 'Filter deployments' },
  { route: 'jobs', endpoint: 'jobs', list: 'Kubernetes jobs', filter: 'Filter jobs' },
  { route: 'cronjobs', endpoint: 'cronjobs', list: 'Kubernetes CronJobs', filter: 'Filter CronJobs' },
  { route: 'services', endpoint: 'services', list: 'Kubernetes services', filter: 'Filter services' },
  { route: 'configs', endpoint: 'configmaps', list: 'Kubernetes config maps', filter: 'Filter config maps' },
  { route: 'secrets', endpoint: 'secrets', list: 'Kubernetes secrets', filter: 'Filter secrets' },
  { route: 'storage', endpoint: 'persistent-volume-claims', list: 'Kubernetes persistent volume claims', filter: 'Filter Kubernetes storage', tab: 'Persistent volume claims' },
  { route: 'gateways', endpoint: 'gateways', list: 'Kubernetes Gateways', filter: 'Filter Gateway API resources' },
  { route: 'gateways', endpoint: 'http-routes', list: 'Kubernetes HTTPRoutes', filter: 'Filter Gateway API resources', tab: 'HTTP routes' },
]

for (const screen of screens) {
  test(`${screen.endpoint} search matches namespace substrings without case sensitivity`, async ({ page }) => {
    const requests: string[] = []
    await page.route('**/api/**', async (route) => {
      const url = new URL(route.request().url())
      const path = url.pathname
      const values: Record<string, unknown> = {
        '/api/settings': { dockerEnabled: false, kubernetesEnabled: true, vmsEnabled: false },
        '/api/kubernetes/status': { available: true, enabled: true, context: 'synthetic-context' },
        '/api/kubernetes/contexts': [{ name: 'synthetic-context', cluster: 'synthetic-cluster', namespace: 'default' }],
        '/api/docker/status': { available: false, enabled: false },
        '/api/docker/cleanup': { enabled: false, dockerEnabled: false, runs: [] },
      }
      if (path === `/api/kubernetes/${screen.endpoint}`) {
        requests.push(url.searchParams.get('namespace') ?? '')
        const exact = url.searchParams.get('namespace')
        values[path] = namespaces.filter((namespace) => !exact || exact === 'all' || namespace === exact).map((namespace, index) => ({
          name: `fixture-${index}`, uid: `synthetic-${index}`, namespace, node: 'synthetic-node', age: '1m',
          phase: 'Running', ready: '1/1', restarts: 0, containers: [], state: 'running',
          desired: 1, current: 1, updated: 1, available: 1, strategy: 'RollingUpdate',
          succeeded: 0, completions: 1, active: 1, failed: 0, schedule: '0 * * * *', concurrencyPolicy: 'Forbid',
          type: 'ClusterIP', clusterIP: '10.0.0.1', ports: [], keys: [], binaryKeys: [], accessModes: [],
          storageClass: 'synthetic', volume: 'synthetic-volume', className: 'synthetic-class',
          programmed: 'True', accepted: 'True', addresses: [], listeners: [], hostnames: [], parentRefs: [], backendRefs: [], parents: [],
        }))
      }
      await route.fulfill({ contentType: 'application/json', body: JSON.stringify(values[path] ?? []) })
    })
    await page.goto(`/#/${screen.route}`)
    if (screen.tab) await page.getByRole('tab', { name: screen.tab, exact: true }).click()
    const rows = page.getByRole('listbox', { name: screen.list, exact: true }).getByRole('option')
    await expect(rows).toHaveCount(namespaces.length)
    const namespace = page.getByRole('textbox', { name: 'Namespace', exact: true })
    for (const query of ['app', 'App', 'APP', ' app ']) {
      await namespace.fill(query)
      await expect(rows).toHaveCount(5)
      await expect(rows.filter({ hasText: 'unrelated' })).toHaveCount(0)
    }
    await namespace.fill('ppl')
    await expect(rows).toHaveCount(2)
    await namespace.fill('app-1')
    await expect(rows).toHaveCount(1)
    await namespace.clear()
    await expect(rows).toHaveCount(namespaces.length)
    const search = page.getByRole('searchbox', { name: screen.filter, exact: true })
    await search.fill('APP')
    await expect(rows).toHaveCount(5)
    await namespace.fill('app2')
    await expect(rows).toHaveCount(1)
    await search.clear()
    await namespace.clear()
    await expect(rows).toHaveCount(namespaces.length)
    await namespace.fill('app.')
    await expect(rows).toHaveCount(0)
    expect(requests.every((value) => value === '' || value === 'all')).toBe(true)
  })
}
