import { test, expect, type Page } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { mockAccessiblePortal } from './accessibility-fixture'

async function axe(page: Page) {
  const result = await new AxeBuilder({ page })
    .withTags(['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa'])
    .analyze()
  expect(
    result.violations.map((v) => ({
      id: v.id,
      nodes: v.nodes.map((n) => ({
        target: n.target,
        summary: n.failureSummary,
      })),
    })),
  ).toEqual([])
}

// No rule exclusions: use real components and only synthetic API data.
for (const theme of ['light', 'dark'] as const) {
  test(`portal views pass WCAG automated checks in ${theme} theme`, async ({
    page,
  }) => {
    const errors: string[] = []
    page.on('pageerror', (e) => errors.push(e.message))
    await page.emulateMedia({ colorScheme: theme })
    await mockAccessiblePortal(page)
    for (const path of [
      'keys',
      'usage',
      'connect',
      'requests?live=off',
      'admin',
      'admin/presets',
      'admin/users',
      'admin/advanced',
      'admin/agents',
    ]) {
      await page.goto(`/portal/${path}`)
      await expect(page.locator('main')).toBeVisible()
      await expect(page.getByRole('heading', { level: 1 })).toBeVisible()
      await axe(page)
    }
    expect(errors).toEqual([])
  })
}

test('public home passes automated checks and has a clear keyboard focus', async ({
  page,
}) => {
  await page.goto(process.env.OLR_HOME_URL ?? '/')
  await expect(
    page.getByRole('link', { name: 'Open LLM Router home' }),
  ).toBeVisible()
  await page.keyboard.press('Tab')
  await expect(
    page.getByRole('link', { name: 'Skip to content' }),
  ).toBeFocused()
  const colors = await page.evaluate(() => {
    const ring = getComputedStyle(
      document.querySelector('.skip-link')!,
    ).outlineColor
    const backgrounds = [...document.querySelectorAll('a')].map((link) => {
      let parent = link.parentElement
      while (
        parent &&
        getComputedStyle(parent).backgroundColor === 'rgba(0, 0, 0, 0)'
      )
        parent = parent.parentElement
      return parent
        ? getComputedStyle(parent).backgroundColor
        : getComputedStyle(document.documentElement).backgroundColor
    })
    return { ring, backgrounds }
  })
  for (const background of colors.backgrounds)
    expect(ratio(colors.ring, background)).toBeGreaterThanOrEqual(3)
  await axe(page)
})

test('daily chart uses one tab stop and supports zero-value days', async ({
  page,
}) => {
  await mockAccessiblePortal(page)
  await page.goto('/portal/usage')
  const chart = page.getByRole('listbox', {
    name: 'Daily token usage',
    exact: false,
  })
  const days = chart.getByRole('option')
  await expect(days).toHaveCount(30)
  await expect(chart.locator('[tabindex="0"]')).toHaveCount(1)
  await chart.locator('[tabindex="0"]').focus()
  await page.keyboard.press('Home')
  await expect(days.first()).toBeFocused()
  await expect(days.first()).toHaveAttribute('aria-selected', 'true')
  await expect(days.first()).toHaveAccessibleName(/0 tokens/)
  const stroke = await days
    .first()
    .locator('.chart-hit')
    .evaluate((e) => getComputedStyle(e).stroke)
  expect(stroke).not.toBe('none')
  await page.keyboard.press('ArrowRight')
  await expect(days.nth(1)).toBeFocused()
  await page.keyboard.press('End')
  await expect(days.last()).toBeFocused()
  await expect(chart.locator('[tabindex="0"]')).toHaveCount(1)
  await page.keyboard.press('Tab')
  await expect(days.last()).not.toBeFocused()
  await axe(page)
})

test('logging save retains form focus and announces the result', async ({
  page,
}) => {
  await mockAccessiblePortal(page)
  let release!: () => void
  const saving = new Promise<void>((resolve) => {
    release = resolve
  })
  await page.route('**/portal/api/trace-preferences', async (route) => {
    if (route.request().method() === 'PUT') await saving
    await route.fallback()
  })
  await page.goto('/portal/requests?live=off')
  const settings = page.getByRole('region', {
    name: 'Detailed logging settings',
  })
  await settings.getByRole('switch').check()
  await settings.getByRole('button', { name: 'Save settings' }).click()
  await expect(settings.getByRole('button', { name: 'Saving…' })).toBeDisabled()
  release()
  await expect(settings.getByRole('status')).toContainText(
    'Detailed logging enabled. Traces kept for 7 days.',
  )
  await expect(settings.getByRole('switch')).toBeFocused()
  const firstTitle = await page.title()
  await page.getByRole('link', { name: 'Usage', exact: true }).click()
  await expect(page).not.toHaveTitle(firstTitle)
  await expect(page).toHaveTitle('Usage · Open LLM Router')
})

test('trace JSON has named disclosure controls and no invalid tree role', async ({
  page,
}) => {
  await mockAccessiblePortal(page)
  await page.goto('/portal/requests/a11y-trace?live=off')
  await expect(page.getByRole('dialog')).toBeVisible()
  await page.getByRole('tab', { name: 'Raw', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByRole('tree')).toHaveCount(0)
  const names = await dialog
    .locator('.json-toggle')
    .evaluateAll((nodes) => nodes.map((n) => n.getAttribute('aria-label')))
  expect(names.length).toBeGreaterThan(1)
  expect(new Set(names).size).toBe(names.length)
  await axe(page)
})

test('request filters do not rewrite a live region', async ({ page }) => {
  await mockAccessiblePortal(page)
  await page.goto('/portal/requests?live=off')
  await expect(page.locator('.trace-history tbody')).not.toHaveAttribute(
    'aria-live',
  )
  await expect(page.locator('.trace-group')).toHaveCount(1)
  await page.evaluate(() => {
    const changes: string[] = []
    Object.assign(window, { a11yLiveChanges: changes })
    new MutationObserver((records) => {
      for (const record of records) {
        const target =
          record.target instanceof Element
            ? record.target
            : record.target.parentElement
        if (
          record.addedNodes.length &&
          target?.closest(
            '[aria-live]:not([aria-live="off"]),[role="status"],[role="alert"]',
          )
        )
          changes.push(target.tagName)
      }
    }).observe(document.body, { childList: true, subtree: true })
  })
  await page.getByLabel('Filter requests').selectOption('stream')
  await page.getByRole('button', { name: 'Group runs', exact: true }).click()
  await expect(page.locator('.trace-group')).toHaveCount(0)
  await page.getByLabel('Search requests').fill('no-matching-request')
  await expect(page.locator('.trace-history tbody')).toHaveCount(0)
  expect(
    await page.evaluate(
      () =>
        (window as unknown as { a11yLiveChanges: string[] }).a11yLiveChanges,
    ),
  ).toEqual([])
  await expect(
    page.locator('.trace-history tbody').locator('[aria-live]'),
  ).toHaveCount(0)
})

function rgb(color: string): number[] {
  if (color.startsWith('#')) {
    const hex =
      color.length === 4
        ? '#' + [...color.slice(1)].map((c) => c + c).join('')
        : color
    return [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16))
  }
  return (color.match(/[\d.]+/g) ?? []).slice(0, 3).map(Number)
}
function ratio(a: string, b: string) {
  const luminance = (c: string) =>
    rgb(c)
      .map((v) => v / 255)
      .map((v) => (v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4))
      .reduce((sum, v, i) => sum + v * [0.2126, 0.7152, 0.0722][i], 0)
  const x = luminance(a),
    y = luminance(b)
  return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05)
}
for (const theme of ['light', 'dark'] as const) {
  test(`form controls meet non-text contrast in ${theme} theme`, async ({
    page,
  }) => {
    await page.emulateMedia({ colorScheme: theme })
    await mockAccessiblePortal(page)
    await page.goto('/portal/requests?live=off')
    const input = page.getByLabel('Trace retention days')
    const border = await input.evaluate((e) => ({
      border: getComputedStyle(e).borderTopColor,
      panel: getComputedStyle(e.closest('.panel')!).backgroundColor,
    }))
    expect(ratio(border.border, border.panel)).toBeGreaterThanOrEqual(3)
    await input.evaluate((e) => e.setAttribute('aria-invalid', 'true'))
    const invalid = await input.evaluate((e) => ({
      actual: getComputedStyle(e).borderTopColor,
      expected: getComputedStyle(e).getPropertyValue('--destructive').trim(),
    }))
    expect(rgb(invalid.actual)).toEqual(rgb(invalid.expected))
    await input.evaluate((e) => e.removeAttribute('aria-invalid'))
    await input.focus()
    const focus = await input.evaluate((e) => ({
      actual: getComputedStyle(e).borderTopColor,
      expected: getComputedStyle(e).getPropertyValue('--ring').trim(),
    }))
    expect(rgb(focus.actual)).toEqual(rgb(focus.expected))
    const toggle = page.getByRole('switch')
    for (const checked of [false, true]) {
      await toggle.setChecked(checked)
      const colors = await toggle.evaluate((e) => ({
        track: getComputedStyle(e).backgroundColor,
        knob: getComputedStyle(e, '::after').backgroundColor,
        panel: getComputedStyle(e.closest('.panel')!).backgroundColor,
      }))
      expect(ratio(colors.track, colors.panel)).toBeGreaterThanOrEqual(3)
      expect(ratio(colors.knob, colors.track)).toBeGreaterThanOrEqual(3)
    }
    for (const selector of ['.trace-tool-chip', 'th']) {
      const colors = await page
        .locator(selector)
        .first()
        .evaluate((e) => ({
          color: getComputedStyle(e).color,
          bg: getComputedStyle(e.closest('th') ?? e.closest('.panel')!)
            .backgroundColor,
        }))
      expect(ratio(colors.color, colors.bg)).toBeGreaterThanOrEqual(4.5)
    }
  })
}

async function inViewport(page: Page, locator: ReturnType<Page['getByRole']>) {
  await locator.scrollIntoViewIfNeeded()
  const box = await locator.boundingBox()
  expect(box).not.toBeNull()
  const size = page.viewportSize()!
  expect(box!.x).toBeGreaterThanOrEqual(0)
  expect(box!.y).toBeGreaterThanOrEqual(0)
  expect(box!.x + box!.width).toBeLessThanOrEqual(size.width)
  expect(box!.y + box!.height).toBeLessThanOrEqual(size.height)
}
for (const viewport of [
  { width: 640, height: 360 },
  { width: 1366, height: 650 },
]) {
  test(`restriction dialog scrolls at ${viewport.width} by ${viewport.height}`, async ({
    page,
  }) => {
    await page.setViewportSize(viewport)
    await mockAccessiblePortal(page)
    await page.goto('/portal/admin/agents')
    await page
      .getByRole('button', { name: 'Restrict access', exact: true })
      .click()
    const dialog = page.getByRole('dialog')
    await inViewport(page, dialog.getByRole('heading'))
    await inViewport(
      page,
      dialog.getByRole('button', { name: 'Restrict access', exact: true }),
    )
    await axe(page)
  })
}
for (const width of [320, 390]) {
  test(`agent access reflows at ${width} pixels`, async ({ page }) => {
    await page.setViewportSize({ width, height: 844 })
    await mockAccessiblePortal(page)
    await page.goto('/portal/admin/agents')
    await inViewport(
      page,
      page.getByRole('button', { name: 'Revoke', exact: true }),
    )
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth),
    ).toBe(width)
    await axe(page)
  })
}
for (const viewport of [
  { width: 640, height: 360 },
  { width: 844, height: 390 },
]) {
  test(`personal key dialog actions stay reachable at ${viewport.width} by ${viewport.height}`, async ({
    page,
  }) => {
    await page.setViewportSize(viewport)
    await mockAccessiblePortal(page)
    await page.route('**/portal/api/keys', async (route) =>
      route.fulfill({
        json:
          route.request().method() === 'POST'
            ? { key: 'olr-synthetic-browser-fixture' }
            : { keys: [] },
      }),
    )
    await page.goto('/portal/keys')
    await page.getByRole('button', { name: 'Create key', exact: true }).click()
    const dialog = page.getByRole('dialog')
    await inViewport(
      page,
      dialog.getByRole('button', { name: 'Cancel', exact: true }),
    )
    await inViewport(
      page,
      dialog.getByRole('button', { name: 'Create key', exact: true }),
    )
    await dialog.getByLabel('Key label').fill('Synthetic example')
    await dialog
      .getByRole('button', { name: 'Create key', exact: true })
      .click()
    await inViewport(
      page,
      dialog.getByRole('button', { name: 'I’ve saved my key', exact: true }),
    )
    await axe(page)
  })
}
