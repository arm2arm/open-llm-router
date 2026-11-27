import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowLeft, Plus } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { GroupPolicies, AccessPreview } from '@/policies'
import { EndpointSettingsPanel } from '@/endpoints'
import { routingDocument } from '@/backend-workspace'
import { RouteEditor } from '@/route-editor'
import { RoutingAliases } from '@/routing-aliases'
import {
  DraftReview,
  RevisionHistory,
  RouteList,
  sameSettings,
  type RouteScreen,
} from '@/routing-views'
import {
  api,
  type CheckResult,
  type Replica,
  type RoutingDocument,
  type RoutingRevision,
  type RoutingView,
} from '@/lib/api'

const queryKey = ['admin-routing']
type WorkingDraft = {
  base: RoutingRevision
  version: number
  document: RoutingDocument
}
type Screen = RouteScreen | { kind: 'endpoints' }

function RoutingWorkspace({
  view,
  health,
}: {
  view: RoutingView
  health: { model: string; replicas: Replica[] }[]
}) {
  const cache = useQueryClient()
  const { settings } = view
  const active = settings.active?.document ?? routingDocument(view)
  const base = settings.draft ?? settings.active
  const [working, setWorking] = useState<WorkingDraft | null>(null)
  const document = working?.document ?? base?.document ?? active
  const dirty =
    !!working && !sameSettings(working.base.document, working.document)
  const stale =
    !!working &&
    (working.version !== settings.version || working.base.id !== base?.id)
  const [screen, setScreen] = useState<Screen>({ kind: 'routes' })
  const [pending, setPending] = useState(false)
  const [error, setError] = useState('')
  const [message, setMessage] = useState('')
  const [checks, setChecks] = useState<{
    id: number
    results: CheckResult[]
  } | null>(null)
  const heading = useRef<HTMLHeadingElement>(null)
  const alert = useRef<HTMLDivElement>(null)
  const focusHeading = useRef(false)
  useEffect(() => {
    if (focusHeading.current) heading.current?.focus()
    focusHeading.current = false
  }, [screen])
  useEffect(() => {
    if (error) alert.current?.focus()
  }, [error])
  const section =
    screen.kind === 'review' && !settings.draft ? 'routes' : screen.kind

  function navigate(next: Screen) {
    if (base && !working && ['editor', 'groups', 'aliases'].includes(next.kind))
      setWorking({ base, version: settings.version, document: base.document! })
    focusHeading.current = true
    setScreen(next)
    setError('')
    setMessage('')
  }
  function editDocument(next: RoutingDocument) {
    setWorking((current) => ({
      base: current?.base ?? base!,
      version: current?.version ?? settings.version,
      document: next,
    }))
  }
  function back() {
    if (!dirty) setWorking(null)
    navigate({ kind: 'routes' })
  }
  async function reload() {
    await Promise.all(
      [
        'admin-routing',
        'admin-replicas',
        'admin-endpoints',
        'admin-groups',
        'me',
        'models',
      ].map((name) => cache.invalidateQueries({ queryKey: [name] })),
    )
  }
  async function run(task: () => Promise<void>) {
    setPending(true)
    setError('')
    setMessage('')
    try {
      await task()
    } catch (error) {
      setError(
        error instanceof Error ? error.message : 'The routing update failed.',
      )
    } finally {
      setPending(false)
    }
  }
  function save(next: RoutingDocument) {
    void run(async () => {
      const saved = await api<RoutingView>(
        'admin/settings/routing/draft',
        'PUT',
        {
          version: working?.version ?? settings.version,
          document: next,
        },
      )
      cache.setQueryData(queryKey, saved)
      setWorking(null)
      setChecks(null)
      navigate({ kind: 'review' })
      await reload()
    })
  }
  function act(
    path: 'import' | 'apply' | 'activate' | 'discard' | 'rollback',
    revision?: number,
  ) {
    void run(async () => {
      const body =
        path === 'discard'
          ? { version: settings.version }
          : revision !== undefined
            ? { version: settings.version, revision }
            : {}
      try {
        const result = await api<RoutingView>(
          'admin/settings/routing/' + path,
          'POST',
          body,
        )
        if (result?.settings) cache.setQueryData(queryKey, result)
      } catch (error) {
        if (path === 'activate') await reload()
        throw error
      }
      if (path !== 'apply') setWorking(null)
      setChecks(null)
      navigate({ kind: 'routes' })
      setMessage(
        {
          import: 'Current routing imported.',
          apply: 'Active routing applied to this router.',
          activate: 'Draft activated. New requests use the active routes.',
          discard: 'Routing draft discarded.',
          rollback: 'Previous routing revision restored.',
        }[path],
      )
      await reload()
    })
  }
  function testEndpoints() {
    const id = settings.draft!.id
    void run(async () => {
      const response = await api<{ results: CheckResult[] }>(
        'admin/settings/routing/checks',
        'POST',
        { revision: id },
      )
      setChecks({ id, results: response.results })
      await reload()
    })
  }
  const title =
    screen.kind === 'editor'
      ? screen.name === null
        ? 'Create route'
        : 'Edit route · ' + screen.name
      : {
          routes: 'Routes',
          aliases: 'Aliases',
          groups: 'Group access',
          history: 'Revision history',
          endpoints: 'Per-model endpoints',
          review: 'Review draft',
        }[section as Exclude<Screen['kind'], 'editor'>]
  const groups = settings.managed
    ? Object.fromEntries(
        Object.entries(document.groups ?? {}).map(([group, policy]) => [
          group,
          policy.models ?? [],
        ]),
      )
    : view.group_grants
  return (
    <fieldset disabled={pending} className="routing-workspace">
      <header className="panel-head">
        <div className="min-w-0">
          {section !== 'routes' && section !== 'editor' && (
            <button type="button" className="route-back" onClick={back}>
              <ArrowLeft size={15} />
              Routes
            </button>
          )}
          <h2 ref={heading} tabIndex={-1} className="routing-title">
            {title}
          </h2>
          {section === 'routes' && (
            <p className="subtitle">
              {settings.managed
                ? 'Active revision ' + settings.active?.id
                : 'File-managed routing'}{' '}
              ·{' '}
              <span data-testid="routing-runtime">
                {view.runtime.revision
                  ? 'routing revision ' + view.runtime.revision
                  : 'the file routing'}
              </span>{' '}
              running here
            </p>
          )}
        </div>
        {section === 'routes' &&
          (settings.managed ? (
            <div className="flex flex-wrap gap-2">
              {dirty ? (
                <Button onClick={() => save(document)}>
                  Save routing draft
                </Button>
              ) : (
                settings.draft && (
                  <Button onClick={() => navigate({ kind: 'review' })}>
                    Review draft
                  </Button>
                )
              )}
              <Button
                variant={dirty || settings.draft ? 'outline' : 'default'}
                onClick={() => navigate({ kind: 'editor', name: null })}
              >
                <Plus />
                Create route
              </Button>
            </div>
          ) : (
            <Button onClick={() => act('import')}>
              Import current routing
            </Button>
          ))}
      </header>
      <div className="routing-body">
        {error && (
          <div
            ref={alert}
            tabIndex={-1}
            role="alert"
            className="error whitespace-pre-wrap"
          >
            {error}
          </div>
        )}
        {message && (
          <p role="status" className="notice">
            {message}
          </p>
        )}
        {stale && (
          <div role="status" className="notice grid justify-items-start gap-2">
            <p>
              Settings changed elsewhere. Your edits are kept. Saving will check
              for conflicts.
            </p>
            <Button
              type="button"
              variant="outline"
              onClick={() => {
                setWorking(null)
                navigate({ kind: 'routes' })
              }}
            >
              Load latest and discard local edits
            </Button>
          </div>
        )}
        {section === 'routes' && (
          <>
            {!settings.managed && (
              <p className="subtitle">
                Import to create routes and manage revisions in the portal.
                Import copies the routing currently in use.
              </p>
            )}
            {settings.managed &&
              settings.active &&
              view.runtime.revision !== settings.active.id && (
                <div className="notice flex flex-wrap items-center justify-between gap-2">
                  <span>This router is running a different revision.</span>
                  <Button variant="outline" onClick={() => act('apply')}>
                    Apply active routing to this router
                  </Button>
                </div>
              )}
            {dirty && (
              <p role="status" className="notice">
                You have unsaved routing changes.
              </p>
            )}
            {!dirty && settings.draft && (
              <p className="subtitle">
                Draft {settings.draft.id} is waiting for review.
              </p>
            )}
            <RouteList
              active={active}
              document={document}
              health={health}
              grants={groups}
              edit={
                settings.managed
                  ? (name) => navigate({ kind: 'editor', name })
                  : undefined
              }
            />
            <div
              className="routing-tools"
              role="group"
              aria-label="Routing tools"
            >
              {settings.managed ? (
                <>
                  <Button
                    variant="outline"
                    onClick={() => navigate({ kind: 'aliases' })}
                  >
                    Aliases
                  </Button>
                  <Button
                    variant="outline"
                    onClick={() => navigate({ kind: 'groups' })}
                  >
                    Group access
                  </Button>
                  <Button
                    variant="outline"
                    onClick={() => navigate({ kind: 'history' })}
                  >
                    Revision history
                  </Button>
                </>
              ) : (
                <Button
                  variant="outline"
                  onClick={() => navigate({ kind: 'endpoints' })}
                >
                  Per-model endpoints
                </Button>
              )}
            </div>
          </>
        )}
        {screen.kind === 'editor' && (
          <RouteEditor
            source={screen.name}
            document={document}
            references={view.credential_references}
            advanced={!!screen.advanced}
            pending={pending}
            onSave={save}
            onCancel={back}
            onKeep={(next) => {
              editDocument(next)
              navigate({ kind: 'routes' })
            }}
          />
        )}
        {(section === 'aliases' || section === 'groups') && (
          <form
            aria-label="Edit routing draft"
            className="grid gap-4"
            onSubmit={(e) => {
              e.preventDefault()
              save(document)
            }}
          >
            {section === 'aliases' ? (
              <RoutingAliases document={document} onChange={editDocument} />
            ) : (
              <>
                <GroupPolicies document={document} onChange={editDocument} />
                <AccessPreview document={document} />
              </>
            )}
            <Button
              type="submit"
              className="justify-self-start"
              disabled={!dirty}
            >
              Save routing draft
            </Button>
          </form>
        )}
        {section === 'endpoints' && <EndpointSettingsPanel />}
        {section === 'review' && settings.draft && (
          <DraftReview
            view={view}
            draft={settings.draft}
            active={active}
            go={navigate}
            busy={pending}
            results={
              checks?.id === settings.draft.id ? checks?.results : undefined
            }
            check={testEndpoints}
            activate={() => act('activate', settings.draft!.id)}
            discard={() => act('discard')}
          />
        )}
        {section === 'history' && (
          <>
            {dirty && (
              <p className="notice">
                Save your local edits before rolling back.
              </p>
            )}
            <RevisionHistory
              history={settings.history}
              disabled={dirty || pending}
              rollback={(id) => act('rollback', id)}
            />
          </>
        )}
      </div>
    </fieldset>
  )
}

export function RoutingSettingsPanel() {
  const view = useQuery({
    queryKey,
    queryFn: () => api<RoutingView>('admin/settings/routing'),
  })
  const health = useQuery({
    queryKey: ['admin-replicas'],
    queryFn: () =>
      api<{ models: { model: string; replicas: Replica[] }[] }>(
        'admin/replicas',
      ),
    refetchInterval: 10_000,
  })
  return (
    <section className="panel" id="routing" aria-label="Routing settings">
      {view.isPending && (
        <p role="status" className="p-4">
          Loading routes…
        </p>
      )}
      {view.error && (
        <p role="alert" className="error">
          {view.error.message}
        </p>
      )}
      {view.data && (
        <RoutingWorkspace view={view.data} health={health.data?.models ?? []} />
      )}
    </section>
  )
}
