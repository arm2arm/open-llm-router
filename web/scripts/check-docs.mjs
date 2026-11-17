/* global console, process */
import { execFileSync } from 'node:child_process'
import { readFileSync, existsSync, statSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import GithubSlugger from 'github-slugger'
import { fromMarkdown } from 'mdast-util-from-markdown'

function walk(node, visit) {
  visit(node)
  for (const child of node.children ?? []) walk(child, visit)
}

function headingText(node) {
  if (node.type === 'html') return node.value.replace(/<[^>]*>/g, '')
  return (
    node.value ?? node.alt ?? (node.children ?? []).map(headingText).join('')
  )
}

export function document(source) {
  const tree = fromMarkdown(source)
  const slugger = new GithubSlugger()
  const anchors = new Set()
  const links = []
  walk(tree, (node) => {
    if (node.type === 'heading') anchors.add(slugger.slug(headingText(node)))
    if (['link', 'image', 'definition'].includes(node.type)) {
      links.push({ url: node.url, line: node.position.start.line })
    }
    if (node.type === 'html') {
      for (const match of node.value.matchAll(
        /\b(?:id|name)=["']([^"']+)["']/g,
      )) {
        anchors.add(match[1])
      }
      for (const match of node.value.matchAll(
        /\b(?:href|src)=["']([^"']+)["']/g,
      )) {
        links.push({ url: match[1], line: node.position.start.line })
      }
    }
  })
  return { anchors, links }
}

export function checkLinks(root, files) {
  const cache = new Map()
  const load = (file) => {
    if (!cache.has(file)) cache.set(file, document(readFileSync(file, 'utf8')))
    return cache.get(file)
  }
  const errors = []
  for (const file of files) {
    const source = path.resolve(root, file)
    for (const { url, line } of load(source).links) {
      if (/^(?:[a-z][a-z0-9+.-]*:|\/\/)/i.test(url)) continue
      try {
        const [before, ...fragments] = url.split('#')
        const pathname = decodeURIComponent(before.split('?')[0])
        const fragment = decodeURIComponent(fragments.join('#'))
        let target = pathname
          ? path.resolve(path.dirname(source), pathname)
          : source
        if (!existsSync(target)) throw new Error('missing target')
        if (!fragment) continue
        if (statSync(target).isDirectory())
          target = path.join(target, 'README.md')
        if (!existsSync(target)) throw new Error('missing directory README')
        if (/^L\d+(?:-L\d+)?$/.test(fragment)) {
          const lines = readFileSync(target, 'utf8').split('\n').length
          const numbers = fragment.match(/\d+/g).map(Number)
          if (numbers.some((n) => n < 1 || n > lines)) {
            throw new Error('line anchor outside file')
          }
        } else if (
          !/\.md$/i.test(target) ||
          !load(target).anchors.has(fragment)
        ) {
          throw new Error('missing anchor')
        }
      } catch (error) {
        errors.push(`${file}:${line}: ${url} (${error.message})`)
      }
    }
  }
  return errors
}

const script = fileURLToPath(import.meta.url)
if (process.argv[1] && path.resolve(process.argv[1]) === script) {
  const root = path.resolve(path.dirname(script), '../..')
  const files = execFileSync(
    'git',
    ['ls-files', '-z', '--cached', '--others', '--exclude-standard'],
    { cwd: root, encoding: 'utf8' },
  )
    .split('\0')
    .filter((file) => /\.md$/i.test(file))
  const errors = checkLinks(root, [...new Set(files)])
  if (errors.length) {
    console.error(errors.join('\n'))
    process.exitCode = 1
  } else
    console.log(
      `Checked relative links and GitHub anchors in ${files.length} Markdown files`,
    )
}
