import { test, expect, type Page } from '@playwright/test'
import { agentKey, mockAccessiblePortal } from './accessibility-fixture'

async function secretCannotBeDismissed(page: Page, acknowledgement: string) {
  const dialog = page.getByRole('dialog')
  await expect(
    dialog.getByRole('button', { name: 'Close', exact: true }),
  ).toHaveCount(0)
  await page.keyboard.press('Escape')
  await expect(dialog).toBeVisible()
  await page.mouse.click(5, 5)
  await expect(dialog).toBeVisible()
  await dialog
    .getByRole('button', { name: acknowledgement, exact: true })
    .click()
  await expect(dialog).toHaveCount(0)
}

test('personal key stays available through pending creation and until acknowledgement', async ({
  page,
}) => {
  await mockAccessiblePortal(page)
  let release!: () => void
  const response = new Promise<void>((resolve) => {
    release = resolve
  })
  await page.route('**/portal/api/keys', async (route) => {
    if (route.request().method() !== 'POST') {
      await route.fulfill({ json: { keys: [] } })
      return
    }
    await response
    await route.fulfill({ json: { key: 'olr-synthetic-once-only' } })
  })
  await page.goto('/portal/keys')
  await page.getByRole('button', { name: 'Create key', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('Key label').fill('Synthetic once-only key')
  await dialog.getByRole('button', { name: 'Create key', exact: true }).click()
  await expect(dialog.getByRole('button', { name: 'Creating…' })).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(dialog).toBeVisible()
  release()
  await expect(dialog.getByTestId('new-key')).toHaveValue(
    'olr-synthetic-once-only',
  )
  await secretCannotBeDismissed(page, 'I’ve saved my key')
  await page.getByRole('button', { name: 'Create key', exact: true }).click()
  await expect(page.getByTestId('new-key')).toHaveCount(0)
})

for (const rotation of [false, true]) {
  test(`administration key ${rotation ? 'rotation' : 'creation'} needs review and explicit acknowledgement`, async ({
    page,
  }) => {
    await mockAccessiblePortal(page)
    const writes: Record<string, unknown>[] = []
    await page.route('**/portal/api/admin/agent-keys', async (route) => {
      if (route.request().method() !== 'POST') {
        await route.fallback()
        return
      }
      writes.push(route.request().postDataJSON())
      await route.fulfill({ json: { key: 'olr-admin-synthetic-once-only' } })
    })
    await page.goto('/portal/admin/agents')
    await page
      .getByRole('button', {
        name: rotation ? 'Rotate key' : 'Create administration key',
        exact: true,
      })
      .click()
    const dialog = page.getByRole('dialog')
    const name = dialog.getByLabel('Agent or purpose')
    await name.fill('Synthetic administration key')
    await name.press('Enter')
    await expect(dialog.getByRole('heading', { level: 2 })).toContainText(
      '2 of 4',
    )
    expect(writes).toHaveLength(0)
    const networks = dialog.getByLabel('Approved IP addresses or CIDRs')
    await networks.fill('192.0.2.10/32')
    // Also exercise implicit form submission without a submit button on this step.
    await dialog
      .locator('form')
      .evaluate((form) => (form as HTMLFormElement).requestSubmit())
    await expect(dialog.getByRole('heading', { level: 2 })).toContainText(
      '3 of 4',
    )
    expect(writes).toHaveLength(0)
    const days = dialog.getByLabel('Expires in days')
    await days.fill('7')
    await days.press('Enter')
    await expect(dialog.getByRole('heading', { level: 2 })).toContainText(
      '4 of 4',
    )
    expect(writes).toHaveLength(0)
    await dialog
      .getByRole('button', { name: 'Create key', exact: true })
      .click()
    await expect(dialog.getByRole('heading', { level: 2 })).toHaveText(
      'Save your administration key',
    )
    expect(writes).toHaveLength(1)
    expect(writes[0].replaces_key_id).toBe(rotation ? agentKey.id : undefined)
    await secretCannotBeDismissed(page, 'I saved the key')
  })
}

test('failed revoke stays visible in the confirmation dialog', async ({
  page,
}) => {
  await mockAccessiblePortal(page)
  await page.route('**/portal/api/admin/agent-keys/agent-fixture', (route) =>
    route.fulfill({
      status: 503,
      json: { error: { message: 'Synthetic storage unavailable' } },
    }),
  )
  await page.goto('/portal/admin/agents')
  await page.getByRole('button', { name: 'Revoke', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('button', { name: 'Revoke key', exact: true }).click()
  await expect(dialog.getByRole('alert')).toHaveText(
    'Synthetic storage unavailable',
  )
  await expect(dialog).toBeVisible()
})

for (const maximum of [30, 90, 120]) {
  test(`personal key choices respect the ${maximum}-day site maximum`, async ({
    page,
  }) => {
    await mockAccessiblePortal(page)
    await page.route('**/portal/api/keys', (route) =>
      route.fulfill({
        json: {
          keys: [],
          policy: {
            default_lifetime_days: Math.min(90, maximum),
            max_lifetime_days: maximum,
          },
        },
      }),
    )
    await page.goto('/portal/keys')
    await page.getByRole('button', { name: 'Create key', exact: true }).click()
    const select = page.getByLabel('Expires after')
    const options = await select.locator('option').evaluateAll((elements) =>
      elements.map((e) => ({
        value: (e as HTMLOptionElement).value,
        text: e.textContent,
      })),
    )
    expect(options[0].text).toBe(`Site default (${Math.min(90, maximum)} days)`)
    expect(
      options
        .filter((o) => o.value)
        .every((o) => Number(o.value) >= 1 && Number(o.value) <= maximum),
    ).toBe(true)
    expect(options.some((o) => Number(o.value) === maximum)).toBe(true)
  })
}
