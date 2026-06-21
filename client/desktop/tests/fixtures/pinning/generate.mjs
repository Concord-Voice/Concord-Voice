// Regenerate cert-pinning test fixtures.
// Usage: node client/desktop/tests/fixtures/pinning/generate.mjs
// Commit the resulting PEMs + spki-fingerprints.json.
// These fixtures are test-only and contain no live secrets.
// See plan [internal]plans/2026-04-20-658-updater-feed-cert-pin.md Task 1.2.
import selfsigned from 'selfsigned';
import { writeFileSync } from 'node:fs';
import { X509Certificate, createHash } from 'node:crypto';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const dir = dirname(fileURLToPath(import.meta.url));
const cns = { primary: 'pinning-primary', fallback: 'pinning-fallback', rogue: 'pinning-rogue' };
const out = {};

for (const [name, cn] of Object.entries(cns)) {
  const pems = await selfsigned.generate([{ name: 'commonName', value: cn }], {
    days: 3650,
    keySize: 2048,
    algorithm: 'sha256',
  });
  writeFileSync(join(dir, `${name}.pem`), pems.cert, 'utf-8');
  const spki = new X509Certificate(pems.cert).publicKey.export({ type: 'spki', format: 'der' });
  out[name] = createHash('sha256').update(spki).digest('hex');
}

writeFileSync(
  join(dir, 'spki-fingerprints.json'),
  JSON.stringify(out, null, 2) + '\n',
  'utf-8',
);

console.log('Generated fixtures:', out);
