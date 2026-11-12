import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'

// Metadata-only events from GET /portal/api/requests/events.
export type LiveEvent = {
  type: 'started' | 'progress' | 'finished' | 'trace_ready'
  request_id: string
  time?: string
  model?: string
  operation?: string
  protocol?: string
  stream?: boolean
  elapsed_ms?: number
  bytes?: number
  writes?: number
  status?: string
}
export type Running = {
  request_id: string
  started: number // client clock, for a smooth elapsed timer
  time: string
  model?: string
  operation?: string
  protocol?: string
  bytes: number
  writes: number
}
export type LiveStatus =
  'connecting' | 'live' | 'reconnecting' | 'unavailable' | 'paused'

// Running requests older than this are assumed finished on another path
// (for example a finished event dropped for a slow connection).
const stale = 30 * 60_000

export function useLive(enabled: boolean) {
  const client = useQueryClient()
  const [status, setStatus] = useState<LiveStatus>(
    enabled ? 'connecting' : 'paused',
  )
  const [running, setRunning] = useState<Map<string, Running>>(new Map())
  const [updated, setUpdated] = useState<number | null>(null)
  const refresh = useRef<ReturnType<typeof setTimeout> | null>(null)
  // Bumped to open a new stream after the server ended or refused one.
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    if (!enabled) {
      // eslint-disable-next-line react-hooks/set-state-in-effect -- reflect the toggle
      setStatus('paused')
      setRunning(new Map())
      return
    }
    setStatus('connecting')
    const source = new EventSource('/portal/api/requests/events', {
      withCredentials: true,
    })
    // Coalesce list refreshes when many requests finish at once.
    const scheduleRefresh = () => {
      if (refresh.current) return
      refresh.current = setTimeout(() => {
        refresh.current = null
        void client.invalidateQueries({ queryKey: ['requests'] })
      }, 300)
    }
    source.addEventListener('ready', () => {
      setStatus('live')
      // Missed events while disconnected: start from the server's list.
      setRunning(new Map())
      scheduleRefresh()
    })
    let retry: ReturnType<typeof setTimeout> | undefined
    const reopen = (delay: number) => {
      source.close()
      retry = setTimeout(() => setAttempt((a) => a + 1), delay)
    }
    source.addEventListener('end', () => {
      setStatus('reconnecting')
      reopen(3000)
      // A signed-out session redirects through the normal 401 handling.
      void client.invalidateQueries()
    })
    source.onerror = () => {
      // The browser retries dropped streams itself, but not refused ones
      // (401, 429, 503): those are retried slowly while polling continues.
      if (source.readyState === EventSource.CLOSED) {
        setStatus('unavailable')
        reopen(15_000)
      } else setStatus('reconnecting')
    }
    source.onmessage = (message) => {
      let e: LiveEvent
      try {
        e = JSON.parse(message.data) as LiveEvent
      } catch {
        return
      }
      setUpdated(Date.now())
      if (e.type === 'started' || e.type === 'progress') {
        setRunning((prev) => {
          const next = new Map(prev)
          const old = next.get(e.request_id)
          next.set(e.request_id, {
            request_id: e.request_id,
            started: old?.started ?? Date.now() - (e.elapsed_ms ?? 0),
            time: old?.time ?? e.time ?? new Date().toISOString(),
            model: e.model || old?.model,
            operation: e.operation || old?.operation,
            protocol: e.protocol || old?.protocol,
            bytes: e.bytes ?? old?.bytes ?? 0,
            writes: e.writes ?? old?.writes ?? 0,
          })
          for (const [id, r] of next)
            if (Date.now() - r.started > stale) next.delete(id)
          return next
        })
      } else if (e.type === 'finished') {
        setRunning((prev) => {
          if (!prev.has(e.request_id)) return prev
          const next = new Map(prev)
          next.delete(e.request_id)
          return next
        })
        scheduleRefresh()
      } else if (e.type === 'trace_ready') {
        scheduleRefresh()
        // An open, still-pending trace view loads as soon as content exists.
        void client.invalidateQueries({
          queryKey: ['request-trace', e.request_id],
        })
      }
    }
    return () => {
      source.close()
      clearTimeout(retry)
      if (refresh.current) clearTimeout(refresh.current)
      refresh.current = null
    }
  }, [enabled, client, attempt])

  return { status, running, updated }
}

// A ticking clock for elapsed timers, only while something is running.
export function useNow(active: boolean, interval = 250) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!active) return
    const id = setInterval(() => setNow(Date.now()), interval)
    return () => clearInterval(id)
  }, [active, interval])
  return now
}
