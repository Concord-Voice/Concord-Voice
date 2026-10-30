import { spawnSync } from 'node:child_process';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import path from 'node:path';

const require = createRequire(import.meta.url);
const { satisfies } = require('semver');

/** Refuse a stale installation or a lockfile that no longer matches the manifest. */
export function verifiedElectronVersion(root) {
  const manifest = JSON.parse(readFileSync(path.join(root, 'package.json'), 'utf8'));
  const lock = JSON.parse(readFileSync(path.join(root, 'package-lock.json'), 'utf8'));
  const installed = JSON.parse(
    readFileSync(path.join(root, 'node_modules/electron/package.json'), 'utf8')
  );
  const declared = manifest.devDependencies.electron;
  const version = lock.packages['node_modules/electron'].version;
  assert.equal(
    lock.packages[''].devDependencies.electron,
    declared,
    'Electron lock specifier differs from manifest'
  );
  assert(satisfies(version, declared), 'Locked Electron does not satisfy the manifest');
  assert.equal(installed.version, version, 'Installed Electron differs from the lock');
  return version;
}

/** A clean exit and an explicit verdict are both required for a runtime proof. */
export function runProbe(executable, script, args, marker, timeoutMs = 60_000) {
  const result = spawnSync(executable, [script, ...args], {
    encoding: 'utf8',
    timeout: timeoutMs,
    killSignal: 'SIGKILL',
    maxBuffer: 1024 * 1024,
    shell: false,
  });
  if (result.stdout) process.stdout.write(result.stdout);
  if (result.stderr) process.stderr.write(result.stderr);
  if (result.error) throw new Error(`Probe could not complete: ${result.error.message}`);
  if (result.status !== 0)
    throw new Error(`Probe exited with status ${result.status}, signal ${result.signal}`);
  if (!result.stdout.split(/\r?\n/).includes(marker))
    throw new Error(`Probe did not emit ${marker}`);
}
