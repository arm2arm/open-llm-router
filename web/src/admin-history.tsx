import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { api } from '@/lib/api'

type HistoryEntry = {
  id: number
  time: string
  action: string
  actor: { type: string; id?: string; name?: string; host?: string }
  target_type?: string
  target_id?: string
  revision?: number
  result: string
  reason?: string
  details?: Record<string, unknown>
}
type HistoryPage = { events: HistoryEntry[]; next_before?: number }

export function AdministrationHistory() {
  const [filters, setFilters] = useState({
    action: '',
    actor: '',
    target: '',
    result: '',
    from: '',
    to: '',
  })
  const [query, setQuery] = useState('limit=50')
  const [cursors, setCursors] = useState<number[]>([0])
  const before = cursors[cursors.length - 1]
  const history = useQuery({
    queryKey: ['admin-history', query, before],
    queryFn: () => api<HistoryPage>(`admin/history?${query}&before=${before}`),
  })
  return (
    <section className="panel" aria-label="Administration history">
      <div className="panel-head">
        <h2 className="panel-title">Administration history</h2>
        <Button variant="outline" onClick={() => void history.refetch()}>
          Refresh history
        </Button>
      </div>
      <p className="subtitle">
        Append-only account, credential and configuration events. Filters match
        exact actions or stable actor/target IDs. Passwords, tokens and content
        are excluded. Dates use RFC3339, for example 2026-11-13T00:00:00Z.
      </p>
      <form
        className="grid gap-3 sm:grid-cols-3"
        aria-label="Search administration history"
        onSubmit={(e) => {
          e.preventDefault()
          setQuery(new URLSearchParams({ ...filters, limit: '50' }).toString())
          setCursors([0])
        }}
      >
        {(['action', 'actor', 'target', 'from', 'to'] as const).map((field) => (
          <label key={field} className="grid gap-1 text-sm">
            {field}
            <Input
              aria-label={`History ${field}`}
              value={filters[field]}
              onChange={(e) =>
                setFilters({ ...filters, [field]: e.target.value })
              }
            />
          </label>
        ))}
        <label className="grid gap-1 text-sm">
          Result
          <select
            className="select"
            aria-label="History result"
            value={filters.result}
            onChange={(e) => setFilters({ ...filters, result: e.target.value })}
          >
            <option value="">All results</option>
            {['succeeded', 'refused', 'failed'].map((result) => (
              <option key={result}>{result}</option>
            ))}
          </select>
        </label>
        <Button type="submit">Search history</Button>
      </form>
      {history.error && (
        <p className="error" role="alert">
          {history.error.message}
        </p>
      )}
      {history.isPending ? (
        <p>Loading history…</p>
      ) : (
        <>
          <div className="table-wrap">
            <table aria-label="Administration events">
              <thead>
                <tr>
                  <th>When</th>
                  <th>Action / result</th>
                  <th>Actor</th>
                  <th>Target / revision</th>
                  <th>Reason / metadata</th>
                </tr>
              </thead>
              <tbody>
                {history.data?.events.map((event) => (
                  <tr key={event.id}>
                    <td>
                      {new Date(event.time).toLocaleString()}
                      <br />#{event.id}
                    </td>
                    <td>
                      {event.action}
                      <br />
                      {event.result}
                    </td>
                    <td>
                      {event.actor.type}
                      <br />
                      {event.actor.id ?? event.actor.name}
                      {event.actor.host && (
                        <>
                          <br />
                          {event.actor.host}
                        </>
                      )}
                    </td>
                    <td>
                      {event.target_type}: {event.target_id}
                      <br />
                      {event.revision ? `Revision ${event.revision}` : ''}
                    </td>
                    <td>
                      {event.reason}
                      <pre className="text-xs whitespace-pre-wrap">
                        {event.details
                          ? JSON.stringify(event.details, null, 2)
                          : ''}
                      </pre>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {history.data?.events.length === 0 && (
            <p>No matching administration events.</p>
          )}
          <div className="flex gap-2">
            <Button
              variant="outline"
              disabled={cursors.length === 1}
              onClick={() => setCursors(cursors.slice(0, -1))}
            >
              Newer events
            </Button>
            <Button
              variant="outline"
              disabled={!history.data?.next_before}
              onClick={() =>
                setCursors([...cursors, history.data!.next_before!])
              }
            >
              Older events
            </Button>
          </div>
        </>
      )}
    </section>
  )
}
