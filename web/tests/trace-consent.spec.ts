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
