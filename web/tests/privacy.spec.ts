import { test, expect, type Locator, type Page } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { mockAccessiblePortal } from './accessibility-fixture'

async function tabTo(page: Page, link: Locator) {
  for (let i = 0; i < 50; i++) {
    await page.keyboard.press('Tab')
    if (await link.evaluate((element) => element === document.activeElement))
      return
  }
  await expect(link).toBeFocused()
}

for (const privacyURL of ['', 'https://site.example/privacy']) {
  for (const path of ['/portal/sign-in', '/portal/sign-in/local']) {
    test(`sign-in privacy notice ${privacyURL ? 'configured' : 'unconfigured'} at ${path}`, async ({
      page,
    }) => {
      await page.setViewportSize({ width: 360, height: 780 })
      await page.route('**/portal/api/auth/session', (route) =>
        route.fulfill({ status: 401, json: {} }),
      )
      await page.route('**/portal/api/auth', (route) =>
        route.fulfill({
          json: {
            privacy_url: privacyURL,
            default: 'local',
            local: { available: true },
            oidc: { configured: false, available: false },
          },
        }),
      )
      await page.goto(path)
      await expect(
        page.getByRole('button', { name: 'Sign in with local account' }),
      ).toBeEnabled()
      const link = page.getByRole('link', {
        name: 'Site privacy notice',
        exact: true,
      })
      if (privacyURL) {
        await expect(link).toHaveAttribute('href', privacyURL)
        await tabTo(page, link)
      } else {
        await expect(link).toHaveCount(0)
        await expect(
          page.getByText(/No site privacy notice is linked/),
        ).toBeVisible()
      }
      expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
      expect(
        await page.evaluate(() => document.documentElement.scrollWidth),
      ).toBeLessThanOrEqual(360)
      if (privacyURL) {
        await page.route(privacyURL, (route) =>
          route.fulfill({
            contentType: 'text/html',
            body: '<h1>Site privacy information</h1>',
          }),
        )
        await page.keyboard.press('Enter')
        await expect(
          page.getByRole('heading', { name: 'Site privacy information' }),
        ).toBeVisible()
      }
    })
  }
}

for (const privacyURL of ['', 'https://site.example/privacy']) {
  test(`Connect explains AI and recipients with ${privacyURL ? 'configured' : 'unconfigured'} site notice`, async ({
    page,
  }) => {
    await mockAccessiblePortal(page)
    await page.setViewportSize({ width: 390, height: 844 })
    await page.route('**/portal/api/models', (route) =>
      route.fulfill({
        json: {
          router_url: 'https://router.example',
          base_url: 'https://router.example/v1',
          privacy_url: privacyURL,
          models: [
            {
              id: 'alias',
              canonical_model: 'mixed',
              protocols: ['openai'],
              recipients: [
                {
                  organization: 'Research Institute',
                  privacy_url: 'https://institute.example/privacy',
                },
                { organization: '', privacy_url: '' },
              ],
              recipients_complete: false,
            },
            {
              id: 'complete',
              canonical_model: 'complete',
              protocols: ['openai'],
              recipients: [
                {
                  organization: 'Other Institute',
                  privacy_url: 'https://other.example/privacy',
                },
              ],
              recipients_complete: true,
            },
          ],
        },
      }),
    )
    await page.goto('/portal/connect')
    await expect(
      page.getByRole('heading', { name: 'Before you send a prompt' }),
    ).toBeVisible()
    await expect(
      page.getByText(/Turning off personal request tracing/),
    ).toBeVisible()
    await expect(
      page.getByRole('heading', { name: 'Possible recipients for alias' }),
    ).toBeVisible()
    await expect(page.getByText(/Unlabelled destination/)).toBeVisible()
    await expect(
      page.getByText(/Recipient information is incomplete/),
    ).toBeVisible()
    const site = page.getByRole('link', {
      name: 'Site privacy notice',
      exact: true,
    })
    if (privacyURL) {
      await expect(site).toHaveAttribute('href', privacyURL)
      await tabTo(page, site)
    } else {
      await expect(site).toHaveCount(0)
      await expect(
        page.getByText(/No site privacy notice is linked/),
      ).toBeVisible()
    }
    const recipient = page.getByRole('link', {
      name: 'Privacy notice for Research Institute',
    })
    await tabTo(page, recipient)
    await expect(recipient).toHaveAttribute(
      'href',
      'https://institute.example/privacy',
    )
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth),
    ).toBeLessThanOrEqual(390)
    await page.getByLabel('Model', { exact: true }).selectOption('complete')
    await expect(
      page.getByRole('heading', { name: 'Possible recipients for complete' }),
    ).toBeVisible()
    await expect(
      page.getByText(/Recipient information is incomplete/),
    ).toHaveCount(0)
    await expect(
      page.getByRole('link', { name: 'Privacy notice for Other Institute' }),
    ).toBeVisible()
  })
}
