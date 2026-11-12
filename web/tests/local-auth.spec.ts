import { test, expect, type Page } from '@playwright/test'
import { execFileSync, spawnSync } from 'node:child_process'
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
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
  const config = join(fixture.directory, 'config.yaml')
  writeFileSync(
    config,
    `${outage ? 'oidc:\n  issuer: http://127.0.0.1:1/realms/unavailable\n  portal_client_id: browser-test\n' : ''}models:\n  test:\n    upstream_model: chat-model\n    protocols: ${suffix === 'tracing' ? '[openai, anthropic]' : '[openai]'}\n    timeout_s: 5\n    pool:\n      - {url: http://mock-llm-a:8000, weight: 1, max_inflight: 4}\n  limited:\n    upstream_model: chat-model\n    protocols: [openai]\n    timeout_s: 5\n    pool:\n      - {url: http://mock-llm-b:8000, weight: 1, max_inflight: 4}\ngroups:\n  staff: {models: [test], rpm: 7, max_concurrent: 2}\n  students: {models: [limited], rpm: 3}\n  admins: {models: ['*'], admin: true}\n`,
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
      '-e',
      'OIDC_PORTAL_CLIENT_SECRET=browser-test-only',
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
test.beforeAll(async () => {
  test.setTimeout(60_000)
  local = await startFixture(false)
  outage = await startFixture(true)
  administration = await startFixture(false, 'administration')
  endpointAdministration = await startFixture(false, 'endpoints')
  tracing = await startFixture(false, 'tracing')
})
test.afterAll(() => {
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
  await expect(toggle).toBeChecked()
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
  await dialog.getByRole('tab', { name: /^Stream events/ }).click()
  await dialog.getByLabel('Filter stream events').selectOption('tool_call')
  await expect(dialog.locator('.trace-timeline')).toContainText('weather')
  await dialog.getByRole('tab', { name: 'Details', exact: true }).click()
  await expect(dialog).toContainText(toolID)
  const trace = await (
    await context.request.get(`${tracing}/portal/api/requests/${toolID}`)
  ).json()
  expect(Date.parse(trace.expires_at) - Date.now()).toBeGreaterThan(
    13 * 86_400_000,
  )
  expect(JSON.stringify(trace).includes(key)).toBe(false)
  expect(JSON.stringify(trace).includes('private-header-marker')).toBe(false)
  await dialog.getByRole('button', { name: 'Close', exact: true }).click()
  await page
    .getByRole('button', { name: `Inspect request ${thinkingID}`, exact: true })
    .click()
  await dialog.getByText('Reasoning', { exact: true }).click()
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
  const fixture = fixtures.find((fixture) => fixture.base === local)!
  sql(
    fixture.database,
    "UPDATE api_keys SET key_ciphertext=NULL,key_encryption_id=NULL WHERE label='legacy personal key'",
  )
  await page.reload()
  const personalKey = page
    .getByRole('table', { name: 'Personal API keys' })
    .getByRole('row')
    .filter({ hasText: 'admin personal key' })
  const copy = personalKey.getByRole('button', {
    name: 'Copy key admin personal key',
    exact: true,
  })
  await expect(copy).toBeEnabled()
  await expect(personalKey.getByRole('cell').nth(4)).toContainText('Active')
  await expect(personalKey.getByRole('cell').nth(4)).toContainText('Copy key')
  await copy.click()
  await expect(
    page
      .getByRole('status')
      .filter({ hasText: 'API key copied to clipboard.' }),
  ).toBeVisible()
  expect(
    await page.evaluate(
      async (key) => (await navigator.clipboard.readText()) === key,
      secret,
    ),
  ).toBe(true)
  expect((await page.locator('body').innerText()).includes(secret)).toBe(false)
  expect(
    await page.evaluate(
      (key) =>
        [...Object.values(localStorage), ...Object.values(sessionStorage)].some(
          (value) => value.includes(key),
        ),
      secret,
    ),
  ).toBe(false)
  await expect(
    page.getByRole('button', {
      name: 'Copy key legacy personal key',
      exact: true,
    }),
  ).toBeDisabled()
  sql(
    fixture.database,
    "UPDATE api_keys SET expires=clock_timestamp()-interval '1 day' WHERE label='legacy personal key'",
  )
  await page.reload()
  await expect(
    page.getByRole('button', {
      name: 'Copy key legacy personal key',
      exact: true,
    }),
  ).toHaveCount(0)
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
  await expect(page).toHaveURL(`${local}/portal/sign-in/local`)
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
  await expect(page).toHaveURL(`${outage}/portal/sign-in/local`)
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
    await keys
      .getByRole('button', { name: 'Copy key laptop', exact: true })
      .click()
    await expect(keys.getByRole('status')).toHaveText(
      'API key copied to clipboard.',
    )
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(key)
    expect(
      await page.evaluate(() =>
        JSON.stringify({ ...localStorage, ...sessionStorage }),
      ),
    ).not.toContain(key)
    const fixture = fixtures.find((fixture) => fixture.base === base)!
    sql(
      fixture.database,
      "UPDATE api_keys SET key_ciphertext=NULL,key_encryption_id=NULL WHERE label='laptop'",
    )
    await managePerson()
    await expect(
      keys.getByRole('button', { name: 'Copy key laptop', exact: true }),
    ).toBeDisabled()
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
