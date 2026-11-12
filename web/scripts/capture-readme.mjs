// Real portal screenshots with synthetic data in an isolated database/container.
// Requires the development Compose stack and its current router image.
import { chromium, expect } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:net'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import process from 'node:process'
import { fileURLToPath } from 'node:url'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..')
const project = process.env.COMPOSE_PROJECT_NAME ?? 'olr'
const compose = ['compose', '--env-file', '/dev/null', '-p', project]
const database = `readme_demo_${process.pid}`
const container = `${database}-router`
const directory = mkdtempSync(join(tmpdir(), 'olr-readme-'))
const output = join(root, 'docs/images')
const observabilityOnly = process.argv.includes('--observability-only')
const initial = 'readme-demo-initial-password'
const password = 'readme-demo-replacement-password'
const docker = (args) =>
  execFileSync('docker', args, {
    cwd: root,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  }).trim()
const sql = (db, query) =>
  docker([
    ...compose,
    'exec',
    '-T',
    'postgres',
    'psql',
    '-U',
    'olr',
    '-d',
    db,
    '-v',
    'ON_ERROR_STOP=1',
    '-At',
    '-c',
    query,
  ])
const port = await new Promise((resolve, reject) => {
  const listener = createServer()
  listener.on('error', reject)
  listener.listen(0, '127.0.0.1', () => {
    const { port } = listener.address()
    listener.close(() => resolve(port))
  })
})
const base = `http://127.0.0.1:${port}`
let browser
let created = false
try {
  const image = docker([
    'inspect',
    '--format',
    '{{.Image}}',
    docker([...compose, 'ps', '-q', 'router']),
  ])
  sql('olr', `CREATE DATABASE ${database}`)
  created = true
  const config = join(directory, 'config.yaml')
  writeFileSync(
    config,
    `oidc:
  issuer: http://localhost:8081/realms/olr-dev
  portal_client_id: olr-portal
models:
  chat-model:
    upstream_model: chat-model
    protocols: [openai, anthropic]
    timeout_s: 900
    idle_timeout_s: 120
    queue_timeout_s: 120
    session_affinity: true
    pool:
      - {url: http://mock-llm-a:8000, weight: 2, max_inflight: 8}
      - {url: http://mock-llm-b:8000, weight: 1, max_inflight: 4}
aliases:
  claude-sonnet-4-5: chat-model
groups:
  staff: {models: ['*'], tokens_per_day: 2000000, tokens_per_month: 20000000, rpm: 60, tpm: 100000, max_concurrent: 4, max_concurrent_batch: 8}
  students: {models: [chat-model], tokens_per_day: 300000, rpm: 20, tpm: 20000, max_concurrent: 2}
  admins: {models: ['*'], admin: true}
`,
  )
  docker([
    'run',
    '-d',
    '--name',
    container,
    '--network',
    `${project}_default`,
    '-p',
    `127.0.0.1:${port}:8080`,
    '-e',
    `OLR_DATABASE_URL=postgres://olr:olr-dev-only@postgres:5432/${database}?sslmode=disable`,
    '-e',
    `PORTAL_BASE_URL=${base}`,
    '-e',
    'PORTAL_COOKIE_SECURE=false',
    '-e',
    `OLR_BOOTSTRAP_ADMIN_PASSWORD=${initial}`,
    '-e',
    'OLR_API_KEY_ENCRYPTION_KEY=BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc=',
    '-e',
    'OIDC_PORTAL_CLIENT_SECRET=olr-portal-dev-only',
    '-e',
    'OIDC_BACKCHANNEL_URL=http://keycloak:8080/realms/olr-dev',
    '-v',
    `${config}:/etc/olr/config.yaml:ro`,
    image,
  ])
  browser = await chromium.launch()
  const context = await browser.newContext({
    viewport: { width: 1440, height: 1100 },
    deviceScaleFactor: 1,
    locale: 'en-US',
    timezoneId: 'UTC',
    colorScheme: 'light',
    reducedMotion: 'reduce',
  })
  const page = await context.newPage()
  const errors = []
  page.on('pageerror', (error) => errors.push(error.message))
  await expect
    .poll(
      async () => {
        try {
          return (await context.request.get(`${base}/portal/api/auth`)).status()
        } catch {
          return 0
        }
      },
      { timeout: 30_000 },
    )
    .toBe(200)
  const post = async (path, data, client = context) => {
    const response = await client.request.post(`${base}${path}`, {
      headers: { Origin: base },
      data,
    })
    if (!response.ok())
      throw new Error(`Demo operation ${path} returned ${response.status()}`)
    return response
  }
  await post('/portal/login/local', { username: 'admin', password: initial })
  await post('/portal/password', {
    current_password: initial,
    new_password: password,
  })
  await post('/portal/login/local', { username: 'admin', password })
  const response = await post('/portal/api/admin/users', {
    username: 'alex-demo',
    email: 'alex@example.org',
    groups: ['staff'],
    password: initial,
  })
  const { id } = await response.json()
  const person = await browser.newContext()
  await post(
    '/portal/login/local',
    { username: 'alex-demo', password: initial },
    person,
  )
  await post(
    '/portal/password',
    { current_password: initial, new_password: password },
    person,
  )
  await person.close()
  // All following SQL is confined to this disposable demo database.
  sql(
    database,
    `INSERT INTO users(issuer,sub,username,email,groups) VALUES
    ('https://id.example.org/realms/demo','morgan','morgan-demo','morgan@example.org','{staff}'),
    ('https://id.example.org/realms/demo','sam','sam-demo','sam@example.org','{students}');
    INSERT INTO usage_daily(day,user_id,model,requests,input_tokens,output_tokens)
    SELECT CURRENT_DATE-n,u.id,'chat-model',18+(n*7%65),28000+(n*7919%97000),6000+(n*2713%17000)
    FROM users u CROSS JOIN generate_series(0,29) n WHERE u.username IN ('alex-demo','morgan-demo','sam-demo');
    INSERT INTO usage(ts,user_id,model,replica,protocol,status,input_tokens,output_tokens,latency_ms,ttfb_ms,is_stream)
    SELECT clock_timestamp()-n*interval '23 minutes','${id}','chat-model',
      CASE WHEN n%3=0 THEN 'http://mock-llm-b:8000' ELSE 'http://mock-llm-a:8000' END,
      CASE WHEN n%4=0 THEN 'anthropic' ELSE 'openai' END,'200',2100+n*130,480+n*17,4200+n*50,180+n*12,true
    FROM generate_series(0,12) n;`,
  )
  // Issue only disposable keys; stdout is captured and never put in an image.
  let demoKey
  for (const label of ['Hermes workstation', 'Research notebook']) {
    const key = docker([
      'exec',
      container,
      'olr',
      'admin',
      'key',
      'create',
      '-user',
      id,
      '-label',
      label,
    ])
    demoKey ??= key
  }
  mkdirSync(output, { recursive: true })
  const shot = async (name) => {
    // eslint-disable-next-line no-undef -- this callback runs inside Chromium.
    await page.evaluate(() => document.fonts.ready)
    await expect(page.getByRole('alert')).toHaveCount(0)
    await page.screenshot({
      path: join(output, `${name}.png`),
      animations: 'disabled',
    })
    process.stdout.write(`Captured ${name}.png\n`)
  }
  if (!observabilityOnly) {
    await page.goto(`${base}/portal/admin`)
    await expect(
      page.getByRole('region', { name: 'Replica health' }),
    ).toContainText('healthy')
    await expect(page.getByTestId('users-table')).toContainText('alex-demo')
    const health = await page
      .getByRole('region', { name: 'Replica health' })
      .boundingBox()
    await page.setViewportSize({
      width: 1440,
      height: Math.ceil(health.y + health.height + 12),
    })
    await shot('admin-overview')
    await page.setViewportSize({ width: 1440, height: 1440 })
    await page
      .getByRole('button', {
        name: 'Edit endpoints for chat-model',
        exact: true,
      })
      .click()
    // Show a complete one-replica draft, including test/save controls. Never apply it.
    await page
      .getByRole('button', { name: 'Remove replica 2', exact: true })
      .click()
    await expect(
      page.getByLabel('Detected upstream models', { exact: true }),
    ).toBeVisible()
    await shot('endpoint-editor')
    await page.getByRole('button', { name: 'Close', exact: true }).click()
  }
  await page.setViewportSize({ width: 1440, height: 1100 })
  await post('/portal/logout', {})
  await post('/portal/login/local', { username: 'alex-demo', password })
  if (!observabilityOnly) {
    await page.goto(`${base}/portal/usage`)
    await expect(
      page.getByRole('img', { name: /Tokens per day, peak/ }),
    ).toBeVisible()
    await expect(page.getByTestId('usage-table')).toContainText('chat-model')
    const chart = await page
      .getByRole('region', { name: 'Tokens per day', exact: true })
      .boundingBox()
    await page.setViewportSize({
      width: 1440,
      height: Math.ceil(chart.y + chart.height + 12),
    })
    await shot('portal-usage')
  }

  // These are genuine native API calls through the disposable router. Capture
  // only the resulting portal views, never the key issuance dialog or headers.
  const infer = async (path, data, expectedStatus = 200) => {
    const response = await context.request.post(`${base}${path}`, {
      headers: {
        Authorization: `Bearer ${demoKey}`,
        'X-Mock-Token-Delay': '60ms',
      },
      data,
    })
    expect(response.status()).toBe(expectedStatus)
    return response.headers()['x-request-id']
  }
  const traceIDs = []
  for (const stream of [false, true])
    traceIDs.push(
      await infer('/v1/chat/completions', {
        model: 'chat-model',
        messages: [
          {
            role: 'user',
            content: 'Summarize the observing plan for tonight.',
          },
        ],
        max_tokens: 16,
        stream,
      }),
    )
  traceIDs.push(
    await infer('/v1/embeddings', {
      model: 'chat-model',
      input: 'Galaxy spectra and observing conditions',
    }),
  )
  traceIDs.push(
    await infer(
      '/v1/chat/completions',
      {
        model: 'unavailable-demo-model',
        messages: [
          {
            role: 'user',
            content: 'Demonstrate a routing error in the request history.',
          },
        ],
      },
      404,
    ),
  )
  const toolTrace = await infer('/v1/messages', {
    model: 'chat-model',
    max_tokens: 64,
    stream: true,
    thinking: { type: 'enabled', budget_tokens: 32 },
    messages: [
      {
        role: 'user',
        content: 'Check the weather in Berlin for an evening observation.',
      },
      {
        role: 'assistant',
        content: [
          {
            type: 'tool_use',
            id: 'weather-berlin',
            name: 'get_weather',
            input: { city: 'Berlin', units: 'celsius' },
          },
        ],
      },
      {
        role: 'user',
        content: [
          {
            type: 'tool_result',
            tool_use_id: 'weather-berlin',
            content:
              '{"temperature":18,"conditions":"clear","rain_probability":0.1}',
          },
        ],
      },
      {
        role: 'user',
        content: 'Check the forecast too before recommending what to pack.',
      },
    ],
    tools: [
      {
        name: 'get_weather',
        description: 'Read weather conditions for a city',
        input_schema: {
          type: 'object',
          properties: { city: { type: 'string' } },
        },
      },
    ],
  })
  traceIDs.push(toolTrace)
  await expect
    .poll(async () => {
      const response = await context.request.get(`${base}/portal/api/requests`)
      const { requests } = await response.json()
      return traceIDs.every((id) =>
        requests.some((r) => r.request_id === id && r.trace_available),
      )
    })
    .toBe(true)
  await page.setViewportSize({ width: 1440, height: 1150 })
  await page.goto(`${base}/portal/requests`)
  await expect(
    page.getByRole('switch', { name: 'Capture enabled' }),
  ).toBeChecked()
  await expect(page.getByLabel('Trace retention days')).toHaveValue('7')
  await expect(
    page.getByRole('button', {
      name: `Inspect request ${toolTrace}`,
      exact: true,
    }),
  ).toBeVisible()
  await shot('portal-requests')
  await page
    .getByRole('button', { name: `Inspect request ${toolTrace}`, exact: true })
    .click()
  const dialog = page.getByRole('dialog', { name: 'Request trace' })
  await expect(
    dialog.getByText('Tool call · get_weather', { exact: true }),
  ).toHaveCount(2)
  await dialog.locator('.trace-reasoning summary').click()
  await expect(dialog).toContainText('Considering the request.')
  await expect(dialog).toContainText('rain_probability')
  await shot('request-trace-conversation')
  await dialog.getByRole('tab', { name: /^Stream events/ }).click()
  await expect(dialog.locator('.trace-timeline')).toContainText('get_weather')
  await expect(dialog.locator('.trace-timeline')).toContainText('Considering')
  await shot('request-trace-events')
  await dialog.getByRole('tab', { name: 'Details', exact: true }).click()
  await expect(dialog).toContainText(toolTrace)
  await expect(dialog).toContainText('max_tokens')
  await shot('request-trace-details')
  await dialog.getByRole('button', { name: 'Close', exact: true }).click()

  if (!observabilityOnly) {
    await post('/portal/logout', {})
    await expect
      .poll(
        async () =>
          (await (await context.request.get(`${base}/portal/api/auth`)).json())
            .oidc.available,
        { timeout: 20_000 },
      )
      .toBe(true)
    await page.setViewportSize({ width: 1440, height: 900 })
    await page.goto(`${base}/portal/sign-in`)
    await expect(
      page.getByRole('button', { name: 'Continue with Keycloak' }),
    ).toBeEnabled()
    await shot('sign-in')
  }
  expect(errors).toEqual([])
} finally {
  await browser?.close()
  try {
    docker(['rm', '-f', container])
  } finally {
    try {
      if (created) sql('olr', `DROP DATABASE ${database} WITH (FORCE)`)
    } finally {
      rmSync(directory, { recursive: true, force: true })
    }
  }
}
