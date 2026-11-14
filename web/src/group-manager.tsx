import { useState } from 'react'
import { Button } from '@/components/ui/button'
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
import { GroupPolicies } from '@/policies'
import { useBackendWorkspace } from '@/backend-workspace'
import type { RoutingDocument } from '@/lib/api'

export function GroupManager() {
  const { query, document, save, saving } = useBackendWorkspace()
  const [editor, setEditor] = useState<{
    document: RoutingDocument
    version: number
  } | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [dirty, setDirty] = useState(false)
  const [discard, setDiscard] = useState(false)
  function close() {
    if (!saving) {
      if (dirty) setDiscard(true)
      else setEditor(null)
    }
  }
  return (
    <div>
      <Button
        variant="outline"
        disabled={!document || !!query.data?.settings.draft}
        onClick={() => {
          setEditor({
            document: structuredClone(document!),
            version: query.data!.settings.version,
          })
          setDirty(false)
          setError('')
        }}
      >
        Manage group policies
      </Button>
      {notice && (
        <p role="status" className="subtitle">
          {notice}
        </p>
      )}
      <Dialog
        open={!!editor}
        onOpenChange={(open) => {
          if (!open) close()
        }}
      >
        <DialogContent className="sm:max-w-3xl">
          <DialogHeader>
            <DialogTitle>Manage group policies</DialogTitle>
            <DialogDescription>
              Create staff or student groups and set their per-user permissions
              and limits. Backend group restrictions and shared backend presets
              apply in addition. Reassign a backend before removing a group it
              uses.
            </DialogDescription>
          </DialogHeader>
          {editor && (
            <GroupPolicies
              document={editor.document}
              onChange={(document) => {
                setEditor({ ...editor, document })
                setDirty(true)
              }}
            />
          )}
          {error && (
            <p role="alert" className="error whitespace-pre-line">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button variant="outline" disabled={saving} onClick={close}>
              Cancel
            </Button>
            <Button
              disabled={saving || !editor || !dirty}
              onClick={async () => {
                if (!editor) return
                try {
                  await save(editor.document, {}, editor.version)
                  setEditor(null)
                  setNotice('Group policies saved and active.')
                } catch (error) {
                  setError((error as Error).message)
                }
              }}
            >
              {saving ? 'Saving…' : 'Save group policies'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <AlertDialog open={discard} onOpenChange={setDiscard}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Discard group changes?</AlertDialogTitle>
            <AlertDialogDescription>
              Your unsaved changes will be lost.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep editing</AlertDialogCancel>
            <Button
              variant="destructive"
              onClick={() => {
                setDiscard(false)
                setEditor(null)
              }}
            >
              Discard changes
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}
