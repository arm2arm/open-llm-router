import { test, expect } from '@playwright/test'

test('administrator validates override numbers and chooses unlimited explicitly', async ({
  page,
}) => {
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  const writes: { field: string; value: number | null }[] = []
  const limits = {
    tokens_per_day: null,
    tokens_per_month: null,
    rpm: null,
    tpm: null,
    max_concurrent: null,
    max_concurrent_batch: null,
    strict_quota: false,
  }
  const user = {
    id: 'target-user',
    issuer: 'https://identity.example',
    username: 'Test member',
    email: 'member@example.org',
    groups: ['staff'],
    enabled: true,
    active_keys: 0,
    tokens_today: 0,
    tokens_this_month: 0,
    overrides: {},
    protected_admin: false,
    has_local_password: false,
    must_change_password: false,
  }
  await page.route('**/portal/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname.replace(
      '/portal/api/',
      '',
    )
    let body: unknown = {}
    if (path === 'me')
      body = {
        user: { id: 'operator', username: 'Operator', groups: ['admins'] },
        limits,
        admin: true,
      }
    else if (path === 'admin/users') body = { users: [user] }
    else if (path === 'admin/groups') body = { groups: [] }
    else if (path === 'admin/replicas') body = { models: [] }
    else if (path === 'admin/settings/routing')
      body = {
        settings: {
          domain: 'routing',
          managed: false,
          version: 0,
          effective: [],
          conflicts: [],
          history: [],
        },
        runtime: { revision: 0 },
        credential_references: [],
        allowed_private_networks: [],
        group_grants: {},
        affected_users: [],
        impact_available: false,
        encryption_key_configured: true,
      }
    else if (path === `admin/users/${user.id}`)
      body = {
        user,
        enabled: true,
        limits,
        overrides: [],
        keys: [],
        models: [],
        admin: false,
      }
    else if (
      path.includes('/overrides/') &&
      route.request().method() === 'PUT'
    ) {
      writes.push({
        field: path.split('/').at(-1)!,
        value: route.request().postDataJSON().value,
      })
    }
    await route.fulfill({ json: body })
  })
  await page.goto('/portal/admin/users')
  await expect(
    page.getByRole('button', { name: 'Manage', exact: true }),
  ).toBeVisible({ timeout: 5000 })
  await page.getByRole('button', { name: 'Manage', exact: true }).click()
  const form = page.getByRole('form', { name: 'Set user override' })
  const value = form.getByLabel('Value', { exact: true })
  const submit = form.getByRole('button', { name: 'Set override', exact: true })
  for (const invalid of [
    '1,000',
    '10k',
    'abc',
    '',
    '0',
    '-1',
    '1.5',
    '9007199254740992',
  ]) {
    await value.fill(invalid)
    await submit.click()
    await expect(value).toHaveAttribute('aria-invalid', 'true')
    await expect(value).toBeFocused()
    await expect(page.getByRole('alert')).toContainText('Enter a whole number')
    expect(writes).toHaveLength(0)
  }
  await value.fill('1000')
  await submit.click()
  await expect
    .poll(() => writes)
    .toEqual([{ field: 'tokens_per_day', value: 1000 }])
  await expect(submit).toBeEnabled()
  await form.getByLabel('Override type').selectOption('unlimited')
  await expect(value).toHaveCount(0)
  await submit.click()
  await expect
    .poll(() => writes.at(-1))
    .toEqual({ field: 'tokens_per_day', value: null })
  await expect(submit).toBeEnabled()
  await form
    .getByRole('combobox', { name: 'Limit', exact: true })
    .selectOption('rpm')
  await value.fill('2147483648')
  await submit.click()
  await expect(page.getByRole('alert')).toContainText('2,147,483,647')
  expect(writes).toHaveLength(2)
  await form
    .getByRole('combobox', { name: 'Limit', exact: true })
    .selectOption('strict_quota')
  await expect(form.getByLabel('Override type')).toHaveCount(0)
  await form.getByLabel('Strict quota setting').selectOption('off')
  await submit.click()
  await expect
    .poll(() => writes.at(-1))
    .toEqual({ field: 'strict_quota', value: 0 })
  await expect(page.getByRole('alert')).toHaveCount(0)
  for (const control of [
    form.getByRole('combobox', { name: 'Limit', exact: true }),
    form.getByLabel('Strict quota setting'),
    form.getByLabel('Reason', { exact: true }),
    submit,
  ]) {
    const box = await control.boundingBox()
    expect(box).not.toBeNull()
    expect(box!.x).toBeGreaterThanOrEqual(0)
    expect(box!.x + box!.width).toBeLessThanOrEqual(page.viewportSize()!.width)
  }
  expect(errors).toEqual([])
})
