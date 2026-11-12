import { useEffect, useRef, useState, type FormEvent } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ShieldCheck } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  api,
  redirect,
  type AuthenticationDocument,
  type AuthenticationView,
  type CheckResult,
  type SecretState,
  type SettingsRevision,
} from '@/lib/api'

const queryKey = ['admin-authentication']

const settingLabels: Record<string, string> = {
  enabled: 'Single sign-on',
  default_sign_in: 'Primary sign-in option',
  issuer: 'Issuer URL',
  backchannel_url: 'Back-channel URL',
  portal_client_id: 'Portal client ID',
  portal_client_secret: 'Portal client secret',
  audience: 'API audience',
  groups_claim: 'Groups claim',
  group_names: 'Group names',
  username_claim: 'Username claim',
  required_claim: 'Required claim',
  sync_client_id: 'Sync client ID',
  sync_client_secret: 'Sync client secret',
  sync_interval_s: 'Sync interval (seconds)',
}
const checkLabels: Record<string, string> = {
  discovery: 'Discovery',
  logout_metadata: 'Logout metadata',
  portal_credentials: 'Portal client credentials',
  service_token_audience: 'Service-token audience',
  sync_permissions: 'Sync permissions',
  test_sign_in: 'Test sign-in',
}
const secretEnv: Record<string, string> = {
  portal_client_secret: 'OIDC_PORTAL_CLIENT_SECRET',
  sync_client_secret: 'OIDC_SYNC_CLIENT_SECRET',
}

function Failure({ error }: { error: Error | null }) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (error) ref.current?.focus()
  }, [error])
  return error ? (
    <div
      ref={ref}
      role="alert"
      tabIndex={-1}
      className="error whitespace-pre-wrap"
    >
      {error.message}
    </div>
  ) : null
}
// Mirrors the server rule: enabling SSO needs passed checks and a test
// sign-in; turning it off does not.
function readyToActivate(revision: SettingsRevision) {
  if (!revision.document?.enabled) return true
  const checks = revision.checks
  return (
    !!checks?.connection &&
    checks.connection.results.every((result) => result.status !== 'failed') &&
    checks.test_sign_in?.status === 'passed'
  )
}

const asError = (error: unknown) =>
  error instanceof Error
    ? error
    : new Error('Authentication settings could not be updated.')

function describeValue(value: unknown): string {
  if (value === null || value === undefined || value === '') return '—'
  if (typeof value === 'boolean') return value ? 'on' : 'off'
  if (typeof value === 'object') {
    const claim = value as { name?: string; value?: string }
    return `${claim.name} = ${claim.value}`
  }
  return String(value)
}

function describeSecret(secret?: SecretState): string {
  if (!secret?.configured) return 'not configured'
  const where =
    secret.reference === 'environment'
      ? `environment variable ${secret.env}`
      : `stored encrypted${secret.created_at ? ` (${new Date(secret.created_at).toLocaleString()})` : ''}`
  return `${where}${secret.available ? '' : ' — unavailable'}`
}

function Status({ status }: { status: CheckResult['status'] }) {
  return (
    <span
      className={`badge ${status === 'failed' ? 'inactive' : ''}`}
      data-status={status}
    >
      {status === 'passed'
        ? '✓ passed'
        : status === 'failed'
          ? '✕ failed'
          : '– skipped'}
    </span>
  )
}

function Results({ results }: { results: CheckResult[] }) {
  return (
    <ul className="grid gap-2" aria-label="Check results">
      {results.map((result) => (
        <li key={result.name} className="flex flex-wrap gap-2 items-baseline">
          <Status status={result.status} />
          <strong>{checkLabels[result.name] ?? result.name}</strong>
          <span className="subtitle">{result.message}</span>
        </li>
      ))}
    </ul>
  )
}

type SecretMode = 'keep' | 'environment' | 'store' | 'none'

function SecretField({
  name,
  state,
  disabled,
  encryption,
}: {
  name: string
  state?: SecretState
  disabled: boolean
  encryption: boolean
}) {
  const [mode, setMode] = useState<SecretMode>('keep')
  return (
    <fieldset className="grid gap-2 rounded-md border p-3" disabled={disabled}>
      <legend className="px-1 text-sm">{settingLabels[name]}</legend>
      <p className="subtitle">Current: {describeSecret(state)}</p>
      <select
        name={`${name}.mode`}
        aria-label={`${settingLabels[name]} source`}
        value={mode}
        onChange={(event) => setMode(event.target.value as SecretMode)}
        className="select"
      >
        <option value="keep">Keep the current secret</option>
        <option value="environment">
          Use environment variable {secretEnv[name]}
        </option>
        <option value="store" disabled={!encryption}>
          Store a new secret
          {encryption ? '' : ' (needs OLR_SETTINGS_ENCRYPTION_KEY)'}
        </option>
        <option value="none">No secret</option>
      </select>
      {mode === 'store' && (
        <Input
          name={`${name}.value`}
          type="password"
          autoComplete="off"
          required
          maxLength={4096}
          aria-label={`New ${settingLabels[name].toLowerCase()}`}
        />
      )}
    </fieldset>
  )
}

function DraftEditor({
  view,
  base,
  onChanged,
}: {
  view: AuthenticationView
  base: SettingsRevision
  onChanged: () => Promise<void>
}) {
  const document = base.document as AuthenticationDocument
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<Error | null>(null)
  const secretState = (name: string) =>
    base.secrets?.find((secret) => secret.setting === name)
  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const fields = new FormData(event.currentTarget)
    const text = (key: string) => String(fields.get(key) ?? '').trim()
    const secrets: Record<string, string> = {}
    const next: AuthenticationDocument = {
      ...document,
      enabled: fields.has('enabled'),
      default_sign_in:
        text('default_sign_in') === 'local' ? 'local' : 'keycloak',
      issuer: text('issuer'),
      backchannel_url: text('backchannel_url'),
      portal_client_id: text('portal_client_id'),
      audience: text('audience'),
      groups_claim: text('groups_claim'),
      group_names: text('group_names') === 'path' ? 'path' : 'leaf',
      username_claim: text('username_claim'),
      required_claim:
        text('required_claim.name') || text('required_claim.value')
          ? {
              name: text('required_claim.name'),
              value: text('required_claim.value'),
            }
          : null,
      sync_client_id: text('sync_client_id'),
      sync_interval_s: Number(fields.get('sync_interval_s')),
    }
    for (const name of [
      'portal_client_secret',
      'sync_client_secret',
    ] as const) {
      const mode = fields.get(`${name}.mode`) as SecretMode
      if (mode === 'environment') next[name] = { env: secretEnv[name] }
      if (mode === 'none') next[name] = {}
      if (mode === 'store')
        secrets[name] = String(fields.get(`${name}.value`) ?? '')
    }
    setPending(true)
    setError(null)
    try {
      await api('admin/settings/authentication/draft', 'PUT', {
        version: view.settings.version,
        document: next,
        secrets,
      })
      await onChanged()
    } catch (error) {
      setError(asError(error))
    } finally {
      setPending(false)
    }
  }
  const disabled = pending || !view.can_change
  const field = (
    name: keyof AuthenticationDocument,
    hint?: string,
    type = 'text',
  ) => (
    <label className="grid gap-2 text-sm">
      {settingLabels[name]}
      <Input
        name={name}
        type={type}
        defaultValue={String(document[name] ?? '')}
        aria-label={settingLabels[name]}
        maxLength={2048}
        disabled={disabled}
      />
      {hint && <span className="subtitle">{hint}</span>}
    </label>
  )
  return (
    <form
      onSubmit={(event) => void save(event)}
      aria-label="Edit authentication draft"
      className="grid gap-4"
      key={`${base.id}-${view.settings.version}`}
    >
      <h3 className="panel-title">
        {view.settings.draft ? `Draft revision ${base.id}` : 'New draft'}
      </h3>
      <p className="subtitle">
        Based on{' '}
        {base.status === 'draft'
          ? `draft ${base.id}`
          : `active revision ${base.id}`}
        . Saving creates a new draft; it does not change how people sign in.
      </p>
      <Failure error={error} />
      <label className="flex gap-2 items-center text-sm">
        <input
          type="checkbox"
          name="enabled"
          defaultChecked={document.enabled}
          disabled={disabled}
          className="h-4 w-4"
        />
        Enable single sign-on (local sign-in always stays available)
      </label>
      <label className="grid gap-2 text-sm">
        {settingLabels.default_sign_in}
        <select
          name="default_sign_in"
          defaultValue={document.default_sign_in}
          disabled={disabled}
          className="select"
          aria-label={settingLabels.default_sign_in}
        >
          <option value="keycloak">Keycloak</option>
          <option value="local">Local account</option>
        </select>
      </label>
      <div className="grid gap-3 sm:grid-cols-2">
        {field(
          'issuer',
          'The public issuer, e.g. https://login.example/realms/staff',
        )}
        {field(
          'backchannel_url',
          'Optional internal URL the router uses to reach the issuer',
        )}
        {field('portal_client_id')}
        {field(
          'audience',
          'Empty: identity-provider tokens are not accepted on /v1',
        )}
        {field('groups_claim')}
        <label className="grid gap-2 text-sm">
          {settingLabels.group_names}
          <select
            name="group_names"
            defaultValue={document.group_names}
            disabled={disabled}
            className="select"
            aria-label={settingLabels.group_names}
          >
            <option value="leaf">Leaf names (staff)</option>
            <option value="path">Full paths (/org/staff)</option>
          </select>
          <span className="subtitle">
            Applies to sign-in, API tokens and group sync. Leaf names reached
            through two paths are refused; use full paths for such realms.
          </span>
        </label>
        {field('username_claim')}
        <label className="grid gap-2 text-sm">
          Required claim name
          <Input
            name="required_claim.name"
            defaultValue={document.required_claim?.name ?? ''}
            aria-label="Required claim name"
            maxLength={256}
            disabled={disabled}
          />
        </label>
        <label className="grid gap-2 text-sm">
          Required claim value
          <Input
            name="required_claim.value"
            defaultValue={document.required_claim?.value ?? ''}
            aria-label="Required claim value"
            maxLength={2048}
            disabled={disabled}
          />
        </label>
        {field('sync_client_id')}
        <label className="grid gap-2 text-sm">
          {settingLabels.sync_interval_s}
          <Input
            name="sync_interval_s"
            type="number"
            min={60}
            max={86400}
            defaultValue={document.sync_interval_s}
            aria-label={settingLabels.sync_interval_s}
            disabled={disabled}
          />
        </label>
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <SecretField
          name="portal_client_secret"
          state={secretState('portal_client_secret')}
          disabled={disabled}
          encryption={view.encryption_key_configured}
        />
        <SecretField
          name="sync_client_secret"
          state={secretState('sync_client_secret')}
          disabled={disabled}
          encryption={view.encryption_key_configured}
        />
      </div>
      <div className="flex flex-wrap gap-2">
        <Button type="submit" disabled={disabled}>
          {pending ? 'Saving…' : 'Save draft'}
        </Button>
      </div>
    </form>
  )
}

function ChecksPanel({
  view,
  target,
  onChanged,
}: {
  view: AuthenticationView
  target?: SettingsRevision
  onChanged: () => Promise<void>
}) {
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<Error | null>(null)
  const [fresh, setFresh] = useState<CheckResult[] | null>(null)
  const revision = target?.id ?? 0
  const stored = target?.checks
  const results = fresh ?? stored?.connection?.results ?? null
  async function run(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const fields = new FormData(event.currentTarget)
    setPending(true)
    setError(null)
    try {
      const response = await api<{ results: CheckResult[] }>(
        'admin/settings/authentication/checks',
        'POST',
        {
          revision,
          test_client_id: String(fields.get('test_client_id') ?? '').trim(),
          test_client_secret: String(fields.get('test_client_secret') ?? ''),
        },
      )
      setFresh(response.results)
      await onChanged()
    } catch (error) {
      setError(asError(error))
    } finally {
      setPending(false)
    }
  }
  async function testSignIn() {
    setPending(true)
    setError(null)
    try {
      const { url } = await api<{ url: string }>(
        'admin/settings/authentication/test-sign-in',
        'POST',
        { revision },
      )
      redirect(url)
    } catch (error) {
      setError(asError(error))
      setPending(false)
    }
  }
  const label = target
    ? `${target.status === 'draft' ? 'draft' : 'active revision'} ${target.id}`
    : 'the current file configuration'
  return (
    <section aria-label="Connection checks" className="grid gap-3">
      <h3 className="panel-title">Connection checks for {label}</h3>
      <p className="subtitle">
        Checks contact only the issuer (or its back-channel URL), never follow
        redirects and never show secrets. Optionally name a service client to
        check that its tokens carry the API audience; it is used once and not
        stored.
      </p>
      <Failure error={error} />
      <form
        onSubmit={(event) => void run(event)}
        className="grid gap-3 sm:grid-cols-3 items-end"
        aria-label="Run connection checks"
      >
        <label className="grid gap-2 text-sm">
          Test service client ID
          <Input name="test_client_id" maxLength={256} autoComplete="off" />
        </label>
        <label className="grid gap-2 text-sm">
          Test service client secret
          <Input
            name="test_client_secret"
            type="password"
            maxLength={4096}
            autoComplete="off"
          />
        </label>
        <div className="flex flex-wrap gap-2">
          <Button type="submit" disabled={pending || !view.can_change}>
            {pending ? 'Checking…' : 'Run checks'}
          </Button>
          {target && (
            <Button
              type="button"
              variant="outline"
              disabled={pending || !view.can_change}
              onClick={() => void testSignIn()}
            >
              Test sign-in
            </Button>
          )}
        </div>
      </form>
      {results && <Results results={results} />}
      {stored?.test_sign_in && (
        <div aria-label="Test sign-in result">
          <Results results={[stored.test_sign_in]} />
        </div>
      )}
    </section>
  )
}

export function AuthenticationSettingsPanel() {
  const cache = useQueryClient()
  const view = useQuery({
    queryKey,
    queryFn: () => api<AuthenticationView>('admin/settings/authentication'),
  })
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<Error | null>(null)
  const tested = new URLSearchParams(window.location.search).get(
    'authentication_test',
  )
  const reload = async () => {
    await cache.invalidateQueries({ queryKey })
  }
  async function act(path: string, body: object) {
    setPending(true)
    setError(null)
    try {
      await api(`admin/settings/authentication/${path}`, 'POST', body)
      await reload()
    } catch (error) {
      setError(asError(error))
    } finally {
      setPending(false)
    }
  }
  const data = view.data
  const managed = data?.settings.managed
  const target = data?.settings.draft ?? data?.settings.active
  return (
    <section
      className="panel"
      id="authentication"
      aria-label="Authentication settings"
    >
      <div className="panel-head">
        <h2 className="panel-title">Authentication</h2>
        <ShieldCheck size={20} className="text-muted-foreground" />
      </div>
      <Failure error={view.error ?? error} />
      {tested && (
        <p role="status" className="subtitle">
          Test sign-in{' '}
          {tested === 'passed' ? 'passed' : 'finished with a problem'}; see the
          result below.
        </p>
      )}
      {data && (
        <div className="grid gap-6">
          <dl
            className="grid gap-2 sm:grid-cols-2 text-sm"
            aria-label="Authentication status"
          >
            <div>
              <dt className="subtitle">Configuration authority</dt>
              <dd>
                {managed
                  ? `Database — active revision ${data.settings.active?.id}`
                  : 'Config file and environment'}
              </dd>
            </div>
            <div>
              <dt className="subtitle">This router is running</dt>
              <dd data-testid="authentication-runtime">
                {data.runtime.revision
                  ? `revision ${data.runtime.revision}`
                  : 'the file configuration'}
                {' · '}
                {data.runtime.sso_configured
                  ? data.runtime.sso_available
                    ? 'Keycloak reachable'
                    : 'Keycloak unavailable'
                  : 'local sign-in only'}
              </dd>
            </div>
            {data.runtime.problem && (
              <div className="sm:col-span-2 error" role="alert">
                The active revision could not be applied at startup, so single
                sign-on is off: {data.runtime.problem}
              </div>
            )}
          </dl>
          {managed &&
            data.settings.active &&
            data.runtime.revision !== data.settings.active.id && (
              <div role="note" className="grid gap-2">
                <p className="subtitle">
                  This router runs{' '}
                  {data.runtime.revision
                    ? `revision ${data.runtime.revision}`
                    : 'the file configuration'}
                  , not active revision {data.settings.active.id} (for example
                  after a CLI activation).
                </p>
                <div>
                  <Button
                    variant="outline"
                    disabled={pending || !data.can_change}
                    onClick={() => void act('apply', {})}
                  >
                    Apply active revision to this router
                  </Button>
                </div>
              </div>
            )}
          {data.other_issuers.length > 0 && (
            <div role="note" className="subtitle">
              Accounts of previous identity providers are kept separate and are
              no longer updated by group sync. Disable them under Users when
              they should lose access:
              <ul>
                {data.other_issuers.map((issuer) => (
                  <li key={issuer.issuer}>
                    <code>{issuer.issuer}</code>: {issuer.enabled_users} enabled
                    users, {issuer.active_keys} active keys
                  </li>
                ))}
              </ul>
            </div>
          )}
          <div className="grid gap-2" aria-label="Identity provider URLs">
            <h3 className="panel-title">Register these on the portal client</h3>
            <dl className="grid gap-1 text-sm">
              <div>
                <dt className="subtitle">Valid redirect URI</dt>
                <dd>
                  <code>{data.urls.redirect_uri}</code>
                </dd>
              </div>
              <div>
                <dt className="subtitle">Valid post logout redirect URI</dt>
                <dd>
                  <code>{data.urls.post_logout_redirect_uri}</code>
                </dd>
              </div>
              <div>
                <dt className="subtitle">Web origin</dt>
                <dd>
                  <code>{data.urls.web_origin}</code>
                </dd>
              </div>
            </dl>
          </div>
          <div className="table-wrap">
            <table aria-label="Effective authentication settings">
              <thead>
                <tr>
                  <th>Setting</th>
                  <th>Effective value</th>
                  <th>Source</th>
                </tr>
              </thead>
              <tbody>
                {data.settings.effective.map((setting) => (
                  <tr key={setting.name}>
                    <td>{settingLabels[setting.name] ?? setting.name}</td>
                    <td>
                      {setting.secret
                        ? describeSecret(setting.secret)
                        : describeValue(setting.value)}
                    </td>
                    <td>
                      <span className="badge">{setting.source}</span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {data.settings.conflicts.length > 0 && (
            <div role="alert" className="error">
              These file or environment values differ from the active revision
              and are ignored:
              <ul>
                {data.settings.conflicts.map((conflict) => (
                  <li key={conflict.setting}>
                    {settingLabels[conflict.setting] ?? conflict.setting}:
                    remove <code>{conflict.where}</code>
                  </li>
                ))}
              </ul>
            </div>
          )}
          {!data.can_change && (
            <p className="subtitle" role="note">
              {data.change_requirement}{' '}
              <a className="underline" href="/portal/sign-in/local">
                Sign in again locally
              </a>
            </p>
          )}
          {!managed ? (
            <div className="grid gap-2">
              <p className="subtitle">
                These settings come from the config file and environment.
                Importing copies the current values into the database without
                changing how anyone signs in; afterwards the database is their
                only source.
              </p>
              <div>
                <Button
                  disabled={pending || !data.can_change}
                  onClick={() => void act('import', {})}
                >
                  Import current settings
                </Button>
              </div>
            </div>
          ) : (
            target && (
              <DraftEditor view={data} base={target} onChanged={reload} />
            )
          )}
          {data.settings.draft && (
            <div className="grid gap-2" aria-label="Draft status">
              {data.settings.draft.validation?.valid ? (
                <p className="subtitle">
                  {readyToActivate(data.settings.draft)
                    ? `Draft ${data.settings.draft.id} passed its checks. Activating applies it to this router at once; other routers apply it when they restart.`
                    : `Draft ${data.settings.draft.id} is valid. Run the connection checks (none may fail) and a successful test sign-in before activating it.`}
                </p>
              ) : (
                <div role="alert" className="error">
                  Draft {data.settings.draft.id} cannot be activated yet:
                  <ul>
                    {data.settings.draft.validation?.problems.map((problem) => (
                      <li key={problem}>{problem}</li>
                    ))}
                  </ul>
                </div>
              )}
              <div className="flex flex-wrap gap-2">
                <Button
                  disabled={
                    pending ||
                    !data.can_change ||
                    !data.settings.draft.validation?.valid ||
                    !readyToActivate(data.settings.draft)
                  }
                  onClick={() =>
                    void act('activate', {
                      version: data.settings.version,
                      revision: data.settings.draft?.id,
                    })
                  }
                >
                  Activate draft
                </Button>
                <Button
                  variant="outline"
                  disabled={pending || !data.can_change}
                  onClick={() =>
                    void act('discard', { version: data.settings.version })
                  }
                >
                  Discard draft
                </Button>
              </div>
            </div>
          )}
          <ChecksPanel
            key={target?.id ?? 0}
            view={data}
            target={target}
            onChanged={reload}
          />
          {data.settings.history.length > 0 && (
            <div className="table-wrap">
              <table aria-label="Authentication revision history">
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
                  {data.settings.history.map((revision) => (
                    <tr key={revision.id}>
                      <td>{revision.id}</td>
                      <td>{revision.status.replace('_', ' ')}</td>
                      <td>{new Date(revision.created_at).toLocaleString()}</td>
                      <td>{revision.created_by}</td>
                      <td>
                        {revision.status === 'previously_active' && (
                          <Button
                            variant="outline"
                            size="sm"
                            disabled={pending || !data.can_change}
                            aria-label={`Roll back to revision ${revision.id}`}
                            onClick={() =>
                              void act('rollback', {
                                version: data.settings.version,
                                revision: revision.id,
                              })
                            }
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
          )}
        </div>
      )}
    </section>
  )
}
