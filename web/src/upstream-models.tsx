import {
  useEffect,
  useId,
  useState,
  type Dispatch,
  type SetStateAction,
} from 'react'
import { Input } from '@/components/ui/input'
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
  refresh,
}: {
  model: string
  sources: string
  value: string
  onChoose: Dispatch<SetStateAction<string>>
  refresh: number
}) {
  const list = useId()
  const [catalog, setCatalog] = useState<Catalog | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(true)
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
            { model, replicas: JSON.parse(sources) },
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
    <section aria-label="Upstream model discovery" className="grid gap-2">
      <label className="grid gap-2 text-sm">
        Model
        <Input
          name="upstream_model"
          aria-label="Upstream model name"
          list={list}
          value={value}
          onChange={(event) => onChoose(event.target.value)}
          placeholder={model}
          maxLength={256}
        />
      </label>
      <datalist id={list}>
        {catalog?.complete &&
          catalog.models.map((name) => (
            <option key={name} value={name}>
              {name}
            </option>
          ))}
      </datalist>
      <div role="status" className="subtitle">
        {busy
          ? 'Reading model catalogs…'
          : error ||
            (catalog?.complete
              ? value && !catalog.models.includes(value)
                ? 'The current upstream name was not found on every replica. Choose a detected model or keep your manual entry.'
                : `${catalog.models.length} model(s) available. Choose a model or enter its ID.`
              : 'Some catalogs could not be read. Enter the model ID manually.')}
      </div>
      {!busy &&
        catalog?.replicas.map((r) => (
          <p className="subtitle break-all" key={r.url}>
            {r.url}: {r.error || r.provider || 'Compatible model catalog'}
          </p>
        ))}
    </section>
  )
}
