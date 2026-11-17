// Conservatively include the full production dependency closure, even when Vite
// removes unused exports. Missing notices or unreviewed licenses fail the build.
import fs from 'node:fs'
import path from 'node:path'
import process from 'node:process'
import { fileURLToPath } from 'node:url'

export const allowedLicenses = new Set([
  'MIT',
  'Apache-2.0',
  'BSD-2-Clause',
  'BSD-3-Clause',
  'ISC',
  '0BSD',
  'CC0-1.0',
  'Unlicense',
])

export function collectNotices(root) {
  const lock = JSON.parse(
    fs.readFileSync(path.join(root, 'package-lock.json'), 'utf8'),
  )
  const sections = ['Third-party portal dependency notices\n']
  for (const [directory, dependency] of Object.entries(lock.packages).sort()) {
    if (!directory.startsWith('node_modules/') || dependency.dev) continue
    const folder = path.join(root, directory)
    const pkg = JSON.parse(
      fs.readFileSync(path.join(folder, 'package.json'), 'utf8'),
    )
    const license =
      typeof pkg.license === 'object' ? pkg.license.type : pkg.license
    const identifiers =
      typeof license === 'string'
        ? license.replace(/[()]/g, '').split(/\s+(?:OR|AND)\s+/)
        : []
    if (
      !identifiers.length ||
      identifiers.some((id) => !allowedLicenses.has(id))
    ) {
      throw new Error(
        `Unapproved license for ${pkg.name}@${pkg.version}: ${license}`,
      )
    }
    const notices = fs
      .readdirSync(folder)
      .filter((name) => /^(licen[sc]e|notice|copying)(\..*)?$/i.test(name))
      .sort()
    const fallback = path.join(
      root,
      'licenses',
      `${pkg.name.replaceAll('/', '_')}-${pkg.version}-LICENSE`,
    )
    if (
      !notices.some((name) => /^licen[sc]e/i.test(name)) &&
      !fs.existsSync(fallback)
    ) {
      throw new Error(`Missing license text for ${pkg.name}@${pkg.version}`)
    }
    sections.push(`\n=== ${pkg.name}@${pkg.version} (${license}) ===\n`)
    for (const file of notices) {
      sections.push(
        `\n--- ${file} ---\n${fs.readFileSync(path.join(folder, file), 'utf8')}\n`,
      )
    }
    if (fs.existsSync(fallback))
      sections.push(
        `\n--- upstream LICENSE ---\n${fs.readFileSync(fallback, 'utf8')}\n`,
      )
  }
  return sections.join('')
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  const destination = process.argv[2]
  if (!destination) throw new Error('Supply the output notices path')
  fs.mkdirSync(path.dirname(destination), { recursive: true })
  fs.writeFileSync(destination, collectNotices(process.cwd()))
}
