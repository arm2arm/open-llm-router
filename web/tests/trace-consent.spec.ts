import { test, expect } from '@playwright/test'

test('trace settings show the saved choice timestamp and keep capture explicit', async ({
  page,
}) => {
  let preferences: {
    available: boolean
    enabled: boolean
    retention_days: number
    updated_at: string | null
  } = {
    available: true,
    enabled: false,
    retention_days: 7,
    updated_at: null,
  }
  await page.route('**/portal/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname.replace(
      '/portal/api/',
      '',
    )
    let body: unknown = {}
    if (path === 'me')
      body = {
        user: { id: 'test', username: 'tester', groups: ['staff'] },
        limits: {},
        admin: false,
      }
    else if (path === 'requests') body = { requests: [], limit: 200 }
    else if (path === 'trace-preferences') {
      if (route.request().method() === 'PUT')
        preferences = {
          ...preferences,
          ...route.request().postDataJSON(),
          updated_at: '2026-11-17T10:15:00Z',
        }
      body = preferences
    }
    await route.fulfill({ json: body })
  })
  await page.goto('/portal/requests?live=off')
  const settings = page.getByRole('region', {
    name: 'Detailed logging settings',
  })
  await expect(settings.getByRole('switch')).not.toBeChecked()
  await expect(settings).not.toContainText('Last changed')
  await settings.getByRole('switch').click()
  await expect(settings).toContainText('Enable capture (not saved)')
  await expect(settings).toContainText('Unsaved changes')
  expect(preferences.enabled).toBe(false)
  await settings.getByRole('button', { name: 'Save settings' }).click()
  await expect(settings).toContainText('Last changed')
  await expect(settings).not.toContainText('Invalid Date')
  await expect(settings.getByRole('switch')).toBeChecked()
  await expect(
    settings.getByRole('button', { name: 'Save settings' }),
  ).toBeDisabled()
  await page.reload()
  await expect(settings).toContainText('Last changed')
  await expect(settings.getByRole('switch')).toBeChecked()
})

test('nearly full trace storage explains missing details and requires confirmation before freeing space', async ({
  page,
}) => {
  let deleted = false
  const storage = {
    traces: 198,
    bytes: 67105949,
    max_traces: 512,
    max_bytes: 67108864,
    next_expiry: '2026-11-24T10:00:00Z',
  }
  await page.route('**/portal/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname.replace(
      '/portal/api/',
      '',
    )
    if (path === 'request-traces' && route.request().method() === 'DELETE') {
      deleted = true
      Object.assign(storage, { traces: 0, bytes: 0, next_expiry: null })
      await route.fulfill({ status: 204 })
      return
    }
    const body =
      path === 'me'
        ? {
            user: { id: 'test', username: 'tester', groups: ['staff'] },
            limits: {},
            admin: false,
          }
        : path === 'trace-preferences'
          ? { available: true, enabled: true, retention_days: 7, storage }
          : path === 'requests'
            ? { requests: [], limit: 200 }
            : {}
    await route.fulfill({ json: body })
  })
  await page.goto('/portal/requests?live=off')
  const settings = page.getByRole('region', {
    name: 'Detailed logging settings',
  })
  const status = settings.getByRole('status', { name: 'Trace storage status' })
  await expect(settings).toContainText('Logging for tester')
  await expect(settings).toContainText('Earlier requests cannot be backfilled')
  await expect(status).toContainText('198 / 512 traces')
  await expect(status).toContainText('almost full')
  await expect(status).toContainText('New request details may be skipped')
  storage.traces = 512
  await page.getByRole('button', { name: 'Refresh', exact: true }).click()
  await expect(status).toContainText('Trace storage limit reached')
  await status.getByRole('button', { name: 'Free trace storage…' }).click()
  const dialog = page.getByRole('dialog', {
    name: 'Delete your detailed traces?',
  })
  await expect(dialog).toBeVisible()
  expect(deleted).toBe(false)
  await dialog.getByRole('button', { name: 'Close', exact: true }).click()
  expect(deleted).toBe(false)
  await status.getByRole('button', { name: 'Free trace storage…' }).click()
  await dialog.getByRole('button', { name: 'Delete all my traces' }).click()
  await expect(dialog).not.toBeVisible()
  await expect(status).toContainText('0 / 512 traces')
  await expect(status).toContainText('0.0 / 64 MiB')
  await expect(status).not.toContainText('almost full')
  await expect(settings.getByRole('switch')).toBeChecked()
})
