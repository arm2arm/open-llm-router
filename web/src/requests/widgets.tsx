import { useState, type ReactNode } from 'react'
import { Check, ChevronRight, Copy, Download } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { parseJSON } from './model'

// Clipboard and download act only on an explicit click. Nothing is stored.
export function CopyButton({
  value,
  label = 'Copy',
}: {
  value: string
  label?: string
}) {
  const [done, setDone] = useState(false)
  return (
    <Button
      variant="ghost"
      size="sm"
      className="trace-icon-button"
      aria-label={label}
      title={label}
      onClick={() => {
        void navigator.clipboard?.writeText(value).then(() => {
          setDone(true)
          setTimeout(() => setDone(false), 1200)
        })
      }}
    >
      {done ? <Check size={13} /> : <Copy size={13} />}
      <span className="sr-only">{label}</span>
    </Button>
  )
}

export function DownloadButton({
  value,
  name,
}: {
  value: unknown
  name: string
}) {
  return (
    <Button
      variant="ghost"
      size="sm"
      className="trace-icon-button"
      aria-label={`Download ${name}`}
      title={`Download ${name}`}
      onClick={() => {
        const url = URL.createObjectURL(
          new Blob([JSON.stringify(value, null, 2)], {
            type: 'application/json',
          }),
        )
        const a = document.createElement('a')
        a.href = url
        a.download = name
        a.click()
        setTimeout(() => URL.revokeObjectURL(url), 0)
      }}
    >
      <Download size={13} />
    </Button>
  )
}

// Router markers such as «image payload omitted» render as muted badges.
function StringValue({ value }: { value: string }) {
  const parts = value.split(/(«[^»]*»)/g)
  return (
    <span className="json-string">
      "
      {parts.map((p, i) =>
        p.startsWith('«') ? (
          <span key={i} className="json-marker">
            {p.slice(1, -1)}
          </span>
        ) : (
          p
        ),
      )}
      "
    </span>
  )
}

function Leaf({ value }: { value: unknown }) {
  if (typeof value === 'string') return <StringValue value={value} />
  if (value === null) return <span className="json-null">null</span>
  if (typeof value === 'boolean')
    return <span className="json-bool">{String(value)}</span>
  return <span className="json-number">{String(value)}</span>
}

// A collapsed object names itself by its most telling field.
function preview(o: Record<string, unknown>) {
  for (const k of ['role', 'name', 'type', 'id', 'tool_call_id']) {
    if (typeof o[k] === 'string')
      return `${k}: ${(o[k] as string).slice(0, 40)}`
  }
  return ''
}

function Node({
  name,
  value,
  depth,
  open,
  last,
}: {
  name?: string
  value: unknown
  depth: number
  open: number
  last: boolean
}) {
  const container = typeof value === 'object' && value !== null
  const [expanded, setExpanded] = useState(depth < open)
  const key = name !== undefined && <span className="json-key">{name}: </span>
  if (!container)
    return (
      <div className="json-row" style={{ paddingLeft: depth * 14 }}>
        {key}
        <Leaf value={value} />
        {!last && ','}
      </div>
    )
  const list = Array.isArray(value)
  const entries = list
    ? (value as unknown[]).map((v, i) => [String(i), v] as const)
    : Object.entries(value as Record<string, unknown>)
  const [l, r] = list ? ['[', ']'] : ['{', '}']
  if (entries.length === 0)
    return (
      <div className="json-row" style={{ paddingLeft: depth * 14 }}>
        {key}
        {l}
        {r}
        {!last && ','}
      </div>
    )
  return (
    <>
      <div className="json-row" style={{ paddingLeft: depth * 14 }}>
        <button
          type="button"
          className="json-toggle"
          aria-expanded={expanded}
          aria-label={expanded ? 'Collapse' : 'Expand'}
          onClick={() => setExpanded(!expanded)}
        >
          <ChevronRight size={11} />
        </button>
        {key}
        {l}
        {!expanded && (
          <button
            type="button"
            className="json-summary"
            onClick={() => setExpanded(true)}
          >
            {list ? `${entries.length} items` : `${entries.length} keys`}
          </button>
        )}
        {!expanded && !list && preview(value as Record<string, unknown>) && (
          <span className="json-preview">
            {preview(value as Record<string, unknown>)}
          </span>
        )}
        {!expanded && r}
        {!expanded && !last && ','}
      </div>
      {expanded &&
        entries.map(([k, v], i) => (
          <Node
            key={k}
            name={list ? undefined : k}
            value={v}
            depth={depth + 1}
            open={open}
            last={i === entries.length - 1}
          />
        ))}
      {expanded && (
        <div className="json-row" style={{ paddingLeft: depth * 14 }}>
          {r}
          {!last && ','}
        </div>
      )}
    </>
  )
}

export function JsonTree({
  value,
  open = 2,
  title,
  download,
}: {
  value: unknown
  open?: number
  title?: ReactNode
  download?: string
}) {
  const text = JSON.stringify(value, null, 2)
  return (
    <div className="json-tree">
      <div className={title ? 'json-tree-head' : 'json-tree-float'}>
        {title && <span>{title}</span>}
        <span>
          <CopyButton value={text} label="Copy JSON" />
          {download && <DownloadButton value={value} name={download} />}
        </span>
      </div>
      <div className="json-tree-body" role="tree">
        <Node value={value} depth={0} open={open} last />
      </div>
    </div>
  )
}

// A JSON tree when the value parses, otherwise preformatted text.
export function Structured({ value }: { value: string }) {
  const parsed = parseJSON(value)
  if (typeof parsed === 'object' && parsed !== null)
    return <JsonTree value={parsed} open={3} />
  return <pre className="trace-plain">{value}</pre>
}

export function Highlight({ text, term }: { text: string; term: string }) {
  if (!term) return <>{text}</>
  const lower = text.toLowerCase()
  const needle = term.toLowerCase()
  const out: ReactNode[] = []
  let at = 0
  for (let i = lower.indexOf(needle); i >= 0; i = lower.indexOf(needle, at)) {
    out.push(
      text.slice(at, i),
      <mark key={i}>{text.slice(i, i + needle.length)}</mark>,
    )
    at = i + needle.length
  }
  out.push(text.slice(at))
  return <>{out}</>
}

// A deliberately small Markdown subset rendered as React elements: fenced
// code, headings, lists, quotes, paragraphs, inline code, bold and italics.
// Never HTML: model output is untrusted.
function inline(text: string, term: string): ReactNode[] {
  const out: ReactNode[] = []
  const re = /(`[^`]+`|\*\*[^*]+\*\*|\*[^*\s][^*]*\*)/g
  let at = 0
  for (const m of text.matchAll(re)) {
    out.push(
      <Highlight key={`t${at}`} text={text.slice(at, m.index)} term={term} />,
    )
    const token = m[0]
    if (token.startsWith('`'))
      out.push(<code key={m.index}>{token.slice(1, -1)}</code>)
    else if (token.startsWith('**'))
      out.push(
        <strong key={m.index}>
          <Highlight text={token.slice(2, -2)} term={term} />
        </strong>,
      )
    else
      out.push(
        <em key={m.index}>
          <Highlight text={token.slice(1, -1)} term={term} />
        </em>,
      )
    at = m.index + token.length
  }
  out.push(<Highlight key="end" text={text.slice(at)} term={term} />)
  return out
}

export function Markdown({ text, term = '' }: { text: string; term?: string }) {
  const lines = text.split('\n')
  const out: ReactNode[] = []
  for (let i = 0; i < lines.length;) {
    const line = lines[i]
    const fence = line.match(/^\s*```\s*([\w+-]*)/)
    if (fence) {
      const body: string[] = []
      i++
      while (i < lines.length && !/^\s*```/.test(lines[i]))
        body.push(lines[i++])
      i++
      out.push(
        <pre key={i} className="md-code" data-lang={fence[1] || undefined}>
          <Highlight text={body.join('\n')} term={term} />
        </pre>,
      )
      continue
    }
    const heading = line.match(/^(#{1,6})\s+(.*)/)
    if (heading) {
      out.push(
        <p key={i} className={`md-h md-h${heading[1].length}`}>
          {inline(heading[2], term)}
        </p>,
      )
      i++
      continue
    }
    if (/^\s*([-*+]|\d+[.)])\s+/.test(line)) {
      const ordered = /^\s*\d/.test(line)
      const items: [string, number][] = []
      while (i < lines.length && /^\s*([-*+]|\d+[.)])\s+/.test(lines[i]))
        items.push([
          lines[i].replace(/^\s*([-*+]|\d+[.)])\s+/, ''),
          Math.min(Math.floor(lines[i++].search(/\S/) / 2), 2),
        ])
      const List = ordered ? 'ol' : 'ul'
      out.push(
        <List key={i}>
          {items.map((item, j) => (
            <li key={j} data-depth={item[1] || undefined}>
              {inline(item[0], term)}
            </li>
          ))}
        </List>,
      )
      continue
    }
    if (line.startsWith('>')) {
      const quote: string[] = []
      while (i < lines.length && lines[i].startsWith('>'))
        quote.push(lines[i++].replace(/^>\s?/, ''))
      out.push(<blockquote key={i}>{inline(quote.join(' '), term)}</blockquote>)
      continue
    }
    if (line.trim() === '') {
      i++
      continue
    }
    const para: string[] = []
    while (
      i < lines.length &&
      lines[i].trim() !== '' &&
      !/^\s*(```|#{1,6}\s|[-*+]\s|\d+[.)]\s|>)/.test(lines[i])
    )
      para.push(lines[i++])
    if (para.length === 0) para.push(lines[i++]) // never stall on an odd line
    out.push(<p key={i}>{inline(para.join('\n'), term)}</p>)
  }
  return <div className="md">{out}</div>
}

export function Metric({
  icon,
  title,
  value,
  hint,
}: {
  icon: ReactNode
  title: string
  value: ReactNode
  hint?: ReactNode
}) {
  return (
    <div className="trace-metric">
      <span>
        {icon}
        {title}
      </span>
      <strong>{value}</strong>
      {hint && <small>{hint}</small>}
    </div>
  )
}
