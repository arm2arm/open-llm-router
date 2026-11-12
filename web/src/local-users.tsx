import { useEffect, useRef, useState, type FormEvent } from 'react'
import { useMutation } from '@tanstack/react-query'
import { Plus, KeyRound, Copy, Check } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import {
  api,
  type AdminUserDetail,
  type GroupTotals,
  type Key,
} from '@/lib/api'

function Failure({ error }: { error: Error | null }) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (error) ref.current?.focus()
  }, [error])
  return error ? (
    <div ref={ref} role="alert" tabIndex={-1} className="error">
      {error.message}
    </div>
  ) : null
}
const failure = (error: unknown) =>
  error instanceof Error
    ? error
    : new Error('The request failed. Please try again.')
type Groups = Pick<GroupTotals, 'group' | 'admin'>[]

function GroupPicker({
  groups,
  selected = [],
  disabled,
}: {
  groups: Groups
  selected?: string[]
  disabled: boolean
}) {
  return (
    <fieldset
      disabled={disabled}
      className="rounded-md border p-3 grid gap-2 max-h-64 overflow-y-auto"
    >
      <legend className="px-1 text-sm font-medium">Existing groups</legend>
      {groups.map((g) => (
        <label key={g.group} className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            name="group"
            value={g.group}
            defaultChecked={selected.includes(g.group)}
            className="h-4 w-4 accent-primary"
          />
          {g.group}
          {g.admin ? ' (administrator)' : ''}
        </label>
      ))}
      <p className="subtitle">
        An empty selection grants no models. Administrator groups grant Admin
        access.
      </p>
    </fieldset>
  )
}

export function CreateLocalUser({
  groups,
  ready,
  onCreated,
}: {
  groups: Groups
  ready: boolean
  onCreated: (id: string) => Promise<void>
}) {
  const [open, setOpen] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<Error | null>(null)
  async function create(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = event.currentTarget
    const fields = new FormData(form)
    setPending(true)
    setError(null)
    try {
      const result = await api<{ id: string }>('admin/users', 'POST', {
        username: fields.get('username'),
        email: fields.get('email'),
        groups: fields.getAll('group'),
        password: fields.get('password'),
      })
      form.reset()
      setOpen(false)
      await onCreated(result.id)
    } catch (error) {
      setError(failure(error))
    } finally {
      ;(form.elements.namedItem('password') as HTMLInputElement).value = ''
      setPending(false)
    }
  }
  return (
    <>
      <Button
        disabled={!ready}
        onClick={() => {
          setError(null)
          setOpen(true)
        }}
      >
        <Plus />
        Create local user
      </Button>
      <Dialog
        open={open}
        onOpenChange={(value) => {
          if (!pending) setOpen(value)
        }}
      >
        <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>Create local user</DialogTitle>
            <DialogDescription>
              The user must replace the temporary password at first login. Their
              identity stays separate from any Keycloak account with the same
              name or email.
            </DialogDescription>
          </DialogHeader>
          <form
            onSubmit={(event) => void create(event)}
            className="grid gap-4"
            aria-label="Create local user"
          >
            <Failure error={error} />
            <label className="field">
              Username
              <Input
                name="username"
                autoComplete="off"
                required
                maxLength={128}
                disabled={pending}
              />
            </label>
            <label className="field">
              Email (optional)
              <Input
                name="email"
                type="email"
                autoComplete="off"
                maxLength={254}
                disabled={pending}
              />
            </label>
            <label className="field">
              Temporary password
              <Input
                name="password"
                type="password"
                autoComplete="new-password"
                required
                minLength={15}
                maxLength={1024}
                disabled={pending}
              />
            </label>
            <GroupPicker groups={groups} disabled={pending} />
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                disabled={pending}
                onClick={() => setOpen(false)}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={pending}>
                {pending ? 'Creating…' : 'Create user'}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </>
  )
}

export function LocalMemberships({
  id,
  selected,
  groups,
  onSaved,
}: {
  id: string
  selected: string[]
  groups: Groups
  onSaved: () => Promise<void>
}) {
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<Error | null>(null)
  const [saved, setSaved] = useState(false)
  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const fields = new FormData(event.currentTarget)
    setPending(true)
    setError(null)
    setSaved(false)
    try {
      await api(`admin/users/${id}/groups`, 'PUT', {
        groups: fields.getAll('group'),
      })
      await onSaved()
      setSaved(true)
    } catch (error) {
      setError(failure(error))
    } finally {
      setPending(false)
    }
  }
  return (
    <form
      key={selected.join('\u0000')}
      onSubmit={(event) => void save(event)}
      className="grid gap-3"
      aria-label="Local group memberships"
    >
      <h3 className="font-semibold">Local memberships</h3>
      <GroupPicker groups={groups} selected={selected} disabled={pending} />
      {selected.some(
        (name) => !groups.some((group) => group.group === name),
      ) && (
        <p className="subtitle">
          Some stored memberships are absent from the current configuration.
          Saving removes those memberships.
        </p>
      )}
      <p className="subtitle">
        Saving signs out this user's browser sessions. Their existing keys use
        the updated model access and limits on the next request.
      </p>
      <Failure error={error} />
      {saved && (
        <p role="status">
          Memberships saved. Effective access is updated below.
        </p>
      )}
      <Button
        type="submit"
        variant="outline"
        disabled={pending}
        className="justify-self-start"
      >
        {pending ? 'Saving…' : 'Save memberships'}
      </Button>
    </form>
  )
}

export function ResetLocalPassword({
  id,
  hasPassword,
  onSaved,
}: {
  id: string
  hasPassword: boolean
  onSaved: () => Promise<void>
}) {
  const [open, setOpen] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<Error | null>(null)
  const [saved, setSaved] = useState(false)
  async function reset(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = event.currentTarget
    const fields = new FormData(form)
    if (fields.get('password') !== fields.get('confirm')) {
      setError(new Error('The temporary passwords do not match.'))
      return
    }
    setPending(true)
    setError(null)
    try {
      await api(`admin/users/${id}/password`, 'POST', {
        password: fields.get('password'),
      })
      form.reset()
      setOpen(false)
      setSaved(true)
      await onSaved()
    } catch (error) {
      setError(failure(error))
    } finally {
      form.reset()
      setPending(false)
    }
  }
  return (
    <div>
      <Button
        variant="outline"
        size="sm"
        onClick={() => {
          setError(null)
          setSaved(false)
          setOpen(true)
        }}
      >
        <KeyRound />
        {hasPassword ? 'Reset local password' : 'Set local password'}
      </Button>
      {saved && (
        <p role="status" className="subtitle">
          Temporary password set. All sessions ended and all API keys were
          revoked. The user must change the password before accessing the
          portal.
        </p>
      )}
      <Dialog
        open={open}
        onOpenChange={(value) => {
          if (!pending) setOpen(value)
        }}
      >
        <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>
              {hasPassword ? 'Reset local password' : 'Set local password'}
            </DialogTitle>
            <DialogDescription>
              This ends all of the user's sessions and permanently revokes their
              API keys. Give them the temporary password through a private
              channel; they must replace it at their next login.
            </DialogDescription>
          </DialogHeader>
          <form
            onSubmit={(event) => void reset(event)}
            className="grid gap-4"
            aria-label="Reset local password"
          >
            <Failure error={error} />
            <label className="field">
              Temporary password
              <Input
                name="password"
                type="password"
                autoComplete="new-password"
                required
                minLength={15}
                maxLength={1024}
                disabled={pending}
              />
            </label>
            <label className="field">
              Confirm temporary password
              <Input
                name="confirm"
                type="password"
                autoComplete="new-password"
                required
                maxLength={1024}
                disabled={pending}
              />
            </label>
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                disabled={pending}
                onClick={() => setOpen(false)}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={pending}>
                {pending ? 'Resetting…' : 'Set password and revoke keys'}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  )
}

const time = (value: string | null) =>
  value ? new Date(value).toLocaleString() : 'Never'
export function AdminUserKeys({
  id,
  keys,
  onSaved,
}: {
  id: string
  keys: Key[]
  onSaved: () => Promise<void>
}) {
  const [selected, setSelected] = useState<Key | null>(null)
  const [copying, setCopying] = useState<string | null>(null)
  const [copied, setCopied] = useState<string | null>(null)
  const [copyError, setCopyError] = useState<Error | null>(null)
  async function copyKey(key: Key) {
    setCopying(key.id)
    setCopied(null)
    setCopyError(null)
    try {
      // Start the clipboard write during the click gesture (Safari requires it).
      // The raw key stays out of React state, query caches and browser storage.
      const value = api<{ key: string }>(
        `admin/users/${id}/keys/${key.id}/copy`,
        'POST',
      )
      // A denied clipboard permission may reject before the HTTP request does.
      void value.catch(() => {})
      if (typeof ClipboardItem !== 'undefined' && navigator.clipboard?.write) {
        const content = value.then(
          ({ key }) => new Blob([key], { type: 'text/plain' }),
        )
        void content.catch(() => {})
        await navigator.clipboard.write([
          new ClipboardItem({
            'text/plain': content,
          }),
        ])
      } else {
        const result = await value
        if (!navigator.clipboard?.writeText)
          throw new Error('Clipboard access requires HTTPS or localhost.')
        await navigator.clipboard.writeText(result.key)
      }
      setCopied(key.id)
    } catch (error) {
      setCopyError(
        error instanceof Error ? error : new Error('Could not copy the key.'),
      )
    } finally {
      setCopying(null)
    }
  }
  const revoke = useMutation({
    mutationFn: (key: string) => api(`admin/users/${id}/keys/${key}`, 'DELETE'),
    onSuccess: onSaved,
  })
  return (
    <section aria-label="User API keys">
      <h3 className="font-semibold mb-3">API keys</h3>
      <Failure error={revoke.error} />
      <Failure error={copyError} />
      <p className="subtitle mb-3">
        Copy is available for keys issued with encryption enabled. Older keys
        need replacement; they continue to work until revoked.
      </p>
      {copied && (
        <p role="status" className="subtitle">
          API key copied to clipboard.
        </p>
      )}
      <div className="table-wrap">
        <table>
          <caption className="sr-only">User key metadata</caption>
          <thead>
            <tr>
              <th>Key / label</th>
              <th>Class</th>
              <th>Created / last used</th>
              <th>Expires</th>
              <th>Status</th>
              <th>
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {keys.map((key) => {
              const expired = key.expired
              return (
                <tr key={key.id}>
                  <td>
                    <code>{key.prefix}…</code>
                    <div>{key.label}</div>
                  </td>
                  <td>
                    {key.class}
                    {key.trusted_forwarder && ' · trusted forwarder'}
                  </td>
                  <td>
                    {time(key.created)}
                    <div className="subtitle">{time(key.last_used)}</div>
                  </td>
                  <td>{time(key.expires)}</td>
                  <td>
                    {key.revoked ? 'Revoked' : expired ? 'Expired' : 'Active'}
                    {!key.revoked && !expired && (
                      <Button
                        variant="ghost"
                        size="sm"
                        className="ml-2"
                        disabled={!key.copy_available || copying !== null}
                        title={
                          key.copy_available
                            ? 'Copy API key'
                            : 'Key is not recoverable with this server’s encryption key. Issue a replacement.'
                        }
                        aria-label={`Copy key ${key.label || key.prefix}`}
                        onClick={() => void copyKey(key)}
                      >
                        {copied === key.id ? (
                          <Check aria-hidden="true" />
                        ) : (
                          <Copy aria-hidden="true" />
                        )}
                        {copying === key.id ? 'Copying…' : 'Copy key'}
                      </Button>
                    )}
                  </td>
                  <td>
                    {!key.revoked && !expired && (
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={revoke.isPending}
                        onClick={() => setSelected(key)}
                        aria-label={`Revoke ${key.label || key.prefix}`}
                      >
                        Revoke
                      </Button>
                    )}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
      {keys.length === 0 && <p className="subtitle">No API keys.</p>}
      <AlertDialog
        open={!!selected}
        onOpenChange={(value) => {
          if (!value) setSelected(null)
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Revoke this user's key?</AlertDialogTitle>
            <AlertDialogDescription>
              {selected?.label || selected?.prefix} will stop authorizing new
              API requests. Its metadata and usage history will remain
              available.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep key</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (selected) revoke.mutate(selected.id)
              }}
            >
              Revoke key
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  )
}

export function LocalUserControls({
  id,
  data,
  groups,
  onSaved,
}: {
  id: string
  data: AdminUserDetail
  groups: Groups | undefined
  onSaved: () => Promise<void>
}) {
  if (data.user.issuer !== 'local')
    return (
      <p className="subtitle">
        Keycloak controls this user's password and memberships. Authoritative
        issuer: <span className="break-all">{data.user.issuer}</span>.
      </p>
    )
  if (data.user.protected_admin)
    return (
      <p className="subtitle">
        Protected local administrator. Change your own password under Account.
      </p>
    )
  return (
    <div className="grid gap-4">
      {groups ? (
        <LocalMemberships
          id={id}
          selected={data.user.groups}
          groups={groups}
          onSaved={onSaved}
        />
      ) : (
        <p role="status">Loading configured groups…</p>
      )}
      <ResetLocalPassword
        id={id}
        hasPassword={data.user.has_local_password}
        onSaved={onSaved}
      />
    </div>
  )
}
