import { useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Activity,
  ArrowDownLeft,
  ArrowUpRight,
  Braces,
  Clock3,
  Fingerprint,
  RefreshCw,
  Search,
  ShieldCheck,
  Sparkles,
  Trash2,
  Workflow,
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
import { api, type RequestRecord } from '@/lib/api'

type Preferences = { enabled: boolean; retention_days: number }
type Block = {
  type: string
  text?: string
  name?: string
  id?: string
  arguments?: string
}
type Message = { role: string; blocks: Block[] }
type Trace = {
  request_id: string
  expires_at: string
  detail: {
    operation: string
    input: Message[]
    output: Message[]
    events: { ms: number; type: string; text?: string }[]
    parameters: Record<string, unknown>
    truncated: boolean
    warnings: string[]
  }
}
const n = (v: number) => new Intl.NumberFormat().format(v)
const ms = (v: number | null) =>
  v === null ? '—' : v >= 1000 ? `${(v / 1000).toFixed(2)} s` : `${n(v)} ms`
const date = (v: string) => new Date(v).toLocaleString()
const shortDate = (v: string) =>
  new Date(v).toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
const label = (v: string) => v.replaceAll('_', ' ')
const operation = (r: RequestRecord) =>
  !r.operation
    ? 'Request'
    : r.operation === 'embeddings'
      ? 'Embedding'
      : r.operation === 'completions'
        ? 'Completion'
        : 'Chat'
function ErrorNotice({ error }: { error: Error | null }) {
  return error ? (
    <p className="error" role="alert">
      {error.message}
    </p>
  ) : null
}

export function RequestsPage() {
  const client = useQueryClient()
  const query = useQuery({
    queryKey: ['requests'],
    queryFn: ({ signal }) =>
      api<{ requests: RequestRecord[]; limit: number }>(
        'requests',
        'GET',
        undefined,
        signal,
      ),
    refetchInterval: 5000,
  })
  const prefs = useQuery({
    queryKey: ['trace-preferences'],
    queryFn: () => api<Preferences>('trace-preferences'),
  })
  const [search, setSearch] = useState('')
  const [mode, setMode] = useState('all')
  const [selected, setSelected] = useState<RequestRecord | null>(null)
  const [confirmClear, setConfirmClear] = useState(false)
  const clear = useMutation({
    mutationFn: () => api('request-traces', 'DELETE'),
    onSuccess: () => {
      setSelected(null)
      setConfirmClear(false)
      client.removeQueries({ queryKey: ['request-trace'] })
      void client.invalidateQueries({ queryKey: ['requests'] })
    },
  })
  const all = query.data?.requests ?? []
  const rows = all.filter(
    (r) =>
      `${r.model} ${r.status} ${r.request_id ?? ''} ${r.protocol} ${r.operation}`
        .toLowerCase()
        .includes(search.toLowerCase()) &&
      (mode === 'all' ||
        (mode === 'stream' && r.stream) ||
        (mode === 'errors' && !r.status.startsWith('2')) ||
        (mode === 'traces' && r.trace_available)),
  )
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
        <Button
          variant="outline"
          onClick={() => void query.refetch()}
          disabled={query.isFetching}
        >
          <RefreshCw size={15} />
          Refresh
        </Button>
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
          hint={`Last ${query.data?.limit ?? 200} requests`}
        />
        <Metric
          icon={<Zap size={17} />}
          title="Streaming"
          value={n(all.filter((r) => r.stream).length)}
          hint="Incremental responses"
        />
        <Metric
          icon={<ArrowUpRight size={17} />}
          title="Input tokens"
          value={n(all.reduce((s, r) => s + r.input_tokens, 0))}
          hint="Across recent requests"
        />
        <Metric
          icon={<ArrowDownLeft size={17} />}
          title="Output tokens"
          value={n(all.reduce((s, r) => s + r.output_tokens, 0))}
          hint="Across recent requests"
        />
      </div>
      <section className="panel trace-history" aria-label="Recent requests">
        <div className="trace-toolbar">
          <div className="trace-search">
            <Search size={16} />
            <Input
              aria-label="Search requests"
              placeholder="Search model, status or request ID…"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
            />
          </div>
          <select
            aria-label="Filter requests"
            value={mode}
            onChange={(e) => setMode(e.target.value)}
          >
            <option value="all">All requests</option>
            <option value="stream">Streaming</option>
            <option value="errors">Errors & interrupted</option>
            <option value="traces">With traces</option>
          </select>
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
        ) : rows.length === 0 ? (
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
                  <th>Tokens</th>
                  <th>Latency</th>
                  <th>Key</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r, i) => (
                  <tr key={r.request_id ?? `${r.time}-${i}`}>
                    <td className="whitespace-nowrap">
                      {shortDate(r.time)}
                      {r.trace_available && r.request_id ? (
                        <Button
                          variant="ghost"
                          className="trace-request-link"
                          size="sm"
                          aria-label={`Inspect request ${r.request_id}`}
                          onClick={() => setSelected(r)}
                        >
                          <Workflow size={14} />
                          <code>{r.request_id.slice(0, 12)}</code>
                        </Button>
                      ) : (
                        <span
                          className="trace-secondary"
                          title="Capture may be disabled, expired, pending or skipped when tracing is at capacity. Older requests have no content."
                        >
                          {r.request_id?.slice(0, 12) ?? 'Older request'} · No
                          detail
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
                      <span
                        className={`badge ${r.status.startsWith('2') ? '' : 'inactive'}`}
                      >
                        {r.status}
                      </span>
                    </td>
                    <td>
                      {n(r.output_tokens)} out
                      <span className="trace-secondary">
                        {n(r.input_tokens)} in
                      </span>
                    </td>
                    <td>
                      {ms(r.latency_ms)}
                      <span className="trace-secondary">
                        First {ms(r.ttfb_ms)}
                      </span>
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
                ))}
              </tbody>
            </table>
          </div>
        )}
        <p className="trace-footnote">
          Select a request ID to inspect. Updates every 5 seconds while this
          page is open. Traces appear after completion; event timing shows how
          streamed content arrived.
        </p>
      </section>
      <Dialog
        open={selected !== null}
        onOpenChange={(open) => {
          if (!open) setSelected(null)
        }}
      >
        <DialogContent className="trace-dialog">
          <DialogHeader>
            <DialogTitle>Request trace</DialogTitle>
            <DialogDescription>
              {selected?.model} · {selected && date(selected.time)}
            </DialogDescription>
          </DialogHeader>
          {selected && <TraceDetail record={selected} />}
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
            Enabled by default · 7-day default retention · Images and file
            payloads omitted
          </p>
        </div>
      </div>
      <form onSubmit={submit} className="trace-settings-controls">
        <label className="trace-toggle">
          <input
            type="checkbox"
            role="switch"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
          <span>{enabled ? 'Capture enabled' : 'Capture disabled'}</span>
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
function Metric({
  icon,
  title,
  value,
  hint,
}: {
  icon: React.ReactNode
  title: string
  value: string
  hint: string
}) {
  return (
    <div className="trace-metric">
      <span>
        {icon}
        {title}
      </span>
      <strong>{value}</strong>
      <small>{hint}</small>
    </div>
  )
}
function pretty(value: string) {
  try {
    return JSON.stringify(JSON.parse(value), null, 2)
  } catch {
    return value
  }
}
function Content({ block }: { block: Block }) {
  if (block.type === 'reasoning')
    return (
      <details className="trace-reasoning">
        <summary>
          <Sparkles size={14} />
          Reasoning
        </summary>
        <pre>{block.text}</pre>
      </details>
    )
  if (block.type === 'tool_call' || block.type === 'tool_result')
    return (
      <div className="trace-tool">
        <div>
          <Braces size={15} />
          <strong>
            {block.type === 'tool_call' ? 'Tool call' : 'Tool result'}
            {block.name ? ` · ${block.name}` : ''}
          </strong>
        </div>
        {block.id && <code className="trace-secondary">{block.id}</code>}
        <pre>{pretty(block.arguments ?? block.text ?? '')}</pre>
      </div>
    )
  return (
    <pre
      className={`trace-text ${['image', 'document', 'audio'].includes(block.type) ? 'text-muted-foreground' : ''}`}
    >
      {block.text}
    </pre>
  )
}
function Messages({ title, messages }: { title: string; messages: Message[] }) {
  return (
    <section className="trace-messages">
      <h3>
        {title}
        <span>
          {messages.length} message{messages.length === 1 ? '' : 's'}
        </span>
      </h3>
      {messages.length === 0 ? (
        <p className="trace-secondary">No content captured.</p>
      ) : (
        messages.map((m, i) => (
          <article key={i} className={`trace-message trace-role-${m.role}`}>
            <div className="trace-role">{m.role}</div>
            {m.blocks.map((b, j) => (
              <Content key={j} block={b} />
            ))}
          </article>
        ))
      )}
    </section>
  )
}
function TraceDetail({ record }: { record: RequestRecord }) {
  // Content is fetched only on demand and removed from the query cache when
  // the dialog closes. Never persist trace bodies in browser storage.
  const query = useQuery({
    queryKey: ['request-trace', record.request_id],
    queryFn: ({ signal }) =>
      api<Trace>(`requests/${record.request_id}`, 'GET', undefined, signal),
    gcTime: 0,
    staleTime: 0,
    refetchOnWindowFocus: false,
  })
  const [tab, setTab] = useState('conversation')
  const [eventKind, setEventKind] = useState('all')
  const [eventLimit, setEventLimit] = useState(100)
  const d = query.data?.detail
  const events =
    d?.events.filter((e) => eventKind === 'all' || e.type === eventKind) ?? []
  return (
    <div className="trace-detail">
      <ErrorNotice error={query.error} />
      <div className="trace-detail-metrics">
        <Metric
          icon={<Clock3 size={15} />}
          title="Total time"
          value={ms(record.latency_ms)}
          hint={`First token ${ms(record.ttfb_ms)}`}
        />
        <Metric
          icon={<Zap size={15} />}
          title="Output speed"
          value={
            record.output_tokens > 0 &&
            record.ttfb_ms !== null &&
            record.latency_ms > record.ttfb_ms
              ? `${((record.output_tokens * 1000) / (record.latency_ms - record.ttfb_ms)).toFixed(1)} tok/s`
              : '—'
          }
          hint={`${n(record.input_tokens)} in · ${n(record.output_tokens)} out`}
        />
        <Metric
          icon={<Activity size={15} />}
          title={record.stream ? 'Streaming' : 'JSON response'}
          value={record.status}
          hint={`${record.protocol} · ${operation(record)}`}
        />
      </div>
      {query.isPending && <p role="status">Loading trace…</p>}
      {d && (
        <>
          {d.warnings.map((w, i) => (
            <p className="trace-warning" key={i}>
              {w}
            </p>
          ))}
          <div className="trace-tabs" role="tablist" aria-label="Trace views">
            {[
              ['conversation', 'Conversation'],
              ['events', `Stream events (${d.events.length})`],
              ['details', 'Details'],
            ].map(([id, name]) => (
              <button
                key={id}
                type="button"
                role="tab"
                id={`trace-tab-${id}`}
                aria-controls="trace-panel"
                aria-selected={tab === id}
                tabIndex={tab === id ? 0 : -1}
                onKeyDown={(e) => {
                  const tabs = ['conversation', 'events', 'details']
                  if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') {
                    e.preventDefault()
                    const next =
                      tabs[
                        (tabs.indexOf(tab) + (e.key === 'ArrowRight' ? 1 : 2)) %
                          3
                      ]
                    setTab(next)
                    document.getElementById(`trace-tab-${next}`)?.focus()
                  }
                }}
                onClick={() => setTab(id)}
              >
                {name}
              </button>
            ))}
          </div>
          <div
            id="trace-panel"
            role="tabpanel"
            aria-labelledby={`trace-tab-${tab}`}
          >
            {tab === 'conversation' && (
              <div className="trace-conversation">
                <Messages title="Input" messages={d.input} />
                <Messages title="Output" messages={d.output} />
              </div>
            )}
            {tab === 'events' && (
              <>
                <div className="trace-event-head">
                  <p>
                    Observed delivery times relative to request start. Tool
                    calls shown here are model requests; tool results appear in
                    subsequent input messages.
                  </p>
                  <select
                    aria-label="Filter stream events"
                    value={eventKind}
                    onChange={(e) => {
                      setEventKind(e.target.value)
                      setEventLimit(100)
                    }}
                  >
                    <option value="all">All events</option>
                    {[...new Set(d.events.map((e) => e.type))].map((k) => (
                      <option key={k} value={k}>
                        {label(k)}
                      </option>
                    ))}
                  </select>
                </div>
                {events.length ? (
                  <ol className="trace-timeline">
                    {events.slice(0, eventLimit).map((e, i) => (
                      <li key={i}>
                        <time>+{ms(e.ms)}</time>
                        <span
                          className={`trace-event-kind trace-event-${e.type}`}
                        >
                          {label(e.type)}
                        </span>
                        <pre>{e.text || '—'}</pre>
                      </li>
                    ))}
                  </ol>
                ) : (
                  <p className="empty">
                    No stream events. JSON responses are shown in Conversation.
                  </p>
                )}
                {events.length > eventLimit && (
                  <Button
                    variant="outline"
                    onClick={() => setEventLimit((v) => v + 100)}
                  >
                    Show 100 more events
                  </Button>
                )}
              </>
            )}
            {tab === 'details' && (
              <div className="trace-properties">
                <p>
                  <Fingerprint size={15} />
                  Request ID <code>{record.request_id}</code>
                </p>
                <p>
                  Operation <code>{d.operation}</code>
                </p>
                <p>
                  Expires <strong>{date(query.data!.expires_at)}</strong>
                </p>
                <h3>Request parameters</h3>
                <pre>{JSON.stringify(d.parameters, null, 2)}</pre>
              </div>
            )}
          </div>
        </>
      )}
    </div>
  )
}
