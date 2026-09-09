import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { NATIVE_ADDON_ENV } from '../../../src/main/nativeAddonPath';

// The loader is plain CJS living outside src/. vi.resetModules() + a fresh
// dynamic import re-runs its module body, which is where the throw happens.
const loadFresh = () => {
  vi.resetModules();
  return import('../../../native/concord-audiocap/index.js');
};

// A Resources directory that does not exist, plus the path the loader DERIVES
// from it. Since #3194 F1 the env var must equal that derived path — an arbitrary
// value is refused before the load is even attempted, so pointing it at a bare
// /nonexistent/... would exercise the refusal branch instead of the one these
// cases are about.
const RESOURCES = '/nonexistent-resources';
const ABSENT = `${RESOURCES}/concord_audiocap.node`;

let resourcesDescriptor: PropertyDescriptor | undefined;

beforeEach(() => {
  resourcesDescriptor = Object.getOwnPropertyDescriptor(process, 'resourcesPath');
  Object.defineProperty(process, 'resourcesPath', {
    value: RESOURCES,
    configurable: true,
  });
});

afterEach(() => {
  if (resourcesDescriptor) Object.defineProperty(process, 'resourcesPath', resourcesDescriptor);
  else delete (process as { resourcesPath?: string }).resourcesPath;
  delete process.env[NATIVE_ADDON_ENV];
  vi.resetModules();
});

describe('concord-audiocap loader (#3194)', () => {
  it('throws when the binary is absent, naming the reachable route', async () => {
    process.env[NATIVE_ADDON_ENV] = ABSENT;
    await expect(loadFresh()).rejects.toThrow(/extraResource/);
  });

  it('names the env var, so the fork and the loader cannot silently disagree', async () => {
    process.env[NATIVE_ADDON_ENV] = ABSENT;
    await expect(loadFresh()).rejects.toThrow(new RegExp(NATIVE_ADDON_ENV));
  });

  it('no longer points the reader at the unreachable unpackDir route', async () => {
    process.env[NATIVE_ADDON_ENV] = ABSENT;
    await expect(loadFresh()).rejects.not.toThrow(/unpackDir/);
  });

  it('reports the path it actually tried, not a hardcoded one', async () => {
    process.env[NATIVE_ADDON_ENV] = ABSENT;
    await expect(loadFresh()).rejects.toThrow(new RegExp(ABSENT));
  });

  // VACUITY CONTROL — [internal]rules/tests.md § Vacuity.
  //
  // The failure this guards is a loader that returns a false capability instead
  // of throwing. That would be indistinguishable from a legitimate "unsupported
  // platform" rung and would hide a PACKAGING DEFECT behind a normal-looking
  // silent video-only state, so the missing audio gets reported by a user rather
  // than by CI. Asserting only "it throws" passes for the wrong reason if the
  // module happens to fail for some unrelated import error, so also assert that
  // nothing resolved.
  it('does not degrade to a false capability', async () => {
    process.env[NATIVE_ADDON_ENV] = ABSENT;
    let resolved: unknown;
    try {
      resolved = await loadFresh();
    } catch {
      resolved = undefined;
    }
    expect(resolved).toBeUndefined();
  });
});
