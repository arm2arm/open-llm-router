import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { collectNotices } from './licenses.mjs'

test('unapproved licenses and missing license texts fail closed', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'olr-license-test-'))
  try {
    const dependency = path.join(root, 'node_modules', 'fixture')
    fs.mkdirSync(dependency, { recursive: true })
    fs.writeFileSync(
      path.join(root, 'package-lock.json'),
      JSON.stringify({
        packages: {
          'node_modules/fixture': { version: '1.0.0' },
        },
      }),
    )
    const pkg = path.join(dependency, 'package.json')
    fs.writeFileSync(
      pkg,
      JSON.stringify({
        name: 'fixture',
        version: '1.0.0',
        license: 'GPL-3.0-only',
      }),
    )
    assert.throws(() => collectNotices(root), /Unapproved license/)
    fs.writeFileSync(
      pkg,
      JSON.stringify({ name: 'fixture', version: '1.0.0', license: 'MIT' }),
    )
    assert.throws(() => collectNotices(root), /Missing license text/)
    fs.writeFileSync(
      path.join(dependency, 'LICENSE'),
      'Synthetic license fixture',
    )
    assert.match(collectNotices(root), /fixture@1.0.0.*MIT/)
  } finally {
    fs.rmSync(root, { recursive: true })
  }
})
