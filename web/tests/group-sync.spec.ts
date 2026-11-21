import { test, expect } from '@playwright/test'
import { mockAccessiblePortal } from './accessibility-fixture'

const result = {
  checked: 2,
  updated: 1,
  disabled: 0,
  failed: 0,
  remaining: 0,
  skipped: false,
}

test('sync now prevents double submission and refreshes user memberships', async ({
  page,
}) => {
  await mockAccessiblePortal(page)
  await page.setViewportSize({ width: 390, height: 844 })
  let synced = false
  await page.route('**/portal/api/admin/groups', (route) =>
    route.fulfill({
      json: {
        groups: [
          {
            group: 'staff',
            members: synced ? 0 : 1,
            requests_today: 0,
            tokens_today: 0,
            tokens_this_month: 0,
            admin: false,
          },
          {
            group: 'research',
            members: synced ? 1 : 0,
            requests_today: 0,
            tokens_today: 0,
            tokens_this_month: 0,
            admin: false,
          },
        ],
        sync_enabled: true,
      },
    }),
  )
  let submissions = 0
  let finish!: () => void
  const pending = new Promise<void>((resolve) => {
    finish = resolve
  })
  await page.route('**/portal/api/admin/users?**', (route) =>
    route.fulfill({
      json: {
        users: [
          {
            id: 'member',
            username: 'Member',
            email: 'member@example.org',
            issuer: 'https://identity.example',
            enabled: true,
            groups: synced ? ['research'] : ['staff'],
            active_keys: 0,
            tokens_today: 0,
            tokens_this_month: 0,
            overrides: {},
          },
        ],
      },
    }),
  )
  await page.route('**/portal/api/admin/groups/sync', async (route) => {
    submissions++
    expect(route.request().method()).toBe('POST')
    expect(route.request().postDataJSON()).toEqual({})
    await pending
    synced = true
    await route.fulfill({ json: { result } })
  })
  await page.goto('/portal/admin/users')
  const controls = page.getByRole('region', { name: 'Group totals' })
  await expect(page.getByTestId('users-table')).toContainText('staff')
  await controls.getByRole('button', { name: 'Sync now', exact: true }).click()
  await expect(
    controls.getByRole('button', { name: 'Syncing…', exact: true }),
  ).toBeDisabled()
  finish()
  await expect(controls.getByRole('status')).toContainText(
    '2 checked, 1 updated',
  )
  await expect(page.getByTestId('users-table')).toContainText('research')
  const research = controls.getByRole('row').filter({ hasText: 'research' })
  await expect(
    research.getByRole('cell', { name: '1', exact: true }),
  ).toBeVisible()
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(390)
  await expect(
    controls.getByRole('button', { name: 'Sync now', exact: true }),
  ).toBeEnabled()
  expect(submissions).toBe(1)
})

test('disabled sync explains how to enable it', async ({ page }) => {
  await mockAccessiblePortal(page)
  await page.goto('/portal/admin/users')
  const controls = page.getByRole('region', { name: 'Group totals' })
  await expect(
    controls.getByRole('button', { name: 'Sync now', exact: true }),
  ).toBeDisabled()
  await expect(controls).toContainText('Configure a sync client and secret')
})

test('sync now reports overlapping runs, partial progress and provider failures', async ({
  page,
}) => {
  await mockAccessiblePortal(page)
  await page.route('**/portal/api/admin/groups', (route) =>
    route.fulfill({ json: { groups: [], sync_enabled: true } }),
  )
  let attempt = 0
  await page.route('**/portal/api/admin/groups/sync', (route) => {
    attempt++
    if (attempt === 1)
      return route.fulfill({ json: { result: { ...result, skipped: true } } })
    if (attempt === 2)
      return route.fulfill({
        json: {
          result: { ...result, failed: 1, remaining: 3 },
          problem: 'Group sync did not finish. Try again.',
        },
      })
    return route.fulfill({
      status: 503,
      json: {
        error: {
          type: 'sync_unavailable',
          message: 'Keycloak group sync is unavailable.',
        },
      },
    })
  })
  await page.goto('/portal/admin/users')
  const controls = page.getByRole('region', { name: 'Group totals' })
  const button = controls.getByRole('button', { name: 'Sync now', exact: true })
  await button.click()
  await expect(controls.getByRole('status')).toContainText('already running')
  await button.click()
  await expect(controls.getByRole('alert')).toContainText(
    '1 failed, 3 remaining',
  )
  await button.click()
  await expect(controls.getByRole('alert')).toHaveText(
    'Keycloak group sync is unavailable.',
  )
  await expect(button).toBeEnabled()
})

test('Keycloak sync interval offers sync for the running configuration to SSO admins', async ({
  page,
}) => {
  await mockAccessiblePortal(page)
  await page.route('**/portal/api/admin/settings/authentication', (route) =>
    route.fulfill({
      json: {
        settings: {
          managed: false,
          version: 0,
          effective: [{ name: 'sync_interval_s', value: 900, source: 'file' }],
          conflicts: [],
          history: [],
        },
        runtime: {
          revision: 0,
          sso_configured: true,
          sso_available: true,
          group_sync_enabled: true,
        },
        urls: {},
        other_issuers: [],
        can_change: false,
        change_requirement: 'Settings changes need a local sign-in.',
      },
    }),
  )
  await page.route('**/portal/api/admin/groups/sync', (route) =>
    route.fulfill({ json: { result } }),
  )
  await page.goto('/portal/admin/keycloak')
  const interval = page
    .getByRole('row')
    .filter({ hasText: 'Sync interval (seconds)' })
  await expect(interval).toContainText('900')
  await interval.getByRole('button', { name: 'Sync now', exact: true }).click()
  await expect(interval.getByRole('status')).toContainText(
    'Group sync complete',
  )
})
