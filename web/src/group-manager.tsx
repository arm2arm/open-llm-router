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

export function GroupManager({ groupName }: { groupName?: string }) {
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
        aria-label={groupName ? `Configure policy for ${groupName}` : undefined}
        disabled={
          !document ||
          !!query.data?.settings.draft ||
          (groupName !== undefined &&
            new TextEncoder().encode(groupName).length > 128)
        }
        onClick={() => {
          const next = structuredClone(document!)
          const added =
            groupName !== undefined && !Object.hasOwn(next.groups, groupName)
          if (added)
            next.groups = {
              ...next.groups,
              [groupName]: {
                models: [],
                strict_quota: false,
                admin: false,
                priority: 0,
              },
            }
          setEditor({
            document: next,
            version: query.data!.settings.version,
          })
          setDirty(added)
          setError('')
        }}
      >
        {groupName ? 'Configure policy' : 'Manage group policies'}
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
            <DialogTitle>
              {groupName
                ? `Configure ${groupName} policy`
                : 'Manage group policies'}
            </DialogTitle>
            <DialogDescription>
              Use the exact group name received from Keycloak and choose its
              models and per-user limits. New policies start with no model or
              administrator access. Backend restrictions and shared presets
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
