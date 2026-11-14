import { test, expect } from '@playwright/test'
import type { RoutingDocument, RoutingView } from '../src/lib/api'

// The UI contract is isolated from identity-provider and Docker fixtures.
// All API values and credentials here are public synthetic test data.
test('connect a backend before creating routes and keep keys out of documents', async ({
  page,
}) => {
  const document: RoutingDocument = {
    models: {
      baseline: {
        upstream_model: '',
        protocols: ['openai'],
        timeout_s: 30,
        idle_timeout_s: 30,
        queue_timeout_s: 0,
        session_affinity: false,
        priority: null,
        pool: [
          {
            url: 'http://baseline:8000',
            health_path: '/health',
            weight: 1,
            max_inflight: 8,
            bearer_token_env: '',
          },
        ],
      },
    },
    groups: {
      staff: {
        models: ['baseline'],
        admin: false,
        priority: 0,
        strict_quota: false,
      },
    },
    aliases: {},
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
    group_grants: { staff: ['baseline'] },
    affected_users: [],
    impact_available: false,
    encryption_key_configured: true,
  }
  let saved:
    { document: RoutingDocument; secrets: Record<string, string> } | undefined
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
      saved = route.request().postDataJSON() as typeof saved
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
        json: { error: { message: 'Unused API in isolated UI fixture' } },
      })
      return
    }
    await route.fulfill({ json: body })
  })
  await page.goto('/portal/admin')
  const panel = page.getByRole('region', { name: 'Backends', exact: true })
  await panel.getByLabel('Backend name', { exact: true }).fill('gpu')
  await panel
    .getByLabel('Backend API key', { exact: true })
    .fill('synthetic-cluster-key')
  await panel.getByLabel('Backend host URL').fill('http://gpu.cluster:8000/v1')
  await expect(
    panel.getByText('2 model(s) detected', { exact: false }),
  ).toBeVisible()
  await expect(panel.getByText('vLLM', { exact: true })).toBeVisible()
  await expect(
    panel.getByLabel('Private cluster endpoint', { exact: false }),
  ).toBeChecked()
  await panel.getByLabel('Route deepseek-r1', { exact: true }).check()
  await panel.getByLabel('staff', { exact: true }).check()
  await panel
    .getByRole('button', { name: 'Add selected models as routes' })
    .click()
  await expect(
    page.getByRole('article', { name: 'Model deepseek-r1', exact: true }),
  ).toBeVisible()
  await page
    .getByRole('button', { name: 'Save routing draft', exact: true })
    .click()
  await expect.poll(() => saved).toBeDefined()
  expect(saved?.document.backends?.gpu).toEqual({
    url: 'http://gpu.cluster:8000',
    api_key: {},
    private_network: true,
    health_path: '/v1/models',
  })
  expect(saved?.document.models['deepseek-r1'].pool[0].backend).toBe('gpu')
  expect(saved?.document.groups.staff.models).toContain('deepseek-r1')
  expect(saved?.secrets).toEqual({ gpu: 'synthetic-cluster-key' })
  expect(JSON.stringify(saved?.document)).not.toContain('synthetic-cluster-key')
  expect(
    detections.some((body) => body.api_key === 'synthetic-cluster-key'),
  ).toBeTruthy()
  const storage = await page.evaluate(() =>
    JSON.stringify({ ...localStorage, ...sessionStorage }),
  )
  expect(storage).not.toContain('synthetic-cluster-key')
})
