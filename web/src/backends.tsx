import { useEffect, useState } from 'react'
import { Plus, Trash2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  api,
  type Backend,
  type EndpointModel,
  type RoutingDocument,
} from '@/lib/api'

type Catalog = {
  url: string
  models: string[]
  provider?: string
  health_path?: string
  error?: string
}
type Props = {
  document: RoutingDocument
  secrets: Record<string, string>
  encryption: boolean
  onChange: (document: RoutingDocument, secrets: Record<string, string>) => void
}

function backendOrigin(raw: string) {
  const parsed = new URL(raw.trim())
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
    throw new Error('Enter the host URL or its /v1 API base.')
  return parsed.origin
}

export function Backends({ document, secrets, encryption, onChange }: Props) {
  const [editing, setEditing] = useState('')
  const [notice, setNotice] = useState('')
  const backends = document.backends ?? {}
  return (
    <section aria-label="Backends" className="grid gap-4 rounded-md border p-4">
      <h4 className="font-medium">1. Connect a backend</h4>
      <p className="subtitle">
        Add your cluster host first, then choose its models to create routes.
        Changes are saved with the routing draft.
      </p>
      {Object.entries(backends).map(([id, backend]) => {
        const used = Object.values(document.models).some((model) =>
          model.pool.some((replica) => replica.backend === id),
        )
        return (
          <div key={id} className="flex flex-wrap items-center gap-2 text-sm">
            <strong>{id}</strong>
            <code className="break-all">{backend.url}</code>
            <span className="subtitle">
              {secrets[id]
                ? 'New key pending save'
                : backend.api_key.stored
                  ? 'API key configured'
                  : 'No API key'}
            </span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => setEditing(id)}
            >
              Edit / discover models
            </Button>
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={used}
              title={
                used
                  ? 'Remove its routes or select another backend first'
                  : undefined
              }
              aria-label={`Remove backend ${id}`}
              onClick={() => {
                const next = { ...backends }
                delete next[id]
                const keys = { ...secrets }
                delete keys[id]
                onChange({ ...document, backends: next }, keys)
                if (editing === id) setEditing('')
              }}
            >
              <Trash2 />
            </Button>
          </div>
        )
      })}
      {editing && (
        <Button type="button" variant="outline" onClick={() => setEditing('')}>
          Connect another backend
        </Button>
      )}
      <BackendForm
        key={editing}
        {...{ document, secrets, encryption, onChange }}
        editing={editing}
        onAdded={(id, message) => {
          setEditing(id)
          setNotice(message)
        }}
      />
      {notice && (
        <p role="status" className="subtitle">
          {notice}
        </p>
      )}
    </section>
  )
}

function BackendForm({
  document,
  secrets,
  encryption,
  onChange,
  editing,
  onAdded,
}: Props & {
  editing: string
  onAdded: (id: string, message: string) => void
}) {
  const original = document.backends?.[editing]
  const [id, setID] = useState(editing)
  const [url, setURL] = useState(original?.url ?? '')
  const [key, setKey] = useState(secrets[editing] ?? '')
  const [removeKey, setRemoveKey] = useState(false)
  const [privateNetwork, setPrivateNetwork] = useState(
    original?.private_network ?? true,
  )
  const [healthPath, setHealthPath] = useState(original?.health_path ?? '')
  const [refresh, setRefresh] = useState(0)
  const [catalog, setCatalog] = useState<{
    source: string
    result: Catalog
  } | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [selected, setSelected] = useState<string[]>([])
  const [manual, setManual] = useState('')
  const [groups, setGroups] = useState<string[]>([])
  const [anthropic, setAnthropic] = useState(false)
  let changedHost = !!original
  try {
    changedHost = !!original && backendOrigin(url) !== original.url
  } catch {
    /* Incomplete URLs never inherit a stored key. */
  }
  const stored = !changedHost && !removeKey ? (original?.api_key ?? {}) : {}
  const source = JSON.stringify({
    id: id || 'new-backend',
    backend: {
      url,
      api_key: stored,
      private_network: privateNetwork,
      health_path: '/v1/models',
    },
    ...(key || removeKey ? { api_key: key } : {}),
  })
  useEffect(() => {
    const controller = new AbortController()
    const timer = setTimeout(async () => {
      setCatalog(null)
      setSelected([])
      setError('')
      if (!url.trim()) {
        setBusy(false)
        return
      }
      setBusy(true)
      try {
        const result = await api<Catalog>(
          'admin/backends/detect',
          'POST',
          JSON.parse(source),
          controller.signal,
        )
        if (!controller.signal.aborted) {
          setCatalog({ source, result })
          if (!result.error && result.models.length === 1)
            setSelected(result.models)
        }
      } catch (error) {
        if (!controller.signal.aborted)
          setError(error instanceof Error ? error.message : 'Detection failed.')
      } finally {
        if (!controller.signal.aborted) setBusy(false)
      }
    }, 500)
    return () => {
      clearTimeout(timer)
      controller.abort()
    }
  }, [source, refresh, url])
  // A response belongs to its exact request, so stale catalogs cannot create routes.
  const current = catalog?.source === source ? catalog.result : null
  const validID =
    /^[a-zA-Z0-9_-]{1,64}$/.test(id) && (!!editing || !document.backends?.[id])
  function add(withRoutes: boolean) {
    setError('')
    let origin: string
    try {
      origin = backendOrigin(url)
    } catch {
      setError('Enter the host URL or its /v1 API base.')
      return
    }
    const backend: Backend = {
      url: origin,
      api_key: key || removeKey ? {} : stored,
      private_network: privateNetwork,
      health_path: healthPath || current?.health_path || '/v1/models',
    }
    const next = {
      ...document,
      backends: { ...document.backends, [id]: backend },
      models: { ...document.models },
      groups: { ...document.groups },
    }
    const keys = { ...secrets }
    delete keys[id]
    if (key) keys[id] = key
    const names = withRoutes
      ? [
          ...new Set([
            ...(current ? selected : []),
            ...(manual.trim() ? [manual.trim()] : []),
          ]),
        ]
      : []
    if (names.some((name) => name.length > 128 || name === '*')) {
      setError(
        'Route names must be at most 128 characters and cannot be *. Add a shorter logical name in the route editor.',
      )
      return
    }
    if (names.some((name) => name in next.models)) {
      setError(
        'A selected route already exists. Add this backend as a replica in its route editor.',
      )
      return
    }
    for (const name of names) {
      const model: EndpointModel = {
        upstream_model: name,
        protocols: anthropic ? ['anthropic'] : ['openai'],
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
            bearer_token_env: '',
            weight: 1,
            max_inflight: 8,
          },
        ],
      }
      next.models[name] = model
      for (const group of groups) {
        const policy = next.groups[group]
        next.groups[group] = {
          ...policy,
          models: [...new Set([...policy.models, name])],
        }
      }
    }
    onChange(next, keys)
    onAdded(
      id,
      names.length
        ? `Added ${names.length} route(s) to the editor. Save and activate the routing draft to publish them.`
        : 'Backend added to the editor. Choose models now or save the routing draft.',
    )
  }
  return (
    <div className="grid gap-3" aria-label="Connect backend">
      <div className="grid gap-3 sm:grid-cols-2">
        <label className="grid gap-1 text-sm">
          Backend name
          <Input
            aria-label="Backend name"
            value={id}
            disabled={!!editing}
            maxLength={64}
            placeholder="gpu-cluster"
            onChange={(event) => setID(event.target.value)}
          />
        </label>
        <label className="grid gap-1 text-sm">
          Host URL
          <Input
            aria-label="Backend host URL"
            value={url}
            maxLength={2048}
            placeholder="http://gpu-node.cluster:8000"
            onChange={(event) => setURL(event.target.value)}
          />
        </label>
      </div>
      <div
        className="grid gap-2 rounded-md bg-muted/30 p-3"
        aria-label="Backend detection"
      >
        <div className="flex flex-wrap items-center gap-3">
          <strong className="text-sm">
            {busy
              ? 'Detecting API and models…'
              : (current?.provider ?? 'Automatic API and model detection')}
          </strong>
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={busy || !url.trim()}
            onClick={() => setRefresh((value) => value + 1)}
          >
            Detect again
          </Button>
        </div>
        <p className="subtitle" role="status">
          {error ||
            current?.error ||
            (current
              ? `${current.models.length} model(s) detected at ${current.url}${current.health_path ?? ''}`
              : 'Enter a host URL. Detection runs automatically from the router.')}
        </p>
        <p className="subtitle">
          Shared OpenAI APIs do not always identify their server. vLLM and
          SGLang are identified when advertised; Ollama is detected through its
          native catalog.
        </p>
      </div>
      <label className="grid gap-1 text-sm">
        API key (optional)
        <Input
          aria-label="Backend API key"
          type="password"
          autoComplete="new-password"
          maxLength={4096}
          value={key}
          disabled={!encryption}
          placeholder={
            stored.stored
              ? 'Stored key — enter a replacement'
              : 'Leave empty for an unauthenticated backend'
          }
          onChange={(event) => setKey(event.target.value)}
        />
      </label>
      {!encryption && (
        <p className="subtitle">
          To save API keys, configure OLR_SETTINGS_ENCRYPTION_KEY on the router.
          Backends without a key can be added now.
        </p>
      )}
      {original?.api_key.stored && (
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={removeKey}
            onChange={(event) => setRemoveKey(event.target.checked)}
          />
          Remove saved API key
        </label>
      )}
      {changedHost && original?.api_key.stored && (
        <p className="subtitle">
          Changing the host clears its saved key. Enter a new key for this host
          if required.
        </p>
      )}
      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={privateNetwork}
          onChange={(event) => setPrivateNetwork(event.target.checked)}
        />
        Private cluster endpoint (allows HTTP on private networks)
      </label>
      <label className="grid gap-1 text-sm">
        Health check path
        <Input
          aria-label="Backend health path"
          value={healthPath}
          placeholder={current?.health_path ?? '/v1/models'}
          maxLength={256}
          onChange={(event) => setHealthPath(event.target.value)}
        />
      </label>
      <div>
        <Button
          type="button"
          variant="outline"
          disabled={!validID || !url.trim() || (!!key && !encryption)}
          onClick={() => add(false)}
        >
          {editing ? 'Update backend in draft' : 'Add backend to draft'}
        </Button>
      </div>
      <h4 className="font-medium">2. Choose models and create routes</h4>
      {!!current?.models.length && (
        <div
          className="grid gap-2 max-h-64 overflow-auto"
          aria-label="Detected backend models"
        >
          {current.models.map((name) => (
            <label
              key={name}
              className="flex items-center gap-2 text-sm break-all"
            >
              <input
                type="checkbox"
                aria-label={`Route ${name}`}
                checked={selected.includes(name)}
                onChange={(event) =>
                  setSelected(
                    event.target.checked
                      ? [...selected, name]
                      : selected.filter((model) => model !== name),
                  )
                }
              />
              {name}
            </label>
          ))}
        </div>
      )}
      <label className="grid gap-1 text-sm">
        Manual upstream model (if detection is unavailable)
        <Input
          aria-label="Manual backend model"
          value={manual}
          maxLength={128}
          onChange={(event) => setManual(event.target.value)}
        />
      </label>
      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={anthropic}
          onChange={(event) => setAnthropic(event.target.checked)}
        />
        This endpoint serves the Anthropic Messages API
      </label>
      <fieldset className="grid gap-2">
        <legend className="text-sm">Grant these routes to groups</legend>
        {Object.keys(document.groups)
          .sort()
          .map((name) => (
            <label key={name} className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={groups.includes(name)}
                onChange={(event) =>
                  setGroups(
                    event.target.checked
                      ? [...groups, name]
                      : groups.filter((group) => group !== name),
                  )
                }
              />
              {name}
            </label>
          ))}
      </fieldset>
      <p className="subtitle">
        Groups with a wildcard grant already have access to new routes.
      </p>
      <div>
        <Button
          type="button"
          disabled={
            !validID ||
            !url.trim() ||
            (!(current && selected.length) && !manual.trim()) ||
            (!!key && !encryption)
          }
          onClick={() => add(true)}
        >
          <Plus /> Add selected models as routes
        </Button>
      </div>
    </div>
  )
}
