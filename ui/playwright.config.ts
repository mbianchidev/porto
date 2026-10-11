import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: './tests',
  fullyParallel: true,
  workers: 2,
  use: {
    baseURL: 'http://127.0.0.1:4174', headless: true,
    browserName: process.env.PORTO_TEST_BROWSER === 'firefox' ? 'firefox' : process.env.PORTO_TEST_BROWSER === 'webkit' ? 'webkit' : 'chromium',
    channel: process.env.PORTO_TEST_BROWSER && process.env.PORTO_TEST_BROWSER !== 'chromium' ? undefined : 'chromium',
  },
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 4174 --strictPort',
    url: 'http://127.0.0.1:4174',
    reuseExistingServer: false,
  },
})
