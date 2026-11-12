// Trace shapes from GET /portal/api/requests/{id}. Version 1 traces (stored
// before raw previews existed) lack every optional field below.
import type { RequestRecord } from '@/lib/api'

export type Block = {
  type: string
  text?: string
  name?: string
  id?: string
  arguments?: string
}
export type Message = { role: string; blocks: Block[]; hash?: string }
export type TraceEvent = { ms: number; type: string; text?: string }
export type Tool = {
  name: string
  description?: string
  schema?: unknown
  truncated?: boolean
}
export type Change = {
  field: string
  action: string
  from?: unknown
  to?: unknown
}
export type Attempt = {
  replica: string
  queue_ms: number
  ms: number
  status?: number
  error?: string
}
export type Routing = {
  requested_model?: string
  model?: string
  upstream_model?: string
  changes: Change[]
  attempts: Attempt[]
  admission_ms: number
  headers: string[]
  upstream_status?: number
  content_type?: string
}
export type Result = {
  id?: string
  model?: string
  system_fingerprint?: string
  finish_reasons: string[]
  usage: Record<string, number>
}
export type Summary = {
  input_messages: number
  output_messages: number
  tool_calls: number
  tool_results: number
  tools: number
  finish_reason?: string
  cached_tokens: number
  reasoning_tokens: number
  truncated: boolean
}
export type Detail = {
  v?: number
  operation: string
  input: Message[]
  output: Message[]
  events: TraceEvent[]
  parameters: Record<string, unknown>
  tools?: Tool[]
  request?: unknown
  response?: unknown
  frames?: { ms: number; data: unknown }[]
  routing?: Routing
  result?: Result
  summary?: Summary
  truncated: boolean
  warnings: string[]
}
export type Trace = { request_id: string; expires_at: string; detail: Detail }
export type Row = RequestRecord & {
  trace_summary?: Summary | null
  conversation?: string | null
  fresh?: boolean // client-side: arrived after the page loaded
}

export const n = (v: number) => new Intl.NumberFormat().format(v)
export const ms = (v: number | null | undefined) =>
  v === null || v === undefined
    ? '—'
    : v >= 1000
      ? `${(v / 1000).toFixed(2)} s`
      : `${n(Math.round(v))} ms`
export const date = (v: string) => new Date(v).toLocaleString()
export const shortDate = (v: string) =>
  new Date(v).toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
export const label = (v: string) => v.replaceAll('_', ' ')
export const plural = (count: number, word: string) =>
  `${n(count)} ${word}${count === 1 ? '' : 's'}`
export const operation = (r: { operation: string }) =>
  !r.operation
    ? 'Request'
    : r.operation === 'embeddings'
      ? 'Embedding'
      : r.operation === 'completions'
        ? 'Completion'
        : 'Chat'
export const ok = (status: string) => status.startsWith('2')

export function parseJSON(value: string | undefined): unknown {
  if (value === undefined || value === '') return undefined
  try {
    return JSON.parse(value)
  } catch {
    return undefined
  }
}

// A tool call and the result the client later submitted for it.
export type ToolRun = {
  key: string
  id?: string
  name: string
  arguments?: string
  result?: string
  // Where the call appears: an earlier round in the input, or this response.
  origin: 'input' | 'output'
  callMessage?: number
  resultMessage?: number
  declared: boolean
}

export function toolRuns(d: Detail): ToolRun[] {
  const declared = new Set((d.tools ?? []).map((t) => t.name))
  const names = Array.isArray(d.parameters.tools)
    ? (d.parameters.tools as string[])
    : []
  names.forEach((name) => declared.add(name))
  const runs: ToolRun[] = []
  const byID = new Map<string, ToolRun>()
  const pending: ToolRun[] = []
  const add = (b: Block, origin: 'input' | 'output', index: number) => {
    const run: ToolRun = {
      key: `${origin}-${index}-${runs.length}`,
      id: b.id || undefined,
      name: b.name || 'unnamed tool',
      arguments: b.arguments,
      origin,
      callMessage: index,
      declared: declared.size === 0 || declared.has(b.name ?? ''),
    }
    runs.push(run)
    if (run.id) byID.set(run.id, run)
    else pending.push(run)
  }
  d.input.forEach((m, i) =>
    m.blocks.forEach((b) => {
      if (b.type === 'tool_call') add(b, 'input', i)
      if (b.type === 'tool_result') {
        // Prefer the ID; legacy function results pair in order.
        const run = (b.id && byID.get(b.id)) || pending.shift()
        if (run && run.result === undefined) {
          run.result = b.text ?? ''
          run.resultMessage = i
        } else {
          runs.push({
            key: `orphan-${i}-${runs.length}`,
            id: b.id || undefined,
            name: b.name || 'unknown call',
            result: b.text ?? '',
            origin: 'input',
            resultMessage: i,
            declared: true,
          })
        }
      }
    }),
  )
  d.output.forEach((m, i) =>
    m.blocks.forEach((b) => {
      if (b.type === 'tool_call') add(b, 'output', i)
    }),
  )
  return runs
}

// Consecutive deltas of one kind become a segment, so a 900-event stream reads
// as "reasoning 0.4–2.1 s, then text 2.1–5.0 s" instead of 900 rows.
export type Segment = {
  type: string
  start: number
  end: number
  count: number
  text: string
}
export function segments(events: TraceEvent[]): Segment[] {
  const out: Segment[] = []
  for (const e of events) {
    const last = out.at(-1)
    if (
      last &&
      last.type === e.type &&
      !['complete', 'error'].includes(e.type)
    ) {
      last.end = e.ms
      last.count++
      last.text += e.text ?? ''
    } else {
      out.push({
        type: e.type,
        start: e.ms,
        end: e.ms,
        count: 1,
        text: e.text ?? '',
      })
    }
  }
  return out
}

// The body forwarded upstream: the client's request with the router's
// recorded changes applied. Returns undefined without a raw preview.
export function forwarded(d: Detail): unknown {
  if (!d.request || typeof d.request !== 'object') return undefined
  const body = structuredClone(d.request) as Record<string, unknown>
  for (const c of d.routing?.changes ?? []) {
    const path = c.field.split('.')
    let target = body
    for (const key of path.slice(0, -1)) {
      if (typeof target[key] !== 'object' || target[key] === null)
        target[key] = {}
      target = target[key] as Record<string, unknown>
    }
    target[path.at(-1)!] = c.to
  }
  return body
}

export function textOf(m: Message) {
  return m.blocks
    .map((b) => `${b.name ?? ''} ${b.arguments ?? ''} ${b.text ?? ''}`)
    .join(' ')
}

// Agent runs: the requests of one conversation (same client session, or the
// same system prompt and first user message). A content-keyed conversation
// is split where its input stops growing: an agent loop always appends, while
// repeating one prompt (a benchmark, a retry script) starts over each time.
// Rows stay newest first; a run with a single request stays a plain row.
export type Item =
  { kind: 'row'; row: Row } | { kind: 'group'; key: string; rows: Row[] }
export function runKeys(rows: Row[]): Map<Row, string> {
  const byKey = new Map<string, Row[]>()
  for (const r of rows)
    if (r.conversation) {
      const list = byKey.get(r.conversation) ?? []
      list.push(r)
      byKey.set(r.conversation, list)
    }
  const keys = new Map<Row, string>()
  for (const [key, list] of byKey) {
    const ordered = [...list].sort(
      (a, b) => Date.parse(a.time) - Date.parse(b.time),
    )
    let run = 0
    let last = -1
    for (const r of ordered) {
      const size = r.trace_summary?.input_messages ?? 0
      if (key.startsWith('c') && last >= 0 && size <= last) run++
      last = size
      keys.set(r, `${key}:${run}`)
    }
  }
  return keys
}
export function groupRows(rows: Row[]): Item[] {
  const keys = runKeys(rows)
  const members = new Map<string, Row[]>()
  for (const r of rows) {
    const key = keys.get(r)
    if (key) members.set(key, [...(members.get(key) ?? []), r])
  }
  const items: Item[] = []
  const placed = new Set<string>()
  for (const r of rows) {
    const key = keys.get(r)
    const list = key ? members.get(key)! : undefined
    if (!list || list.length < 2) items.push({ kind: 'row', row: r })
    else if (!placed.has(key!)) {
      placed.add(key!)
      items.push({ kind: 'group', key: key!, rows: list })
    }
  }
  return items
}

export type RunStats = {
  calls: number
  start: number
  end: number
  busy: number // summed request latency; the rest is client time
  input: number
  output: number
  toolCalls: number
  failed: number
  models: string[]
}
export function runStats(rows: Row[]): RunStats {
  const start = Math.min(...rows.map((r) => Date.parse(r.time)))
  const end = Math.max(...rows.map((r) => Date.parse(r.time) + r.latency_ms))
  return {
    calls: rows.length,
    start,
    end,
    busy: rows.reduce((s, r) => s + r.latency_ms, 0),
    input: rows.reduce((s, r) => s + r.input_tokens, 0),
    output: rows.reduce((s, r) => s + r.output_tokens, 0),
    toolCalls: rows.reduce((s, r) => s + (r.trace_summary?.tool_calls ?? 0), 0),
    failed: rows.filter((r) => !ok(r.status)).length,
    models: [...new Set(rows.map((r) => r.model).filter(Boolean))],
  }
}

// Which input messages a call added since the previous call of its
// conversation. Agent loops append: when the earlier input is an unchanged
// prefix, everything after it is new (an assistant message there is the
// previous response sent back). Otherwise fall back to hash membership.
export type Delta = Map<number, 'new' | 'echo'>
export function delta(current: Detail, previous: Detail): Delta {
  const out: Delta = new Map()
  const prefix = previous.input.every(
    (m, i) => !m.hash || current.input[i]?.hash === m.hash,
  )
  if (prefix && previous.input.length <= current.input.length) {
    current.input.forEach((m, i) => {
      if (i >= previous.input.length)
        out.set(i, m.role === 'assistant' ? 'echo' : 'new')
    })
    return out
  }
  const known = new Set(
    [...previous.input, ...previous.output].map((m) => m.hash),
  )
  current.input.forEach((m, i) => {
    if (m.hash && !known.has(m.hash)) out.set(i, 'new')
  })
  return out
}
