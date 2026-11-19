import { test, expect } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { mockAccessiblePortal, limits } from './accessibility-fixture'

const person = {
  id: 'profile-user',
  username: 'ada',
  email: 'ada@example.org',
  full_name: 'Dr. Ada Lovelace',
  given_name: 'Ada',
  family_name: 'Lovelace',
  institution: 'Example Research Institute',
  groups: ['staff'],
  protected_admin: false,
}

for (const theme of ['light', 'dark'] as const) {
  test(`institutional profile is readable and accessible on mobile in ${theme}`, async ({
    page,
  }) => {
    await mockAccessiblePortal(page)
    await page.emulateMedia({ colorScheme: theme })
    await page.setViewportSize({ width: 360, height: 780 })
    await page.route('**/portal/api/auth/session', (route) =>
      route.fulfill({
        json: { auth_method: 'oidc', must_change_password: false },
      }),
    )
    await page.route('**/portal/api/me', (route) =>
      route.fulfill({ json: { user: person, limits, admin: false } }),
    )
    await page.goto('/portal/account')
    const profile = page.getByRole('region', { name: 'Your profile' })
    await expect(
      profile.getByRole('heading', { name: person.full_name }),
    ).toBeVisible()
    for (const value of [
      person.full_name,
      person.given_name,
      person.family_name,
      person.institution,
      person.username,
      person.email,
    ]) {
      await expect(
        profile.locator('dl').getByText(value, { exact: true }),
      ).toBeVisible()
    }
    await expect(profile).toContainText('updated when you sign in')
    await expect(
      page.getByRole('heading', { name: 'Password managed by Keycloak' }),
    ).toBeVisible()
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth),
    ).toBeLessThanOrEqual(360)
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
  })
}

test('missing optional claims retain the username and local password controls', async ({
  page,
}) => {
  await mockAccessiblePortal(page)
  await page.goto('/portal/account')
  const profile = page.getByRole('region', { name: 'Your profile' })
  await expect(profile.getByRole('heading', { name: 'Operator' })).toBeVisible()
  await expect(profile.getByText('Not provided', { exact: true })).toHaveCount(
    4,
  )
  await expect(page.getByLabel('Current password')).toBeVisible()
})

test('admin user management displays profile fields and searches by institution', async ({
  page,
}) => {
  await mockAccessiblePortal(page)
  await page.route('**/portal/api/admin/users?**', (route) =>
    route.fulfill({
      json: {
        users: [
          {
            ...person,
            issuer: 'https://idp.example/realms/test',
            enabled: true,
            active_keys: 0,
            tokens_today: 0,
            tokens_this_month: 0,
            overrides: {},
          },
        ],
        limit: 500,
        has_more: false,
      },
    }),
  )
  await page.route('**/portal/api/admin/users/profile-user', (route) =>
    route.fulfill({
      json: {
        user: {
          ...person,
          issuer: 'https://idp.example/realms/test',
          has_local_password: false,
        },
        enabled: true,
        limits,
        overrides: [],
        keys: [],
        models: [],
        admin: false,
      },
    }),
  )
  await page.goto('/portal/admin/users')
  await page
    .getByPlaceholder('Name, email or institution')
    .fill(person.institution)
  const request = page.waitForRequest(
    (r) =>
      r.url().includes('/admin/users?') &&
      new URL(r.url()).searchParams.get('match') === person.institution,
  )
  await page.getByRole('button', { name: 'Find', exact: true }).click()
  await request
  const table = page.getByTestId('users-table')
  await expect(table.getByText(person.full_name, { exact: true })).toBeVisible()
  await expect(table.getByText(person.username, { exact: true })).toBeVisible()
  await expect(
    table.getByText(person.institution, { exact: true }),
  ).toBeVisible()
  await table.getByRole('button', { name: 'Manage', exact: true }).click()
  await expect(
    table.locator('dl').getByText(person.family_name, { exact: true }),
  ).toBeVisible()
})

test('profile claims render as text, never HTML', async ({ page }) => {
  await mockAccessiblePortal(page)
  const name = '<img src=x onerror="window.profileInjected=true">'
  await page.route('**/portal/api/me', (route) =>
    route.fulfill({
      json: {
        user: { ...person, full_name: name, institution: name },
        limits,
        admin: false,
      },
    }),
  )
  await page.goto('/portal/account')
  const profile = page.getByRole('region', { name: 'Your profile' })
  await expect(profile.getByRole('heading', { name })).toBeVisible()
  await expect(profile.locator('img')).toHaveCount(0)
  expect(
    await page.evaluate(() =>
      Object.prototype.hasOwnProperty.call(window, 'profileInjected'),
    ),
  ).toBe(false)
})
