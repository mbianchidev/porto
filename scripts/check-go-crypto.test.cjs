const assert = require('node:assert/strict');
const test = require('node:test');
const { checkGoCrypto } = require('./check-go-crypto.cjs');

test('maintained SSH and unrelated packages remain allowed', () => {
  const output = checkGoCrypto((command, args) => {
    assert.equal(command, 'go');
    assert.deepEqual(args, ['list', '-deps', '-test', '-f', '{{.ImportPath}}', './...']);
    return 'crypto/ecdsa\ngolang.org/x/crypto/ssh\ngolang.org/x/crypto/ssh/knownhosts\n';
  });
  assert.match(output, /no deprecated OpenPGP imports/);
});

test('all deprecated OpenPGP package and test variants are rejected', () => {
  for (const dependency of [
    'golang.org/x/crypto/openpgp',
    'golang.org/x/crypto/openpgp/packet',
    'golang.org/x/crypto/openpgp/armor',
    'golang.org/x/crypto/openpgp [synthetic.test]',
  ]) {
    assert.throws(() => checkGoCrypto(() => dependency), /GO-2026-5932/);
  }
});

test('a similar package name is not mistaken for the deprecated package', () => {
  assert.doesNotThrow(() => checkGoCrypto(() => 'synthetic.invalid/openpgp\ngolang.org/x/crypto/openpgp-example'));
});

test('failed dependency discovery is not treated as an empty safe graph', () => {
  const failure = new Error('synthetic dependency discovery failure');
  assert.throws(() => checkGoCrypto(() => { throw failure; }), error => error === failure);
});
