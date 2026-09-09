// @vitest-environment node
/**
 * CONCORD_AUDIOCAP_PATH must be a value that AGREES, never a specifier (#3194, F1).
 *
 * Found by the Phase-4 red-team pass. The loader took the env var and handed it
 * straight to `require()`. The sink is `require()`, not `dlopen()`, which is what
 * made it serious: the payload need not be a Mach-O, so a `.js` file works — and
 * then macOS library validation, Windows Authenticode and
 * EnableEmbeddedAsarIntegrityValidation are all irrelevant, because no signed
 * binary is ever loaded and nothing is written into the app bundle. `codesign
 * --verify` stays green and macOS App Management never prompts, while attacker
 * JS runs inside a notarized process holding Screen Recording and Microphone
 * grants — and `capability()` becomes attacker-authored, able to forge
 * `perProcessAudio: true`, which is #2161's defect.
 *
 * The env var needs LESS privilege than replacing the .node on disk, and defeats
 * a control that route does not. ADR-0043 § R6 said local replacement was "a
 * restatement of pre-existing local-write risk"; that was true of the Mach-O swap
 * and false of this channel.
 *
 * The fix is an allowlist of ONE derived absolute path, not input sanitization.
 */
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resolveNativeAddonPath } from '../../../src/main/nativeAddonPath';

const BINARY = 'concord_audiocap.node';

/** Load the CJS loader fresh with process.resourcesPath and the env var forced. */
async function loadWith(opts: { resourcesPath?: string; envPath?: string }) {
  const hadRes = Object.prototype.hasOwnProperty.call(process, 'resourcesPath');
  const resDescriptor = Object.getOwnPropertyDescriptor(process, 'resourcesPath');
  const prevEnv = process.env.CONCORD_AUDIOCAP_PATH;
  try {
    if (opts.resourcesPath === undefined) {
      delete (process as { resourcesPath?: string }).resourcesPath;
    } else {
      Object.defineProperty(process, 'resourcesPath', {
        value: opts.resourcesPath,
        configurable: true,
      });
    }
    if (opts.envPath === undefined) delete process.env.CONCORD_AUDIOCAP_PATH;
    else process.env.CONCORD_AUDIOCAP_PATH = opts.envPath;

    vi.resetModules();
    return await import('../../../native/concord-audiocap/index.js');
  } finally {
    if (hadRes && resDescriptor) Object.defineProperty(process, 'resourcesPath', resDescriptor);
    else delete (process as { resourcesPath?: string }).resourcesPath;
    if (prevEnv === undefined) delete process.env.CONCORD_AUDIOCAP_PATH;
    else process.env.CONCORD_AUDIOCAP_PATH = prevEnv;
    vi.resetModules();
  }
}

afterEach(() => {
  vi.resetModules();
});

describe('concord-audiocap loader trust boundary (#3194 F1)', () => {
  const RES = '/Some/App.app/Contents/Resources';

  // DERIVED comes from the RESOLVER, not from this file's own BINARY constant (#3194
  // B4). The two halves of this contract declare the binary name independently —
  // nativeAddonPath.ts has one, native/concord-audiocap/index.js has another — and
  // before this the suite pinned only that they agree on the ENV VAR NAME. A drift in
  // the FILENAME would fail closed in packaged builds only, with the confusing "does
  // not name this build's addon" message, and no test would have caught it. Feeding
  // the accept-control from the resolver turns it into a genuine cross-file agreement
  // check, matching the precedent in packagingIdentity.test.ts ("asserted against the
  // CONSTANT, not a re-hardcoded literal").
  const DERIVED = resolveNativeAddonPath('darwin', true, RES, '/repo') as string;

  // THE EXPLOIT, inverted. Every one of these was loadable before the fix.
  it.each([
    ['an arbitrary absolute path', '/tmp/attacker/payload.js'],
    ['a traversal back out of Resources', `${RES}/../../../../tmp/payload.js`],
    ['a bare module specifier', 'lodash'],
    ['a relative path', './payload.js'],
    ['the right basename in the wrong directory', `/tmp/attacker/${BINARY}`],
  ])('refuses %s', async (_label, envPath) => {
    await expect(loadWith({ resourcesPath: RES, envPath })).rejects.toThrow(
      /does not name this build's addon/
    );
  });

  it('refuses a hostile path even when resourcesPath is absent', async () => {
    await expect(
      loadWith({ resourcesPath: undefined, envPath: '/tmp/attacker/payload.js' })
    ).rejects.toThrow(/does not name this build's addon/);
  });

  // CONTROL — the legitimate packaged path must still be ACCEPTED. Without this
  // the suite passes for a loader that refuses everything, which would satisfy
  // every case above while breaking the product entirely.
  it('accepts the derived Resources path and proceeds to load it', async () => {
    await expect(loadWith({ resourcesPath: RES, envPath: DERIVED })).rejects.toThrow(
      /not built or not loadable/
    );
    await expect(loadWith({ resourcesPath: RES, envPath: DERIVED })).rejects.not.toThrow(
      /does not name this build's addon/
    );
  });

  // CONTROL — dev, where main deliberately sets nothing and the loader uses its
  // own node-gyp output path.
  //
  // Asserts only that the REFUSAL does not fire. It deliberately does not assert
  // a load failure: whether the dev binary exists depends on whether anyone has
  // run `npm run build:native`, and a packaging verification leaves it built.
  // That state-dependence already broke one control in this PR.
  it('does not refuse the dev path when the env var is absent', async () => {
    let refused = false;
    try {
      await loadWith({ resourcesPath: RES, envPath: undefined });
    } catch (err) {
      refused = /does not name this build's addon/.test((err as Error).message);
    }
    expect(refused).toBe(false);
  });

  it('names the rejected value and the expected one, so a refusal is diagnosable', async () => {
    await expect(
      loadWith({ resourcesPath: RES, envPath: '/tmp/attacker/payload.js' })
    ).rejects.toThrow(/\/tmp\/attacker\/payload\.js/);
  });
});

/**
 * VULN-1 — pinning the PATH is not pinning WHAT IS AT IT (#3194 red-team pass).
 *
 * The F1 fix above demotes CONCORD_AUDIOCAP_PATH to a cross-check, so the env var can
 * no longer choose a target. It left the OBJECT at the derived path unconstrained, and
 * `require()` resolves a MODULE rather than a file: a DIRECTORY named
 * `concord_audiocap.node` resolves `index.js` inside it as a CommonJS package, and a
 * SYMLINK resolves to its realpath so the `.js` handler runs instead of the `.node`
 * one. Either executes attacker JS in the utilityProcess with `capability()`
 * attacker-authored and free to forge `perProcessAudio: true` — #2161's defect, reached
 * through the ACCEPTED branch of the allowlist, with no Mach-O loaded and therefore
 * library validation, Authenticode and asar integrity all bypassed.
 *
 * These cases run against the REAL FILESYSTEM rather than a mock, because the defect is
 * a property of Node's module resolver meeting a real directory entry. A mocked `fs`
 * would answer whatever it was told and could not observe the resolver at all — the
 * exact shape `[internal]rules/tests.md` calls testing the handshake instead of the
 * consumer.
 */
describe('concord-audiocap loader object-type guard (#3194 VULN-1)', () => {
  let dir: string;

  beforeEach(() => {
    dir = fs.mkdtempSync(path.join(os.tmpdir(), 'audiocap-vuln1-'));
  });

  afterEach(() => {
    fs.rmSync(dir, { recursive: true, force: true });
    vi.resetModules();
  });

  it('refuses a DIRECTORY at the derived path, and no attacker module is evaluated', async () => {
    const res = path.join(dir, 'Resources');
    const pkg = path.join(res, BINARY);
    fs.mkdirSync(pkg, { recursive: true });
    // If this file were ever evaluated the marker below would be written.
    const marker = path.join(dir, 'EXECUTED');
    fs.writeFileSync(
      path.join(pkg, 'index.js'),
      `require('node:fs').writeFileSync(${JSON.stringify(marker)}, 'x');\n` +
        'module.exports = { capability: () => ({ perProcessAudio: true }) };\n'
    );

    await expect(loadWith({ resourcesPath: res, envPath: pkg })).rejects.toThrow(
      /is not a regular file/
    );
    // The assertion that actually matters: refusing is worthless if it ran first.
    expect(fs.existsSync(marker)).toBe(false);
  });

  it('refuses a SYMLINK at the derived path — lstat, not stat', async () => {
    const res = path.join(dir, 'Resources');
    fs.mkdirSync(res, { recursive: true });
    const payload = path.join(dir, 'payload.js');
    const marker = path.join(dir, 'EXECUTED_LINK');
    fs.writeFileSync(
      payload,
      `require('node:fs').writeFileSync(${JSON.stringify(marker)}, 'x');\n` +
        'module.exports = { capability: () => ({ perProcessAudio: true }) };\n'
    );
    const link = path.join(res, BINARY);
    fs.symlinkSync(payload, link);

    await expect(loadWith({ resourcesPath: res, envPath: link })).rejects.toThrow(
      /is not a regular file/
    );
    expect(fs.existsSync(marker)).toBe(false);
  });

  // CONTROL — the guard must not degrade into "refuse everything". A real regular file
  // has to reach the load attempt and fail there, because an unloadable binary is a
  // PACKAGING DEFECT and must not be reported as a trust violation. Without this the
  // suite passes for a loader that refuses every input, which would break the product.
  it('admits a regular file, which then fails as a packaging defect rather than a refusal', async () => {
    const res = path.join(dir, 'Resources');
    fs.mkdirSync(res, { recursive: true });
    const real = path.join(res, BINARY);
    fs.writeFileSync(real, 'not a mach-o');

    await expect(loadWith({ resourcesPath: res, envPath: real })).rejects.toThrow(
      /not built or not loadable/
    );
    await expect(loadWith({ resourcesPath: res, envPath: real })).rejects.not.toThrow(
      /is not a regular file/
    );
  });

  // CONTROL — absent must stay a packaging defect too, not become a trust refusal.
  it('reports an absent addon as a packaging defect, not an object-type refusal', async () => {
    const res = path.join(dir, 'Resources');
    fs.mkdirSync(res, { recursive: true });
    await expect(loadWith({ resourcesPath: res, envPath: path.join(res, BINARY) })).rejects.toThrow(
      /not built or not loadable/
    );
  });
});
