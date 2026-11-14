import { useState, type FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { Plus, Pencil, Trash2, SlidersHorizontal } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from '@/components/ui/dialog'
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogCancel,
} from '@/components/ui/alert-dialog'
import { useBackendWorkspace } from '@/backend-workspace'
import type { BackendPreset } from '@/lib/api'

export function BackendPresets() {
  const { query, document, saving, save } = useBackendWorkspace()
  const [editing, setEditing] = useState<{
    id: string
    version: number
  } | null>(null)
  const [removing, setRemoving] = useState<string | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const pending = !!query.data?.settings.draft
  const attached = (id: string) =>
    Object.entries(document?.backends ?? {})
      .filter(([, backend]) => backend.preset === id)
      .map(([id, backend]) => backend.name || id)
  async function remove() {
    if (!document || !removing) return
    try {
      const next = structuredClone(document)
      delete next.backend_presets![removing]
      await save(next, {}, query.data?.settings.version)
      setRemoving(null)
      setNotice('Preset removed.')
    } catch (error) {
      setError((error as Error).message)
    }
  }
  return (
    <section aria-label="Backend presets">
      <div className="backend-heading">
        <div>
          <h2>Backend presets</h2>
          <p className="subtitle">
            Reusable limits shared by everyone using a backend.
          </p>
        </div>
        <Button
          disabled={!query.data || pending || saving}
          onClick={() =>
            setEditing({ id: '', version: query.data!.settings.version })
          }
        >
          <Plus size={16} /> Create preset
        </Button>
      </div>
      <p className="notice">
        Each backend gets its own budget, even when it uses the same preset.
        Requests and tokens use fixed one-minute windows. Existing streams can
        finish.
      </p>
      {pending && (
        <p className="notice">
          Review pending changes in{' '}
          <Link to="/admin/advanced">Advanced routing</Link> before editing
          presets.
        </p>
      )}
      {(error || query.error) && (
        <p className="error" role="alert">
          {error || query.error?.message}
        </p>
      )}
      {notice && (
        <p className="notice" role="status">
          {notice}
        </p>
      )}
      <div className="backend-grid">
        {Object.entries(document?.backend_presets ?? {})
          .sort(([a], [b]) =>
            a === 'unlimited' ? -1 : b === 'unlimited' ? 1 : a.localeCompare(b),
          )
          .map(([id, preset]) => (
            <article key={id} className="backend-card" aria-label={preset.name}>
              <div className="backend-card-head">
                <span className="backend-icon">
                  <SlidersHorizontal size={22} />
                </span>
                {id === 'unlimited' && (
                  <span className="backend-status">Default</span>
                )}
              </div>
              <h2>{preset.name}</h2>
              <dl className="preset-values">
                <div>
                  <dt>Requests / minute</dt>
                  <dd>{preset.rpm?.toLocaleString() ?? 'Unlimited'}</dd>
                </div>
                <div>
                  <dt>Tokens / minute</dt>
                  <dd>{preset.tpm?.toLocaleString() ?? 'Unlimited'}</dd>
                </div>
                <div>
                  <dt>Concurrent requests</dt>
                  <dd>
                    {preset.max_concurrent?.toLocaleString() ?? 'Unlimited'}
                  </dd>
                </div>
              </dl>
              <p className="subtitle">
                Used by{' '}
                {attached(id).length
                  ? attached(id).join(', ')
                  : 'no backends yet'}
              </p>
              <div className="backend-card-actions">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={id === 'unlimited' || pending || saving}
                  onClick={() =>
                    setEditing({ id, version: query.data!.settings.version })
                  }
                >
                  <Pencil size={14} /> Edit preset
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  aria-label={'Remove preset ' + preset.name}
                  disabled={
                    id === 'unlimited' ||
                    attached(id).length > 0 ||
                    pending ||
                    saving
                  }
                  onClick={() => {
                    setError('')
                    setRemoving(id)
                  }}
                >
                  <Trash2 size={15} />
                </Button>
              </div>
              {attached(id).length > 0 && id !== 'unlimited' && (
                <p className="subtitle">
                  Reassign these backends before removing this preset.
                </p>
              )}
            </article>
          ))}
      </div>
      {editing && document && (
        <PresetEditor
          initial={document.backend_presets?.[editing.id]}
          saving={saving}
          onClose={() => setEditing(null)}
          onSave={async (preset) => {
            const id = editing.id || 'preset-' + crypto.randomUUID().slice(0, 8)
            await save(
              {
                ...document,
                backend_presets: { ...document.backend_presets, [id]: preset },
              },
              {},
              editing.version,
            )
            setEditing(null)
            setNotice('Preset saved. Assigned backends now use these limits.')
          }}
        />
      )}
      <AlertDialog
        open={!!removing}
        onOpenChange={(open) => {
          if (!open && !saving) setRemoving(null)
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Remove this preset?</AlertDialogTitle>
            <AlertDialogDescription>
              Only presets without assigned backends can be removed.
            </AlertDialogDescription>
          </AlertDialogHeader>
          {error && (
            <p role="alert" className="error">
              {error}
            </p>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={saving}>Cancel</AlertDialogCancel>
            <Button variant="destructive" disabled={saving} onClick={remove}>
              Remove preset
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  )
}

function PresetEditor({
  initial,
  saving,
  onClose,
  onSave,
}: {
  initial?: BackendPreset
  saving: boolean
  onClose: () => void
  onSave: (preset: BackendPreset) => Promise<void>
}) {
  const [name, setName] = useState(initial?.name ?? '')
  const [rpm, setRPM] = useState(initial?.rpm?.toString() ?? '')
  const [tpm, setTPM] = useState(initial?.tpm?.toString() ?? '')
  const [concurrent, setConcurrent] = useState(
    initial?.max_concurrent?.toString() ?? '',
  )
  const [error, setError] = useState('')
  const [dirty, setDirty] = useState(false)
  const [discard, setDiscard] = useState(false)
  function close() {
    if (!saving) {
      if (dirty) setDiscard(true)
      else onClose()
    }
  }
  async function submit(event: FormEvent) {
    event.preventDefault()
    try {
      await onSave({
        name: name.trim(),
        rpm: rpm ? Number(rpm) : null,
        tpm: tpm ? Number(tpm) : null,
        max_concurrent: concurrent ? Number(concurrent) : null,
      })
    } catch (error) {
      setError((error as Error).message)
    }
  }
  return (
    <>
      <Dialog
        open
        onOpenChange={(open) => {
          if (!open) close()
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {initial ? 'Edit preset' : 'Create preset'}
            </DialogTitle>
            <DialogDescription>
              Leave a limit blank for unlimited. Changes apply to every backend
              assigned to this preset.
            </DialogDescription>
          </DialogHeader>
          <form
            className="backend-form"
            onSubmit={submit}
            onChange={() => setDirty(true)}
          >
            <label className="field">
              Preset name
              <Input
                aria-label="Preset name"
                required
                maxLength={128}
                value={name}
                onChange={(event) => setName(event.target.value)}
              />
            </label>
            <label className="field">
              Requests per minute
              <Input
                aria-label="Requests per minute"
                type="number"
                min={1}
                step={1}
                max={1000000000}
                placeholder="Unlimited"
                value={rpm}
                onChange={(event) => setRPM(event.target.value)}
              />
            </label>
            <label className="field">
              Tokens per minute
              <Input
                aria-label="Tokens per minute"
                type="number"
                min={1}
                step={1}
                max={1000000000000}
                placeholder="Unlimited"
                value={tpm}
                onChange={(event) => setTPM(event.target.value)}
              />
            </label>
            <label className="field">
              Concurrent requests
              <Input
                aria-label="Concurrent requests"
                type="number"
                min={1}
                step={1}
                max={1000000}
                placeholder="Unlimited"
                value={concurrent}
                onChange={(event) => setConcurrent(event.target.value)}
              />
            </label>
            {error && (
              <p className="error whitespace-pre-line" role="alert">
                {error}
              </p>
            )}
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                disabled={saving}
                onClick={close}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={saving}>
                {saving ? 'Saving…' : 'Save preset'}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
      <AlertDialog open={discard} onOpenChange={setDiscard}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Discard preset changes?</AlertDialogTitle>
            <AlertDialogDescription>
              Your unsaved changes will be lost.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep editing</AlertDialogCancel>
            <Button variant="destructive" onClick={onClose}>
              Discard changes
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
