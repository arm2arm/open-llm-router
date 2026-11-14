import { useEffect, useState, type Dispatch, type SetStateAction } from 'react'
import { Button } from '@/components/ui/button'
import { api } from '@/lib/api'

type Catalog = {
  models: string[]
  complete: boolean
  replicas: {
    url: string
    models: string[]
    provider?: string
    error?: string
  }[]
}

// Remounted when sources change, so stale replies cannot fill a new draft.
export function UpstreamModels({
  model,
  sources,
  value,
  onChoose,
}: {
  model: string
  sources: string
  value: string
  onChoose: Dispatch<SetStateAction<string>>
}) {
  const [catalog, setCatalog] = useState<Catalog | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(true)
  const [refresh, setRefresh] = useState(0)
  useEffect(() => {
    const controller = new AbortController()
    const timer = setTimeout(() => {
      async function detect() {
        setBusy(true)
        setError('')
        try {
          const result = await api<Catalog>(
            'admin/endpoints/models',
            'POST',
            {
              model,
              replicas: JSON.parse(sources),
            },
            controller.signal,
          )
          if (controller.signal.aborted) return
          setCatalog(result)
          if (result.complete && result.models.length === 1)
            onChoose((current) => current || result.models[0])
        } catch (error) {
          if (!controller.signal.aborted) {
            setCatalog(null)
            setError(
              error instanceof Error
                ? error.message
                : 'Model discovery failed.',
            )
          }
        } finally {
          if (!controller.signal.aborted) setBusy(false)
        }
      }
      void detect()
    }, 400)
    return () => {
      clearTimeout(timer)
      controller.abort()
    }
  }, [model, sources, refresh, onChoose])
  return (
    <section
      aria-label="Upstream model discovery"
      className="grid gap-2 rounded-md border p-3"
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="text-sm font-medium">
          Models detected on the backend
        </span>
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={busy}
          onClick={() => {
            setCatalog(null)
            setBusy(true)
            setRefresh((n) => n + 1)
          }}
        >
          {busy ? 'Detecting models…' : 'Refresh models'}
        </Button>
      </div>
      {catalog?.complete && catalog.models.length > 0 && (
        <label className="grid gap-2 text-sm">
          Detected upstream models
          <select
            className="select"
            aria-label="Detected upstream models"
            value={catalog.models.includes(value) ? value : ''}
            onChange={(event) => {
              if (event.target.value) onChoose(event.target.value)
            }}
          >
            <option value="">Choose a model</option>
            {catalog.models.map((name) => (
              <option key={name} value={name}>
                {name}
              </option>
            ))}
          </select>
        </label>
      )}
      <div role="status" className="subtitle">
        {busy
          ? 'Reading model catalogs…'
          : error ||
            (catalog?.complete
              ? catalog.models.length === 0
                ? 'No models are shared by all replicas. Enter a model name manually.'
                : value && !catalog.models.includes(value)
                  ? 'The current upstream name was not found on every replica. Choose a detected model or keep your manual entry.'
                  : `${catalog.models.length} model(s) available on every replica.`
              : 'Some catalogs could not be read. You can still enter a model name manually.')}
      </div>
      {catalog?.replicas.map((r) => (
        <p className="subtitle break-all" key={r.url}>
          {r.url}: {r.error || r.provider || 'Compatible model catalog'}
        </p>
      ))}
      <p className="subtitle">
        A single detected model fills an empty name. Detection does not save
        changes or prove inference capabilities.
      </p>
    </section>
  )
}
