import {
  useEffect,
  useRef,
  useState,
  type FormEvent,
  type ReactNode,
} from 'react'
import { Link, useLocation } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import {
  ArrowUpRight,
  Building2,
  ChevronDown,
  KeyRound,
  Layers3,
  ShieldCheck,
} from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { safeNext } from '@/lib/session-state'
import {
  redirect,
  request,
  type AuthMethods,
  type Me,
  type Session,
} from '@/lib/api'

export function AuthFrame({
  children,
  embedded = false,
}: {
  children: ReactNode
  embedded?: boolean
}) {
  if (embedded) return <div>{children}</div>
  return (
    <main id="main" className="auth-screen">
      <div className="auth-card">
        <div className="brand auth-brand">
          <Layers3 className="brand-mark" size={39} />
          <span>Open LLM Router</span>
        </div>
        {children}
        <Link className="auth-about-link" to="/about">
          About Open LLM Router
        </Link>
      </div>
    </main>
  )
}

function FormError({ error }: { error: string }) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (error) ref.current?.focus()
  }, [error])
  return error ? (
    <div id="auth-error" className="error" role="alert" tabIndex={-1} ref={ref}>
      {error}
    </div>
  ) : null
}

export function SignIn({
  embedded = false,
  onAuthenticated,
}: { embedded?: boolean; onAuthenticated?: () => Promise<void> } = {}) {
  const location = useLocation()
  const directLocal = location.pathname === '/sign-in/local'
  const changed =
    new URLSearchParams(location.search).get('password') === 'changed'
  const params = new URLSearchParams(location.search)
  const next = safeNext(
    embedded
      ? window.location.pathname + window.location.search + window.location.hash
      : params.get('next'),
  )
  const methods = useQuery({
    queryKey: ['auth-methods'],
    queryFn: () => request<AuthMethods>('auth'),
    refetchInterval: 10_000,
  })
  const [pending, setPending] = useState(false)
  const [error, setError] = useState('')
  const sso = methods.data?.oidc
  // Respect the site's primary method; local recovery always remains available.
  const keycloakFirst = methods.data?.default !== 'local'
  const keycloak = !directLocal && sso?.configured && (
    <section
      aria-label="Institutional sign-in"
      className={`institutional-signin ${keycloakFirst ? '' : 'institutional-signin-secondary'}`}
    >
      <div className="institutional-signin-heading">
        <span className="institutional-signin-icon">
          <Building2 aria-hidden="true" size={24} />
        </span>
        <div>
          <h2>Institutional login</h2>
          <p>Use your university or organization account.</p>
        </div>
      </div>
      <Button
        type="button"
        className="institutional-signin-button"
        variant={keycloakFirst ? 'default' : 'outline'}
        disabled={!sso.available}
        onClick={() => {
          const url = new URL(sso.login_url, window.location.origin)
          url.searchParams.set('next', next)
          if (embedded) window.open(url.href, '_blank', 'noopener,noreferrer')
          else redirect(url.href)
        }}
      >
        Continue with institutional login <ArrowUpRight aria-hidden="true" />
      </Button>
      <p className="institutional-signin-provider">
        <ShieldCheck aria-hidden="true" size={15} /> Secure sign-in with
        Keycloak
      </p>
      <p className="institutional-signin-reminder">
        Your institution may keep you signed in in this browser. On a shared
        computer, sign out of your institutional account and close the browser
        when finished.
      </p>
      {!sso.available && (
        <p role="status" className="institutional-signin-status">
          Institutional login is temporarily unavailable. Use a local account
          below.
        </p>
      )}
    </section>
  )

  async function login(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = event.currentTarget
    setPending(true)
    setError('')
    try {
      const data = new FormData(form)
      const result = await request<{ must_change_password: boolean }>(
        '/portal/login/local',
        'POST',
        { username: data.get('username'), password: data.get('password') },
      )
      form.reset()
      if (result.must_change_password) redirect('/portal/password')
      else if (onAuthenticated) await onAuthenticated()
      else redirect(next)
    } catch (error) {
      setError(
        error instanceof Error
          ? error.message
          : 'Sign-in failed. Please try again.',
      )
    } finally {
      const password = form.elements.namedItem('password') as HTMLInputElement
      password.value = ''
      setPending(false)
    }
  }

  const localForm = (
    <form
      onSubmit={(event) => void login(event)}
      aria-label="Local sign-in"
      className="grid gap-4"
    >
      {!directLocal && (
        <div>
          <p className="subtitle">For accounts created on this router.</p>
        </div>
      )}
      <FormError error={error} />
      <div className="grid gap-2">
        <label htmlFor="username">Local username</label>
        <Input
          id="username"
          name="username"
          autoComplete="username"
          required
          maxLength={256}
          disabled={pending}
          aria-invalid={!!error}
          aria-describedby={error ? 'auth-error' : undefined}
        />
      </div>
      <div className="grid gap-2">
        <label htmlFor="password">Local password</label>
        <Input
          id="password"
          name="password"
          type="password"
          autoComplete="current-password"
          required
          maxLength={1024}
          disabled={pending}
          aria-invalid={!!error}
          aria-describedby={error ? 'auth-error' : undefined}
        />
      </div>
      {methods.data && !methods.data.local.available && (
        <p role="alert" className="error">
          Local sign-in is temporarily unavailable. Contact your administrator.
        </p>
      )}
      <Button
        type="submit"
        variant={keycloakFirst && sso?.available ? 'outline' : 'default'}
        disabled={pending || !methods.data?.local.available}
      >
        <KeyRound />
        {pending ? 'Signing in…' : 'Sign in with local account'}
      </Button>
      <p className="subtitle text-sm">
        The protected local administrator is separate from a Keycloak
        administrator.
      </p>
    </form>
  )

  return (
    <AuthFrame embedded={embedded}>
      <h1>
        {directLocal
          ? 'Sign in with a local account'
          : 'Welcome to Open LLM Router'}
      </h1>
      <p className="subtitle mb-6">
        {!directLocal && keycloakFirst && sso?.configured
          ? 'Use your institutional account to access Open LLM Router.'
          : 'Access your models, API keys and usage.'}
      </p>
      {!embedded && params.get('expired') === '1' && (
        <p role="status" className="notice mb-5">
          Your session ended. Sign in again to return to your previous page.
        </p>
      )}
      {embedded && sso?.configured && (
        <p className="subtitle mb-5">
          Institutional login opens in a new tab. Return here after signing in;
          your open forms stay here.
        </p>
      )}
      {changed && (
        <p role="status" className="notice mb-5">
          Password changed. All local sessions have been signed out. Sign in
          with your new password.
        </p>
      )}
      {methods.isPending && (
        <p role="status" className="subtitle">
          Checking sign-in options…
        </p>
      )}
      {methods.error && (
        <div className="error" role="alert">
          Could not check sign-in options.{' '}
          <button className="underline" onClick={() => void methods.refetch()}>
            Try again
          </button>
        </div>
      )}
      {keycloakFirst && keycloak}
      {!methods.isPending &&
        (keycloakFirst && sso?.configured && !directLocal ? (
          <details className="local-signin">
            <summary>
              <KeyRound aria-hidden="true" size={16} />
              <span>Use a local account</span>
              <ChevronDown aria-hidden="true" size={16} />
            </summary>
            <div className="local-signin-form">{localForm}</div>
          </details>
        ) : (
          localForm
        ))}
      {!keycloakFirst && keycloak}
      {directLocal && (
        <Link className="auth-link mt-5" to="/sign-in">
          All sign-in options
        </Link>
      )}
    </AuthFrame>
  )
}

export function identityLabel(session: Session, protectedAdmin = false) {
  return session.auth_method === 'oidc'
    ? 'Keycloak account'
    : protectedAdmin
      ? 'Protected local administrator'
      : 'Local account'
}

export function PasswordForm({ required = false }: { required?: boolean }) {
  const [pending, setPending] = useState(false)
  const [error, setError] = useState('')
  async function changePassword(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = event.currentTarget
    setError('')
    const data = new FormData(form)
    const current = String(data.get('current_password'))
    const next = String(data.get('new_password'))
    const confirmation = String(data.get('confirm_password'))
    if (next !== confirmation) {
      setError('The new passwords do not match.')
      return
    }
    if (
      Array.from(next).length < 15 ||
      new TextEncoder().encode(next).length > 1024
    ) {
      setError(
        'Use at least 15 characters and at most 1024 bytes for your new password.',
      )
      return
    }
    if (next === current) {
      setError('Choose a new password that differs from your current password.')
      return
    }
    setPending(true)
    try {
      await request('/portal/password', 'POST', {
        current_password: current,
        new_password: next,
      })
      form.reset()
      redirect('/portal/sign-in/local?password=changed')
    } catch (error) {
      setError(
        error instanceof Error
          ? error.message
          : 'Password change failed. Please try again.',
      )
    } finally {
      form.reset()
      setPending(false)
    }
  }
  return (
    <section className="max-w-lg">
      {required ? (
        <h1>Change your password</h1>
      ) : (
        <h2 className="panel-title">Change your password</h2>
      )}
      <p id="password-help" className="subtitle mb-6">
        {required
          ? 'Replace your initial password before using the portal. Enter the initial password as your current password.'
          : 'Changing your password signs out all your local sessions. You will need to sign in again.'}{' '}
        Use at least 15 characters and choose a different password.
      </p>
      <form
        onSubmit={(event) => void changePassword(event)}
        aria-label="Change local password"
        className="grid gap-4"
      >
        <FormError error={error} />
        <div className="grid gap-2">
          <label htmlFor="current-password">Current password</label>
          <Input
            id="current-password"
            name="current_password"
            type="password"
            autoComplete="current-password"
            required
            maxLength={1024}
            disabled={pending}
            aria-describedby={
              error ? 'auth-error password-help' : 'password-help'
            }
            aria-invalid={!!error}
          />
        </div>
        <div className="grid gap-2">
          <label htmlFor="new-password">New password</label>
          <Input
            id="new-password"
            name="new_password"
            type="password"
            autoComplete="new-password"
            required
            maxLength={1024}
            disabled={pending}
            aria-describedby={
              error ? 'auth-error password-help' : 'password-help'
            }
            aria-invalid={!!error}
          />
        </div>
        <div className="grid gap-2">
          <label htmlFor="confirm-password">Confirm new password</label>
          <Input
            id="confirm-password"
            name="confirm_password"
            type="password"
            autoComplete="new-password"
            required
            maxLength={1024}
            disabled={pending}
            aria-describedby={error ? 'auth-error' : undefined}
            aria-invalid={!!error}
          />
        </div>
        <Button type="submit" disabled={pending}>
          {pending ? 'Changing password…' : 'Change password'}
        </Button>
      </form>
    </section>
  )
}

export function AccountPage({ me, session }: { me: Me; session: Session }) {
  return (
    <div>
      <div className="page-head">
        <div>
          <h1>Account</h1>
          <p className="subtitle">
            {me.user.username} ·{' '}
            {identityLabel(session, me.user.protected_admin)}
          </p>
        </div>
      </div>
      {session.auth_method === 'local' ? (
        <PasswordForm />
      ) : (
        <div className="panel p-6">
          <h2 className="panel-title">Password managed by Keycloak</h2>
          <p className="subtitle">
            Contact your organization to change or recover your Keycloak
            password.
          </p>
        </div>
      )}
    </div>
  )
}
