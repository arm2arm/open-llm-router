import { Building2, UserRound } from 'lucide-react'
import type { IdentityProfile } from '@/lib/api'

type ProfileUser = IdentityProfile & { username: string; email: string }

export function userDisplayName(user: IdentityProfile & { username: string }) {
  return (
    user.full_name ||
    [user.given_name, user.family_name].filter(Boolean).join(' ') ||
    user.username
  )
}

export function ProfileFields({ user }: { user: ProfileUser }) {
  return (
    <dl className="grid grid-cols-1 sm:grid-cols-2 gap-x-8 gap-y-5">
      {[
        [
          'Full name',
          user.full_name ||
            [user.given_name, user.family_name].filter(Boolean).join(' '),
        ],
        ['Username', user.username],
        ['First name', user.given_name],
        ['Last name', user.family_name],
        ['Email', user.email],
        ['Institution', user.institution],
      ].map(([label, value]) => (
        <div key={label} className="min-w-0">
          <dt className="text-sm text-muted-foreground mb-1">{label}</dt>
          <dd className="font-medium break-words">
            {value || (
              <span className="font-normal text-muted-foreground">
                Not provided
              </span>
            )}
          </dd>
        </div>
      ))}
    </dl>
  )
}

export function ProfileCard({
  user,
  institutional,
}: {
  user: ProfileUser
  institutional: boolean
}) {
  return (
    <section className="panel p-6 mb-6" aria-label="Your profile">
      <div className="flex items-start gap-4 mb-6">
        <span className="rounded-2xl bg-accent p-3 text-accent-foreground">
          <UserRound aria-hidden="true" size={28} />
        </span>
        <div className="min-w-0">
          <h2 className="panel-title break-words">{userDisplayName(user)}</h2>
          {user.institution && (
            <p className="flex items-start gap-2 text-sm mt-2 text-muted-foreground">
              <Building2
                aria-hidden="true"
                size={16}
                className="shrink-0 mt-0.5"
              />
              <span className="break-words">{user.institution}</span>
            </p>
          )}
          <p className="subtitle mt-2">
            {institutional
              ? 'From your institutional account · updated when you sign in.'
              : 'Your local account details.'}
          </p>
        </div>
      </div>
      <ProfileFields user={user} />
      {institutional && (
        <p className="text-sm text-muted-foreground mt-6">
          To update these details, contact your institution, then sign out and
          sign in again. Missing details have not been shared by your
          institution.
        </p>
      )}
    </section>
  )
}
