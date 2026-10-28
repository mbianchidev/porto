const assert = require('node:assert/strict')
const test = require('node:test')

const { addUtf8Manifest, loadPE, manifest } = require('./windows-qemu-manifest.cjs')

async function fixture(architecture, customManifest) {
  const { NtExecutable, NtExecutableResource } = await loadPE()
  const executable = NtExecutable.createEmpty(false, false)
  executable.newHeader.fileHeader.machine = architecture === 'amd64' ? 0x8664 : 0xaa64
  executable.setSectionByEntry(0, {
    info: {
      name: '.text', virtualSize: 512, virtualAddress: 0, sizeOfRawData: 512, pointerToRawData: 0,
      pointerToRelocations: 0, pointerToLineNumbers: 0, numberOfRelocations: 0,
      numberOfLineNumbers: 0, characteristics: 0x60000020,
    },
    data: new Uint8Array(512).fill(0xcc).buffer,
  })
  executable.newHeader.optionalHeader.addressOfEntryPoint = 0x1000
  executable.setExtraData(Buffer.from('Synthetic retained overlay'))
  const resources = NtExecutableResource.from(executable)
  resources.replaceResourceEntryFromString(10, 'SYNTHETIC', 1033, 'Synthetic retained resource')
  if (customManifest) resources.replaceResourceEntryFromString(24, 1, 0, customManifest)
  resources.outputResource(executable)
  return Buffer.from(executable.generate())
}

for (const architecture of ['amd64', 'arm64']) {
  test(`UTF-8 manifest preserves native ${architecture} code, resources and executable metadata`, async () => {
    const { NtExecutable, NtExecutableResource } = await loadPE()
    const input = await fixture(architecture)
    const result = await addUtf8Manifest(input, architecture)
    const executable = NtExecutable.from(result)
    const resources = NtExecutableResource.from(executable)
    assert.equal(executable.is32bit(), false)
    assert.equal(executable.newHeader.fileHeader.machine, architecture === 'amd64' ? 0x8664 : 0xaa64)
    assert.equal(executable.newHeader.optionalHeader.addressOfEntryPoint, 0x1000)
    const code = executable.getAllSections().find(({ info }) => info.name === '.text')
    assert.deepEqual(new Uint8Array(code.data), new Uint8Array(512).fill(0xcc))
    assert.equal(Buffer.from(executable.getExtraData()).toString(), 'Synthetic retained overlay')
    assert.deepEqual(resources.getResourceEntriesAsString(10, 'SYNTHETIC'), [[1033, 'Synthetic retained resource']])
    assert.deepEqual(resources.getResourceEntriesAsString(24, 1), [[0, manifest]])
    assert.equal(resources.entries.find(({ type }) => type === 24).codepage, 65001)
    assert.equal(executable.newHeader.optionalHeaderDataDirectory.get(4).size, 0)
    assert.deepEqual(await addUtf8Manifest(result, architecture), result)
  })
}

test('manifest editing removes an invalidated synthetic certificate instead of retaining a false signature', async () => {
  const { NtExecutable } = await loadPE()
  const input = await fixture('amd64')
  const padding = Buffer.alloc((8 - input.length % 8) % 8)
  const certificate = Buffer.alloc(8)
  certificate.writeUInt32LE(8)
  certificate.writeUInt16LE(0x0200, 4)
  certificate.writeUInt16LE(2, 6)
  const signed = Buffer.concat([input, padding, certificate])
  const securityDirectory = signed.readUInt32LE(0x3c) + 4 + 20 + 112 + 4 * 8
  signed.writeUInt32LE(input.length + padding.length, securityDirectory)
  signed.writeUInt32LE(certificate.length, securityDirectory + 4)
  assert.throws(() => NtExecutable.from(signed), /signed executable/)
  const result = await addUtf8Manifest(signed, 'amd64')
  const executable = NtExecutable.from(result)
  assert.deepEqual(executable.newHeader.optionalHeaderDataDirectory.get(4), { virtualAddress: 0, size: 0 })
  assert.deepEqual(Buffer.from(executable.getExtraData()),
    Buffer.concat([Buffer.from('Synthetic retained overlay'), padding]))
})

test('an existing custom manifest is refused without replacing its compatibility or privilege settings', async () => {
  const input = await fixture('amd64', '<assembly manifestVersion="1.0">Synthetic custom settings</assembly>')
  const previous = Buffer.from(input)
  await assert.rejects(addUtf8Manifest(input, 'amd64'), /already has a different manifest/)
  assert.deepEqual(input, previous)
})

test('invalid, 32-bit and mismatched native executables never receive a manifest', async () => {
  const { NtExecutable } = await loadPE()
  await assert.rejects(addUtf8Manifest(Buffer.from('Synthetic invalid binary'), 'amd64'))
  await assert.rejects(addUtf8Manifest(await fixture('amd64'), 'arm64'), /architecture mismatch/)
  await assert.rejects(addUtf8Manifest(Buffer.from(NtExecutable.createEmpty(true, false).generate()), 'amd64'),
    /native 64-bit executable/)
  await assert.rejects(addUtf8Manifest(await fixture('amd64'), 'unknown'), /requires amd64 or arm64/)
})
