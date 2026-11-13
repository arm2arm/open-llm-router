import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  api,
  type GroupPolicy,
  type PolicyPreview,
  type RoutingDocument,
} from '@/lib/api'

const fields = [
  'tokens_per_day',
  'tokens_per_month',
  'rpm',
  'tpm',
  'max_concurrent',
  'max_concurrent_batch',
] as const

export function GroupPolicies({
  document,
  onChange,
}: {
  document: RoutingDocument
  onChange: (doc: RoutingDocument) => void
}) {
  const [name, setName] = useState('')
  const groups = document.groups ?? {}
  const update = (name: string, policy: GroupPolicy) =>
    onChange({ ...document, groups: { ...groups, [name]: policy } })
  return (
    <fieldset
      className="grid gap-3 rounded-md border p-3"
      aria-label="Group policies"
    >
      <legend>Group policies</legend>
      <p className="subtitle">
        Model grants use canonical names; endpoint access follows each
        model&apos;s pool. These defaults apply separately to each user, not as
        shared group or provider budgets. Local memberships are edited in Users;
        SSO memberships remain managed by the identity provider.
      </p>
      <p className="subtitle">
        Across matching groups, grants and admin access are combined. Numeric
        limits use the largest value; one omitted or unlimited value makes that
        field unlimited. Batch concurrency falls back to interactive concurrency
        when omitted. Strict quota requires every matching group to enable it.
        The lowest priority wins. Stored user overrides replace their fields.
        The reserved local administrator always retains unrestricted access.
      </p>
      {Object.entries(groups)
        .sort()
        .map(([name, policy]) => (
          <fieldset
            key={name}
            className="grid gap-3 rounded-md border p-3"
            aria-label={`Group ${name}`}
          >
            <legend>{name}</legend>
            <div className="flex flex-wrap gap-3">
              {['*', ...Object.keys(document.models).sort()].map((model) => (
                <label key={model} className="flex gap-2 items-center text-sm">
                  <input
                    type="checkbox"
                    aria-label={`${name} grant ${model}`}
                    checked={policy.models.includes(model)}
                    onChange={(e) =>
                      update(name, {
                        ...policy,
                        models: e.target.checked
                          ? [...policy.models, model]
                          : policy.models.filter((m) => m !== model),
                      })
                    }
                  />
                  {model === '*' ? 'All models (*)' : model}
                </label>
              ))}
            </div>
            <div className="grid gap-3 sm:grid-cols-3">
              {fields.map((field) => (
                <label key={field} className="grid gap-1 text-sm">
                  {field.replaceAll('_', ' ')}
                  <Input
                    type="number"
                    min={1}
                    aria-label={`${name} ${field}`}
                    value={policy[field] ?? ''}
                    placeholder={
                      field === 'max_concurrent_batch'
                        ? 'Use interactive limit'
                        : 'Unlimited'
                    }
                    onChange={(e) =>
                      update(name, {
                        ...policy,
                        [field]:
                          e.target.value === '' ? null : Number(e.target.value),
                      })
                    }
                  />
                </label>
              ))}
            </div>
            <label className="text-sm flex gap-2 items-center">
              <input
                type="checkbox"
                aria-label={`${name} strict quota`}
                checked={policy.strict_quota}
                onChange={(e) =>
                  update(name, { ...policy, strict_quota: e.target.checked })
                }
              />
              Strict quota
            </label>
            <label className="text-sm flex gap-2 items-center">
              <input
                type="checkbox"
                aria-label={`${name} administrator`}
                checked={policy.admin}
                onChange={(e) =>
                  update(name, { ...policy, admin: e.target.checked })
                }
              />
              Administrator
            </label>
            <label className="grid gap-1 text-sm">
              Priority
              <Input
                type="number"
                aria-label={`${name} priority`}
                value={policy.priority}
                onChange={(e) =>
                  update(name, { ...policy, priority: Number(e.target.value) })
                }
              />
            </label>
            <Button
              type="button"
              variant="outline"
              aria-label={`Remove group ${name}`}
              onClick={() => {
                const next = { ...groups }
                delete next[name]
                onChange({ ...document, groups: next })
              }}
            >
              Remove group
            </Button>
          </fieldset>
        ))}
      <div className="flex flex-wrap gap-2 items-end">
        <label className="grid gap-1 text-sm">
          New group name
          <Input
            aria-label="New group name"
            value={name}
            maxLength={128}
            onChange={(e) => setName(e.target.value)}
          />
        </label>
        <Button
          type="button"
          variant="outline"
          disabled={!name.trim() || name.trim() in groups}
          onClick={() => {
            update(name.trim(), {
              models: [],
              strict_quota: false,
              admin: false,
              priority: 0,
            })
            setName('')
          }}
        >
          Add group
        </Button>
      </div>
    </fieldset>
  )
}

export function AccessPreview({ document }: { document: RoutingDocument }) {
  const users = useQuery({
    queryKey: ['policy-preview-users'],
    queryFn: () =>
      api<{
        users: {
          id: string
          username: string
          groups: string[]
          protected_admin: boolean
        }[]
      }>('admin/users'),
  })
  const [user, setUser] = useState('')
  const [memberships, setMemberships] = useState<string[]>([])
  const [result, setResult] = useState<{
    signature: string
    value: PolicyPreview
  } | null>(null)
  const [error, setError] = useState('')
  const [pending, setPending] = useState(false)
  const signature = JSON.stringify({ document, user, memberships })
  const preview = result?.signature === signature ? result.value : null
  return (
    <fieldset
      className="grid gap-3 rounded-md border p-3"
      aria-label="Effective access preview"
    >
      <legend>Effective access and affected users</legend>
      <label className="grid gap-1 text-sm">
        Stored user overrides
        <select
          className="select"
          aria-label="Preview user"
          value={user}
          onChange={(e) => {
            setUser(e.target.value)
            setMemberships(
              users.data?.users.find((u) => u.id === e.target.value)?.groups ??
                [],
            )
          }}
        >
          <option value="">No user overrides</option>
          {users.data?.users.map((u) => (
            <option key={u.id} value={u.id}>
              {u.username}
              {u.protected_admin ? ' (protected administrator)' : ''}
            </option>
          ))}
        </select>
      </label>
      <div className="flex flex-wrap gap-3">
        {Array.from(
          new Set([...Object.keys(document.groups ?? {}), ...memberships]),
        )
          .sort()
          .map((name) => (
            <label key={name} className="text-sm flex gap-2 items-center">
              <input
                type="checkbox"
                aria-label={`Preview membership ${name}`}
                checked={memberships.includes(name)}
                onChange={(e) =>
                  setMemberships(
                    e.target.checked
                      ? [...memberships, name]
                      : memberships.filter((g) => g !== name),
                  )
                }
              />
              {name}
            </label>
          ))}
      </div>
      <Button
        type="button"
        variant="outline"
        disabled={pending}
        onClick={async () => {
          setPending(true)
          setError('')
          try {
            const value = await api<PolicyPreview>(
              'admin/settings/routing/preview',
              'POST',
              { document, user_id: user, memberships },
            )
            setResult({ signature, value })
          } catch (e) {
            setError(e instanceof Error ? e.message : 'Preview failed')
          } finally {
            setPending(false)
          }
        }}
      >
        Preview access and affected users
      </Button>
      {(error || users.error) && (
        <p role="alert" className="error">
          {error || users.error?.message}
        </p>
      )}
      {preview && (
        <div aria-label="Policy preview results" className="grid gap-2 text-sm">
          <p>
            Models: {preview.models.join(', ') || 'None'}. Aliases:{' '}
            {Object.keys(preview.aliases).join(', ') || 'None'}.
          </p>
          <p>
            Administrator: {preview.admin ? 'Yes' : 'No'}. Priority:{' '}
            {preview.priority}.
          </p>
          {preview.protected_admin && (
            <p>
              Protected administrator: policy edits and overrides cannot
              restrict this account.
            </p>
          )}
          <dl className="grid gap-1">
            {Object.entries(preview.limits).map(([field, value]) => (
              <div key={field} className="flex gap-2">
                <dt>{field}:</dt>
                <dd>
                  {value === null ? 'Unlimited' : String(value)}
                  {field in preview.overrides ? ' (user override)' : ''}
                </dd>
              </div>
            ))}
          </dl>
          <p>Affected users: {preview.affected_users.length}</p>
          <ul>
            {preview.affected_users.map((u) => (
              <li key={u.id}>
                {u.username} ({u.id})
              </li>
            ))}
          </ul>
        </div>
      )}
    </fieldset>
  )
}
