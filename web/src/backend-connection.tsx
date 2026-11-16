import { useEffect, useRef, useState } from 'react'
import { RefreshCw } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from '@/components/ui/dialog'
import { api, type Backend, type RoutingDocument } from '@/lib/api'

type ModelAPI = 'chat' | 'responses' | 'messages'
type Result = {
  ok: boolean
  message: string
  http_status?: number
  latency_ms: number
  input_tokens: number
  output_tokens: number
  request_id?: string
}

export function BackendConnection({
  id,
  backend,
  legacy,
  document,
  onClose,
  onSelectModels,
}: {
  id: string
  backend: Backend
  legacy?: boolean
  document: RoutingDocument
  onClose: () => void
  onSelectModels: () => void
}) {
  const routes = Object.entries(document.models).filter(([, model]) =>
    model.pool.some((replica) =>
      legacy
        ? !replica.backend && replica.url === backend.url
        : replica.backend === id,
    ),
  )
  const [model, setModel] = useState(routes[0]?.[0] ?? '')
  const [modelAPI, setModelAPI] = useState<ModelAPI>(
    routes[0]?.[1].protocols.includes('openai') ? 'chat' : 'messages',
  )
  const protocols = document.models[model]?.protocols ?? []
  const [busy, setBusy] = useState(false)
  const [catalog, setCatalog] = useState<{
    models: string[]
    error?: string
  } | null>(null)
  const [result, setResult] = useState<Result | null>(null)
  const [error, setError] = useState('')
  const request = useRef<AbortController | null>(null)
  useEffect(() => () => request.current?.abort(), [])

  async function run() {
    request.current?.abort()
    const controller = new AbortController()
    request.current = controller
    setBusy(true)
    setCatalog(null)
    setResult(null)
    setError('')
    const replica = document.models[model]?.pool.find(
      (replica) => !replica.backend && replica.url === backend.url,
    )
    const discovery = (
      legacy
        ? api<{
            models: string[]
            replicas: { error?: string }[]
          }>(
            'admin/endpoints/models',
            'POST',
            {
              model,
              replicas: [
                {
                  url: backend.url,
                  private_network: replica?.private_network,
                  bearer_token_env: replica?.bearer_token_env ?? '',
                },
              ],
            },
            controller.signal,
          ).then((value) => ({
            models: value.models,
            error: value.replicas[0]?.error,
          }))
        : api<{ models: string[]; error?: string }>(
            'admin/backends/detect',
            'POST',
            { id, backend },
            controller.signal,
          )
    )
      .then((value) => {
        if (!controller.signal.aborted) setCatalog(value)
      })
      .catch((error: Error) => {
        if (!controller.signal.aborted)
          setCatalog({ models: [], error: error.message })
      })
    const inference = model
      ? api<Result>(
          'admin/backends/test',
          'POST',
          {
            model,
            api: modelAPI,
            backend: legacy ? '' : id,
            ...(legacy ? { url: backend.url } : {}),
          },
          controller.signal,
        )
          .then((value) => {
            if (!controller.signal.aborted) setResult(value)
          })
          .catch((error: Error) => {
            if (!controller.signal.aborted) setError(error.message)
          })
      : Promise.resolve()
    await Promise.all([discovery, inference])
    if (!controller.signal.aborted) setBusy(false)
  }

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-w-xl max-h-[90vh] flex flex-col p-0 gap-0">
        <DialogHeader className="p-5 pb-0">
          <DialogTitle>Test connection</DialogTitle>
          <DialogDescription>
            Check {backend.name || id} from the OLR server. A reachable model
            catalog does not confirm that generation works.
          </DialogDescription>
        </DialogHeader>
        <div className="overflow-y-auto p-5 grid gap-4">
          <code className="backend-host">{backend.url}</code>
          {routes.length ? (
            <>
              <label className="field">
                Model route
                <select
                  className="select"
                  aria-label="Connection test model"
                  value={model}
                  disabled={busy}
                  onChange={(event) => {
                    setModel(event.target.value)
                    setModelAPI(
                      document.models[event.target.value].protocols.includes(
                        'openai',
                      )
                        ? 'chat'
                        : 'messages',
                    )
                    setResult(null)
                    setCatalog(null)
                    setError('')
                  }}
                >
                  {routes.map(([name, route]) => (
                    <option key={name} value={name}>
                      {name}
                      {route.upstream_model && route.upstream_model !== name
                        ? ` → ${route.upstream_model}`
                        : ''}
                    </option>
                  ))}
                </select>
              </label>
              <label className="field">
                API
                <select
                  className="select"
                  aria-label="Connection test API"
                  value={modelAPI}
                  disabled={busy}
                  onChange={(event) => {
                    setModelAPI(event.target.value as ModelAPI)
                    setResult(null)
                    setCatalog(null)
                    setError('')
                  }}
                >
                  {protocols.includes('openai') && (
                    <>
                      <option value="chat">OpenAI Chat Completions</option>
                      <option value="responses">OpenAI Responses</option>
                    </>
                  )}
                  {protocols.includes('anthropic') && (
                    <option value="messages">Anthropic Messages</option>
                  )}
                </select>
              </label>
              <p className="subtitle">
                Sends a small request through OLR to this backend only. Uses
                your normal limits and up to 16 output tokens. The test stops
                after 15 seconds; closing this dialog cancels it.
              </p>
            </>
          ) : (
            <p className="notice" role="status">
              No model is selected for this backend. You can check its model
              API, then select a model to enable routing and test generation.
            </p>
          )}
          {busy && <p role="status">Checking from the OLR server…</p>}
          {catalog && (
            <div role="status" className={catalog.error ? 'error' : 'notice'}>
              <strong>Model API</strong>
              <p>
                {catalog.error ||
                  `Connection OK · ${catalog.models.length} models discovered`}
              </p>
              {!!catalog.models.length && (
                <p className="break-all">{catalog.models.join(', ')}</p>
              )}
            </div>
          )}
          {error && (
            <p role="alert" className="error">
              {error}
            </p>
          )}
          {result && (
            <div role="status" className={result.ok ? 'notice' : 'error'}>
              <strong>Generation through OLR</strong>
              <p>{result.message}</p>
              <p>
                {result.http_status ? `HTTP ${result.http_status} · ` : ''}
                {result.latency_ms} ms · {result.input_tokens} input /{' '}
                {result.output_tokens} output tokens
              </p>
              {result.request_id && (
                <p className="break-all">Request ID: {result.request_id}</p>
              )}
            </div>
          )}
        </div>
        <DialogFooter className="border-t p-5">
          {!routes.length && (
            <Button variant="outline" onClick={onSelectModels}>
              Select models
            </Button>
          )}
          <Button disabled={busy} onClick={run}>
            <RefreshCw size={16} className={busy ? 'animate-spin' : ''} />
            {routes.length ? 'Run test' : 'Check model API'}
          </Button>
          <Button variant="outline" onClick={onClose}>
            {busy ? 'Cancel test' : 'Close'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
