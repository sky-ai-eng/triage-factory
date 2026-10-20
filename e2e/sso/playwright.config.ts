import { defineConfig } from '@playwright/test'

// One worker, in file order: the scenarios share one stack and build on the
// state earlier ones leave (orgs, memberships, Require SSO).
export default defineConfig({
  testDir: './tests',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 90_000,
  expect: { timeout: 15_000 },
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : [['list']],
  use: {
    baseURL: 'http://localhost:3000',
    ignoreHTTPSErrors: true,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    launchOptions: {
      // The test IdPs and Orrery listen on loopback under .test names.
      args: ['--host-resolver-rules=MAP *.test 127.0.0.1'],
    },
  },
  projects: [{ name: 'chromium', use: { browserName: 'chromium' } }],
})
