import assert from 'node:assert/strict'
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { checkLinks, document } from './check-docs.mjs'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

test('GitHub slugs include duplicates, Unicode, code and Setext headings', () => {
  const parsed = document(
    '# Hello, `world`!\n## Hello, world!\n## Über die API\nSetext\n---\n# Inline <em>HTML</em>\n',
  )
  assert.deepEqual(
    [...parsed.anchors],
    ['hello-world', 'hello-world-1', 'über-die-api', 'setext'],
  )
})

test('code examples do not invent headings or Markdown links', () => {
  const parsed = document(
    '```md\n# Example\n[not a link](absent.md)\n```\n`[also code](missing.md)`',
  )
  assert.equal(parsed.anchors.size, 0)
  assert.equal(parsed.links.length, 0)
})

test('reference links, HTML IDs and percent-encoded relative targets resolve', () => {
  const root = mkdtempSync(path.join(os.tmpdir(), 'olr-doc-links-'))
  try {
    writeFileSync(
      path.join(root, 'source.md'),
      '[read][target]\n\n[target]: other%20file.md#über\n',
    )
    writeFileSync(path.join(root, 'other file.md'), '<a id="über"></a>\n')
    assert.deepEqual(checkLinks(root, ['source.md', 'other file.md']), [])
    writeFileSync(path.join(root, 'other file.md'), '# Different heading\n')
    assert.match(checkLinks(root, ['source.md'])[0], /missing anchor/)
  } finally {
    rmSync(root, { recursive: true })
  }
})

test('missing files, stale anchors and out-of-bounds source lines fail', () => {
  const root = mkdtempSync(path.join(os.tmpdir(), 'olr-doc-links-'))
  try {
    writeFileSync(
      path.join(root, 'source.md'),
      '# Present\n[one](missing.md) [two](#absent) [three](source.md#L999)\n',
    )
    assert.equal(checkLinks(root, ['source.md']).length, 3)
  } finally {
    rmSync(root, { recursive: true })
  }
})

test('operator instructions and user-key help cannot promise key recovery', () => {
  const root = path.resolve(
    path.dirname(fileURLToPath(import.meta.url)),
    '../..',
  )
  const panel = readFileSync(path.join(root, 'web/src/local-users.tsx'), 'utf8')
  assert.doesNotMatch(panel, /encryption enabled|Copy is available|Older keys/i)
  assert.match(panel, /shown once when created/)
  const install = readFileSync(path.join(root, 'docs/INSTALL.md'), 'utf8')
  assert.equal((install.match(/OLR_API_KEY_ENCRYPTION_KEY/g) ?? []).length, 1)
  assert.match(install, /Retired setting: remove `OLR_API_KEY_ENCRYPTION_KEY`/)
  assert.doesNotMatch(
    install,
    /admins should be able to copy|API-key encryption key are|Copy key is disabled/i,
  )
})

test('a file outside the repository is not a valid GitHub relative target', () => {
  const parent = mkdtempSync(path.join(os.tmpdir(), 'olr-doc-boundary-'))
  try {
    const root = path.join(parent, 'repo')
    mkdirSync(root)
    writeFileSync(path.join(parent, 'outside.md'), '# Outside\n')
    writeFileSync(path.join(root, 'source.md'), '[outside](../outside.md)\n')
    assert.match(checkLinks(root, ['source.md'])[0], /outside repository/)
  } finally {
    rmSync(parent, { recursive: true })
  }
})
