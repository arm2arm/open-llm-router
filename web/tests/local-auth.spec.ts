import { test, expect, type Page } from '@playwright/test'
import { checkAgentAccess } from './agent-access'
import { execFileSync, spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createServer } from 'node:net'

// Public synthetic credentials, isolated databases and disposable containers.
// Never reset the protected administrator in the developer's existing database.
const initial = 'browser-initial-password-only'
const changed = 'browser-replacement-password-only'
const rotated = 'browser-rotated-password-only'
const project = process.env.COMPOSE_PROJECT_NAME ?? 'olr'
const compose = ['compose', '--env-file', '/dev/null', '-p', project]
const externalContainers: string[] = []
const fixtures: {
  database: string
  container: string
  directory: string
  base: string
}[] = []
const docker = (args: string[], env = process.env) =>
  execFileSync('docker', args, {
    cwd: '..',
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
    env,
  }).trim()
const dockerLogs = (container: string) => {
  const result = spawnSync('docker', ['logs', container], { encoding: 'utf8' })
  return result.stdout + result.stderr
}
const sql = (database: string, command: string) =>
  docker([
    ...compose,
    'exec',
    '-T',
    'postgres',
    'psql',
    '-U',
    'olr',
    '-d',
    database,
    '-v',
    'ON_ERROR_STOP=1',
    '-At',
    '-c',
    command,
  ])

async function startFixture(
  outage: boolean,
  suffix = outage ? 'outage' : 'local',
) {
  const port = await new Promise<number>((resolve, reject) => {
    const listener = createServer()
    listener.on('error', reject)
    listener.listen(0, '127.0.0.1', () => {
      const address = listener.address()
      if (!address || typeof address === 'string')
        return reject(new Error('No test port'))
      listener.close(() => resolve(address.port))
    })
  })
  const database = `portal_browser_${process.pid}_${suffix}`
  const fixture = {
    database,
    container: `${database}-router`,
    directory: mkdtempSync(join(tmpdir(), 'olr-portal-')),
    base: `http://127.0.0.1:${port}`,
  }
  fixtures.push(fixture)
  sql('olr', `CREATE DATABASE ${database}`)
  // The authentication fixture uses the bundled development Keycloak through
  // its internal back-channel URL, with its public development secrets.
  const keycloak = suffix === 'authentication' || suffix === 'activation'
  const keycloakPort = process.env.KEYCLOAK_PORT ?? '8081'
  const oidc = outage
    ? 'oidc:\n  issuer: http://127.0.0.1:1/realms/unavailable\n  portal_client_id: browser-test\n'
    : suffix === 'authentication'
      ? `oidc:\n  issuer: http://localhost:${keycloakPort}/realms/olr-dev\n  portal_client_id: olr-portal\n  audience: olr-api\n`
      : ''
  const managedSecrets = suffix === 'external' || suffix === 'restore'
  const keycloakEnv = keycloak
    ? [
        '-e',
        'OIDC_BACKCHANNEL_URL=http://keycloak:8080/realms/olr-dev',
        '-e',
        'OIDC_SYNC_CLIENT_SECRET=olr-sync-dev-only',
        '-e',
        'OLR_SETTINGS_ENCRYPTION_KEY=CAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAg=',
      ]
    : []
  const config = join(fixture.directory, 'config.yaml')
  writeFileSync(
    config,
    `${suffix === 'agents' ? 'admin_api: {enabled: true}\n' : ''}${suffix === 'tracing' ? 'content_tracing: {enabled: true}\n' : ''}${oidc}models:\n  test:\n    upstream_model: chat-model\n    protocols: ${suffix === 'tracing' ? '[openai, anthropic]' : '[openai]'}\n    timeout_s: 5\n    pool:\n      - {url: http://mock-llm-a:8000, weight: 1, max_inflight: 4}\n  limited:\n    upstream_model: chat-model\n    protocols: [openai]\n    timeout_s: 5\n    pool:\n      - {url: http://mock-llm-b:8000, weight: 1, max_inflight: 4}\ngroups:\n  staff: {models: [test], rpm: 7, max_concurrent: 2}\n  students: {models: [limited], rpm: 3}\n  admins: {models: ['*'], admin: true}\n`,
  )
  // Test a new image without restarting the developer's running router.
  const image =
    process.env.OLR_TEST_ROUTER_IMAGE ??
    docker([
      'inspect',
      '--format',
      '{{.Image}}',
      docker([...compose, 'ps', '-q', 'router']),
    ])
  docker(
    [
      'run',
      '-d',
      '--name',
      fixture.container,
      '--network',
      `${project}_default`,
      '-p',
      `127.0.0.1:${port}:8080`,
      '-e',
      `OLR_DATABASE_URL=postgres://olr:olr-dev-only@postgres:5432/${database}?sslmode=disable`,
      '-e',
      `PORTAL_BASE_URL=${fixture.base}`,
      '-e',
      'PORTAL_COOKIE_SECURE=false',
      '-e',
      'OLR_BOOTSTRAP_ADMIN_PASSWORD',
      ...(managedSecrets
        ? [
            '-e',
            'OLR_SETTINGS_ENCRYPTION_KEY=CAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAg=',
          ]
        : [
            '-e',
            `OIDC_PORTAL_CLIENT_SECRET=${keycloak ? 'olr-portal-dev-only' : 'browser-test-only'}`,
          ]),
      ...keycloakEnv,
      '-e',
      'OLR_API_KEY_ENCRYPTION_KEY=BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc=',
      '-v',
      `${config}:/etc/olr/config.yaml:ro`,
      image,
    ],
    { ...process.env, OLR_BOOTSTRAP_ADMIN_PASSWORD: initial },
  )
  await expect
    .poll(
      async () => {
        try {
          return (await fetch(`${fixture.base}/portal/api/auth`)).status
        } catch {
          return 0
        }
      },
      { timeout: 30_000 },
    )
    .toBe(200)
    .catch(() => {
      throw new Error(
        `Disposable router did not start: ${dockerLogs(fixture.container)}`,
      )
    })
  // A pre-existing ordinary identity exercises login authorization independently
  // of user creation. Copy only this disposable fixture's password hash.
  sql(
    database,
    `WITH added AS (INSERT INTO users(issuer,sub,username) VALUES('local','reader','reader') RETURNING id) INSERT INTO local_credentials(user_id,version,password_hash,must_change_password) SELECT added.id,c.version,c.password_hash,false FROM added CROSS JOIN local_credentials c JOIN users u ON u.id=c.user_id WHERE u.protected_admin`,
  )
  if (suffix === 'tracing')
    sql(database, "UPDATE users SET groups='{admins}' WHERE protected_admin")
  return fixture.base
}

let local: string
let outage: string
let administration: string
let endpointAdministration: string
let tracing: string
let authentication: string
let activation: string
let routing: string
test.beforeAll(async () => {
  test.setTimeout(60_000)
  local = await startFixture(false)
  outage = await startFixture(true)
  administration = await startFixture(false, 'administration')
  endpointAdministration = await startFixture(false, 'endpoints')
  tracing = await startFixture(false, 'tracing')
  authentication = await startFixture(false, 'authentication')
  activation = await startFixture(false, 'activation')
  routing = await startFixture(false, 'routing')
})
test.afterAll(() => {
  for (const container of externalContainers) docker(['rm', '-f', container])
  for (const fixture of fixtures.reverse()) {
    try {
      docker(['rm', '-f', fixture.container])
    } finally {
      try {
        sql('olr', `DROP DATABASE IF EXISTS ${fixture.database} WITH (FORCE)`)
      } finally {
        rmSync(fixture.directory, { recursive: true, force: true })
      }
    }
  }
})

async function signIn(page: Page, password: string, username = 'admin') {
  await page.getByLabel('Local username', { exact: true }).fill(username)
  await page.getByLabel('Local password', { exact: true }).fill(password)
  await page
    .getByRole('button', { name: 'Sign in with local account', exact: true })
    .click()
}
async function passwordFields(
  page: Page,
  current: string,
  next: string,
  confirmation = next,
) {
  await page.getByLabel('Current password', { exact: true }).fill(current)
  await page.getByLabel('New password', { exact: true }).fill(next)
  await page
    .getByLabel('Confirm new password', { exact: true })
    .fill(confirmation)
  await page
    .getByRole('button', { name: 'Change password', exact: true })
    .click()
}

test('personal traces, reasoning, tools, retention, opt-out and erasure', async ({
  page,
  context,
}) => {
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  await page.goto(`${tracing}/portal/sign-in/local`)
  await signIn(page, initial)
  await passwordFields(page, initial, changed)
  await signIn(page, changed)
  await expect(
    page.getByRole('heading', { name: 'API keys', exact: true }),
  ).toBeVisible()
  const issued = await context.request.post(`${tracing}/portal/api/keys`, {
    headers: { Origin: tracing },
    data: { label: 'trace browser test' },
  })
  expect(issued.status()).toBe(201)
  const key = (await issued.json()).key as string
  await page.goto(`${tracing}/portal/requests`)
  const toggle = page.getByRole('switch')
  await expect(toggle).not.toBeChecked()
  await toggle.click()
  await expect(page.getByLabel('Trace retention days')).toHaveValue('7')
  await page.getByLabel('Trace retention days').fill('14')
  await page.getByRole('button', { name: 'Save settings' }).click()
  await expect(
    page.getByRole('button', { name: 'Save settings' }),
  ).toBeDisabled()
  await page.reload()
  await expect(page.getByLabel('Trace retention days')).toHaveValue('14')
  const send = async (anthropic = false) => {
    const response = await context.request.post(
      `${tracing}/v1/${anthropic ? 'messages' : 'chat/completions'}`,
      {
        headers: {
          Authorization: `Bearer ${key}`,
          'X-Mock-Thinking': 'true',
          'X-Private-Header': 'private-header-marker',
        },
        data: anthropic
          ? {
              model: 'test',
              max_tokens: 7,
              stream: true,
              messages: [
                {
                  role: 'user',
                  content: 'Reason about this synthetic request',
                },
              ],
            }
          : {
              model: 'test',
              max_tokens: 7,
              stream: true,
              messages: [
                {
                  role: 'user',
                  content: [
                    {
                      type: 'text',
                      text: '<img src=x onerror=alert(1)> trace input',
                    },
                    {
                      type: 'image_url',
                      image_url: {
                        url: 'https://example.test/private-image?token=private-image-marker',
                      },
                    },
                  ],
                },
                {
                  role: 'tool',
                  tool_call_id: 'previous-tool',
                  content: 'Earlier result',
                },
                { role: 'user', content: 'Look up weather' },
              ],
              tools: [
                {
                  type: 'function',
                  function: { name: 'weather', parameters: { type: 'object' } },
                },
              ],
            },
      },
    )
    expect(response.status()).toBe(200)
    expect(response.headers()['content-type']).toContain('text/event-stream')
    return response.headers()['x-request-id']
  }
  const toolID = await send()
  const thinkingID = await send(true)
  const records = async () =>
    (await (await context.request.get(`${tracing}/portal/api/requests`)).json())
      .requests as { request_id: string; trace_available: boolean }[]
  await expect
    .poll(async () => (await records()).filter((r) => r.trace_available).length)
    .toBe(2)
  await page.getByRole('button', { name: 'Refresh', exact: true }).click()
  await page
    .getByRole('button', { name: `Inspect request ${toolID}`, exact: true })
    .click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByRole('heading', { name: 'Routing' })).toBeVisible()
  await expect(dialog).toContainText(toolID)
  await dialog.getByRole('tab', { name: /^Conversation/ }).click()
  await expect(
    dialog.getByText('Tool call · weather', { exact: true }),
  ).toBeVisible()
  await expect(
    dialog.getByText('Earlier result', { exact: true }),
  ).toBeVisible()
  await expect(
    dialog.getByText('Image payload omitted', { exact: true }),
  ).toBeVisible()
  expect(await dialog.locator('img').count()).toBe(0)
  expect((await dialog.innerText()).includes('private-image-marker')).toBe(
    false,
  )
  await dialog.getByRole('tab', { name: /^Timeline/ }).click()
  await expect(dialog.locator('.trace-segments')).toContainText('tool call')
  await dialog.getByRole('button', { name: 'All events', exact: true }).click()
  await dialog.getByLabel('Filter stream events').selectOption('tool_call')
  await expect(dialog.locator('.trace-timeline')).toContainText('weather')
  await dialog.getByRole('tab', { name: /^Raw/ }).click()
  await expect(
    dialog.getByText('Request body as received (sanitized)'),
  ).toBeVisible()
  expect((await dialog.innerText()).includes('private-image-marker')).toBe(
    false,
  )
  const trace = await (
    await context.request.get(`${tracing}/portal/api/requests/${toolID}`)
  ).json()
  expect(Date.parse(trace.expires_at) - Date.now()).toBeGreaterThan(
    13 * 86_400_000,
  )
  expect(JSON.stringify(trace).includes(key)).toBe(false)
  expect(JSON.stringify(trace.detail.request)).toContain(
    'image payload omitted',
  )
  expect(JSON.stringify(trace).includes('private-header-marker')).toBe(false)
  await dialog.getByRole('button', { name: 'Close', exact: true }).click()
  await page
    .getByRole('button', { name: `Inspect request ${thinkingID}`, exact: true })
    .click()
  await dialog.getByRole('tab', { name: /^Conversation/ }).click()
  await dialog.locator('summary', { hasText: 'Reasoning' }).click()
  await expect(dialog).toContainText('Considering the request.')
  await page.setViewportSize({ width: 390, height: 844 })
  await expect
    .poll(async () =>
      dialog.evaluate((el) => ({
        right: el.getBoundingClientRect().right,
        width: el.getBoundingClientRect().width,
        viewport: window.innerWidth,
        inside: el.getBoundingClientRect().right <= window.innerWidth,
      })),
    )
    .toMatchObject({ inside: true })
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true)
  await dialog.getByRole('button', { name: 'Close', exact: true }).click()
  await page.setViewportSize({ width: 1280, height: 900 })
  await toggle.uncheck()
  await page.getByRole('button', { name: 'Save settings' }).click()
  await expect(
    page.getByRole('button', { name: 'Save settings' }),
  ).toBeDisabled()
  const uncaptured = await send()
  await expect.poll(async () => (await records()).length).toBe(3)
  expect(
    (await records()).find((r) => r.request_id === uncaptured)?.trace_available,
  ).toBe(false)
  await page.getByRole('button', { name: 'Delete traces', exact: true }).click()
  await dialog
    .getByRole('button', { name: 'Delete all my traces', exact: true })
    .click()
  await expect(dialog).not.toBeVisible()
  await expect
    .poll(async () => (await records()).filter((r) => r.trace_available).length)
    .toBe(0)
  expect(
    (
      await context.request.get(`${tracing}/portal/api/requests/${toolID}`)
    ).status(),
  ).toBe(404)
  await page.reload()
  await expect(toggle).not.toBeChecked()
  await expect(page.getByLabel('Trace retention days')).toHaveValue('14')
  expect(
    await page.evaluate(() =>
      [...Object.values(localStorage), ...Object.values(sessionStorage)].some(
        (value) => value.includes('Earlier result'),
      ),
    ),
  ).toBe(false)
  expect(errors).toEqual([])
})

test('local-only first password change, rotation, revocation and logout', async ({
  page,
  context,
  browser,
}) => {
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  await page.goto(`${local}/portal/login/local`)
  await expect(page).toHaveURL(`${local}/portal/sign-in/local`)
  await expect(
    page.getByRole('button', { name: 'Continue with Keycloak' }),
  ).toHaveCount(0)
  await signIn(page, 'wrong-password')
  await expect(page.getByRole('alert')).toContainText(
    'invalid local username or password',
  )
  await expect(page.getByRole('alert')).toBeFocused()
  await expect(page.getByLabel('Local password', { exact: true })).toHaveValue(
    '',
  )
  await signIn(page, initial)
  await expect(
    page.getByRole('heading', { name: 'Change your password' }),
  ).toBeVisible()
  await expect(
    page.getByRole('link', { name: 'Admin', exact: true }),
  ).toHaveCount(0)
  expect(
    (await context.request.get(`${local}/portal/api/admin/users`)).status(),
  ).toBe(403)
  await passwordFields(page, initial, changed, 'does-not-match')
  await expect(page.getByRole('alert')).toHaveText(
    'The new passwords do not match.',
  )
  await passwordFields(page, initial, 'short')
  await expect(page.getByRole('alert')).toContainText('at least 15 characters')
  await passwordFields(page, initial, changed)
  await expect(
    page.getByRole('status').filter({ hasText: 'Password changed.' }),
  ).toBeVisible()
  expect((await context.request.get(`${local}/portal/api/me`)).status()).toBe(
    401,
  )
  await signIn(page, changed)
  await expect(
    page.getByText('Protected local administrator', { exact: true }),
  ).toBeVisible()
  const created = await context.request.post(`${local}/portal/api/keys`, {
    headers: { Origin: local },
    data: { label: 'admin personal key' },
  })
  expect(created.status()).toBe(201)
  const secret = (await created.json()).key as string
  expect(
    (
      await context.request.post(`${local}/portal/api/keys`, {
        headers: { Origin: local },
        data: { label: 'legacy personal key' },
      })
    ).status(),
  ).toBe(201)
  await page.reload()
  const personalKey = page
    .getByRole('table', { name: 'Personal API keys' })
    .getByRole('row')
    .filter({ hasText: 'admin personal key' })
  const copy = personalKey.getByRole('button', { name: /^Copy key/ })
  await expect(copy).toHaveCount(0)
  await expect(personalKey).toContainText('Active')
  expect((await page.locator('body').innerText()).includes(secret)).toBe(false)
  await personalKey
    .getByRole('button', { name: 'Revoke admin personal key', exact: true })
    .click()
  await page.getByRole('button', { name: 'Revoke key', exact: true }).click()
  await expect(personalKey).toContainText('Revoked')
  await expect(copy).toHaveCount(0)
  await page.getByRole('link', { name: 'Admin', exact: true }).click()
  await expect(
    page.getByRole('heading', { name: 'Admin', exact: true }),
  ).toBeVisible()
  const other = await browser.newContext()
  try {
    expect(
      (
        await other.request.post(`${local}/portal/login/local`, {
          headers: { Origin: local },
          data: { username: 'admin', password: changed },
        })
      ).status(),
    ).toBe(200)
    await page.getByRole('link', { name: 'Account', exact: true }).click()
    await passwordFields(page, 'wrong-current-password', rotated)
    await expect(page.getByRole('alert')).toContainText(
      'invalid local username or password',
    )
    await passwordFields(page, changed, rotated)
    await expect(
      page.getByRole('status').filter({ hasText: 'Password changed.' }),
    ).toBeVisible()
    expect((await other.request.get(`${local}/portal/api/me`)).status()).toBe(
      401,
    )
  } finally {
    await other.close()
  }
  await signIn(page, changed)
  await expect(page.getByRole('alert')).toContainText(
    'invalid local username or password',
  )
  await signIn(page, rotated)
  await expect(
    page.getByText('Protected local administrator', { exact: true }),
  ).toBeVisible()
  const cookies = await context.cookies()
  expect(
    cookies.find((cookie) => cookie.name === 'olr_session')?.httpOnly,
  ).toBe(true)
  await page.getByRole('button', { name: 'Sign out', exact: true }).click()
  await expect(page).toHaveURL(`${local}/`)
  await expect(
    page.getByRole('link', { name: 'Sign in', exact: true }),
  ).toBeVisible()
  expect((await context.request.get(`${local}/portal/api/me`)).status()).toBe(
    401,
  )
  expect(
    await page.evaluate(
      (secrets) =>
        [...Object.values(localStorage), ...Object.values(sessionStorage)].some(
          (value) => secrets.some((secret) => value.includes(secret)),
        ),
      [initial, changed, rotated],
    ),
  ).toBe(false)
  const logs = dockerLogs(
    fixtures.find((fixture) => fixture.base === local)!.container,
  )
  expect(
    [initial, changed, rotated].some(
      (secret) => logs.includes(secret) || page.url().includes(secret),
    ),
  ).toBe(false)
  expect(errors).toEqual([])
})

test('regular local user cannot discover or use Admin mutations', async ({
  page,
  context,
}) => {
  await page.goto(`${local}/portal/login/local`)
  await signIn(page, initial, 'reader')
  await expect(page.getByText('Local account', { exact: true })).toBeVisible()
  await expect(
    page.getByRole('link', { name: 'Admin', exact: true }),
  ).toHaveCount(0)
  expect(
    (await context.request.get(`${local}/portal/api/admin/users`)).status(),
  ).toBe(403)
  expect(
    (
      await context.request.put(
        `${local}/portal/api/admin/users/00000000-0000-0000-0000-000000000000/enabled`,
        { headers: { Origin: local }, data: { enabled: false } },
      )
    ).status(),
  ).toBe(403)
  await page.goto(`${local}/portal/admin`)
  await expect(
    page.getByRole('heading', { name: 'API keys', exact: true }),
  ).toBeVisible()
})

test('unreachable Keycloak keeps the default choice visible and direct local login usable', async ({
  page,
}) => {
  await page.goto(`${outage}/portal/`)
  await expect(
    page.getByRole('button', { name: 'Continue with Keycloak' }),
  ).toBeDisabled()
  await expect(
    page.getByText('Keycloak is temporarily unavailable.', { exact: false }),
  ).toBeVisible()
  await page.goto(`${outage}/portal/sign-in/keycloak`)
  await expect(page).toHaveURL(`${outage}/portal/sign-in?sso=unavailable`)
  await page.goto(`${outage}/portal/login/local`)
  await signIn(page, initial)
  await passwordFields(page, initial, changed)
  await signIn(page, changed)
  await expect(
    page.getByRole('link', { name: 'Admin', exact: true }),
  ).toBeVisible()
  await page.getByRole('button', { name: 'Sign out', exact: true }).click()
  await expect(page).toHaveURL(`${outage}/`)
  await expect(
    page.getByRole('link', { name: 'Sign in', exact: true }),
  ).toBeVisible()
  await page.setViewportSize({ width: 390, height: 844 })
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true)
})

test('administrator creates a local user and manages memberships, keys, disablement and password reset', async ({
  page,
  context,
  browser,
}) => {
  const temporary = 'person-temporary-password-only'
  const personal = 'person-replacement-password-only'
  const reset = 'person-reset-password-only'
  const personContext = await browser.newContext()
  const person = await personContext.newPage()
  const base = administration
  const managePerson = async () => {
    await page.reload()
    await page
      .getByTestId('users-table')
      .getByRole('row')
      .filter({ hasText: 'browser-user' })
      .getByRole('button', { name: 'Manage', exact: true })
      .click()
    await expect(
      page.getByRole('form', { name: 'Local group memberships' }),
    ).toBeVisible()
  }
  const createKey = async (label: string) => {
    await person
      .getByRole('button', { name: 'Create key', exact: true })
      .click()
    await person.getByLabel('Key label').fill(label)
    await person
      .getByRole('dialog')
      .getByRole('button', { name: 'Create key', exact: true })
      .click()
    const key = await person.getByTestId('new-key').inputValue()
    await person.getByRole('button', { name: 'I’ve saved my key' }).click()
    return key
  }
  const apiStatus = async (key: string) =>
    (
      await personContext.request.get(`${base}/v1/models`, {
        headers: { Authorization: `Bearer ${key}` },
      })
    ).status()
  try {
    await page.goto(`${base}/portal/login/local`)
    await signIn(page, initial)
    await passwordFields(page, initial, changed)
    await signIn(page, changed)
    await page.getByRole('link', { name: 'Admin', exact: true }).click()
    const root = page
      .getByTestId('users-table')
      .getByRole('row')
      .filter({ hasText: 'Protected local administrator' })
    await root.getByRole('button', { name: 'Manage', exact: true }).click()
    await expect(
      page.getByRole('form', { name: 'Local group memberships' }),
    ).toHaveCount(0)
    await expect(
      page.getByRole('button', { name: 'Reset local password', exact: true }),
    ).toHaveCount(0)
    await expect(
      page.getByRole('button', { name: 'Disable user', exact: true }),
    ).toHaveCount(0)
    await page
      .getByRole('button', { name: 'Create local user', exact: true })
      .click()
    const dialog = page.getByRole('dialog')
    await dialog.getByLabel('Username', { exact: true }).fill('browser-user')
    await dialog
      .getByLabel('Email (optional)', { exact: true })
      .fill('shared@example.test')
    await dialog
      .getByLabel('Temporary password', { exact: true })
      .fill(temporary)
    await dialog.getByLabel('staff', { exact: true }).check()
    await dialog
      .getByRole('button', { name: 'Create user', exact: true })
      .click()
    await expect(page.getByLabel('Effective access')).toContainText('test')
    await expect(
      page
        .getByRole('table', { name: 'Effective limits' })
        .getByRole('row')
        .filter({ hasText: 'Requests per minute' }),
    ).toContainText('7')

    await person.goto(`${base}/portal/login/local`)
    await signIn(person, temporary, 'browser-user')
    await expect(
      person.getByRole('heading', { name: 'Change your password' }),
    ).toBeVisible()
    await passwordFields(person, temporary, personal)
    await signIn(person, personal, 'browser-user')
    await expect(
      person.getByRole('link', { name: 'Admin', exact: true }),
    ).toHaveCount(0)
    const key = await createKey('laptop')
    await expect(person.getByRole('button', { name: /^Copy key/ })).toHaveCount(
      0,
    )
    const owner = (
      await (await personContext.request.get(`${base}/portal/api/me`)).json()
    ).user.id
    const metadata = (
      await (await personContext.request.get(`${base}/portal/api/keys`)).json()
    ).keys as { id: string; label: string }[]
    expect(
      (
        await personContext.request.post(
          `${base}/portal/api/admin/users/${owner}/keys/${metadata.find((item) => item.label === 'laptop')!.id}/copy`,
          { headers: { Origin: base }, data: {} },
        )
      ).status(),
    ).toBe(403)
    expect(await apiStatus(key)).toBe(200)
    await managePerson()
    const keys = page.getByRole('region', { name: 'User API keys' })
    await expect(keys).toContainText('laptop')
    await expect(keys.getByRole('button', { name: /^Copy key/ })).toHaveCount(0)
    expect(
      (
        await context.request.post(
          `${base}/portal/api/admin/users/${owner}/keys/${metadata.find((item) => item.label === 'laptop')!.id}/copy`,
          { headers: { Origin: base }, data: {} },
        )
      ).status(),
    ).toBe(410)
    expect(await apiStatus(key)).toBe(200)
    expect((await page.locator('body').innerText()).includes(key)).toBe(false)
    await keys
      .getByRole('button', { name: 'Revoke laptop', exact: true })
      .click()
    await page.getByRole('button', { name: 'Keep key', exact: true }).click()
    expect(await apiStatus(key)).toBe(200)
    await keys
      .getByRole('button', { name: 'Revoke laptop', exact: true })
      .click()
    await page.getByRole('button', { name: 'Revoke key', exact: true }).click()
    await expect(keys).toContainText('Revoked')
    expect(await apiStatus(key)).toBe(401)
    const retained = await createKey('retained until reset')

    const memberships = page.getByRole('form', {
      name: 'Local group memberships',
    })
    await memberships.getByLabel('staff', { exact: true }).uncheck()
    await memberships.getByLabel('students', { exact: true }).check()
    await memberships.getByRole('button', { name: 'Save memberships' }).click()
    await expect(page.getByLabel('Effective access')).toContainText('limited')
    await expect(
      page
        .getByRole('table', { name: 'Effective limits' })
        .getByRole('row')
        .filter({ hasText: 'Requests per minute' }),
    ).toContainText('3')
    expect(
      (await personContext.request.get(`${base}/portal/api/me`)).status(),
    ).toBe(401)
    const models = await personContext.request.get(`${base}/v1/models`, {
      headers: { Authorization: `Bearer ${retained}` },
    })
    expect(
      (await models.json()).data.map((model: { id: string }) => model.id),
    ).toEqual(['limited'])
    await memberships.getByLabel('staff', { exact: true }).check()
    await memberships.getByLabel('students', { exact: true }).uncheck()
    await memberships.getByRole('button', { name: 'Save memberships' }).click()
    await expect(page.getByLabel('Effective access')).toContainText('test')
    await person.goto(`${base}/portal/login/local`)
    await signIn(person, personal, 'browser-user')
    await page
      .getByRole('button', { name: 'Disable user', exact: true })
      .click()
    await expect(
      page.getByRole('button', { name: 'Enable user', exact: true }),
    ).toBeVisible()
    expect(
      (await personContext.request.get(`${base}/portal/api/me`)).status(),
    ).toBe(401)
    expect(await apiStatus(retained)).toBe(401)
    await page.getByRole('button', { name: 'Enable user', exact: true }).click()
    await expect(
      page.getByRole('button', { name: 'Disable user', exact: true }),
    ).toBeVisible()
    expect(
      (await personContext.request.get(`${base}/portal/api/me`)).status(),
    ).toBe(401)
    expect(await apiStatus(retained)).toBe(200)
    await person.goto(`${base}/portal/login/local`)
    await signIn(person, personal, 'browser-user')

    await page
      .getByRole('button', { name: 'Reset local password', exact: true })
      .click()
    await dialog.getByLabel('Temporary password', { exact: true }).fill(reset)
    await dialog
      .getByLabel('Confirm temporary password', { exact: true })
      .fill(reset)
    await dialog
      .getByRole('button', {
        name: 'Set password and revoke keys',
        exact: true,
      })
      .click()
    await expect(
      page.getByRole('status').filter({ hasText: 'Temporary password set.' }),
    ).toBeVisible()
    expect(
      (await personContext.request.get(`${base}/portal/api/me`)).status(),
    ).toBe(401)
    expect(await apiStatus(retained)).toBe(401)
    await person.goto(`${base}/portal/login/local`)
    await signIn(person, reset, 'browser-user')
    await passwordFields(person, reset, personal)
    await signIn(person, personal, 'browser-user')
    expect(await apiStatus(retained)).toBe(401)
    expect(
      await page.evaluate(
        (secrets) =>
          [
            ...Object.values(localStorage),
            ...Object.values(sessionStorage),
          ].some((value) => secrets.some((secret) => value.includes(secret))),
        [temporary, personal, reset, key, retained],
      ),
    ).toBe(false)
    expect(
      await person.evaluate(
        (secrets) =>
          [
            ...Object.values(localStorage),
            ...Object.values(sessionStorage),
          ].some((value) => secrets.some((secret) => value.includes(secret))),
        [temporary, personal, reset, key, retained],
      ),
    ).toBe(false)
    const logs = dockerLogs(
      fixtures.find((fixture) => fixture.base === base)!.container,
    )
    expect(
      [temporary, personal, reset, key, retained].some(
        (secret) => logs.includes(secret) || page.url().includes(secret),
      ),
    ).toBe(false)
    await page.setViewportSize({ width: 390, height: 844 })
    await expect
      .poll(() =>
        page.evaluate(
          () => document.documentElement.scrollWidth <= window.innerWidth,
        ),
      )
      .toBe(true)
    expect(
      (await context.request.get(`${base}/portal/api/admin/users`)).status(),
    ).toBe(200)
  } finally {
    await personContext.close()
  }
})

test('administrator edits endpoints, tests, applies, restarts and restores a routed model', async ({
  page,
  context,
}) => {
  const base = endpointAdministration
  const fixture = fixtures.find((fixture) => fixture.base === base)!
  const owner = docker([
    'exec',
    fixture.container,
    '/usr/local/bin/olr',
    'admin',
    'user',
    'add',
    '-username',
    'endpoint-client',
    '-groups',
    'staff',
  ])
  const key = docker([
    'exec',
    fixture.container,
    '/usr/local/bin/olr',
    'admin',
    'key',
    'create',
    '-user',
    owner,
    '-label',
    'endpoint-browser-test',
  ])
  const complete = async () => {
    const response = await context.request.post(`${base}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${key}` },
      data: {
        model: 'test',
        messages: [{ role: 'user', content: 'hello' }],
        max_tokens: 16,
      },
    })
    expect(response.status()).toBe(200)
    expect((await response.json()).model).toBe('chat-model')
  }
  await page.goto(`${base}/portal/login/local`)
  await signIn(page, initial)
  await passwordFields(page, initial, changed)
  await signIn(page, changed)
  await page.getByRole('link', { name: 'Admin', exact: true }).click()
  const panel = page.getByRole('region', {
    name: 'Endpoint configuration',
    exact: true,
  })
  const entry = panel.getByRole('article', {
    name: 'Endpoints for test',
    exact: true,
  })
  await expect(entry).toContainText('Source: YAML/environment')
  await entry
    .getByRole('button', { name: 'Edit endpoints for test', exact: true })
    .click()
  const dialog = page.getByRole('dialog')
  const form = dialog.getByRole('form', {
    name: 'Edit model endpoints',
    exact: true,
  })
  const detected = form.getByLabel('Detected upstream models', { exact: true })
  await expect(detected).toBeVisible()
  await expect(detected.locator('option')).toContainText([
    'Choose a model',
    'chat-model',
  ])
  await form
    .getByLabel('Upstream model name', { exact: true })
    .fill('missing-model')
  await form
    .getByRole('button', { name: 'Refresh models', exact: true })
    .click()
  await expect(
    form.getByRole('region', { name: 'Upstream model discovery' }),
  ).toContainText('not found on every replica')
  await expect(
    form.getByLabel('Upstream model name', { exact: true }),
  ).toHaveValue('missing-model')
  await detected.selectOption('chat-model')
  await form.getByLabel('Upstream model name', { exact: true }).fill('')
  await form
    .getByRole('button', { name: 'Refresh models', exact: true })
    .click()
  await expect(
    form.getByLabel('Upstream model name', { exact: true }),
  ).toHaveValue('chat-model')
  await form.getByRole('button', { name: 'Add replica', exact: true }).click()
  const second = form.getByRole('group', { name: 'Replica 2', exact: true })
  await second
    .getByLabel('Backend origin', { exact: true })
    .fill('http://mock-llm-b:8000')
  await second.getByLabel('Weight', { exact: true }).fill('2')
  await form
    .getByRole('button', { name: 'Remove replica 1', exact: true })
    .click()
  await form
    .getByLabel('Upstream model name', { exact: true })
    .fill('chat-model')
  await form
    .getByRole('button', { name: 'Test connections', exact: true })
    .click()
  await expect(
    form.getByRole('region', { name: 'Connection test results', exact: true }),
  ).toContainText('Healthy')
  const uncommitted = await (
    await context.request.get(`${base}/portal/api/admin/endpoints`)
  ).json()
  expect(
    uncommitted.models.find(
      (entry: { model: string }) => entry.model === 'test',
    ).revision,
  ).toBe(0)
  await form
    .getByRole('button', { name: 'Save and apply', exact: true })
    .click()
  await expect(dialog).toHaveCount(0)
  await expect(entry).toContainText('Source: Admin database')
  await expect(entry).toContainText('http://mock-llm-b:8000')
  await expect(entry).not.toContainText('http://mock-llm-a:8000')
  await complete()
  const first = (
    await (
      await context.request.get(`${base}/portal/api/admin/endpoints`)
    ).json()
  ).models.find((entry: { model: string }) => entry.model === 'test')

  // A second tab must retain its draft and surface a revision conflict.
  await entry
    .getByRole('button', { name: 'Edit endpoints for test', exact: true })
    .click()
  const current = structuredClone(first.configuration)
  current.pool[0].weight = 3
  expect(
    (
      await context.request.put(`${base}/portal/api/admin/endpoints`, {
        headers: { Origin: base },
        data: {
          model: 'test',
          revision: first.revision,
          configuration: current,
        },
      })
    ).status(),
  ).toBe(200)
  await form.getByLabel('Weight', { exact: true }).fill('4')
  await form
    .getByRole('button', { name: 'Save and apply', exact: true })
    .click()
  await expect(form.getByRole('alert')).toContainText('configuration changed')
  await form
    .getByRole('button', {
      name: 'Discard draft and reload current configuration',
      exact: true,
    })
    .click()
  await expect(form.getByLabel('Weight', { exact: true })).toHaveValue('3')
  await form
    .getByLabel('Health path', { exact: true })
    .fill('/nonexistent-health-check')
  await form
    .getByRole('button', { name: 'Test connections', exact: true })
    .click()
  await expect(
    form.getByRole('region', { name: 'Connection test results', exact: true }),
  ).toContainText('Failed')
  await form
    .getByRole('button', { name: 'Save and apply', exact: true })
    .click()
  await expect(form.getByRole('alert')).toContainText(
    'failed their health check',
  )
  await dialog.getByRole('button', { name: 'Close', exact: true }).click()
  await complete()

  docker(['restart', fixture.container])
  await expect
    .poll(
      async () => {
        try {
          return (await fetch(`${base}/readyz`)).status
        } catch {
          return 0
        }
      },
      { timeout: 30_000 },
    )
    .toBe(200)
  await page.reload()
  await expect(entry).toContainText('weight 3')
  await complete()
  await entry.getByText('Revision history and restore', { exact: true }).click()
  await entry
    .getByRole('button', {
      name: `Restore test revision ${first.revision}`,
      exact: true,
    })
    .click()
  await expect(page.getByRole('alertdialog')).toContainText(
    'http://mock-llm-b:8000',
  )
  await page.getByRole('button', { name: 'Cancel', exact: true }).click()
  await entry
    .getByRole('button', {
      name: `Restore test revision ${first.revision}`,
      exact: true,
    })
    .click()
  await page
    .getByRole('button', { name: 'Restore and apply', exact: true })
    .click()
  await expect(entry).toContainText('weight 2')
  await complete()
  await entry
    .getByRole('button', { name: 'Restore test from file', exact: true })
    .click()
  await page
    .getByRole('button', { name: 'Restore and apply', exact: true })
    .click()
  await expect(entry).toContainText('Source: YAML/environment')
  await expect(entry).toContainText('http://mock-llm-a:8000')
  await complete()
  await page.setViewportSize({ width: 390, height: 844 })
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    )
    .toBe(true)
  expect(
    await page.evaluate(
      (secret) =>
        [...Object.values(localStorage), ...Object.values(sessionStorage)].some(
          (value) => value.includes(secret),
        ),
      key,
    ),
  ).toBe(false)
  expect(dockerLogs(fixture.container).includes(key)).toBe(false)
})

test('administrator imports, checks and drafts Keycloak connection settings', async ({
  page,
}) => {
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  await page.goto(`${authentication}/portal/login/local`)
  await signIn(page, initial)
  await passwordFields(page, initial, changed)
  await signIn(page, changed)
  await page.getByRole('link', { name: 'Admin', exact: true }).click()
  const panel = page.getByRole('region', {
    name: 'Authentication settings',
    exact: true,
  })
  await expect(panel).toContainText('Config file and environment')
  await expect(panel).toContainText(`${authentication}/portal/callback`)
  const effective = panel.getByRole('table', {
    name: 'Effective authentication settings',
  })
  await expect(effective).toContainText(
    'environment variable OIDC_PORTAL_CLIENT_SECRET',
  )
  await expect(
    effective.getByRole('row', { name: /Issuer URL/ }),
  ).toContainText('file')

  // Checks against the real development Keycloak, before any import.
  const checks = panel.getByRole('form', { name: 'Run connection checks' })
  await checks.getByLabel('Test service client ID').fill('olr-service')
  await checks
    .getByLabel('Test service client secret')
    .fill('olr-service-dev-only')
  await checks.getByRole('button', { name: 'Run checks', exact: true }).click()
  const results = panel.getByRole('list', { name: 'Check results' })
  for (const name of [
    'Discovery',
    'Logout metadata',
    'Portal client credentials',
    'Service-token audience',
    'Sync permissions',
  ])
    await expect(
      results.getByRole('listitem').filter({ hasText: name }),
    ).toContainText('passed')

  await panel.getByRole('button', { name: 'Import current settings' }).click()
  await expect(panel).toContainText('Database — active revision 1')
  // A draft with a wrong stored portal secret fails its credential check
  // and never displays the value.
  const draft = panel.getByRole('form', { name: 'Edit authentication draft' })
  await draft.getByLabel('Portal client secret source').selectOption('store')
  await draft
    .getByLabel('New portal client secret')
    .fill('wrong-portal-secret-value')
  await draft.getByLabel('Primary sign-in option').selectOption('local')
  await draft.getByRole('button', { name: 'Save draft', exact: true }).click()
  await expect(panel).toContainText('Draft revision 2')
  await expect(panel).toContainText('stored encrypted')
  await checks.getByRole('button', { name: 'Run checks', exact: true }).click()
  await expect(
    results
      .getByRole('listitem')
      .filter({ hasText: 'Portal client credentials' }),
  ).toContainText('rejected the portal client ID or secret')
  await expect(page.locator('body')).not.toContainText(
    'wrong-portal-secret-value',
  )
  await panel
    .getByRole('button', { name: 'Discard draft', exact: true })
    .click()
  await expect(panel).not.toContainText('Draft revision 2')
  expect(errors).toEqual([])
})

test('administrator applies and rolls back authentication settings without a restart', async ({
  page,
  context,
  browser,
}) => {
  // This fixture's file configures no SSO and its environment only the
  // client secrets, so the database can become the only source of the rest.
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  const fixture = fixtures.find((fixture) => fixture.base === activation)!
  const keycloakPort = process.env.KEYCLOAK_PORT ?? '8081'
  const methods = async () =>
    (await (await context.request.get(`${activation}/portal/api/auth`)).json())
      .oidc as { configured: boolean; available: boolean }
  const settings = async () =>
    (await (
      await context.request.get(
        `${activation}/portal/api/admin/settings/authentication`,
      )
    ).json()) as { settings: { version: number } }
  await page.goto(`${activation}/portal/login/local`)
  await signIn(page, initial)
  await passwordFields(page, initial, changed)
  await signIn(page, changed)
  await page.getByRole('link', { name: 'Admin', exact: true }).click()
  const panel = page.getByRole('region', {
    name: 'Authentication settings',
    exact: true,
  })
  const status = panel.getByTestId('authentication-runtime')
  await panel.getByRole('button', { name: 'Import current settings' }).click()
  await expect(panel).toContainText('Database — active revision 1')
  // Importing does not change what this router runs until it is applied.
  await panel
    .getByRole('button', { name: 'Apply active revision to this router' })
    .click()
  await expect(status).toContainText('revision 1 · local sign-in only')

  // Draft the bundled Keycloak and check it.
  const draft = panel.getByRole('form', { name: 'Edit authentication draft' })
  await draft
    .getByLabel('Enable single sign-on (local sign-in always stays available)')
    .check()
  await draft
    .getByLabel('Issuer URL', { exact: true })
    .fill(`http://localhost:${keycloakPort}/realms/olr-dev`)
  await draft
    .getByLabel('Back-channel URL', { exact: true })
    .fill('http://keycloak:8080/realms/olr-dev')
  await draft.getByLabel('Portal client ID', { exact: true }).fill('olr-portal')
  await draft.getByLabel('API audience', { exact: true }).fill('olr-api')
  await draft.getByRole('button', { name: 'Save draft', exact: true }).click()
  await expect(panel).toContainText('Draft revision 2')
  const checks = panel.getByRole('form', { name: 'Run connection checks' })
  await checks.getByLabel('Test service client ID').fill('olr-service')
  await checks
    .getByLabel('Test service client secret')
    .fill('olr-service-dev-only')
  await checks.getByRole('button', { name: 'Run checks', exact: true }).click()
  await expect(
    panel
      .getByRole('list', { name: 'Check results' })
      .getByRole('listitem')
      .filter({ hasText: 'Discovery' }),
  ).toContainText('passed')
  // Without a test sign-in, Admin activation stays disabled.
  await expect(
    panel.getByRole('button', { name: 'Activate draft', exact: true }),
  ).toBeDisabled()

  // An operator activates with the CLI; Admin applies it live.
  const { settings: current } = await settings()
  docker([
    'exec',
    fixture.container,
    '/usr/local/bin/olr',
    'admin',
    'settings',
    'activate',
    '-expect-version',
    String(current.version),
    '-revision',
    '2',
  ])
  await page.reload()
  await panel
    .getByRole('button', { name: 'Apply active revision to this router' })
    .click()
  await expect(status).toContainText('revision 2')
  await expect.poll(async () => (await methods()).available).toBe(true)
  await page.reload()
  await expect(status).toContainText('revision 2 · Keycloak reachable')
  // The sign-in page now offers Keycloak, without a restart.
  const visitor = await browser.newContext()
  const signInPage = await visitor.newPage()
  await signInPage.goto(`${activation}/portal/sign-in`)
  await expect(
    signInPage.getByRole('button', { name: 'Continue with Keycloak' }),
  ).toBeEnabled()
  await visitor.close()

  // Roll back to local sign-in only; administration keeps working.
  await panel
    .getByRole('button', { name: 'Roll back to revision 1', exact: true })
    .click()
  await expect(status).toContainText('revision 1 · local sign-in only')
  expect((await methods()).configured).toBe(false)
  expect(errors).toEqual([])
})

test('administrator adds a model, alias and endpoint as a routing revision, activates and rolls back', async ({
  page,
  context,
}) => {
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  const fixture = fixtures.find((fixture) => fixture.base === routing)!
  const olr = (...args: string[]) =>
    docker(['exec', fixture.container, '/usr/local/bin/olr', 'admin', ...args])
  const owner = olr(
    'user',
    'add',
    '-username',
    'routing-client',
    '-groups',
    'admins',
  )
  const key = olr(
    'key',
    'create',
    '-user',
    owner,
    '-label',
    'routing-browser-test',
  )
  const complete = async (model: string) =>
    context.request.post(`${routing}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${key}` },
      data: {
        model,
        messages: [{ role: 'user', content: 'hello' }],
        max_tokens: 8,
      },
    })
  await page.goto(`${routing}/portal/login/local`)
  await signIn(page, initial)
  await passwordFields(page, initial, changed)
  await signIn(page, changed)
  await page.getByRole('link', { name: 'Admin', exact: true }).click()
  const panel = page.getByRole('region', {
    name: 'Routing settings',
    exact: true,
  })
  const models = panel.getByRole('table', { name: 'Routed models' })
  await expect(models.getByRole('row', { name: /^test/ })).toContainText(
    'staff',
  )
  await panel.getByRole('button', { name: 'Import current routing' }).click()
  await expect(panel).toContainText('Database — active routing revision 1')
  await panel
    .getByRole('button', { name: 'Apply active routing to this router' })
    .click()
  await expect(panel.getByTestId('routing-runtime')).toHaveText(
    'routing revision 1',
  )

  // A model granted by a group policy cannot be removed here.
  const draft = panel.getByRole('form', { name: 'Edit routing draft' })
  await expect(
    draft.getByRole('button', { name: 'Remove model test', exact: true }),
  ).toBeDisabled()
  // Add a model on the second compatible backend, plus an alias.
  await draft.getByLabel('New logical model name').fill('added-model')
  await draft.getByRole('button', { name: 'Add model', exact: true }).click()
  const added = draft.getByRole('article', { name: 'Model added-model' })
  await added.getByLabel('added-model upstream model').fill('chat-model')
  await added
    .getByLabel('added-model replica 1 origin')
    .fill('http://mock-llm-b:8000')
  await draft.getByLabel('New alias', { exact: true }).fill('quick')
  await draft.getByLabel('New alias target').selectOption('added-model')
  await draft.getByRole('button', { name: 'Add alias', exact: true }).click()
  // A duplicate origin in one pool is an actionable validation problem.
  await added
    .getByRole('button', { name: 'Add replica to added-model' })
    .click()
  await added
    .getByLabel('added-model replica 2 origin')
    .fill('http://mock-llm-b:8000')
  await draft
    .getByRole('button', { name: 'Save routing draft', exact: true })
    .click()
  const status = panel.locator('[aria-label="Routing draft status"]')
  await expect(status).toContainText('is listed twice in this pool')
  await expect(
    panel.getByRole('button', { name: 'Activate routing draft' }),
  ).toBeDisabled()
  await draft
    .getByRole('article', { name: 'Model added-model' })
    .getByRole('button', { name: 'Remove added-model replica 2' })
    .click()
  await draft
    .getByRole('button', { name: 'Save routing draft', exact: true })
    .click()
  await expect(status).toContainText('is valid')
  await panel.getByRole('button', { name: 'Test changed endpoints' }).click()
  await expect(
    panel.getByRole('list', { name: 'Endpoint check results' }),
  ).toContainText('models.added-model.pool[0]')
  await expect(
    panel.getByRole('list', { name: 'Endpoint check results' }),
  ).toContainText('passed')
  expect((await complete('quick')).status()).toBe(404)
  await panel.getByRole('button', { name: 'Activate routing draft' }).click()
  // Wait for this activation, not the previous runtime revision.
  await expect(status).toHaveCount(0)
  await expect(panel.getByTestId('routing-runtime')).not.toHaveText(
    'routing revision 1',
  )
  const routed = await complete('quick')
  expect(routed.status()).toBe(200)
  expect((await routed.json()).model).toBe('chat-model')
  await expect(models.getByRole('row', { name: /^added-model/ })).toContainText(
    'quick',
  )

  // Roll back to the imported routing: the alias and model are gone.
  await panel
    .getByRole('button', {
      name: 'Roll back routing to revision 1',
      exact: true,
    })
    .click()
  await expect(panel.getByTestId('routing-runtime')).toHaveText(
    'routing revision 1',
  )
  expect((await complete('quick')).status()).toBe(404)
  expect((await complete('test')).status()).toBe(200)
  expect(errors).toEqual([])
})

test('administrator edits group defaults and previews overrides before activation', async ({
  page,
  context,
}) => {
  const base = await startFixture(false, 'policies')
  await page.goto(`${base}/portal/sign-in/local`)
  await signIn(page, initial)
  await passwordFields(page, initial, changed)
  await signIn(page, changed)
  await expect(
    page.getByRole('heading', { name: 'API keys', exact: true }),
  ).toBeVisible()
  const created = await context.request.post(`${base}/portal/api/admin/users`, {
    headers: { Origin: base },
    data: { username: 'policy-user', groups: ['staff'], password: initial },
  })
  expect(created.status()).toBe(201)
  const id = (await created.json()).id as string
  expect(
    (
      await context.request.put(
        `${base}/portal/api/admin/users/${id}/overrides/rpm`,
        {
          headers: { Origin: base },
          data: { value: 2, reason: 'synthetic acceptance' },
        },
      )
    ).status(),
  ).toBe(204)
  await page.goto(`${base}/portal/admin`)
  const panel = page.getByRole('region', { name: 'Routing settings' })
  await panel.getByRole('button', { name: 'Import current routing' }).click()
  const form = panel.getByRole('form', { name: 'Edit routing draft' })
  await form.getByLabel('staff grant limited', { exact: true }).check()
  await form.getByLabel('staff rpm', { exact: true }).fill('9')
  await form.getByLabel('staff max_concurrent_batch', { exact: true }).fill('4')
  await form.getByLabel('staff strict quota', { exact: true }).check()
  await form.getByLabel('Preview user', { exact: true }).selectOption(id)
  await form
    .getByRole('button', { name: 'Preview access and affected users' })
    .click()
  const preview = form.locator('[aria-label="Policy preview results"]')
  await expect(preview).toContainText('limited')
  await expect(preview).toContainText('2 (user override)')
  await expect(preview).toContainText('Unlimited')
  await form
    .getByRole('button', { name: 'Save routing draft', exact: true })
    .click()
  await expect(
    panel.locator('[aria-label="Draft affected users"]'),
  ).toContainText('policy-user')
  await panel.getByRole('button', { name: 'Activate routing draft' }).click()
  await expect(
    panel.locator('[aria-label="Routing draft status"]'),
  ).toHaveCount(0)
  const detail = await context.request.get(
    `${base}/portal/api/admin/users/${id}`,
  )
  expect(detail.status()).toBe(200)
  const effective = await detail.json()
  expect(effective.models).toContain('limited')
  expect(effective.limits.rpm).toBe(2)
  expect(effective.limits.max_concurrent_batch).toBe(4)
  expect(effective.limits.strict_quota).toBe(true)
  await panel
    .getByRole('button', {
      name: 'Roll back routing to revision 1',
      exact: true,
    })
    .click()
  await expect(panel.getByTestId('routing-runtime')).toHaveText(
    'routing revision 1',
  )
  const restored = await (
    await context.request.get(`${base}/portal/api/admin/users/${id}`)
  ).json()
  expect(restored.models).not.toContain('limited')
  expect(restored.limits.rpm).toBe(2)
})

test('administration history shows searchable immutable events without credentials', async ({
  page,
  context,
}) => {
  const base = await startFixture(false, 'history')
  await page.goto(`${base}/portal/sign-in/local`)
  await signIn(page, initial)
  await passwordFields(page, initial, changed)
  await signIn(page, changed)
  await expect(
    page.getByRole('heading', { name: 'API keys', exact: true }),
  ).toBeVisible()
  const created = await context.request.post(`${base}/portal/api/admin/users`, {
    headers: { Origin: base },
    data: {
      username: 'history-browser-user',
      groups: ['staff'],
      password: initial,
    },
  })
  expect(created.status()).toBe(201)
  const id = (await created.json()).id as string
  expect(
    (
      await context.request.put(
        `${base}/portal/api/admin/users/${id}/overrides/rpm`,
        {
          headers: { Origin: base },
          data: { value: 4, reason: 'private-browser-reason' },
        },
      )
    ).status(),
  ).toBe(204)
  await page.goto(`${base}/portal/admin`)
  const history = page.getByRole('region', { name: 'Administration history' })
  await history.getByLabel('History target', { exact: true }).fill(id)
  await history
    .getByRole('button', { name: 'Search history', exact: true })
    .click()
  const events = history.getByRole('table', { name: 'Administration events' })
  await expect(events).toContainText('user.create_local')
  await expect(events).toContainText('user.override_set')
  await expect(events).not.toContainText(initial)
  await expect(events).not.toContainText('private-browser-reason')
  await history
    .getByLabel('History action', { exact: true })
    .fill('user.override_set')
  await history
    .getByRole('button', { name: 'Search history', exact: true })
    .click()
  await expect(events.getByRole('row')).toHaveCount(2)
  await expect(events).toContainText('succeeded')
  await history
    .getByLabel('History result', { exact: true })
    .selectOption('failed')
  await history
    .getByRole('button', { name: 'Search history', exact: true })
    .click()
  await expect(history).toContainText('No matching administration events')
})

// The external connection is a fresh process and realm, never the bundled IdP
// or an operator's production realm. All credentials below are public fixtures.
async function externalKeycloak(base: string, directory: string) {
  const port = await new Promise<number>((resolve, reject) => {
    const listener = createServer()
    listener.on('error', reject)
    listener.listen(0, '127.0.0.1', () => {
      const address = listener.address()
      if (!address || typeof address === 'string')
        return reject(new Error('No IdP test port'))
      listener.close(() => resolve(address.port))
    })
  })
  const container = `olr-external-${process.pid}`
  externalContainers.push(container)
  const source = JSON.parse(
    readFileSync('../deploy/keycloak/olr-dev-realm.json', 'utf8'),
  )
  source.realm = `olr-acceptance-${process.pid}`
  source.displayName = 'Isolated local acceptance realm (test only)'
  for (const client of source.clients) {
    if (client.clientId === 'olr-portal') {
      client.secret = 'external-portal-test-only'
      client.redirectUris = [`${base}/portal/callback`]
      client.webOrigins = [base]
      client.attributes['post.logout.redirect.uris'] = `${base}/`
    }
  }
  const realmFile = join(directory, 'external-realm.json')
  writeFileSync(realmFile, JSON.stringify(source), { mode: 0o644 })
  const origin = `http://127.0.0.1:${port}`
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
    `KC_HOSTNAME=${origin}`,
    '-e',
    'KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true',
    '-v',
    `${realmFile}:/opt/keycloak/data/import/acceptance.json:ro`,
    'quay.io/keycloak/keycloak:26.8.2',
    'start-dev',
    '--import-realm',
  ])
  const issuer = `${origin}/realms/${source.realm}`
  await expect
    .poll(
      async () => {
        try {
          return (await fetch(`${issuer}/.well-known/openid-configuration`))
            .status
        } catch {
          return 0
        }
      },
      { timeout: 90_000 },
    )
    .toBe(200)
  return {
    container,
    issuer,
    backchannel: `http://${container}:8080/realms/${source.realm}`,
  }
}

test('isolated external Keycloak checks, dual login, issuer switch, outage, restart and rollback', async ({
  page,
  context,
  browser,
}) => {
  test.setTimeout(180_000)
  const base = await startFixture(false, 'external')
  const fixture = fixtures.find((f) => f.base === base)!
  const external = await externalKeycloak(base, fixture.directory)
  await page.goto(`${base}/portal/sign-in/local`)
  await signIn(page, initial)
  await passwordFields(page, initial, changed)
  await signIn(page, changed)
  await expect(
    page.getByRole('heading', { name: 'API keys', exact: true }),
  ).toBeVisible()
  const endpoint = `${base}/portal/api/admin/settings/authentication`
  const call = async (method: string, suffix: string, data?: unknown) => {
    const response = await context.request.fetch(endpoint + suffix, {
      method,
      headers: method === 'GET' ? {} : { Origin: base },
      data,
    })
    expect(response.status()).toBe(200)
    return response.json()
  }
  const imported = await call('POST', '/import', {})
  const localRevision = imported.settings.active.id as number
  const baseline = imported.settings.active.document
  const save = async (
    document: Record<string, unknown>,
    secrets?: Record<string, string>,
  ) => {
    const current = await call('GET', '')
    return call('PUT', '/draft', {
      version: current.settings.version,
      document,
      secrets,
    })
  }
  const check = async (revision: number) =>
    call('POST', '/checks', {
      revision,
      test_client_id: 'olr-service',
      test_client_secret: 'olr-service-dev-only',
    })
  const document = {
    ...baseline,
    enabled: true,
    issuer: external.issuer,
    backchannel_url: external.backchannel,
    portal_client_id: 'olr-portal',
    audience: 'olr-api',
    sync_client_id: 'olr-sync',
  }
  let draft = await save(document, {
    portal_client_secret: 'wrong-test-secret',
    sync_client_secret: 'wrong-sync-test-secret',
  })
  let checks = await check(draft.settings.draft.id)
  expect(
    checks.results.find(
      (r: { name: string }) => r.name === 'portal_credentials',
    ).status,
  ).toBe('failed')
  expect(
    checks.results.find((r: { name: string }) => r.name === 'sync_permissions')
      .status,
  ).toBe('failed')
  expect(JSON.stringify(checks)).not.toContain('wrong-test-secret')
  draft = await save(
    { ...document, backchannel_url: 'http://127.0.0.1:1/realms/unavailable' },
    {
      portal_client_secret: 'external-portal-test-only',
      sync_client_secret: 'olr-sync-dev-only',
    },
  )
  checks = await check(draft.settings.draft.id)
  expect(
    checks.results.find((r: { name: string }) => r.name === 'discovery').status,
  ).toBe('failed')
  // A mismatched public issuer is refused even through a reachable backchannel.
  draft = await save(
    { ...document, issuer: `${external.issuer}-wrong` },
    {
      portal_client_secret: 'external-portal-test-only',
      sync_client_secret: 'olr-sync-dev-only',
    },
  )
  checks = await check(draft.settings.draft.id)
  expect(
    checks.results.find((r: { name: string }) => r.name === 'discovery').status,
  ).toBe('failed')
  draft = await save(document, {
    portal_client_secret: 'external-portal-test-only',
    sync_client_secret: 'olr-sync-dev-only',
  })
  const revision = draft.settings.draft.id as number
  checks = await check(revision)
  expect(
    checks.results.every((r: { status: string }) => r.status !== 'failed'),
  ).toBe(true)
  const started = await call('POST', '/test-sign-in', { revision })
  await page.goto(started.url)
  await page.getByLabel('Username', { exact: true }).fill('staff')
  await page.getByLabel('Password', { exact: true }).fill('staff-dev-only')
  await page.getByRole('button', { name: 'Sign In', exact: true }).click()
  await expect(page).toHaveURL(
    `${base}/portal/admin?authentication_test=passed#authentication`,
  )
  const checked = await call('GET', '')
  expect(checked.settings.draft.checks.test_sign_in.status).toBe('passed')
  await call('POST', '/activate', {
    version: checked.settings.version,
    revision,
  })
  const visitor = await browser.newContext()
  const sso = await visitor.newPage()
  await sso.goto(`${base}/portal/sign-in/keycloak`)
  await sso.getByLabel('Username', { exact: true }).fill('staff')
  await sso.getByLabel('Password', { exact: true }).fill('staff-dev-only')
  await sso.getByRole('button', { name: 'Sign In', exact: true }).click()
  await expect(
    sso.getByRole('heading', { name: 'API keys', exact: true }),
  ).toBeVisible()
  expect((await visitor.request.get(`${base}/portal/api/me`)).status()).toBe(
    200,
  )
  const ssoMe = await (
    await visitor.request.get(`${base}/portal/api/me`)
  ).json()
  const localMe = await (
    await context.request.get(`${base}/portal/api/me`)
  ).json()
  expect(ssoMe.user.id).not.toBe(localMe.user.id)
  expect(
    (await visitor.request.get(`${base}/portal/api/admin/history`)).status(),
  ).toBe(403)
  // Stop only the isolated IdP. Local administration survives both the outage
  // and a real router restart, and can roll back without contacting the IdP.
  docker(['stop', external.container])
  docker(['restart', fixture.container])
  await expect
    .poll(
      async () => {
        try {
          return (await fetch(`${base}/portal/api/auth`)).status
        } catch {
          return 0
        }
      },
      { timeout: 30_000 },
    )
    .toBe(200)
  await page.goto(`${base}/portal/sign-in/local`)
  await signIn(page, changed)
  await expect(
    page.getByRole('heading', { name: 'API keys', exact: true }),
  ).toBeVisible()
  const afterRestart = await call('GET', '')
  expect(afterRestart.runtime.revision).toBe(revision)
  await call('POST', '/rollback', {
    version: afterRestart.settings.version,
    revision: localRevision,
  })
  expect((await call('GET', '')).runtime.sso_configured).toBe(false)
  expect((await visitor.request.get(`${base}/portal/api/me`)).status()).toBe(
    401,
  )
  await visitor.close()
  const history = await (
    await context.request.get(
      `${base}/portal/api/admin/history?target=authentication`,
    )
  ).json()
  const encoded = JSON.stringify(history)
  for (const secret of [
    'external-portal-test-only',
    'olr-sync-dev-only',
    'staff-dev-only',
    'wrong-test-secret',
    'wrong-sync-test-secret',
  ])
    expect(encoded).not.toContain(secret)
  expect(
    history.events.some(
      (e: { action: string }) => e.action === 'settings.rollback',
    ),
  ).toBe(true)
})

test('backup restore preserves protected credentials, revisions, audit and accounting before recovery', async ({
  page,
  context,
  browser,
}) => {
  test.setTimeout(120_000)
  const base = await startFixture(false, 'restore')
  const fixture = fixtures.find((f) => f.base === base)!
  await page.goto(`${base}/portal/sign-in/local`)
  await signIn(page, initial)
  await passwordFields(page, initial, changed)
  await signIn(page, changed)
  await expect(
    page.getByRole('heading', { name: 'API keys', exact: true }),
  ).toBeVisible()
  const call = async (method: string, path: string, data?: unknown) => {
    const response = await context.request.fetch(`${base}/portal/api/${path}`, {
      method,
      headers: method === 'GET' ? {} : { Origin: base },
      data,
    })
    expect(response.status()).toBe(200)
    return response.json()
  }
  const root = (await call('GET', 'me')).user.id as string
  const readers = await call('GET', 'admin/users?match=reader')
  const reader = readers.users[0].id as string
  expect(
    (
      await context.request.put(
        `${base}/portal/api/admin/users/${reader}/groups`,
        { headers: { Origin: base }, data: { groups: ['staff'] } },
      )
    ).status(),
  ).toBe(204)
  expect(
    (
      await context.request.put(
        `${base}/portal/api/admin/users/${reader}/overrides/rpm`,
        {
          headers: { Origin: base },
          data: { value: 3, reason: 'restore acceptance' },
        },
      )
    ).status(),
  ).toBe(204)
  const rootKey = (
    await (
      await context.request.post(`${base}/portal/api/keys`, {
        headers: { Origin: base },
        data: { label: 'recovery-revoked-key' },
      })
    ).json()
  ).key as string
  const imported = await call('POST', 'admin/settings/routing/import', {})
  const document = imported.settings.active.document
  document.groups.staff.models = ['test', 'limited']
  document.groups.staff.rpm = 9
  let routing = await call('PUT', 'admin/settings/routing/draft', {
    version: imported.settings.version,
    document,
  })
  routing = await call('POST', 'admin/settings/routing/activate', {
    version: routing.settings.version,
    revision: routing.settings.draft.id,
  })
  const authentication = await call(
    'POST',
    'admin/settings/authentication/import',
    {},
  )
  const visitor = await browser.newContext()
  expect(
    (
      await visitor.request.post(`${base}/portal/login/local`, {
        headers: { Origin: base },
        data: { username: 'reader', password: initial },
      })
    ).status(),
  ).toBe(200)
  const key = (
    await (
      await visitor.request.post(`${base}/portal/api/keys`, {
        headers: { Origin: base },
        data: { label: 'retained-reader-key' },
      })
    ).json()
  ).key as string
  const streamed = await visitor.request.post(`${base}/v1/chat/completions`, {
    headers: { Authorization: `Bearer ${key}` },
    data: {
      model: 'limited',
      stream: true,
      max_tokens: 7,
      messages: [{ role: 'user', content: 'synthetic restore acceptance' }],
    },
  })
  expect(streamed.status()).toBe(200)
  expect(await streamed.text()).toContain('[DONE]')
  await expect
    .poll(() =>
      sql(
        fixture.database,
        `SELECT count(*) FROM usage WHERE user_id='${reader}' AND is_stream AND status='200'`,
      ),
    )
    .toBe('1')
  const snapshotSQL = `SELECT json_build_object('root',u.id,'hash',c.password_hash,'credential_revision',c.revision,'settings',(SELECT jsonb_object_agg(domain,active_revision) FROM settings_state),'audit',(SELECT md5(string_agg(row_to_json(e)::text,'|' ORDER BY id)) FROM audit_events e),'usage',(SELECT sum(input_tokens+output_tokens) FROM usage_daily WHERE user_id='${reader}')) FROM users u JOIN local_credentials c ON c.user_id=u.id WHERE u.protected_admin`
  const before = sql(fixture.database, snapshotSQL)
  const image = docker(['inspect', '--format', '{{.Image}}', fixture.container])
  docker(['stop', fixture.container])
  // The dump stays in this test process's memory. It is never a test artifact,
  // terminal log, screenshot, trace or file in the repository.
  const backup = docker([
    ...compose,
    'exec',
    '-T',
    'postgres',
    'pg_dump',
    '-U',
    'olr',
    '--no-owner',
    '--no-acl',
    fixture.database,
  ])
  sql('olr', `DROP DATABASE ${fixture.database} WITH (FORCE)`)
  sql('olr', `CREATE DATABASE ${fixture.database}`)
  const restore = spawnSync(
    'docker',
    [
      ...compose,
      'exec',
      '-T',
      'postgres',
      'psql',
      '-U',
      'olr',
      '-d',
      fixture.database,
      '-v',
      'ON_ERROR_STOP=1',
    ],
    { cwd: '..', input: backup, encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 },
  )
  if (restore.status !== 0)
    throw new Error(
      'Isolated restore failed; credential-bearing output suppressed',
    )
  expect(sql(fixture.database, snapshotSQL) === before).toBe(true)
  // Recreate this disposable router with both bootstrap sources retired.
  docker(['rm', '-f', fixture.container])
  const port = new URL(base).port
  docker([
    'run',
    '-d',
    '--name',
    fixture.container,
    '--network',
    `${project}_default`,
    '-p',
    `127.0.0.1:${port}:8080`,
    '-e',
    `OLR_DATABASE_URL=postgres://olr:olr-dev-only@postgres:5432/${fixture.database}?sslmode=disable`,
    '-e',
    `PORTAL_BASE_URL=${base}`,
    '-e',
    'PORTAL_COOKIE_SECURE=false',
    '-e',
    'OLR_SETTINGS_ENCRYPTION_KEY=CAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAg=',
    '-v',
    `${join(fixture.directory, 'config.yaml')}:/etc/olr/config.yaml:ro`,
    image,
  ])
  await expect
    .poll(
      async () => {
        try {
          return (await fetch(`${base}/portal/api/auth`)).status
        } catch {
          return 0
        }
      },
      { timeout: 30_000 },
    )
    .toBe(200)
  expect(sql(fixture.database, snapshotSQL) === before).toBe(true)
  await page.goto(`${base}/portal/sign-in/local`)
  await signIn(page, changed)
  await expect(
    page.getByRole('heading', { name: 'API keys', exact: true }),
  ).toBeVisible()
  expect((await call('GET', 'me')).user.id).toBe(root)
  expect((await call('GET', 'admin/settings/routing')).runtime.revision).toBe(
    routing.settings.active.id,
  )
  expect(
    (await call('GET', 'admin/settings/authentication')).runtime.revision,
  ).toBe(authentication.settings.active.id)
  const detail = await call('GET', `admin/users/${reader}`)
  expect(detail.limits.rpm).toBe(3)
  expect(detail.models).toContain('limited')
  expect(
    (
      await visitor.request.get(`${base}/v1/models`, {
        headers: { Authorization: `Bearer ${key}` },
      })
    ).status(),
  ).toBe(200)
  // Lost-password recovery uses redirected input, with no secret in argv,
  // while retaining protected UUID and the restored configuration/history.
  const recovery = 'restore-recovery-test-password'
  const final = 'restore-final-test-password'
  const recovered = spawnSync(
    'docker',
    [
      'exec',
      '-i',
      fixture.container,
      '/usr/local/bin/olr',
      'admin',
      'recover',
      '-password-stdin',
    ],
    { input: recovery, encoding: 'utf8' },
  )
  expect(recovered.status).toBe(0)
  expect(recovered.stdout + recovered.stderr).not.toContain(recovery)
  expect(
    (
      await context.request.get(`${base}/v1/models`, {
        headers: { Authorization: `Bearer ${rootKey}` },
      })
    ).status(),
  ).toBe(401)
  await page.goto(`${base}/portal/sign-in/local`)
  await signIn(page, recovery)
  await passwordFields(page, recovery, final)
  await signIn(page, final)
  await expect(
    page.getByRole('heading', { name: 'API keys', exact: true }),
  ).toBeVisible()
  expect((await call('GET', 'me')).user.id).toBe(root)
  const history = await call('GET', `admin/history?target=${root}`)
  expect(
    history.events.some(
      (e: { action: string; result: string }) =>
        e.action === 'local_admin.recover' && e.result === 'succeeded',
    ),
  ).toBe(true)
  for (const secret of [key, rootKey, initial, changed, recovery, final])
    expect(JSON.stringify(history)).not.toContain(secret)
  await visitor.close()
})

test('agent administration key lifecycle', async ({ page, context }) => {
  const base = await startFixture(false, 'agents')
  const headers = { Origin: base }
  expect(
    (
      await context.request.post(`${base}/portal/login/local`, {
        headers,
        data: { username: 'admin', password: initial },
      })
    ).status(),
  ).toBe(200)
  expect(
    (
      await context.request.post(`${base}/portal/password`, {
        headers,
        data: { current_password: initial, new_password: changed },
      })
    ).status(),
  ).toBe(200)
  await checkAgentAccess(page, context.request, base, changed)
})
