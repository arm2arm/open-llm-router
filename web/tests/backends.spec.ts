import { test, expect, type Page } from '@playwright/test'
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
  await dialog
    .getByLabel('Backend name', { exact: true })
    .fill('Physics cluster')
  await dialog
    .getByLabel('IP address or service URL')
    .fill('gpu.cluster:8000/v1')
  await expect(dialog.getByLabel('Backend detection')).toContainText('vLLM')
  await expect(dialog.getByText('2 models discovered')).toBeVisible()
  await expect(
    dialog.getByLabel('Private network', { exact: false }),
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
  await expect(page.getByLabel('Backend access research')).toBeVisible()
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
