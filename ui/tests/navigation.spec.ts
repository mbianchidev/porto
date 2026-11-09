import { expect, test, type Locator, type Page } from '@playwright/test'
import type { StorageUsage } from '../src/dataTypes'
import type { EnvironmentProfile } from '../src/managementTypes'
import type { RouteID } from '../src/types'

type Destination = [RouteID, string]
const navigation: Array<{
  name: string
  pages: Destination[]
  sections?: Array<{ name: string; pages: Destination[] }>
}> = [
  { name: 'Development', pages: [['localhost-ing', 'localhost-ing'], ['applications', 'Applications'], ['databases', 'Databases Soon']] },
  {
    name: 'Containers', pages: [['containers', 'Containers'], ['networks', 'Networks']],
    sections: [
      { name: 'Images & builds', pages: [['images', 'Images'], ['builds', 'Builds']] },
      { name: 'Storage & data', pages: [['volumes', 'Volumes'], ['docker-storage', 'Storage tools'], ['migration', 'Migrate runtime data']] },
    ],
  },
  {
    name: 'Kubernetes', pages: [['kubernetes', 'Overview']],
    sections: [
      { name: 'Workloads', pages: [['deployments', 'Deployments'], ['pods', 'Pods'], ['jobs', 'Jobs'], ['cronjobs', 'Scheduled jobs']] },
      { name: 'Networking', pages: [['services', 'Services'], ['port-forwards', 'Port forwarding'], ['gateways', 'Gateway API']] },
      { name: 'Storage & config', pages: [['storage', 'Storage'], ['configs', 'ConfigMaps'], ['secrets', 'Secrets']] },
      { name: 'Delivery', pages: [['helm', 'Helm releases'], ['gitops', 'GitOps applications']] },
      { name: 'Cluster tools', pages: [['nodes', 'Nodes'], ['kubernetes-admin', 'Resources and RBAC']] },
    ],
  },
  {
    name: 'System', pages: [
      ['environments', 'Environments and fleets'], ['machines', 'Virtual machines'], ['extensions', 'Extensions'],
      ['activity', 'Activity'], ['diagnostics', 'Diagnostics'], ['settings', 'Settings'],
    ],
  },
]

const importedID = 'a'.repeat(32)
const dockerID = 'b'.repeat(32)
const remoteID = 'c'.repeat(32)
const profile = (id: string, name: string, backend: EnvironmentProfile['backend']): EnvironmentProfile => ({
  id, name, backend, address: backend === 'local' ? 'loopback' : 'https://synthetic.invalid',
  identity: `synthetic-navigation-${id}`, capabilities: [], labels: {}, state: 'online',
  revision: 1, revoked: false, remoteRevoked: false, owned: backend === 'local',
})
const emptyStorage: StorageUsage = {
  resources: [], contentBytes: 0, snapshotBytes: 0, writableBytes: 0, warnings: [],
  accounting: 'Synthetic empty storage inventory.',
}

async function mockNavigation(page: Page) {
  await page.route('**/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname.replace(/^\/api\/environments\/(?:local|[a-f0-9]{32})(\/api\/)/, '$1')
    const values: Record<string, unknown> = {
      '/api/environments': [
        profile('local', 'This machine', 'local'),
        profile(importedID, 'Synthetic imported cluster', 'kubernetes'),
        profile(dockerID, 'Synthetic Docker endpoint', 'docker'),
        profile(remoteID, 'Synthetic Porto host', 'porto'),
      ],
      '/api/settings': { dockerEnabled: false, kubernetesEnabled: false, vmsEnabled: false, theme: 'dark', protectedBranches: [] },
      '/api/docker/status': { available: false, enabled: false, context: 'synthetic-navigation' },
      '/api/docker/cleanup': { enabled: false, dockerEnabled: false, runs: [] },
      '/api/docker/containers/snapshot': { instanceId: 'synthetic-navigation', revision: 1, available: false, stale: false, containers: [], capabilities: {} },
      '/api/docker/storage': emptyStorage,
      '/api/kubernetes/status': { available: false, enabled: false },
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(values[path] ?? []) })
  })
}

async function expand(toggle: Locator) {
  if (await toggle.getAttribute('aria-expanded') === 'false') await toggle.click()
}

test.beforeEach(async ({ page }) => {
  await mockNavigation(page)
})

test.afterEach(async ({ page }) => {
  expect(await page.pageErrors()).toEqual([])
})

test('four main sections retain every existing destination in short groups', async ({ page }, testInfo) => {
  await page.goto('/#/localhost-ing', { waitUntil: 'networkidle' })
  const rail = page.getByRole('navigation', { name: 'Primary navigation' })
  await page.screenshot({ path: testInfo.outputPath('sidebar.png'), animations: 'disabled' })
  await expect(rail.getByRole('button')).toHaveCount(6)
  await expect(rail.getByRole('button', { name: 'Development', exact: true })).toHaveAttribute('aria-expanded', 'true')
  const destinations = navigation.flatMap((group) => [...group.pages, ...(group.sections ?? []).flatMap((section) => section.pages)])
  const hrefs = await rail.locator('a').evaluateAll((links) => links
    .filter((link) => link.getAttribute('aria-label') !== 'Porto')
    .map((link) => link.getAttribute('href')).sort())
  expect(hrefs).toEqual(destinations.map(([id]) => `#/${id}`).sort())

  let expandedCount = 0
  for (const group of navigation) {
    await expand(rail.getByRole('button', { name: group.name, exact: true }))
    expandedCount += 1
    await expect(rail.getByRole('button', { expanded: true })).toHaveCount(expandedCount)
    for (const [id, label] of group.pages) {
      const link = rail.getByRole('link', { name: label, exact: true })
      await expect(link).toBeVisible()
      await expect(link).toHaveAttribute('href', `#/${id}`)
    }
    for (const section of group.sections ?? []) {
      await expand(rail.getByRole('button', { name: section.name, exact: true }))
      expandedCount += 1
      for (const [id, label] of section.pages) {
        const link = rail.getByRole('link', { name: label, exact: true })
        await expect(link).toBeVisible()
        await expect(link).toHaveAttribute('href', `#/${id}`)
      }
      await expect(rail.getByRole('button', { expanded: true })).toHaveCount(expandedCount)
    }
  }
})

test('sections and subgroups expand independently and retain their nested choices', async ({ page }) => {
  await page.goto('/#/gitops')
  const rail = page.getByRole('navigation', { name: 'Primary navigation' })
  const containers = rail.getByRole('button', { name: 'Containers', exact: true })
  const kubernetes = rail.getByRole('button', { name: 'Kubernetes', exact: true })
  const images = rail.getByRole('button', { name: 'Images & builds', exact: true })
  const data = rail.getByRole('button', { name: 'Storage & data', exact: true })
  const workloads = rail.getByRole('button', { name: 'Workloads', exact: true })
  const delivery = rail.getByRole('button', { name: 'Delivery', exact: true })
  await containers.click()
  await expect(kubernetes).toHaveAttribute('aria-expanded', 'true')
  await images.click()
  await data.click()
  await workloads.click()
  for (const toggle of [containers, kubernetes, images, data, workloads, delivery]) {
    await expect(toggle).toHaveAttribute('aria-expanded', 'true')
  }
  await expect(rail.getByRole('link', { name: 'Images', exact: true })).toBeVisible()
  await expect(rail.getByRole('link', { name: 'Volumes', exact: true })).toBeVisible()
  await expect(rail.getByRole('link', { name: 'Pods', exact: true })).toBeVisible()
  await expect(rail.getByRole('link', { name: 'GitOps applications', exact: true })).toBeVisible()

  await containers.click()
  await expect(rail.getByRole('link', { name: 'Images', exact: true })).toBeHidden()
  await expect(kubernetes).toHaveAttribute('aria-expanded', 'true')
  await containers.click()
  await expect(images).toHaveAttribute('aria-expanded', 'true')
  await expect(data).toHaveAttribute('aria-expanded', 'true')
  await workloads.click()
  await expect(delivery).toHaveAttribute('aria-expanded', 'true')
  await expect(images).toHaveAttribute('aria-expanded', 'true')
})

test('expand all and collapse all control every section and subgroup with the keyboard', async ({ page }) => {
  await page.goto('/#/gitops')
  const rail = page.getByRole('navigation', { name: 'Primary navigation' })
  const expandAll = rail.getByRole('button', { name: 'Expand all', exact: true })
  const collapseAll = rail.getByRole('button', { name: 'Collapse all', exact: true })
  const disclosureCount = navigation.reduce((count, group) => count + 1 + (group.sections?.length ?? 0), 0)
  await expandAll.focus()
  await page.keyboard.press('Enter')
  await expect(expandAll).toBeFocused()
  await expect(rail.getByRole('button', { expanded: true })).toHaveCount(disclosureCount)
  for (const group of navigation) {
    for (const [, label] of [...group.pages, ...(group.sections ?? []).flatMap((section) => section.pages)]) {
      await expect(rail.getByRole('link', { name: label, exact: true })).toBeVisible()
    }
  }
  for (const toggle of await rail.getByRole('button', { expanded: true }).all()) {
    const contentID = await toggle.getAttribute('aria-controls')
    expect(contentID).toBeTruthy()
    await expect(page.locator(`[id="${contentID}"]`)).toBeVisible()
  }

  await page.keyboard.press('Tab')
  await expect(collapseAll).toBeFocused()
  await page.keyboard.press('Space')
  await expect(collapseAll).toBeFocused()
  await expect(rail.getByRole('button', { expanded: true, includeHidden: true })).toHaveCount(0)
  await expect(rail.getByRole('button', { expanded: false, includeHidden: true })).toHaveCount(disclosureCount)
  for (const group of navigation) {
    await rail.getByRole('button', { name: group.name, exact: true }).click()
    for (const section of group.sections ?? []) {
      await expect(rail.getByRole('button', { name: section.name, exact: true })).toHaveAttribute('aria-expanded', 'false')
      for (const [, label] of section.pages) {
        await expect(rail.getByRole('link', { name: label, exact: true })).toBeHidden()
      }
    }
  }
  await expandAll.click()
  await expect(rail.getByRole('button', { expanded: true })).toHaveCount(disclosureCount)
})

for (const [route, label, group, section] of [
  ['gitops', 'GitOps applications', 'Kubernetes', 'Delivery'],
  ['cronjobs', 'Scheduled jobs', 'Kubernetes', 'Workloads'],
  ['gateways', 'Gateway API', 'Kubernetes', 'Networking'],
  ['configs', 'ConfigMaps', 'Kubernetes', 'Storage & config'],
  ['kubernetes-admin', 'Resources and RBAC', 'Kubernetes', 'Cluster tools'],
  ['docker-storage', 'Storage tools', 'Containers', 'Storage & data'],
  ['builds', 'Builds', 'Containers', 'Images & builds'],
  ['machines', 'Virtual machines', 'System', ''],
]) {
  test(`deep link to ${route} reveals its current page and only its branch`, async ({ page }) => {
    await page.goto(`/#/${route}`)
    const rail = page.getByRole('navigation', { name: 'Primary navigation' })
    await expect(rail.getByRole('button', { name: group, exact: true })).toHaveAttribute('aria-expanded', 'true')
    if (section) await expect(rail.getByRole('button', { name: section, exact: true })).toHaveAttribute('aria-expanded', 'true')
    await expect(rail.getByRole('link', { name: label, exact: true })).toHaveAttribute('aria-current', 'page')
    await expect(rail.locator('a[aria-current="page"]')).toHaveCount(1)
    await expect(rail.getByRole('button', { expanded: true })).toHaveCount(section ? 2 : 1)
  })
}

test('navigation and browser history reveal the current branch without closing other sections', async ({ page }) => {
  await page.goto('/#/gitops')
  const rail = page.getByRole('navigation', { name: 'Primary navigation' })
  await rail.getByRole('button', { name: 'Workloads', exact: true }).click()
  await expect(rail.getByRole('button', { name: 'Delivery', exact: true })).toHaveAttribute('aria-expanded', 'true')
  await rail.getByRole('link', { name: 'Pods', exact: true }).click()
  await expect(page).toHaveURL(/#\/pods$/)
  await expect(rail.getByRole('link', { name: 'Pods', exact: true })).toHaveAttribute('aria-current', 'page')
  await rail.getByRole('button', { name: 'Delivery', exact: true }).click()
  await page.goBack()
  await expect(rail.getByRole('link', { name: 'GitOps applications', exact: true })).toHaveAttribute('aria-current', 'page')
  await expect(rail.getByRole('button', { name: 'Workloads', exact: true })).toHaveAttribute('aria-expanded', 'true')
  await rail.getByRole('button', { name: 'Workloads', exact: true }).click()
  await page.goForward()
  await expect(rail.getByRole('link', { name: 'Pods', exact: true })).toHaveAttribute('aria-current', 'page')
  await expect(rail.getByRole('button', { name: 'Delivery', exact: true })).toHaveAttribute('aria-expanded', 'true')
  await page.getByRole('link', { name: 'Manage environments', exact: true }).click()
  await expect(rail.getByRole('button', { name: 'System', exact: true })).toHaveAttribute('aria-expanded', 'true')
  await expect(rail.getByRole('link', { name: 'Environments and fleets', exact: true })).toHaveAttribute('aria-current', 'page')
  await expect(rail.getByRole('button', { name: 'Kubernetes', exact: true })).toHaveAttribute('aria-expanded', 'true')
  await expect(rail.getByRole('link', { name: 'Pods', exact: true })).toBeVisible()
  await expect(rail.getByRole('link', { name: 'GitOps applications', exact: true })).toBeVisible()
})

test('environment capabilities filter both groups and nested destinations without local fallback', async ({ page }) => {
  await page.goto('/#/gitops')
  const selector = page.getByRole('combobox', { name: 'Environment', exact: true })
  const rail = page.getByRole('navigation', { name: 'Primary navigation' })
  await selector.selectOption(importedID)
  await expect(rail.getByRole('button', { name: 'Kubernetes', exact: true })).toBeVisible()
  await expect(rail.getByRole('button', { name: 'Development', exact: true })).toHaveCount(0)
  await expect(rail.getByRole('button', { name: 'Containers', exact: true })).toHaveCount(0)
  await expect(rail.getByRole('button', { name: 'Workloads', exact: true })).toHaveCount(0)
  await expect(rail.getByRole('button', { name: 'Networking', exact: true })).toHaveCount(0)
  await expect(rail.getByRole('button', { name: 'Storage & config', exact: true })).toHaveCount(0)
  await expect(rail.getByRole('link', { name: 'GitOps applications', exact: true })).toHaveAttribute('aria-current', 'page')
  await expect(rail.getByRole('link', { name: 'Helm releases', exact: true })).toBeVisible()
  await expand(rail.getByRole('button', { name: 'Cluster tools', exact: true }))
  await expect(rail.getByRole('link', { name: 'Resources and RBAC', exact: true })).toBeVisible()
  await expect(rail.getByRole('link', { name: 'Nodes', exact: true })).toHaveCount(0)
  await rail.getByRole('button', { name: 'Expand all', exact: true }).click()
  await expect(rail.getByRole('button', { expanded: false, includeHidden: true })).toHaveCount(0)

  await selector.selectOption(dockerID)
  await expect(page.getByRole('heading', { name: 'Unsupported target capability' })).toBeVisible()
  await expect(page).toHaveURL(/#\/gitops$/)
  await expect(rail.getByRole('button', { name: 'Kubernetes', exact: true })).toHaveCount(0)
  await expand(rail.getByRole('button', { name: 'Containers', exact: true }))
  await expect(rail.getByRole('link', { name: 'Containers', exact: true })).toBeVisible()
  await expect(rail.getByRole('button', { name: 'Images & builds', exact: true })).toHaveCount(0)
  await expect(rail.getByRole('button', { name: 'Storage & data', exact: true })).toHaveCount(0)
  await rail.getByRole('button', { name: 'Collapse all', exact: true }).click()
  await rail.getByRole('button', { name: 'Expand all', exact: true }).click()
  await expect(rail.getByRole('button', { expanded: false, includeHidden: true })).toHaveCount(0)
  await expect(rail.getByRole('button', { name: 'Kubernetes', exact: true })).toHaveCount(0)

  await selector.selectOption(remoteID)
  await expand(rail.getByRole('button', { name: 'Containers', exact: true }))
  await expand(rail.getByRole('button', { name: 'Storage & data', exact: true }))
  await expect(rail.getByRole('link', { name: 'Volumes', exact: true })).toBeVisible()
  await expect(rail.getByRole('link', { name: 'Storage tools', exact: true })).toHaveCount(0)
  await expect(rail.getByRole('link', { name: 'Migrate runtime data', exact: true })).toHaveCount(0)
})

test('disclosures and skip navigation work with the keyboard', async ({ page }) => {
  await page.goto('/#/containers')
  const rail = page.getByRole('navigation', { name: 'Primary navigation' })
  const images = rail.getByRole('button', { name: 'Images & builds', exact: true })
  await images.focus()
  await page.keyboard.press('Space')
  await expect(images).toHaveAttribute('aria-expanded', 'true')
  await page.keyboard.press('Tab')
  await expect(rail.getByRole('link', { name: 'Images', exact: true })).toBeFocused()
  await page.keyboard.press('Enter')
  await expect(page).toHaveURL(/#\/images$/)
  const skip = page.getByRole('link', { name: 'Skip to content', exact: true })
  await skip.focus()
  await page.keyboard.press('Enter')
  await expect(page.getByRole('main')).toBeFocused()
  await expect(page).toHaveURL(/#\/images$/)
})

test('narrow navigation manages focus, dismissal, touch targets and viewport changes', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/#/containers')
  const rail = page.locator('nav[aria-label="Primary navigation"]')
  const main = page.getByRole('main')
  const open = page.getByRole('button', { name: 'Open navigation', exact: true })
  await expect(rail).toHaveAttribute('inert', '')
  await expect(rail).toBeHidden()
  await expect(open).toHaveAttribute('aria-controls', await rail.getAttribute('id') ?? '')
  await open.focus()
  await page.keyboard.press('Enter')
  const close = page.getByRole('button', { name: 'Close navigation', exact: true })
  await expect(main).toHaveAttribute('inert', '')
  await expect(rail.getByRole('link', { name: 'Containers', exact: true })).toBeFocused()
  await close.focus()
  await page.keyboard.press('Shift+Tab')
  await expect(rail.getByRole('button', { name: 'System', exact: true })).toBeFocused()
  await page.keyboard.press('Tab')
  await expect(close).toBeFocused()
  await page.keyboard.press('Escape')
  await expect(open).toBeFocused()
  await expect(rail).toBeHidden()
  await expect(main).not.toHaveAttribute('inert', '')

  await open.click()
  await rail.getByRole('button', { name: 'System', exact: true }).click()
  await page.keyboard.press('Escape')
  await open.click()
  await expect(rail.getByRole('link', { name: 'Containers', exact: true })).toBeFocused()
  await expand(rail.getByRole('button', { name: 'Storage & data', exact: true }))
  for (const control of [...await rail.getByRole('link').all(), ...await rail.getByRole('button').all()]) {
    const bounds = await control.boundingBox()
    expect(bounds?.height).toBeGreaterThanOrEqual(44)
  }
  await rail.getByRole('link', { name: 'Storage tools', exact: true }).click()
  await expect(page).toHaveURL(/#\/docker-storage$/)
  await expect(open).toBeFocused()
  await expect(rail).toBeHidden()
  await open.click()
  await page.getByRole('button', { name: 'Dismiss navigation', exact: true }).click({ position: { x: 300, y: 100 } })
  await expect(open).toBeFocused()

  await open.click()
  await page.setViewportSize({ width: 861, height: 844 })
  await expect(rail).toBeVisible()
  await expect(main).not.toHaveAttribute('inert', '')
  await page.setViewportSize({ width: 860, height: 844 })
  await expect(open).toHaveAttribute('aria-expanded', 'false')
  await expect(rail).toBeHidden()
})

test('the grouped rail stays dark and fits desktop and narrow layouts in both themes', async ({ page }, testInfo) => {
  await page.goto('/#/gitops', { waitUntil: 'networkidle' })
  const rail = page.locator('nav[aria-label="Primary navigation"]')
  await rail.getByRole('button', { name: 'Expand all', exact: true }).click()
  for (const theme of ['dark', 'light']) {
    await page.evaluate((value) => { document.documentElement.dataset.theme = value }, theme)
    for (const width of [1440, 1024, 390]) {
      await page.setViewportSize({ width, height: 900 })
      if (width <= 860) await page.getByRole('button', { name: 'Open navigation', exact: true }).click()
      await expect(rail.getByRole('link', { name: 'GitOps applications', exact: true })).toBeVisible()
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width)
      expect(await rail.evaluate((element) => getComputedStyle(element).backgroundColor)).toBe('rgb(15, 16, 17)')
      await page.screenshot({ path: testInfo.outputPath(`sidebar-${theme}-${width}.png`), animations: 'disabled' })
      if (width <= 860) await page.keyboard.press('Escape')
    }
  }
})

test('narrow navigation preserves expansion and recursive collapse when dismissed', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/#/gitops')
  const rail = page.locator('nav[aria-label="Primary navigation"]')
  const open = page.getByRole('button', { name: 'Open navigation', exact: true })
  await open.click()
  await rail.getByRole('button', { name: 'Expand all', exact: true }).click()
  await page.keyboard.press('Escape')
  await open.click()
  await expect(rail.getByRole('button', { expanded: false, includeHidden: true })).toHaveCount(0)
  await rail.getByRole('link', { name: 'Images', exact: true }).click()
  await expect(page).toHaveURL(/#\/images$/)
  await expect(rail).toBeHidden()
  await open.click()
  await expect(rail.getByRole('button', { expanded: false, includeHidden: true })).toHaveCount(0)
  for (const control of await rail.getByRole('button').all()) {
    expect((await control.boundingBox())?.height).toBeGreaterThanOrEqual(44)
  }
  await rail.getByRole('button', { name: 'Collapse all', exact: true }).click()
  await page.keyboard.press('Escape')
  await open.click()
  await expect(rail.getByRole('button', { expanded: true, includeHidden: true })).toHaveCount(0)
  await expect(rail.getByRole('link', { name: 'Porto', exact: true })).toBeFocused()
  await rail.getByRole('button', { name: 'Kubernetes', exact: true }).click()
  await expect(rail.getByRole('button', { name: 'Delivery', exact: true })).toHaveAttribute('aria-expanded', 'false')
  await page.keyboard.press('Escape')
  await expect(open).toBeFocused()
})
