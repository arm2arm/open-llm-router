import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { RefreshCw } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { api } from '@/lib/api'

type SyncResponse = {
  result: {
    checked: number
    updated: number
    disabled: number
    failed: number
    remaining: number
    skipped: boolean
  }
  problem?: string
}

export function GroupSyncButton({ enabled }: { enabled: boolean }) {
  const cache = useQueryClient()
  const [pending, setPending] = useState(false)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState(false)

  async function sync() {
    setPending(true)
    setNotice('')
    setError(false)
    try {
      const { result, problem } = await api<SyncResponse>(
        'admin/groups/sync',
        'POST',
      )
      if (result.skipped) {
        setNotice(
          'Group sync is already running or settings are being applied. Try again shortly.',
        )
      } else {
        const counts = `${result.checked} checked, ${result.updated} updated, ${result.disabled} disabled, ${result.failed} failed, ${result.remaining} remaining.`
        setNotice(
          problem ? `${problem} ${counts}` : `Group sync complete: ${counts}`,
        )
        setError(!!problem)
      }
      await Promise.all(
        ['admin-users', 'admin-user', 'admin-groups', 'me', 'models'].map(
          (name) => cache.invalidateQueries({ queryKey: [name] }),
        ),
      )
    } catch (error) {
      setNotice(
        error instanceof Error
          ? error.message
          : 'Group sync failed. Try again.',
      )
      setError(true)
    } finally {
      setPending(false)
    }
  }

  return (
    <div className="grid min-w-0 max-w-md gap-2 whitespace-normal">
      <div>
        <Button
          type="button"
          variant="outline"
          disabled={!enabled || pending}
          onClick={() => void sync()}
        >
          <RefreshCw
            aria-hidden="true"
            size={16}
            className={pending ? 'animate-spin' : ''}
          />
          {pending ? 'Syncing…' : 'Sync now'}
        </Button>
      </div>
      {!enabled && (
        <p className="subtitle">
          Configure a sync client and secret in Admin → Keycloak to enable sync.
        </p>
      )}
      {notice && (
        <p
          role={error ? 'alert' : 'status'}
          className={error ? 'error' : 'subtitle'}
        >
          {notice}
        </p>
      )}
    </div>
  )
}
