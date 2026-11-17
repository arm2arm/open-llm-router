import { test, expect } from '@playwright/test'

test('limited output has unknown tool counts in history, filters and detail', async ({
  page,
}) => {
  const summary = {
    input_messages: 1,
    output_messages: 1,
    tool_calls: null,
    observed_tool_calls: 0,
    tool_results: 0,
    tools: 1,
    cached_tokens: 7,
    reasoning_tokens: 9,
    truncated: true,
  }
  const row = {
    request_id: 'limited-stream',
    operation: 'chat/completions',
    trace_available: true,
    trace_summary: summary,
    time: new Date().toISOString(),
    model: 'test',
    protocol: 'openai',
    status: '200',
    input_tokens: 10,
    output_tokens: 3000,
    latency_ms: 1000,
    stream: true,
  }
  await page.route('**/portal/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname.replace(
      '/portal/api/',
      '',
    )
    let body: unknown = {}
    if (path === 'me')
      body = {
        user: { id: 'test', username: 'tester', groups: ['staff'] },
        limits: {},
        admin: false,
      }
    else if (path === 'requests') body = { requests: [row], limit: 200 }
    else if (path === 'trace-preferences')
      body = { available: true, enabled: false, retention_days: 7 }
    else if (path === 'requests/limited-stream')
      body = {
        request_id: 'limited-stream',
        expires_at: row.time,
        detail: {
          v: 4,
          operation: 'chat/completions',
          input: [],
          output: [],
          events: [],
          tools: [],
          parameters: {},
          result: {
            finish_reasons: [],
            usage: { 'completion_tokens_details.reasoning_tokens': 9 },
          },
          summary,
          output_truncated: true,
          truncated: true,
          warnings: ['Output capture stopped; tool count is unknown.'],
        },
      }
    await route.fulfill({ json: body })
  })
  await page.goto('/portal/requests?live=off')
  await expect(
    page.getByText('Unknown · capture limited', { exact: true }),
  ).toBeVisible()
  await expect(
    page.getByText('Some tool counts are unknown: capture limited'),
  ).toBeVisible()
  await page.getByLabel('Filter requests').selectOption('tools')
  await page
    .getByRole('button', {
      name: 'Inspect request limited-stream',
      exact: true,
    })
    .click()
  await expect(page.getByRole('dialog')).toContainText(
    'Finish reason unknown · capture limited',
  )
})
