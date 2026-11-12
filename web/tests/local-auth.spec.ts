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

async function startFixture(outage: boolean) {
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
  const database = `portal_browser_${process.pid}_${outage ? 'outage' : 'local'}`
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
    `${outage ? 'oidc:\n  issuer: http://127.0.0.1:1/realms/unavailable\n  portal_client_id: browser-test\n' : ''}models:\n  test:\n    protocols: [openai]\n    timeout_s: 5\n    pool:\n      - {url: http://127.0.0.1:1, weight: 1, max_inflight: 4}\n`,
  )
  const image = docker([
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
  // A regular local identity exercises authorization without adding user
  // management UI that belongs to #75. Copy only this fixture's password hash.
  sql(
    database,
    `WITH added AS (INSERT INTO users(issuer,sub,username) VALUES('local','reader','reader') RETURNING id) INSERT INTO local_credentials(user_id,version,password_hash,must_change_password) SELECT added.id,c.version,c.password_hash,false FROM added CROSS JOIN local_credentials c JOIN users u ON u.id=c.user_id WHERE u.protected_admin`,
  )
  return fixture.base
}

let local: string
let outage: string
test.beforeAll(async () => {
  test.setTimeout(60_000)
  local = await startFixture(false)
  outage = await startFixture(true)
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
