const assert = require('node:assert/strict');
const test = require('node:test');
const { checkAudit } = require('./npm-audit-policy.cjs');

function advisory(id, severity, name = 'synthetic-package') {
  return { source: 1, name, title: 'synthetic advisory', url: `https://github.com/advisories/${id}`, severity };
}

const accepted = new Map([['GHSA-aaaa-bbbb-cccc', 'synthetic build-only advisory']]);

test('accepted advisories pass with a visible warning', () => {
  const warnings = checkAudit({
    vulnerabilities: {
      'synthetic-package': { via: [advisory('GHSA-aaaa-bbbb-cccc', 'high')] },
      'synthetic-parent': { via: ['synthetic-package'] },
    },
  }, accepted);
  assert.equal(warnings.length, 1);
  assert.match(warnings[0], /^::warning::Accepted GHSA-aaaa-bbbb-cccc/);
});

test('any other high or critical advisory fails even beside an accepted one', () => {
  for (const severity of ['high', 'critical']) {
    assert.throws(() => checkAudit({
      vulnerabilities: {
        'synthetic-package': { via: [advisory('GHSA-aaaa-bbbb-cccc', 'high'), advisory('GHSA-dddd-eeee-ffff', severity)] },
      },
    }, accepted), /GHSA-dddd-eeee-ffff/);
  }
});

test('moderate advisories and malformed reports are handled explicitly', () => {
  assert.deepEqual(checkAudit({ vulnerabilities: { 'synthetic-package': { via: [advisory('GHSA-dddd-eeee-ffff', 'moderate')] } } }, accepted), []);
  assert.throws(() => checkAudit({ error: { code: 'ENOLOCK' } }, accepted), /JSON report/);
});
