import { useEffect, useState, type FormEvent } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api, request } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

type AgentKey = {
  active: boolean
  id: string
  label: string
  prefix: string
  owner_id: string
  parent_id: string | null
  permissions: string[]
  allowed_networks: string[]
  expires: string
  revoked: boolean
  last_used: string | null
  last_ip: string | null
}
type Inventory = {
  enabled: boolean
  keys: AgentKey[]
  permissions: string[]
  source_ip: string
  peer_ip: string
  proxy_trusted: boolean
  address_error: boolean
  base_url: string
  max_lifetime_days: number
  default_lifetime_days: number
}
const observer = [
  'service:read',
  'usage:read',
  'audit:read',
  'users:read',
  'routing:read',
]
const operator = [...observer, 'users:write', 'routing:write']
const sensitive = [
  'authentication:write',
  'users:credentials',
  'admin_keys:delegate',
]
const permissionHelp: Record<string, string> = {
  'authentication:write': 'Change how administrators and users sign in.',
  'users:credentials': 'Reset user passwords and revoke inference keys.',
  'admin_keys:delegate':
    'Issue child keys and grant administrative roles through users and routing policies.',
  'admin_keys:write':
    'Revoke or narrow administration keys; issuance also needs delegation permission.',
}
export function AgentAccessPage() {
  const cache = useQueryClient()
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 60000)
    return () => window.clearInterval(timer)
  }, [])
  const inventory = useQuery({
    queryKey: ['agent-keys'],
    queryFn: () => api<Inventory>('admin/agent-keys'),
  })
  const [open, setOpen] = useState(false)
  const [step, setStep] = useState(0)
  const [label, setLabel] = useState('')
  const [permissions, setPermissions] = useState<string[]>(observer)
  const [networks, setNetworks] = useState('')
  const [days, setDays] = useState(7)
  const [secret, setSecret] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [filter, setFilter] = useState('')
  const [selected, setSelected] = useState<AgentKey | null>(null)
  const [rotation, setRotation] = useState<AgentKey | null>(null)
  const [restrict, setRestrict] = useState<AgentKey | null>(null)
  const [copied, setCopied] = useState(false)
  const data = inventory.data
  const entries = networks.split(/[\s,]+/).filter(Boolean)
  const hermes = `mcp_servers:\n  open_llm_router_admin:\n    url: "${data?.base_url ?? ''}/api/admin/v1/mcp"\n    headers:\n      Authorization: "Bearer \u0024{OLR_ADMIN_API_KEY}"\n    supports_parallel_tool_calls: false\n`
  const rest = `curl --fail '${data?.base_url ?? ''}/api/admin/v1/identity' \\\n  -H "Authorization: Bearer $OLR_ADMIN_API_KEY"`
  function start(key?: AgentKey) {
    setRotation(key ?? null)
    setStep(0)
    setSecret('')
    setError('')
    setCopied(false)
    setLabel(key ? `${key.label} replacement` : '')
    setPermissions(key?.permissions ?? observer)
    setNetworks(key?.allowed_networks.join('\n') ?? '')
    setDays(data?.default_lifetime_days ?? 7)
    setOpen(true)
  }
  async function create(event: FormEvent) {
    event.preventDefault()
    setBusy(true)
    setError('')
    try {
      const result = await request<{ key: string }>(
        'admin/agent-keys',
        'POST',
        {
          label,
          permissions,
          allowed_networks: entries,
          expires_in_days: days,
          replaces_key_id: rotation?.id,
        },
      )
      setSecret(result.key)
      setStep(4)
      await cache.invalidateQueries({ queryKey: ['agent-keys'] })
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  async function revoke() {
    if (!selected) return
    setBusy(true)
    setError('')
    try {
      await api(`admin/agent-keys/${selected.id}`, 'DELETE')
      setSelected(null)
      await cache.invalidateQueries({ queryKey: ['agent-keys'] })
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  function download(generic = false) {
    const contents = generic
      ? JSON.stringify(
          {
            mcpServers: {
              open_llm_router_admin: {
                type: 'http',
                url: `${data?.base_url ?? ''}/api/admin/v1/mcp`,
                headers: { Authorization: 'Bearer ${OLR_ADMIN_API_KEY}' },
              },
            },
          },
          null,
          2,
        )
      : hermes
    const url = URL.createObjectURL(
      new Blob([contents], {
        type: generic ? 'application/json' : 'text/yaml',
      }),
    )
    const link = document.createElement('a')
    link.href = url
    link.download = generic ? 'olr-mcp.json' : 'olr-hermes.yaml'
    link.click()
    URL.revokeObjectURL(url)
  }
  return (
    <section className="grid gap-6">
      <div>
        <h1 className="text-2xl font-semibold">Agent access</h1>
        <p className="subtitle">
          Give Hermes or another agent explicit administration permissions from
          approved IP addresses.
        </p>
      </div>
      {(inventory.error || error) && (
        <p role="alert" className="error">
          {error || inventory.error?.message}
        </p>
      )}
      {inventory.isPending && <p role="status">Loading administration keys…</p>}
      {data && (
        <>
          {!data.enabled && (
            <p role="status" className="error">
              Agent administration is disabled. An operator must set
              admin_api.enabled to true in the router configuration.
            </p>
          )}
          <div className="rounded-lg border p-4 grid gap-2">
            <h2 className="font-semibold">Connection address</h2>
            <p>
              Address seen by router:{' '}
              <code>{data.source_ip || 'Unavailable'}</code>. Socket peer:{' '}
              <code>{data.peer_ip}</code>.{' '}
              {data.proxy_trusted
                ? 'Trusted proxy chain is in use.'
                : 'Forwarding headers are ignored.'}
            </p>
            {data.address_error && (
              <p role="alert">
                Check the configured proxy chain before issuing a key.
              </p>
            )}
            <p className="text-sm text-muted-foreground">
              Your agent may use a different address. Use its stable public, VPN
              or private egress IP. Allowlist entries are never added
              automatically.
            </p>
          </div>
          <div className="flex flex-wrap gap-3">
            <Button disabled={!data.enabled} onClick={() => start()}>
              Create administration key
            </Button>
            <Input
              aria-label="Filter administration keys"
              placeholder="Filter by name, permission or address"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
            />
          </div>
          <div className="grid gap-3">
            {data.keys
              .filter((k) =>
                `${k.label} ${k.permissions.join(' ')} ${k.allowed_networks.join(' ')}`
                  .toLowerCase()
                  .includes(filter.toLowerCase()),
              )
              .map((k) => {
                const expired = new Date(k.expires).getTime() <= now
                const soon = new Date(k.expires).getTime() - now < 2 * 86400000
                return (
                  <article
                    key={k.id}
                    className="rounded-lg border p-4 grid gap-2"
                  >
                    <div className="flex flex-wrap justify-between gap-2">
                      <h2 className="font-semibold">{k.label}</h2>
                      <span>
                        {k.revoked
                          ? 'Revoked'
                          : expired
                            ? 'Expired'
                            : !k.active
                              ? 'Blocked by parent'
                              : soon
                                ? 'Expires soon'
                                : 'Active'}
                        {k.parent_id && ' · delegated'}
                      </span>
                    </div>
                    <code>{k.prefix}…</code>
                    <p className="text-sm">{k.permissions.join(', ')}</p>
                    <p>Allowed: {k.allowed_networks.join(', ')}</p>
                    <p className="text-sm text-muted-foreground">
                      Expires {new Date(k.expires).toLocaleString()}. Last used{' '}
                      {k.last_used
                        ? new Date(k.last_used).toLocaleString()
                        : 'never'}
                      {k.last_ip && ` from ${k.last_ip}`}.
                    </p>
                    {k.parent_id && (
                      <p className="text-sm">
                        Parent: {k.parent_id}. Access also depends on every
                        parent remaining valid.
                      </p>
                    )}
                    {!k.revoked && (
                      <div className="flex gap-2">
                        <Button
                          variant="outline"
                          disabled={!data.enabled}
                          onClick={() => start(k)}
                        >
                          Rotate key
                        </Button>
                        <Button
                          variant="outline"
                          onClick={() => setRestrict(k)}
                        >
                          Restrict access
                        </Button>
                        <Button
                          variant="outline"
                          onClick={() => setSelected(k)}
                        >
                          Revoke
                        </Button>
                      </div>
                    )}
                  </article>
                )
              })}
            {data.keys.length === 0 && (
              <p>
                No administration keys yet. Start with Observer access and add
                permissions only when needed.
              </p>
            )}
          </div>
          <div className="rounded-lg border p-4 grid gap-3">
            <h2 className="font-semibold">Connect Hermes</h2>
            <p>
              Put OLR_ADMIN_API_KEY in the agent profile’s secret environment.
              The downloaded configuration contains a placeholder.
            </p>
            <pre className="overflow-auto text-sm">{hermes}</pre>
            <Button variant="outline" onClick={() => download()}>
              Download Hermes configuration
            </Button>
            <Button variant="outline" onClick={() => download(true)}>
              Download generic MCP example
            </Button>
            <h3 className="font-semibold">Verify from the agent host</h3>
            <pre className="overflow-auto text-sm">{rest}</pre>
            <p>
              Check the returned source IP and permissions. Then ask the agent
              to inspect service health, preview a change, explain its effects,
              apply it and verify the result. After an interrupted write,
              inspect the change status before retrying.
            </p>
            <p className="text-sm">
              Generic MCP clients use the same URL and Authorization header.
              Configure the example’s environment reference using your client’s
              secret settings. OpenAPI: <code>/api/admin/v1/openapi.json</code>{' '}
              (administration key required).
            </p>
          </div>
        </>
      )}
      <Dialog
        open={open}
        onOpenChange={(value) => {
          if (!busy) {
            setOpen(value)
            if (!value) {
              setSecret('')
              setError('')
            }
          }
        }}
      >
        <DialogContent className="max-h-[90vh] overflow-auto">
          <DialogHeader>
            <DialogTitle>
              {step === 4
                ? 'Save your administration key'
                : `Create administration key · ${step + 1} of 4`}
            </DialogTitle>
            <DialogDescription>
              {step === 4
                ? 'This is the only time the complete key is shown.'
                : 'Changes made with this key can run autonomously within its permissions.'}
            </DialogDescription>
          </DialogHeader>
          <form onSubmit={create} className="grid gap-4">
            {step === 0 && (
              <>
                <label className="grid gap-2">
                  Agent or purpose
                  <Input
                    required
                    maxLength={100}
                    value={label}
                    onChange={(e) => setLabel(e.target.value)}
                    placeholder="Hermes operations"
                  />
                </label>
                <div className="flex gap-2">
                  <Button
                    type="button"
                    variant="outline"
                    onClick={() =>
                      setPermissions(
                        observer.filter(
                          (p) => !rotation || rotation.permissions.includes(p),
                        ),
                      )
                    }
                  >
                    Observer
                  </Button>
                  <Button
                    type="button"
                    variant="outline"
                    onClick={() =>
                      setPermissions(
                        operator.filter(
                          (p) => !rotation || rotation.permissions.includes(p),
                        ),
                      )
                    }
                  >
                    Operator
                  </Button>
                </div>
                <fieldset className="grid gap-2">
                  <legend>Custom permissions</legend>
                  {data?.permissions
                    .filter(
                      (p) => !rotation || rotation.permissions.includes(p),
                    )
                    .map((p) => (
                      <label key={p} className="text-sm flex gap-2 items-start">
                        <input
                          type="checkbox"
                          checked={permissions.includes(p)}
                          onChange={(e) =>
                            setPermissions(
                              e.target.checked
                                ? [...permissions, p]
                                : permissions.filter((v) => v !== p),
                            )
                          }
                        />
                        <span>
                          <code>{p}</code>
                          {permissionHelp[p] && (
                            <span className="block text-muted-foreground">
                              {permissionHelp[p]}
                            </span>
                          )}
                        </span>
                      </label>
                    ))}
                </fieldset>
              </>
            )}
            {step === 1 && (
              <>
                <label className="grid gap-2">
                  Approved IP addresses or CIDRs
                  <textarea
                    className="rounded border bg-background p-2"
                    rows={4}
                    required
                    value={networks}
                    onChange={(e) => setNetworks(e.target.value)}
                    placeholder={'203.0.113.8\n2001:db8::/64'}
                  />
                </label>
                <Button
                  type="button"
                  variant="outline"
                  disabled={!data?.source_ip}
                  onClick={() =>
                    setNetworks(
                      [...entries, data?.source_ip ?? '']
                        .filter(Boolean)
                        .join('\n'),
                    )
                  }
                >
                  Add my detected address
                </Button>
                <p className="text-sm">
                  Use the agent’s egress address. IPv4 and IPv6 are supported;
                  unrestricted /0 networks are rejected.
                </p>
              </>
            )}
            {step === 2 && (
              <>
                <label className="grid gap-2">
                  Expires in days
                  <Input
                    type="number"
                    min={1}
                    max={data?.max_lifetime_days ?? 90}
                    required
                    value={days}
                    onChange={(e) => setDays(Number(e.target.value))}
                  />
                </label>
                <p>
                  Rotation creates a replacement. Revoke the old key after
                  verifying the new connection.
                </p>
              </>
            )}
            {step === 3 && (
              <>
                <h3 className="font-semibold">Review access</h3>
                <p>
                  {label} · expires in {days} days
                </p>
                <p>Permissions: {permissions.join(', ')}</p>
                <p>Approved networks: {entries.join(', ')}</p>
                <p className="text-sm">
                  Issuance requires a sign-in within the last 15 minutes.
                  {permissions.some((p) => sensitive.includes(p)) &&
                    ' These sensitive permissions require a local administrator sign-in.'}
                </p>
                <a className="underline" href="/portal/sign-in/local">
                  Sign in again as local administrator
                </a>
              </>
            )}
            {step === 4 && (
              <>
                <pre
                  className="overflow-auto whitespace-pre-wrap break-all rounded border p-3"
                  aria-label="New administration key"
                >
                  {secret}
                </pre>
                <Button
                  type="button"
                  variant="outline"
                  onClick={async () => {
                    try {
                      await navigator.clipboard.writeText(secret)
                      setCopied(true)
                    } catch {
                      setError('Select and copy the key manually.')
                    }
                  }}
                >
                  {copied ? 'Copied' : 'Copy key'}
                </Button>
                <p>
                  Store this in your agent’s secret environment. Do not paste it
                  into a conversation or commit it. Key metadata stays available
                  in the inventory.
                </p>
              </>
            )}
            {error && (
              <p role="alert" className="error">
                {error}
              </p>
            )}
            <div className="flex gap-2">
              {step > 0 && step < 4 && (
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => setStep(step - 1)}
                >
                  Back
                </Button>
              )}
              {step < 3 && (
                <Button
                  type="button"
                  disabled={
                    !label.trim() ||
                    permissions.length === 0 ||
                    (step >= 1 && entries.length === 0) ||
                    (step === 2 &&
                      (!days ||
                        days < 1 ||
                        days > (data?.max_lifetime_days ?? 90)))
                  }
                  onClick={() => setStep(step + 1)}
                >
                  Continue
                </Button>
              )}
              {step === 3 && (
                <Button disabled={busy}>
                  {busy ? 'Creating…' : 'Create key'}
                </Button>
              )}
              {step === 4 && (
                <Button
                  type="button"
                  onClick={() => {
                    setOpen(false)
                    setSecret('')
                  }}
                >
                  I saved the key
                </Button>
              )}
            </div>
          </form>
        </DialogContent>
      </Dialog>
      {restrict && (
        <RestrictKey value={restrict} close={() => setRestrict(null)} />
      )}
      <Dialog
        open={!!selected}
        onOpenChange={(value) => {
          if (!busy && !value) setSelected(null)
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke {selected?.label}?</DialogTitle>
            <DialogDescription>
              The key and all its descendants will stop working on their next
              request.
            </DialogDescription>
          </DialogHeader>
          <Button disabled={busy} onClick={() => void revoke()}>
            Revoke key
          </Button>
        </DialogContent>
      </Dialog>
    </section>
  )
}

function RestrictKey({ value, close }: { value: AgentKey; close: () => void }) {
  const cache = useQueryClient()
  function localDate(iso: string) {
    const date = new Date(iso)
    return new Date(date.getTime() - date.getTimezoneOffset() * 60000)
      .toISOString()
      .slice(0, 16)
  }
  const [permissions, setPermissions] = useState(value.permissions)
  const [networks, setNetworks] = useState(value.allowed_networks.join('\n'))
  const [expires, setExpires] = useState(() => localDate(value.expires))
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  async function save(event: FormEvent) {
    event.preventDefault()
    setBusy(true)
    setError('')
    try {
      await api(`admin/agent-keys/${value.id}`, 'PUT', {
        permissions,
        allowed_networks: networks.split(/[\s,]+/).filter(Boolean),
        expires: new Date(expires).toISOString(),
      })
      await cache.invalidateQueries({ queryKey: ['agent-keys'] })
      close()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !busy) close()
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Restrict {value.label}</DialogTitle>
          <DialogDescription>
            Only reduce permissions, networks or expiration. All child keys will
            be revoked.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={save} className="grid gap-4">
          {value.permissions.map((p) => (
            <label key={p} className="flex gap-2 text-sm">
              <input
                type="checkbox"
                checked={permissions.includes(p)}
                onChange={(e) =>
                  setPermissions(
                    e.target.checked
                      ? [...permissions, p]
                      : permissions.filter((v) => v !== p),
                  )
                }
              />
              {p}
            </label>
          ))}
          <label className="grid gap-2">
            Approved networks
            <textarea
              className="rounded border bg-background p-2"
              value={networks}
              onChange={(e) => setNetworks(e.target.value)}
            />
          </label>
          <label className="grid gap-2">
            Expires at
            <Input
              type="datetime-local"
              required
              max={localDate(value.expires)}
              value={expires}
              onChange={(e) => setExpires(e.target.value)}
            />
          </label>
          {error && (
            <p role="alert" className="error">
              {error}
            </p>
          )}
          <Button disabled={busy || permissions.length === 0}>
            {busy ? 'Saving…' : 'Restrict access'}
          </Button>
        </form>
      </DialogContent>
    </Dialog>
  )
}
