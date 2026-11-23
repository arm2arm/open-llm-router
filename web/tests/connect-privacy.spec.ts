import { expect, test } from '@playwright/test'
import { mockAccessiblePortal } from './accessibility-fixture'

test('Connect shows the possible recipients for the selected model', async ({
  page,
}) => {
  await mockAccessiblePortal(page)
  const base = {
    base_url: 'https://router.example.org/v1',
    router_url: 'https://router.example.org',
  }
  const model = {
    id: 'chat',
    canonical_model: 'chat',
    protocols: ['openai'],
  }
  await page.route('**/portal/api/models', (route) =>
    route.fulfill({
      json: {
        ...base,
        privacy_notice_url: 'https://site.example.org/privacy',
        models: [
          {
            ...model,
            recipients: ['Home institute', 'Partner institute'],
            has_unlisted_recipients: true,
          },
          {
            ...model,
            id: 'local',
            canonical_model: 'local',
            recipients: ['Local lab'],
            has_unlisted_recipients: false,
          },
        ],
      },
    }),
  )
  await page.goto('/portal/connect')
  await expect(
    page.getByRole('link', { name: 'Site privacy notice' }),
  ).toHaveAttribute('href', 'https://site.example.org/privacy')
  await expect(
    page.getByText(/The router can send the full request/),
  ).toBeVisible()
  await expect(
    page.getByText('Home institute · No recipient privacy notice linked'),
  ).toBeVisible()
  await expect(
    page.getByText('Partner institute · No recipient privacy notice linked'),
  ).toBeVisible()
  await expect(
    page.getByText(/Recipient information is incomplete/),
  ).toBeVisible()
  await page.getByLabel('Model', { exact: true }).selectOption('local')
  await expect(page.getByText(/Local lab/)).toBeVisible()
  await expect(page.getByText(/Home institute/)).toHaveCount(0)
  await expect(page.getByTestId('snippet')).toContainText('local')

  await page.route('**/portal/api/models', (route) =>
    route.fulfill({ json: { ...base, models: [model] } }),
  )
  await page.reload()
  await expect(
    page.getByText(/Recipient information is incomplete/),
  ).toBeVisible()
  await expect(
    page.getByRole('link', { name: 'Site privacy notice' }),
  ).toHaveCount(0)
  await expect(page.getByTestId('snippet')).toContainText('chat')
})
