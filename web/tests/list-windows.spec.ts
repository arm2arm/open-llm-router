import { test, expect } from '@playwright/test'

const limits = {
  tokens_per_day: null,
  tokens_per_month: null,
  rpm: null,
  tpm: null,
  max_concurrent: null,
  max_concurrent_batch: null,
  strict_quota: false,
}
const member = {
  id: 'first-user',
  issuer: 'https://identity.example',
  username: 'First member',
  email: 'first@example.org',
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

test('capped history identifies an incomplete run window', async ({ page }) => {
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(e.message))
  const requests = Array.from({ length: 200 }, (_, i) => ({
    request_id: `request-${i}`,
    operation: 'chat/completions',
    trace_available: false,
    time: new Date(Date.now() - i * 1000).toISOString(),
    model: 'test',
    protocol: 'openai',
    status: '200',
    input_tokens: 10,
    output_tokens: 20,
    latency_ms: 100,
    stream: i !== 199,
    conversation: 's-window',
  }))
  await page.route('**/portal/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname.replace(
      '/portal/api/',
      '',
    )
    const body =
      path === 'me'
        ? {
            user: { id: 'viewer', username: 'Viewer', groups: ['staff'] },
            limits,
            admin: false,
          }
        : path === 'requests'
          ? { requests, limit: 200, has_more: true }
          : path === 'trace-preferences'
            ? { available: true, enabled: false, retention_days: 7 }
            : {}
    await route.fulfill({ json: body })
  })
  await page.goto('/portal/requests?live=off')
  await expect(page.getByRole('status')).toContainText(
    'Showing the latest 200 requests',
  )
  await expect(
    page.getByText('Earlier calls may be outside this window.', {
      exact: false,
    }),
  ).toBeVisible()
  await expect(page.locator('.trace-group-head')).toContainText('first shown')
  await expect(page.locator('.trace-group-head')).not.toContainText('started')
  await expect(page.locator('.trace-group-head')).toContainText('shown ·')
  await page.getByLabel('Filter requests').selectOption('stream')
  await expect(
    page.getByText('Earlier calls may be outside this window.', {
      exact: false,
    }),
  ).toBeVisible()
  expect(errors).toEqual([])
})

test('administrator can search beyond the user cap for a policy preview', async ({
  page,
}) => {
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(e.message))
  let previewUser = ''
  const late = {
    ...member,
    id: 'late-user',
    username: 'Zebra beyond window',
    email: 'zebra@example.org',
  }
  const document = {
    models: {},
    aliases: {},
    groups: { staff: { models: ['*'] } },
    backends: {},
    backend_presets: {},
  }
  await page.route('**/portal/api/**', async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname.replace('/portal/api/', '')
    let body: unknown = {}
    if (path === 'me')
      body = {
        user: { id: 'operator', username: 'Operator', groups: ['admins'] },
        limits,
        admin: true,
      }
    else if (path === 'admin/users')
      body =
        url.searchParams.get('match') === 'zebra'
          ? { users: [late], limit: 500, has_more: false }
          : { users: [member], limit: 500, has_more: true }
    else if (path === 'admin/groups') body = { groups: [] }
    else if (path === 'admin/endpoints') body = { managed: true, models: [] }
    else if (path === 'admin/replicas') body = { models: [] }
    else if (path === 'admin/settings/routing')
      body = {
        settings: {
          domain: 'routing',
          managed: true,
          version: 1,
          active: { id: 1, document, created_at: new Date().toISOString() },
          effective: [],
          conflicts: [],
          history: [],
        },
        runtime: { revision: 1 },
        credential_references: [],
        allowed_private_networks: [],
        group_grants: {},
        affected_users: [],
        impact_available: false,
        encryption_key_configured: true,
      }
    else if (path === 'admin/settings/routing/preview') {
      previewUser = route.request().postDataJSON().user_id
      body = {
        models: [],
        aliases: {},
        admin: false,
        priority: 0,
        limits,
        affected_users: [],
        overrides: {},
        memberships: ['staff'],
      }
    }
    await route.fulfill({ json: body })
  })
  await page.goto('/portal/admin/users')
  await expect(page.getByRole('status')).toContainText(
    'Showing the first 500 matching users',
  )
  await page
    .getByRole('link', { name: 'Advanced routing', exact: true })
    .click()
  const preview = page.getByRole('group', { name: 'Effective access preview' })
  await expect(preview.getByRole('status')).toContainText('Narrow the search')
  await preview.getByLabel('Find a user by name or email').fill('zebra')
  await expect(
    preview.getByRole('option', { name: late.username, exact: true }),
  ).toHaveCount(1)
  await preview.getByLabel('Preview user').selectOption(late.id)
  await preview.getByLabel('Find a user by name or email').fill('')
  await expect(preview.getByRole('status')).toContainText('Narrow the search')
  await expect(preview.getByLabel('Preview user')).toHaveValue(late.id)
  await preview
    .getByRole('button', { name: 'Preview access and affected users' })
    .click()
  await expect.poll(() => previewUser).toBe(late.id)
  await expect(page.getByRole('alert')).toHaveCount(0)
  expect(errors).toEqual([])
})
