import { useCallback, useEffect, useState, useSyncExternalStore } from 'react'
import { SignIn } from '@/auth'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import {
  ApiError,
  queryClient,
  redirect,
  request,
  type Me,
  type Session,
} from '@/lib/api'
import {
  safeNext,
  sessionEnded,
  setSessionEnded,
  watchSession,
} from '@/lib/session-state'

export function SessionRecovery() {
  const expired = useSyncExternalStore(watchSession, sessionEnded)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const resume = useCallback(
    async (automatic = false) => {
      if (busy) return
      setBusy(true)
      setError('')
      try {
        await queryClient.cancelQueries()
        const [session, me] = await Promise.all([
          request<Session>('auth/session'),
          request<Me>('me'),
        ])
        const previous = queryClient.getQueryData<Me>(['me'])
        if (session.must_change_password) {
          redirect('/portal/password')
          return
        }
        if (!previous || previous.user.id !== me.user.id) {
          redirect(
            safeNext(
              window.location.pathname +
                window.location.search +
                window.location.hash,
            ),
          )
          return
        }
        queryClient.setQueryData(['session'], session)
        queryClient.setQueryData(['me'], me)
        setSessionEnded(false)
        void queryClient.invalidateQueries()
      } catch (error) {
        if (!automatic || !(error instanceof ApiError && error.status === 401))
          setError(
            error instanceof Error
              ? error.message
              : 'Could not restore your session.',
          )
        if (!automatic) throw error
      } finally {
        setBusy(false)
      }
    },
    [busy],
  )
  useEffect(() => {
    if (!expired) return
    const focus = () => {
      void resume(true)
    }
    window.addEventListener('focus', focus)
    return () => window.removeEventListener('focus', focus)
  }, [expired, resume])
  return (
    <Dialog open={expired}>
      <DialogContent showCloseButton={false}>
        <DialogHeader>
          <DialogTitle>Session ended</DialogTitle>
          <DialogDescription>
            Your edits are kept in this tab. Sign in again with the same account
            to continue.
          </DialogDescription>
        </DialogHeader>
        <SignIn embedded onAuthenticated={() => resume()} />
        {error && (
          <p role="alert" className="error">
            {error}
          </p>
        )}
        <Button
          variant="outline"
          disabled={busy}
          onClick={() => {
            void resume().catch(() => {})
          }}
        >
          {busy ? 'Checking sign-in…' : 'I signed in in another tab'}
        </Button>
      </DialogContent>
    </Dialog>
  )
}
