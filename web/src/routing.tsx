import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Network, Plus, Trash2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { GroupPolicies, AccessPreview } from '@/policies'
import { Backends } from '@/backends'
import {
  api,
  type CheckResult,
  type EndpointModel,
  type EndpointReplica,
  type Replica,
  type RoutingDocument,
  type RoutingRevision,
  type RoutingView,
  type Backend,
} from '@/lib/api'

const queryKey = ['admin-routing']

function Failure({ error }: { error: Error | null }) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (error) ref.current?.focus()
  }, [error])
  return error ? (
    <div
      ref={ref}
      role="alert"
      tabIndex={-1}
      className="error whitespace-pre-wrap"
    >
      {error.message}
    </div>
  ) : null
}
const asError = (error: unknown) =>
  error instanceof Error
    ? error
    : new Error('Routing settings could not be updated.')

const newReplica = (): EndpointReplica => ({
  url: '',
  health_path: '/health',
  weight: 1,
  max_inflight: 8,
  bearer_token_env: '',
})
const newModel = (): EndpointModel => ({
  upstream_model: '',
  protocols: ['openai'],
  timeout_s: 120,
  idle_timeout_s: 30,
  queue_timeout_s: 0,
  session_affinity: false,
  priority: null,
  pool: [newReplica()],
})

function copyDocument(document: RoutingDocument): RoutingDocument {
  return JSON.parse(JSON.stringify(document)) as RoutingDocument
}

// grantsOf lists the groups that name a model explicitly or grant all models.
function grantsOf(model: string, grants: Record<string, string[]>) {
  return Object.entries(grants)
    .filter(([, models]) => models.includes(model) || models.includes('*'))
    .map(([group, models]) => (models.includes('*') ? `${group} (*)` : group))
}

function Results({ results }: { results: CheckResult[] }) {
  return (
    <ul className="grid gap-2" aria-label="Endpoint check results">
      {results.map((result, index) => (
        <li
          key={`${result.name}-${index}`}
          className="flex flex-wrap gap-2 items-baseline"
        >
          <span
            className={`badge ${result.status === 'failed' ? 'inactive' : ''}`}
          >
            {result.status === 'passed' ? '✓ passed' : '✕ failed'}
          </span>
          <code className="text-xs">{result.name}</code>
          <span className="subtitle">{result.message}</span>
        </li>
      ))}
    </ul>
  )
}

function ReplicaFields({
  replica,
  index,
  model,
  references,
  backends,
  onChange,
  onRemove,
}: {
  replica: EndpointReplica
  index: number
  model: string
  references: string[]
  backends: Record<string, Backend>
  onChange: (replica: EndpointReplica) => void
  onRemove?: () => void
}) {
  const label = `${model} replica ${index + 1}`
  return (
    <fieldset
      className="grid gap-2 sm:grid-cols-6 items-end rounded-md border p-3"
      aria-label={label}
    >
      <label className="grid gap-1 text-sm sm:col-span-6">
        Backend
        <select
          className="select"
          aria-label={`${label} backend`}
          value={replica.backend ?? ''}
          onChange={(event) =>
            onChange({
              ...replica,
              backend: event.target.value,
              url: '',
              health_path: '',
              bearer_token_env: '',
              private_network: false,
            })
          }
        >
          <option value="">Enter an origin manually</option>
          {Object.entries(backends).map(([id, backend]) => (
            <option key={id} value={id}>
              {id} — {backend.url}
            </option>
          ))}
        </select>
      </label>
      {replica.backend ? (
        <p className="subtitle sm:col-span-2">
          {backends[replica.backend]?.url ?? 'Missing backend'} · uses the
          backend’s API key and network setting
        </p>
      ) : (
        <label className="grid gap-1 text-sm sm:col-span-2">
          Origin URL
          <Input
            aria-label={`${label} origin`}
            value={replica.url}
            required
            maxLength={2048}
            placeholder="http://gpu-node:8000"
            onChange={(event) =>
              onChange({ ...replica, url: event.target.value })
            }
          />
        </label>
      )}
      <label className="grid gap-1 text-sm">
        Health path
        <Input
          aria-label={`${label} health path`}
          value={replica.health_path}
          placeholder={
            replica.backend ? backends[replica.backend]?.health_path : '/health'
          }
          maxLength={256}
          onChange={(event) =>
            onChange({ ...replica, health_path: event.target.value })
          }
        />
      </label>
      <label className="grid gap-1 text-sm">
        Weight
        <Input
          aria-label={`${label} weight`}
          type="number"
          min={0.01}
          step="any"
          value={replica.weight}
          onChange={(event) =>
            onChange({ ...replica, weight: Number(event.target.value) })
          }
        />
      </label>
      <label className="grid gap-1 text-sm">
        Max in flight
        <Input
          aria-label={`${label} max in flight`}
          type="number"
          min={1}
          value={replica.max_inflight}
          onChange={(event) =>
            onChange({ ...replica, max_inflight: Number(event.target.value) })
          }
        />
      </label>
      {!replica.backend && (
        <label className="grid gap-1 text-sm">
          Credential
          <select
            className="select"
            aria-label={`${label} credential reference`}
            value={replica.bearer_token_env}
            onChange={(event) =>
              onChange({ ...replica, bearer_token_env: event.target.value })
            }
          >
            <option value="">None</option>
            {references.map((reference) => (
              <option key={reference} value={reference}>
                {reference}
              </option>
            ))}
            {replica.bearer_token_env &&
              !references.includes(replica.bearer_token_env) && (
                <option value={replica.bearer_token_env}>
                  {replica.bearer_token_env} (not allowed)
                </option>
              )}
          </select>
        </label>
      )}
      {!replica.backend && (
        <label className="flex items-center gap-2 text-sm sm:col-span-6">
          <input
            type="checkbox"
            checked={replica.private_network ?? false}
            onChange={(event) =>
              onChange({ ...replica, private_network: event.target.checked })
            }
          />
          Private cluster endpoint (allows HTTP)
        </label>
      )}
      {onRemove && (
        <Button
          type="button"
          variant="outline"
          aria-label={`Remove ${label}`}
          onClick={onRemove}
        >
          <Trash2 />
        </Button>
      )}
    </fieldset>
  )
}

function ModelEditor({
  name,
  model,
  references,
  backends,
  blockers,
  onChange,
  onRemove,
}: {
  name: string
  model: EndpointModel
  references: string[]
  backends: Record<string, Backend>
  blockers: string[]
  onChange: (model: EndpointModel) => void
  onRemove: () => void
}) {
  const number = (key: keyof EndpointModel) => (
    <label className="grid gap-1 text-sm">
      {
        {
          timeout_s: 'Header timeout (s)',
          idle_timeout_s: 'Idle timeout (s)',
          queue_timeout_s: 'Queue timeout (s)',
        }[key as string]
      }
      <Input
        aria-label={`${name} ${String(key)}`}
        type="number"
        min={0}
        value={Number(model[key] ?? 0)}
        onChange={(event) =>
          onChange({ ...model, [key]: Number(event.target.value) })
        }
      />
    </label>
  )
  return (
    <article
      className="grid gap-3 rounded-md border p-3"
      aria-label={`Model ${name}`}
    >
      <div className="flex flex-wrap gap-2 items-center justify-between">
        <h4 className="font-medium">{name}</h4>
        <Button
          type="button"
          variant="outline"
          disabled={blockers.length > 0}
          aria-label={`Remove model ${name}`}
          title={blockers.join('; ')}
          onClick={onRemove}
        >
          <Trash2 /> Remove model
        </Button>
      </div>
      {blockers.length > 0 && (
        <p className="subtitle">
          Cannot be removed while {blockers.join('; ')}.
        </p>
      )}
      <div className="grid gap-3 sm:grid-cols-4">
        <label className="grid gap-1 text-sm">
          Upstream model name
          <Input
            aria-label={`${name} upstream model`}
            value={model.upstream_model}
            placeholder={name}
            maxLength={256}
            onChange={(event) =>
              onChange({ ...model, upstream_model: event.target.value })
            }
          />
        </label>
        {number('timeout_s')}
        {number('idle_timeout_s')}
        {number('queue_timeout_s')}
      </div>
      <div className="flex flex-wrap gap-4 text-sm">
        {(['openai', 'anthropic'] as const).map((protocol) => (
          <label key={protocol} className="flex gap-2 items-center">
            <input
              type="checkbox"
              className="h-4 w-4"
              aria-label={`${name} ${protocol} protocol`}
              checked={model.protocols.includes(protocol)}
              onChange={(event) =>
                onChange({
                  ...model,
                  protocols: event.target.checked
                    ? [...model.protocols, protocol]
                    : model.protocols.filter((p) => p !== protocol),
                })
              }
            />
            {protocol === 'openai' ? 'OpenAI API' : 'Anthropic API'}
          </label>
        ))}
        <label className="flex gap-2 items-center">
          <input
            type="checkbox"
            className="h-4 w-4"
            aria-label={`${name} session affinity`}
            checked={model.session_affinity}
            onChange={(event) =>
              onChange({ ...model, session_affinity: event.target.checked })
            }
          />
          Session affinity
        </label>
      </div>
      {model.pool.map((replica, index) => (
        <ReplicaFields
          key={index}
          replica={replica}
          index={index}
          model={name}
          references={references}
          backends={backends}
          onChange={(next) =>
            onChange({
              ...model,
              pool: model.pool.map((r, i) => (i === index ? next : r)),
            })
          }
          onRemove={
            model.pool.length > 1
              ? () =>
                  onChange({
                    ...model,
                    pool: model.pool.filter((_, i) => i !== index),
                  })
              : undefined
          }
        />
      ))}
      <div>
        <Button
          type="button"
          variant="outline"
          aria-label={`Add replica to ${name}`}
          onClick={() =>
            onChange({ ...model, pool: [...model.pool, newReplica()] })
          }
        >
          <Plus /> Add replica
        </Button>
      </div>
    </article>
  )
}

function DraftEditor({
  view,
  base,
  onSaved,
}: {
  view: RoutingView
  base: RoutingRevision
  onSaved: () => Promise<void>
}) {
  const [document, setDocument] = useState(() =>
    copyDocument(base.document as RoutingDocument),
  )
  const [modelName, setModelName] = useState('')
  const [secrets, setSecrets] = useState<Record<string, string>>({})
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<Error | null>(null)
  const blockers = (name: string) => {
    const reasons: string[] = []
    const groups = Object.entries(document.groups)
      .map(([name, policy]) => [name, policy.models] as const)
      .filter(([, models]) => models.includes(name))
      .map(([group]) => group)
    if (groups.length)
      reasons.push(
        `group ${groups.join(', ')} grants it (remove its grant first)`,
      )
    const aliases = Object.entries(document.aliases)
      .filter(([, target]) => target === name)
      .map(([alias]) => alias)
    if (aliases.length) reasons.push(`alias ${aliases.join(', ')} names it`)
    return reasons
  }
  async function save() {
    setPending(true)
    setError(null)
    try {
      await api('admin/settings/routing/draft', 'PUT', {
        version: view.settings.version,
        document,
        secrets,
      })
      await onSaved()
    } catch (error) {
      setError(asError(error))
    } finally {
      setPending(false)
    }
  }
  const models = Object.keys(document.models).sort()
  return (
    <form
      aria-label="Edit routing draft"
      className="grid gap-4"
      onSubmit={(event) => {
        event.preventDefault()
        void save()
      }}
    >
      <h3 className="panel-title">
        {view.settings.draft
          ? `Routing draft ${view.settings.draft.id}`
          : 'New routing draft'}
      </h3>
      <p className="subtitle">
        Saving creates a draft; it does not change routing until activated.
        Backend API keys are encrypted when saved.
      </p>
      <Failure error={error} />
      <fieldset disabled={pending} className="grid gap-4 min-w-0">
        <Backends
          document={document}
          secrets={secrets}
          encryption={view.encryption_key_configured}
          onChange={(next, keys) => {
            setDocument(next)
            setSecrets(keys)
          }}
        />
        <h4 className="font-medium">Route settings</h4>
        {models.map((name) => (
          <ModelEditor
            key={name}
            name={name}
            model={document.models[name]}
            references={view.credential_references}
            backends={document.backends ?? {}}
            blockers={blockers(name)}
            onChange={(model) =>
              setDocument({
                ...document,
                models: { ...document.models, [name]: model },
              })
            }
            onRemove={() => {
              const next = copyDocument(document)
              delete next.models[name]
              setDocument(next)
            }}
          />
        ))}
        <div className="flex flex-wrap gap-2 items-end">
          <label className="grid gap-1 text-sm">
            New logical model name
            <Input
              aria-label="New logical model name"
              value={modelName}
              maxLength={128}
              onChange={(event) => setModelName(event.target.value)}
            />
          </label>
          <Button
            type="button"
            variant="outline"
            disabled={!modelName.trim() || modelName.trim() in document.models}
            onClick={() => {
              setDocument({
                ...document,
                models: { ...document.models, [modelName.trim()]: newModel() },
              })
              setModelName('')
            }}
          >
            <Plus /> Add model
          </Button>
        </div>
        <fieldset className="grid gap-2 rounded-md border p-3">
          <legend className="px-1 text-sm">Aliases</legend>
          {Object.entries(document.aliases)
            .sort()
            .map(([alias, target]) => (
              <div key={alias} className="flex flex-wrap gap-2 items-center">
                <code>{alias}</code>→
                <select
                  className="select"
                  aria-label={`Alias ${alias} target`}
                  value={target}
                  onChange={(event) =>
                    setDocument({
                      ...document,
                      aliases: {
                        ...document.aliases,
                        [alias]: event.target.value,
                      },
                    })
                  }
                >
                  {models.map((name) => (
                    <option key={name}>{name}</option>
                  ))}
                  {!models.includes(target) && (
                    <option value={target}>{target} (missing)</option>
                  )}
                </select>
                <Button
                  type="button"
                  variant="outline"
                  aria-label={`Remove alias ${alias}`}
                  onClick={() => {
                    const next = copyDocument(document)
                    delete next.aliases[alias]
                    setDocument(next)
                  }}
                >
                  <Trash2 />
                </Button>
              </div>
            ))}
          <AliasAdder
            models={models}
            onAdd={(alias, target) =>
              setDocument({
                ...document,
                aliases: { ...document.aliases, [alias]: target },
              })
            }
          />
        </fieldset>
        <GroupPolicies document={document} onChange={setDocument} />
        <AccessPreview document={document} />
        <div>
          <Button type="submit">
            {pending ? 'Saving…' : 'Save routing draft'}
          </Button>
        </div>
      </fieldset>
    </form>
  )
}

function AliasAdder({
  models,
  onAdd,
}: {
  models: string[]
  onAdd: (alias: string, target: string) => void
}) {
  const [alias, setAlias] = useState('')
  const [target, setTarget] = useState('')
  return (
    <div className="flex flex-wrap gap-2 items-end">
      <label className="grid gap-1 text-sm">
        New alias
        <Input
          aria-label="New alias"
          value={alias}
          maxLength={128}
          onChange={(event) => setAlias(event.target.value)}
        />
      </label>
      <label className="grid gap-1 text-sm">
        For model
        <select
          className="select"
          aria-label="New alias target"
          value={target}
          onChange={(event) => setTarget(event.target.value)}
        >
          <option value="">Choose a model</option>
          {models.map((name) => (
            <option key={name}>{name}</option>
          ))}
        </select>
      </label>
      <Button
        type="button"
        variant="outline"
        disabled={!alias.trim() || !target}
        onClick={() => {
          onAdd(alias.trim(), target)
          setAlias('')
        }}
      >
        <Plus /> Add alias
      </Button>
    </div>
  )
}

export function RoutingSettingsPanel() {
  const cache = useQueryClient()
  const view = useQuery({
    queryKey,
    queryFn: () => api<RoutingView>('admin/settings/routing'),
  })
  const health = useQuery({
    queryKey: ['admin-replicas'],
    queryFn: () =>
      api<{ models: { model: string; replicas: Replica[] }[] }>(
        'admin/replicas',
      ),
    refetchInterval: 10_000,
  })
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<Error | null>(null)
  const [results, setResults] = useState<CheckResult[] | null>(null)
  const reload = async () => {
    await cache.invalidateQueries({ queryKey })
    await cache.invalidateQueries({ queryKey: ['admin-replicas'] })
    await cache.invalidateQueries({ queryKey: ['admin-endpoints'] })
    await cache.invalidateQueries({ queryKey: ['admin-groups'] })
    await cache.invalidateQueries({ queryKey: ['me'] })
  }
  async function act(path: string, body: object) {
    setPending(true)
    setError(null)
    try {
      await api(`admin/settings/routing/${path}`, 'POST', body)
      await reload()
    } catch (error) {
      setError(asError(error))
    } finally {
      setPending(false)
    }
  }
  async function check(revision: number) {
    setPending(true)
    setError(null)
    try {
      const response = await api<{ results: CheckResult[] }>(
        'admin/settings/routing/checks',
        'POST',
        { revision },
      )
      setResults(response.results)
      await reload()
    } catch (error) {
      setError(asError(error))
    } finally {
      setPending(false)
    }
  }
  const data = view.data
  const settings = data?.settings
  const active = settings?.active
  const draft = settings?.draft
  const effective = (active?.document ?? {
    models: (settings?.effective.find((s) => s.name === 'models')?.value ??
      {}) as Record<string, EndpointModel>,
    aliases: (settings?.effective.find((s) => s.name === 'aliases')?.value ??
      {}) as Record<string, string>,
  }) as RoutingDocument
  const replicaHealth = (model: string, url: string) =>
    health.data?.models
      .find((m) => m.model === model)
      ?.replicas.find((r) => r.url === url.replace(/\/$/, ''))
  return (
    <section className="panel" id="routing" aria-label="Routing settings">
      <div className="panel-head">
        <h2 className="panel-title">Routing</h2>
        <Network size={20} className="text-muted-foreground" />
      </div>
      <Failure error={view.error ?? error} />
      {data && settings && (
        <div className="grid gap-6">
          <p className="subtitle">
            A request names a model or alias. Group policies grant models; each
            model forwards its native OpenAI or Anthropic API to a pool of
            compatible backends. Connect a backend to discover models and create
            routes.
          </p>
          <dl className="grid gap-2 sm:grid-cols-2 text-sm">
            <div>
              <dt className="subtitle">Configuration authority</dt>
              <dd>
                {settings.managed
                  ? `Database — active routing revision ${active?.id}`
                  : 'Config file (and per-model endpoint revisions)'}
              </dd>
            </div>
            <div>
              <dt className="subtitle">This router is running</dt>
              <dd data-testid="routing-runtime">
                {data.runtime.revision
                  ? `routing revision ${data.runtime.revision}`
                  : 'the file routing'}
              </dd>
            </div>
          </dl>
          <div className="table-wrap">
            <table aria-label="Routed models">
              <thead>
                <tr>
                  <th>Model</th>
                  <th>Granted by</th>
                  <th>Aliases</th>
                  <th>Endpoints</th>
                </tr>
              </thead>
              <tbody>
                {Object.entries(effective.models)
                  .sort()
                  .map(([name, model]) => (
                    <tr key={name}>
                      <td className="font-medium">
                        {name}
                        <div className="subtitle">
                          {model.protocols.join(', ')}
                          {model.upstream_model
                            ? ` → ${model.upstream_model}`
                            : ''}
                        </div>
                      </td>
                      <td>
                        {grantsOf(name, data.group_grants).join(', ') || '—'}
                      </td>
                      <td>
                        {Object.entries(effective.aliases)
                          .filter(([, target]) => target === name)
                          .map(([alias]) => alias)
                          .join(', ') || '—'}
                      </td>
                      <td>
                        <ul className="grid gap-1">
                          {model.pool.map((replica) => {
                            const url = replica.backend
                              ? (effective.backends?.[replica.backend]?.url ??
                                replica.url)
                              : replica.url
                            const state = replicaHealth(name, url)
                            return (
                              <li
                                key={replica.backend || replica.url}
                                className="text-xs"
                              >
                                <code>{url}</code>{' '}
                                {state
                                  ? `${state.healthy ? '● healthy' : '✕ unavailable'} · ${state.in_flight}/${state.max_inflight} in flight`
                                  : '· not running'}
                                {replica.bearer_token_env &&
                                  ` · credential ${replica.bearer_token_env}`}
                              </li>
                            )
                          })}
                        </ul>
                      </td>
                    </tr>
                  ))}
              </tbody>
            </table>
          </div>
          {!settings.managed ? (
            <div className="grid gap-2">
              <p className="subtitle">
                Importing copies the routing in use into the database without
                changing it. Afterwards models, endpoints and aliases are edited
                here as complete revisions, and the per-model endpoint editor is
                closed.
              </p>
              <div>
                <Button
                  disabled={pending}
                  onClick={() => void act('import', {})}
                >
                  Import current routing
                </Button>
              </div>
            </div>
          ) : (
            <>
              {active && data.runtime.revision !== active.id && (
                <div role="note" className="grid gap-2">
                  <p className="subtitle">
                    This router runs a different routing than active revision{' '}
                    {active.id}.
                  </p>
                  <div>
                    <Button
                      variant="outline"
                      disabled={pending}
                      onClick={() => void act('apply', {})}
                    >
                      Apply active routing to this router
                    </Button>
                  </div>
                </div>
              )}
              <DraftEditor
                key={`${(draft ?? active)?.id}-${settings.version}`}
                view={data}
                base={(draft ?? active) as RoutingRevision}
                onSaved={reload}
              />
              {draft && (
                <div className="grid gap-2" aria-label="Routing draft status">
                  {draft.validation?.valid ? (
                    <p className="subtitle">
                      Draft {draft.id} is valid. Activation probes the changed
                      models&apos; endpoints first and changes nothing if one
                      fails; running requests finish normally.
                    </p>
                  ) : (
                    <div role="alert" className="error">
                      Draft {draft.id} cannot be activated:
                      <ul>
                        {draft.validation?.problems.map((problem) => (
                          <li key={problem}>{problem}</li>
                        ))}
                      </ul>
                    </div>
                  )}
                  {data.impact_available && (
                    <div aria-label="Draft affected users">
                      <p>
                        Affected users: {data.affected_users.length}. The
                        protected local administrator retains access.
                      </p>
                      <ul>
                        {data.affected_users.map((user) => (
                          <li key={user.id}>
                            {user.username} ({user.id})
                          </li>
                        ))}
                      </ul>
                    </div>
                  )}
                  <div className="flex flex-wrap gap-2">
                    <Button
                      variant="outline"
                      disabled={pending}
                      onClick={() => void check(draft.id)}
                    >
                      Test changed endpoints
                    </Button>
                    <Button
                      disabled={
                        pending ||
                        !draft.validation?.valid ||
                        !data.impact_available
                      }
                      onClick={() =>
                        void act('activate', {
                          version: settings.version,
                          revision: draft.id,
                        })
                      }
                    >
                      Activate routing draft
                    </Button>
                    <Button
                      variant="outline"
                      disabled={pending}
                      onClick={() =>
                        void act('discard', { version: settings.version })
                      }
                    >
                      Discard routing draft
                    </Button>
                  </div>
                  {results && <Results results={results} />}
                </div>
              )}
              {settings.history.length > 0 && (
                <div className="table-wrap">
                  <table aria-label="Routing revision history">
                    <thead>
                      <tr>
                        <th>Revision</th>
                        <th>Status</th>
                        <th>Created</th>
                        <th>By</th>
                        <th>
                          <span className="sr-only">Actions</span>
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {settings.history.map((revision) => (
                        <tr key={revision.id}>
                          <td>{revision.id}</td>
                          <td>{revision.status.replace('_', ' ')}</td>
                          <td>
                            {new Date(revision.created_at).toLocaleString()}
                          </td>
                          <td>{revision.created_by}</td>
                          <td>
                            {revision.status === 'previously_active' && (
                              <Button
                                variant="outline"
                                size="sm"
                                disabled={pending}
                                aria-label={`Roll back routing to revision ${revision.id}`}
                                onClick={() =>
                                  void act('rollback', {
                                    version: settings.version,
                                    revision: revision.id,
                                  })
                                }
                              >
                                Roll back
                              </Button>
                            )}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </>
          )}
        </div>
      )}
    </section>
  )
}
