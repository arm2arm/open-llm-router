let expired = false
const listeners = new Set<() => void>()
export const sessionEnded = () => expired
export function watchSession(listener: () => void) {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}
export function setSessionEnded(value: boolean) {
  if (expired === value) return
  expired = value
  for (const listener of listeners) listener()
}

// Return only to portal pages. Never interpret a caller's value as an origin.
export function safeNext(value: string | null) {
  if (
    !value ||
    value.length > 2048 ||
    value.includes('\\') ||
    [...value].some((character) => character.charCodeAt(0) <= 32)
  )
    return '/portal/'
  try {
    const url = new URL(value, window.location.origin)
    if (
      !value.startsWith('/portal/') ||
      url.origin !== window.location.origin ||
      !url.pathname.startsWith('/portal/')
    )
      return '/portal/'
    if (
      /^\/portal\/(?:sign-in|login|logout|callback)(?:\/|$)/.test(url.pathname)
    )
      return '/portal/'
    return url.pathname + url.search + url.hash
  } catch {
    return '/portal/'
  }
}
