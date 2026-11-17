import type { Page } from '@playwright/test'

export const permissions = [
  'service:read',
  'usage:read',
  'audit:read',
  'users:read',
  'users:write',
  'routing:read',
  'routing:write',
  'admin_keys:read',
  'admin_keys:write',
  'admin_keys:delegate',
  'policies:read',
  'policies:write',
  'keys:write',
]
export const limits = {
  tokens_per_day: 100000,
  tokens_per_month: null,
  rpm: 60,
  tpm: null,
  max_concurrent: 4,
  max_concurrent_batch: 4,
  strict_quota: false,
}
export const agentKey = {
  id: 'agent-fixture',
  label: 'Example administration key',
  prefix: 'olr-admin-fixture',
  owner_id: 'operator',
  parent_id: null,
  permissions,
  allowed_networks: ['2001:db8:1234:5678::/64'],
  expires: new Date(Date.now() + 30 * 86400000).toISOString(),
  revoked: false,
  active: true,
  last_used: null,
  last_ip: null,
}
const user = {
  id: 'operator',
  username: 'Operator',
  email: 'operator@example.org',
  groups: ['admins'],
  protected_admin: false,
}
const summary = {
  input_messages: 4,
  output_messages: 1,
  tool_calls: 1,
  tool_results: 1,
  tools: 1,
  cached_tokens: 0,
  reasoning_tokens: 0,
  truncated: false,
}
const row = {
  request_id: 'a11y-trace',
  operation: 'chat/completions',
  trace_available: true,
  trace_summary: summary,
  time: new Date().toISOString(),
  model: 'test-model',
  protocol: 'openai',
  status: '200',
  input_tokens: 10,
  output_tokens: 20,
  latency_ms: 100,
  ttfb_ms: 10,
  stream: true,
  key_prefix: 'olr-fixture',
  conversation: 's-a11y',
}
const detail = {
  v: 4,
  operation: 'chat/completions',
  input: [
    { role: 'user', blocks: [{ type: 'text', text: 'A synthetic example' }] },
    {
      role: 'assistant',
      blocks: [
        {
          type: 'tool_call',
          id: 'call-one',
          name: 'weather',
          arguments: '{"location":{"city":"Berlin"}}',
        },
      ],
    },
    {
      role: 'tool',
      blocks: [
        {
          type: 'tool_result',
          id: 'call-one',
          name: 'weather',
          text: '{"temperature":12}',
        },
      ],
    },
  ],
  output: [
    {
      role: 'assistant',
      blocks: [{ type: 'text', text: 'It is twelve degrees.' }],
    },
  ],
  events: [],
  tools: [
    {
      name: 'weather',
      description: 'Read the weather',
      schema: {
        type: 'object',
        properties: {
          location: {
            type: 'object',
            properties: { city: { type: 'string' } },
          },
        },
      },
    },
  ],
  parameters: { temperature: 0.5, settings: { sampling: { top_p: 1 } } },
  request: { model: 'test-model', options: { stream: true } },
  response: {
    choices: [
      { message: { role: 'assistant', content: 'It is twelve degrees.' } },
    ],
  },
  result: {
    finish_reasons: ['stop'],
    usage: { prompt_tokens: 10, completion_tokens: 20 },
  },
  summary,
  warnings: [],
  truncated: false,
}

export async function mockAccessiblePortal(page: Page) {
  let preferences = { available: true, enabled: false, retention_days: 7 }
  const document = {
    models: {},
    aliases: {},
    groups: { staff: { models: ['*'], ...limits } },
    backends: {},
    backend_presets: {},
  }
  await page.route('**/portal/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname.replace(
      '/portal/api/',
      '',
    )
    let body: unknown = {}
    if (path === 'auth/session')
      body = { auth_method: 'local', must_change_password: false }
    else if (path === 'me') body = { user, limits, admin: true }
    else if (path === 'keys') body = { keys: [] }
    else if (path === 'models')
      body = {
        router_url: 'https://router.example',
        base_url: 'https://router.example/v1',
        models: [],
      }
    else if (path === 'usage')
      body = {
        retention: { usage_days: 90, usage_daily_days: 400 },
        today: [],
        month: [],
        quota: {
          timezone: 'UTC',
          limits,
          used: { tokens_today: 0, tokens_this_month: 0, rpm: 0 },
          remaining: {
            tokens_today: 100000,
            tokens_this_month: null,
            rpm: 60,
            tpm: null,
          },
          reset_at: {
            day: new Date(Date.now() + 86400000).toISOString(),
            month: new Date(Date.now() + 30 * 86400000).toISOString(),
            rpm: null,
          },
          in_flight: 0,
          in_flight_batch: 0,
          reserved_tokens: 0,
        },
      }
    else if (path === 'usage/daily')
      body = {
        timezone: 'UTC',
        days: Array.from({ length: 30 }, (_, i) => ({
          day: new Date(Date.UTC(2026, 10, i + 1)).toISOString().slice(0, 10),
          requests: i === 0 ? 0 : 1,
          input_tokens: i === 0 ? 0 : 10,
          output_tokens: i === 0 ? 0 : 20,
        })),
      }
    else if (path === 'requests')
      body = {
        requests: [
          row,
          {
            ...row,
            request_id: 'second-a11y',
            trace_available: false,
            time: new Date(Date.now() - 1000).toISOString(),
          },
        ],
        limit: 200,
        has_more: false,
      }
    else if (path === 'requests/a11y-trace')
      body = {
        request_id: 'a11y-trace',
        expires_at: new Date(Date.now() + 86400000).toISOString(),
        detail,
      }
    else if (path === 'trace-preferences') {
      if (route.request().method() === 'PUT')
        preferences = { ...preferences, ...route.request().postDataJSON() }
      body = preferences
    } else if (path === 'admin/agent-keys')
      body = {
        enabled: true,
        keys: [agentKey],
        permissions,
        source_ip: '2001:db8:1234:5678::1',
        peer_ip: '2001:db8::1',
        proxy_trusted: true,
        address_error: false,
        base_url: 'https://router.example',
        max_lifetime_days: 90,
        default_lifetime_days: 30,
      }
    else if (path === 'admin/users')
      body = {
        users: [
          {
            ...user,
            issuer: 'https://identity.example',
            enabled: true,
            active_keys: 0,
            tokens_today: 0,
            tokens_this_month: 0,
            overrides: {},
          },
        ],
        limit: 500,
        has_more: false,
      }
    else if (path === 'admin/groups') body = { groups: [] }
    else if (path === 'admin/replicas') body = { models: [] }
    else if (path === 'admin/endpoints') body = { managed: true, models: [] }
    else if (path === 'admin/settings/routing')
      body = {
        settings: {
          domain: 'routing',
          managed: true,
          version: 1,
          active: { id: 1, document, created_at: new Date().toISOString() },
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
    else if (path === 'admin/settings/routing/preview')
      body = {
        models: [],
        aliases: {},
        admin: false,
        priority: 0,
        limits,
        affected_users: [],
        overrides: {},
        memberships: ['staff'],
      }
    await route.fulfill({ json: body })
  })
}
