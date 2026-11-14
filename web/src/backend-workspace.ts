import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  api,
  type RoutingDocument,
  type RoutingView,
  type EndpointModel,
  type Replica,
  type BackendPreset,
} from '@/lib/api'

export const defaultPresets: Record<string, BackendPreset> = {
  unlimited: { name: 'Unlimited', rpm: null, tpm: null, max_concurrent: null },
  standard: { name: 'Standard', rpm: 60, tpm: 60000, max_concurrent: 8 },
  small: { name: 'Small cluster', rpm: 20, tpm: 20000, max_concurrent: 2 },
}

export function routingDocument(view: RoutingView): RoutingDocument {
  const effective = (name: string) =>
    view.settings.effective.find((setting) => setting.name === name)?.value ??
    {}
  const document =
    view.settings.active?.document ??
    ({
      models: effective('models'),
      groups: effective('groups'),
      aliases: effective('aliases'),
    } as RoutingDocument)
  return {
    ...document,
    models: document.models ?? {},
    groups: document.groups ?? {},
    aliases: document.aliases ?? {},
    backends: document.backends ?? {},
    backend_presets: document.backend_presets ?? defaultPresets,
  }
}

export function useBackendWorkspace() {
  const cache = useQueryClient()
  const query = useQuery({
    queryKey: ['admin-routing'],
    queryFn: () => api<RoutingView>('admin/settings/routing'),
  })
  const health = useQuery({
    queryKey: ['admin-replicas'],
    queryFn: () =>
      api<{ models: { model: string; replicas: Replica[] }[] }>(
        'admin/replicas',
      ),
    refetchInterval: 10000,
  })
  const [saving, setSaving] = useState(false)
  const document = query.data ? routingDocument(query.data) : null
  async function save(
    next: RoutingDocument,
    secrets: Record<string, string> = {},
    expected?: number,
  ) {
    if (!query.data) throw new Error('Backend settings are still loading.')
    if (expected !== undefined && expected !== query.data.settings.version)
      throw new Error(
        'Settings changed while this editor was open. Close it and reopen to review the latest settings.',
      )
    if (query.data.settings.draft)
      throw new Error(
        'There are pending advanced changes. Review or discard them in Advanced routing before saving a backend.',
      )
    setSaving(true)
    let drafted = false
    try {
      const current = query.data.settings.managed
        ? query.data
        : await api<RoutingView>('admin/settings/routing/import', 'POST')
      const draft = await api<RoutingView>(
        'admin/settings/routing/draft',
        'PUT',
        { version: current.settings.version, document: next, secrets },
      )
      drafted = true
      if (!draft.settings.draft?.validation?.valid)
        throw new Error(
          draft.settings.draft?.validation?.problems.join('\n') ||
            'These settings could not be validated.',
        )
      await api('admin/settings/routing/activate', 'POST', {
        version: draft.settings.version,
        revision: draft.settings.draft.id,
      })
    } catch (error) {
      const message =
        error instanceof Error
          ? error.message
          : 'Backend settings could not be saved.'
      throw new Error(
        drafted
          ? `${message}\nYour changes are saved but are not active. Review pending changes in Advanced routing.`
          : message,
        { cause: error },
      )
    } finally {
      setSaving(false)
      await Promise.all(
        [
          'admin-routing',
          'admin-replicas',
          'admin-endpoints',
          'admin-groups',
          'me',
          'models',
        ].map((name) => cache.invalidateQueries({ queryKey: [name] })),
      )
    }
  }
  return { query, document, health, saving, save }
}

export function backendOrigin(raw: string) {
  const input = raw.trim()
  const parsed = new URL(input.includes('://') ? input : `http://${input}`)
  if (
    !['http:', 'https:'].includes(parsed.protocol) ||
    parsed.username ||
    parsed.password ||
    parsed.search ||
    parsed.hash ||
    ![
      '',
      '/v1',
      '/v1/models',
      '/v1/chat/completions',
      '/v1/messages',
      '/api/tags',
    ].includes(parsed.pathname.replace(/\/+$/, ''))
  )
    throw new Error(
      'Enter an IP address or service host, optionally with a port or /v1 base.',
    )
  return parsed.origin
}

export function removeBackendModels(
  document: RoutingDocument,
  id: string,
  keep: string[] = [],
) {
  const next = structuredClone(document)
  for (const [name, model] of Object.entries(next.models)) {
    if (keep.includes(model.upstream_model || name)) continue
    const pool = model.pool.filter((replica) => replica.backend !== id)
    if (pool.length === model.pool.length) continue
    if (pool.length) next.models[name] = { ...model, pool }
    else {
      delete next.models[name]
      for (const group of Object.values(next.groups))
        group.models = group.models.filter((model) => model !== name)
      for (const [alias, target] of Object.entries(next.aliases))
        if (target === name) delete next.aliases[alias]
    }
  }
  return next
}

export function addBackendModels(
  document: RoutingDocument,
  id: string,
  models: string[],
  groups: string[],
  legacyURL?: string,
) {
  const next = structuredClone(document)
  if (legacyURL)
    for (const model of Object.values(next.models)) {
      model.pool = model.pool.map((replica) =>
        !replica.backend && replica.url === legacyURL
          ? {
              ...replica,
              backend: id,
              bearer_token_env: '',
              max_inflight: 2147483647,
            }
          : replica,
      )
    }
  const current = removeBackendModels(next, id, models)
  for (const upstream of models) {
    let names = Object.keys(current.models).filter(
      (name) =>
        (current.models[name].upstream_model || name) === upstream &&
        current.models[name].pool.some((replica) => replica.backend === id),
    )
    if (!names.length) {
      let name =
        upstream.length <= 128 && !current.models[upstream]
          ? upstream
          : `${id}/${upstream}`.slice(0, 120)
      let suffix = 1
      while (current.models[name])
        name = `${id}/${upstream}`.slice(0, 115) + `-${suffix++}`
      const model: EndpointModel = {
        upstream_model: upstream,
        protocols: ['openai'],
        timeout_s: 120,
        idle_timeout_s: 30,
        queue_timeout_s: 0,
        session_affinity: false,
        priority: null,
        pool: [
          {
            backend: id,
            url: '',
            health_path: '',
            weight: 1,
            max_inflight: 2147483647,
            bearer_token_env: '',
          },
        ],
      }
      current.models[name] = model
      names = [name]
    }
    for (const group of groups)
      if (current.groups[group] && !current.groups[group].models.includes('*'))
        current.groups[group].models = [
          ...new Set([...current.groups[group].models, ...names]),
        ]
  }
  return current
}
