import { useState, type FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { Plus, Trash2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import type {
  Backend,
  EndpointModel,
  EndpointReplica,
  RoutingDocument,
} from '@/lib/api'

const defaultReplica = (): EndpointReplica => ({
  url: '',
  health_path: '/health',
  weight: 1,
  max_inflight: 8,
  bearer_token_env: '',
})
const defaultModel = (): EndpointModel => ({
  upstream_model: '',
  protocols: ['openai'],
  timeout_s: 120,
  idle_timeout_s: 30,
  queue_timeout_s: 0,
  session_affinity: false,
  priority: null,
  pool: [defaultReplica()],
})

function Destination({
  replica,
  label,
  backends,
  onChange,
}: {
  replica: EndpointReplica
  label: string
  backends: Record<string, Backend>
  onChange: (replica: EndpointReplica) => void
}) {
  // A blank new replica must not implicitly select a destination.
  const options = Object.entries(backends).filter(
    ([, backend]) => backend != null,
  )
  const manual =
    !replica.backend &&
    (replica.backend === '' || !!replica.url || !options.length)
  const selected = replica.backend
    ? 'backend:' + replica.backend
    : manual
      ? 'manual'
      : ''
  return (
    <div className="grid gap-2">
      {(options.length > 0 || replica.backend) && (
        <label className="field">
          Backend
          <select
            className="select"
            aria-label={label + ' backend'}
            value={selected}
            required
            onChange={(event) => {
              const value = event.target.value
              const backend = value.startsWith('backend:') ? value.slice(8) : ''
              onChange({
                ...replica,
                backend,
                url: '',
                health_path: backend ? '' : '/health',
                bearer_token_env: '',
                private_network: false,
              })
            }}
          >
            <option value="" disabled>
              Choose a backend
            </option>
            {replica.backend && !backends[replica.backend] && (
              <option value={selected}>{replica.backend} (missing)</option>
            )}
            {options.map(([id, backend]) => (
              <option key={id} value={'backend:' + id}>
                {backend.name || id} — {backend.url}
              </option>
            ))}
            <option value="manual">Enter an endpoint URL</option>
          </select>
        </label>
      )}
      {!replica.backend && manual && (
        <label className="field">
          Endpoint URL
          <Input
            aria-label={label + ' origin'}
            value={replica.url ?? ''}
            required
            maxLength={2048}
            placeholder="https://gpu.example.org"
            onChange={(e) => onChange({ ...replica, url: e.target.value })}
          />
        </label>
      )}
    </div>
  )
}

function ReplicaOptions({
  replica,
  label,
  backends,
  references,
  onChange,
  renameBackend,
}: {
  replica: EndpointReplica
  label: string
  backends: Record<string, Backend>
  references: string[]
  onChange: (replica: EndpointReplica) => void
  renameBackend: (id: string, name: string) => void
}) {
  const backend = replica.backend ? backends[replica.backend] : undefined
  return (
    <div className="grid gap-3 sm:grid-cols-3">
      {backend && (
        <label className="field sm:col-span-3">
          Shared backend name
          <Input
            aria-label={label + ' backend name'}
            value={backend.name ?? ''}
            maxLength={128}
            onChange={(e) => renameBackend(replica.backend!, e.target.value)}
          />
          <span className="subtitle">
            Displayed everywhere this backend is used.
          </span>
        </label>
      )}
      <label className="field">
        Health path
        <Input
          aria-label={label + ' health path'}
          value={replica.health_path ?? ''}
          placeholder={backend?.health_path || '/health'}
          maxLength={256}
          onChange={(e) =>
            onChange({ ...replica, health_path: e.target.value })
          }
        />
      </label>
      <label className="field">
        Weight
        <Input
          aria-label={label + ' weight'}
          type="number"
          min={0.01}
          step="any"
          value={replica.weight}
          onChange={(e) =>
            onChange({ ...replica, weight: Number(e.target.value) })
          }
        />
      </label>
      <label className="field">
        Max in flight
        <Input
          aria-label={label + ' max in flight'}
          type="number"
          min={1}
          value={replica.max_inflight}
          onChange={(e) =>
            onChange({ ...replica, max_inflight: Number(e.target.value) })
          }
        />
      </label>
      {!replica.backend && (
        <>
          <label className="field">
            Credential reference
            <select
              className="select"
              aria-label={label + ' credential reference'}
              value={replica.bearer_token_env ?? ''}
              onChange={(e) =>
                onChange({ ...replica, bearer_token_env: e.target.value })
              }
            >
              <option value="">None</option>
              {references.map((ref) => (
                <option key={ref}>{ref}</option>
              ))}
              {replica.bearer_token_env &&
                !references.includes(replica.bearer_token_env) && (
                  <option value={replica.bearer_token_env}>
                    {replica.bearer_token_env} (not allowed)
                  </option>
                )}
            </select>
          </label>
          <label className="flex gap-2 items-center text-sm sm:col-span-2">
            <input
              type="checkbox"
              checked={replica.private_network ?? false}
              onChange={(e) =>
                onChange({ ...replica, private_network: e.target.checked })
              }
            />
            Internal network endpoint (allows HTTP)
          </label>
        </>
      )}
    </div>
  )
}

const timeouts = {
  timeout_s: 'Header timeout',
  idle_timeout_s: 'Idle timeout',
  queue_timeout_s: 'Queue timeout',
} as const
type Timeout = keyof typeof timeouts

function summary(model: EndpointModel) {
  const defaults = defaultModel()
  const parts: string[] = []
  if (model.protocols.join() !== 'openai')
    parts.push(model.protocols.join(' + ') || 'No protocol')
  if (model.pool.length !== 1) parts.push(model.pool.length + ' replicas')
  if (model.type === 'embedding') parts.push('Embedding')
  if (model.context_length)
    parts.push(model.context_length.toLocaleString() + ' tokens')
  if (model.session_affinity) parts.push('Session affinity')
  for (const key of Object.keys(timeouts) as Timeout[])
    if (model[key] !== defaults[key])
      parts.push(timeouts[key] + ' ' + model[key] + ' s')
  if (
    model.pool.some(
      (r) =>
        r.weight !== 1 ||
        r.max_inflight !== 8 ||
        r.bearer_token_env ||
        r.private_network ||
        (r.health_path && r.health_path !== '/health'),
    )
  )
    parts.push('Custom replica settings')
  return parts.length ? parts.join(' · ') : 'OpenAI API · 1 replica'
}

export function RouteEditor({
  source,
  document,
  references,
  advanced,
  pending,
  onSave,
  onKeep,
  onCancel,
}: {
  source: string | null
  document: RoutingDocument
  references: string[]
  advanced: boolean
  pending: boolean
  onSave: (next: RoutingDocument) => void
  onKeep: (next: RoutingDocument) => void
  onCancel: () => void
}) {
  const [name, setName] = useState(source ?? '')
  const [model, setModel] = useState(() => {
    if (source === null) return defaultModel()
    const route = document.models[source]
    return {
      ...route,
      pool: route.pool ?? [],
      protocols: route.protocols ?? [],
    }
  })
  const [backends, setBackends] = useState(document.backends)
  const [nameError, setNameError] = useState('')
  const groups = Object.entries(document.groups ?? {}).filter(
    ([, policy]) => source && policy.models?.includes(source),
  )
  const aliases = Object.entries(document.aliases).filter(
    ([, target]) => source && target === source,
  )
  const blocked = groups.length > 0 || aliases.length > 0
  function updateReplica(index: number, replica: EndpointReplica) {
    setModel({
      ...model,
      pool: model.pool.map((current, i) => (i === index ? replica : current)),
    })
  }
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const nextName = name.trim()
    if (
      !nextName ||
      nextName === '*' ||
      new TextEncoder().encode(nextName).length > 128 ||
      /\p{Cc}/u.test(nextName)
    ) {
      setNameError(
        'Choose a name of 1–128 bytes without control characters or “*”.',
      )
      return
    }
    if (
      nextName !== source &&
      (Object.hasOwn(document.models, nextName) ||
        Object.hasOwn(document.aliases, nextName))
    ) {
      setNameError('This name is already used by a model or alias.')
      return
    }
    const next = {
      ...document,
      models: { ...document.models },
      ...(backends ? { backends } : {}),
    }
    if (source !== null && source !== nextName) {
      delete next.models[source]
      next.groups = { ...next.groups }
      next.aliases = { ...next.aliases }
      for (const [group, policy] of groups)
        next.groups[group] = {
          ...policy,
          models: policy.models.map((value) =>
            value === source ? nextName : value,
          ),
        }
      for (const [alias] of aliases) next.aliases[alias] = nextName
    }
    const updated =
      source && source !== nextName && !model.upstream_model
        ? { ...model, upstream_model: source }
        : model
    next.models = { ...next.models, [nextName]: updated }
    const button = (event.nativeEvent as SubmitEvent)
      .submitter as HTMLButtonElement | null
    if (button?.value === 'keep') onKeep(next)
    else onSave(next)
  }
  const label = source ?? 'New route'
  const labelFor = (index: number) => `${label} replica ${index + 1}`
  const renameBackend = (id: string, name: string) =>
    setBackends((current) => ({ ...current, [id]: { ...current![id], name } }))
  const first = model.pool[0]
  return (
    <form
      onSubmit={submit}
      aria-label="Edit routing draft"
      className="routing-editor"
    >
      <fieldset disabled={pending} className="grid min-w-0 gap-4">
        <label className="field">
          Name clients use
          <Input
            aria-label="Name clients use"
            required
            value={name}
            maxLength={128}
            placeholder="research-chat"
            aria-invalid={!!nameError}
            aria-describedby={nameError ? 'route-name-error' : undefined}
            onChange={(e) => {
              setName(e.target.value)
              setNameError('')
            }}
          />
        </label>
        {nameError && (
          <p id="route-name-error" role="alert" className="error">
            {nameError}
          </p>
        )}
        {source && name.trim() !== source && (
          <p className="subtitle">
            Aliases and group grants follow the new name. Update clients that
            use the old name.
          </p>
        )}
        {first && (
          <Destination
            replica={first}
            label={labelFor(0)}
            backends={backends ?? {}}
            onChange={(replica) => updateReplica(0, replica)}
          />
        )}
        <label className="field">
          Model on that backend
          <Input
            aria-label="Model on that backend"
            value={model.upstream_model ?? ''}
            placeholder={source || name || 'Same as the client name'}
            maxLength={256}
            onChange={(e) =>
              setModel({ ...model, upstream_model: e.target.value })
            }
          />
        </label>
        <details open={advanced || undefined} className="route-options">
          <summary>
            <span>Advanced settings</span>
            <small>{summary(model)}</small>
          </summary>
          <div className="grid gap-4 pt-4">
            <div className="flex flex-wrap gap-4 text-sm">
              {(['openai', 'anthropic'] as const).map((protocol) => (
                <label key={protocol} className="flex gap-2 items-center">
                  <input
                    type="checkbox"
                    aria-label={label + ' ' + protocol + ' protocol'}
                    checked={model.protocols.includes(protocol)}
                    onChange={(e) =>
                      setModel({
                        ...model,
                        protocols: e.target.checked
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
                  checked={model.session_affinity}
                  onChange={(e) =>
                    setModel({ ...model, session_affinity: e.target.checked })
                  }
                />
                Session affinity
              </label>
            </div>
            <div className="grid gap-3 sm:grid-cols-3">
              {(Object.keys(timeouts) as Timeout[]).map((key) => (
                <label className="field" key={key}>
                  {timeouts[key]} (s)
                  <Input
                    aria-label={label + ' ' + key}
                    type="number"
                    min={0}
                    value={model[key] ?? 0}
                    onChange={(e) =>
                      setModel({ ...model, [key]: Number(e.target.value) })
                    }
                  />
                </label>
              ))}
              <label className="field">
                Context length (tokens)
                <Input
                  aria-label={label + ' context length'}
                  type="number"
                  min={1}
                  step={1}
                  value={model.context_length ?? ''}
                  placeholder="Detected"
                  onChange={(e) =>
                    setModel({
                      ...model,
                      context_length:
                        e.target.value === ''
                          ? undefined
                          : Number(e.target.value),
                    })
                  }
                />
              </label>
              <label className="field">
                Model type
                <select
                  className="select"
                  aria-label={label + ' model type'}
                  value={model.type ?? 'chat'}
                  onChange={(e) =>
                    setModel({
                      ...model,
                      type:
                        e.target.value === 'embedding'
                          ? 'embedding'
                          : undefined,
                    })
                  }
                >
                  <option value="chat">Chat</option>
                  <option value="embedding">Embedding</option>
                </select>
              </label>
            </div>
            {model.pool.map((replica, index) => (
              <fieldset
                key={index}
                className="grid min-w-0 gap-3 rounded-md border p-3"
              >
                <legend className="px-1 font-medium">
                  Replica {index + 1}
                </legend>
                {index > 0 && (
                  <Destination
                    replica={replica}
                    label={labelFor(index)}
                    backends={backends ?? {}}
                    onChange={(next) => updateReplica(index, next)}
                  />
                )}
                <ReplicaOptions
                  replica={replica}
                  label={labelFor(index)}
                  backends={backends ?? {}}
                  references={references}
                  onChange={(next) => updateReplica(index, next)}
                  renameBackend={renameBackend}
                />
                {model.pool.length > 1 && (
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="justify-self-start"
                    aria-label={'Remove ' + labelFor(index)}
                    onClick={() =>
                      setModel({
                        ...model,
                        pool: model.pool.filter((_, i) => index !== i),
                      })
                    }
                  >
                    <Trash2 />
                    Remove replica
                  </Button>
                )}
              </fieldset>
            ))}
            <Button
              type="button"
              className="justify-self-start"
              variant="outline"
              onClick={() =>
                setModel({ ...model, pool: [...model.pool, defaultReplica()] })
              }
            >
              <Plus />
              Add replica
            </Button>
            {source && (
              <div className="grid justify-items-start gap-2 border-t pt-3">
                <Button
                  type="button"
                  variant="outline"
                  disabled={blocked}
                  onClick={() => {
                    const models = { ...document.models }
                    delete models[source]
                    onKeep({ ...document, models })
                  }}
                >
                  Remove route
                </Button>
                {blocked && (
                  <p className="subtitle">
                    Remove references first from{' '}
                    {[
                      ...groups.map(([key]) => 'group ' + key),
                      ...aliases.map(([key]) => 'alias ' + key),
                    ].join(', ')}
                    .
                  </p>
                )}
              </div>
            )}
          </div>
        </details>
        <div className="flex flex-wrap gap-2">
          <Button type="submit" value="save">
            Save routing draft
          </Button>
          <Button type="submit" variant="outline" value="keep">
            Keep editing other routes
          </Button>
          <Button type="button" variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
        </div>
        {!Object.keys(backends ?? {}).length && (
          <p className="subtitle">
            To discover models from a new connection, use{' '}
            <Link className="underline" to="/admin">
              Backends
            </Link>
            .
          </p>
        )}
      </fieldset>
    </form>
  )
}
