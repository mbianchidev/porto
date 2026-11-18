const { readFileSync } = require('node:fs');

// Accepted advisories must have no patched release and stay out of shipped runtime code.
const ACCEPTED_ADVISORIES = new Map([
  // TODO: remove once picomatch publishes a release patched for this ReDoS; every version <= 4.0.7 is affected.
  ['GHSA-fc5q-39xc-vjpf', 'picomatch extglob ReDoS, reached only through build tooling (vite, electron-builder)'],
]);
const BLOCKING_SEVERITIES = new Set(['high', 'critical']);

function advisoryID(via) {
  const match = /GHSA-[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}/i.exec(String(via.url || ''));
  return match ? match[0] : `npm-${via.source}`;
}

function checkAudit(report, accepted = ACCEPTED_ADVISORIES) {
  if (!report || typeof report.vulnerabilities !== 'object') {
    throw new Error('npm audit did not return a JSON report with vulnerabilities');
  }
  const blocking = new Map();
  const tolerated = new Map();
  for (const vulnerability of Object.values(report.vulnerabilities)) {
    for (const via of vulnerability.via || []) {
      if (typeof via !== 'object' || !BLOCKING_SEVERITIES.has(via.severity)) continue;
      const id = advisoryID(via);
      const target = accepted.has(id) ? tolerated : blocking;
      target.set(id, `${id} ${via.name}: ${via.title} (${via.severity})`);
    }
  }
  if (blocking.size > 0) {
    throw new Error(`npm audit found high or critical advisories:\n${[...blocking.values()].join('\n')}`);
  }
  return [...tolerated.keys()].map(id => `::warning::Accepted ${tolerated.get(id)}: ${accepted.get(id)}`);
}

if (require.main === module) {
  try {
    for (const warning of checkAudit(JSON.parse(readFileSync(0, 'utf8')))) console.log(warning);
    console.log('npm audit policy passed: no unaccepted high or critical advisories.');
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}

module.exports = { checkAudit };
