import { useEffect, useState, type FormEvent } from 'react'
import { NavLink, Navigate, Route, Routes } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  ArrowUpRight,
  Check,
  CheckCircle2,
  Code2,
  Copy,
  KeyRound,
  Layers3,
  LogOut,
  Moon,
  Plus,
  RefreshCw,
  ShieldCheck,
  Sun,
  Trash2,
  BarChart3,
  ListOrdered,
} from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
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
  redirect,
  type Key,
  type Me,
  type Models,
  type Usage,
} from '@/lib/api'
import { clients, snippet, type Client } from '@/lib/snippets'
import { AdminPage, DailyChart, RequestsPage } from '@/pages'

const number = (value: number) => new Intl.NumberFormat().format(value)
const date = (value: string | null) =>
  value
    ? new Date(value).toLocaleDateString(undefined, {
        year: 'numeric',
        month: 'short',
        day: 'numeric',
      })
    : 'Never'
const reset = (value: string) =>
  new Date(value).toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    timeZoneName: 'short',
  })

function ErrorMessage({
  error,
  retry,
}: {
  error: Error | null
  retry?: () => void
}) {
  return (
    error && (
      <div className="error" role="alert">
        {error.message}{' '}
        {retry && (
          <button className="underline ml-2" onClick={retry}>
            Try again
          </button>
        )}
      </div>
    )
  )
}

function CopyButton({
  text,
  label = 'Copy',
  disabled = false,
}: {
  text: string
  label?: string
  disabled?: boolean
}) {
  const [copied, setCopied] = useState(false)
  const [error, setError] = useState(false)
  useEffect(() => {
    if (copied) {
      const id = window.setTimeout(() => setCopied(false), 2000)
      return () => window.clearTimeout(id)
    }
  }, [copied])
  return (
    <span className="inline-flex items-center gap-2">
      <Button
        variant="outline"
        size="sm"
        disabled={disabled}
        onClick={async () => {
          try {
            await navigator.clipboard.writeText(text)
            setCopied(true)
            setError(false)
          } catch {
            setError(true)
          }
        }}
      >
        {copied ? <Check /> : <Copy />}
        {copied ? 'Copied' : label}
      </Button>
      <span role="status" className="text-xs text-muted-foreground">
        {error
          ? 'Select and copy the text manually.'
          : copied
            ? 'Copied to clipboard'
            : ''}
      </span>
    </span>
  )
}

export default function App() {
  const me = useQuery({ queryKey: ['me'], queryFn: () => api<Me>('me') })
  const [dark, setDark] = useState(
    () => window.matchMedia('(prefers-color-scheme: dark)').matches,
  )
  const [logoutError, setLogoutError] = useState<Error | null>(null)
  const logout = useMutation({
    mutationFn: async () => {
      const result = await api<{ logout_url: string }>('/portal/logout', 'POST')
      redirect(result.logout_url)
    },
    onError: setLogoutError,
  })
  useEffect(() => {
    document.documentElement.classList.toggle('dark', dark)
  }, [dark])
  return (
    <div className="shell">
      <a
        className="sr-only focus:not-sr-only focus:fixed focus:top-2 focus:left-2 bg-background p-3 z-50"
        href="#main"
      >
        Skip to content
      </a>
      <aside className="sidebar">
        <div className="brand">
          <Layers3 className="brand-mark" size={39} />
          <span>
            Open LLM
            <br />
            <span className="font-normal text-muted-foreground text-xs tracking-normal">
              ROUTER PORTAL
            </span>
          </span>
        </div>
        <nav aria-label="Main navigation" className="grid gap-1">
          <NavLink to="/keys" className="nav-link">
            <KeyRound size={18} />
            API keys
          </NavLink>
          <NavLink to="/usage" className="nav-link">
            <BarChart3 size={18} />
            Usage
          </NavLink>
          <NavLink to="/requests" className="nav-link">
            <ListOrdered size={18} />
            My requests
          </NavLink>
          <NavLink to="/connect" className="nav-link">
            <Code2 size={18} />
            Connect
          </NavLink>
          {me.data?.admin && (
            <NavLink to="/admin" className="nav-link">
              <ShieldCheck size={18} />
              Admin
            </NavLink>
          )}
        </nav>
        <div className="account">
          <div className="flex items-center gap-2 mb-3">
            <span className="rounded-full bg-accent p-2 text-accent-foreground">
              <ShieldCheck size={17} />
            </span>
            <div>
              <div className="font-semibold">
                {me.data?.user.username ?? 'Your account'}
              </div>
              <div className="text-muted-foreground text-xs mt-1">
                {me.data?.user.groups.join(', ') || 'Personal workspace'}
              </div>
            </div>
          </div>
          <span className="text-muted-foreground text-xs">
            Private access to your models.
          </span>
        </div>
      </aside>
      <div className="workspace">
        <header className="topbar">
          <div className="text-sm text-muted-foreground">
            Workspace <span className="mx-2 text-border">/</span>
            <span className="text-foreground font-medium">
              {me.data?.user.username ?? 'Personal'}
            </span>
          </div>
          <div className="flex gap-1">
            <Button
              variant="ghost"
              size="icon"
              aria-label={
                dark ? 'Switch to light theme' : 'Switch to dark theme'
              }
              onClick={() => setDark(!dark)}
            >
              {dark ? <Sun /> : <Moon />}
            </Button>
            <Button
              variant="ghost"
              size="sm"
              disabled={logout.isPending}
              onClick={() => logout.mutate()}
            >
              <LogOut />
              Sign out
            </Button>
          </div>
        </header>
        <main id="main">
          <ErrorMessage error={logoutError} />
          <ErrorMessage
            error={me.error}
            retry={() => {
              void me.refetch()
            }}
          />
          {me.isPending ? (
            <p role="status" className="subtitle">
              Loading your workspace…
            </p>
          ) : (
            me.data && (
              <Routes>
                <Route path="/keys" element={<Keys />} />
                <Route path="/usage" element={<UsagePage />} />
                <Route path="/connect" element={<Connect />} />
                <Route path="/requests" element={<RequestsPage />} />
                {me.data.admin && (
                  <Route
                    path="/admin"
                    element={<AdminPage self={me.data.user.id} />}
                  />
                )}
                <Route path="*" element={<Navigate to="/keys" replace />} />
              </Routes>
            )
          )}
        </main>
      </div>
    </div>
  )
}

function Keys() {
  const query = useQuery({
    queryKey: ['keys'],
    queryFn: () => api<{ keys: Key[] }>('keys'),
  })
  const cache = useQueryClient()
  const [open, setOpen] = useState(false)
  const [secret, setSecret] = useState('')
  const [label, setLabel] = useState('')
  const [expiry, setExpiry] = useState('')
  const [keyClass, setKeyClass] = useState<'interactive' | 'batch'>(
    'interactive',
  )
  const [selected, setSelected] = useState<Key | null>(null)
  const create = useMutation({
    mutationFn: async () => {
      const result = await api<{ key: string }>('keys', 'POST', {
        label,
        class: keyClass,
        ...(expiry ? { expires_in_days: Number(expiry) } : {}),
      })
      setSecret(result.key) // The mutation cache receives no plaintext key.
      await cache.invalidateQueries({ queryKey: ['keys'] })
    },
  })
  const revoke = useMutation({
    mutationFn: async (id: string) => {
      await api(`keys/${id}`, 'DELETE')
    },
    onSuccess: async () => {
      setSelected(null)
      await cache.invalidateQueries({ queryKey: ['keys'] })
    },
  })
  function changeOpen(next: boolean) {
    if (create.isPending) return
    setOpen(next)
    setSecret('')
    setLabel('')
    setExpiry('')
    setKeyClass('interactive')
    create.reset()
  }
  function submit(event: FormEvent) {
    event.preventDefault()
    create.mutate()
  }
  const keys = query.data?.keys ?? []
  const removing =
    !!selected &&
    (selected.revoked ||
      (!!selected.expires && new Date(selected.expires) <= new Date()))
  const active = keys.filter(
    (key) =>
      !key.revoked && (!key.expires || new Date(key.expires) > new Date()),
  ).length
  return (
    <>
      <div className="page-head">
        <div>
          <div className="eyebrow">Your workspace</div>
          <h1>API keys</h1>
          <p className="subtitle">
            Secure access for your apps, scripts, and coding tools.
          </p>
        </div>
        <Button onClick={() => changeOpen(true)}>
          <Plus />
          Create key
        </Button>
      </div>
      <ErrorMessage
        error={query.error}
        retry={() => {
          void query.refetch()
        }}
      />
      <section className="panel" aria-label="Your API keys">
        <div className="panel-head">
          <div>
            <h2 className="panel-title">
              Your keys <span className="ml-2 badge">{active} active</span>
            </h2>
            <p className="subtitle">
              Keys are personal. Keep them out of source control.
            </p>
          </div>
          <KeyRound size={20} className="text-muted-foreground" />
        </div>
        {query.isPending ? (
          <p role="status" className="empty">
            Loading keys…
          </p>
        ) : keys.length === 0 ? (
          <div className="empty">
            <KeyRound className="mx-auto mb-4" size={28} />
            <p className="font-medium text-foreground mb-2">
              Your first connection starts here
            </p>
            <p>Create a key, then visit Connect to set up your client.</p>
          </div>
        ) : (
          <div className="table-wrap">
            <table>
              <caption className="sr-only">Personal API keys</caption>
              <thead>
                <tr>
                  <th>Key</th>
                  <th>Created</th>
                  <th>Last used</th>
                  <th>Expires</th>
                  <th>Status</th>
                  <th>
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {keys.map((key) => {
                  const status = key.revoked
                    ? 'Revoked'
                    : key.expires && new Date(key.expires) <= new Date()
                      ? 'Expired'
                      : 'Active'
                  return (
                    <tr key={key.id}>
                      <td>
                        <div
                          className="font-medium mb-1 max-w-52 truncate"
                          title={key.label}
                        >
                          {key.label}
                        </div>
                        <code className="text-xs text-muted-foreground">
                          {key.prefix}…
                        </code>
                        {key.class === 'batch' && (
                          <span className="badge inactive ml-2">batch</span>
                        )}
                      </td>
                      <td>{date(key.created)}</td>
                      <td>{date(key.last_used)}</td>
                      <td>{date(key.expires)}</td>
                      <td>
                        <span
                          className={`badge ${status === 'Active' ? '' : 'inactive'}`}
                        >
                          {status === 'Active' && (
                            <span aria-hidden="true">●</span>
                          )}
                          {status}
                        </span>
                      </td>
                      <td>
                        <Button
                          variant="ghost"
                          size="icon"
                          aria-label={`${status === 'Active' ? 'Revoke' : 'Remove'} ${key.label}`}
                          title={
                            status === 'Active'
                              ? 'Revoke key'
                              : 'Remove key for good'
                          }
                          onClick={() => {
                            revoke.reset()
                            setSelected(key)
                          }}
                        >
                          <Trash2 size={15} />
                        </Button>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>
      <div className="hint">
        <ShieldCheck size={18} className="shrink-0 mt-1" />
        <p>
          Full keys appear only once, when you create them. Revoking a key takes
          effect immediately; a revoked key can then be removed for good.
          <br />
          <NavLink
            to="/connect"
            className="text-primary font-medium inline-flex items-center gap-1 mt-1"
          >
            Set up your first client <ArrowUpRight size={13} />
          </NavLink>
        </p>
      </div>
      <Dialog open={open} onOpenChange={changeOpen}>
        <DialogContent className="max-w-lg">
          <DialogHeader>
            <DialogTitle>
              {secret ? 'Your key is ready' : 'Create an API key'}
            </DialogTitle>
            <DialogDescription>
              {secret
                ? 'Copy this key now. You won’t be able to see it again.'
                : 'Give your key a name so you can recognize it later.'}
            </DialogDescription>
          </DialogHeader>
          {secret ? (
            <>
              <div className="rounded-lg bg-accent p-4 text-sm text-accent-foreground flex gap-3">
                <CheckCircle2 className="shrink-0" size={19} />
                Store it securely. Anyone with this key can use your quota.
              </div>
              <label className="field mt-2">
                Your new API key
                <textarea
                  data-testid="new-key"
                  className="select font-mono text-xs resize-none"
                  readOnly
                  value={secret}
                  rows={2}
                />
              </label>
              <CopyButton text={secret} label="Copy key" />
              <DialogFooter>
                <Button onClick={() => changeOpen(false)}>
                  I’ve saved my key
                </Button>
              </DialogFooter>
            </>
          ) : (
            <form onSubmit={submit}>
              <ErrorMessage error={create.error} />
              <label className="field">
                Key label
                <Input
                  autoComplete="off"
                  placeholder="e.g. Work laptop"
                  value={label}
                  onChange={(e) => setLabel(e.target.value)}
                  required
                  maxLength={100}
                />
              </label>
              <label className="field">
                Expires after
                <select
                  className="select"
                  value={expiry}
                  onChange={(e) => setExpiry(e.target.value)}
                >
                  <option value="">Site default</option>
                  {[7, 30, 90, 365].map((days) => (
                    <option key={days} value={days}>
                      {days} days
                    </option>
                  ))}
                </select>
              </label>
              <label className="field">
                Use
                <select
                  className="select"
                  value={keyClass}
                  onChange={(e) =>
                    setKeyClass(e.target.value as 'interactive' | 'batch')
                  }
                >
                  <option value="interactive">
                    Interactive: chat, coding tools
                  </option>
                  <option value="batch">
                    Batch: scripts and bulk jobs (own concurrency limit)
                  </option>
                </select>
              </label>
              <DialogFooter>
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => changeOpen(false)}
                >
                  Cancel
                </Button>
                <Button
                  type="submit"
                  disabled={create.isPending || !label.trim()}
                >
                  {create.isPending ? 'Creating…' : 'Create key'}
                </Button>
              </DialogFooter>
            </form>
          )}
        </DialogContent>
      </Dialog>
      <AlertDialog
        open={!!selected}
        onOpenChange={(next) => {
          if (!next && !revoke.isPending) setSelected(null)
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {removing ? 'Remove this key for good?' : 'Revoke this key?'}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {removing
                ? `“${selected?.label}” no longer works. Removing it deletes it from your list; your usage records stay, without a reference to the key.`
                : `Apps using “${selected?.label}” will lose access immediately. This cannot be undone.`}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <ErrorMessage error={revoke.error} />
          <AlertDialogFooter>
            <AlertDialogCancel disabled={revoke.isPending}>
              Keep key
            </AlertDialogCancel>
            <AlertDialogAction
              disabled={revoke.isPending}
              onClick={(e) => {
                e.preventDefault()
                if (selected) revoke.mutate(selected.id)
              }}
            >
              {revoke.isPending
                ? removing
                  ? 'Removing…'
                  : 'Revoking…'
                : removing
                  ? 'Remove key'
                  : 'Revoke key'}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}

function Quota({
  title,
  used,
  limit,
  remaining,
  resetAt,
}: {
  title: string
  used: number
  limit: number | null
  remaining: number | null
  resetAt: string
}) {
  return (
    <div className="panel stat">
      <div className="text-sm text-muted-foreground">{title}</div>
      <div className="stat-value">
        {number(used)}
        <span className="text-sm font-normal text-muted-foreground ml-2">
          tokens used
        </span>
      </div>
      <progress
        className="quota-bar"
        aria-label={`${title} quota used`}
        value={limit ? Math.min(used, limit) : 0}
        max={limit ?? 1}
      />
      <p className="text-xs text-muted-foreground mt-3">
        {limit === null
          ? 'Unlimited allowance'
          : `${number(remaining ?? 0)} of ${number(limit)} remaining`}
      </p>
      <p className="text-xs text-muted-foreground mt-2">
        Resets {reset(resetAt)}
      </p>
    </div>
  )
}

function UsagePage() {
  const query = useQuery({
    queryKey: ['usage'],
    queryFn: () => api<Usage>('usage'),
    refetchInterval: 30_000,
  })
  const [period, setPeriod] = useState<'today' | 'month'>('today')
  const [confirmErase, setConfirmErase] = useState(false)
  const [erased, setErased] = useState<number | null>(null)
  const cache = useQueryClient()
  const erase = useMutation({
    mutationFn: () =>
      api<{ deleted: number }>('usage', 'DELETE').then((r) => r.deleted),
    onSuccess: async (deleted) => {
      setConfirmErase(false)
      setErased(deleted)
      await cache.invalidateQueries({ queryKey: ['usage'] })
      await cache.invalidateQueries({ queryKey: ['requests'] })
      await cache.invalidateQueries({ queryKey: ['daily'] })
    },
  })
  const data = query.data
  return (
    <>
      <div className="page-head">
        <div>
          <div className="eyebrow">Activity & allowance</div>
          <h1>Usage</h1>
          <p className="subtitle">
            Your model activity, performance, and remaining quota.
          </p>
        </div>
        <Button
          variant="outline"
          onClick={() => {
            void query.refetch()
          }}
          disabled={query.isFetching}
        >
          <RefreshCw size={15} />
          Refresh
        </Button>
      </div>
      <ErrorMessage
        error={query.error}
        retry={() => {
          void query.refetch()
        }}
      />
      {query.isPending ? (
        <p role="status" className="empty">
          Loading usage…
        </p>
      ) : (
        data && (
          <>
            <div className="stats">
              <Quota
                title="Today"
                used={data.quota.used.tokens_today}
                limit={data.quota.limits.tokens_per_day}
                remaining={data.quota.remaining.tokens_today}
                resetAt={data.quota.reset_at.day}
              />
              <Quota
                title="This month"
                used={data.quota.used.tokens_this_month}
                limit={data.quota.limits.tokens_per_month}
                remaining={data.quota.remaining.tokens_this_month}
                resetAt={data.quota.reset_at.month}
              />
              <div className="panel stat">
                <div className="text-sm text-muted-foreground">
                  Request limits
                </div>
                <div className="stat-value">
                  {data.quota.used.rpm ?? 0}
                  <span className="text-sm font-normal text-muted-foreground ml-2">
                    / {data.quota.limits.rpm ?? '∞'} per minute
                  </span>
                </div>
                <p className="text-xs text-muted-foreground">
                  {data.quota.remaining.rpm === null
                    ? 'Unlimited request allowance'
                    : `${number(data.quota.remaining.rpm)} requests remaining`}
                </p>
                <p className="text-xs text-muted-foreground mt-3">
                  {data.quota.in_flight - data.quota.in_flight_batch} /{' '}
                  {data.quota.limits.max_concurrent ?? '∞'} concurrent requests
                  · batch {data.quota.in_flight_batch} /{' '}
                  {data.quota.limits.max_concurrent_batch ?? '∞'}
                </p>
                {data.quota.limits.tpm !== null && (
                  <p className="text-xs text-muted-foreground mt-2">
                    {number(data.quota.remaining.tpm ?? 0)} of{' '}
                    {number(data.quota.limits.tpm)} tokens per minute available
                  </p>
                )}
                {data.quota.limits.strict_quota && (
                  <p className="text-xs text-muted-foreground mt-2">
                    Strict quota: each request reserves its max_tokens plus its
                    size until it ends ({number(data.quota.reserved_tokens)}{' '}
                    reserved).
                  </p>
                )}
                {data.quota.reset_at.rpm && (
                  <p className="text-xs text-muted-foreground mt-2">
                    Next request reset {reset(data.quota.reset_at.rpm)}
                  </p>
                )}
              </div>
            </div>
            <DailyChart />
            <section className="panel">
              <div className="panel-head">
                <h2 className="panel-title">Activity by model</h2>
                <div className="segmented" aria-label="Usage period">
                  {(['today', 'month'] as const).map((value) => (
                    <Button
                      key={value}
                      size="sm"
                      variant={period === value ? 'secondary' : 'ghost'}
                      aria-pressed={period === value}
                      onClick={() => setPeriod(value)}
                    >
                      {value === 'today' ? 'Today' : 'This month'}
                    </Button>
                  ))}
                </div>
              </div>
              {data[period].length === 0 ? (
                <p className="empty">
                  No activity yet for this period. Make your first request from
                  Connect.
                </p>
              ) : (
                <div className="table-wrap">
                  <table data-testid="usage-table">
                    <caption className="sr-only">
                      {period === 'today' ? 'Today' : 'This month'} usage by
                      model
                    </caption>
                    <thead>
                      <tr>
                        <th>Model</th>
                        <th>Requests</th>
                        <th>Input tokens</th>
                        <th>Output tokens</th>
                        <th>Avg. first token</th>
                        <th>Avg. tokens / sec</th>
                      </tr>
                    </thead>
                    <tbody>
                      {data[period].map((row) => (
                        <tr key={row.model}>
                          <td className="font-medium">{row.model}</td>
                          <td>{number(row.requests)}</td>
                          <td>{number(row.input_tokens)}</td>
                          <td>{number(row.output_tokens)}</td>
                          <td>
                            {row.average_ttfb_ms === null
                              ? '—'
                              : `${number(Math.round(row.average_ttfb_ms))} ms`}
                          </td>
                          <td>
                            {row.average_tokens_per_second === null
                              ? '—'
                              : row.average_tokens_per_second.toFixed(1)}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </section>
            <div className="hint">
              <BarChart3 size={18} className="shrink-0 mt-1" />
              <p>
                Calendar: {data.quota.timezone}. Activity totals update as
                requests are saved; quota includes live charges. Speed is
                measured for streaming responses. Refreshes every 30 seconds.
              </p>
            </div>
            <section className="panel p-6" aria-label="Your privacy">
              <div className="flex flex-wrap justify-between items-center gap-4">
                <div>
                  <h2 className="panel-title">Your activity records</h2>
                  <p className="subtitle">
                    The router keeps metadata of each request (time, model,
                    tokens, timing), never prompts or responses. You can delete
                    these records at any time; otherwise they are deleted after{' '}
                    {data.retention.usage_days} days. Daily totals stay for
                    quotas and accounting for {data.retention.usage_daily_days}{' '}
                    days.
                  </p>
                </div>
                <Button
                  variant="outline"
                  onClick={() => {
                    erase.reset()
                    setErased(null)
                    setConfirmErase(true)
                  }}
                >
                  <Trash2 size={15} />
                  Delete my activity
                </Button>
              </div>
              {erased !== null && (
                <p role="status" className="text-sm mt-3">
                  Deleted {number(erased)} request{erased === 1 ? '' : 's'}.
                </p>
              )}
            </section>
          </>
        )
      )}
      <AlertDialog
        open={confirmErase}
        onOpenChange={(next) => {
          if (!next && !erase.isPending) setConfirmErase(false)
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Delete all your activity records?
            </AlertDialogTitle>
            <AlertDialogDescription>
              Every per-request record of yours is deleted. Today’s and this
              month’s quota stay as they are. This cannot be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <ErrorMessage error={erase.error} />
          <AlertDialogFooter>
            <AlertDialogCancel disabled={erase.isPending}>
              Keep records
            </AlertDialogCancel>
            <AlertDialogAction
              disabled={erase.isPending}
              onClick={(e) => {
                e.preventDefault()
                erase.mutate()
              }}
            >
              {erase.isPending ? 'Deleting…' : 'Delete records'}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}

function Connect() {
  const query = useQuery({
    queryKey: ['models'],
    queryFn: () => api<Models>('models'),
  })
  const [client, setClient] = useState<Client>('curl')
  const [model, setModel] = useState('')
  const protocol =
    client === 'Anthropic Python' || client === 'Claude Code'
      ? 'anthropic'
      : 'openai'
  const models =
    query.data?.models.filter((item) => item.protocols.includes(protocol)) ?? []
  const selected = models.find((item) => item.id === model) ?? models[0]
  const code =
    query.data && selected ? snippet(client, query.data, selected.id) : ''
  return (
    <>
      <div className="page-head">
        <div>
          <div className="eyebrow">From key to first request</div>
          <h1>Connect</h1>
          <p className="subtitle">
            One endpoint for your apps and the tools you already use.
          </p>
        </div>
        <span className="badge">
          <ShieldCheck size={13} />
          Your permitted models
        </span>
      </div>
      <ErrorMessage
        error={query.error}
        retry={() => {
          void query.refetch()
        }}
      />
      {query.isPending ? (
        <p role="status" className="empty">
          Loading connection settings…
        </p>
      ) : (
        query.data && (
          <>
            <div className="panel p-6 mb-6">
              <div className="flex flex-wrap justify-between items-center gap-4">
                <div>
                  <div className="text-xs text-muted-foreground mb-2">
                    OPENAI BASE URL
                  </div>
                  <code className="text-sm break-all">
                    {query.data.base_url}
                  </code>
                </div>
                <CopyButton text={query.data.base_url} label="Copy URL" />
              </div>
              <p className="subtitle">
                Anthropic clients use the router origin:{' '}
                <code>{query.data.router_url}</code>
              </p>
            </div>
            <div className="segmented mb-6" aria-label="Client examples">
              {clients.map((value) => (
                <Button
                  key={value}
                  size="sm"
                  variant={client === value ? 'secondary' : 'ghost'}
                  aria-pressed={client === value}
                  onClick={() => {
                    setClient(value)
                    setModel('')
                  }}
                >
                  {value}
                </Button>
              ))}
            </div>
            <div className="panel">
              <div className="panel-head">
                <div className="flex flex-wrap items-center gap-4">
                  <Code2 size={19} className="text-primary" />
                  <h2 className="panel-title">{client}</h2>
                  <label className="sr-only" htmlFor="model">
                    Model
                  </label>
                  <select
                    id="model"
                    className="select w-auto! max-w-64"
                    value={selected?.id ?? ''}
                    onChange={(e) => setModel(e.target.value)}
                    disabled={!selected}
                  >
                    {models.map((item) => (
                      <option key={item.id} value={item.id}>
                        {item.id}
                        {item.id !== item.canonical_model
                          ? ` (alias for ${item.canonical_model})`
                          : ''}
                      </option>
                    ))}
                  </select>
                </div>
                <CopyButton
                  text={code}
                  label="Copy snippet"
                  disabled={!selected}
                />
              </div>
              {selected ? (
                <pre className="snippet" data-testid="snippet">
                  <code>{code}</code>
                </pre>
              ) : (
                <p className="empty">
                  No {protocol === 'openai' ? 'OpenAI' : 'Anthropic'} models are
                  available for your groups. Contact your operator for access.
                </p>
              )}
            </div>
            <div className="hint">
              <ShieldCheck size={18} className="shrink-0 mt-1" />
              <p>
                Create a key on the API keys page and set{' '}
                <code>OLR_API_KEY</code> in your terminal. Examples use this
                environment variable; your key is never included in snippets.
                {client === 'OpenCode' && (
                  <>
                    <br />
                    Save as <code>opencode.json</code> for the tested OpenCode
                    1.18.10. Set context and output limits to match your
                    backend.
                  </>
                )}
                {client === 'Claude Code' && (
                  <>
                    <br />
                    Settings match the tested Claude Code 2.1.322. Reasoning is
                    disabled for the initial compatibility check.
                  </>
                )}
              </p>
            </div>
          </>
        )
      )}
    </>
  )
}
