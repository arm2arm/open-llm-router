import { test, expect, type Page } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import type { RoutingDocument, RoutingView } from '../src/lib/api'

// Synthetic API fixture isolates admin interaction from Docker and OIDC.
async function workspace(page: Page) {
  const document: RoutingDocument = {
    models: {},
    aliases: {},
    groups: {
      staff: { models: [], admin: false, priority: 0, strict_quota: false },
      students: {
        models: ['*'],
        admin: false,
        priority: 0,
        strict_quota: false,
      },
      admins: { models: ['*'], admin: true, priority: 0, strict_quota: false },
    },
    backends: {},
    backend_presets: {
      unlimited: {
        name: 'Unlimited',
        rpm: null,
        tpm: null,
        max_concurrent: null,
      },
      standard: { name: 'Standard', rpm: 60, tpm: 60000, max_concurrent: 8 },
    },
  }
  const view: RoutingView = {
    settings: {
      domain: 'routing',
      managed: true,
      version: 1,
      active: {
        id: 1,
        document,
        status: 'active',
        created_at: '2026-01-01T00:00:00Z',
        created_by: 'test',
        origin: 'import',
      },
      effective: [],
      conflicts: [],
      history: [],
    },
    runtime: { revision: 1 },
    credential_references: [],
    allowed_private_networks: [],
    group_grants: {},
    affected_users: [],
    impact_available: false,
    encryption_key_configured: true,
  }
  const saved: {
    document: RoutingDocument
    secrets: Record<string, string>
  }[] = []
  const detections: Record<string, unknown>[] = []
  await page.route('**/portal/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname.replace(
      '/portal/api/',
      '',
    )
    let body: unknown
    if (path === 'auth/session')
      body = { auth_method: 'local', must_change_password: false }
    else if (path === 'me')
      body = {
        user: {
          id: 'test-admin',
          username: 'admin',
          email: '',
          groups: [],
          protected_admin: true,
        },
        limits: {},
        admin: true,
      }
    else if (path === 'admin/settings/routing') body = view
    else if (path === 'admin/settings/routing/draft') {
      const request = route.request().postDataJSON() as (typeof saved)[number]
      request.secrets ??= {}
      saved.push(structuredClone(request))
      const next = structuredClone(request.document)
      for (const id of Object.keys(request.secrets))
        next.backends![id].api_key = {
          stored: '00000000-0000-0000-0000-000000000001',
        }
      view.settings.version++
      view.settings.draft = {
        ...view.settings.active!,
        id: view.settings.version,
        document: next,
        status: 'draft',
        validation: {
          valid: true,
          problems: [],
          checked_at: '2026-01-01T00:00:00Z',
        },
      }
      body = view
    } else if (path === 'admin/settings/routing/activate') {
      view.settings.active = { ...view.settings.draft!, status: 'active' }
      view.runtime.revision = view.settings.active.id
      view.settings.draft = undefined
      view.settings.version++
      body = view
    } else if (path === 'admin/backends/detect') {
      detections.push(route.request().postDataJSON() as Record<string, unknown>)
      body = {
        url: 'http://gpu.cluster:8000',
        models: ['deepseek-r1', 'qwen'],
        provider: 'vLLM',
        health_path: '/v1/models',
      }
    } else if (path === 'admin/replicas') body = { models: [] }
    else if (path === 'admin/groups') body = { groups: [] }
    else if (path.startsWith('admin/users')) body = { users: [] }
    else {
      await route.fulfill({
        status: 503,
        json: { error: { message: 'Unused synthetic API' } },
      })
      return
    }
    await route.fulfill({ json: body })
  })
  return { view, saved, detections }
}

function connectionFixture(view: RoutingView, model = true) {
  const doc = view.settings.active!.document!
  doc.backends!.gpu = {
    name: 'Test GPU',
    url: 'http://gpu.cluster:8000',
    api_key: { env: 'SYNTHETIC_GPU_KEY' },
    private_network: true,
    health_path: '/v1/models',
    groups: ['staff'],
    preset: 'unlimited',
  }
  if (model)
    doc.models.fast = {
      upstream_model: 'deepseek-r1',
      protocols: ['openai', 'anthropic'],
      timeout_s: 30,
      idle_timeout_s: 15,
      queue_timeout_s: 0,
      session_affinity: false,
      priority: null,
      pool: [
        {
          backend: 'gpu',
          url: '',
          health_path: '',
          bearer_token_env: '',
          weight: 1,
          max_inflight: 8,
        },
      ],
    }
}

test('public recipient details save separately from the internal backend name', async ({
  page,
}) => {
  const { view, saved } = await workspace(page)
  connectionFixture(view, false)
  view.settings.active!.document!.backends!.gpu.api_key = {}
  await page.goto('/portal/admin')
  await page
    .getByRole('article', { name: 'Test GPU' })
    .getByRole('button', { name: 'Edit backend', exact: true })
    .click()
  const dialog = page.getByRole('dialog', { name: 'Edit backend' })
  await dialog
    .getByLabel('Public organization', { exact: true })
    .fill('Research Institute')
  await dialog
    .getByLabel('Recipient privacy URL', { exact: true })
    .fill('https://institute.example/privacy')
  await dialog.getByRole('button', { name: 'Save backend' }).click()
  await expect(dialog).toBeHidden()
  expect(saved[0].document.backends!.gpu).toMatchObject({
    name: 'Test GPU',
    url: 'http://gpu.cluster:8000',
    public_organization: 'Research Institute',
    privacy_url: 'https://institute.example/privacy',
  })
  await page
    .getByRole('article', { name: 'Test GPU' })
    .getByRole('button', { name: 'Edit backend', exact: true })
    .click()
  await expect(
    dialog.getByLabel('Public organization', { exact: true }),
  ).toHaveValue('Research Institute')
  await expect(
    dialog.getByLabel('Recipient privacy URL', { exact: true }),
  ).toHaveValue('https://institute.example/privacy')
})

test('model-less backend checks catalog without claiming generation works', async ({
  page,
}) => {
  const { view, detections } = await workspace(page)
  connectionFixture(view, false)
  let inference = 0
  page.on('request', (request) => {
    if (request.url().endsWith('/admin/backends/test')) inference++
  })
  await page.goto('/portal/admin')
  const card = page.getByRole('article', { name: 'Test GPU' })
  await expect(card).toContainText('Needs model selection')
  await card.getByRole('button', { name: 'Test connection' }).click()
  const dialog = page.getByRole('dialog', { name: 'Test connection' })
  await expect(dialog).toContainText('No model is selected')
  await dialog.getByRole('button', { name: 'Check model API' }).click()
  await expect(dialog).toContainText('Connection OK · 2 models discovered')
  await expect(dialog).not.toContainText('Model answered through OLR')
  expect(inference).toBe(0)
  expect(detections).toHaveLength(1)
  expect(detections[0]).toMatchObject({
    id: 'gpu',
    backend: { api_key: { env: 'SYNTHETIC_GPU_KEY' } },
  })
  await dialog.getByRole('button', { name: 'Select models' }).click()
  const editor = page.getByRole('dialog', { name: 'Edit backend' })
  await expect(editor).toBeVisible()
  await expect(editor.getByLabel('Backend detection')).toContainText('vLLM')
  await editor.getByRole('button', { name: 'Save backend' }).click()
  await expect(
    page.getByText('Backend saved. Select a model to enable routing.'),
  ).toBeVisible()
})

test('legacy connection checks keep the approved deployment credential reference', async ({
  page,
}) => {
  const { view } = await workspace(page)
  connectionFixture(view)
  const doc = view.settings.active!.document!
  doc.backends = {}
  doc.models.fast.pool[0] = {
    url: 'http://gpu.cluster:8000',
    health_path: '/v1/models',
    private_network: true,
    bearer_token_env: 'SYNTHETIC_GPU_KEY',
    max_inflight: 8,
    weight: 1,
  }
  let discovery: unknown
  await page.route('**/portal/api/admin/endpoints/models', async (route) => {
    discovery = route.request().postDataJSON()
    await route.fulfill({
      json: {
        models: ['deepseek-r1'],
        replicas: [{ models: ['deepseek-r1'] }],
      },
    })
  })
  await page.route('**/portal/api/admin/backends/test', async (route) => {
    expect(route.request().postDataJSON()).toEqual({
      model: 'fast',
      backend: '',
      url: 'http://gpu.cluster:8000',
      api: 'chat',
    })
    await route.fulfill({
      json: {
        ok: true,
        message: 'Model answered through OLR.',
        latency_ms: 1,
        input_tokens: 4,
        output_tokens: 2,
      },
    })
  })
  await page.goto('/portal/admin')
  await page.getByRole('button', { name: 'Test connection' }).click()
  const dialog = page.getByRole('dialog', { name: 'Test connection' })
  await dialog.getByRole('button', { name: 'Run test' }).click()
  await expect(dialog).toContainText('Connection OK · 1 models discovered')
  await expect(dialog).toContainText('Model answered through OLR.')
  expect(discovery).toEqual({
    model: 'fast',
    replicas: [
      {
        url: 'http://gpu.cluster:8000',
        private_network: true,
        bearer_token_env: 'SYNTHETIC_GPU_KEY',
      },
    ],
  })
})

test('connection diagnostic separates healthy catalog from failed generation and supports native APIs', async ({
  page,
}) => {
  const { view } = await workspace(page)
  connectionFixture(view)
  const attempts: Record<string, string>[] = []
  await page.route('**/portal/api/admin/backends/test', async (route) => {
    const body = route.request().postDataJSON() as Record<string, string>
    attempts.push(body)
    await route.fulfill({
      json: {
        ok: body.api !== 'chat',
        message:
          body.api === 'chat'
            ? 'No healthy replica with available capacity'
            : 'Model answered through OLR.',
        http_status: body.api === 'chat' ? 503 : 200,
        latency_ms: 240,
        input_tokens: 4,
        output_tokens: 2,
        request_id: 'synthetic-diagnostic-id',
      },
    })
  })
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/portal/admin')
  await page.getByRole('button', { name: 'Test connection' }).click()
  const dialog = page.getByRole('dialog', { name: 'Test connection' })
  await expect(dialog.getByLabel('Connection test model')).toHaveValue('fast')
  await dialog.getByRole('button', { name: 'Run test' }).click()
  await expect(dialog).toContainText('Connection OK · 2 models discovered')
  await expect(dialog).toContainText('HTTP 503')
  await expect(dialog).toContainText('No healthy replica')
  for (const api of ['responses', 'messages']) {
    await dialog.getByLabel('Connection test API').selectOption(api)
    await expect(dialog).not.toContainText('HTTP 503')
    await dialog.getByRole('button', { name: 'Run test' }).click()
    await expect(dialog).toContainText('Model answered through OLR.')
    await expect(dialog).toContainText('240 ms · 4 input / 2 output tokens')
    await expect(dialog).toContainText('Request ID: synthetic-diagnostic-id')
  }
  expect(attempts).toEqual(
    ['chat', 'responses', 'messages'].map((api) => ({
      model: 'fast',
      backend: 'gpu',
      api,
    })),
  )
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true)
  const bounds = await dialog.boundingBox()
  expect(bounds!.x).toBeGreaterThanOrEqual(0)
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(390)
})

test('closing a pending connection check cancels requests and clears its result', async ({
  page,
}) => {
  const { view } = await workspace(page)
  connectionFixture(view)
  let release!: () => void
  const pending = new Promise<void>((resolve) => (release = resolve))
  await page.route('**/portal/api/admin/backends/test', async (route) => {
    await pending
    await route.fulfill({
      json: { ok: true, message: 'Stale result', latency_ms: 0 },
    })
  })
  await page.goto('/portal/admin')
  await page.getByRole('button', { name: 'Test connection' }).click()
  const dialog = page.getByRole('dialog', { name: 'Test connection' })
  const started = page.waitForRequest('**/portal/api/admin/backends/test')
  await dialog.getByRole('button', { name: 'Run test' }).click()
  await started
  await expect(dialog.getByRole('button', { name: 'Run test' })).toBeDisabled()
  const aborted = page.waitForEvent('requestfailed', {
    predicate: (request) => request.url().endsWith('/admin/backends/test'),
  })
  await dialog.getByRole('button', { name: 'Cancel test' }).click()
  release()
  await aborted
  await expect(dialog).toHaveCount(0)
  await page.getByRole('button', { name: 'Test connection' }).click()
  await expect(dialog).not.toContainText('Stale result')
  await expect(dialog.getByRole('button', { name: 'Run test' })).toBeEnabled()
})

test('simple editor keeps URL refresh, optional key and models visible', async ({
  page,
}) => {
  const { view, saved, detections } = await workspace(page)
  await page.goto('/portal/admin')
  await page
    .getByRole('button', { name: 'Add backend', exact: true })
    .first()
    .click()
  const dialog = page.getByRole('dialog', { name: 'Add backend', exact: true })
  await expect(
    dialog.getByLabel('Backend name', { exact: true }),
  ).not.toBeVisible()
  await expect(dialog.getByLabel('Backend preset')).not.toBeVisible()
  await expect(dialog.getByLabel('Backend access staff')).not.toBeVisible()
  await dialog
    .getByLabel('IP address or service URL')
    .fill('gpu.cluster:8000/v1')
  await expect(dialog.getByLabel('Backend detection')).toContainText('vLLM')
  const row = dialog.locator('.endpoint-url-row')
  await expect(row.getByLabel('IP address or service URL')).toBeVisible()
  await expect(
    row.getByRole('button', { name: 'Refresh models', exact: true }),
  ).toBeVisible()
  await dialog.getByLabel('qwen', { exact: true }).uncheck()
  const before = detections.length
  await row.getByRole('button', { name: 'Refresh models', exact: true }).click()
  await expect.poll(() => detections.length).toBeGreaterThan(before)
  await expect(dialog.getByLabel('Backend detection')).toContainText('vLLM')
  await expect(dialog.getByLabel('qwen', { exact: true })).not.toBeChecked()
  await dialog
    .getByRole('button', { name: 'Save backend', exact: true })
    .click()
  await expect(dialog).toHaveCount(0)
  expect(Object.values(saved[0].document.backends!)[0]).toMatchObject({
    name: 'gpu.cluster:8000',
    groups: [],
    preset: 'unlimited',
    api_key: {},
  })
  const card = page.getByRole('article', {
    name: 'gpu.cluster:8000',
    exact: true,
  })
  await card.getByRole('button', { name: 'Edit backend' }).click()
  const edit = page.getByRole('dialog', { name: 'Edit backend', exact: true })
  await expect(
    edit.getByLabel('Backend name', { exact: true }),
  ).not.toBeVisible()
  await edit.getByLabel('Use an API key').check()
  await edit.getByLabel('Backend API key').fill('synthetic-optional-key')
  await edit.getByRole('button', { name: 'Save backend', exact: true }).click()
  await expect(edit).toHaveCount(0)
  await card.getByRole('button', { name: 'Edit backend' }).click()
  await expect(edit.getByLabel('Backend API key')).toHaveValue('')
  await edit.getByRole('button', { name: 'Save backend', exact: true }).click()
  await expect(edit).toHaveCount(0)
  expect(
    Object.values(view.settings.active!.document!.backends!)[0].api_key.stored,
  ).toBeTruthy()
  expect(saved[2].secrets).toEqual({})
})

test('firewall-protected HTTP endpoint detects SGLang and saves its canonical origin', async ({
  page,
}) => {
  const { saved } = await workspace(page)
  await page.route('**/portal/api/admin/backends/detect', async (route) => {
    const request = route.request().postDataJSON()
    expect(request.backend.private_network).toBe(true)
    await route.fulfill({
      json: {
        url: 'http://141.33.4.201:8081',
        models: ['deepseek-v4.1-flash-1017'],
        provider: 'SGLang',
        health_path: '/v1/models',
      },
    })
  })
  await page.goto('/portal/admin')
  await page
    .getByRole('button', { name: 'Add backend', exact: true })
    .first()
    .click()
  const dialog = page.getByRole('dialog', { name: 'Add backend', exact: true })
  await dialog.locator('.backend-advanced > summary').click()
  await dialog.getByLabel('Backend name', { exact: true }).fill('AIP DeepSeek')
  await dialog
    .getByLabel('IP address or service URL')
    .fill('http://141.33.4.201:8081/v1')
  await expect(dialog.getByLabel('Backend detection')).toContainText('SGLang')
  await expect(
    dialog.getByLabel('deepseek-v4.1-flash-1017', { exact: true }),
  ).toBeChecked()
  await dialog
    .getByRole('button', { name: 'Save backend', exact: true })
    .click()
  await expect(dialog).toHaveCount(0)
  const backend = Object.values(saved[0].document.backends!)[0]
  expect(backend).toMatchObject({
    url: 'http://141.33.4.201:8081',
    private_network: true,
    provider: 'SGLang',
    api_key: {},
  })
  expect(
    saved[0].document.models['deepseek-v4.1-flash-1017'].pool,
  ).toHaveLength(1)
})

test('backend-first CRUD, adjacent detection, optional key and group assignments', async ({
  page,
}) => {
  const { view, saved, detections } = await workspace(page)
  await page.goto('/portal/admin')
  await expect(
    page.getByRole('heading', { name: 'Backends', exact: true }),
  ).toBeVisible()
  await expect(page.getByTestId('users-table')).toHaveCount(0)
  await page
    .getByRole('button', { name: 'Add backend', exact: true })
    .first()
    .click()
  const dialog = page.getByRole('dialog', { name: 'Add backend', exact: true })
  await dialog.locator('.backend-advanced > summary').click()
  await dialog
    .getByLabel('Backend name', { exact: true })
    .fill('Physics cluster')
  await dialog
    .getByLabel('IP address or service URL')
    .fill('gpu.cluster:8000/v1')
  await expect(dialog.getByLabel('Backend detection')).toContainText('vLLM')
  await expect(dialog.getByText('2 models discovered')).toBeVisible()
  await expect(
    dialog.getByLabel('Internal network', { exact: false }),
  ).toBeChecked()
  await expect(dialog.getByLabel('Backend preset')).toHaveValue('unlimited')
  await dialog.getByLabel('Backend access staff').check()
  await dialog
    .getByRole('button', { name: 'Save backend', exact: true })
    .click()
  const card = page.getByRole('article', {
    name: 'Physics cluster',
    exact: true,
  })
  await expect(card).toBeVisible()
  const [id] = Object.keys(view.settings.active!.document!.backends!)
  expect(saved[0].document.backends![id]).toMatchObject({
    name: 'Physics cluster',
    url: 'http://gpu.cluster:8000',
    private_network: true,
    groups: ['staff'],
    preset: 'unlimited',
    api_key: {},
  })
  expect(saved[0].document.models['deepseek-r1'].pool[0].backend).toBe(id)
  expect(saved[0].document.groups.staff.models).toContain('deepseek-r1')
  expect(saved[0].secrets).toEqual({})
  const legacy = view.settings.active!.document!
  legacy.backends![id].groups = null
  legacy.models['friendly-route'] = legacy.models['deepseek-r1']
  delete legacy.models['deepseek-r1']
  legacy.groups.staff.models = ['friendly-route']
  await page.reload()
  await card.getByRole('button', { name: 'Edit backend' }).click()
  const edit = page.getByRole('dialog', { name: 'Edit backend', exact: true })
  await edit.locator('.backend-advanced > summary').click()
  await expect(edit.getByLabel('Backend access staff')).toBeChecked()
  await edit.getByLabel('Use an API key').check()
  await edit.getByLabel('Backend API key').fill('synthetic-cluster-key')
  await edit
    .getByLabel('Backend name', { exact: true })
    .fill('Research cluster')
  await edit.getByLabel('Backend access students').check()
  await edit.getByLabel('IP address or service URL').fill('gpu.cluster:8000/v1')
  await expect(edit.getByLabel('deepseek-r1', { exact: true })).toBeChecked()
  await edit.getByRole('button', { name: 'Save backend', exact: true }).click()
  await expect(
    page.getByRole('article', { name: 'Research cluster' }),
  ).toBeVisible()
  expect(saved[1].secrets).toEqual({ [id]: 'synthetic-cluster-key' })
  expect(JSON.stringify(saved[1].document)).not.toContain(
    'synthetic-cluster-key',
  )
  expect(detections.length).toBeGreaterThan(0)
  expect(
    await page.evaluate(() =>
      JSON.stringify({ ...localStorage, ...sessionStorage }),
    ),
  ).not.toContain('synthetic-cluster-key')
  await page.getByRole('button', { name: 'Remove Research cluster' }).click()
  await page
    .getByRole('alertdialog')
    .getByRole('button', { name: 'Remove backend', exact: true })
    .click()
  await expect(page.getByRole('alertdialog')).toHaveCount(0)
  await expect(
    page.getByRole('article', { name: 'Research cluster' }),
  ).toHaveCount(0)
  expect(view.settings.active!.document!.models).toEqual({})
  expect(view.settings.active!.document!.groups.staff.models).toEqual([])
})

test('separate editable presets and a usable narrow-screen connection dialog', async ({
  page,
}) => {
  const { view } = await workspace(page)
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/portal/admin/presets')
  await page.getByRole('button', { name: 'Create preset', exact: true }).click()
  const preset = page.getByRole('dialog', {
    name: 'Create preset',
    exact: true,
  })
  await preset.getByLabel('Preset name').fill('Teaching lab')
  await preset.getByLabel('Requests per minute').fill('12')
  await preset.getByLabel('Concurrent requests').fill('2')
  await preset.getByRole('button', { name: 'Save preset' }).click()
  const card = page.getByRole('article', { name: 'Teaching lab', exact: true })
  await expect(card).toContainText('Unlimited')
  await card.getByRole('button', { name: 'Edit preset' }).click()
  await page.getByLabel('Requests per minute').fill('15')
  await page.getByRole('button', { name: 'Save preset' }).click()
  await expect(card).toContainText('15')
  await page.getByRole('link', { name: 'Backends', exact: true }).click()
  await page
    .getByRole('button', { name: 'Add backend', exact: true })
    .first()
    .click()
  const dialog = page.getByRole('dialog', { name: 'Add backend', exact: true })
  await dialog.locator('.backend-advanced > summary').click()
  await dialog.getByLabel('IP address or service URL').fill('10.0.0.12:8000')
  await expect(dialog.getByLabel('Backend detection')).toContainText('vLLM')
  const [id] = Object.entries(
    view.settings.active!.document!.backend_presets!,
  ).find(([, value]) => value.name === 'Teaching lab')!
  await dialog.getByLabel('Backend preset').selectOption(id)
  await expect
    .poll(() =>
      page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),
    )
    .toBe(true)
  await dialog.getByRole('button', { name: 'Save backend' }).click()
  await expect(dialog).toHaveCount(0)
  await page.getByRole('link', { name: 'Backend presets', exact: true }).click()
  await expect(
    page.getByRole('button', { name: 'Remove preset Teaching lab' }),
  ).toBeDisabled()
  await page.getByRole('link', { name: 'Backends', exact: true }).click()
  await page
    .getByRole('button', { name: 'Add backend', exact: true })
    .first()
    .click()
  await page.getByRole('dialog').locator('.backend-advanced > summary').click()
  await page.getByLabel('Backend name', { exact: true }).fill('Unsaved')
  await page.getByRole('dialog').getByRole('button', { name: 'Cancel' }).click()
  await expect(
    page.getByRole('alertdialog', { name: 'Discard backend changes?' }),
  ).toBeVisible()
  await page
    .getByRole('button', { name: 'Discard changes', exact: true })
    .click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
})

test('group policies live in user management and become available to backends', async ({
  page,
}) => {
  const { view } = await workspace(page)
  await page.goto('/portal/admin/users')
  await page.getByRole('button', { name: 'Manage group policies' }).click()
  const dialog = page.getByRole('dialog', { name: 'Manage group policies' })
  await dialog.getByLabel('New group name').fill('research')
  await dialog.getByRole('button', { name: 'Add group', exact: true }).click()
  await dialog
    .getByRole('button', { name: 'Save group policies', exact: true })
    .click()
  await expect(dialog).toHaveCount(0)
  expect(view.settings.active!.document!.groups.research.models).toEqual([])
  await page.getByRole('link', { name: 'Backends', exact: true }).click()
  await page
    .getByRole('button', { name: 'Add backend', exact: true })
    .first()
    .click()
  await page.getByRole('dialog').locator('.backend-advanced > summary').click()
  await expect(page.getByLabel('Backend access research')).toBeVisible()
})

test('received Keycloak groups are visible and can become policies without implicit grants', async ({
  page,
}) => {
  const { view, saved } = await workspace(page)
  await page.route('**/portal/api/admin/groups', (route) =>
    route.fulfill({
      json: {
        groups: ['ErUM', 'erum', 'staff'].map((group) => ({
          group,
          configured: Object.hasOwn(
            view.settings.active!.document!.groups,
            group,
          ),
          members: 1,
          requests_today: 0,
          tokens_today: 0,
          tokens_this_month: 0,
          admin: false,
        })),
      },
    }),
  )
  await page.goto('/portal/admin/users')
  const groups = page.getByRole('region', { name: 'Group totals' })
  const erum = groups.getByRole('row').filter({
    has: page.getByRole('cell', { name: 'ErUM', exact: true }),
  })
  await expect(erum).toContainText('No policy')
  await expect(
    groups.getByRole('cell', { name: 'erum', exact: true }),
  ).toBeVisible()
  await page
    .getByRole('button', { name: 'Create local user', exact: true })
    .click()
  let dialog = page.getByRole('dialog')
  await expect(
    dialog.getByRole('checkbox', { name: 'ErUM', exact: true }),
  ).toHaveCount(0)
  await expect(
    dialog.getByRole('checkbox', { name: 'staff', exact: true }),
  ).toBeVisible()
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()

  await erum
    .getByRole('button', { name: 'Configure policy for ErUM', exact: true })
    .click()
  dialog = page.getByRole('dialog', {
    name: 'Configure ErUM policy',
    exact: true,
  })
  await expect(
    dialog.getByRole('checkbox', { name: 'ErUM grant *', exact: true }),
  ).not.toBeChecked()
  await expect(
    dialog.getByRole('checkbox', { name: 'ErUM administrator', exact: true }),
  ).not.toBeChecked()
  expect(saved).toHaveLength(0)
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
  await page
    .getByRole('button', { name: 'Discard changes', exact: true })
    .click()
  expect(saved).toHaveLength(0)

  await erum
    .getByRole('button', { name: 'Configure policy for ErUM', exact: true })
    .click()
  await dialog
    .getByRole('button', { name: 'Save group policies', exact: true })
    .click()
  await expect(dialog).toHaveCount(0)
  await expect(erum).toContainText('Configured')
  expect(view.settings.active!.document!.groups.ErUM).toEqual({
    models: [],
    admin: false,
    priority: 0,
    strict_quota: false,
  })
  expect(view.settings.active!.document!.groups).not.toHaveProperty('erum')
  await page.getByRole('link', { name: 'Backends', exact: true }).click()
  await page
    .getByRole('button', { name: 'Add backend', exact: true })
    .first()
    .click()
  await page.getByRole('dialog').locator('.backend-advanced > summary').click()
  await expect(
    page.getByLabel('Backend access ErUM', { exact: true }),
  ).toBeVisible()
})

test('group refresh reloads stored memberships and explains missing claims', async ({
  page,
}) => {
  await workspace(page)
  let received = false
  await page.route('**/portal/api/admin/groups', (route) =>
    route.fulfill({
      json: {
        groups: received
          ? [
              {
                group: 'ErUM',
                configured: false,
                members: 1,
                admin: false,
                requests_today: 0,
                tokens_today: 0,
                tokens_this_month: 0,
              },
            ]
          : [],
      },
    }),
  )
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/portal/admin/users')
  const groups = page.getByRole('region', { name: 'Group totals' })
  await expect(
    groups.getByRole('cell', { name: 'ErUM', exact: true }),
  ).toHaveCount(0)
  await groups.getByText('Missing a Keycloak group?', { exact: true }).click()
  await expect(groups).toContainText('ID token')
  await expect(groups).toContainText('signs out and signs in again')
  await expect(
    groups.getByRole('link', { name: 'Keycloak settings' }),
  ).toHaveAttribute('href', '/portal/admin/keycloak')
  received = true
  await groups
    .getByRole('button', { name: 'Refresh memberships', exact: true })
    .click()
  await expect(
    groups.getByRole('cell', { name: 'ErUM', exact: true }),
  ).toBeVisible()
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
})

test('pending advanced drafts block direct edits and save errors preserve input', async ({
  page,
}) => {
  const { view } = await workspace(page)
  view.settings.draft = { ...view.settings.active!, id: 2, status: 'draft' }
  await page.goto('/portal/admin')
  await expect(
    page.getByRole('button', { name: 'Add backend', exact: true }).first(),
  ).toBeDisabled()
  await expect(
    page.getByText('Advanced routing has pending changes.', { exact: false }),
  ).toBeVisible()
  view.settings.draft = undefined
  await page.reload()
  await page
    .getByRole('button', { name: 'Add backend', exact: true })
    .first()
    .click()
  const dialog = page.getByRole('dialog', { name: 'Add backend', exact: true })
  await dialog.locator('.backend-advanced > summary').click()
  await dialog.getByLabel('Backend name', { exact: true }).fill('Research')
  await dialog.getByLabel('IP address or service URL').fill('gpu.cluster:8000')
  await expect(dialog.getByLabel('Backend detection')).toContainText('vLLM')
  await page.route('**/portal/api/admin/settings/routing/draft', (route) =>
    route.fulfill({
      status: 409,
      json: {
        error: { message: 'Settings changed; reload the latest version.' },
      },
    }),
  )
  await dialog
    .getByRole('button', { name: 'Save backend', exact: true })
    .click()
  await expect(dialog.getByRole('alert')).toContainText('Settings changed')
  await expect(dialog.getByLabel('Backend name', { exact: true })).toHaveValue(
    'Research',
  )
  await expect(dialog.getByLabel('IP address or service URL')).toHaveValue(
    'gpu.cluster:8000',
  )
})

test('clone preserves configuration and model selection under a unique new name', async ({
  page,
}) => {
  const { view, saved } = await workspace(page)
  connectionFixture(view)
  const document = view.settings.active!.document!
  const original = document.backends!.gpu
  original.api_key = {}
  original.provider = 'vLLM'
  original.preset = 'standard'
  original.health_path = '/health'
  document.models.fast.pool[0].weight = 3
  document.models.fast.pool[0].max_inflight = 5
  document.models['deepseek-r1'] = structuredClone(document.models.fast)
  document.backends!.existingCopy = {
    ...structuredClone(original),
    name: 'Test GPU (copy)',
  }
  const before = structuredClone(original)
  await page.goto('/portal/admin')
  const card = page.getByRole('article', { name: 'Test GPU', exact: true })
  await card
    .getByRole('button', { name: 'Clone Test GPU', exact: true })
    .click()
  let dialog = page.getByRole('dialog', { name: 'Clone backend', exact: true })
  await expect(dialog.getByLabel('Backend name', { exact: true })).toHaveValue(
    'Test GPU (copy 2)',
  )
  await expect(dialog.getByLabel('IP address or service URL')).toHaveValue(
    original.url,
  )
  await expect(dialog.getByLabel('Backend access staff')).toBeChecked()
  await expect(dialog.getByLabel('Backend preset')).toHaveValue('standard')
  await expect(dialog.getByLabel('deepseek-r1', { exact: true })).toBeChecked()
  await expect(dialog.getByLabel('qwen', { exact: true })).not.toBeChecked()
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
  await expect(dialog).toHaveCount(0)
  expect(saved).toHaveLength(0)
  expect(document.backends!.gpu).toEqual(before)
  await card
    .getByRole('button', { name: 'Clone Test GPU', exact: true })
    .click()
  dialog = page.getByRole('dialog', { name: 'Clone backend', exact: true })
  await dialog
    .getByRole('button', { name: 'Save backend', exact: true })
    .click()
  await expect(dialog).toHaveCount(0)
  expect(saved).toHaveLength(1)
  const [id, clone] = Object.entries(saved[0].document.backends!).find(
    ([id]) => !['gpu', 'existingCopy'].includes(id),
  )!
  expect(clone).toEqual({ ...before, name: 'Test GPU (copy 2)' })
  expect(saved[0].document.backends!.gpu).toEqual(before)
  expect(saved[0].secrets).toEqual({})
  expect(saved[0].document.models.fast).toEqual(document.models.fast)
  const copiedRoute = Object.values(saved[0].document.models).find((model) =>
    model.pool.some((replica) => replica.backend === id),
  )!
  expect(saved[0].document.models['Test GPU (copy 2)/deepseek-r1']).toEqual(
    copiedRoute,
  )
  expect(copiedRoute).toMatchObject({
    upstream_model: 'deepseek-r1',
    protocols: document.models.fast.protocols,
    timeout_s: document.models.fast.timeout_s,
    idle_timeout_s: document.models.fast.idle_timeout_s,
    pool: [{ backend: id, bearer_token_env: '', weight: 3, max_inflight: 5 }],
  })
  await expect(
    page.getByRole('article', { name: 'Test GPU (copy 2)', exact: true }),
  ).toBeVisible()
})

for (const legacy of [false, true]) {
  test(`cloning ${legacy ? 'deployment' : 'stored'} credentials requires a new key and preserves the source`, async ({
    page,
  }) => {
    const { view, saved, detections } = await workspace(page)
    connectionFixture(view)
    const document = view.settings.active!.document!
    let cardName = 'Test GPU'
    if (legacy) {
      const pool = document.models.fast.pool[0]
      delete pool.backend
      pool.url = 'http://gpu.cluster:8000'
      pool.health_path = '/health'
      pool.bearer_token_env = 'SYNTHETIC_GPU_KEY'
      document.backends = {}
      document.groups.staff.models = ['fast']
      cardName = 'gpu.cluster:8000'
    } else {
      document.backends!.gpu.api_key = {
        stored: '00000000-0000-0000-0000-000000000001',
      }
    }
    const before = structuredClone(document)
    await page.goto('/portal/admin')
    await page
      .getByRole('article', { name: cardName, exact: true })
      .getByRole('button', { name: 'Clone ' + cardName, exact: true })
      .click()
    const dialog = page.getByRole('dialog', {
      name: 'Clone backend',
      exact: true,
    })
    await expect(dialog.getByLabel('Use an API key')).toBeChecked()
    await expect(dialog.getByLabel('Backend API key')).toHaveValue('')
    await expect(
      dialog.getByText('Enter the API key for this copy.', { exact: false }),
    ).toBeVisible()
    await expect(
      dialog.getByRole('button', { name: 'Save backend', exact: true }),
    ).toBeDisabled()
    expect(saved).toHaveLength(0)
    await dialog.getByLabel('Backend API key').fill('synthetic-clone-key')
    await dialog
      .getByRole('button', { name: 'Save backend', exact: true })
      .click()
    await expect(dialog).toHaveCount(0)
    expect(saved).toHaveLength(1)
    const id = Object.keys(saved[0].secrets)[0]
    expect(saved[0].secrets).toEqual({ [id]: 'synthetic-clone-key' })
    expect(saved[0].document.backends![id].api_key).toEqual({})
    expect(saved[0].document.models.fast.pool[0]).toEqual(
      before.models.fast.pool[0],
    )
    expect(saved[0].document.models.fast).toEqual(before.models.fast)
    const copiedRoute = Object.values(saved[0].document.models).find((model) =>
      model.pool.some((replica) => replica.backend === id),
    )!
    expect(copiedRoute).toMatchObject({
      protocols: before.models.fast.protocols,
      timeout_s: before.models.fast.timeout_s,
      pool: [{ backend: id, bearer_token_env: '' }],
    })
    if (!legacy)
      expect(saved[0].document.backends!.gpu).toEqual(before.backends!.gpu)
    for (const detection of detections.filter((request) => request.id === id)) {
      expect((detection.backend as { api_key: unknown }).api_key).toEqual({})
    }
  })
}

test('replica backend names update shared labels without changing IDs or credentials', async ({
  page,
}) => {
  await page.setViewportSize({ width: 360, height: 780 })
  const { view, saved } = await workspace(page)
  connectionFixture(view)
  const document = view.settings.active!.document!
  document.backends!.gpu.api_key = {
    stored: '00000000-0000-0000-0000-000000000001',
  }
  document.backends!.other = {
    ...structuredClone(document.backends!.gpu),
    name: 'Other GPU',
    url: 'http://gpu2.cluster:8000',
    api_key: {},
  }
  document.models.shared = structuredClone(document.models.fast)
  document.models.fast.pool.push({
    ...structuredClone(document.models.fast.pool[0]),
    backend: 'other',
  })
  const before = structuredClone(document)
  await page.goto('/portal/admin/advanced')
  const name = page.getByLabel('fast replica 1 backend name', { exact: true })
  await expect(name).toHaveValue('Test GPU')
  await name.fill('AIP fast1')
  expect(
    await page.evaluate(() => window.document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(360)
  await expect(
    page.getByLabel('shared replica 1 backend name', { exact: true }),
  ).toHaveValue('AIP fast1')
  const selector = page.getByRole('combobox', {
    name: 'fast replica 1 backend',
    exact: true,
  })
  await expect(selector).toHaveValue('gpu')
  await expect(selector.locator('option:checked')).toHaveText(
    'AIP fast1 — http://gpu.cluster:8000',
  )
  await expect(
    page.getByLabel('fast replica 2 backend name', { exact: true }),
  ).toHaveValue('Other GPU')
  await name.fill('')
  await expect(selector.locator('option:checked')).toHaveText(
    'gpu — http://gpu.cluster:8000',
  )
  await name.fill('AIP fast1')
  await page
    .getByRole('button', { name: 'Save routing draft', exact: true })
    .click()
  await expect.poll(() => saved.length).toBe(1)
  expect(saved[0].document.backends!.gpu).toEqual({
    ...before.backends!.gpu,
    name: 'AIP fast1',
  })
  expect(saved[0].document.backends!.other).toEqual(before.backends!.other)
  expect(saved[0].document.models).toEqual(before.models)
  expect(Object.keys(saved[0].document.backends!).sort()).toEqual(
    Object.keys(before.backends!).sort(),
  )
  expect(saved[0].secrets).toEqual({})
})

test('model renaming replaces generated names and preserves grants, aliases and replicas', async ({
  page,
}) => {
  const { view, saved } = await workspace(page)
  connectionFixture(view)
  const doc = view.settings.active!.document!
  const generated = 'backend-94683a76/qwen3.8-27b-bf16-1m'
  doc.models[generated] = {
    ...structuredClone(doc.models.fast),
    upstream_model: '',
  }
  doc.groups.staff.models = [generated]
  doc.aliases['aip-chat'] = generated
  const before = structuredClone(doc)
  await page.goto('/portal/admin/advanced')
  const model = page.getByRole('article', {
    name: `Model ${generated}`,
    exact: true,
  })
  const input = model.getByLabel(`${generated} model name`, { exact: true })
  await input.fill('fast')
  await model.getByRole('button', { name: 'Rename model' }).click()
  await expect(model.getByRole('alert')).toContainText('already used')
  await input.fill('aip-chat')
  await model.getByRole('button', { name: 'Rename model' }).click()
  await expect(model.getByRole('alert')).toContainText('already used')
  await input.fill('aip-fast')
  await model.getByRole('button', { name: 'Rename model' }).click()
  await expect(
    page.getByRole('article', { name: 'Model aip-fast', exact: true }),
  ).toBeVisible()
  await page
    .getByRole('button', { name: 'Save routing draft', exact: true })
    .click()
  await expect.poll(() => saved.length).toBe(1)
  const result = saved[0].document
  expect(result.models[generated]).toBeUndefined()
  expect(result.models['aip-fast']).toEqual({
    ...before.models[generated],
    upstream_model: generated,
  })
  expect(result.groups.staff.models).toEqual(['aip-fast'])
  expect(result.aliases['aip-chat']).toBe('aip-fast')
  expect(result.models.fast).toEqual(before.models.fast)
  expect(result.backends).toEqual(before.backends)
})
