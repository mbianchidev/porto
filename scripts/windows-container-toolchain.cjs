const fs = require('node:fs');
const path = require('node:path');

const binaries = [
  'containerd.exe', 'containerd-shim-runhcs-v1.exe', 'nerdctl.exe',
  'nat.exe', 'sdnbridge.exe', 'sdnoverlay.exe',
];

function validateWindowsToolchain(directory, architecture) {
  if (architecture !== 'amd64') {
    throw new Error('Native Windows-container runtime artifacts are amd64 only; architecture substitution is forbidden.');
  }
  for (const name of binaries) {
    const filename = path.join(directory, name);
    if (!fs.lstatSync(filename).isFile()) throw new Error(`${filename} must be a regular native executable.`);
    const descriptor = fs.openSync(filename, 'r');
    try {
      const header = Buffer.alloc(64);
      if (fs.readSync(descriptor, header, 0, header.length, 0) !== header.length || header.toString('ascii', 0, 2) !== 'MZ') {
        throw new Error(`${filename} does not contain a Windows PE executable.`);
      }
      const offset = header.readUInt32LE(0x3c);
      const signature = Buffer.alloc(6);
      if (offset < 64 || offset + signature.length > fs.fstatSync(descriptor).size ||
          fs.readSync(descriptor, signature, 0, signature.length, offset) !== signature.length ||
          signature.toString('ascii', 0, 4) !== 'PE\0\0') {
        throw new Error(`${filename} has an invalid Windows PE header.`);
      }
      if (signature.readUInt16LE(4) !== 0x8664) throw new Error(`${filename} is not a native AMD64 Windows executable.`);
    } finally {
      fs.closeSync(descriptor);
    }
  }
}

if (require.main === module) {
  try {
    const [, , directory, architecture] = process.argv;
    if (!directory || !architecture) throw new Error('usage: windows-container-toolchain.cjs <bin-directory> <architecture>');
    validateWindowsToolchain(directory, architecture);
    console.log(`Native Windows ${architecture} container runtime artifacts verified.`);
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}

module.exports = { binaries, validateWindowsToolchain };
