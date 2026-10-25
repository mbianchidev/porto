const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { binaries, validateWindowsToolchain } = require('./windows-container-toolchain.cjs');

function fixture(t) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-windows-tools-'));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  for (const name of binaries) {
    const image = Buffer.alloc(128);
    image.write('MZ', 0);
    image.writeUInt32LE(64, 0x3c);
    image.write('PE\0\0', 64);
    image.writeUInt16LE(0x8664, 68);
    fs.writeFileSync(path.join(directory, name), image);
  }
  return directory;
}

test('all promised native Windows binaries must be AMD64 PE images', t => {
  validateWindowsToolchain(fixture(t), 'amd64');
});

test('ARM64 never silently receives the AMD64 runtime', t => {
  assert.throws(() => validateWindowsToolchain(fixture(t), 'arm64'), /amd64.*only|only.*amd64/);
});

test('a missing runhcs shim is an incomplete native backend', t => {
  const directory = fixture(t);
  fs.unlinkSync(path.join(directory, 'containerd-shim-runhcs-v1.exe'));
  assert.throws(() => validateWindowsToolchain(directory, 'amd64'), /containerd-shim-runhcs-v1/);
});

test('foreign architecture and invalid PE offsets are rejected', t => {
  const directory = fixture(t);
  const filename = path.join(directory, 'nerdctl.exe');
  const image = fs.readFileSync(filename);
  image.writeUInt16LE(0xaa64, 68);
  fs.writeFileSync(filename, image);
  assert.throws(() => validateWindowsToolchain(directory, 'amd64'), /AMD64/);
  image.writeUInt16LE(0x8664, 68);
  image.writeUInt32LE(0x7fffffff, 0x3c);
  fs.writeFileSync(filename, image);
  assert.throws(() => validateWindowsToolchain(directory, 'amd64'), /PE/);
});
