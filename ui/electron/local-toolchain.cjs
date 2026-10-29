const fs = require('node:fs')
const path = require('node:path')

function localToolchain(runtimeRoot, versions, {
  platform = process.platform,
  existsImpl = fs.existsSync,
} = {}) {
  const suffix = platform === 'win32' ? '.exe' : ''
  const commands = []
  for (const name of ['syft', 'grype', 'helm']) {
    const expected = versions.get(name)
    if (!expected) throw new Error(`Bundled local tool ${name} has no version metadata`)
    const executable = path.join(runtimeRoot, 'bin', `${name}${suffix}`)
    if (expected.startsWith('not available')) {
      if (existsImpl(executable)) throw new Error(`Unsupported local tool ${name} unexpectedly contains an executable`)
      continue
    }
    if (!existsImpl(executable) || !existsImpl(path.join(runtimeRoot, 'licenses', `${name}.txt`))) {
      throw new Error(`Bundled local tool ${name} is missing its executable or license`)
    }
    commands.push([name, executable, name === 'helm' ? ['version', '--template', '{{.Version}}'] : ['version', '-o', 'json'], expected])
  }
  return commands
}

module.exports = { localToolchain }
