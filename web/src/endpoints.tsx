import { useEffect, useRef, useState, type FormEvent } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Plus, Settings2, Trash2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { UpstreamModels } from './upstream-models'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import {
  api,
  ApiError,
  type EndpointEntry,
  type EndpointModel,
  type EndpointSettings,
  type Replica,
} from '@/lib/api'

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
const failure = (error: unknown) =>
  error instanceof Error
    ? error
    : new Error('Endpoint settings could not be updated.')
type TestResult = { ok: boolean; message: string; replicas: Replica[] }

function Editor({
  entry,
  credentials,
  onSaved,
  onReload,
  onPending,
}: {
  entry: EndpointEntry
  credentials: string[]
  onSaved: () => Promise<void>
  onReload: () => Promise<void>
  onPending: (pending: boolean) => void
}) {
  const [replicas, setReplicas] = useState(() =>
    entry.configuration.pool.map((value, index) => ({ id: index, value })),
  )
  const nextID = useRef(replicas.length)
  const [upstream, setUpstream] = useState(
    entry.configuration.upstream_model || '',
  )
  const sources = JSON.stringify(
    replicas.map(({ value }) => ({
      url: value.url,
      bearer_token_env: value.bearer_token_env || '',
    })),
  )
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<Error | null>(null)
  const [result, setResult] = useState<TestResult | null>(null)
  function configuration(form: HTMLFormElement): EndpointModel {
    const fields = new FormData(form)
    const text = (key: string) => String(fields.get(key) ?? '').trim()
    const number = (key: string) => Number(fields.get(key))
    return {
      ...entry.configuration,
      upstream_model: text('upstream_model'),
      protocols: fields.getAll('protocol') as EndpointModel['protocols'],
      timeout_s: number('timeout_s'),
      idle_timeout_s: number('idle_timeout_s'),
      queue_timeout_s: number('queue_timeout_s'),
      session_affinity: fields.has('session_affinity'),
      pool: replicas.map(({ id }) => ({
        url: text(`url.${id}`),
        health_path: text(`health_path.${id}`),
        weight: number(`weight.${id}`),
        max_inflight: number(`max_inflight.${id}`),
        bearer_token_env: text(`bearer_token_env.${id}`),
      })),
    }
  }
  async function act(form: HTMLFormElement, test: boolean) {
    if (!form.reportValidity()) return
    setPending(true)
    onPending(true)
    setError(null)
    setResult(null)
    try {
      const model = configuration(form)
      if (test)
        setResult(
          await api<TestResult>('admin/endpoints/test', 'POST', {
            model: entry.model,
            configuration: model,
          }),
        )
      else {
        await api('admin/endpoints', 'PUT', {
          model: entry.model,
          revision: entry.revision,
          configuration: model,
        })
        await onSaved()
      }
    } catch (error) {
      setError(failure(error))
    } finally {
      setPending(false)
      onPending(false)
    }
  }
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    void act(event.currentTarget, false)
  }
  return (
    <form
      onSubmit={submit}
      onChange={() => setResult(null)}
      aria-label="Edit model endpoints"
      className="grid gap-4"
    >
      <Failure error={error} />
      {error instanceof ApiError && error.status === 409 && (
        <Button type="button" variant="outline" onClick={() => void onReload()}>
          Discard draft and reload current configuration
        </Button>
      )}
      <fieldset disabled={pending} className="grid gap-4 min-w-0">
        <label className="grid gap-2 text-sm">
          Upstream model name
          <Input
            name="upstream_model"
            aria-label="Upstream model name"
            value={upstream}
            onChange={(event) => setUpstream(event.target.value)}
            placeholder={entry.model}
            maxLength={256}
          />
          <span className="subtitle">
            Use the name served by Ollama or vLLM. Empty forwards {entry.model}.
          </span>
        </label>
        <UpstreamModels
          key={sources}
          model={entry.model}
          sources={sources}
          value={upstream}
          onChoose={setUpstream}
        />
        <fieldset className="grid gap-2 rounded-md border p-3">
          <legend className="px-1 text-sm">Native backend protocols</legend>
          {(['openai', 'anthropic'] as const).map((protocol) => (
            <label key={protocol} className="flex gap-2 items-center text-sm">
              <input
                type="checkbox"
                name="protocol"
                value={protocol}
                defaultChecked={entry.configuration.protocols.includes(
                  protocol,
                )}
                className="h-4 w-4"
              />
              {protocol === 'openai' ? 'OpenAI' : 'Anthropic'}
            </label>
          ))}
          <p className="subtitle">
            The router forwards native APIs. Enable only protocols provided by
            this backend.
          </p>
        </fieldset>
        <div className="grid gap-3 sm:grid-cols-3">
          {(
            [
              ['timeout_s', 'Header timeout (seconds)', 1],
              ['idle_timeout_s', 'Idle timeout (seconds)', 1],
              ['queue_timeout_s', 'Queue timeout (seconds)', 0],
            ] as const
          ).map(([key, label, min]) => (
            <label key={key} className="grid gap-2 text-sm">
              {label}
              <Input
                name={key}
                type="number"
                required
                min={min}
                step="1"
                defaultValue={entry.configuration[key]}
              />
            </label>
          ))}
        </div>
        <label className="flex gap-2 items-center text-sm">
          <input
            type="checkbox"
            name="session_affinity"
            defaultChecked={entry.configuration.session_affinity}
            className="h-4 w-4"
          />
          Prefer the same replica for a session
        </label>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <h3 className="font-semibold">Pool replicas</h3>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={(event) => {
              const form = event.currentTarget.form!
              for (const { id } of replicas) {
                ;(
                  form.elements.namedItem(
                    `health_path.${id}`,
                  ) as HTMLInputElement
                ).value = '/'
                ;(
                  form.elements.namedItem(
                    `max_inflight.${id}`,
                  ) as HTMLInputElement
                ).value = '1'
              }
              ;(
                form.elements.namedItem('idle_timeout_s') as HTMLInputElement
              ).value = '120'
              ;(
                form.elements.namedItem('timeout_s') as HTMLInputElement
              ).value = '900'
              setResult(null)
            }}
          >
            Use Ollama health and capacity defaults
          </Button>
        </div>
        {replicas.map(({ id, value }, index) => (
          <fieldset
            key={id}
            className="rounded-md border p-3 grid gap-3 min-w-0"
            aria-label={`Replica ${index + 1}`}
          >
            <legend className="px-1 text-sm">Replica {index + 1}</legend>
            <label className="grid gap-2 text-sm">
              Backend origin
              <Input
                name={`url.${id}`}
                type="url"
                required
                placeholder="http://ollama:11434"
                value={value.url}
                onChange={(event) =>
                  setReplicas((current) =>
                    current.map((replica) =>
                      replica.id === id
                        ? {
                            ...replica,
                            value: {
                              ...replica.value,
                              url: event.target.value,
                            },
                          }
                        : replica,
                    ),
                  )
                }
                maxLength={512}
              />
            </label>
            <p className="subtitle">
              Origin only, without /v1. Ollama health path: /; vLLM usually:
              /health.
            </p>
            <div className="grid gap-3 sm:grid-cols-3">
              <label className="grid gap-2 text-sm">
                Health path
                <Input
                  name={`health_path.${id}`}
                  defaultValue={value.health_path}
                  placeholder="/health"
                  maxLength={256}
                />
              </label>
              <label className="grid gap-2 text-sm">
                Weight
                <Input
                  name={`weight.${id}`}
                  type="number"
                  required
                  min="0.000001"
                  step="any"
                  defaultValue={value.weight}
                />
              </label>
              <label className="grid gap-2 text-sm">
                Maximum in-flight
                <Input
                  name={`max_inflight.${id}`}
                  type="number"
                  required
                  min="1"
                  step="1"
                  defaultValue={value.max_inflight}
                />
              </label>
            </div>
            <label className="grid gap-2 text-sm">
              Backend credential reference
              <select
                name={`bearer_token_env.${id}`}
                className="select"
                value={value.bearer_token_env || ''}
                onChange={(event) =>
                  setReplicas((current) =>
                    current.map((replica) =>
                      replica.id === id
                        ? {
                            ...replica,
                            value: {
                              ...replica.value,
                              bearer_token_env: event.target.value,
                            },
                          }
                        : replica,
                    ),
                  )
                }
              >
                <option value="">No backend authentication</option>
                {credentials.map((name) => (
                  <option key={name} value={name}>
                    {name}
                  </option>
                ))}
              </select>
            </label>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="justify-self-start"
              disabled={replicas.length === 1}
              onClick={() => {
                setReplicas((current) =>
                  current.filter((replica) => replica.id !== id),
                )
                setResult(null)
              }}
              aria-label={`Remove replica ${index + 1}`}
            >
              <Trash2 />
              Remove replica
            </Button>
          </fieldset>
        ))}
        <Button
          type="button"
          variant="outline"
          className="justify-self-start"
          disabled={replicas.length >= 32}
          onClick={() => {
            setReplicas((current) => [
              ...current,
              {
                id: nextID.current++,
                value: {
                  url: '',
                  health_path: '/health',
                  weight: 1,
                  max_inflight: 1,
                  bearer_token_env: '',
                },
              },
            ])
            setResult(null)
          }}
        >
          <Plus />
          Add replica
        </Button>
      </fieldset>
      {result && (
        <section
          aria-label="Connection test results"
          className="rounded-md border p-3 text-sm"
        >
          <p role="status">{result.message}</p>
          <ul className="mt-2 grid gap-1">
            {result.replicas.map((replica) => (
              <li key={replica.url} className="break-all">
                {replica.url}: {replica.healthy ? 'Healthy' : 'Failed'}
              </li>
            ))}
          </ul>
          <p className="subtitle">
            This checks the health path. The upstream model must also exist on
            the backend. Save tests connectivity again.
          </p>
        </section>
      )}
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          disabled={pending}
          onClick={(event) => void act(event.currentTarget.form!, true)}
        >
          {pending ? 'Checking…' : 'Test connections'}
        </Button>
        <Button type="submit" disabled={pending}>
          {pending ? 'Working…' : 'Save and apply'}
        </Button>
      </DialogFooter>
    </form>
  )
}

export function EndpointSettingsPanel() {
  const cache = useQueryClient()
  const query = useQuery({
    queryKey: ['admin-endpoints'],
    queryFn: () => api<EndpointSettings>('admin/endpoints'),
  })
  const [selected, setSelected] = useState<EndpointEntry | null>(null)
  const [editorPending, setEditorPending] = useState(false)
  const [restore, setRestore] = useState<{
    entry: EndpointEntry
    target: number
    configuration: EndpointModel
  } | null>(null)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<Error | null>(null)
  const [saved, setSaved] = useState(false)
  async function refresh() {
    setSelected(null)
    setSaved(true)
    await Promise.all([
      cache.invalidateQueries({ queryKey: ['admin-endpoints'] }),
      cache.invalidateQueries({ queryKey: ['admin-replicas'] }),
      cache.invalidateQueries({ queryKey: ['models'] }),
    ])
  }
  async function restoreRevision() {
    if (!restore) return
    setPending(true)
    setError(null)
    setSaved(false)
    try {
      await api('admin/endpoints/restore', 'POST', {
        model: restore.entry.model,
        revision: restore.entry.revision,
        target_revision: restore.target,
      })
      await refresh()
    } catch (error) {
      setError(failure(error))
    } finally {
      setPending(false)
      setRestore(null)
    }
  }
  return (
    <section className="panel mb-6" aria-label="Endpoint configuration">
      <div className="panel-head">
        <div>
          <h2 className="panel-title">Endpoint configuration</h2>
          <p className="subtitle">
            Groups grant access to a public model; its pool connects that model
            to backend replicas.
          </p>
        </div>
      </div>
      <div className="p-5 grid gap-5">
        <Failure error={query.error ?? error} />
        {query.isPending && (
          <p role="status">Loading endpoint configuration…</p>
        )}
        {saved && (
          <p role="status">
            Endpoint configuration applied. New requests use the active
            revision; existing streams continue.
          </p>
        )}
        {query.data?.models.map((entry) => (
          <article
            key={entry.model}
            className="rounded-md border p-4 min-w-0"
            aria-label={`Endpoints for ${entry.model}`}
          >
            <div className="flex flex-wrap items-center justify-between gap-3">
              <div>
                <h3 className="font-semibold break-all">{entry.model}</h3>
                <p className="subtitle">
                  Active revision: {entry.revision || 'initial'} · Source:{' '}
                  {entry.source === 'file'
                    ? 'YAML/environment'
                    : 'Admin database'}{' '}
                  · Upstream:{' '}
                  {entry.configuration.upstream_model || entry.model}
                </p>
              </div>
              <Button
                variant="outline"
                size="sm"
                disabled={pending}
                onClick={() => {
                  setSelected(entry)
                  setSaved(false)
                }}
                aria-label={`Edit endpoints for ${entry.model}`}
              >
                <Settings2 />
                Edit endpoints
              </Button>
            </div>
            <ul className="mt-3 grid gap-1 text-sm">
              {entry.configuration.pool.map((replica) => (
                <li key={replica.url} className="break-all">
                  {replica.url} · health {replica.health_path || '/health'} ·
                  weight {replica.weight} · capacity {replica.max_inflight} ·{' '}
                  {replica.bearer_token_env
                    ? `credential: ${replica.bearer_token_env}`
                    : 'no backend authentication'}
                </li>
              ))}
            </ul>
            {entry.revision > 0 && (
              <details className="mt-4 text-sm">
                <summary className="cursor-pointer">
                  Revision history and restore
                </summary>
                <p className="subtitle">
                  Restoring creates a new revision after checking the target
                  endpoints.
                </p>
                <div className="mt-3 grid gap-2">
                  {entry.history
                    .filter((revision) => revision.id !== entry.revision)
                    .map((revision) => (
                      <div
                        key={revision.id}
                        className="flex flex-wrap items-center gap-2"
                      >
                        <span>
                          Revision {revision.id} ·{' '}
                          {new Date(revision.created_at).toLocaleString()}
                          {revision.file ? ' · file configuration' : ''}
                        </span>
                        <Button
                          variant="outline"
                          size="sm"
                          disabled={pending}
                          onClick={() =>
                            setRestore({
                              entry,
                              target: revision.id,
                              configuration:
                                revision.configuration ??
                                entry.file_configuration,
                            })
                          }
                          aria-label={`Restore ${entry.model} revision ${revision.id}`}
                        >
                          Restore
                        </Button>
                      </div>
                    ))}
                  <Button
                    variant="outline"
                    size="sm"
                    className="justify-self-start"
                    disabled={pending}
                    onClick={() =>
                      setRestore({
                        entry,
                        target: 0,
                        configuration: entry.file_configuration,
                      })
                    }
                    aria-label={`Restore ${entry.model} from file`}
                  >
                    Restore current YAML/environment configuration
                  </Button>
                </div>
              </details>
            )}
          </article>
        ))}
        <p className="subtitle">
          Edits survive restarts and apply immediately. Logical model names,
          aliases and group policies are configured in YAML. Private networks
          allowed for new endpoints:{' '}
          {query.data?.allowed_private_networks.join(', ') ||
            'none beyond file-defined destinations'}
          . Backend credentials are server-defined references; their values are
          never displayed.
        </p>
      </div>
      <Dialog
        open={!!selected}
        onOpenChange={(open) => {
          if (!open && !editorPending) setSelected(null)
        }}
      >
        <DialogContent className="sm:max-w-3xl max-h-[calc(100dvh-2rem)] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>Edit endpoints: {selected?.model}</DialogTitle>
            <DialogDescription>
              Save validates every replica and activates a complete model
              configuration. Concurrent edits are rejected. Running streams and
              user quotas continue.
            </DialogDescription>
          </DialogHeader>
          {selected && (
            <Editor
              key={`${selected.model}:${selected.revision}`}
              entry={selected}
              credentials={query.data?.credential_references ?? []}
              onSaved={refresh}
              onPending={setEditorPending}
              onReload={async () => {
                const fresh = await query.refetch()
                const entry = fresh.data?.models.find(
                  (entry) => entry.model === selected.model,
                )
                if (entry) setSelected(entry)
              }}
            />
          )}
        </DialogContent>
      </Dialog>
      <AlertDialog
        open={!!restore}
        onOpenChange={(open) => {
          if (!open && !pending) setRestore(null)
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Restore endpoint configuration?</AlertDialogTitle>
            <AlertDialogDescription>
              This restores {restore?.entry.model} from{' '}
              {restore?.target
                ? `revision ${restore.target}`
                : 'the current YAML/environment configuration'}{' '}
              as a new active revision.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="text-sm grid gap-2">
            <p>
              Upstream model:{' '}
              {restore?.configuration.upstream_model || restore?.entry.model}
            </p>
            {restore?.configuration.pool.map((replica) => (
              <p key={replica.url} className="break-all">
                {replica.url} · health {replica.health_path || '/health'} ·
                capacity {replica.max_inflight}
              </p>
            ))}
          </div>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={pending}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={pending}
              onClick={() => void restoreRevision()}
            >
              Restore and apply
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  )
}
