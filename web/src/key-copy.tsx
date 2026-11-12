import { useState } from 'react'
import { Check, Copy } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { api, type Key } from '@/lib/api'

export function useAdminKeyCopy(userID: string) {
  const [copying, setCopying] = useState<string | null>(null)
  const [copied, setCopied] = useState<string | null>(null)
  const [error, setError] = useState<Error | null>(null)
  async function copyKey(key: Key) {
    setCopying(key.id)
    setCopied(null)
    setError(null)
    try {
      // Start the clipboard write during the click gesture (Safari requires it).
      // The raw key stays out of React state, query caches and browser storage.
      const value = api<{ key: string }>(
        `admin/users/${userID}/keys/${key.id}/copy`,
        'POST',
      )
      // A denied clipboard permission may reject before the HTTP request does.
      void value.catch(() => {})
      if (typeof ClipboardItem !== 'undefined' && navigator.clipboard?.write) {
        const content = value.then(
          ({ key }) => new Blob([key], { type: 'text/plain' }),
        )
        void content.catch(() => {})
        await navigator.clipboard.write([
          new ClipboardItem({ 'text/plain': content }),
        ])
      } else {
        const result = await value
        if (!navigator.clipboard?.writeText)
          throw new Error('Clipboard access requires HTTPS or localhost.')
        await navigator.clipboard.writeText(result.key)
      }
      setCopied(key.id)
    } catch (error) {
      setError(
        error instanceof Error ? error : new Error('Could not copy the key.'),
      )
    } finally {
      setCopying(null)
    }
  }
  return { copying, copied, error, copyKey }
}

export function AdminKeyCopyButton({
  keyInfo,
  copy,
}: {
  keyInfo: Key
  copy: ReturnType<typeof useAdminKeyCopy>
}) {
  return (
    <Button
      variant="ghost"
      size="sm"
      className="ml-2"
      disabled={!keyInfo.copy_available || copy.copying !== null}
      title={
        keyInfo.copy_available
          ? 'Copy API key'
          : 'Key is not recoverable with this server’s encryption key. Issue a replacement.'
      }
      aria-label={`Copy key ${keyInfo.label || keyInfo.prefix}`}
      onClick={() => void copy.copyKey(keyInfo)}
    >
      {copy.copied === keyInfo.id ? (
        <Check aria-hidden="true" />
      ) : (
        <Copy aria-hidden="true" />
      )}
      {copy.copying === keyInfo.id ? 'Copying…' : 'Copy key'}
    </Button>
  )
}
