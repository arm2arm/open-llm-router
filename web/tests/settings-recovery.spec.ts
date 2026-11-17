import { test, expect, type Page } from '@playwright/test'
import { mockAccessiblePortal } from './accessibility-fixture'

const authentication = {
  enabled: false,
  default_sign_in: 'local',
  issuer: 'https://identity.example',
  backchannel_url: '',
  portal_client_id: 'portal',
  portal_client_secret: {},
  audience: 'router',
  groups_claim: 'groups',
  group_names: 'leaf',
  username_claim: 'preferred_username',
  required_claim: null,
  sync_client_id: 'sync',
  sync_client_secret: {},
  sync_interval_s: 300,
  sync_max_disable: 100,
}
const routing = {
  models: {},
  aliases: {},
  groups: {},
  backends: {},
  backend_presets: {},
}

async function refocus(page: Page) {
  await page.clock.fastForward(16_000)
  await page.evaluate(() => window.dispatchEvent(new Event('visibilitychange')))
}

for (const domain of ['routing', 'authentication'] as const) {
  test(`${domain} editor retains edits and its baseline after another writer saves`, async ({
    page,
  }) => {
    await mockAccessiblePortal(page)
    await page.clock.install()
    let version = 1
    let submitted: Record<string, unknown> | undefined
    await page.route(`**/portal/api/admin/settings/${domain}`, (route) =>
      route.fulfill({
        json: {
          settings: {
            domain,
            managed: true,
            version,
            active: {
              id: version,
              status: 'active',
              document:
                domain === 'routing'
                  ? routing
                  : {
                      ...authentication,
                      portal_client_id:
                        version === 1 ? 'portal' : 'other-writer',
                    },
            },
            effective: [],
            conflicts: [],
            history: [],
          },
          runtime: {
            revision: version,
            sso_configured: false,
            sso_available: false,
          },
          urls: {},
          other_issuers: [],
          can_change: true,
          encryption_key_configured: true,
          credential_references: [],
          allowed_private_networks: [],
          group_grants: {},
          affected_users: [],
          impact_available: false,
        },
      }),
    )
    await page.route(
      `**/portal/api/admin/settings/${domain}/draft`,
      (route) => {
        submitted = route.request().postDataJSON()
        return route.fulfill({
          status: 409,
          json: {
            error: { message: 'Settings changed; reload before saving.' },
          },
        })
      },
    )
    await page.goto(
      `/portal/admin/${domain === 'routing' ? 'advanced' : 'keycloak'}`,
    )
    const form = page.getByRole('form', {
      name:
        domain === 'routing'
          ? 'Edit routing draft'
          : 'Edit authentication draft',
    })
    const input = form.getByLabel(
      domain === 'routing' ? 'New logical model name' : 'Portal client ID',
      { exact: true },
    )
    await input.fill('my-unsaved-work')
    if (domain === 'routing')
      await form.getByRole('button', { name: 'Add model', exact: true }).click()
    else {
      await form
        .getByLabel('Portal client secret source', { exact: true })
        .selectOption('store')
      await form
        .getByLabel('New portal client secret', { exact: true })
        .fill('synthetic-unsaved-credential')
    }
    if (domain === 'routing')
      await form
        .getByLabel('my-unsaved-work replica 1 origin', { exact: true })
        .fill('https://backend.example')
    version = 2
    await refocus(page)
    await expect(form.getByRole('status')).toContainText(
      'Settings changed elsewhere. Your edits are kept.',
    )
    if (domain === 'routing')
      await expect(
        form.getByText('my-unsaved-work', { exact: true }).first(),
      ).toBeVisible()
    else {
      await expect(input).toHaveValue('my-unsaved-work')
      await expect(
        form.getByLabel('New portal client secret', { exact: true }),
      ).toHaveValue('synthetic-unsaved-credential')
    }
    await form
      .getByRole('button', {
        name: domain === 'routing' ? 'Save routing draft' : 'Save draft',
        exact: true,
      })
      .click()
    await expect(form.getByRole('alert')).toContainText(
      'Settings changed; reload before saving.',
    )
    expect(submitted?.version).toBe(1)
    if (domain === 'routing')
      expect((submitted?.document as typeof routing).models).toHaveProperty(
        'my-unsaved-work',
      )
    else
      expect(submitted?.secrets).toEqual({
        portal_client_secret: 'synthetic-unsaved-credential',
      })
    await form
      .getByRole('button', { name: 'Load latest and discard local edits' })
      .click()
    await expect(form.getByRole('status')).toHaveCount(0)
    if (domain === 'authentication') {
      await expect(input).toHaveValue('other-writer')
      await expect(
        form.getByLabel('New portal client secret', { exact: true }),
      ).toHaveCount(0)
    }
  })
}

for (const method of ['local', 'oidc'])
  test(`expired background cookie preserves an open key form through ${method} reauthentication`, async ({
    page,
    context,
  }) => {
    await mockAccessiblePortal(page)
    await context.addCookies([
      {
        name: 'olr_session',
        value: 'synthetic',
        url: new URL('/portal/', String(test.info().project.use.baseURL)).href,
      },
    ])
    const restoredCookie =
      'olr_session=synthetic; Path=/portal; HttpOnly; SameSite=Lax'
    await page.route('**/portal/api/auth', (route) =>
      route.fulfill({
        json: {
          local: { available: true },
          oidc: {
            available: method === 'oidc',
            configured: method === 'oidc',
            login_url: '/portal/sign-in/keycloak',
          },
        },
      }),
    )
    await page.route('**/portal/api/auth/session', async (route) => {
      if (
        !route.request().headers()['cookie']?.includes('olr_session=synthetic')
      )
        await route.fulfill({
          status: 401,
          json: { error: { message: 'Session expired' } },
        })
      else await route.fallback()
    })
    await page.route('**/portal/login/local', (route) => {
      return route.fulfill({
        headers: { 'Set-Cookie': restoredCookie },
        json: { must_change_password: false },
      })
    })
    await context.route('**/portal/sign-in/keycloak?**', (route) =>
      route.fulfill({
        headers: { 'Set-Cookie': restoredCookie },
        contentType: 'text/html',
        body: '<p>Synthetic successful SSO callback</p>',
      }),
    )
    await page.goto('/portal/keys')
    await page.getByRole('button', { name: 'Create key', exact: true }).click()
    const original = page.getByRole('dialog', { name: 'Create an API key' })
    await original.getByLabel('Key label').fill('Preserve this draft')
    await context.clearCookies({ name: 'olr_session' })
    await refocusWithRealTime(page)
    const recovery = page.getByRole('dialog', { name: 'Session ended' })
    await expect(recovery).toBeVisible()
    await expect(page).toHaveURL(/\/portal\/keys$/)
    if (method === 'local') {
      await recovery
        .getByLabel('Local username', { exact: true })
        .fill('same-synthetic-user')
      await recovery
        .getByLabel('Local password', { exact: true })
        .fill('synthetic-password')
      await recovery
        .getByRole('button', {
          name: 'Sign in with local account',
          exact: true,
        })
        .click()
    } else {
      const opened = context.waitForEvent('page')
      await recovery
        .getByRole('button', { name: 'Continue with Keycloak' })
        .click()
      const popup = await opened
      await popup.waitForLoadState()
      expect(new URL(popup.url()).searchParams.get('next')).toBe('/portal/keys')
      await page.bringToFront()
      await page.evaluate(() => window.dispatchEvent(new Event('focus')))
    }
    await expect(recovery).toHaveCount(0)
    await expect(original.getByLabel('Key label')).toHaveValue(
      'Preserve this draft',
    )
  })

async function refocusWithRealTime(page: Page) {
  // Session polling is stale after 15 seconds. Advance the browser clock only;
  // this exercises the actual query subscription rather than importing app code.
  await page.clock.install()
  await refocus(page)
}

for (const next of [
  '/portal/connect?tab=python',
  'https://other.example/portal/connect',
]) {
  test(`local sign-in validates its return destination ${next}`, async ({
    page,
  }) => {
    await mockAccessiblePortal(page)
    await page.route('**/portal/api/auth', (route) =>
      route.fulfill({
        json: {
          local: { available: true },
          oidc: { available: false, configured: false },
        },
      }),
    )
    await page.route('**/portal/login/local', (route) =>
      route.fulfill({ json: { must_change_password: false } }),
    )
    await page.goto(
      `/portal/sign-in/local?expired=1&next=${encodeURIComponent(next)}`,
    )
    await expect(
      page.getByRole('status').filter({ hasText: 'Your session ended.' }),
    ).toBeVisible()
    await page
      .getByLabel('Local username', { exact: true })
      .fill('synthetic-user')
    await page
      .getByLabel('Local password', { exact: true })
      .fill('synthetic-password')
    await page
      .getByRole('button', { name: 'Sign in with local account', exact: true })
      .click()
    await expect(page).toHaveURL(
      next.startsWith('/portal/')
        ? /\/portal\/connect\?tab=python$/
        : /\/portal\/keys$/,
    )
  })
}
