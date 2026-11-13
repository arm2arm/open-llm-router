import { expect, type APIRequestContext, type Page } from '@playwright/test'
import { readFileSync } from 'node:fs'

// This journey runs inside the local-auth suite's disposable router/database.
export async function checkAgentAccess(
  page: Page,
  request: APIRequestContext,
  base: string,
  password: string,
) {
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  await page.goto(`${base}/portal/sign-in/local`)
  await page.getByLabel('Local username', { exact: true }).fill('admin')
  await page.getByLabel('Local password', { exact: true }).fill(password)
  await page.getByRole('button', { name: 'Sign in with local account' }).click()
  await page.getByRole('link', { name: 'Agent access', exact: true }).click()
  await expect(
    page.getByRole('heading', { name: 'Connection address' }),
  ).toBeVisible()
  const label = `Browser agent ${Date.now()}`
  await page.getByRole('button', { name: 'Create administration key' }).click()
  await page.getByLabel('Agent or purpose').fill(label)
  await page.getByRole('button', { name: 'Continue', exact: true }).click()
  await page.getByRole('button', { name: 'Add my detected address' }).click()
  await page.getByRole('button', { name: 'Continue', exact: true }).click()
  await page.getByRole('button', { name: 'Continue', exact: true }).click()
  await expect(
    page.getByRole('heading', { name: 'Review access' }),
  ).toBeVisible()
  await page
    .getByRole('dialog')
    .getByRole('button', { name: 'Create key', exact: true })
    .click()
  await expect(
    page.getByRole('heading', { name: 'Save your administration key' }),
  ).toBeVisible()
  const secret = await page
    .locator('[aria-label="New administration key"]')
    .innerText()
  expect(/^olr-admin-[A-Za-z0-9_-]{43}$/.test(secret)).toBe(true)
  const auth = { Authorization: `Bearer ${secret}` }
  expect(
    (
      await request.get(`${base}/api/admin/v1/identity`, { headers: auth })
    ).status(),
  ).toBe(200)
  expect(
    (
      await request.post(`${base}/api/admin/v1/read/list_admin_keys`, {
        headers: auth,
        data: {},
      })
    ).status(),
  ).toBe(403)
  await page.getByRole('button', { name: 'I saved the key' }).click()
  expect((await page.locator('body').innerText()).includes(secret)).toBe(false)
  for (const name of [
    'Download Hermes configuration',
    'Download generic MCP example',
  ]) {
    const downloading = page.waitForEvent('download')
    await page.getByRole('button', { name, exact: true }).click()
    const download = await downloading
    const file = await download.path()
    expect(file).toBeTruthy()
    const setup = readFileSync(file!, 'utf8')
    expect(setup.includes('${OLR_ADMIN_API_KEY}')).toBe(true)
    expect(setup.includes(secret)).toBe(false)
    expect(setup.includes(`${base}/api/admin/v1/mcp`)).toBe(true)
  }
  expect(
    await page.evaluate(
      (value) =>
        [...Object.values(localStorage), ...Object.values(sessionStorage)].some(
          (item) => item.includes(value),
        ),
      secret,
    ),
  ).toBe(false)
  const card = page
    .getByRole('article')
    .filter({ has: page.getByRole('heading', { name: label, exact: true }) })
  await card
    .getByRole('button', { name: 'Restrict access', exact: true })
    .click()
  await page
    .getByRole('dialog')
    .getByLabel('usage:read', { exact: true })
    .uncheck()
  await expect(
    page.getByRole('dialog').getByLabel('Expires at'),
  ).toHaveAttribute('type', 'datetime-local')
  await page
    .getByRole('dialog')
    .getByRole('button', { name: 'Restrict access', exact: true })
    .click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(
    (
      await request.post(`${base}/api/admin/v1/read/get_usage`, {
        headers: auth,
        data: {},
      })
    ).status(),
  ).toBe(403)
  await card.getByRole('button', { name: 'Rotate key', exact: true }).click()
  await expect(page.getByLabel('Agent or purpose')).toHaveValue(
    `${label} replacement`,
  )
  for (let step = 0; step < 3; step++)
    await page.getByRole('button', { name: 'Continue', exact: true }).click()
  await page
    .getByRole('dialog')
    .getByRole('button', { name: 'Create key', exact: true })
    .click()
  const replacement = await page
    .locator('[aria-label="New administration key"]')
    .innerText()
  expect(replacement).not.toBe(secret)
  const replacementAuth = { Authorization: `Bearer ${replacement}` }
  expect(
    (
      await request.get(`${base}/api/admin/v1/identity`, {
        headers: replacementAuth,
      })
    ).status(),
  ).toBe(200)
  await page.getByRole('button', { name: 'I saved the key' }).click()
  await card.getByRole('button', { name: 'Revoke', exact: true }).click()
  await page.getByRole('button', { name: 'Revoke key', exact: true }).click()
  await expect(card.getByText('Revoked', { exact: true })).toBeVisible()
  expect(
    (
      await request.get(`${base}/api/admin/v1/identity`, { headers: auth })
    ).status(),
  ).toBe(401)
  expect(
    (
      await request.get(`${base}/api/admin/v1/identity`, {
        headers: replacementAuth,
      })
    ).status(),
  ).toBe(200)
  const replacementCard = page.getByRole('article').filter({
    has: page.getByRole('heading', {
      name: `${label} replacement`,
      exact: true,
    }),
  })
  await replacementCard
    .getByRole('button', { name: 'Revoke', exact: true })
    .click()
  await page.getByRole('button', { name: 'Revoke key', exact: true }).click()
  await expect(
    replacementCard.getByText('Revoked', { exact: true }),
  ).toBeVisible()
  expect(errors).toEqual([])
}
