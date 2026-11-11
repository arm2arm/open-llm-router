import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: './tests',
  timeout: 60_000,
  fullyParallel: false,
  workers: 1,
  reporter: 'list',
  use: {
    baseURL: process.env.OLR_BASE_URL ?? 'http://localhost:8080',
    browserName: 'chromium',
    permissions: ['clipboard-read', 'clipboard-write'],
    // A trace, video or failure screenshot could retain a newly created key.
    trace: 'off',
    video: 'off',
    screenshot: 'off',
  },
})
