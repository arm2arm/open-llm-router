import { test, expect, type Page } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { limits } from './accessibility-fixture'

const mib = 1024 * 1024
const user = {
  id: 'member',
  username: 'Member',
  email: 'member@example.org',
  groups: ['staff'],
  issuer: 'https://identity.example',
  protected_admin: false,
  enabled: true,
  active_keys: 0,
  tokens_today: 0,
  tokens_this_month: 0,
  overrides: {},
}
const initial = () => ({
  version: 0,
  overrides: {
    max_bytes: null as number | null,
    max_traces: null as number | null,
  },
  defaults: { max_bytes: 64 * mib, max_traces: 512 },
  effective: { max_bytes: 64 * mib, max_traces: 512 },
  bounds: { max_mib: 1048576, max_traces: 1000000 },
})
async function mock(page: Page) {
  const defaults = initial()
  const personal = {
    ...initial(),
    storage: {
      bytes: 63 * mib,
      traces: 400,
      next_expiry: '2026-11-28T08:00:00Z',
    },
  }
  const writes: unknown[] = []
  let conflict = false
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
      path === 'admin/trace-quotas' ||
      path === `admin/users/${user.id}/trace-quota`
    ) {
      const view = path === 'admin/trace-quotas' ? defaults : personal
      if (route.request().method() === 'PUT') {
        const write = route.request().postDataJSON()
        writes.push(write)
        if (conflict) {
          conflict = false
          defaults.version++
          defaults.overrides.max_traces = defaults.effective.max_traces = 100
          await route.fulfill({
            status: 409,
            json: {
              error: {
                message:
                  'Trace quotas changed; reload the saved quotas before editing again.',
              },
            },
          })
          return
        }
        view.version++
        view.overrides = {
          max_bytes: write.max_bytes,
          max_traces: write.max_traces,
        }
        view.effective = {
          max_bytes: write.max_bytes ?? view.defaults.max_bytes,
          max_traces: write.max_traces ?? view.defaults.max_traces,
        }
      }
      body = view
    }
    await route.fulfill({ json: body })
  })
  return {
    writes,
    setConflict: () => {
      conflict = true
    },
  }
}

test('admin saves defaults, validates quotas and resets to deployment values', async ({
  page,
}) => {
  const { writes } = await mock(page)
  await page.goto('/portal/admin/logging')
  await expect(page).toHaveTitle(/Request logging/)
  const form = page.getByRole('form', { name: 'Default log quotas' })
  await expect(form).toContainText('64 MiB')
  const storage = form.getByLabel('Maximum storage (MiB)', { exact: true })
  const count = form.getByLabel('Maximum stored traces', { exact: true })
  const save = form.getByRole('button', { name: 'Save defaults', exact: true })
  for (const invalid of ['0', '-1', '1.5', '1048577', '1,000', '1e3']) {
    await storage.fill(invalid)
    await save.click()
    await expect(storage).toBeFocused()
    await expect(storage).toHaveAttribute('aria-invalid', 'true')
    expect(writes).toHaveLength(0)
  }
  await storage.fill('256')
  await count.fill('1000001')
  await save.click()
  await expect(count).toBeFocused()
  expect(writes).toHaveLength(0)
  await count.fill('4096')
  await save.click()
  await expect(page.getByRole('status')).toContainText('Default quotas saved')
  expect(writes).toEqual([
    { version: 0, max_bytes: 256 * mib, max_traces: 4096 },
  ])
  await expect(form).toContainText('256 MiB')
  await form.getByRole('button', { name: 'Use deployment defaults' }).click()
  await save.click()
  await expect(form).toContainText('64 MiB')
  expect(writes.at(-1)).toEqual({
    version: 1,
    max_bytes: null,
    max_traces: null,
  })
})

test('user quota inherits each global field separately and keeps stored history visible', async ({
  page,
}) => {
  const { writes } = await mock(page)
  await page.goto('/portal/admin/users')
  await page.getByRole('button', { name: 'Manage', exact: true }).click()
  const form = page.getByRole('form', { name: 'User log quota' })
  await expect(
    form.getByRole('group', { name: 'Current log storage' }),
  ).toContainText('63 MiB · 400 traces')
  await form.getByLabel('Maximum storage (MiB)', { exact: true }).fill('128')
  await form.getByRole('button', { name: 'Save user quota' }).click()
  await expect(page.getByRole('status')).toContainText('User quota saved')
  expect(writes.at(-1)).toEqual({
    version: 0,
    max_bytes: 128 * mib,
    max_traces: null,
  })
  await expect(form).toContainText('128 MiB')
  await expect(
    form.getByRole('group', { name: 'Current log storage' }),
  ).toContainText('400 traces')
  await form.getByRole('button', { name: 'Use global defaults' }).click()
  await form.getByRole('button', { name: 'Save user quota' }).click()
  await expect(form).toContainText('64 MiB')
  expect(writes.at(-1)).toEqual({
    version: 1,
    max_bytes: null,
    max_traces: null,
  })
})

test('stale edits require an explicit reload before saving again', async ({
  page,
}) => {
  const { writes, setConflict } = await mock(page)
  await page.goto('/portal/admin/logging')
  const form = page.getByRole('form', { name: 'Default log quotas' })
  await form.getByLabel('Maximum stored traces', { exact: true }).fill('10')
  setConflict()
  await form.getByRole('button', { name: 'Save defaults' }).click()
  await expect(form.getByRole('alert')).toContainText('reload')
  await expect(
    form.getByRole('button', { name: 'Save defaults' }),
  ).toBeDisabled()
  await form.getByRole('button', { name: 'Reload saved quotas' }).click()
  await expect(
    form.getByLabel('Maximum stored traces', { exact: true }),
  ).toHaveValue('100')
  await expect(
    form.getByRole('button', { name: 'Save defaults' }),
  ).toBeEnabled()
  expect(writes).toHaveLength(1)
})

for (const theme of ['light', 'dark'] as const)
  test(`log quota controls are accessible and fit mobile in ${theme} theme`, async ({
    page,
  }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    await page.emulateMedia({ colorScheme: theme })
    await mock(page)
    for (const path of ['logging', 'users']) {
      await page.goto(`/portal/admin/${path}`)
      if (path === 'users')
        await page.getByRole('button', { name: 'Manage', exact: true }).click()
      const name = path === 'users' ? 'User log quota' : 'Default log quotas'
      const form = page.getByRole('form', { name })
      await expect(form).toBeVisible()
      const result = await new AxeBuilder({ page })
        .include(`[aria-label="${name}"]`)
        .withTags(['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa'])
        .analyze()
      expect(result.violations).toEqual([])
      for (const control of await form.locator('input, button').all()) {
        const box = await control.boundingBox()
        expect(box).not.toBeNull()
        expect(box!.x).toBeGreaterThanOrEqual(0)
        expect(box!.x + box!.width).toBeLessThanOrEqual(390)
      }
    }
  })
