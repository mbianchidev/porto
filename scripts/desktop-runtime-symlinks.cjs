const fs = require('node:fs')
const path = require('node:path')

const LIMA_TEMPLATES_SUFFIX = path.join(
  'runtime',
  'lima',
  'share',
  'doc',
  'lima',
  'templates',
)
const LIMA_TEMPLATES_TARGET = path.join('..', '..', 'lima', 'templates')

function symlinksUnder(root) {
  const links = []
  const pending = [path.resolve(root)]

  while (pending.length > 0) {
    const current = pending.pop()
    for (const entry of fs.readdirSync(current, { withFileTypes: true })) {
      const entryPath = path.join(current, entry.name)
      if (entry.isSymbolicLink()) {
        links.push(entryPath)
      } else if (entry.isDirectory()) {
        pending.push(entryPath)
      }
    }
  }

  return links
}

function validatePortableSymlinks(root) {
  const resolvedRoot = path.resolve(root)

  for (const link of symlinksUnder(resolvedRoot)) {
    const target = fs.readlinkSync(link)
    if (path.isAbsolute(target)) {
      throw new Error(`absolute symlink is not portable: ${link} -> ${target}`)
    }

    const resolvedTarget = path.resolve(path.dirname(link), target)
    const relativeTarget = path.relative(resolvedRoot, resolvedTarget)
    if (relativeTarget === '..' || relativeTarget.startsWith(`..${path.sep}`)) {
      throw new Error(`symlink escapes package root: ${link} -> ${target}`)
    }
    if (!fs.existsSync(resolvedTarget)) {
      throw new Error(`broken symlink: ${link} -> ${target}`)
    }
  }
}

function normalizeElectronRuntimeSymlinks(root) {
  const resolvedRoot = path.resolve(root)

  for (const link of symlinksUnder(resolvedRoot)) {
    if (!link.endsWith(LIMA_TEMPLATES_SUFFIX)) continue

    const expectedTarget = path.resolve(path.dirname(link), LIMA_TEMPLATES_TARGET)
    if (!fs.existsSync(expectedTarget)) {
      throw new Error(`Lima templates target is missing: ${expectedTarget}`)
    }

    if (fs.readlinkSync(link) !== LIMA_TEMPLATES_TARGET) {
      fs.unlinkSync(link)
      fs.symlinkSync(LIMA_TEMPLATES_TARGET, link)
    }
  }

  validatePortableSymlinks(resolvedRoot)
}

if (require.main === module) {
  const [mode, root] = process.argv.slice(2)
  if (!root || (mode !== '--normalize' && mode !== '--validate')) {
    console.error('usage: desktop-runtime-symlinks.cjs <--normalize|--validate> <package-root>')
    process.exitCode = 2
  } else {
    try {
      if (mode === '--normalize') {
        normalizeElectronRuntimeSymlinks(root)
      } else {
        validatePortableSymlinks(root)
      }
    } catch (error) {
      console.error(error.message)
      process.exitCode = 1
    }
  }
}

module.exports = {
  normalizeElectronRuntimeSymlinks,
  validatePortableSymlinks,
}
