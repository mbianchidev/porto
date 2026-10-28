const assert = require('node:assert/strict')
const crypto = require('node:crypto')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const { spawnSync } = require('node:child_process')
const { createRequire } = require('node:module')

const requireElectron = createRequire(path.join(__dirname, '..', 'ui', 'electron', 'package.json'))
const manifestPath = path.join(__dirname, 'patches', 'qemu-windows-utf8.manifest')
const manifest = fs.readFileSync(manifestPath, 'utf8')
const machines = { amd64: 0x8664, arm64: 0xaa64 }

async function loadPE() {
  return await requireElectron('pe-library/cjs').load()
}

function digest(bytes) {
  return crypto.createHash('sha256').update(Buffer.from(bytes)).digest('hex')
}

function retainedContents(executable, resources) {
  const resourceSection = executable.getSectionByEntry(2)
  return {
    machine: executable.newHeader.fileHeader.machine,
    entryPoint: executable.newHeader.optionalHeader.addressOfEntryPoint,
    imageBase: executable.getImageBase(),
    sections: executable.getAllSections().filter((section) => section !== resourceSection).map(({ info, data }) => [
      info.name, info.virtualAddress, info.virtualSize, info.characteristics, data === null ? null : digest(data),
    ]),
    resources: resources.entries.filter(({ type }) => type !== 24).map(({ type, id, lang, codepage, bin }) => [
      type, id, lang, codepage, digest(bin),
    ]).sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b))),
    extra: executable.getExtraData() === null ? null : digest(executable.getExtraData()),
  }
}

async function addUtf8Manifest(bytes, architecture) {
  assert.ok(Object.hasOwn(machines, architecture), 'Windows QEMU requires amd64 or arm64')
  const { NtExecutable, NtExecutableResource } = await loadPE()
  // This is a verified build copy. Resource editing removes its vendor signature;
  // package signing must happen after this step.
  const executable = NtExecutable.from(bytes, { ignoreCert: true })
  assert.equal(executable.is32bit(), false, 'Windows QEMU must be a native 64-bit executable')
  assert.equal(executable.newHeader.fileHeader.machine, machines[architecture], 'Windows QEMU architecture mismatch')
  const resources = NtExecutableResource.from(executable)
  const previous = resources.entries.filter(({ type }) => type === 24)
  if (previous.length) {
    assert.ok(previous.length === 1 && previous[0].id === 1 && previous[0].lang === 0 &&
      previous[0].codepage === 65001 &&
      Buffer.from(previous[0].bin).toString('utf8') === manifest,
    'QEMU already has a different manifest; review it instead of overwriting its settings')
    return Buffer.from(bytes)
  }
  const before = retainedContents(executable, resources)
  resources.replaceResourceEntry({
    type: 24, id: 1, lang: 0, codepage: 65001, bin: Uint8Array.from(Buffer.from(manifest, 'utf8')).buffer,
  })
  resources.outputResource(executable)
  const result = Buffer.from(executable.generate())
  const verified = NtExecutable.from(result)
  const verifiedResources = NtExecutableResource.from(verified)
  assert.deepEqual(retainedContents(verified, verifiedResources), before,
    'UTF-8 manifest editing changed native QEMU code, addressing or unrelated resources')
  assert.equal(verifiedResources.getResourceEntriesAsString(24, 1).length, 1)
  assert.equal(verifiedResources.getResourceEntriesAsString(24, 1)[0][1], manifest,
    'Windows QEMU UTF-8 process manifest was not retained')
  assert.equal(verifiedResources.entries.find(({ type }) => type === 24).codepage, 65001)
  return result
}

function verifyUnicodeAccess(directory, architecture) {
  const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-qemu-\u00e9 \u8def\u5f84-'))
  const invoke = (name, args, input) => {
    const result = spawnSync(path.join(directory, name), args, {
      input, encoding: 'utf8', timeout: 10000, maxBuffer: 65536, windowsHide: true,
    })
    if (result.error) throw new Error(`Bundled ${name} Unicode-path probe could not complete`, { cause: result.error })
    assert.equal(result.status, 0, `Bundled ${name} Unicode-path probe failed: ${result.stderr}`)
    return result.stdout
  }
  try {
    const image = path.join(temporary, 'synthetic disk.raw')
    invoke('qemu-img.exe', ['create', '-f', 'raw', image, '1M'])
    assert.equal(fs.statSync(image).size, 1024 * 1024)
    const firmware = path.join(temporary, 'firmware.fd')
    fs.copyFileSync(path.join(directory, 'share', `edk2-${architecture === 'amd64' ? 'x86_64' : 'aarch64'}-code.fd`), firmware)
    const output = invoke(`qemu-system-${architecture === 'amd64' ? 'x86_64' : 'aarch64'}.exe`, [
      '-machine', `${architecture === 'amd64' ? 'q35' : 'virt'},accel=tcg`,
      '-cpu', architecture === 'amd64' ? 'qemu64' : 'neoverse-n1',
      '-m', '128', '-S', '-nodefaults', '-no-user-config', '-display', 'none', '-net', 'none',
      '-drive', `if=pflash,format=raw,readonly=on,file=${firmware}`,
      '-qmp', 'stdio',
    ], '{"execute":"qmp_capabilities","id":"capabilities"}\n{"execute":"quit","id":"quit"}\n')
    const responses = output.trim().split(/\r?\n/).map((line) => JSON.parse(line))
    assert.ok(responses.some((message) => message.QMP), 'Native QEMU did not open its Unicode-path firmware')
    assert.ok(responses.some((message) => message.id === 'quit' && Object.hasOwn(message, 'return')),
      'Native QEMU did not exit cleanly after its Unicode-path probe')
  } finally {
    fs.rmSync(temporary, { recursive: true })
  }
}

async function main(args) {
  const [directory, architecture] = args
  assert.ok(directory && args.length === 2, 'Usage: windows-qemu-manifest.cjs <staged-qemu-directory> <amd64|arm64>')
  assert.ok(Object.hasOwn(machines, architecture), 'Windows QEMU requires amd64 or arm64')
  const root = fs.realpathSync(directory)
  const names = ['qemu-img.exe', `qemu-system-${architecture === 'amd64' ? 'x86_64' : 'aarch64'}.exe`]
  const replacements = []
  for (const name of names) {
    const file = path.join(root, name)
    assert.ok(fs.lstatSync(file).isFile(), 'Only regular staged QEMU executables may be edited')
    replacements.push([file, await addUtf8Manifest(fs.readFileSync(file), architecture)])
  }
  for (const [file, bytes] of replacements) fs.writeFileSync(file, bytes)
  console.log('Bundled QEMU UTF-8 manifest applied before signing; modified build copies no longer retain the vendor signature.')
  if (process.platform === 'win32' && process.arch === (architecture === 'amd64' ? 'x64' : 'arm64')) {
    verifyUnicodeAccess(root, architecture)
    console.log('Native QEMU created a Unicode-path image, opened Unicode-path firmware and exited cleanly.')
  } else {
    console.log('Native Windows QEMU Unicode-path execution remains required on a matching Windows host.')
  }
}

if (require.main === module) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(`Windows QEMU UTF-8 preparation failed: ${error.message}`)
    process.exitCode = 1
  })
}

module.exports = { addUtf8Manifest, loadPE, main, manifest, retainedContents, verifyUnicodeAccess }
