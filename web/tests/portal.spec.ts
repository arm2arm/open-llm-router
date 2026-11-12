import { test, expect } from '@playwright/test'
import { execFileSync } from 'node:child_process'

test('self-service login, key, copied completion, usage, revoke and logout', async ({
  page,
  context,
}) => {
  const violations: string[] = []
  const errors: string[] = []
  await page.exposeFunction('onPolicyViolation', (directive: string) =>
    violations.push(directive),
  )
  await page.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', (event) => {
      void (
        window as unknown as {
          onPolicyViolation: (value: string) => Promise<void>
        }
      ).onPolicyViolation(event.violatedDirective)
    })
  })
  page.on('pageerror', (error) => errors.push(error.message))
  await page.goto('/portal/connect')
  await expect(
    page.getByRole('heading', { name: 'Sign in', exact: true }),
  ).toBeVisible()
  await expect(page.getByLabel('Local username', { exact: true })).toBeVisible()
  await expect(
    page.getByRole('button', { name: 'Continue with Keycloak' }),
  ).toHaveAttribute('data-variant', 'default')
  await expect(
    page.getByRole('button', { name: 'Sign in with local account' }),
  ).toHaveAttribute('data-variant', 'outline')
  await page.getByRole('button', { name: 'Continue with Keycloak' }).click()
  await expect(page.getByLabel('Username', { exact: true })).toBeVisible()
  await page.getByLabel('Username', { exact: true }).fill('staff')
  await page.getByLabel('Password', { exact: true }).fill('staff-dev-only')
  await page.getByRole('button', { name: 'Sign In', exact: true }).click()
  await expect(
    page.getByRole('heading', { name: 'API keys', exact: true }),
  ).toBeVisible()
  await expect(
    page.getByText('Keycloak account', { exact: true }),
  ).toBeVisible()
  await page.getByRole('link', { name: 'Account', exact: true }).click()
  await expect(
    page.getByRole('heading', { name: 'Password managed by Keycloak' }),
  ).toBeVisible()
  await expect(
    page.getByLabel('Current password', { exact: true }),
  ).toHaveCount(0)
  await page.getByRole('link', { name: 'API keys', exact: true }).click()

  const label = `<img src=x onerror=alert(1)> ${Date.now()}`
  await page.getByRole('button', { name: 'Create key', exact: true }).click()
  await page.getByLabel('Key label').fill(label)
  await page.getByLabel('Expires after').selectOption('7')
  await page
    .getByRole('dialog')
    .getByRole('button', { name: 'Create key', exact: true })
    .click()
  await expect(page.getByTestId('new-key')).toBeVisible()
  const key = await page.getByTestId('new-key').inputValue()
  if (!/^olr-[A-Za-z0-9]{43}$/.test(key))
    throw new Error('New key has an invalid format')
  await page.getByRole('button', { name: 'Copy key', exact: true }).click()
  expect(
    await page
      .evaluate(() => navigator.clipboard.readText())
      .then((value) => value === key),
  ).toBe(true)
  await page.getByRole('button', { name: 'I’ve saved my key' }).click()
  await expect(page.getByText(label, { exact: true })).toBeVisible()
  expect(await page.locator('main img').count()).toBe(0)
  await page.reload()
  await expect(page.getByText(label, { exact: true })).toBeVisible()
  expect((await page.locator('body').innerText()).includes(key)).toBe(false)
  expect(
    await page.evaluate(
      (secret) =>
        [...Object.values(localStorage), ...Object.values(sessionStorage)].some(
          (value) => value.includes(secret),
        ),
      key,
    ),
  ).toBe(false)

  await page.getByRole('link', { name: 'Connect', exact: true }).click()
  await expect(page.getByTestId('snippet')).toBeVisible()
  await page.getByLabel('Model', { exact: true }).selectOption('chat-model')
  await page.getByRole('button', { name: 'Copy snippet', exact: true }).click()
  const code = await page.evaluate(() => navigator.clipboard.readText())
  if (!code.startsWith('# Set OLR_API_KEY') || !code.includes('curl --fail'))
    throw new Error('Expected a copied curl snippet')
  const completion = JSON.parse(
    execFileSync('bash', ['-c', code], {
      env: { ...process.env, OLR_API_KEY: key },
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
    }),
  )
  expect(Boolean(completion.choices[0].message.content)).toBe(true)
  for (const client of [
    'OpenAI Python',
    'Anthropic Python',
    'Claude Code',
    'OpenCode',
  ]) {
    await page.getByRole('button', { name: client, exact: true }).click()
    await expect(page.getByTestId('snippet')).toContainText('chat-model')
    expect((await page.getByTestId('snippet').innerText()).includes(key)).toBe(
      false,
    )
  }

  await page.getByRole('link', { name: 'Usage', exact: true }).click()
  await expect(page.getByTestId('usage-table')).toBeVisible()
  const database = () =>
    execFileSync(
      'docker',
      [
        'compose',
        '--env-file',
        '/dev/null',
        '-p',
        process.env.COMPOSE_PROJECT_NAME ?? 'olr',
        'exec',
        '-T',
        'postgres',
        'psql',
        '-U',
        'olr',
        '-d',
        'olr',
        '-At',
        '-c',
        "SELECT sum(requests),sum(input_tokens),sum(output_tokens) FROM usage_daily d JOIN users u ON u.id=d.user_id WHERE u.username='staff' AND u.issuer<>'local' AND u.enabled AND d.model='chat-model' AND d.day=current_date",
      ],
      { cwd: '..', encoding: 'utf8' },
    )
      .trim()
      .split('|')
  await expect
    .poll(async () => {
      await page.getByRole('button', { name: 'Refresh', exact: true }).click()
      const row = page
        .getByTestId('usage-table')
        .getByRole('row')
        .filter({ hasText: 'chat-model' })
      if ((await row.count()) !== 1) return false
      const cells = await row.getByRole('cell').allTextContents()
      return database().every(
        (value, index) => cells[index + 1].replaceAll(',', '') === value,
      )
    })
    .toBe(true)
  await page.getByRole('button', { name: 'This month', exact: true }).click()
  await expect(page.getByTestId('usage-table')).toBeVisible()
  await expect(page.getByRole('img', { name: /^Tokens per day/ })).toBeVisible()
  await page.getByRole('link', { name: 'My requests', exact: true }).click()
  await expect(
    page
      .getByTestId('requests-table')
      .getByRole('row')
      .filter({ hasText: 'chat-model' })
      .first(),
  ).toBeVisible()
  await expect(
    page.getByRole('link', { name: 'Admin', exact: true }),
  ).toHaveCount(0)
  expect((await context.request.get('/portal/api/admin/users')).status()).toBe(
    403,
  )
  await page.getByRole('link', { name: 'Usage', exact: true }).click()
  // Deleting the activity records leaves the daily totals and quota as they are.
  const totals = database()
  await page
    .getByRole('button', { name: 'Delete my activity', exact: true })
    .click()
  await page.getByRole('button', { name: 'Keep records', exact: true }).click()
  await page
    .getByRole('button', { name: 'Delete my activity', exact: true })
    .click()
  await page
    .getByRole('button', { name: 'Delete records', exact: true })
    .click()
  await expect(page.getByText(/^Deleted \d+ requests?\.$/)).toBeVisible()
  expect(database()).toEqual(totals)

  await page.getByRole('button', { name: 'Switch to dark theme' }).click()
  await expect(page.locator('html')).toHaveClass('dark')
  await page.setViewportSize({ width: 390, height: 844 })
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true)
  await page.getByRole('link', { name: 'API keys', exact: true }).click()
  await page
    .getByRole('button', { name: `Revoke ${label}`, exact: true })
    .click()
  await page.getByRole('button', { name: 'Keep key', exact: true }).click()
  await page
    .getByRole('button', { name: `Revoke ${label}`, exact: true })
    .click()
  await page.getByRole('button', { name: 'Revoke key', exact: true }).click()
  // The modal hides the table from the accessibility tree; wait for the row.
  await expect(
    page.getByRole('button', { name: `Remove ${label}`, exact: true }),
  ).toBeVisible()
  const denied = await context.request.post('/v1/chat/completions', {
    headers: { Authorization: `Bearer ${key}` },
    data: { model: 'chat-model', messages: [] },
  })
  expect(denied.status()).toBe(401)
  await page
    .getByRole('button', { name: `Remove ${label}`, exact: true })
    .click()
  await page.getByRole('button', { name: 'Remove key', exact: true }).click()
  await expect(page.getByText(label, { exact: true })).toHaveCount(0)
  expect(violations).toEqual([])
  expect(errors).toEqual([])
  await page.getByRole('button', { name: 'Sign out', exact: true }).click()
  await expect(page.getByRole('button', { name: /logout/i })).toBeVisible()
  await page.getByRole('button', { name: /logout/i }).click()
  await page.goto('/portal/keys')
  await expect(
    page.getByRole('button', { name: 'Continue with Keycloak' }),
  ).toBeVisible()
})

test('development realm admin is identified as a Keycloak account', async ({
  page,
  context,
}) => {
  await page.goto('/')
  await page.getByRole('link', { name: 'Sign in', exact: true }).click()
  await page.getByRole('button', { name: 'Continue with Keycloak' }).click()
  await page.getByLabel('Username', { exact: true }).fill('admin')
  await page.getByLabel('Password', { exact: true }).fill('admin-dev-only')
  await page.getByRole('button', { name: 'Sign In', exact: true }).click()
  await expect(
    page.getByRole('link', { name: 'Admin', exact: true }),
  ).toBeVisible()
  await expect(
    page.getByText('Keycloak account', { exact: true }),
  ).toBeVisible()
  await expect(
    page.getByText('Protected local administrator', { exact: true }),
  ).toHaveCount(0)
  await page.goto('/portal/login/local')
  await expect(
    page.getByRole('heading', {
      name: 'Sign in with a local account',
      exact: true,
    }),
  ).toBeVisible()
  await expect(page.getByLabel('Local username', { exact: true })).toBeVisible()
  expect(
    (
      await context.request.post('/portal/logout', {
        headers: { Origin: new URL(page.url()).origin },
        data: {},
      })
    ).status(),
  ).toBe(200)
})
