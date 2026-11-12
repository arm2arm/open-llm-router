import { Fragment, useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Activity, RefreshCw, Server, ShieldCheck, Users } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  api,
  type AdminUser,
  type AdminUserDetail,
  type DailyTotal,
  type GroupTotals,
  type Replica,
  type RequestRecord,
} from '@/lib/api'

const number = (value: number) => new Intl.NumberFormat().format(value)
const shortDay = (day: string) =>
  new Date(`${day}T00:00:00`).toLocaleDateString(undefined, {
    month: 'short',
    day: 'numeric',
  })
const timestamp = (value: string) =>
  new Date(value).toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })

function Failure({ error }: { error: Error | null }) {
  return error ? (
    <div className="error" role="alert">
      {error.message}
    </div>
  ) : null
}

// One series (tokens per day), so the title names it and no legend is needed.
// Bars are anchored to the baseline with rounded tops and a 2px gap; hover or
// focus shows the day's values above the plot. The table below is the
// accessible and exact view of the same data.
export function DailyChart() {
  const query = useQuery({
    queryKey: ['daily'],
    queryFn: () => api<{ timezone: string; days: DailyTotal[] }>('usage/daily'),
    refetchInterval: 60_000,
  })
  const [active, setActive] = useState<number | null>(null)
  const [showTable, setShowTable] = useState(false)
  const days = query.data?.days ?? []
  const tokens = days.map((d) => d.input_tokens + d.output_tokens)
  const peak = Math.max(1, ...tokens)
  const width = 600
  const height = 160
  const gap = 2
  const bar = days.length ? width / days.length - gap : 0
  const shown = active ?? days.length - 1
  const current = days[shown]
  return (
    <section className="panel" aria-label="Tokens per day">
      <div className="panel-head">
        <div>
          <h2 className="panel-title">Tokens per day · last 30 days</h2>
          <p className="subtitle" aria-live="polite">
            {current
              ? `${shortDay(current.day)}: ${number(current.input_tokens + current.output_tokens)} tokens in ${number(current.requests)} request${current.requests === 1 ? '' : 's'}`
              : 'Loading…'}
          </p>
        </div>
        <Button
          size="sm"
          variant="ghost"
          aria-pressed={showTable}
          onClick={() => setShowTable(!showTable)}
        >
          {showTable ? 'Show chart' : 'Show table'}
        </Button>
      </div>
      <Failure error={query.error} />
      {!showTable && days.length > 0 && (
        <div className="px-6 pt-5 pb-6">
          <svg
            className="daily-chart"
            viewBox={`0 0 ${width} ${height + 18}`}
            role="img"
            aria-label={`Tokens per day, peak ${number(peak)}`}
            onMouseLeave={() => setActive(null)}
          >
            <line
              x1={0}
              x2={width}
              y1={height + 0.5}
              y2={height + 0.5}
              className="chart-axis"
            />
            {days.map((d, i) => {
              const h = tokens[i] ? Math.max(2, (tokens[i] / peak) * height) : 0
              const x = i * (bar + gap) + gap / 2
              return (
                <g
                  key={d.day}
                  tabIndex={0}
                  role="button"
                  aria-label={`${shortDay(d.day)}: ${number(tokens[i])} tokens`}
                  onMouseEnter={() => setActive(i)}
                  onFocus={() => setActive(i)}
                  onBlur={() => setActive(null)}
                >
                  {/* The hit target spans the full column, larger than the bar. */}
                  <rect
                    x={x}
                    y={0}
                    width={bar}
                    height={height}
                    fill="transparent"
                  />
                  {h > 0 && (
                    <path
                      className={i === shown ? 'chart-bar active' : 'chart-bar'}
                      d={`M${x},${height} v${-(h - Math.min(4, h))} q0,${-Math.min(4, h)} ${Math.min(4, bar / 2)},${-Math.min(4, h)} h${bar - 2 * Math.min(4, bar / 2)} q${Math.min(4, bar / 2)},0 ${Math.min(4, bar / 2)},${Math.min(4, h)} v${h - Math.min(4, h)} z`}
                    />
                  )}
                </g>
              )
            })}
            {[0, days.length - 1].map((i) => (
              <text
                key={i}
                x={i === 0 ? 0 : width}
                y={height + 14}
                textAnchor={i === 0 ? 'start' : 'end'}
                className="chart-label"
              >
                {shortDay(days[i].day)}
              </text>
            ))}
          </svg>
        </div>
      )}
      {showTable && (
        <div className="table-wrap">
          <table>
            <caption className="sr-only">Daily totals, last 30 days</caption>
            <thead>
              <tr>
                <th>Day</th>
                <th>Requests</th>
                <th>Input tokens</th>
                <th>Output tokens</th>
              </tr>
            </thead>
            <tbody>
              {[...days].reverse().map((d) => (
                <tr key={d.day}>
                  <td>{shortDay(d.day)}</td>
                  <td>{number(d.requests)}</td>
                  <td>{number(d.input_tokens)}</td>
                  <td>{number(d.output_tokens)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}

export function RequestsPage() {
  const query = useQuery({
    queryKey: ['requests'],
    queryFn: () =>
      api<{ requests: RequestRecord[]; limit: number }>('requests'),
  })
  const requests = query.data?.requests ?? []
  return (
    <>
      <div className="page-head">
        <div>
          <div className="eyebrow">Metadata only</div>
          <h1>My requests</h1>
          <p className="subtitle">
            Your last {query.data?.limit ?? 200} requests. Prompts and responses
            are never stored.
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
      <Failure error={query.error} />
      <section className="panel" aria-label="Recent requests">
        {query.isPending ? (
          <p role="status" className="empty">
            Loading requests…
          </p>
        ) : requests.length === 0 ? (
          <p className="empty">
            No requests recorded. Records you delete on the Usage page are gone
            from here too.
          </p>
        ) : (
          <div className="table-wrap">
            <table data-testid="requests-table">
              <caption className="sr-only">Recent requests</caption>
              <thead>
                <tr>
                  <th>Time</th>
                  <th>Model</th>
                  <th>Status</th>
                  <th>Input</th>
                  <th>Output</th>
                  <th>First token</th>
                  <th>Total</th>
                  <th>Key</th>
                </tr>
              </thead>
              <tbody>
                {requests.map((r, i) => (
                  <tr key={`${r.time}-${i}`}>
                    <td className="whitespace-nowrap">{timestamp(r.time)}</td>
                    <td className="font-medium">
                      {r.model || '—'}
                      <span className="text-xs text-muted-foreground ml-2">
                        {r.protocol}
                        {r.stream ? ' · stream' : ''}
                      </span>
                    </td>
                    <td>
                      <span
                        className={`badge ${r.status.startsWith('2') ? '' : 'inactive'}`}
                      >
                        {r.status}
                      </span>
                    </td>
                    <td>{number(r.input_tokens)}</td>
                    <td>{number(r.output_tokens)}</td>
                    <td>
                      {r.ttfb_ms === null ? '—' : `${number(r.ttfb_ms)} ms`}
                    </td>
                    <td>{number(r.latency_ms)} ms</td>
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
      </section>
    </>
  )
}

const limitLabels: Record<string, string> = {
  tokens_per_day: 'Tokens per day',
  tokens_per_month: 'Tokens per month',
  rpm: 'Requests per minute',
  tpm: 'Tokens per minute',
  max_concurrent: 'Concurrent (interactive)',
  max_concurrent_batch: 'Concurrent (batch)',
  strict_quota: 'Strict quota (1 on, 0 off)',
}

function UserDetail({ id, self }: { id: string; self: string }) {
  const cache = useQueryClient()
  const query = useQuery({
    queryKey: ['admin-user', id],
    queryFn: () => api<AdminUserDetail>(`admin/users/${id}`),
  })
  const [field, setField] = useState('tokens_per_day')
  const [value, setValue] = useState('')
  const [reason, setReason] = useState('')
  const refresh = async () => {
    await cache.invalidateQueries({ queryKey: ['admin-user', id] })
    await cache.invalidateQueries({ queryKey: ['admin-users'] })
  }
  const save = useMutation({
    mutationFn: () =>
      api(`admin/users/${id}/overrides/${field}`, 'PUT', {
        value: value.trim() === '' ? null : Number(value),
        reason,
      }),
    onSuccess: async () => {
      setValue('')
      setReason('')
      await refresh()
    },
  })
  const clear = useMutation({
    mutationFn: (name: string) =>
      api(`admin/users/${id}/overrides/${name}`, 'DELETE'),
    onSuccess: refresh,
  })
  const enable = useMutation({
    mutationFn: (enabled: boolean) =>
      api(`admin/users/${id}/enabled`, 'PUT', { enabled }),
    onSuccess: refresh,
  })
  const data = query.data
  function submit(event: FormEvent) {
    event.preventDefault()
    save.mutate()
  }
  if (!data) return <Failure error={query.error} />
  return (
    <div className="p-6 grid gap-4 border-t">
      <div className="flex flex-wrap justify-between items-center gap-3">
        <div>
          <div className="font-semibold">{data.user.username}</div>
          <div className="text-xs text-muted-foreground">
            {data.user.email || 'no email'} ·{' '}
            {data.user.groups.join(', ') || 'no groups'} ·{' '}
            {data.keys.filter((k) => !k.revoked).length} keys
          </div>
        </div>
        {data.user.protected_admin && (
          <span className="text-xs text-muted-foreground">
            Protected local administrator
          </span>
        )}
        {id !== self && !data.user.protected_admin && (
          <Button
            variant="outline"
            size="sm"
            disabled={enable.isPending}
            onClick={() => enable.mutate(!data.enabled)}
          >
            {data.enabled ? 'Disable user' : 'Enable user'}
          </Button>
        )}
      </div>
      <Failure error={enable.error ?? clear.error} />
      <div className="table-wrap">
        <table>
          <caption className="sr-only">Effective limits</caption>
          <thead>
            <tr>
              <th>Limit</th>
              <th>Effective</th>
              <th>Override</th>
              <th>
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {Object.keys(limitLabels).map((name) => {
              const effective = (
                data.limits as unknown as Record<
                  string,
                  number | boolean | null
                >
              )[name]
              const override = data.overrides.find((o) => o.field === name)
              return (
                <tr key={name}>
                  <td>{limitLabels[name]}</td>
                  <td>
                    {typeof effective === 'boolean'
                      ? effective
                        ? 'on'
                        : 'off'
                      : effective === null
                        ? 'unlimited'
                        : number(effective)}
                  </td>
                  <td className="text-xs text-muted-foreground">
                    {override
                      ? `${override.value === null ? 'unlimited' : number(override.value)} by ${override.set_by}${override.reason ? ` — ${override.reason}` : ''}`
                      : 'group policy'}
                  </td>
                  <td>
                    {override && (
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={clear.isPending}
                        onClick={() => clear.mutate(name)}
                      >
                        Clear
                      </Button>
                    )}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
      <form className="flex flex-wrap gap-3 items-end" onSubmit={submit}>
        <label className="field">
          Limit
          <select
            className="select"
            value={field}
            onChange={(e) => setField(e.target.value)}
          >
            {Object.entries(limitLabels).map(([name, label]) => (
              <option key={name} value={name}>
                {label}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          Value (empty: unlimited)
          <Input
            inputMode="numeric"
            value={value}
            onChange={(e) => setValue(e.target.value)}
          />
        </label>
        <label className="field">
          Reason
          <Input
            maxLength={200}
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
        </label>
        <Button type="submit" disabled={save.isPending}>
          Set override
        </Button>
      </form>
      <Failure error={save.error} />
    </div>
  )
}

export function AdminPage({ self }: { self: string }) {
  const [match, setMatch] = useState('')
  const [search, setSearch] = useState('')
  const [open, setOpen] = useState<string | null>(null)
  const users = useQuery({
    queryKey: ['admin-users', search],
    queryFn: () =>
      api<{ users: AdminUser[] }>(
        `admin/users?match=${encodeURIComponent(search)}`,
      ),
  })
  const groups = useQuery({
    queryKey: ['admin-groups'],
    queryFn: () => api<{ groups: GroupTotals[] }>('admin/groups'),
  })
  const replicas = useQuery({
    queryKey: ['admin-replicas'],
    queryFn: () =>
      api<{ models: { model: string; replicas: Replica[] }[] }>(
        'admin/replicas',
      ),
    refetchInterval: 10_000,
  })
  return (
    <>
      <div className="page-head">
        <div>
          <div className="eyebrow">Operators</div>
          <h1>Admin</h1>
          <p className="subtitle">
            Users, per-user limits, group totals and replica health.
          </p>
        </div>
      </div>
      <section className="panel" aria-label="Replica health">
        <div className="panel-head">
          <h2 className="panel-title">Replicas</h2>
          <Server size={20} className="text-muted-foreground" />
        </div>
        <Failure error={replicas.error} />
        <div className="table-wrap">
          <table data-testid="replicas-table">
            <caption className="sr-only">Replica health</caption>
            <thead>
              <tr>
                <th>Model</th>
                <th>Replica</th>
                <th>Health</th>
                <th>In flight</th>
                <th>Weight</th>
              </tr>
            </thead>
            <tbody>
              {replicas.data?.models.flatMap((m) =>
                m.replicas.map((r) => (
                  <tr key={`${m.model}-${r.url}`}>
                    <td className="font-medium">{m.model}</td>
                    <td>
                      <code className="text-xs">{r.url}</code>
                    </td>
                    <td>
                      <span className={`badge ${r.healthy ? '' : 'inactive'}`}>
                        {r.healthy ? '● healthy' : '✕ unavailable'}
                      </span>
                    </td>
                    <td>
                      {r.in_flight} / {r.max_inflight}
                    </td>
                    <td>{r.weight}</td>
                  </tr>
                )),
              )}
            </tbody>
          </table>
        </div>
      </section>
      <section className="panel" aria-label="Group totals">
        <div className="panel-head">
          <h2 className="panel-title">Groups</h2>
          <Activity size={20} className="text-muted-foreground" />
        </div>
        <Failure error={groups.error} />
        <div className="table-wrap">
          <table>
            <caption className="sr-only">Usage by group</caption>
            <thead>
              <tr>
                <th>Group</th>
                <th>Members</th>
                <th>Requests today</th>
                <th>Tokens today</th>
                <th>Tokens this month</th>
              </tr>
            </thead>
            <tbody>
              {groups.data?.groups.map((g) => (
                <tr key={g.group}>
                  <td className="font-medium">
                    {g.group}
                    {g.admin && (
                      <ShieldCheck
                        size={14}
                        className="inline ml-2"
                        aria-label="admin group"
                      />
                    )}
                  </td>
                  <td>{number(g.members)}</td>
                  <td>{number(g.requests_today)}</td>
                  <td>{number(g.tokens_today)}</td>
                  <td>{number(g.tokens_this_month)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
      <section className="panel" aria-label="Users">
        <div className="panel-head">
          <h2 className="panel-title">Users</h2>
          <form
            className="flex gap-2"
            onSubmit={(e) => {
              e.preventDefault()
              setSearch(match.trim())
            }}
          >
            <label className="sr-only" htmlFor="user-search">
              Find users
            </label>
            <Input
              id="user-search"
              placeholder="Name or email"
              value={match}
              onChange={(e) => setMatch(e.target.value)}
            />
            <Button type="submit" variant="outline" size="sm">
              <Users size={15} />
              Find
            </Button>
          </form>
        </div>
        <Failure error={users.error} />
        <div className="table-wrap">
          <table data-testid="users-table">
            <caption className="sr-only">Users</caption>
            <thead>
              <tr>
                <th>User</th>
                <th>Groups</th>
                <th>Keys</th>
                <th>Tokens today</th>
                <th>This month</th>
                <th>Overrides</th>
                <th>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {users.data?.users.map((u) => (
                <Fragment key={u.id}>
                  <tr>
                    <td>
                      <div className="font-medium">{u.username}</div>
                      <div className="text-xs text-muted-foreground">
                        {u.email}
                        {!u.enabled && ' · disabled'}
                      </div>
                    </td>
                    <td>{u.groups.join(', ')}</td>
                    <td>{u.active_keys}</td>
                    <td>{number(u.tokens_today)}</td>
                    <td>{number(u.tokens_this_month)}</td>
                    <td>{Object.keys(u.overrides).length || '—'}</td>
                    <td>
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-expanded={open === u.id}
                        onClick={() => setOpen(open === u.id ? null : u.id)}
                      >
                        {open === u.id ? 'Close' : 'Manage'}
                      </Button>
                    </td>
                  </tr>
                  {open === u.id && (
                    <tr>
                      <td colSpan={7} className="p-0">
                        <UserDetail id={u.id} self={self} />
                      </td>
                    </tr>
                  )}
                </Fragment>
              ))}
            </tbody>
          </table>
        </div>
      </section>
    </>
  )
}
