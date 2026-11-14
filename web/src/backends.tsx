import { useEffect, useRef, useState, type FormEvent } from 'react'
import { Link } from 'react-router-dom'
import {
  Server,
  Plus,
  Pencil,
  Trash2,
  Search,
  RefreshCw,
  Globe,
  KeyRound,
  Users,
  Infinity as InfinityIcon,
} from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from '@/components/ui/dialog'
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogCancel,
  AlertDialogAction,
} from '@/components/ui/alert-dialog'
import { api, type Backend, type RoutingDocument } from '@/lib/api'
import {
  useBackendWorkspace,
  backendOrigin,
  addBackendModels,
  removeBackendModels,
} from '@/backend-workspace'

type Catalog = {
  url: string
  models: string[]
  provider?: string
  health_path?: string
  error?: string
}
type Entry = {
  id: string
  backend: Backend
  models: string[]
  legacy?: boolean
  deploymentKey?: boolean
}

function entries(document: RoutingDocument): Entry[] {
  const result: Entry[] = Object.entries(document.backends ?? {}).map(
    ([id, backend]) => ({
      id,
      backend,
      models: [
        ...new Set(
          Object.entries(document.models)
            .filter(([, model]) =>
              model.pool.some((replica) => replica.backend === id),
            )
            .map(([name, model]) => model.upstream_model || name),
        ),
      ],
    }),
  )
  const legacy = new Map<string, Entry>()
  for (const [name, model] of Object.entries(document.models))
    for (const replica of model.pool) {
      if (replica.backend || !replica.url) continue
      let entry = legacy.get(replica.url)
      if (!entry) {
        const hostname = new URL(replica.url).host
        const base =
          hostname
            .toLowerCase()
            .replace(/[^a-z0-9_-]/g, '-')
            .slice(0, 50) || 'backend'
        let id = base
        let suffix = 1
        while (
          result.some((entry) => entry.id === id) ||
          [...legacy.values()].some((entry) => entry.id === id)
        )
          id = base + '-' + suffix++
        entry = {
          id,
          backend: {
            name: hostname,
            url: replica.url,
            api_key: {},
            private_network:
              replica.private_network ?? replica.url.startsWith('http:'),
            health_path: replica.health_path || '/health',
            groups: [],
            preset: '',
          },
          models: [],
          legacy: true,
          deploymentKey: !!replica.bearer_token_env,
        }
        legacy.set(replica.url, entry)
      }
      entry.models = [
        ...new Set([...entry.models, model.upstream_model || name]),
      ]
      entry.backend.groups = [
        ...new Set([
          ...(entry.backend.groups ?? []),
          ...Object.entries(document.groups)
            .filter(
              ([, group]) =>
                group.models.includes('*') || group.models.includes(name),
            )
            .map(([name]) => name),
        ]),
      ]
      entry.deploymentKey ||= !!replica.bearer_token_env
    }
  return [...result, ...legacy.values()]
}

export function BackendWorkspace() {
  const { query, document, health, saving, save } = useBackendWorkspace()
  const [search, setSearch] = useState('')
  const [editing, setEditing] = useState<Entry | 'new' | null>(null)
  const [removing, setRemoving] = useState<Entry | null>(null)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')
  const pending = !!query.data?.settings.draft
  const backends = document ? entries(document) : []
  const filtered = backends.filter(({ id, backend, models }) =>
    [id, backend.name, backend.url, ...models]
      .join(' ')
      .toLowerCase()
      .includes(search.toLowerCase()),
  )
  async function remove() {
    if (!document || !removing) return
    setError('')
    try {
      const base = structuredClone(document)
      if (removing.legacy)
        for (const model of Object.values(base.models))
          model.pool = model.pool.map((replica) =>
            !replica.backend && replica.url === removing.backend.url
              ? { ...replica, backend: removing.id }
              : replica,
          )
      const next = removeBackendModels(base, removing.id)
      delete next.backends![removing.id]
      await save(next, {}, query.data?.settings.version)
      setNotice('Backend removed.')
      setRemoving(null)
    } catch (error) {
      setError((error as Error).message)
    }
  }
  return (
    <section aria-label="Backend management" className="backend-workspace">
      <div className="backend-heading">
        <div>
          <h2>Backends</h2>
          <p className="subtitle">
            Connect your cluster, choose its models, and control who can use it.
          </p>
        </div>
        <Button
          onClick={() => setEditing('new')}
          disabled={!document || pending || saving}
        >
          <Plus size={16} /> Add backend
        </Button>
      </div>
      {(query.error || error) && (
        <div role="alert" className="error">
          {error || query.error?.message}
        </div>
      )}
      {notice && (
        <p role="status" className="notice">
          {notice}
        </p>
      )}
      {pending && (
        <p className="notice">
          Advanced routing has pending changes.{' '}
          <Link to="/admin/advanced">Review or discard them</Link> before
          editing backends.
        </p>
      )}
      <div className="backend-toolbar">
        <div className="backend-search">
          <Search size={16} />
          <Input
            aria-label="Search backends"
            placeholder="Search by name, host, or model…"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
          />
        </div>
        <span className="subtitle">
          {backends.length} backend{backends.length === 1 ? '' : 's'}
        </span>
      </div>
      {!document && !query.error && <p role="status">Loading backends…</p>}
      {document && filtered.length === 0 && (
        <div className="backend-empty">
          <Server size={36} />
          <h2>
            {backends.length
              ? 'No matching backends'
              : 'Connect your first backend'}
          </h2>
          <p>
            Enter an IP address or service name. HTTP, HTTPS, and optional API
            keys are supported.
          </p>
          {!backends.length && (
            <Button onClick={() => setEditing('new')} disabled={pending}>
              <Plus size={16} /> Add backend
            </Button>
          )}
        </div>
      )}
      <div className="backend-grid">
        {filtered.map((entry) => {
          const states =
            health.data?.models
              .flatMap((model) => model.replicas)
              .filter((replica) =>
                entry.legacy
                  ? !replica.backend && replica.url === entry.backend.url
                  : replica.backend === entry.id,
              ) ?? []
          const status = !states.length
            ? 'Not serving models'
            : states.every((replica) => replica.healthy)
              ? 'Healthy'
              : states.some((replica) => replica.healthy)
                ? 'Degraded'
                : 'Unavailable'
          const preset = entry.backend.preset
            ? document?.backend_presets?.[entry.backend.preset]
            : undefined
          return (
            <article
              key={entry.id}
              className="backend-card"
              aria-label={entry.backend.name || entry.id}
            >
              <div className="backend-card-head">
                <span className="backend-icon">
                  <Server size={22} />
                </span>
                <span
                  className={
                    'backend-status ' + (status === 'Healthy' ? 'healthy' : '')
                  }
                >
                  <span />
                  {status}
                </span>
              </div>
              <h2>{entry.backend.name || entry.id}</h2>
              <code className="backend-host">{entry.backend.url}</code>
              <div className="backend-tags">
                <span>
                  {entry.backend.provider ||
                    (entry.legacy ? 'Existing connection' : 'Custom endpoint')}
                </span>
                <span>
                  <Globe size={12} />
                  {entry.backend.url.startsWith('https:') ? 'HTTPS' : 'HTTP'}
                </span>
                <span>
                  <KeyRound size={12} />
                  {entry.backend.api_key.stored || entry.deploymentKey
                    ? 'API key'
                    : 'No key'}
                </span>
              </div>
              <dl className="backend-details">
                <div>
                  <dt>Models</dt>
                  <dd>
                    {entry.models.length
                      ? entry.models.slice(0, 3).join(', ') +
                        (entry.models.length > 3
                          ? ' +' + (entry.models.length - 3)
                          : '')
                      : 'None selected'}
                  </dd>
                </div>
                <div>
                  <dt>
                    <Users size={14} /> Access
                  </dt>
                  <dd>
                    {entry.backend.groups == null
                      ? 'Existing model permissions'
                      : entry.backend.groups.length
                        ? entry.backend.groups.join(', ')
                        : 'Administrators only'}
                  </dd>
                </div>
                <div>
                  <dt>
                    <InfinityIcon size={14} /> Limits
                  </dt>
                  <dd>{preset?.name || 'Existing route limits'}</dd>
                </div>
              </dl>
              <div className="backend-card-actions">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={pending || saving}
                  onClick={() => setEditing(entry)}
                >
                  <Pencil size={14} />
                  {entry.legacy ? 'Manage backend' : 'Edit backend'}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  aria-label={'Remove ' + (entry.backend.name || entry.id)}
                  disabled={pending || saving}
                  onClick={() => {
                    setError('')
                    setRemoving(entry)
                  }}
                >
                  <Trash2 size={15} />
                </Button>
              </div>
            </article>
          )
        })}
      </div>
      {editing && document && query.data && (
        <BackendEditor
          key={editing === 'new' ? 'new' : editing.id}
          entry={editing === 'new' ? undefined : editing}
          document={document}
          version={query.data.settings.version}
          encryption={query.data.encryption_key_configured}
          saving={saving}
          save={save}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null)
            setNotice('Backend saved and active.')
          }}
        />
      )}
      <AlertDialog
        open={!!removing}
        onOpenChange={(open) => {
          if (!open && !saving) setRemoving(null)
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Remove {removing?.backend.name || removing?.id}?
            </AlertDialogTitle>
            <AlertDialogDescription>
              This removes this backend from its routes. Routes without another
              backend, their aliases, and their group grants will also be
              removed. Existing streams can finish.
            </AlertDialogDescription>
          </AlertDialogHeader>
          {error && (
            <p role="alert" className="error">
              {error}
            </p>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={saving}>Cancel</AlertDialogCancel>
            <Button variant="destructive" disabled={saving} onClick={remove}>
              {saving ? 'Removing…' : 'Remove backend'}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  )
}

type EditorProps = {
  entry?: Entry
  document: RoutingDocument
  version: number
  encryption: boolean
  saving: boolean
  save: (
    document: RoutingDocument,
    secrets: Record<string, string>,
    expected: number,
  ) => Promise<void>
  onClose: () => void
  onSaved: () => void
}

function BackendEditor({
  entry,
  document,
  version,
  encryption,
  saving,
  save,
  onClose,
  onSaved,
}: EditorProps) {
  const [expected] = useState(version)
  const [id] = useState(
    entry?.id ?? 'backend-' + crypto.randomUUID().slice(0, 8),
  )
  const [name, setName] = useState(entry?.backend.name ?? '')
  const [host, setHost] = useState(entry?.backend.url ?? '')
  const [privateNetwork, setPrivateNetwork] = useState(
    entry?.backend.private_network ?? true,
  )
  const [useKey, setUseKey] = useState(
    !!(entry?.backend.api_key.stored || entry?.deploymentKey),
  )
  const [key, setKey] = useState('')
  const [groups, setGroups] = useState(
    entry?.backend.groups ??
      (entry
        ? Object.entries(document.groups)
            .filter(
              ([, group]) =>
                group.models.includes('*') ||
                Object.entries(document.models).some(
                  ([name, model]) =>
                    group.models.includes(name) &&
                    model.pool.some((replica) => replica.backend === entry.id),
                ),
            )
            .map(([name]) => name)
        : []),
  )
  const [preset, setPreset] = useState(
    entry ? entry.backend.preset || '' : 'unlimited',
  )
  const [models, setModels] = useState(entry?.models ?? [])
  const [manual, setManual] = useState('')
  const [healthPath, setHealthPath] = useState(entry?.backend.health_path || '')
  const [anthropic, setAnthropic] = useState(false)
  const [detected, setDetected] = useState<{
    source: string
    catalog: Catalog
  } | null>(null)
  const [detecting, setDetecting] = useState<string | null>(null)
  const autoSelected = useRef('')
  const lastOrigin = useRef(entry?.backend.url ?? '')
  const [refresh, setRefresh] = useState(0)
  const [error, setError] = useState('')
  const [dirty, setDirty] = useState(false)
  const [discard, setDiscard] = useState(false)
  let origin = ''
  try {
    if (host.trim()) origin = backendOrigin(host)
  } catch {
    /* allow partial input */
  }
  const stored =
    useKey && entry?.backend.url === origin
      ? entry.backend.api_key.stored
      : undefined
  const source = JSON.stringify({
    id,
    backend: {
      url: host,
      api_key: stored ? { stored } : {},
      private_network: privateNetwork,
      health_path: '/v1/models',
    },
    ...(useKey && key ? { api_key: key } : {}),
  })
  const catalog = detected?.source === source ? detected.catalog : null
  const busy = detecting === source
  useEffect(() => {
    if (!origin || (useKey && !key && !stored)) return
    const controller = new AbortController()
    const timer = setTimeout(async () => {
      setDetecting(source)
      try {
        const result = await api<Catalog>(
          'admin/backends/detect',
          'POST',
          JSON.parse(source),
          controller.signal,
        )
        if (controller.signal.aborted) return
        setDetected({ source, catalog: result })
        if (!entry && !result.error && autoSelected.current !== origin) {
          setModels(result.models)
          autoSelected.current = origin
        }
      } catch (error) {
        if (!controller.signal.aborted)
          setDetected({
            source,
            catalog: {
              url: origin,
              models: [],
              error: (error as Error).message,
            },
          })
      } finally {
        if (!controller.signal.aborted) setDetecting(null)
      }
    }, 500)
    return () => {
      clearTimeout(timer)
      controller.abort()
    }
  }, [source, refresh, origin, useKey, key, stored, entry])
  useEffect(() => {
    const prevent = (event: BeforeUnloadEvent) => {
      if (dirty) event.preventDefault()
    }
    window.addEventListener('beforeunload', prevent)
    return () => window.removeEventListener('beforeunload', prevent)
  }, [dirty])
  function close() {
    if (!saving) {
      if (dirty) setDiscard(true)
      else onClose()
    }
  }
  async function submit(event: FormEvent) {
    event.preventDefault()
    setError('')
    try {
      if (!origin) throw new Error('Enter a valid IP address or service host.')
      if (useKey && !key && !stored)
        throw new Error(
          'Enter an API key for this host, or turn off Use an API key.',
        )
      const next = addBackendModels(
        document,
        id,
        models,
        groups,
        entry?.legacy ? entry.backend.url : undefined,
      )
      next.backends![id] = {
        name: name.trim() || new URL(origin).host,
        provider: catalog?.provider || entry?.backend.provider,
        url: origin,
        api_key: stored ? { stored } : {},
        private_network: privateNetwork,
        health_path: healthPath.trim() || catalog?.health_path || '/v1/models',
        groups,
        preset: preset || undefined,
      }
      if (preset)
        for (const model of Object.values(next.models))
          model.pool = model.pool.map((replica) =>
            replica.backend === id
              ? { ...replica, max_inflight: 2147483647 }
              : replica,
          )
      if (anthropic)
        for (const model of Object.values(next.models))
          if (
            model.pool.some((replica) => replica.backend === id) &&
            !model.protocols.includes('anthropic')
          )
            model.protocols.push('anthropic')
      await save(next, useKey && key ? { [id]: key } : {}, expected)
      onSaved()
    } catch (error) {
      setError((error as Error).message)
    }
  }
  const available = [...new Set([...(catalog?.models ?? []), ...models])]
  return (
    <>
      <Dialog
        open
        onOpenChange={(open) => {
          if (!open) close()
        }}
      >
        <DialogContent className="backend-dialog sm:max-w-xl">
          <DialogHeader>
            <DialogTitle>{entry ? 'Edit backend' : 'Add backend'}</DialogTitle>
            <DialogDescription>
              Enter the URL, choose authentication, and select the models to
              use.
            </DialogDescription>
          </DialogHeader>
          <form
            onSubmit={submit}
            onChange={() => setDirty(true)}
            className="backend-form"
          >
            <div className="field">
              <label htmlFor={'backend-url-' + id}>URL</label>
              <div className="endpoint-url-row">
                <Input
                  id={'backend-url-' + id}
                  aria-label="IP address or service URL"
                  required
                  maxLength={2048}
                  placeholder="http://llm.cluster:8000/v1"
                  value={host}
                  onChange={(event) => {
                    const raw = event.target.value
                    setHost(raw)
                    try {
                      const nextOrigin = backendOrigin(raw)
                      if (nextOrigin !== lastOrigin.current) {
                        setModels([])
                        autoSelected.current = ''
                        lastOrigin.current = nextOrigin
                      }
                    } catch {
                      /* preserve selections while an API path is partially typed */
                    }
                  }}
                />
                <Button
                  type="button"
                  variant="outline"
                  size="icon"
                  aria-label="Refresh models"
                  title="Refresh models"
                  disabled={!origin || busy || (useKey && !key && !stored)}
                  onClick={() => setRefresh((value) => value + 1)}
                >
                  <RefreshCw size={16} className={busy ? 'animate-spin' : ''} />
                </Button>
              </div>
              <div
                className="backend-detection"
                aria-label="Backend detection"
                aria-live="polite"
              >
                {busy
                  ? 'Detecting models…'
                  : catalog?.error
                    ? catalog.error
                    : catalog
                      ? `${catalog.provider || 'Compatible API'} · ${catalog.models.length} model${catalog.models.length === 1 ? '' : 's'} discovered`
                      : 'Enter a URL to load models. HTTP and HTTPS are supported.'}
              </div>
            </div>
            <div className="backend-options">
              <label>
                <input
                  type="checkbox"
                  checked={useKey}
                  onChange={(event) => {
                    setUseKey(event.target.checked)
                    setKey('')
                  }}
                />{' '}
                Use an API key
              </label>
            </div>
            {useKey && (
              <label className="field">
                API key
                <Input
                  aria-label="Backend API key"
                  type="password"
                  autoComplete="new-password"
                  disabled={!encryption}
                  placeholder={
                    stored ? 'Configured — leave blank to keep' : 'Enter key'
                  }
                  value={key}
                  onChange={(event) => setKey(event.target.value)}
                />
                <span className="subtitle">
                  {!encryption
                    ? 'API key storage is not configured. Contact the server operator.'
                    : stored
                      ? 'Stored securely. Enter a new key to rotate it.'
                      : entry?.deploymentKey
                        ? 'Re-enter the deployment key to manage this backend here.'
                        : 'Leave authentication off for endpoints without a key.'}
                </span>
              </label>
            )}
            <fieldset className="backend-fieldset">
              <legend>
                Models <span>{models.length} selected</span>
              </legend>
              {available.length > 0 && (
                <div className="backend-models">
                  {available.map((model) => (
                    <label key={model}>
                      <input
                        type="checkbox"
                        checked={models.includes(model)}
                        onChange={(event) =>
                          setModels(
                            event.target.checked
                              ? [...models, model]
                              : models.filter((value) => value !== model),
                          )
                        }
                      />
                      <span>{model}</span>
                    </label>
                  ))}
                </div>
              )}
              <details className="backend-manual-entry">
                <summary>Enter a model ID manually</summary>
                <div className="backend-manual">
                  <Input
                    aria-label="Manual model ID"
                    placeholder="Add a model ID manually"
                    maxLength={128}
                    value={manual}
                    onChange={(event) => setManual(event.target.value)}
                  />
                  <Button
                    type="button"
                    variant="outline"
                    disabled={!manual.trim()}
                    onClick={() => {
                      setModels([...new Set([...models, manual.trim()])])
                      setManual('')
                      setDirty(true)
                    }}
                  >
                    Add model
                  </Button>
                </div>
              </details>
            </fieldset>
            <details className="backend-advanced">
              <summary>
                Advanced settings{' '}
                <span>
                  {groups
                    .filter((name) => !document.groups[name]?.admin)
                    .join(', ') || 'Administrators only'}
                </span>
              </summary>
              <div className="grid gap-4 pt-4">
                <label className="field">
                  Backend name
                  <Input
                    aria-label="Backend name"
                    placeholder="e.g. Physics GPU cluster"
                    maxLength={128}
                    value={name}
                    onChange={(event) => setName(event.target.value)}
                  />
                </label>
                <fieldset className="backend-fieldset">
                  <legend>Who can use this backend?</legend>
                  <div className="backend-groups">
                    {Object.entries(document.groups).map(([name, group]) => (
                      <label key={name}>
                        <input
                          type="checkbox"
                          aria-label={'Backend access ' + name}
                          checked={group.admin || groups.includes(name)}
                          disabled={group.admin}
                          onChange={(event) =>
                            setGroups(
                              event.target.checked
                                ? [...groups, name]
                                : groups.filter((value) => value !== name),
                            )
                          }
                        />
                        <span>
                          {name}
                          {group.admin && <small>Administrator</small>}
                        </span>
                      </label>
                    ))}
                  </div>
                  <p className="subtitle">
                    Administrators always have access. With no groups selected,
                    only administrators can use this backend.
                  </p>
                </fieldset>
                <label className="field">
                  Shared backend limits
                  <select
                    aria-label="Backend preset"
                    value={preset}
                    onChange={(event) => setPreset(event.target.value)}
                  >
                    {entry && (
                      <option value="">Keep existing route limits</option>
                    )}
                    {Object.entries(document.backend_presets ?? {}).map(
                      ([id, preset]) => (
                        <option key={id} value={id}>
                          {preset.name}
                        </option>
                      ),
                    )}
                  </select>
                  <span className="subtitle">
                    One budget across all users and router instances. Edit
                    presets in{' '}
                    <Link
                      to="/admin/presets"
                      onClick={(event) => {
                        if (dirty) {
                          event.preventDefault()
                          setError(
                            'Save or discard this backend before leaving to edit presets.',
                          )
                        }
                      }}
                    >
                      Backend presets
                    </Link>
                    .
                  </span>
                </label>
                <label>
                  <input
                    type="checkbox"
                    checked={privateNetwork}
                    onChange={(event) =>
                      setPrivateNetwork(event.target.checked)
                    }
                  />{' '}
                  Internal network · allow HTTP
                </label>

                <label className="field">
                  Health path
                  <Input
                    aria-label="Backend health path"
                    placeholder={catalog?.health_path || '/v1/models'}
                    value={healthPath}
                    onChange={(event) => setHealthPath(event.target.value)}
                  />
                </label>
                <label>
                  <input
                    type="checkbox"
                    checked={anthropic}
                    onChange={(event) => setAnthropic(event.target.checked)}
                  />{' '}
                  Also expose the Anthropic protocol
                </label>
              </div>
            </details>
            {error && (
              <div className="error whitespace-pre-line" role="alert">
                {error}
              </div>
            )}
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                disabled={saving}
                onClick={close}
              >
                Cancel
              </Button>
              <Button
                type="submit"
                disabled={
                  saving ||
                  !origin ||
                  (useKey && !stored && (!encryption || !key))
                }
              >
                {saving ? 'Saving…' : 'Save backend'}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
      <AlertDialog open={discard} onOpenChange={setDiscard}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Discard backend changes?</AlertDialogTitle>
            <AlertDialogDescription>
              Your unsaved changes will be lost.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep editing</AlertDialogCancel>
            <AlertDialogAction onClick={onClose}>
              Discard changes
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
