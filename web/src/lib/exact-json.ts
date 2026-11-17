// Trace previews may contain seeds and IDs outside JavaScript's safe integer
// range. Protect numeric tokens before JSON.parse rounds them. Quoted strings
// are matched as whole tokens, including escapes, and are never interpreted.
export function parseExactJSON(source: string): unknown {
  const prefix = `olr-number-${crypto.randomUUID()}:`
  const numbers = new Map<string, bigint>()
  const protectedSource = source.replace(
    /"(?:\\[\s\S]|[^"\\])*"|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/g,
    (token) => {
      if (
        token.startsWith('"') ||
        /[.eE]/.test(token) ||
        Number.isSafeInteger(Number(token))
      )
        return token
      const key = prefix + numbers.size
      numbers.set(key, BigInt(token))
      return JSON.stringify(key)
    },
  )
  return JSON.parse(protectedSource, (_key, value: unknown) =>
    typeof value === 'string' && numbers.has(value)
      ? numbers.get(value)
      : value,
  )
}

// Copy/download must retain numeric JSON tokens, rather than quote bigints or
// silently round them. Choose a placeholder namespace absent from every string.
export function stringifyExactJSON(value: unknown, space?: number): string {
  let hasBigInt = false
  const ordinary =
    JSON.stringify(
      value,
      (_key, item: unknown) => {
        if (typeof item !== 'bigint') return item
        hasBigInt = true
        return item.toString()
      },
      space,
    ) ?? ''
  if (!hasBigInt) return ordinary
  let prefix = '__olr_exact_number__'
  while (ordinary.includes(prefix)) prefix += '_'
  const numbers: string[] = []
  const text = JSON.stringify(
    value,
    (_key, item: unknown) => {
      if (typeof item !== 'bigint') return item
      numbers.push(item.toString())
      return prefix + (numbers.length - 1)
    },
    space,
  )
  return text.replace(
    new RegExp(`"${prefix}(\\d+)"`, 'g'),
    (_match, index: string) => numbers[Number(index)],
  )
}
