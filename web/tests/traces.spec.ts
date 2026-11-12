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

test('live updates show running requests and can be paused', async ({
  page,
  context,
  baseURL,
}) => {
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  await login(page)
  const live = page.getByRole('button', { name: /^Live$/ })
  await expect(live).toHaveAttribute('aria-pressed', 'true')
  const key = await apiKey(context, baseURL!)
  // Slow enough to be observed while running: the mock streams 40 tokens at
  // 100 ms each; a real backend counts slowly enough as well.
  const pending = context.request.post('/v1/chat/completions', {
    headers: {
      Authorization: `Bearer ${key}`,
      'X-Mock-Tokens': '40',
      'X-Mock-Token-Delay': '100ms',
    },
    data: {
      model: 'chat-model',
      stream: true,
      max_tokens: 200,
      messages: [
        {
          role: 'user',
          content: 'Count from 1 to 60, one number per line.',
        },
      ],
    },
  })
  const running = page.getByRole('button', {
    name: /^Inspect running request/,
  })
  await expect(running).toBeVisible({ timeout: 10_000 })
  await expect(page.locator('.trace-row-running')).toContainText('elapsed')
  // Opening a running request waits, then shows the trace once it is stored.
  await running.click()
  await expect(page.getByText(/still running/)).toBeVisible()
  const id = (await pending).headers()['x-request-id']
  await expect(page.getByRole('heading', { name: 'Routing' })).toBeVisible({
    timeout: 20_000,
  })
  await expect(page).toHaveURL(new RegExp(`/portal/requests/${id}`))
  await page.keyboard.press('Escape')
  await expect(
    page.getByRole('button', { name: `Inspect request ${id}` }),
  ).toBeVisible()
  await expect(page.locator('.trace-row-running')).toHaveCount(0)

  // Paused: the choice is in the URL and no stream is open.
  await live.click()
  await expect(page).toHaveURL(/live=off/)
  const paused = page.getByRole('button', { name: /^Paused$/ })
  await expect(paused).toHaveAttribute('aria-pressed', 'false')
  await page.reload()
  await expect(paused).toBeVisible()
  await paused.click()
  await expect(page).not.toHaveURL(/live=off/)
  await expect(live).toBeVisible()
  expect(errors).toEqual([])
})

test('agent runs group calls and mark what each call added', async ({
  page,
  context,
  baseURL,
}) => {
  await login(page)
  const key = await apiKey(context, baseURL!)
  const session = `run-${Date.now()}`
  const history = [
    { role: 'system', content: `Grouping test ${session}` },
    { role: 'user', content: 'Weather in Berlin?' },
  ]
  const call = async (messages: unknown[]) => {
    const r = await context.request.post('/v1/chat/completions', {
      headers: { Authorization: `Bearer ${key}`, 'X-Session-Id': session },
      data: { model: 'chat-model', max_tokens: 8, messages },
    })
    expect(r.status()).toBe(200)
    return r.headers()['x-request-id']
  }
  const first = await call(history)
  const second = await call([
    ...history,
    {
      role: 'assistant',
      tool_calls: [
        {
          id: 'call_run',
          type: 'function',
          function: { name: 'get_weather', arguments: '{"city":"Berlin"}' },
        },
      ],
    },
    { role: 'tool', tool_call_id: 'call_run', content: '{"temp":9}' },
  ])
  const group = page.locator('.trace-group').filter({
    has: page.locator('.trace-run-call'),
  })
  const secondLink = page.getByRole('button', {
    name: `Inspect request ${second}`,
  })
  await expect(secondLink).toBeVisible({ timeout: 20_000 })
  await expect(
    page.getByRole('button', { name: `Inspect request ${first}` }),
  ).toBeVisible()
  await expect(group.first()).toContainText('2 calls')
  await secondLink.click()
  await expect(page.locator('.trace-run-nav')).toContainText(
    'Call 2 of 2 in this conversation',
  )
  await expect(page.locator('.trace-run-nav')).toContainText('1 tool result')
  await page.getByRole('tab', { name: /^Conversation/ }).click()
  await expect(page.locator('.trace-badge-new')).toHaveCount(1)
  await expect(page.locator('.trace-badge-echo')).toHaveCount(1)
  await page.getByRole('tab', { name: /^Overview/ }).click()
  await page.getByRole('button', { name: 'Previous call' }).click()
  await expect(page).toHaveURL(new RegExp(`/portal/requests/${first}`))
  await page.keyboard.press('Escape')
  // Flat list: no group headers, every call still listed.
  await page.getByRole('button', { name: 'Group runs' }).click()
  await expect(page).toHaveURL(/group=off/)
  await expect(page.locator('.trace-group')).toHaveCount(0)
  await expect(secondLink).toBeVisible()
})
