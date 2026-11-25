import { test, expect } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'

test.beforeEach(async ({ page }) => {
  await page.route('**/portal/api/auth/session', (route) =>
    route.fulfill({
      status: 401,
      json: { error: { message: 'Sign in required' } },
    }),
  )
})

for (const theme of ['light', 'dark'] as const) {
  test(`institutional login leads and local credentials expand accessibly in ${theme}`, async ({
    page,
  }) => {
    await page.emulateMedia({ colorScheme: theme })
    await page.setViewportSize({ width: 360, height: 780 })
    await page.route('**/portal/api/auth', (route) =>
      route.fulfill({
        json: {
          default: 'oidc',
          privacy_notice_url: 'https://example.org/privacy',
          local: { available: true },
          oidc: {
            configured: true,
            available: true,
            login_url: '/portal/sign-in/keycloak',
          },
        },
      }),
    )
    await page.goto('/portal/sign-in?next=%2Fportal%2Fkeys')
    await expect(
      page.getByRole('heading', { name: 'Welcome to Open LLM Router' }),
    ).toBeVisible()
    await expect(
      page.getByText(
        'Use your institutional account to access Open LLM Router.',
      ),
    ).toBeVisible()
    await expect(
      page.getByRole('link', { name: 'Site privacy notice' }),
    ).toHaveAttribute('href', 'https://example.org/privacy')
    await expect(
      page.getByText(/Your institution may keep you signed in in this browser/),
    ).toBeVisible()
    const primary = page.getByRole('button', {
      name: 'Continue with institutional login',
    })
    await expect(primary).toHaveAttribute('data-variant', 'default')
    await expect(page.getByText('Secure sign-in with Keycloak')).toBeVisible()
    await expect(
      page.getByLabel('Local username', { exact: true }),
    ).toBeHidden()
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth),
    ).toBeLessThanOrEqual(360)
    const disclosure = page
      .locator('summary')
      .filter({ hasText: 'Use a local account' })
    await disclosure.focus()
    await page.keyboard.press('Enter')
    await expect(
      page.getByLabel('Local username', { exact: true }),
    ).toBeVisible()
    await expect(
      page.getByRole('button', { name: 'Sign in with local account' }),
    ).toHaveAttribute('data-variant', 'outline')
    const results = await new AxeBuilder({ page }).analyze()
    expect(
      results.violations.map((v) => ({
        id: v.id,
        nodes: v.nodes.map((n) => n.target),
      })),
    ).toEqual([])
    await disclosure.click()
    await expect(
      page.getByLabel('Local password', { exact: true }),
    ).toBeHidden()
    await page.route('**/portal/sign-in/keycloak?**', (route) =>
      route.fulfill({
        contentType: 'text/html',
        body: '<h1>Institutional sign-in started</h1>',
      }),
    )
    await primary.click()
    await expect(page).toHaveURL(
      /\/portal\/sign-in\/keycloak\?next=%2Fportal%2Fkeys$/,
    )
  })
}

test('direct local recovery and local-only sites show credentials immediately', async ({
  page,
}) => {
  await page.route('**/portal/api/auth', (route) =>
    route.fulfill({
      json: {
        default: 'oidc',
        local: { available: true },
        oidc: { configured: false, available: false },
      },
    }),
  )
  await page.goto('/portal/sign-in')
  await expect(page.getByLabel('Local username', { exact: true })).toBeVisible()
  await expect(
    page.getByRole('link', { name: 'Site privacy notice' }),
  ).toHaveCount(0)
  await expect(page.locator('summary')).toHaveCount(0)
  await page.route('**/portal/api/auth', (route) =>
    route.fulfill({
      json: {
        default: 'oidc',
        local: { available: true },
        oidc: {
          configured: true,
          available: true,
          login_url: '/portal/sign-in/keycloak',
        },
      },
    }),
  )
  await page.goto('/portal/sign-in/local')
  await expect(page.getByLabel('Local username', { exact: true })).toBeVisible()
  await expect(
    page.getByRole('button', { name: 'Continue with institutional login' }),
  ).toHaveCount(0)
})
