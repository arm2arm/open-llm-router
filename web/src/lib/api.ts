import { QueryClient } from '@tanstack/react-query'

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 15_000,
      retry: false,
      refetchOnWindowFocus: true,
    },
  },
})

let redirecting = false
export function redirect(url: string) {
  if (redirecting) return
  redirecting = true
  void queryClient.cancelQueries()
  queryClient.clear()
  window.location.assign(url)
}

export async function api<T>(
  path: string,
  method = 'GET',
  body?: unknown,
): Promise<T> {
  if (redirecting) throw new Error('Redirecting…')
  const response = await fetch(
    path.startsWith('/portal/') ? path : `/portal/api/${path}`,
    {
      method,
      credentials: 'same-origin',
      cache: 'no-store',
      headers: method === 'GET' ? {} : { 'Content-Type': 'application/json' },
      body: method === 'GET' ? undefined : JSON.stringify(body ?? {}),
    },
  )
  if (response.status === 401) {
    redirect('/portal/login')
    throw new Error('Your session has expired. Signing in…')
  }
  if (!response.ok) {
    const error = await response.json().catch(() => null)
    throw new Error(
      error?.error?.message ?? 'The request failed. Please try again.',
    )
  }
  return response.status === 204
    ? (undefined as T)
    : (response.json() as Promise<T>)
}

export type Limits = {
  tokens_per_day: number | null
  tokens_per_month: number | null
  rpm: number | null
  tpm: number | null
  max_concurrent: number | null
  max_concurrent_batch: number | null
  strict_quota: boolean
}
export type Me = {
  user: { id: string; username: string; email: string; groups: string[] }
  limits: Limits
  admin: boolean
}
export type Key = {
  id: string
  prefix: string
  label: string
  created: string
  last_used: string | null
  expires: string | null
  revoked: boolean
  class: 'interactive' | 'batch'
}
export type ModelUsage = {
  model: string
  requests: number
  input_tokens: number
  output_tokens: number
  average_ttfb_ms: number | null
  average_tokens_per_second: number | null
}
export type Usage = {
  retention: { usage_days: number; usage_daily_days: number }
  today: ModelUsage[]
  month: ModelUsage[]
  quota: {
    timezone: string
    limits: Limits
    used: {
      tokens_today: number
      tokens_this_month: number
      rpm: number | null
    }
    remaining: {
      tokens_today: number | null
      tokens_this_month: number | null
      rpm: number | null
      tpm: number | null
    }
    reset_at: { day: string; month: string; rpm: string | null }
    in_flight: number
    in_flight_batch: number
    reserved_tokens: number
  }
}
export type Models = {
  router_url: string
  base_url: string
  models: { id: string; canonical_model: string; protocols: string[] }[]
}
export type RequestRecord = {
  time: string
  model: string
  protocol: string
  status: string
  input_tokens: number
  output_tokens: number
  latency_ms: number
  ttfb_ms: number | null
  stream: boolean
  key_prefix: string | null
  forwarded: boolean
}
export type DailyTotal = {
  day: string
  requests: number
  input_tokens: number
  output_tokens: number
}
export type AdminUser = {
  id: string
  issuer: string
  username: string
  email: string
  groups: string[]
  enabled: boolean
  active_keys: number
  tokens_today: number
  tokens_this_month: number
  overrides: Record<string, number | null>
}
export type Override = {
  field: string
  value: number | null
  reason: string
  set_by: string
  set_at: string
}
export type AdminUserDetail = {
  user: { id: string; username: string; email: string; groups: string[] }
  enabled: boolean
  limits: Limits
  overrides: Override[]
  keys: Key[]
}
export type GroupTotals = {
  group: string
  members: number
  requests_today: number
  tokens_today: number
  tokens_this_month: number
  admin: boolean
}
export type Replica = {
  url: string
  in_flight: number
  healthy: boolean
  weight: number
  max_inflight: number
}
