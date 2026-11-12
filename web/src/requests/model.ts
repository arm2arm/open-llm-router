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
