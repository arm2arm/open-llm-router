import { test, expect } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'

for (const theme of ['light', 'dark'] as const) {
  test(`public About shows both coauthors and local avatars in ${theme}`, async ({
    page,
  }) => {
    await page.emulateMedia({ colorScheme: theme })
    await page.setViewportSize({ width: 360, height: 780 })
    let sessionRequests = 0
    await page.route('**/portal/api/auth/session', (route) => {
      sessionRequests++
      return route.fulfill({
        status: 401,
        json: { error: { message: 'Sign in required' } },
      })
    })
    const externalImages: string[] = []
    page.on('request', (request) => {
      if (
        request.resourceType() === 'image' &&
        !request.url().startsWith(new URL(page.url()).origin)
      )
        externalImages.push(request.url())
    })
    await page.goto('/portal/about')
    await expect(
      page.getByRole('heading', { name: 'About Open LLM Router' }),
    ).toBeVisible()
    await expect(page).toHaveTitle('About · Open LLM Router')
    for (const name of ['Arman Khalatyan', 'Ing. E.T.L. Strauss']) {
      const avatar = page.getByRole('img', { name, exact: true })
      await avatar.scrollIntoViewIfNeeded()
      await expect
        .poll(() =>
          avatar.evaluate((image) => (image as HTMLImageElement).naturalWidth),
        )
        .toBeGreaterThan(0)
    }
    await expect(
      page.getByText('Infrastructure Engineer', { exact: true }),
    ).toBeVisible()
    await expect(page.locator('blockquote')).toContainText('Strauss, E. T. L.')
    await expect(
      page.getByRole('link', { name: 'GitHub profile' }),
    ).toHaveAttribute('href', 'https://github.com/arm2arm')
    await expect(
      page.getByRole('heading', { name: 'AI-assisted development' }),
    ).toBeVisible()
    expect(sessionRequests).toBe(0)
    expect(externalImages).toEqual([])
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth),
    ).toBeLessThanOrEqual(360)
    const results = await new AxeBuilder({ page }).analyze()
    expect(
      results.violations.map((v) => ({
        id: v.id,
        nodes: v.nodes.map((n) => n.target),
      })),
    ).toEqual([])
    await page.getByRole('link', { name: 'Open portal' }).click()
    await expect(page).toHaveURL(/\/portal\/sign-in\?next=/)
  })
}
