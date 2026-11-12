import { useMemo, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  Activity,
  ArrowRight,
  Braces,
  CircleAlert,
  Clock3,
  CornerDownRight,
  Fingerprint,
  Gauge,
  Route,
  Search,
  Sparkles,
  Workflow,
  Wrench,
  Zap,
} from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { api } from '@/lib/api'
import {
  date,
  forwarded,
  label,
  ms,
  n,
  ok,
  operation,
  parseJSON,
  plural,
  segments,
  textOf,
  toolRuns,
  type Block,
  type Detail,
  type Message,
  type Row,
  type Trace,
  type ToolRun,
  delta,
  type Delta,
} from './model'
import {
  CopyButton,
  Highlight,
  JsonTree,
  Markdown,
  Metric,
  Structured,
} from './widgets'

const tabs = [
  ['overview', 'Overview'],
  ['conversation', 'Conversation'],
  ['tools', 'Tools'],
  ['timeline', 'Timeline'],
  ['raw', 'Raw'],
] as const
type Tab = (typeof tabs)[number][0]

export function useTrace(id: string | null | undefined, enabled = true) {
  // Content is fetched only on demand and removed from the query cache when
  // the view closes. Never persist trace bodies in browser storage.
  return useQuery({
    queryKey: ['request-trace', id],
    queryFn: ({ signal }) =>
      api<Trace>(`requests/${id}`, 'GET', undefined, signal),
    enabled: !!id && enabled,
    gcTime: 0,
    staleTime: 0,
    refetchOnWindowFocus: false,
    retry: (count, error) =>
      count < 6 && 'status' in error && error.status === 404,
    retryDelay: 1500,
  })
}

// Position of a call within its agent run, from the request list.
export type ConversationNav = {
  index: number // 1-based, oldest first
  total: number
  previous?: string
  next?: string
  open: (id: string) => void
}

export function TraceDetail({
  id,
  record,
  conversation,
  pending,
}: {
  id: string
  record?: Row
  conversation?: ConversationNav
  pending?: boolean
}) {
  const query = useTrace(id, !pending)
  // The previous call shows what this call added to the conversation.
  const previous = useTrace(conversation?.previous, !pending)
  const [tab, setTab] = useState<Tab>('overview')
  const d = query.data?.detail
  const runs = useMemo(() => (d ? toolRuns(d) : []), [d])
  const added = useMemo(
    () => (d && previous.data ? delta(d, previous.data.detail) : undefined),
    [d, previous.data],
  )
  return (
    <div className="trace-detail">
      {pending ? (
        <p role="status" className="trace-pending">
          <Activity size={15} /> This request is still running. Its trace opens
          when it completes.
        </p>
      ) : query.isPending ? (
        <p role="status">Loading trace…</p>
      ) : query.error ? (
        <p className="error" role="alert">
          {'status' in query.error && query.error.status === 404
            ? 'No detail for this request: capture may be disabled, expired, still being saved or skipped at capacity.'
            : query.error.message}
        </p>
      ) : null}
      {d && (
        <>
          {d.warnings.map((w, i) => (
            <p className="trace-warning" key={i}>
              <CircleAlert size={14} /> {w}
            </p>
          ))}
          <div className="trace-tabs" role="tablist" aria-label="Trace views">
            {tabs.map(([key, name]) => (
              <button
                key={key}
                type="button"
                role="tab"
                id={`trace-tab-${key}`}
                aria-controls="trace-panel"
                aria-selected={tab === key}
                tabIndex={tab === key ? 0 : -1}
                onKeyDown={(e) => {
                  if (e.key !== 'ArrowRight' && e.key !== 'ArrowLeft') return
                  e.preventDefault()
                  const keys = tabs.map(([k]) => k)
                  const next =
                    keys[
                      (keys.indexOf(tab) +
                        (e.key === 'ArrowRight' ? 1 : keys.length - 1)) %
                        keys.length
                    ]
                  setTab(next)
                  document.getElementById(`trace-tab-${next}`)?.focus()
                }}
                onClick={() => setTab(key)}
              >
                {name}
                {key === 'tools' && runs.length > 0 && (
                  <span className="trace-count">{runs.length}</span>
                )}
                {key === 'timeline' && d.events.length > 0 && (
                  <span className="trace-count">{d.events.length}</span>
                )}
              </button>
            ))}
          </div>
          <div
            id="trace-panel"
            role="tabpanel"
            aria-labelledby={`trace-tab-${tab}`}
          >
            {tab === 'overview' && (
              <Overview
                d={d}
                record={record}
                trace={query.data!}
                runs={runs}
                conversation={conversation}
                added={added}
                show={setTab}
              />
            )}
            {tab === 'conversation' && (
              <Conversation d={d} runs={runs} added={added} />
            )}
            {tab === 'tools' && (
              <Tools d={d} runs={runs} conversation={conversation} />
            )}
            {tab === 'timeline' && (
              <Timeline d={d} total={record?.latency_ms} />
            )}
            {tab === 'raw' && <Raw d={d} id={id} />}
          </div>
        </>
      )}
    </div>
  )
}

function ConversationCard({
  nav,
  added,
  d,
  show,
}: {
  nav: ConversationNav
  added?: Delta
  d: Detail
  show: (tab: Tab) => void
}) {
  const fresh = added ? [...added].filter(([, k]) => k === 'new') : []
  const kinds = fresh.map(([i]) =>
    d.input[i].blocks.some((b) => b.type === 'tool_result')
      ? 'tool result'
      : `${d.input[i].role} message`,
  )
  const counts = [...new Set(kinds)].map((k) =>
    plural(kinds.filter((x) => x === k).length, k),
  )
  return (
    <section className="trace-run-nav" aria-label="Conversation">
      <Workflow size={15} />
      <div>
        <strong>
          Call {nav.index} of {nav.total} in this conversation
        </strong>
        <span className="trace-secondary">
          {nav.index === 1
            ? 'First call of the run.'
            : added
              ? fresh.length
                ? `Added since the previous call: ${counts.join(', ')}.`
                : 'No new input since the previous call.'
              : 'Comparing with the previous call…'}
          {added && fresh.length > 0 && (
            <button
              type="button"
              className="trace-link"
              onClick={() => show('conversation')}
            >
              Show
            </button>
          )}
        </span>
      </div>
      <Button
        variant="outline"
        size="sm"
        disabled={!nav.previous}
        onClick={() => nav.previous && nav.open(nav.previous)}
      >
        Previous call
      </Button>
      <Button
        variant="outline"
        size="sm"
        disabled={!nav.next}
        onClick={() => nav.next && nav.open(nav.next)}
      >
        Next call
      </Button>
    </section>
  )
}

function Overview({
  d,
  record,
  trace,
  runs,
  conversation,
  added,
  show,
}: {
  d: Detail
  record?: Row
  trace: Trace
  runs: ToolRun[]
  conversation?: ConversationNav
  added?: Delta
  show: (tab: Tab) => void
}) {
  const usage = d.result?.usage ?? {}
  const s = d.summary
  const speed =
    record &&
    record.output_tokens > 0 &&
    record.ttfb_ms !== null &&
    record.latency_ms > record.ttfb_ms
      ? `${((record.output_tokens * 1000) / (record.latency_ms - record.ttfb_ms)).toFixed(1)} tok/s`
      : '—'
  return (
    <div className="trace-overview">
      {conversation && conversation.total > 1 && (
        <ConversationCard nav={conversation} added={added} d={d} show={show} />
      )}
      {record && (
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
            value={speed}
            hint={record.stream ? 'Streamed' : 'JSON response'}
          />
          <Metric
            icon={<Gauge size={15} />}
            title="Tokens"
            value={`${n(record.input_tokens)} → ${n(record.output_tokens)}`}
            hint={
              [
                s?.cached_tokens ? `${n(s.cached_tokens)} cached` : '',
                s?.reasoning_tokens ? `${n(s.reasoning_tokens)} reasoning` : '',
              ]
                .filter(Boolean)
                .join(' · ') || 'input → output'
            }
          />
          <Metric
            icon={<Activity size={15} />}
            title="Result"
            value={
              <span className={ok(record.status) ? '' : 'trace-bad'}>
                {record.status}
              </span>
            }
            hint={
              d.result?.finish_reasons.length
                ? label(d.result.finish_reasons.join(', '))
                : `${record.protocol} · ${operation(record)}`
            }
          />
        </div>
      )}
      {record && <Waterfall d={d} record={record} />}
      <div className="trace-cards">
        <section className="trace-card">
          <h3>
            <Route size={14} /> Routing
          </h3>
          {d.routing ? (
            <>
              <p className="trace-route">
                <code>
                  {d.routing.requested_model ||
                    d.parameters.model?.toString() ||
                    '—'}
                </code>
                {d.routing.model &&
                  d.routing.model !== d.routing.requested_model && (
                    <>
                      <ArrowRight size={12} /> <code>{d.routing.model}</code>
                    </>
                  )}
                {d.routing.upstream_model &&
                  d.routing.upstream_model !== d.routing.model && (
                    <>
                      <ArrowRight size={12} />{' '}
                      <code title="Model name sent upstream">
                        {d.routing.upstream_model}
                      </code>
                    </>
                  )}
              </p>
              <ol className="trace-attempts">
                {d.routing.attempts.map((a, i) => (
                  <li key={i} className={a.error ? 'failed' : ''}>
                    <strong>Attempt {i + 1}</strong> {a.replica || 'no replica'}
                    <span>
                      queue {ms(a.queue_ms)} · {ms(a.ms)}
                      {a.status ? ` · HTTP ${a.status}` : ''}
                    </span>
                    {a.error && <em>{a.error}</em>}
                  </li>
                ))}
                {d.routing.attempts.length === 0 && (
                  <li>Rejected before a replica was selected.</li>
                )}
              </ol>
              {d.routing.changes.length > 0 && (
                <>
                  <h4>Router changes to the forwarded body</h4>
                  <ul className="trace-changes">
                    {d.routing.changes.map((c, i) => (
                      <li key={i}>
                        <code>{c.field}</code> {c.action}
                        {c.from !== undefined && (
                          <>
                            {' '}
                            from <code>{JSON.stringify(c.from)}</code>
                          </>
                        )}
                        {c.to !== undefined && (
                          <>
                            {' '}
                            to <code>{JSON.stringify(c.to)}</code>
                          </>
                        )}
                      </li>
                    ))}
                  </ul>
                </>
              )}
              {d.routing.headers.length > 0 && (
                <>
                  <h4>Forwarded headers (names only)</h4>
                  <p className="trace-chips">
                    {d.routing.headers.map((h) => (
                      <span key={h}>{h}</span>
                    ))}
                  </p>
                </>
              )}
            </>
          ) : (
            <p className="trace-secondary">
              Recorded by an older router version.
            </p>
          )}
        </section>
        <section className="trace-card">
          <h3>
            <Fingerprint size={14} /> Request
          </h3>
          <dl className="trace-dl">
            <dt>Request ID</dt>
            <dd>
              <code>{trace.request_id}</code>{' '}
              <CopyButton value={trace.request_id} label="Copy request ID" />
            </dd>
            <dt>Operation</dt>
            <dd>{d.operation}</dd>
            {record && (
              <>
                <dt>Time</dt>
                <dd>{date(record.time)}</dd>
              </>
            )}
            {d.result?.id && (
              <>
                <dt>Response ID</dt>
                <dd>
                  <code>{d.result.id}</code>
                </dd>
              </>
            )}
            {d.result?.model && (
              <>
                <dt>Served model</dt>
                <dd>
                  <code>{d.result.model}</code>
                </dd>
              </>
            )}
            {d.result?.system_fingerprint && (
              <>
                <dt>Fingerprint</dt>
                <dd>
                  <code>{d.result.system_fingerprint}</code>
                </dd>
              </>
            )}
            {d.routing?.upstream_status !== undefined && (
              <>
                <dt>Upstream</dt>
                <dd>
                  HTTP {d.routing.upstream_status} {d.routing.content_type}
                </dd>
              </>
            )}
            <dt>Messages</dt>
            <dd>
              {plural(d.input.length, 'input message')} ·{' '}
              {plural(d.output.length, 'output')}
            </dd>
            <dt>Tools</dt>
            <dd>
              {plural((d.tools ?? []).length, 'declared tool')} ·{' '}
              {plural(
                runs.filter((r) => r.origin === 'output').length,
                'new call',
              )}
            </dd>
            <dt>Expires</dt>
            <dd>{date(trace.expires_at)}</dd>
          </dl>
          {Object.keys(usage).length > 0 && (
            <>
              <h4>Provider usage</h4>
              <dl className="trace-dl trace-usage">
                {Object.entries(usage).map(([k, v]) => (
                  <div key={k}>
                    <dt>{label(k)}</dt>
                    <dd>{n(v)}</dd>
                  </div>
                ))}
              </dl>
            </>
          )}
        </section>
      </div>
      <JsonTree value={d.parameters} title="Request parameters" open={1} />
    </div>
  )
}

// Where the time went, in the order it was spent.
function Waterfall({ d, record }: { d: Detail; record: Row }) {
  const total = Math.max(record.latency_ms, 1)
  const attempts = d.routing?.attempts ?? []
  const admission = d.routing?.admission_ms ?? 0
  const queue = attempts.reduce((s, a) => s + a.queue_ms, 0)
  const failed = attempts.slice(0, -1).reduce((s, a) => s + a.ms, 0)
  const first = record.ttfb_ms ?? record.latency_ms
  const wait = Math.max(first - admission - queue - failed, 0)
  const phases = [
    ['admission', 'Admission', admission],
    ['queue', 'Replica queue', queue],
    ['failover', 'Failed attempt', failed],
    ['wait', 'Waiting for first byte', wait],
    [
      'generate',
      record.stream ? 'Streaming' : 'Response body',
      Math.max(record.latency_ms - first, 0),
    ],
  ].filter(([, , v]) => (v as number) > 0) as [string, string, number][]
  const segs = segments(d.events).filter((s) =>
    ['reasoning', 'text', 'tool_call', 'refusal'].includes(s.type),
  )
  return (
    <section className="trace-waterfall" aria-label="Timing breakdown">
      <div className="trace-bar">
        {phases.map(([key, name, v]) => (
          <span
            key={key}
            className={`phase-${key}`}
            style={{ flexGrow: v }}
            title={`${name}: ${ms(v)}`}
          />
        ))}
      </div>
      {segs.length > 0 && (
        <div className="trace-bar trace-bar-events" aria-hidden>
          {segs.map((s, i) => (
            <span
              key={i}
              className={`seg-${s.type}`}
              style={{
                left: `${(s.start / total) * 100}%`,
                width: `max(2px, ${((s.end - s.start) / total) * 100}%)`,
              }}
              title={`${label(s.type)} ${ms(s.start)}–${ms(s.end)}`}
            />
          ))}
        </div>
      )}
      <ul className="trace-legend">
        {phases.map(([key, name, v]) => (
          <li key={key}>
            <i className={`phase-${key}`} />
            {name} <strong>{ms(v)}</strong>
          </li>
        ))}
        {[...new Set(segs.map((s) => s.type))].map((t) => (
          <li key={t}>
            <i className={`seg-${t}`} />
            {label(t)}
          </li>
        ))}
      </ul>
    </section>
  )
}

function toolNames(runs: ToolRun[]) {
  const names = new Map<string, ToolRun>()
  runs.forEach((r) => r.id && names.set(r.id, r))
  return names
}

function Conversation({
  d,
  runs,
  added,
}: {
  d: Detail
  runs: ToolRun[]
  added?: Delta
}) {
  const [term, setTerm] = useState('')
  const [rendered, setRendered] = useState(true)
  const [showEarlier, setShowEarlier] = useState(false)
  const byID = toolNames(runs)
  const match = (m: Message) =>
    !term || textOf(m).toLowerCase().includes(term.toLowerCase())
  // Agent loops resend the whole history. Keep system prompts and the last
  // four messages in view, folding the rounds in between.
  const fold = !term && !showEarlier && d.input.length > 8
  const kept = d.input
    .map((m, i) => [m, i] as const)
    .filter(
      ([m, i]) =>
        !fold ||
        m.role === 'system' ||
        m.role === 'developer' ||
        i >= d.input.length - 4 ||
        added?.has(i),
    )
  const hidden = d.input.length - kept.length
  const ctx = { term, rendered, byID, added }
  return (
    <>
      <div className="trace-conversation-tools">
        <div className="trace-search">
          <Search size={15} />
          <Input
            aria-label="Search this trace"
            placeholder="Search messages and tool calls…"
            value={term}
            onChange={(e) => setTerm(e.target.value)}
          />
        </div>
        <div className="trace-segmented" role="group" aria-label="Text format">
          <button
            type="button"
            aria-pressed={rendered}
            onClick={() => setRendered(true)}
          >
            Formatted
          </button>
          <button
            type="button"
            aria-pressed={!rendered}
            onClick={() => setRendered(false)}
          >
            Plain
          </button>
        </div>
      </div>
      <div className="trace-conversation">
        <section className="trace-messages">
          <h3>
            Input <span>{plural(d.input.length, 'message')}</span>
          </h3>
          {d.input.length === 0 && (
            <p className="trace-secondary">No content captured.</p>
          )}
          {kept.map(([m, i], k) => (
            <div key={i}>
              {fold && hidden > 0 && k > 0 && kept[k - 1][1] !== i - 1 && (
                <button
                  type="button"
                  className="trace-fold"
                  onClick={() => setShowEarlier(true)}
                >
                  Show {plural(i - kept[k - 1][1] - 1, 'earlier message')}
                </button>
              )}
              {match(m) && <MessageCard m={m} index={i} {...ctx} />}
            </div>
          ))}
          {term && d.input.every((m) => !match(m)) && (
            <p className="trace-secondary">No input matches.</p>
          )}
        </section>
        <section className="trace-messages">
          <h3>
            Output <span>{plural(d.output.length, 'message')}</span>
          </h3>
          {d.output.length === 0 && (
            <p className="trace-secondary">No content captured.</p>
          )}
          {d.output.map(
            (m, i) =>
              match(m) && (
                <MessageCard key={i} m={m} index={i} output {...ctx} />
              ),
          )}
        </section>
      </div>
    </>
  )
}

type Ctx = {
  term: string
  rendered: boolean
  byID: Map<string, ToolRun>
  added?: Delta
}

function MessageCard({
  m,
  index,
  output,
  ...ctx
}: Ctx & { m: Message; index: number; output?: boolean }) {
  const text = m.blocks.map((b) => b.text ?? b.arguments ?? '').join('\n')
  const kind = output ? undefined : ctx.added?.get(index)
  return (
    <article
      className={`trace-message trace-role-${m.role} ${kind === 'new' ? 'trace-new' : ''}`}
      id={`${output ? 'out' : 'in'}-${index}`}
    >
      <div className="trace-role">
        <span>
          {m.role} <small>#{index + 1}</small>
          {kind === 'new' && (
            <span className="trace-badge-new">new in this call</span>
          )}
          {kind === 'echo' && (
            <span className="trace-badge-echo">previous response</span>
          )}
        </span>
        <span>
          <small>{n(text.length)} chars</small>
          <CopyButton value={text} label="Copy message" />
        </span>
      </div>
      {m.blocks.map((b, j) => (
        <Content key={j} block={b} {...ctx} />
      ))}
    </article>
  )
}

function Folded({
  text,
  children,
  lines = 14,
}: {
  text: string
  children: (t: string) => ReactNode
  lines?: number
}) {
  const [open, setOpen] = useState(false)
  const all = text.split('\n')
  const long = all.length > lines + 4 || text.length > 2400
  if (!long || open)
    return (
      <>
        {children(text)}
        {long && (
          <button
            type="button"
            className="trace-fold"
            onClick={() => setOpen(false)}
          >
            Show less
          </button>
        )}
      </>
    )
  const short = all.slice(0, lines).join('\n').slice(0, 2000)
  return (
    <>
      {children(short)}
      <button
        type="button"
        className="trace-fold"
        onClick={() => setOpen(true)}
      >
        Show all · {plural(all.length, 'line')}, {n(text.length)} chars
      </button>
    </>
  )
}

function Content({ block, term, rendered, byID }: Ctx & { block: Block }) {
  if (block.type === 'reasoning')
    return (
      <details
        className="trace-reasoning"
        open={
          !!term &&
          (block.text ?? '').toLowerCase().includes(term.toLowerCase())
        }
      >
        <summary>
          <Sparkles size={14} />
          Reasoning · {n((block.text ?? '').length)} chars
        </summary>
        <pre>
          <Highlight text={block.text ?? ''} term={term} />
        </pre>
      </details>
    )
  if (block.type === 'tool_call' || block.type === 'tool_result') {
    const run = block.id ? byID.get(block.id) : undefined
    const call = block.type === 'tool_call'
    const name = block.name || run?.name
    return (
      <div className={`trace-tool ${call ? '' : 'trace-tool-result'}`}>
        <div>
          {call ? <Wrench size={14} /> : <CornerDownRight size={14} />}
          <strong>
            {call ? 'Tool call' : 'Tool result'}
            {name ? ` · ${name}` : ''}
          </strong>
          {block.id && <code className="trace-secondary">{block.id}</code>}
          {call && run?.resultMessage !== undefined && (
            <a href={`#in-${run.resultMessage}`} className="trace-link">
              result in #{run.resultMessage + 1}
            </a>
          )}
          {!call &&
            run?.callMessage !== undefined &&
            run.origin === 'input' && (
              <a href={`#in-${run.callMessage}`} className="trace-link">
                called in #{run.callMessage + 1}
              </a>
            )}
        </div>
        {term ? (
          <pre>
            <Highlight
              text={(call ? block.arguments : block.text) ?? ''}
              term={term}
            />
          </pre>
        ) : (
          <Structured value={(call ? block.arguments : block.text) ?? ''} />
        )}
      </div>
    )
  }
  if (block.type === 'text' && !block.text) return null
  const media = ['image', 'document', 'audio'].includes(block.type)
  if (media)
    return (
      <p className="trace-media">
        <Braces size={13} /> {block.text}
      </p>
    )
  return (
    <div
      className={`trace-text ${block.type === 'refusal' ? 'trace-refusal' : ''}`}
    >
      {block.type === 'refusal' && <strong>Refusal</strong>}
      <Folded text={block.text ?? ''}>
        {(t) =>
          rendered ? (
            <Markdown text={t} term={term} />
          ) : (
            <pre>
              <Highlight text={t} term={term} />
            </pre>
          )
        }
      </Folded>
    </div>
  )
}

function Tools({
  d,
  runs,
  conversation,
}: {
  d: Detail
  runs: ToolRun[]
  conversation?: ConversationNav
}) {
  const declared = d.tools ?? []
  const calls = new Map<string, number>()
  runs.forEach((r) => calls.set(r.name, (calls.get(r.name) ?? 0) + 1))
  const fresh = runs.filter((r) => r.origin === 'output')
  const history = runs.filter((r) => r.origin === 'input')
  return (
    <div className="trace-tools">
      <p className="trace-secondary">
        The router forwards tool calls; your client runs the tools and sends
        their results in the next request.
      </p>
      {runs.length === 0 && (
        <p className="empty">No tool calls in this request.</p>
      )}
      {fresh.length > 0 && (
        <section>
          <h3>Requested by the model in this response</h3>
          {fresh.map((r) => (
            <RunCard key={r.key} run={r} conversation={conversation} />
          ))}
        </section>
      )}
      {history.length > 0 && (
        <section>
          <h3>Earlier rounds in the input</h3>
          {history.map((r) => (
            <RunCard key={r.key} run={r} conversation={conversation} />
          ))}
        </section>
      )}
      <section>
        <h3>Declared tools</h3>
        {declared.length === 0 && Array.isArray(d.parameters.tools) && (
          <p className="trace-secondary">
            Names only (recorded by an older router):{' '}
            {(d.parameters.tools as string[]).join(', ')}
          </p>
        )}
        {declared.length === 0 && !Array.isArray(d.parameters.tools) && (
          <p className="trace-secondary">No tools declared.</p>
        )}
        {declared.map((t) => (
          <details key={t.name} className="trace-declared">
            <summary>
              <Wrench size={13} />
              <strong>{t.name}</strong>
              <span className="trace-secondary">{t.description}</span>
              <span className="trace-count">
                {plural(calls.get(t.name) ?? 0, 'call')}
              </span>
            </summary>
            {t.schema !== undefined ? (
              <JsonTree value={t.schema} title="Parameters schema" open={3} />
            ) : (
              <p className="trace-secondary">
                {t.truncated ? 'Schema above the 8 KiB limit.' : 'No schema.'}
              </p>
            )}
          </details>
        ))}
      </section>
    </div>
  )
}

function RunCard({
  run,
  conversation,
}: {
  run: ToolRun
  conversation?: ConversationNav
}) {
  const status =
    run.result !== undefined
      ? [
          'done',
          run.origin === 'input' && run.callMessage === undefined
            ? 'Result without a matching call'
            : 'Result returned',
        ]
      : run.origin === 'output'
        ? [
            'waiting',
            conversation?.next
              ? 'Your client answered in the next call'
              : 'Awaiting your client — the result comes in the next request',
          ]
        : ['missing', 'No result submitted']
  return (
    <article className={`trace-run trace-run-${status[0]}`}>
      <header>
        <Wrench size={14} />
        <strong>{run.name}</strong>
        {!run.declared && <span className="trace-flag">not declared</span>}
        {run.result === undefined &&
          run.origin === 'output' &&
          conversation?.next && (
            <button
              type="button"
              className="trace-link"
              onClick={() => conversation.open(conversation.next!)}
            >
              Open next call
            </button>
          )}
        {run.id && <code className="trace-secondary">{run.id}</code>}
        <span className="trace-run-status">{status[1]}</span>
      </header>
      <div className="trace-run-body">
        <div>
          <h4>Arguments</h4>
          {run.arguments !== undefined ? (
            parseJSON(run.arguments) !== undefined ? (
              <JsonTree value={parseJSON(run.arguments)} open={3} />
            ) : (
              <pre className="trace-plain">{run.arguments || '—'}</pre>
            )
          ) : (
            <p className="trace-secondary">—</p>
          )}
        </div>
        <div>
          <h4>Result</h4>
          {run.result !== undefined ? (
            <Structured value={run.result} />
          ) : (
            <p className="trace-secondary">—</p>
          )}
        </div>
      </div>
    </article>
  )
}

function Timeline({ d, total }: { d: Detail; total?: number }) {
  const [raw, setRaw] = useState(false)
  const [kind, setKind] = useState('all')
  const [limit, setLimit] = useState(100)
  const segs = segments(d.events)
  const end = Math.max(total ?? 0, d.events.at(-1)?.ms ?? 0, 1)
  if (d.events.length === 0)
    return (
      <p className="empty">
        No stream events. JSON responses are shown in Conversation and Raw.
      </p>
    )
  const events = d.events.filter((e) => kind === 'all' || e.type === kind)
  return (
    <>
      <div className="trace-event-head">
        <p>
          Observed delivery times relative to request start. Consecutive deltas
          of one kind are merged into segments.
        </p>
        <div
          className="trace-segmented"
          role="group"
          aria-label="Timeline view"
        >
          <button
            type="button"
            aria-pressed={!raw}
            onClick={() => setRaw(false)}
          >
            Segments
          </button>
          <button type="button" aria-pressed={raw} onClick={() => setRaw(true)}>
            All events
          </button>
        </div>
      </div>
      {!raw ? (
        <ol className="trace-segments">
          {segs.map((s, i) => (
            <li key={i}>
              <div className="trace-segment-bar" aria-hidden>
                <span
                  className={`seg-${s.type}`}
                  style={{
                    left: `${(s.start / end) * 100}%`,
                    width: `max(3px, ${((s.end - s.start) / end) * 100}%)`,
                  }}
                />
              </div>
              <details>
                <summary>
                  <span className={`trace-event-kind trace-event-${s.type}`}>
                    {label(s.type)}
                  </span>
                  <time>
                    +{ms(s.start)}
                    {s.end > s.start && ` – ${ms(s.end)}`}
                  </time>
                  <span className="trace-secondary">
                    {plural(s.count, 'event')}
                    {s.text && ` · ${n(s.text.length)} chars`}
                  </span>
                  {s.text && (
                    <span className="trace-preview">{s.text.slice(0, 90)}</span>
                  )}
                </summary>
                {s.text && <pre>{s.text}</pre>}
              </details>
            </li>
          ))}
        </ol>
      ) : (
        <>
          <select
            aria-label="Filter stream events"
            value={kind}
            onChange={(e) => {
              setKind(e.target.value)
              setLimit(100)
            }}
          >
            <option value="all">All events</option>
            {[...new Set(d.events.map((e) => e.type))].map((k) => (
              <option key={k} value={k}>
                {label(k)}
              </option>
            ))}
          </select>
          <ol className="trace-timeline">
            {events.slice(0, limit).map((e, i) => (
              <li key={i}>
                <time>+{ms(e.ms)}</time>
                <span className={`trace-event-kind trace-event-${e.type}`}>
                  {label(e.type)}
                </span>
                <pre>{e.text || '—'}</pre>
              </li>
            ))}
          </ol>
          {events.length > limit && (
            <Button variant="outline" onClick={() => setLimit((v) => v + 100)}>
              Show 100 more events
            </Button>
          )}
        </>
      )}
    </>
  )
}

function Raw({ d, id }: { d: Detail; id: string }) {
  const sent = forwarded(d)
  const views = [
    d.request !== undefined && (['request', 'Sent by client'] as const),
    sent !== undefined && (['forwarded', 'Forwarded upstream'] as const),
    d.response !== undefined && (['response', 'Response'] as const),
    (d.frames?.length ?? 0) > 0 &&
      (['frames', `Stream frames (${d.frames!.length})`] as const),
    ['trace', 'Full trace'] as const,
  ].filter(Boolean) as (readonly [string, string])[]
  const [view, setView] = useState(views[0][0])
  const short = id.slice(0, 12)
  return (
    <>
      <div
        className="trace-segmented trace-raw-switch"
        role="group"
        aria-label="Raw view"
      >
        {views.map(([key, name]) => (
          <button
            key={key}
            type="button"
            aria-pressed={view === key}
            onClick={() => setView(key)}
          >
            {name}
          </button>
        ))}
      </div>
      {d.request === undefined && (
        <p className="trace-secondary">
          Raw previews are recorded by newer routers. Binary payloads,
          signatures, metadata and vectors are always omitted.
        </p>
      )}
      {view === 'request' && (
        <JsonTree
          value={d.request}
          title="Request body as received (sanitized)"
          open={3}
          download={`request-${short}.json`}
        />
      )}
      {view === 'forwarded' && (
        <>
          {(d.routing?.changes.length ?? 0) > 0 ? (
            <ul className="trace-changes trace-changes-inline">
              {d.routing!.changes.map((c, i) => (
                <li key={i}>
                  Router {c.action} <code>{c.field}</code>
                  {c.to !== undefined && (
                    <>
                      {' '}
                      = <code>{JSON.stringify(c.to)}</code>
                    </>
                  )}
                </li>
              ))}
            </ul>
          ) : (
            <p className="trace-secondary">
              The router forwarded the body unchanged.
            </p>
          )}
          <JsonTree
            value={sent}
            title="Body sent to the model server (reconstructed)"
            open={3}
            download={`forwarded-${short}.json`}
          />
        </>
      )}
      {view === 'response' && (
        <JsonTree
          value={d.response}
          title="Response body (sanitized)"
          open={3}
          download={`response-${short}.json`}
        />
      )}
      {view === 'frames' && (
        <ol className="trace-frames">
          {d.frames!.map((f, i) => (
            <li key={i}>
              <time>+{ms(f.ms)}</time>
              <code>
                {typeof f.data === 'string' ? f.data : JSON.stringify(f.data)}
              </code>
            </li>
          ))}
        </ol>
      )}
      {view === 'trace' && (
        <JsonTree
          value={d}
          title="Normalized trace"
          open={1}
          download={`trace-${short}.json`}
        />
      )}
    </>
  )
}
