// @vitest-environment node
import { afterEach, describe, expect, it } from 'vitest';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { runProbe, verifiedElectronVersion } from './runner.mjs';

const directories: string[] = [];
function fixture(contents: string): string {
  const directory = mkdtempSync(path.join(tmpdir(), 'engine runner '));
  directories.push(directory);
  const script = path.join(directory, 'probe fixture.cjs');
  writeFileSync(script, contents);
  return script;
}
afterEach(() => {
  for (const directory of directories.splice(0))
    rmSync(directory, { recursive: true, force: true });
});
describe('desktop engine probe process verdict', () => {
  it('accepts a real child process that completes and prints its verdict', () => {
    const script = fixture("process.stdout.write('PROBE PASS fixture\\n')");
    expect(() => runProbe(process.execPath, script, [], 'PROBE PASS fixture')).not.toThrow();
  });
  it('refuses a failing child even when it prints a passing verdict', () => {
    const script = fixture("process.stdout.write('PROBE PASS fixture\\n'); process.exitCode = 1");
    expect(() => runProbe(process.execPath, script, [], 'PROBE PASS fixture')).toThrow('status 1');
  });
  it('refuses a clean exit without an explicit passing verdict', () => {
    const script = fixture("process.stdout.write('probe armed\\n')");
    expect(() => runProbe(process.execPath, script, [], 'PROBE PASS fixture')).toThrow(
      'did not emit'
    );
  });
  it('refuses an armed child that never completes', () => {
    const script = fixture(
      "process.stdout.write('PROBE PASS fixture\\n'); setInterval(() => {}, 1000)"
    );
    expect(() => runProbe(process.execPath, script, [], 'PROBE PASS fixture', 250)).toThrow(
      'could not complete'
    );
  });
});

describe('desktop engine version binding', () => {
  function installation(
    declared: string,
    lockedSpecifier: string,
    locked: string,
    installed: string
  ) {
    const root = mkdtempSync(path.join(tmpdir(), 'engine versions '));
    directories.push(root);
    mkdirSync(path.join(root, 'node_modules/electron'), { recursive: true });
    writeFileSync(
      path.join(root, 'package.json'),
      JSON.stringify({ devDependencies: { electron: declared } })
    );
    writeFileSync(
      path.join(root, 'package-lock.json'),
      JSON.stringify({
        packages: {
          '': { devDependencies: { electron: lockedSpecifier } },
          'node_modules/electron': { version: locked },
        },
      })
    );
    writeFileSync(
      path.join(root, 'node_modules/electron/package.json'),
      JSON.stringify({ version: installed })
    );
    return root;
  }
  it('accepts a matching manifest, lock and installed package', () => {
    expect(verifiedElectronVersion(installation('^45.0.0', '^45.0.0', '45.0.0', '45.0.0'))).toBe(
      '45.0.0'
    );
  });
  it.each([
    ['^45.0.0', '^44.9.1', '45.0.0', '45.0.0', 'lock specifier'],
    ['^45.0.0', '^45.0.0', '44.9.1', '44.9.1', 'does not satisfy'],
    ['^45.0.0', '^45.0.0', '45.0.0', '44.9.1', 'Installed Electron'],
  ])(
    'refuses inconsistent versions %s / %s / %s / %s',
    (declared, specifier, locked, installed, reason) => {
      expect(() =>
        verifiedElectronVersion(installation(declared, specifier, locked, installed))
      ).toThrow(reason);
    }
  );
});
