const { execFileSync } = require('node:child_process');

function checkGoCrypto(execute = execFileSync) {
  const output = execute('go', ['list', '-deps', '-test', '-f', '{{.ImportPath}}', './...'], {
    encoding: 'utf8',
    timeout: 120000,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const forbidden = [...new Set(output.split(/\r?\n/)
    .map(line => line.trim().split(/\s/, 1)[0])
    .filter(name => name === 'golang.org/x/crypto/openpgp' || name.startsWith('golang.org/x/crypto/openpgp/')))];
  if (forbidden.length > 0) {
    throw new Error(`GO-2026-5932: deprecated OpenPGP imports are forbidden:\n${forbidden.join('\n')}\nUse a maintained OpenPGP implementation if interoperability is required.`);
  }
  return 'Go crypto policy passed: no deprecated OpenPGP imports in production or test dependencies.';
}

if (require.main === module) {
  try {
    console.log(checkGoCrypto());
  } catch (error) {
    if (error.stderr) process.stderr.write(error.stderr);
    console.error(error.message);
    process.exitCode = 1;
  }
}

module.exports = { checkGoCrypto };
