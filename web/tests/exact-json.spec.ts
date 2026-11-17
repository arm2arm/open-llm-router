import { readFile } from 'node:fs/promises'
import { test, expect } from '@playwright/test'
import { parseExactJSON, stringifyExactJSON } from '../src/lib/exact-json'

test('trace JSON preserves unsafe integers and strings through copy/export', () => {
  const source = String.raw`{"seed":1234567890123456789,"negative":-1234567890123456789,"safe":42,"fraction":0.5,"quoted":"1234567890123456789","escaped":"\"seed\":1234567890123456789","collision":"__olr_exact_number__0"}`
  const value = parseExactJSON(source) as Record<string, unknown>
  expect(value.seed).toBe(1234567890123456789n)
  expect(value.negative).toBe(-1234567890123456789n)
  expect(value.safe).toBe(42)
  expect(value.fraction).toBe(0.5)
  expect(value.quoted).toBe('1234567890123456789')
  expect(value.escaped).toBe('"seed":1234567890123456789')
  const exported = stringifyExactJSON(value, 2)
  expect(exported).toContain('"seed": 1234567890123456789')
  expect(exported).not.toContain('1234567890123456800')
  expect(parseExactJSON(exported)).toEqual(value)
  expect(parseExactJSON('null')).toBeNull()
  expect(() => parseExactJSON('{"seed":01}')).toThrow()
})

test('trace parameters, raw tree, clipboard and download show the exact seed', async ({
  page,
}) => {
  const now = new Date().toISOString()
  const row = {
    request_id: 'exact-json',
    operation: 'chat/completions',
    trace_available: true,
    time: now,
    model: 'test',
    protocol: 'openai',
    status: '200',
    input_tokens: 3,
    output_tokens: 7,
    latency_ms: 100,
    ttfb_ms: 10,
    stream: false,
    key_prefix: null,
    forwarded: false,
  }
  const detail = {
    v: 3,
    operation: 'chat/completions',
    input: [],
    output: [],
    events: [],
    tools: [],
    parameters: { seed: 'EXACT_INTEGER' },
    request: { model: 'test', seed: 'EXACT_INTEGER' },
    response: { extension: 'EXACT_INTEGER' },
    frames: [],
    truncated: false,
    warnings: [],
  }
  await page.route('**/portal/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname.replace(
      '/portal/api/',
      '',
    )
    let body: unknown
    if (path === 'me')
      body = {
        user: {
          id: 'test',
          username: 'tester',
          email: '',
          groups: ['staff'],
          protected_admin: false,
        },
        limits: {},
        admin: false,
      }
    else if (path === 'requests') body = { requests: [row], limit: 200 }
    else if (path === 'trace-preferences')
      body = { available: true, enabled: false, retention_days: 7 }
    else if (path === 'requests/exact-json')
      body = { request_id: 'exact-json', expires_at: now, detail }
    else body = {}
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify(body).replaceAll(
        '"EXACT_INTEGER"',
        '1234567890123456789',
      ),
    })
  })
  await page.goto('/portal/requests/exact-json?live=off')
  await expect(
    page.getByText('Request parameters', { exact: true }),
  ).toBeVisible()
  await expect(page.locator('.json-tree-body').last()).toContainText(
    '1234567890123456789',
  )
  await page.getByRole('tab', { name: /^Raw/ }).click()
  await expect(page.locator('.json-tree-body')).toContainText(
    '1234567890123456789',
  )
  await expect(page.locator('.json-tree-body')).not.toContainText(
    '1234567890123456800',
  )
  await page.getByRole('button', { name: 'Copy JSON', exact: true }).click()
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toContain('"seed": 1234567890123456789')
  const download = page.waitForEvent('download')
  await page.getByRole('button', { name: /^Download request-/ }).click()
  const artifact = await download
  const text = await readFile((await artifact.path())!, 'utf8')
  expect(text).toContain('"seed": 1234567890123456789')
  expect(text).not.toContain('1234567890123456800')
})
