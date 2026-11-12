import { test, expect, type BrowserContext, type Page } from '@playwright/test'

// The request carries its own tool history, so the trace views are
// deterministic whichever backend serves chat-model.
async function login(page: Page) {
  await page.goto('/portal/requests')
  await page.getByRole('button', { name: 'Continue with Keycloak' }).click()
  await page.getByLabel('Username', { exact: true }).fill('staff')
  await page.getByLabel('Password', { exact: true }).fill('staff-dev-only')
  await page.getByRole('button', { name: 'Sign In', exact: true }).click()
  await page.getByRole('link', { name: 'My requests', exact: true }).click()
  await expect(
    page.getByRole('heading', { name: 'My requests', exact: true }),
  ).toBeVisible()
}

async function apiKey(context: BrowserContext, baseURL: string) {
  const created = await context.request.post('/portal/api/keys', {
    headers: { Origin: baseURL },
    data: { label: `trace-e2e-${Date.now()}`, expires_in_days: 1 },
  })
  expect(created.ok()).toBe(true)
  return JSON.stringify(await created.json()).match(/"(olr-[^"]+)"/)![1]
}

test('trace viewer: overview, tool pairing, raw request and deep links', async ({
  page,
  context,
  baseURL,
}) => {
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  await login(page)
  const key = await apiKey(context, baseURL!)
  const marker = `trace-e2e-${Date.now()}`
  const response = await context.request.post('/v1/chat/completions', {
    headers: { Authorization: `Bearer ${key}`, 'X-Session-Id': marker },
    data: {
      model: 'chat-model',
      stream: true,
      max_tokens: 16,
      tool_choice: 'none',
      messages: [
        { role: 'system', content: `You are a test. ${marker}` },
        { role: 'user', content: 'Weather in Berlin?' },
        {
          role: 'assistant',
          tool_calls: [
            {
              id: 'call_e2e',
              type: 'function',
              function: { name: 'get_weather', arguments: '{"city":"Berlin"}' },
            },
          ],
        },
        { role: 'tool', tool_call_id: 'call_e2e', content: '{"temp":12}' },
      ],
      tools: [
        {
          type: 'function',
          function: {
            name: 'get_weather',
            description: 'Current weather',
            parameters: {
              type: 'object',
              properties: { city: { type: 'string' } },
            },
          },
        },
      ],
    },
  })
  expect(response.status()).toBe(200)
  const id = response.headers()['x-request-id']
  await response.body()

  // The list shows the tool summary once the background capture is stored.
  const row = page
    .getByTestId('requests-table')
    .getByRole('row')
    .filter({
      has: page.getByRole('button', { name: `Inspect request ${id}` }),
    })
  await expect(row).toBeVisible({ timeout: 20_000 })
  await expect(row).toContainText('1 result')
  await page.getByRole('button', { name: `Inspect request ${id}` }).click()
  await expect(page).toHaveURL(new RegExp(`/portal/requests/${id}`))

  await expect(page.getByRole('heading', { name: 'Routing' })).toBeVisible()
  await expect(page.getByText('stream_options.include_usage')).toBeVisible()
  await expect(page.getByText(/^Attempt 1/)).toBeVisible()

  await page.getByRole('tab', { name: /^Tools/ }).click()
  const run = page.locator('.trace-run').filter({ hasText: 'call_e2e' })
  await expect(run).toContainText('get_weather')
  await expect(run).toContainText('Result returned')
  await expect(run).toContainText('temp')
  await page.getByText('Current weather').click()
  await expect(page.getByText('Parameters schema')).toBeVisible()

  await page.getByRole('tab', { name: /^Conversation/ }).click()
  await page.getByLabel('Search this trace').fill(marker)
  await expect(page.locator('mark').first()).toHaveText(marker)

  await page.getByRole('tab', { name: /^Raw/ }).click()
  await expect(
    page.getByText('Request body as received (sanitized)'),
  ).toBeVisible()
  await page.getByRole('button', { name: 'Forwarded upstream' }).click()
  await expect(page.locator('.json-tree-body')).toContainText('include_usage')

  // A deep link opens the same trace after a reload.
  await page.reload()
  await expect(page.getByRole('heading', { name: 'Routing' })).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page).toHaveURL(/\/portal\/requests$/)
  await page.setViewportSize({ width: 390, height: 844 })
  await page.getByRole('button', { name: `Inspect request ${id}` }).click()
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true)
  expect(errors).toEqual([])
})
