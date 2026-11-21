import { useId, useRef, useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Database } from 'lucide-react'
import { api, ApiError } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

type Quota = { max_bytes: number; max_traces: number }
type QuotaView = {
  version: number
  overrides: { max_bytes: number | null; max_traces: number | null }
  defaults: Quota
  effective: Quota
  bounds: { max_mib: number; max_traces: number }
  storage?: { bytes: number; traces: number; next_expiry: string | null }
}
const mib = 1024 * 1024
const number = (value: number) => new Intl.NumberFormat().format(value)

export function TraceQuotas({ user }: { user?: string }) {
  const cache = useQueryClient()
  const section = useRef<HTMLElement>(null)
  const path = user ? `admin/users/${user}/trace-quota` : 'admin/trace-quotas'
  const key = ['admin-trace-quota', user ?? 'defaults']
  const query = useQuery({ queryKey: key, queryFn: () => api<QuotaView>(path) })
  const [saved, setSaved] = useState(false)
  return (
    <section
      ref={section}
      className="panel"
      aria-label={user ? 'User log quota' : 'Default log quotas'}
    >
      <div className="panel-head">
        <div>
          <h2 className="panel-title flex items-center gap-2">
            <Database size={20} aria-hidden="true" />
            {user ? 'Detailed log quota' : 'Request logging'}
          </h2>
          <p className="subtitle">
            {user
              ? 'Override the global defaults for this account. Leave a field empty to inherit its default.'
              : 'Set the default storage quota for each user. Individual user overrides take precedence.'}
          </p>
        </div>
      </div>
      <div className="p-6 space-y-4">
        {query.error && (
          <p className="error" role="alert">
            {query.error.message}
          </p>
        )}
        {saved && (
          <p role="status">
            {user ? 'User quota saved.' : 'Default quotas saved.'} Changes apply
            immediately to new captures.
          </p>
        )}
        {query.data?.effective && (
          <QuotaForm
            key={path}
            view={query.data}
            user={user}
            path={path}
            onReload={async () => {
              setSaved(false)
              const result = await query.refetch()
              return result.isError ? undefined : result.data
            }}
            onSaved={async (view) => {
              setSaved(true)
              requestAnimationFrame(() =>
                section.current?.querySelector('input')?.focus(),
              )
              cache.setQueryData(key, view)
              await cache.invalidateQueries({ queryKey: ['trace-preferences'] })
              await cache.invalidateQueries({ queryKey: ['admin-history'] })
              if (!user)
                await cache.invalidateQueries({
                  queryKey: ['admin-trace-quota'],
                  predicate: (q) => q.queryKey[1] !== 'defaults',
                })
            }}
          />
        )}
        {query.isPending && <p role="status">Loading log quotas…</p>}
      </div>
    </section>
  )
}

function QuotaForm({
  view: latest,
  user,
  path,
  onSaved,
  onReload,
}: {
  view: QuotaView
  user?: string
  path: string
  onSaved: (view: QuotaView) => Promise<void>
  onReload: () => Promise<QuotaView | undefined>
}) {
  const id = useId()
  // Keep the version and draft loaded by this editor across background fetches.
  // Only a successful save or an explicit reload replaces them.
  const [view, setView] = useState(latest)
  const storageInput = useRef<HTMLInputElement>(null)
  const countInput = useRef<HTMLInputElement>(null)
  const [storage, setStorage] = useState(
    view.overrides.max_bytes === null
      ? ''
      : String(view.overrides.max_bytes / mib),
  )
  const [count, setCount] = useState(
    view.overrides.max_traces === null ? '' : String(view.overrides.max_traces),
  )
  const [invalid, setInvalid] = useState<'storage' | 'count' | null>(null)
  const [error, setError] = useState('')
  const save = useMutation({
    mutationFn: (values: {
      max_bytes: number | null
      max_traces: number | null
    }) => api<QuotaView>(path, 'PUT', { version: view.version, ...values }),
    onSuccess: async (next) => {
      reset(next)
      await onSaved(next)
    },
  })
  function reset(next: QuotaView) {
    setView(next)
    setStorage(
      next.overrides.max_bytes === null
        ? ''
        : String(next.overrides.max_bytes / mib),
    )
    setCount(
      next.overrides.max_traces === null
        ? ''
        : String(next.overrides.max_traces),
    )
    setError('')
    setInvalid(null)
  }
  const changed =
    latest.version !== view.version ||
    latest.defaults.max_bytes !== view.defaults.max_bytes ||
    latest.defaults.max_traces !== view.defaults.max_traces
  function submit(event: FormEvent) {
    event.preventDefault()
    setError('')
    setInvalid(null)
    for (const [field, value, maximum, input] of [
      ['storage', storage, view.bounds.max_mib, storageInput],
      ['count', count, view.bounds.max_traces, countInput],
    ] as const) {
      if (
        value.trim() &&
        (!/^\d+$/.test(value.trim()) ||
          !Number.isSafeInteger(Number(value)) ||
          Number(value) < 1 ||
          Number(value) > maximum)
      ) {
        setInvalid(field)
        setError(
          `Enter a whole number from 1 to ${number(maximum)}, or leave empty to inherit the default.`,
        )
        input.current?.focus()
        return
      }
    }
    save.mutate({
      max_bytes: storage.trim() ? Number(storage) * mib : null,
      max_traces: count.trim() ? Number(count) : null,
    })
  }
  return (
    <form
      aria-label={user ? 'User log quota' : 'Default log quotas'}
      onSubmit={submit}
      noValidate
      className="space-y-4"
    >
      <p className="text-sm text-muted-foreground">
        Effective limit:{' '}
        <strong>{number(latest.effective.max_bytes / mib)} MiB</strong> and{' '}
        <strong>{number(latest.effective.max_traces)} traces</strong> per
        account. Capture stops when either limit is reached.
      </p>
      {latest.storage && (
        <div
          className="rounded-md bg-muted p-3 text-sm"
          role="group"
          aria-label="Current log storage"
        >
          Stored: {number(latest.storage.bytes / mib)} MiB ·{' '}
          {number(latest.storage.traces)} traces.
          {(latest.storage.bytes >= latest.effective.max_bytes ||
            latest.storage.traces >= latest.effective.max_traces) && (
            <p>Quota reached. Increase the limit to resume new captures.</p>
          )}
          {latest.storage.next_expiry && (
            <p>
              Next expiry:{' '}
              {new Date(latest.storage.next_expiry).toLocaleString()}.
            </p>
          )}
        </div>
      )}
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
        <div className="space-y-2">
          <label htmlFor={`${id}-storage`}>Maximum storage (MiB)</label>
          <Input
            id={`${id}-storage`}
            ref={storageInput}
            inputMode="numeric"
            value={storage}
            onChange={(e) => setStorage(e.target.value)}
            placeholder={String(view.defaults.max_bytes / mib)}
            disabled={save.isPending}
            aria-invalid={invalid === 'storage'}
            aria-describedby={`${id}-storage-help${invalid === 'storage' ? ` ${id}-error` : ''}`}
          />
          <p
            id={`${id}-storage-help`}
            className="text-sm text-muted-foreground"
          >
            {user ? 'Global' : 'Deployment'} default:{' '}
            {number(view.defaults.max_bytes / mib)} MiB. 1 MiB = 1,048,576
            bytes.
          </p>
        </div>
        <div className="space-y-2">
          <label htmlFor={`${id}-count`}>Maximum stored traces</label>
          <Input
            id={`${id}-count`}
            ref={countInput}
            inputMode="numeric"
            value={count}
            onChange={(e) => setCount(e.target.value)}
            placeholder={String(view.defaults.max_traces)}
            disabled={save.isPending}
            aria-invalid={invalid === 'count'}
            aria-describedby={`${id}-count-help${invalid === 'count' ? ` ${id}-error` : ''}`}
          />
          <p id={`${id}-count-help`} className="text-sm text-muted-foreground">
            {user ? 'Global' : 'Deployment'} default:{' '}
            {number(view.defaults.max_traces)} traces.
          </p>
        </div>
      </div>
      <p className="text-sm text-muted-foreground">
        Saving changes preserves existing logs, retention and user opt-in.
        Lowering a quota pauses new captures until storage is below both limits.
        Site policy must also allow detailed logging.
      </p>
      {error && (
        <p id={`${id}-error`} className="error" role="alert">
          {error}
        </p>
      )}
      {save.error && (
        <p className="error" role="alert">
          {save.error.message}
        </p>
      )}
      {changed && (
        <p role="status">
          Saved quotas changed while you were editing. Your draft is preserved.
          Reload saved quotas to use the latest values.
        </p>
      )}
      <div className="flex flex-wrap gap-3">
        <Button
          type="submit"
          disabled={
            save.isPending ||
            changed ||
            (save.error instanceof ApiError && save.error.status === 409)
          }
        >
          {save.isPending
            ? 'Saving…'
            : user
              ? 'Save user quota'
              : 'Save defaults'}
        </Button>
        <Button
          type="button"
          variant="outline"
          disabled={save.isPending}
          onClick={() => {
            setStorage('')
            setCount('')
            setInvalid(null)
            setError('')
          }}
        >
          Use {user ? 'global' : 'deployment'} defaults
        </Button>
        {(changed ||
          (save.error instanceof ApiError && save.error.status === 409)) && (
          <Button
            type="button"
            variant="outline"
            onClick={async () => {
              const next = await onReload()
              if (next) {
                reset(next)
                save.reset()
                storageInput.current?.focus()
              }
            }}
          >
            Reload saved quotas
          </Button>
        )}
      </div>
    </form>
  )
}
