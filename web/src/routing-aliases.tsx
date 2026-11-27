import { useState } from 'react'
import { Plus, Trash2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import type { RoutingDocument } from '@/lib/api'

export function RoutingAliases({
  document,
  onChange,
}: {
  document: RoutingDocument
  onChange: (next: RoutingDocument) => void
}) {
  const [name, setName] = useState('')
  const [target, setTarget] = useState('')
  const models = Object.keys(document.models).sort()
  const alias = name.trim()
  const used =
    Object.hasOwn(document.aliases, alias) ||
    Object.hasOwn(document.models, alias)
  return (
    <div className="grid gap-4">
      <p className="subtitle">
        An alias gives clients another name for an existing route.
      </p>
      {Object.entries(document.aliases)
        .sort()
        .map(([name, model]) => (
          <div key={name} className="flex flex-wrap items-center gap-2">
            <label className="field">
              {name}
              <select
                className="select"
                aria-label={'Alias ' + name + ' target'}
                value={model}
                onChange={(e) =>
                  onChange({
                    ...document,
                    aliases: { ...document.aliases, [name]: e.target.value },
                  })
                }
              >
                {!models.includes(model) && (
                  <option value={model}>{model} (missing)</option>
                )}
                {models.map((value) => (
                  <option key={value}>{value}</option>
                ))}
              </select>
            </label>
            <Button
              type="button"
              variant="outline"
              size="sm"
              aria-label={'Remove alias ' + name}
              onClick={() => {
                const next = { ...document.aliases }
                delete next[name]
                onChange({ ...document, aliases: next })
              }}
            >
              <Trash2 />
            </Button>
          </div>
        ))}
      <div className="flex flex-wrap items-end gap-2">
        <label className="field">
          New alias
          <Input
            maxLength={128}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </label>
        <label className="field">
          For model
          <select
            className="select"
            aria-label="New alias target"
            value={target}
            onChange={(e) => setTarget(e.target.value)}
          >
            <option value="">Choose a model</option>
            {models.map((m) => (
              <option key={m}>{m}</option>
            ))}
          </select>
        </label>
        <Button
          type="button"
          variant="outline"
          disabled={!alias || used || !target}
          onClick={() => {
            onChange({
              ...document,
              aliases: { ...document.aliases, [alias]: target },
            })
            setName('')
          }}
        >
          <Plus />
          Add alias
        </Button>
      </div>
      {alias && used && (
        <p role="alert" className="error">
          This name is already used by a model or alias.
        </p>
      )}
    </div>
  )
}
