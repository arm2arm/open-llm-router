import {
  useMemo,
  useRef,
  useState,
  type FormEvent,
  type KeyboardEvent,
} from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import {
  Activity,
  ArrowDownLeft,
  ArrowUpRight,
  ChevronDown,
  ChevronRight,
  ChevronUp,
  Layers,
  LoaderCircle,
  Pause,
  Radio,
  RefreshCw,
  Search,
  ShieldCheck,
  Trash2,
  Workflow,
  Wrench,
  Zap,
} from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { api } from '@/lib/api'
import {
  date,
  label,
  ms,
  n,
  ok,
  operation,
  plural,
  shortDate,
  groupRows,
  runStats,
  type Item,
  type Row,
} from './requests/model'
import { Metric } from './requests/widgets'
import { TraceDetail, type ConversationNav } from './requests/trace-detail'
import { useLive, useNow, type LiveStatus, type Running } from './requests/live'

type Preferences = {
  enabled: boolean
  retention_days: number
  available: boolean
}
type Filter = 'all' | 'stream' | 'errors' | 'traces' | 'tools' | 'truncated'
const ranges = {
  any: 0,
  '15m': 15 * 60e3,
  '1h': 60 * 60e3,
  '24h': 24 * 60 * 60e3,
} as const

function ErrorNotice({ error }: { error: Error | null }) {
  return error ? (
    <p className="error" role="alert">
      {error.message}
    </p>
  ) : null
}

function matches(r: Row, search: string, filter: Filter, since: number) {
  const s = r.trace_summary
  return (
    `${r.model} ${r.status} ${r.request_id ?? ''} ${r.protocol} ${r.operation} ${s?.finish_reason ?? ''}`
      .toLowerCase()
      .includes(search.toLowerCase()) &&
    (since === 0 || new Date(r.time).getTime() >= since) &&
    (filter === 'all' ||
      (filter === 'stream' && r.stream) ||
      (filter === 'errors' && !ok(r.status)) ||
      (filter === 'traces' && r.trace_available) ||
      (filter === 'tools' && !!s && s.tool_calls + s.tool_results > 0) ||
      (filter === 'truncated' && !!s?.truncated))
  )
}

export function RequestsPage() {
  const client = useQueryClient()
  const navigate = useNavigate()
  const { id } = useParams()
  const [params, setParams] = useSearchParams()
  // Live updates are on by default; "?live=off" pauses them. A URL keeps the
  // choice across reloads without browser storage.
  const liveOn = params.get('live') !== 'off'
  const live = useLive(liveOn)
  const seen = useRef<Set<string> | null>(null)
  const query = useQuery({
    queryKey: ['requests'],
    queryFn: async ({ signal }) => {
      const data = await api<{ requests: Row[]; limit: number }>(
        'requests',
        'GET',
        undefined,
        signal,
      )
      // Rows that arrive after the first load are highlighted once.
      const first = seen.current === null
      seen.current ??= new Set()
      for (const r of data.requests) {
        if (!r.request_id) continue
        r.fresh = !first && !seen.current.has(r.request_id)
        seen.current.add(r.request_id)
      }
      return data
    },
    // The stream announces changes; polling covers a lost or refused stream.
    refetchInterval: !liveOn ? false : live.status === 'live' ? 30_000 : 5000,
  })
  const prefs = useQuery({
    queryKey: ['trace-preferences'],
    queryFn: () => api<Preferences>('trace-preferences'),
  })
  const search = params.get('q') ?? ''
  const filter = (params.get('filter') ?? 'all') as Filter
  const range = (params.get('range') ?? 'any') as keyof typeof ranges
  const setParam = (key: string, value: string, fallback: string) =>
    setParams(
      (p) => {
        if (value === fallback) p.delete(key)
        else p.set(key, value)
        return p
      },
      { replace: true },
    )
  const [confirmClear, setConfirmClear] = useState(false)
  const clear = useMutation({
    mutationFn: () => api('request-traces', 'DELETE'),
    onSuccess: () => {
      close()
      setConfirmClear(false)
      client.removeQueries({ queryKey: ['request-trace'] })
      void client.invalidateQueries({ queryKey: ['requests'] })
    },
  })
  const all = useMemo(() => query.data?.requests ?? [], [query.data])
  // eslint-disable-next-line react-hooks/purity -- the range is relative to each render
  const since = ranges[range] ? Date.now() - ranges[range] : 0
  const filtered = all.filter((r) => matches(r, search, filter, since))
  // Agent runs are grouped by default; "?group=off" lists every call flat.
  const grouped = params.get('group') !== 'off'
  const items: Item[] = grouped
    ? groupRows(filtered)
    : filtered.map((row) => ({ kind: 'row', row }))
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())
  // Display order, which j / k follow.
  const rows = items.flatMap((item) =>
    item.kind === 'row' ? [item.row] : item.rows,
  )
  const selected = id ? all.find((r) => r.request_id === id) : undefined
  const position = rows.findIndex((r) => r.request_id === id)
  // The selected call's place in its run, over all listed requests.
  let conversation: ConversationNav | undefined
  for (const item of groupRows(all)) {
    if (item.kind !== 'group') continue
    const run = [...item.rows].reverse()
    const at = run.findIndex((r) => r.request_id === id)
    if (at < 0) continue
    const traced = (r?: Row) =>
      r?.trace_available && r.request_id ? r.request_id : undefined
    conversation = {
      index: at + 1,
      total: run.length,
      previous: traced(run[at - 1]),
      next: traced(run[at + 1]),
      open: (rid: string) => open(rid),
    }
  }
  const open = (rid: string) =>
    navigate({ pathname: `/requests/${rid}`, search: params.toString() })
  function close() {
    navigate({ pathname: '/requests', search: params.toString() })
  }
  const step = (delta: number) => {
    for (let i = position + delta; i >= 0 && i < rows.length; i += delta) {
      const r = rows[i]
      if (r.trace_available && r.request_id) return open(r.request_id)
    }
  }
  function keys(e: KeyboardEvent) {
    const target = e.target as HTMLElement
    if (target.closest('input, textarea, select, [contenteditable]')) return
    if (e.key === 'j') step(1)
    if (e.key === 'k') step(-1)
  }
  const tools = all.reduce((s, r) => s + (r.trace_summary?.tool_calls ?? 0), 0)
  const listed = new Set(all.map((r) => r.request_id))
  const running = [...live.running.values()]
    .filter((r) => !listed.has(r.request_id))
    .sort((a, b) => b.started - a.started)
  const pending = !!id && !selected && live.running.has(id)
  return (
    <>
      <div className="page-head">
        <div>
          <div className="eyebrow">Request observability</div>
          <h1>My requests</h1>
          <p className="subtitle">
            Follow the conversation, inspect tool calls and understand every
            response.
          </p>
        </div>
        <div className="trace-head-actions">
          <LiveToggle
            on={liveOn}
            status={live.status}
            toggle={() => setParam('live', liveOn ? 'off' : 'on', 'on')}
          />
          <Button
            variant="outline"
            onClick={() => void query.refetch()}
            disabled={query.isFetching}
          >
            <RefreshCw size={15} />
            Refresh
          </Button>
        </div>
      </div>
      <ErrorNotice error={query.error ?? prefs.error} />
      {prefs.data && (
        <TraceSettings
          key={`${prefs.data.enabled}-${prefs.data.retention_days}`}
          preferences={prefs.data}
        />
      )}
      <div className="trace-metrics" aria-label="Recent request summary">
        <Metric
          icon={<Activity size={17} />}
          title="Recent requests"
          value={n(all.length)}
          hint={`${n(all.filter((r) => !ok(r.status)).length)} failed or interrupted`}
        />
        <Metric
          icon={<Wrench size={17} />}
          title="Tool calls"
          value={n(tools)}
          hint={`${n(all.filter((r) => r.stream).length)} streamed requests`}
        />
        <Metric
          icon={<ArrowUpRight size={17} />}
          title="Input tokens"
          value={n(all.reduce((s, r) => s + r.input_tokens, 0))}
          hint={`${n(all.reduce((s, r) => s + (r.trace_summary?.cached_tokens ?? 0), 0))} cached`}
        />
        <Metric
          icon={<ArrowDownLeft size={17} />}
          title="Output tokens"
          value={n(all.reduce((s, r) => s + r.output_tokens, 0))}
          hint={`${n(all.reduce((s, r) => s + (r.trace_summary?.reasoning_tokens ?? 0), 0))} reasoning`}
        />
      </div>
      <section className="panel trace-history" aria-label="Recent requests">
        <div className="trace-toolbar">
          <div className="trace-search">
            <Search size={16} />
            <Input
              aria-label="Search requests"
              placeholder="Search model, status, finish reason or request ID…"
              value={search}
              onChange={(e) => setParam('q', e.target.value, '')}
            />
          </div>
          <select
            aria-label="Filter requests"
            value={filter}
            onChange={(e) => setParam('filter', e.target.value, 'all')}
          >
            <option value="all">All requests</option>
            <option value="stream">Streaming</option>
            <option value="errors">Errors & interrupted</option>
            <option value="traces">With traces</option>
            <option value="tools">With tool calls</option>
            <option value="truncated">Truncated capture</option>
          </select>
          <select
            aria-label="Time range"
            value={range}
            onChange={(e) => setParam('range', e.target.value, 'any')}
          >
            <option value="any">Any time</option>
            <option value="15m">Last 15 minutes</option>
            <option value="1h">Last hour</option>
            <option value="24h">Last 24 hours</option>
          </select>
          <Button
            variant="outline"
            size="sm"
            aria-pressed={grouped}
            title="Group the calls of one agent run or conversation"
            onClick={() => setParam('group', grouped ? 'off' : 'on', 'on')}
          >
            <Layers size={14} />
            Group runs
          </Button>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setConfirmClear(true)}
          >
            <Trash2 size={14} />
            Delete traces
          </Button>
        </div>
        {query.isPending ? (
          <p role="status" className="empty">
            Loading requests…
          </p>
        ) : rows.length === 0 && running.length === 0 ? (
          <p className="empty">
            {all.length
              ? 'No requests match these filters.'
              : 'Your next completed request will appear here. Detailed traces become available shortly after completion.'}
          </p>
        ) : (
          <div className="table-wrap">
            <table data-testid="requests-table">
              <caption className="sr-only">Recent requests</caption>
              <thead>
                <tr>
                  <th>Time / request</th>
                  <th>Model / type</th>
                  <th>Status</th>
                  <th>Tools</th>
                  <th>Tokens</th>
                  <th>Latency</th>
                  <th>Key</th>
                </tr>
              </thead>
              <tbody aria-live="polite" aria-relevant="additions">
                {running.map((r) => (
                  <RunningRow
                    key={r.request_id}
                    r={r}
                    active={r.request_id === id}
                    open={open}
                  />
                ))}
                {items.map((item, i) => {
                  const row = (r: Row, j: number, member?: boolean) => (
                    <RequestRow
                      key={r.request_id ?? `${r.time}-${i}-${j}`}
                      r={r}
                      active={r.request_id === id}
                      fresh={!!r.fresh}
                      saving={liveOn && prefs.data?.enabled === true}
                      member={member}
                      open={open}
                    />
                  )
                  if (item.kind === 'row') return row(item.row, 0)
                  const shut = collapsed.has(item.key)
                  return [
                    <GroupRow
                      key={item.key}
                      rows={item.rows}
                      open={!shut}
                      active={id}
                      toggle={() =>
                        setCollapsed((prev) => {
                          const next = new Set(prev)
                          if (shut) next.delete(item.key)
                          else next.add(item.key)
                          return next
                        })
                      }
                      inspect={open}
                    />,
                    ...(shut ? [] : item.rows.map((r, j) => row(r, j, true))),
                  ]
                })}
              </tbody>
            </table>
          </div>
        )}
        <p className="trace-footnote">
          Select a request ID to inspect; use j / k to move between requests.{' '}
          {liveOn
            ? 'Live: running requests appear as they start and update as they finish.'
            : 'Live updates are paused; use Refresh.'}
        </p>
      </section>
      <Dialog
        open={!!id}
        onOpenChange={(o) => {
          if (!o) close()
        }}
      >
        <DialogContent className="trace-dialog" onKeyDown={keys}>
          <DialogHeader className="trace-dialog-head">
            <div>
              <DialogTitle>Request trace</DialogTitle>
              <DialogDescription>
                {selected ? (
                  `${selected.model || 'unknown model'} · ${date(selected.time)}`
                ) : pending ? (
                  `${live.running.get(id!)?.model || 'Running request'} · started ${date(live.running.get(id!)!.time)}`
                ) : (
                  <code>{id}</code>
                )}
              </DialogDescription>
            </div>
            <div className="trace-nav">
              <Button
                variant="outline"
                size="sm"
                aria-label="Previous request (k)"
                title="Newer request (k)"
                disabled={position <= 0}
                onClick={() => step(-1)}
              >
                <ChevronUp size={15} />
              </Button>
              <Button
                variant="outline"
                size="sm"
                aria-label="Next request (j)"
                title="Older request (j)"
                disabled={position < 0 || position >= rows.length - 1}
                onClick={() => step(1)}
              >
                <ChevronDown size={15} />
              </Button>
            </div>
          </DialogHeader>
          {id && (
            <TraceDetail
              key={id}
              id={id}
              record={selected}
              pending={pending}
              conversation={conversation}
            />
          )}
        </DialogContent>
      </Dialog>
      <Dialog open={confirmClear} onOpenChange={setConfirmClear}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete your detailed traces?</DialogTitle>
            <DialogDescription>
              This permanently removes stored inputs, outputs and stream events.
              Request metadata, token totals and your capture settings stay in
              place.
            </DialogDescription>
          </DialogHeader>
          <ErrorNotice error={clear.error} />
          <Button
            variant="destructive"
            disabled={clear.isPending}
            onClick={() => clear.mutate()}
          >
            Delete all my traces
          </Button>
        </DialogContent>
      </Dialog>
    </>
  )
}

function LiveToggle({
  on,
  status,
  toggle,
}: {
  on: boolean
  status: LiveStatus
  toggle: () => void
}) {
  const text: Record<LiveStatus, string> = {
    live: 'Live',
    connecting: 'Connecting…',
    reconnecting: 'Reconnecting…',
    unavailable: 'Live unavailable · polling',
    paused: 'Paused',
  }
  return (
    <Button
      variant="outline"
      aria-pressed={on}
      className={`trace-live trace-live-${status}`}
      title={on ? 'Pause live updates' : 'Resume live updates'}
      onClick={toggle}
    >
      {on ? <Radio size={15} /> : <Pause size={15} />}
      <span className="trace-live-dot" aria-hidden />
      {text[status]}
    </Button>
  )
}

function RunningRow({
  r,
  active,
  open,
}: {
  r: Running
  active: boolean
  open: (id: string) => void
}) {
  const now = useNow(true)
  return (
    <tr className={`trace-row-running ${active ? 'trace-row-active' : ''}`}>
      <td className="whitespace-nowrap">
        {shortDate(r.time)}
        <Button
          variant="ghost"
          className="trace-request-link"
          size="sm"
          aria-label={`Inspect running request ${r.request_id}`}
          onClick={() => open(r.request_id)}
        >
          <LoaderCircle size={14} className="trace-spin" />
          <code>{r.request_id.slice(0, 12)}</code>
        </Button>
      </td>
      <td>
        <strong>{r.model || '—'}</strong>
        <span className="trace-secondary">
          {r.protocol} · {operation({ operation: r.operation ?? '' })}
        </span>
      </td>
      <td>
        <span className="badge trace-badge-running">running</span>
        <span className="trace-secondary">
          {r.writes ? 'receiving' : 'waiting for model'}
        </span>
      </td>
      <td />
      <td>
        {r.bytes ? `${n(Math.round(r.bytes / 102.4) / 10)} KB` : '—'}
        <span className="trace-secondary">
          {plural(r.writes, 'chunk')} received
        </span>
      </td>
      <td>
        <strong className="tabular-nums">{ms(now - r.started)}</strong>
        <span className="trace-secondary">elapsed</span>
      </td>
      <td />
    </tr>
  )
}

function GroupRow({
  rows,
  open,
  active,
  toggle,
  inspect,
}: {
  rows: Row[]
  open: boolean
  active?: string
  toggle: () => void
  inspect: (id: string) => void
}) {
  const s = runStats(rows)
  const span = Math.max(s.end - s.start, 1)
  const ordered = [...rows].reverse()
  return (
    <tr className="trace-group">
      <td colSpan={7}>
        <div className="trace-group-head">
          <button
            type="button"
            className="trace-group-toggle"
            aria-expanded={open}
            onClick={toggle}
          >
            {open ? <ChevronDown size={15} /> : <ChevronRight size={15} />}
            <Workflow size={14} />
            <strong>Agent run</strong>
          </button>
          <span>{plural(s.calls, 'call')}</span>
          {s.toolCalls > 0 && <span>{plural(s.toolCalls, 'tool call')}</span>}
          <span title="From the first request to the end of the last">
            {ms(s.end - s.start)} total · {ms(s.busy)} in the model
          </span>
          <span>
            {n(s.input)} → {n(s.output)} tokens
          </span>
          {s.failed > 0 && <span className="trace-bad">{s.failed} failed</span>}
          <span className="trace-secondary">
            {s.models.join(', ')} · started{' '}
            {shortDate(new Date(s.start).toISOString())}
          </span>
        </div>
        <div
          className="trace-run-bar"
          role="group"
          aria-label="Calls of this run over time"
        >
          {ordered.map((r, i) => {
            const left = ((Date.parse(r.time) - s.start) / span) * 100
            const width = (r.latency_ms / span) * 100
            const label = `Call ${i + 1}: ${ms(r.latency_ms)}, ${r.trace_summary?.tool_calls ?? 0} tool calls, status ${r.status}`
            return r.trace_available && r.request_id ? (
              <button
                type="button"
                key={r.request_id}
                className={`trace-run-call ${ok(r.status) ? '' : 'failed'} ${active === r.request_id ? 'active' : ''}`}
                style={{ left: `${left}%`, width: `max(4px, ${width}%)` }}
                title={label}
                aria-label={label}
                onClick={() => inspect(r.request_id!)}
              />
            ) : (
              <span
                key={`${r.time}-${i}`}
                className="trace-run-call idle"
                style={{ left: `${left}%`, width: `max(4px, ${width}%)` }}
                title={label}
              />
            )
          })}
        </div>
      </td>
    </tr>
  )
}

function RequestRow({
  r,
  active,
  fresh,
  saving,
  member,
  open,
}: {
  r: Row
  active: boolean
  fresh: boolean
  saving: boolean
  member?: boolean
  open: (id: string) => void
}) {
  const s = r.trace_summary
  // eslint-disable-next-line react-hooks/purity -- only labels a just-finished row
  const recent = Date.now() - Date.parse(r.time) - r.latency_ms < 15_000
  return (
    <tr
      className={`${active ? 'trace-row-active' : ''} ${fresh ? 'trace-row-new' : ''} ${member ? 'trace-row-member' : ''}`}
    >
      <td className="whitespace-nowrap">
        {shortDate(r.time)}
        {r.trace_available && r.request_id ? (
          <Button
            variant="ghost"
            className="trace-request-link"
            size="sm"
            aria-label={`Inspect request ${r.request_id}`}
            onClick={() => open(r.request_id!)}
          >
            <Workflow size={14} />
            <code>{r.request_id.slice(0, 12)}</code>
          </Button>
        ) : (
          <span
            className="trace-secondary"
            title="Capture may be disabled, expired, pending or skipped when tracing is at capacity. Older requests have no content."
          >
            {r.request_id?.slice(0, 12) ?? 'Older request'} ·{' '}
            {saving && recent && r.request_id ? 'Saving trace…' : 'No detail'}
          </span>
        )}
      </td>
      <td>
        <strong>{r.model || '—'}</strong>
        <span className="trace-secondary">
          {r.protocol} · {operation(r)}{' '}
          {r.stream && (
            <span className="trace-stream">
              <Zap size={11} />
              Stream
            </span>
          )}
        </span>
      </td>
      <td>
        <span className={`badge ${ok(r.status) ? '' : 'inactive'}`}>
          {r.status}
        </span>
        {s?.finish_reason && (
          <span className="trace-secondary">{label(s.finish_reason)}</span>
        )}
      </td>
      <td>
        {s && s.tool_calls > 0 && (
          <span className="trace-tool-chip">
            <Wrench size={11} />
            {plural(s.tool_calls, 'call')} requested
          </span>
        )}
        {s && s.tool_results > 0 && (
          <span className="trace-secondary">
            {plural(s.tool_results, 'result')} sent
          </span>
        )}
        {s && s.tool_calls + s.tool_results === 0 && (
          <span className="trace-secondary">
            {s.tools ? `${plural(s.tools, 'tool')} declared` : '—'}
          </span>
        )}
      </td>
      <td>
        {n(r.output_tokens)} out
        <span className="trace-secondary">
          {n(r.input_tokens)} in
          {s?.cached_tokens ? ` · ${n(s.cached_tokens)} cached` : ''}
        </span>
      </td>
      <td>
        {ms(r.latency_ms)}
        <span className="trace-secondary">First {ms(r.ttfb_ms)}</span>
      </td>
      <td>
        <code className="text-xs text-muted-foreground">
          {r.forwarded
            ? 'via application'
            : r.key_prefix
              ? `${r.key_prefix}…`
              : 'removed'}
        </code>
      </td>
    </tr>
  )
}

function TraceSettings({ preferences }: { preferences: Preferences }) {
  const client = useQueryClient()
  const [enabled, setEnabled] = useState(preferences.enabled)
  const [days, setDays] = useState(String(preferences.retention_days))
  const save = useMutation({
    mutationFn: () =>
      api<Preferences>('trace-preferences', 'PUT', {
        enabled,
        retention_days: Number(days),
      }),
    onSuccess: (value) => {
      client.setQueryData(['trace-preferences'], value)
      void client.invalidateQueries({ queryKey: ['requests'] })
    },
  })
  const dirty =
    enabled !== preferences.enabled ||
    Number(days) !== preferences.retention_days
  const valid =
    Number.isInteger(Number(days)) && Number(days) >= 1 && Number(days) <= 30
  function submit(e: FormEvent) {
    e.preventDefault()
    if (dirty && valid) save.mutate()
  }
  return (
    <section
      className="panel trace-settings"
      aria-label="Detailed logging settings"
    >
      <div className="trace-settings-copy">
        <ShieldCheck size={23} />
        <div>
          <h2>Detailed logging</h2>
          <p>
            Private to your account. Stores message text, reasoning and tool
            calls. Text may contain sensitive information.
          </p>
          <p>
            Requires your opt-in · 7-day default retention · Images and file
            payloads omitted
          </p>
        </div>
      </div>
      <form onSubmit={submit} className="trace-settings-controls">
        <label className="trace-toggle">
          <input
            type="checkbox"
            role="switch"
            disabled={!preferences.available}
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
          <span>
            {!preferences.available
              ? 'Disabled by site policy'
              : enabled
                ? 'Capture enabled'
                : 'Capture disabled'}
          </span>
        </label>
        <label className="trace-days">
          Keep traces for{' '}
          <Input
            type="number"
            aria-label="Trace retention days"
            min="1"
            max="30"
            required
            value={days}
            onChange={(e) => setDays(e.target.value)}
          />
          days
        </label>
        <Button type="submit" disabled={!dirty || !valid || save.isPending}>
          {save.isPending ? 'Saving…' : 'Save settings'}
        </Button>
      </form>
      <p className="trace-footnote">
        Choose 1–30 days. Disabling stops new captures; existing traces expire
        on schedule. Shorter retention also applies to stored traces.
      </p>
      <ErrorNotice error={save.error} />
    </section>
  )
}
