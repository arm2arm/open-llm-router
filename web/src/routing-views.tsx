import { useState } from 'react'
import { hashKey } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { Search } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import type {
  CheckResult,
  EndpointModel,
  EndpointReplica,
  Replica,
  RoutingDocument,
  RoutingRevision,
  RoutingView,
} from '@/lib/api'

export type RouteScreen =
  | { kind: 'routes' | 'aliases' | 'groups' | 'history' | 'review' }
  | { kind: 'editor'; name: string | null; advanced?: boolean }
type Navigate = (screen: RouteScreen) => void

export function sameSettings(left: unknown, right: unknown) {
  return hashKey([left]) === hashKey([right])
}

function origin(replica: EndpointReplica, doc: RoutingDocument) {
  return (
    (replica.backend && doc.backends?.[replica.backend]?.url) ||
    replica.url ||
    ''
  )
}
function destination(replica: EndpointReplica, doc: RoutingDocument) {
  return (
    (replica.backend &&
      (doc.backends?.[replica.backend]?.name || replica.backend)) ||
    replica.url ||
    'Missing endpoint'
  )
}
function destinations(model: EndpointModel, doc: RoutingDocument) {
  return (model.pool ?? []).map((replica) => destination(replica, doc))
}

export function RouteList({
  active,
  document,
  health,
  grants,
  edit,
}: {
  active: RoutingDocument
  document: RoutingDocument
  health: { model: string; replicas: Replica[] }[]
  grants: Record<string, string[]>
  edit?: (name: string) => void
}) {
  const [search, setSearch] = useState('')
  const names = [
    ...new Set([
      ...Object.keys(active.models),
      ...Object.keys(document.models),
    ]),
  ].sort()
  const term = search.trim().toLowerCase()
  const rows = names.flatMap((name) => {
    const current = Object.hasOwn(active.models, name)
      ? active.models[name]
      : undefined
    const next = Object.hasOwn(document.models, name)
      ? document.models[name]
      : undefined
    const model = next ?? current!
    const pool = model.pool ?? []
    const doc = next ? document : active
    const targets = destinations(model, doc)
    const aliases = Object.keys(doc.aliases ?? {}).filter(
      (alias) => doc.aliases[alias] === name,
    )
    const groups = Object.entries(grants)
      .filter(([, models]) => models.includes(name) || models.includes('*'))
      .map(([group]) => group)
    if (
      ![
        name,
        model.upstream_model,
        ...targets,
        ...aliases,
        ...groups,
        ...pool.map((r) => origin(r, doc)),
      ].some((text) => text?.toLowerCase().includes(term))
    )
      return []
    const live = health.find((entry) => entry.model === name)
    const states = pool.map((replica) =>
      live?.replicas.find(
        (r) =>
          r.url.replace(/\/$/, '') === origin(replica, doc).replace(/\/$/, ''),
      ),
    )
    const status = !current
      ? 'Draft addition'
      : !next
        ? 'Pending removal'
        : !sameSettings(current, next)
          ? 'Draft changes'
          : ''
    return [
      <tr key={name}>
        <td>
          <strong>{name}</strong>
          {status && <div className="subtitle">{status}</div>}
          {model.upstream_model && model.upstream_model !== name && (
            <div className="subtitle">{model.upstream_model}</div>
          )}
          {aliases.length > 0 && (
            <div className="subtitle">Aliases · {aliases.join(', ')}</div>
          )}
        </td>
        <td>
          <details className="route-destinations">
            <summary>
              {targets[0] ?? 'No endpoints'}
              {targets.length > 1 && ' + ' + (targets.length - 1)}
            </summary>
            <ul>
              {pool.map((replica, index) => (
                <li key={index}>
                  {origin(replica, doc)} ·{' '}
                  {states[index]
                    ? (states[index]!.healthy ? 'healthy' : 'unavailable') +
                      ' · ' +
                      states[index]!.in_flight +
                      '/' +
                      states[index]!.max_inflight +
                      ' in flight'
                    : 'no live health data'}
                  {replica.bearer_token_env &&
                    ' · credential ' + replica.bearer_token_env}
                </li>
              ))}
            </ul>
            <p>
              {(model.protocols ?? []).join(' + ')} · Granted by{' '}
              {groups.join(', ') || 'no groups'}
            </p>
          </details>
          <div className="subtitle">
            {!current
              ? 'Awaiting activation'
              : states.some(Boolean)
                ? states.filter((state) => state?.healthy).length +
                  ' of ' +
                  states.length +
                  ' healthy'
                : 'No live health data'}
          </div>
        </td>
        {edit && (
          <td>
            {next && (
              <Button
                type="button"
                size="sm"
                variant="outline"
                aria-label={'Edit route ' + name}
                onClick={() => edit(name)}
              >
                Edit
              </Button>
            )}
          </td>
        )}
      </tr>,
    ]
  })
  return (
    <div className="grid min-w-0 gap-3">
      {!!names.length && (
        <label className="route-search">
          <Search size={16} aria-hidden="true" />
          <Input
            aria-label="Find a route"
            placeholder="Find a model, alias, or backend"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
        </label>
      )}
      {rows.length ? (
        <table className="route-list" aria-label="Routed models">
          <thead>
            <tr>
              <th>Model clients use</th>
              <th>Backend and health</th>
              {edit && (
                <th className="route-edit-column">
                  <span className="sr-only">Actions</span>
                </th>
              )}
            </tr>
          </thead>
          <tbody>{rows}</tbody>
        </table>
      ) : (
        <p className="subtitle">
          {names.length ? 'No routes match your search.' : 'No routes yet.'}
        </p>
      )}
      {!names.length && (
        <Link to="/admin" className="justify-self-start underline">
          Connect a backend to discover models
        </Link>
      )}
    </div>
  )
}

const collections = {
  models: 'route',
  aliases: 'alias',
  groups: 'group',
  backends: 'backend',
  backend_presets: 'backend preset',
} as const
function changedFields(before: object, after: object) {
  const fields = new Map(Object.entries(before ?? {}))
  const values = new Map(Object.entries(after ?? {}))
  return [...new Set([...fields.keys(), ...values.keys()])].filter(
    (key) => !sameSettings(fields.get(key), values.get(key)),
  )
}
const fieldLabels: Record<string, string> = {
  upstream_model: 'upstream model',
  protocols: 'protocols',
  timeout_s: 'header timeout',
  idle_timeout_s: 'idle timeout',
  queue_timeout_s: 'queue timeout',
  pool: 'replica settings',
  session_affinity: 'session affinity',
  context_length: 'context length',
  type: 'model type',
  models: 'model grants',
  admin: 'administrator access',
  api_key: 'API key reference',
}

function changes(active: RoutingDocument, draft: RoutingDocument) {
  return (Object.keys(collections) as (keyof typeof collections)[]).flatMap(
    (key) => {
      const before = active[key] ?? {}
      const after = draft[key] ?? {}
      const names = [
        ...new Set([...Object.keys(before), ...Object.keys(after)]),
      ].sort()
      return names
        .filter((name) => !sameSettings(before[name], after[name]))
        .map((name) => {
          const existed = Object.hasOwn(before, name)
          const exists = Object.hasOwn(after, name)
          const verb = !existed ? 'Add' : !exists ? 'Remove' : 'Update'
          const details: string[] = []
          if (key === 'models' && exists) {
            const route = draft.models[name]
            details.push(
              (route.upstream_model || name) +
                ' → ' +
                destinations(route, draft).join(', '),
            )
          } else if (key === 'aliases' && exists) {
            details.push(
              (existed ? active.aliases[name] + ' → ' : '→ ') +
                draft.aliases[name],
            )
          }
          if (existed && exists && key !== 'aliases') {
            details.push(
              'Changed ' +
                changedFields(before[name] as object, after[name] as object)
                  .map(
                    (field) => fieldLabels[field] ?? field.replaceAll('_', ' '),
                  )
                  .join(', '),
            )
          }
          return {
            key,
            name,
            title: verb + ' ' + collections[key] + ' ' + name,
            detail: details.join(' · '),
          }
        })
    },
  )
}

function problemTarget(
  problem: string,
  doc: RoutingDocument,
): RouteScreen | undefined {
  const names = Object.keys(doc.models).sort((a, b) => b.length - a.length)
  const model = names.find((name) => {
    const prefix = 'models.' + name
    return (
      [':', '.', '['].some((separator) =>
        problem.startsWith(prefix + separator),
      ) || problem === prefix
    )
  })
  if (model) return { kind: 'editor', name: model, advanced: true }
  const backend = Object.keys(doc.backends ?? {}).find((id) =>
    problem.startsWith('backends.' + id + ':'),
  )
  const referencing =
    backend &&
    names.find((name) =>
      doc.models[name].pool?.some((r) => r.backend === backend),
    )
  if (referencing) return { kind: 'editor', name: referencing, advanced: true }
  if (/^aliases[.:]/.test(problem)) return { kind: 'aliases' }
  if (/^groups[.:]/.test(problem)) return { kind: 'groups' }
  if (/^models:/.test(problem)) return { kind: 'routes' }
}

export function DraftReview({
  view,
  active,
  draft,
  results,
  busy,
  go,
  check,
  activate,
  discard,
}: {
  view: RoutingView
  active: RoutingDocument
  draft: RoutingRevision
  results?: CheckResult[]
  busy: boolean
  go: Navigate
  check: () => void
  activate: () => void
  discard: () => void
}) {
  const doc = draft.document!
  const difference = changes(active, doc)
  const valid = draft.validation?.valid
  const checks = results ?? draft.checks?.connection?.results
  return (
    <div className="grid gap-4" role="group" aria-label="Routing draft status">
      <p>
        Draft {draft.id} {valid ? 'is valid.' : 'needs attention.'} Saving a
        draft keeps the active routes serving traffic.
      </p>
      <div>
        <h3 className="panel-title">Changes</h3>
        {difference.length ? (
          <ul className="route-changes">
            {difference.map(({ key, name, title, detail }) => (
              <li key={key + ':' + name}>
                <strong>{title}</strong>
                {detail && <span className="subtitle">{detail}</span>}
              </li>
            ))}
          </ul>
        ) : (
          <p className="subtitle">
            This draft matches the active configuration.
          </p>
        )}
      </div>
      {!valid && (
        <div role="alert" className="error">
          <p>Fix these problems before activation.</p>
          <ul>
            {draft.validation?.problems.map((problem) => {
              const target = problemTarget(problem, doc)
              return (
                <li key={problem}>
                  {problem}
                  {target && (
                    <button
                      type="button"
                      className="ml-2 underline"
                      onClick={() => go(target)}
                    >
                      {target.kind === 'editor'
                        ? 'Edit route ' + target.name
                        : target.kind === 'aliases'
                          ? 'Edit aliases'
                          : target.kind === 'groups'
                            ? 'Edit group access'
                            : 'View routes'}
                    </button>
                  )}
                </li>
              )
            })}
          </ul>
        </div>
      )}
      {view.impact_available ? (
        <div role="group" aria-label="Draft affected users">
          {view.affected_users.length ? (
            <details>
              <summary className="cursor-pointer font-medium">
                Affected users ({view.affected_users.length})
              </summary>
              <ul>
                {view.affected_users.map((user) => (
                  <li key={user.id}>
                    {user.username} ({user.id})
                  </li>
                ))}
              </ul>
            </details>
          ) : (
            <p>Affected users · 0</p>
          )}
        </div>
      ) : (
        valid && (
          <p role="status" className="notice">
            Waiting for the affected-user preview before activation.
          </p>
        )
      )}
      <p className="subtitle">
        Activation checks the changed endpoints before switching traffic.
      </p>
      <div className="flex flex-wrap gap-2">
        <Button
          type="button"
          disabled={busy || !valid || !view.impact_available}
          onClick={activate}
        >
          Activate routing draft
        </Button>
        <Button type="button" variant="outline" disabled={busy} onClick={check}>
          Test changed endpoints
        </Button>
        <Button type="button" variant="ghost" disabled={busy} onClick={discard}>
          Discard routing draft
        </Button>
      </div>
      {checks && (
        <ul className="grid gap-2" aria-label="Endpoint check results">
          {!checks.length && <li>No changed endpoints to test.</li>}
          {checks.map((result, i) => (
            <li className="flex flex-wrap items-baseline gap-2" key={i}>
              <span
                className={
                  'badge ' + (result.status === 'failed' ? 'inactive' : '')
                }
              >
                {result.status}
              </span>
              <code>{result.name}</code>
              <span className="subtitle">{result.message}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

export function RevisionHistory({
  history,
  disabled,
  rollback,
}: {
  history: RoutingRevision[]
  disabled: boolean
  rollback: (id: number) => void
}) {
  return history.length ? (
    <div className="table-wrap">
      <table aria-label="Routing revision history">
        <thead>
          <tr>
            <th>Revision</th>
            <th>Status</th>
            <th>Created</th>
            <th>By</th>
            <th>
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {history.map((rev) => (
            <tr key={rev.id}>
              <td>{rev.id}</td>
              <td>{rev.status.replace('_', ' ')}</td>
              <td>{new Date(rev.created_at).toLocaleString()}</td>
              <td>{rev.created_by}</td>
              <td>
                {rev.status === 'previously_active' && (
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={disabled}
                    aria-label={'Roll back routing to revision ' + rev.id}
                    onClick={() => rollback(rev.id)}
                  >
                    Roll back
                  </Button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  ) : (
    <p className="subtitle">No routing revisions yet.</p>
  )
}
