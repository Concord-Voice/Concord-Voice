// @vitest-environment node
import { spawnSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import { mkdir, mkdtemp, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { parse as parseYaml } from 'yaml';
import { FuseV1Options, FuseVersion, getCurrentFuseWire } from '@electron/fuses';
import {
  CheckError,
  INTEGRITY_DIGEST_SENTINEL,
  computeIntegrityDigest,
  deriveExpectedFuses,
  describeFuseWire,
  findFusesConfig,
  isAsarIntegrity,
  loadForgeConfig,
  locatePackagedApp,
  main,
  readAsarIntegrity,
  readPackagedFuseWire,
  scanDigestSlots,
  scanFuseWires,
  verifyAsarDigest,
  verifyDigestSlots,
  verifyFuseWire,
  type AsarIntegrity,
  type Deps,
  type FuseWire,
} from './verify-packaged-app.lib.mts';

// The verifier exists because FusesPlugin and the packager's digest write both fail open, so every
// block below pairs a passing artifact with a broken one: a check that has only ever seen green
// input has not been shown to detect anything.

const DISABLE = 0x30;
const ENABLE = 0x31;
const REMOVED = 0x72;

// @electron/fuses' own sentinel. Hard-coded on purpose: the fixtures below are read back through
// the REAL getCurrentFuseWire, so if an upstream bump moves the sentinel they fail loudly.
const FUSE_SENTINEL = 'dL7pKGdnNz796PbbjQWNKmHXBZaB9tsX';

const RUN_AS_NODE = FuseV1Options.RunAsNode;
const COOKIE = FuseV1Options.EnableCookieEncryption;
const INSPECT = FuseV1Options.EnableNodeCliInspectArguments;
const ASAR_VALIDATION = FuseV1Options.EnableEmbeddedAsarIntegrityValidation;
const ONLY_ASAR = FuseV1Options.OnlyLoadAppFromAsar;

// What forge.config.ts configures, expressed as the config object FusesPlugin receives.
const FUSES_CONFIG = {
  version: FuseVersion.V1,
  [RUN_AS_NODE]: false,
  [INSPECT]: false,
  [ASAR_VALIDATION]: true,
  [ONLY_ASAR]: true,
  [COOKIE]: true,
};

// A produced wire for FUSES_CONFIG: the five configured fuses flipped, the other four left at
// Electron's defaults (index 2, 6, 7, 8).
const GOOD_STATES = [DISABLE, ENABLE, ENABLE, DISABLE, ENABLE, ENABLE, DISABLE, ENABLE, ENABLE];

function fuseBinary(states: number[], version = 1): Buffer {
  return Buffer.concat([
    Buffer.from('junk-before'),
    Buffer.from(FUSE_SENTINEL),
    Buffer.from([version, states.length, ...states]),
    Buffer.from('junk-after'),
  ]);
}

function wireOf(states: number[], version = '1'): FuseWire {
  return { version, ...Object.fromEntries(states.map((s, i) => [String(i), s])) };
}

let tmp: string;
beforeEach(async () => {
  tmp = await mkdtemp(path.join(tmpdir(), 'verify-packaged-app-'));
});
afterEach(async () => {
  await rm(tmp, { recursive: true, force: true });
});

async function writeAt(file: string, content: string | Buffer): Promise<string> {
  await mkdir(path.dirname(file), { recursive: true });
  await writeFile(file, content);
  return file;
}

// ─── fuses: deriving the expectation from the config ────────────────────────────────────────

describe('deriveExpectedFuses', () => {
  it('derives every configured fuse, by name and index order, from the plugin config', () => {
    expect(deriveExpectedFuses(FUSES_CONFIG)).toEqual({
      version: '1',
      fuses: [
        { index: RUN_AS_NODE, name: 'RunAsNode', enabled: false },
        { index: COOKIE, name: 'EnableCookieEncryption', enabled: true },
        { index: INSPECT, name: 'EnableNodeCliInspectArguments', enabled: false },
        { index: ASAR_VALIDATION, name: 'EnableEmbeddedAsarIntegrityValidation', enabled: true },
        { index: ONLY_ASAR, name: 'OnlyLoadAppFromAsar', enabled: true },
      ],
    });
  });

  it('ignores the flip options and explicit undefined (inherit)', () => {
    const expected = deriveExpectedFuses({
      version: '1',
      strictlyRequireAllFuses: true,
      resetAdHocDarwinSignature: true,
      [RUN_AS_NODE]: undefined,
      [COOKIE]: true,
    });
    expect(expected.fuses.map((f) => f.name)).toEqual(['EnableCookieEncryption']);
  });

  it('names an index this fuses package does not know rather than dropping it', () => {
    expect(deriveExpectedFuses({ version: '1', 99: true }).fuses).toEqual([
      { index: 99, name: 'fuse#99', enabled: true },
    ]);
  });

  it.each([
    ['a missing version', { [COOKIE]: true }],
    ['a non-boolean value', { version: '1', [COOKIE]: 'yes' }],
    ['a key that is not a fuse index', { version: '1', someNewOption: true }],
  ])('refuses %s instead of guessing', (_label, config) => {
    expect(() => deriveExpectedFuses(config)).toThrow(CheckError);
  });

  it('refuses a config that sets no fuses, because that verifies nothing', () => {
    expect(() => deriveExpectedFuses({ version: '1', strictlyRequireAllFuses: true })).toThrow(
      /sets no fuses/
    );
  });
});

describe('findFusesConfig', () => {
  it('returns the fusesConfig of the plugin named fuses', () => {
    const config = { plugins: [{ name: 'other' }, { name: 'fuses', fusesConfig: FUSES_CONFIG }] };
    expect(findFusesConfig(config)).toBe(FUSES_CONFIG);
  });

  it.each([
    ['no plugins', {}],
    ['no fuses plugin', { plugins: [{ name: 'other' }, 'not-an-object', null] }],
    ['a fuses plugin without a config', { plugins: [{ name: 'fuses' }] }],
  ])('refuses %s', (_label, config) => {
    expect(() => findFusesConfig(config)).toThrow(CheckError);
  });
});

describe('the real forge.config.ts', () => {
  // The bridge between the config and [internal]rules/electron.md § Package and capture hardening,
  // which names exactly these five. packagingIdentity.test.ts pins the config object; this pins
  // what the verifier derives from it, so a plugin rename or a sixth fuse shows up here too.
  it('yields the five documented fuses through the verifier derivation', async () => {
    // The verifier's own loader, so this exercises the path CI takes.
    const expected = deriveExpectedFuses(findFusesConfig(await loadForgeConfig()));
    expect(expected.version).toBe('1');
    expect(expected.fuses.map(({ name, enabled }) => [name, enabled])).toEqual([
      ['RunAsNode', false],
      ['EnableCookieEncryption', true],
      ['EnableNodeCliInspectArguments', false],
      ['EnableEmbeddedAsarIntegrityValidation', true],
      ['OnlyLoadAppFromAsar', true],
    ]);
  });
});

// ─── fuses: judging a wire ──────────────────────────────────────────────────────────────────

describe('verifyFuseWire', () => {
  const expected = deriveExpectedFuses(FUSES_CONFIG);

  it('accepts a wire that carries every configured fuse, whatever the others are', () => {
    expect(verifyFuseWire(expected, wireOf(GOOD_STATES))).toEqual([]);
    const otherFusesFlipped = [...GOOD_STATES];
    otherFusesFlipped[2] = DISABLE; // EnableNodeOptionsEnvironmentVariable is not configured
    expect(verifyFuseWire(expected, wireOf(otherFusesFlipped))).toEqual([]);
  });

  it('names a configured fuse that shipped at the wrong value', () => {
    const states = [...GOOD_STATES];
    states[RUN_AS_NODE] = ENABLE;
    expect(verifyFuseWire(expected, wireOf(states))).toEqual([
      'RunAsNode is ENABLE in the packaged app but forge.config.ts sets DISABLE',
    ]);
  });

  it('reports every wrong fuse, not just the first', () => {
    const stock = [ENABLE, DISABLE, ENABLE, ENABLE, DISABLE, DISABLE, DISABLE, ENABLE, ENABLE];
    expect(verifyFuseWire(expected, wireOf(stock))).toHaveLength(5);
  });

  it('fails a configured fuse Electron has removed, since the value cannot take effect', () => {
    const states = [...GOOD_STATES];
    states[COOKIE] = REMOVED;
    expect(verifyFuseWire(expected, wireOf(states))).toEqual([
      'EnableCookieEncryption is marked REMOVED in this Electron build, so the configured ENABLE cannot take effect',
    ]);
  });

  it('fails a configured fuse beyond the end of the wire', () => {
    // A 3-byte wire holds indices 0-2; the inspect, asar-validation and only-asar fuses are past it.
    expect(verifyFuseWire(expected, wireOf(GOOD_STATES.slice(0, 3)))).toEqual([
      `EnableNodeCliInspectArguments (index ${INSPECT}) is beyond the end of the packaged fuse wire`,
      `EnableEmbeddedAsarIntegrityValidation (index ${ASAR_VALIDATION}) is beyond the end of the packaged fuse wire`,
      `OnlyLoadAppFromAsar (index ${ONLY_ASAR}) is beyond the end of the packaged fuse wire`,
    ]);
  });

  it('fails a wire of another version', () => {
    expect(verifyFuseWire(expected, wireOf(GOOD_STATES, '2'))[0]).toContain('version 2');
  });

  it('fails when the config configures nothing, because that verifies nothing', () => {
    const violations = verifyFuseWire({ version: '1', fuses: [] }, wireOf(GOOD_STATES));
    expect(violations).toEqual([
      'the FusesPlugin config sets no fuses, so there is nothing to verify',
    ]);
  });
});

describe('describeFuseWire', () => {
  it('lists every fuse in the wire and says which ones the config does not control', () => {
    const lines = describeFuseWire(deriveExpectedFuses(FUSES_CONFIG), wireOf(GOOD_STATES));
    expect(lines).toHaveLength(GOOD_STATES.length);
    expect(lines[0]).toMatch(/^RunAsNode\s+DISABLE\s+forge\.config\.ts sets DISABLE$/);
    expect(lines[2]).toMatch(/^EnableNodeOptionsEnvironmentVariable\s+ENABLE\s+not configured/);
    expect(
      describeFuseWire(deriveExpectedFuses(FUSES_CONFIG), { version: '1', 3: 0x7a })[0]
    ).toMatch(/0x7a/);
  });
});

describe('the real @electron/fuses reader on a produced binary', () => {
  // These run the installed library, so a Forge/fuses bump that changes the wire format or the
  // `.app` path mapping fails here before it can make the CI check vacuous.
  const expected = deriveExpectedFuses(FUSES_CONFIG);

  it('passes a binary whose wire matches the config', async () => {
    const exe = await writeAt(path.join(tmp, 'concord-voice'), fuseBinary(GOOD_STATES));
    const wire = (await getCurrentFuseWire(exe)) as unknown as FuseWire;
    expect(verifyFuseWire(expected, wire)).toEqual([]);
  });

  it('fails the same binary once one fuse is flipped back to the stock value', async () => {
    const states = [...GOOD_STATES];
    states[RUN_AS_NODE] = ENABLE;
    const exe = await writeAt(path.join(tmp, 'concord-voice'), fuseBinary(states));
    const wire = (await getCurrentFuseWire(exe)) as unknown as FuseWire;
    expect(verifyFuseWire(expected, wire)).toEqual([
      'RunAsNode is ENABLE in the packaged app but forge.config.ts sets DISABLE',
    ]);
  });

  it('reads a macOS bundle through the framework path', async () => {
    const app = path.join(tmp, 'Concord Voice.app');
    await writeAt(
      path.join(
        app,
        'Contents',
        'Frameworks',
        'Electron Framework.framework',
        'Electron Framework'
      ),
      fuseBinary(GOOD_STATES)
    );
    const wire = (await getCurrentFuseWire(app)) as unknown as FuseWire;
    expect(verifyFuseWire(expected, wire)).toEqual([]);
  });

  it('rejects a binary with no sentinel at all', async () => {
    const exe = await writeAt(path.join(tmp, 'plain'), 'not an electron binary');
    await expect(getCurrentFuseWire(exe)).rejects.toThrow(/sentinel/);
  });
});

// getCurrentFuseWire returns only the first wire in a file, so the cases below put two wires in one
// file: the flipped one and a stock one. A real fat (universal) Mach-O never gets this far, because
// it is refused before any wire is read; see "a fat (universal) Mach-O is refused".
const STOCK_STATES = [ENABLE, DISABLE, ENABLE, ENABLE, DISABLE, DISABLE, DISABLE, ENABLE, ENABLE];
const multiWireBinary = (...wires: number[][]) =>
  Buffer.concat(wires.map((states) => fuseBinary(states)));
// A file that opens with a given magic, which is all the fat-binary refusal looks at.
const withMagic = (magic: string, body: Buffer) =>
  Buffer.concat([Buffer.from(magic, 'hex'), Buffer.alloc(28), body]);

describe('scanFuseWires', () => {
  it('finds the wire and the bytes in it', async () => {
    const exe = await writeAt(path.join(tmp, 'exe'), fuseBinary(GOOD_STATES));
    const hits = await scanFuseWires(exe);
    expect(hits).toHaveLength(1);
    expect([...(hits[0].bytes ?? [])]).toEqual([1, GOOD_STATES.length, ...GOOD_STATES]);
  });

  it('finds every wire when a binary carries several, in file order', async () => {
    const exe = await writeAt(path.join(tmp, 'exe'), multiWireBinary(GOOD_STATES, STOCK_STATES));
    const hits = await scanFuseWires(exe);
    expect(hits.map((h) => h.bytes?.at(2))).toEqual([GOOD_STATES[0], STOCK_STATES[0]]);
    expect(hits[0].offset).toBeLessThan(hits[1].offset);
  });

  it('finds a wire whose sentinel straddles a chunk boundary, once', async () => {
    const exe = await writeAt(path.join(tmp, 'exe'), fuseBinary(GOOD_STATES));
    // junk-before is 11 bytes and the sentinel 32, so a 16-byte chunk cuts it in two.
    const hits = await scanFuseWires(exe, 16);
    expect(hits).toHaveLength(1);
    expect([...(hits[0].bytes ?? [])]).toEqual([1, GOOD_STATES.length, ...GOOD_STATES]);
  });

  it('reports a wire cut off by the end of the file instead of reading past it', async () => {
    const cut = Buffer.concat([Buffer.from(FUSE_SENTINEL), Buffer.from([1, 9, DISABLE, ENABLE])]);
    const exe = await writeAt(path.join(tmp, 'exe'), cut);
    expect((await scanFuseWires(exe))[0].bytes).toBeNull();
  });
});

describe('readPackagedFuseWire', () => {
  const expected = deriveExpectedFuses(FUSES_CONFIG);

  it('returns the wire of a thin binary', async () => {
    const exe = await writeAt(path.join(tmp, 'concord-voice'), fuseBinary(GOOD_STATES));
    expect(verifyFuseWire(expected, await readPackagedFuseWire(exe))).toEqual([]);
  });

  it('accepts a binary whose repeated wires agree', async () => {
    const exe = await writeAt(
      path.join(tmp, 'concord-voice'),
      multiWireBinary(GOOD_STATES, GOOD_STATES)
    );
    expect(verifyFuseWire(expected, await readPackagedFuseWire(exe))).toEqual([]);
  });

  it.each([
    ['second', [GOOD_STATES, STOCK_STATES]],
    ['first', [STOCK_STATES, GOOD_STATES]],
  ])(
    'refuses a binary whose repeated wire differs (the %s one is stock)',
    async (_where, wires) => {
      const exe = await writeAt(path.join(tmp, 'concord-voice'), multiWireBinary(...wires));
      await expect(readPackagedFuseWire(exe)).rejects.toThrow(/carries 2 fuse wires.*differ/);
    }
  );

  it('reads a macOS bundle through its framework', async () => {
    const app = path.join(tmp, 'Concord Voice.app');
    await writeAt(
      path.join(
        app,
        'Contents',
        'Frameworks',
        'Electron Framework.framework',
        'Electron Framework'
      ),
      multiWireBinary(GOOD_STATES, STOCK_STATES)
    );
    await expect(readPackagedFuseWire(app)).rejects.toThrow(/Electron Framework.*differ/);
  });

  it('refuses a wire the scan cannot complete, since there is nothing to compare', async () => {
    const cut = Buffer.concat([Buffer.from(FUSE_SENTINEL), Buffer.from([1, 9, DISABLE, ENABLE])]);
    const exe = await writeAt(path.join(tmp, 'concord-voice'), cut);
    await expect(readPackagedFuseWire(exe)).rejects.toThrow(/cannot cross-check the fuse wire/);
  });
});

// ─── locating the packaged app ──────────────────────────────────────────────────────────────

describe('locatePackagedApp', () => {
  it('finds the .app bundle of a darwin package and ignores make output', async () => {
    await mkdir(path.join(tmp, 'Concord Voice-darwin-arm64', 'Concord Voice.app'), {
      recursive: true,
    });
    await mkdir(path.join(tmp, 'make'));
    await mkdir(path.join(tmp, 'make-results'));
    const app = await locatePackagedApp(tmp);
    expect(app.platform).toBe('darwin');
    expect(app.target).toBe(path.join(tmp, 'Concord Voice-darwin-arm64', 'Concord Voice.app'));
  });

  it('finds the .exe of a win32 package and the bare executable of a linux package', async () => {
    const win = path.join(tmp, 'win');
    await writeAt(path.join(win, 'Concord Voice-win32-x64', 'Concord Voice.exe'), 'x');
    expect((await locatePackagedApp(win, 'Concord Voice')).target).toBe(
      path.join(win, 'Concord Voice-win32-x64', 'Concord Voice.exe')
    );
    const linux = path.join(tmp, 'linux');
    await writeAt(path.join(linux, 'Concord Voice-linux-arm64', 'concord-voice'), 'x');
    expect((await locatePackagedApp(linux, 'concord-voice')).target).toBe(
      path.join(linux, 'Concord Voice-linux-arm64', 'concord-voice')
    );
  });

  it('refuses zero packaged directories and refuses two', async () => {
    await mkdir(path.join(tmp, 'make'));
    await expect(locatePackagedApp(tmp)).rejects.toThrow(
      /exactly 1 packaged app directory.*\(none\)/
    );
    await mkdir(path.join(tmp, 'Concord Voice-darwin-arm64'));
    await mkdir(path.join(tmp, 'Concord Voice-linux-x64'));
    await expect(locatePackagedApp(tmp)).rejects.toThrow(/found 2/);
  });

  it('refuses a darwin package without exactly one .app', async () => {
    await mkdir(path.join(tmp, 'Concord Voice-darwin-x64'));
    await expect(locatePackagedApp(tmp)).rejects.toThrow(/exactly 1 \.app bundle/);
  });

  it('treats a mas package like a darwin one', async () => {
    await mkdir(path.join(tmp, 'Concord Voice-mas-arm64', 'Concord Voice.app'), {
      recursive: true,
    });
    const app = await locatePackagedApp(tmp);
    expect(app.platform).toBe('mas');
    expect(app.target).toBe(path.join(tmp, 'Concord Voice-mas-arm64', 'Concord Voice.app'));
  });

  it('refuses a universal build: no leg produces one and its layout is unchecked', async () => {
    await mkdir(path.join(tmp, 'Concord Voice-darwin-universal', 'Concord Voice.app'), {
      recursive: true,
    });
    await expect(locatePackagedApp(tmp)).rejects.toThrow(
      /exactly 1 packaged app directory.*\(none\)/
    );
  });

  it('refuses a win32 or linux package when the executable is unknown, missing or a directory', async () => {
    await mkdir(path.join(tmp, 'Concord Voice-win32-x64'));
    await expect(locatePackagedApp(tmp)).rejects.toThrow(/executableName/);
    await expect(locatePackagedApp(tmp, 'Concord Voice')).rejects.toThrow(/not found/);
    await mkdir(path.join(tmp, 'Concord Voice-win32-x64', 'Concord Voice.exe'));
    await expect(locatePackagedApp(tmp, 'Concord Voice')).rejects.toThrow(/not found/);
  });
});

// ─── asar-digest ────────────────────────────────────────────────────────────────────────────

const INTEGRITY: AsarIntegrity = {
  'Resources/app.asar': { algorithm: 'SHA256', hash: 'ab'.repeat(32) },
};
const DIGEST = computeIntegrityDigest(INTEGRITY);

function slotBytes(used: number, version: number, digest: Buffer): Buffer {
  return Buffer.concat([Buffer.from([used, version]), digest]);
}

function digestBinary(slot: Buffer, pad = 100): Buffer {
  return Buffer.concat([
    Buffer.alloc(pad, 0x41),
    Buffer.from(INTEGRITY_DIGEST_SENTINEL),
    slot,
    Buffer.alloc(pad, 0x42),
  ]);
}

describe('computeIntegrityDigest', () => {
  // Expected values come from `shasum -a 256`, an implementation independent of this code.
  it('hashes key, algorithm and hash in turn', () => {
    expect(
      computeIntegrityDigest({
        'Resources/app.asar': { algorithm: 'SHA256', hash: 'abcd' },
      }).toString('hex')
    ).toBe('abc9a3e8ca2cb964769c8871aeb6145b6c3d752ecde0c0f3ba5f8d7c0c6ae9d8'); // pragma: allowlist secret  (public shasum test vector)
  });

  it('walks the keys in sorted order whatever order they were inserted in', () => {
    const entries = {
      'b-key': { algorithm: 'SHA256', hash: 'h2' },
      'a-key': { algorithm: 'SHA512', hash: 'h1' },
    };
    expect(computeIntegrityDigest(entries).toString('hex')).toBe(
      'b7e2d42db3408b05c002fbb01f7125601daa56c2d286881f996e83393b408fce' // pragma: allowlist secret  (public shasum test vector)
    );
    expect(computeIntegrityDigest(entries).toString('hex')).not.toBe(
      '185bae23b27ac1b76a9dcf110a7c7a5879793f6305f476fa3deb0cd9548324ce' // pragma: allowlist secret  (public shasum test vector)
    );
  });

  it('changes when the hash or the algorithm changes', () => {
    const base = computeIntegrityDigest(INTEGRITY);
    const hash = { 'Resources/app.asar': { algorithm: 'SHA256', hash: 'cd'.repeat(32) } };
    const algorithm = { 'Resources/app.asar': { algorithm: 'SHA512', hash: 'ab'.repeat(32) } };
    expect(computeIntegrityDigest(hash).equals(base)).toBe(false);
    expect(computeIntegrityDigest(algorithm).equals(base)).toBe(false);
  });
});

describe('isAsarIntegrity', () => {
  it('accepts a non-empty map of { algorithm, hash }', () => {
    expect(isAsarIntegrity(INTEGRITY)).toBe(true);
  });

  it.each([
    ['null', null],
    ['a string', 'x'],
    ['an empty object', {}],
    ['an array', []],
    ['an entry that is not an object', { a: 'x' }],
    ['an entry without a hash', { a: { algorithm: 'SHA256' } }],
    ['an entry with an empty algorithm', { a: { algorithm: '', hash: 'h' } }],
    ['an entry with a non-string hash', { a: { algorithm: 'SHA256', hash: 7 } }],
    ['one bad entry among good ones', { ...INTEGRITY, b: { algorithm: 'SHA256', hash: '' } }],
  ])('rejects %s, as the packager does before it skips the write', (_label, value) => {
    expect(isAsarIntegrity(value)).toBe(false);
  });
});

describe('scanDigestSlots', () => {
  it('reads the slot behind the sentinel', async () => {
    const file = await writeAt(path.join(tmp, 'fw'), digestBinary(slotBytes(1, 1, DIGEST)));
    const [slot, ...rest] = await scanDigestSlots(file);
    expect(rest).toEqual([]);
    expect(slot).toMatchObject({ complete: true, used: 1, version: 1 });
    expect(slot.digest.equals(DIGEST)).toBe(true);
    expect(slot.offset).toBe(100 + INTEGRITY_DIGEST_SENTINEL.length);
  });

  it('finds a sentinel that straddles a read-chunk boundary exactly once', async () => {
    // pad 60 + a 32-byte sentinel crosses a 64-byte chunk edge.
    const file = await writeAt(path.join(tmp, 'fw'), digestBinary(slotBytes(1, 1, DIGEST), 60));
    expect(await scanDigestSlots(file, 64)).toHaveLength(1);
  });

  it('returns every slot, so a half-patched binary cannot hide one', async () => {
    const good = digestBinary(slotBytes(1, 1, DIGEST));
    const file = await writeAt(
      path.join(tmp, 'fw'),
      Buffer.concat([good, digestBinary(slotBytes(0, 0, Buffer.alloc(32)))])
    );
    const slots = await scanDigestSlots(file);
    expect(slots.map((s) => s.used)).toEqual([1, 0]);
  });

  it('marks a slot cut off by the end of the file', async () => {
    const file = await writeAt(
      path.join(tmp, 'fw'),
      Buffer.concat([Buffer.from(INTEGRITY_DIGEST_SENTINEL), Buffer.from([1, 1, 9, 9])])
    );
    expect((await scanDigestSlots(file))[0].complete).toBe(false);
  });

  it('returns nothing for a binary with no sentinel', async () => {
    const file = await writeAt(path.join(tmp, 'fw'), Buffer.alloc(300, 0x41));
    expect(await scanDigestSlots(file)).toEqual([]);
  });
});

describe('a fat (universal) Mach-O is refused, since its slices are not read one by one', () => {
  // Reproduced on a real lipo-built framework: with one slice's sentinel destroyed, the marker in
  // the other slice made both modes exit 0 while Apple Silicon ran the slice that had none.
  it.each([
    ['cafebabe', 'FAT_MAGIC'],
    ['cafebabf', 'FAT_MAGIC_64'],
  ])('refuses a file that opens with %s (%s), in both scans', async (magic) => {
    const fuse = await writeAt(path.join(tmp, 'fuse'), withMagic(magic, fuseBinary(GOOD_STATES)));
    const digest = await writeAt(
      path.join(tmp, 'digest'),
      withMagic(magic, digestBinary(slotBytes(1, 1, DIGEST)))
    );
    await expect(scanFuseWires(fuse)).rejects.toThrow(CheckError);
    await expect(scanFuseWires(fuse)).rejects.toThrow(/fat \(universal\) Mach-O/);
    await expect(scanDigestSlots(digest)).rejects.toThrow(CheckError);
    await expect(scanDigestSlots(digest)).rejects.toThrow(/fat \(universal\) Mach-O/);
  });

  it.each([
    ['a thin 64-bit Mach-O', 'cffaedfe'],
    ['a thin 32-bit Mach-O', 'cefaedfe'],
    ['a Windows executable', '4d5a9000'],
    ['a Linux executable', '7f454c46'],
  ])('still reads %s', async (_label, magic) => {
    const fuse = await writeAt(path.join(tmp, 'fuse'), withMagic(magic, fuseBinary(GOOD_STATES)));
    const digest = await writeAt(
      path.join(tmp, 'digest'),
      withMagic(magic, digestBinary(slotBytes(1, 1, DIGEST)))
    );
    expect(await scanFuseWires(fuse)).toHaveLength(1);
    expect(await scanDigestSlots(digest)).toHaveLength(1);
  });

  it('looks only at the first four bytes, since the same bytes occur inside ordinary binaries', async () => {
    const inside = Buffer.concat([
      Buffer.alloc(64, 0x41),
      Buffer.from('cafebabe', 'hex'),
      fuseBinary(GOOD_STATES),
    ]);
    expect(await scanFuseWires(await writeAt(path.join(tmp, 'fuse'), inside))).toHaveLength(1);
  });

  it('treats a file shorter than the magic as not fat instead of failing', async () => {
    expect(
      await scanFuseWires(await writeAt(path.join(tmp, 'tiny'), Buffer.from('ca', 'hex')))
    ).toEqual([]);
  });
});

describe('verifyDigestSlots', () => {
  const slot = (used: number, version: number, digest: Buffer, complete = true) => ({
    offset: 7,
    complete,
    used,
    version,
    digest,
  });

  it('accepts slots that all hold the expected digest', () => {
    expect(verifyDigestSlots([slot(1, 1, DIGEST), slot(1, 1, DIGEST)], DIGEST)).toEqual([]);
  });

  it('fails closed when there is no sentinel', () => {
    const [violation, ...rest] = verifyDigestSlots([], DIGEST);
    expect(rest).toEqual([]);
    expect(violation).toMatch(/no integrity-digest sentinel.*only warns/);
  });

  it('fails an unused slot, which Electron treats as nothing to enforce', () => {
    expect(verifyDigestSlots([slot(0, 0, Buffer.alloc(32))], DIGEST)).toEqual([
      'digest slot at offset 7 is unused (used=0): Electron fails open on it',
    ]);
  });

  it('fails a slot of another version, a truncated slot and a digest mismatch', () => {
    expect(verifyDigestSlots([slot(1, 2, DIGEST)], DIGEST)[0]).toContain('version 2');
    expect(verifyDigestSlots([slot(1, 1, DIGEST, false)], DIGEST)[0]).toContain('truncated');
    const other = Buffer.from(DIGEST);
    other[0] ^= 0xff;
    const [mismatch] = verifyDigestSlots([slot(1, 1, other)], DIGEST);
    expect(mismatch).toContain(other.toString('hex'));
    expect(mismatch).toContain(DIGEST.toString('hex'));
  });

  it('reports only the slot that is wrong', () => {
    expect(
      verifyDigestSlots([slot(1, 1, DIGEST), slot(0, 0, Buffer.alloc(32))], DIGEST)
    ).toHaveLength(1);
  });
});

describe('readAsarIntegrity', () => {
  const plist = '/x/Info.plist';

  it('parses what plutil prints, calling the system plutil with the path after --', () => {
    const calls: Array<[string, string[]]> = [];
    const result = readAsarIntegrity(plist, (file, args) => {
      calls.push([file, args]);
      return JSON.stringify(INTEGRITY);
    });
    expect(result).toEqual(INTEGRITY);
    expect(calls).toEqual([
      ['/usr/bin/plutil', ['-extract', 'ElectronAsarIntegrity', 'json', '-o', '-', '--', plist]],
    ]);
  });

  it('reports an absent key as a problem with the artifact, quoting plutil', () => {
    expect(() =>
      readAsarIntegrity(plist, () => {
        throw Object.assign(new Error('exit 1'), { stderr: 'No value at that key path\nmore' });
      })
    ).toThrow(/no readable ElectronAsarIntegrity: No value at that key path$/);
  });

  it.each(['ENOENT', 'EACCES'])(
    'reports a plutil that could not be run (%s) as a check that could not run',
    (code) => {
      const run = () => {
        throw Object.assign(new Error('spawn'), { code });
      };
      expect(() => readAsarIntegrity(plist, run)).toThrow(CheckError);
      expect(() => readAsarIntegrity(plist, run)).toThrow(`could not be run (${code})`);
    }
  );

  it('falls back to the error text when plutil printed nothing on stderr', () => {
    expect(() =>
      readAsarIntegrity(plist, () => {
        throw Object.assign(new Error('exit 1'), { stderr: '' });
      })
    ).toThrow(/no readable ElectronAsarIntegrity: Error: exit 1$/);
  });

  it('does not accept output that is not JSON', () => {
    expect(() => readAsarIntegrity(plist, () => 'not json')).toThrow(SyntaxError);
  });

  it('rejects an entry the packager would have skipped', () => {
    expect(() => readAsarIntegrity(plist, () => '{}')).toThrow(/non-empty map/);
  });
});

describe('verifyAsarDigest', () => {
  async function bundle(frameworkBytes?: Buffer): Promise<string> {
    const app = path.join(tmp, 'Concord Voice.app');
    if (frameworkBytes) {
      await writeAt(
        path.join(
          app,
          'Contents',
          'Frameworks',
          'Electron Framework.framework',
          'Electron Framework'
        ),
        frameworkBytes
      );
    }
    return app;
  }
  const readOk = () => INTEGRITY;

  it('passes a framework whose slot holds the digest of the plist entries', async () => {
    const app = await bundle(digestBinary(slotBytes(1, 1, DIGEST)));
    expect(await verifyAsarDigest(app, readOk)).toEqual([]);
  });

  it('reads the plist at Contents/Info.plist', async () => {
    const app = await bundle(digestBinary(slotBytes(1, 1, DIGEST)));
    const seen: string[] = [];
    await verifyAsarDigest(app, (p) => {
      seen.push(p);
      return INTEGRITY;
    });
    expect(seen).toEqual([path.join(app, 'Contents', 'Info.plist')]);
  });

  it('fails a zeroed slot, a flipped digest byte and a missing sentinel', async () => {
    const flipped = Buffer.from(DIGEST);
    flipped[31] ^= 0x01;
    const cases: Array<[string, Buffer, RegExp]> = [
      ['zeroed slot', digestBinary(Buffer.alloc(34)), /unused/],
      ['flipped digest byte', digestBinary(slotBytes(1, 1, flipped)), /holds .* but SHA-256/],
      ['no sentinel', Buffer.alloc(300, 0x41), /no integrity-digest sentinel/],
    ];
    for (const [label, bytes, pattern] of cases) {
      await rm(path.join(tmp, 'Concord Voice.app'), { recursive: true, force: true });
      const app = await bundle(bytes);
      const violations = await verifyAsarDigest(app, readOk);
      expect(violations, label).toHaveLength(1);
      expect(violations[0], label).toMatch(pattern);
    }
  });

  it('fails when the plist entries no longer match the digest the framework carries', async () => {
    const app = await bundle(digestBinary(slotBytes(1, 1, DIGEST)));
    const tampered = { 'Resources/app.asar': { algorithm: 'SHA256', hash: 'cd'.repeat(32) } };
    expect(await verifyAsarDigest(app, () => tampered)).toHaveLength(1);
  });

  it('treats an unreadable plist or an absent framework as a finding about the artifact', async () => {
    const withFramework = await bundle(digestBinary(slotBytes(1, 1, DIGEST)));
    const unreadable = await verifyAsarDigest(withFramework, () => {
      throw new Error('no readable ElectronAsarIntegrity');
    });
    expect(unreadable).toEqual(['no readable ElectronAsarIntegrity']);
    await rm(withFramework, { recursive: true, force: true });
    const [missing] = await verifyAsarDigest(await bundle(), readOk);
    expect(missing).toMatch(/cannot read .*Electron Framework/);
  });

  it('lets a check that could not run propagate instead of reporting a pass', async () => {
    const app = await bundle(digestBinary(slotBytes(1, 1, DIGEST)));
    await expect(
      verifyAsarDigest(app, () => {
        throw new CheckError('plutil missing');
      })
    ).rejects.toThrow(CheckError);
  });

  it('reports plutil output that is not JSON as one finding about the artifact', async () => {
    const app = await bundle(digestBinary(slotBytes(1, 1, DIGEST)));
    const findings = await verifyAsarDigest(app, (p) => readAsarIntegrity(p, () => 'not json'));
    expect(findings).toHaveLength(1);
  });
});

// ─── CLI ────────────────────────────────────────────────────────────────────────────────────

describe('main', () => {
  function makeDeps(overrides: Partial<Deps> = {}): { deps: Deps; lines: string[] } {
    const lines: string[] = [];
    const deps: Deps = {
      loadForgeConfig: async () => ({
        plugins: [{ name: 'fuses', fusesConfig: FUSES_CONFIG }],
        packagerConfig: { executableName: 'Concord Voice' },
      }),
      readFuseWire: async () => wireOf(GOOD_STATES),
      readIntegrity: readOkIntegrity,
      platform: 'darwin',
      log: (line) => lines.push(line),
      ...overrides,
    };
    return { deps, lines };
  }
  function readOkIntegrity(): AsarIntegrity {
    return INTEGRITY;
  }
  async function darwinOut(frameworkBytes?: Buffer): Promise<string> {
    const out = path.join(tmp, 'out');
    const app = path.join(out, 'Concord Voice-darwin-arm64', 'Concord Voice.app');
    await mkdir(app, { recursive: true });
    if (frameworkBytes) {
      await writeAt(
        path.join(
          app,
          'Contents',
          'Frameworks',
          'Electron Framework.framework',
          'Electron Framework'
        ),
        frameworkBytes
      );
    }
    return out;
  }

  it.each([
    ['no mode', []],
    ['an unknown mode', ['sign']],
    ['an unknown flag', ['fuses', '--output', 'out']],
    ['a flag without a value', ['fuses', '--out']],
  ])('exits 2 with the usage line for %s', async (_label, argv) => {
    const { deps, lines } = makeDeps();
    expect(await main(argv, deps)).toBe(2);
    expect(lines.join('\n')).toContain('::error::verify-packaged-app could not run: usage:');
  });

  it('fuses: exits 0 and prints the table when the packaged wire matches', async () => {
    const out = await darwinOut();
    const { deps, lines } = makeDeps();
    expect(await main(['fuses', '--out', out], deps)).toBe(0);
    expect(lines.some((l) => l.includes('RunAsNode') && l.includes('DISABLE'))).toBe(true);
    expect(lines.at(-1)).toBe(
      '::notice::Packaged fuses match forge.config.ts (5 configured fuses verified)'
    );
  });

  it('fuses: exits 1 and names the fuse when one shipped at the wrong value', async () => {
    const out = await darwinOut();
    const states = [...GOOD_STATES];
    states[ONLY_ASAR] = DISABLE;
    const { deps, lines } = makeDeps({ readFuseWire: async () => wireOf(states) });
    expect(await main(['fuses', '--out', out], deps)).toBe(1);
    expect(lines).toContain(
      '::error::OnlyLoadAppFromAsar is DISABLE in the packaged app but forge.config.ts sets ENABLE'
    );
  });

  it('fuses: exits 2 when the app, the config or the plugin cannot be found', async () => {
    const missingOut = makeDeps();
    expect(await main(['fuses', '--out', path.join(tmp, 'nope')], missingOut.deps)).toBe(2);
    const out = await darwinOut();
    const noPlugin = makeDeps({ loadForgeConfig: async () => ({ plugins: [] }) });
    expect(await main(['fuses', '--out', out], noPlugin.deps)).toBe(2);
    expect(noPlugin.lines.join('\n')).toContain('no FusesPlugin');
    const noFuses = makeDeps({
      loadForgeConfig: async () => ({
        plugins: [{ name: 'fuses', fusesConfig: { version: '1' } }],
      }),
    });
    expect(await main(['fuses', '--out', out], noFuses.deps)).toBe(2);
    expect(noFuses.lines.join('\n')).toContain('sets no fuses');
  });

  it('fuses: exits 1 when the fuse wire cannot be read, as asar-digest does for its framework', async () => {
    const out = await darwinOut();
    const { deps, lines } = makeDeps({
      readFuseWire: async () => {
        throw new Error('Could not find a fuse wire');
      },
    });
    expect(await main(['fuses', '--out', out], deps)).toBe(1);
    expect(
      lines.some((l) =>
        /^::error::cannot read the fuse wire from .*Concord Voice\.app: Could not find a fuse wire$/.test(
          l
        )
      )
    ).toBe(true);
  });

  it('fuses: with the real reader, a framework whose second wire is stock exits 1', async () => {
    const out = await darwinOut(multiWireBinary(GOOD_STATES, STOCK_STATES));
    const { deps, lines } = makeDeps({ readFuseWire: readPackagedFuseWire });
    expect(await main(['fuses', '--out', out], deps)).toBe(1);
    expect(
      lines.some((l) =>
        /^::error::cannot read the fuse wire from .*: .*carries 2 fuse wires/.test(l)
      )
    ).toBe(true);
  });

  it('fuses: with the real reader, a framework whose repeated wires agree exits 0', async () => {
    const out = await darwinOut(multiWireBinary(GOOD_STATES, GOOD_STATES));
    const { deps } = makeDeps({ readFuseWire: readPackagedFuseWire });
    expect(await main(['fuses', '--out', out], deps)).toBe(0);
  });

  it('fuses: with the real reader, a fat framework exits 2 even when every slice holds the right wire', async () => {
    const out = await darwinOut(withMagic('cafebabe', multiWireBinary(GOOD_STATES, GOOD_STATES)));
    const { deps, lines } = makeDeps({ readFuseWire: readPackagedFuseWire });
    expect(await main(['fuses', '--out', out], deps)).toBe(2);
    expect(lines.join('\n')).toContain('could not run');
    expect(lines.join('\n')).toContain('fat (universal) Mach-O');
  });

  it('fuses: with the real reader, a fat framework holding no wire at all is exit 2, not a read failure', async () => {
    // The refusal has to come before the library is asked for a wire: asked first, it would throw
    // "no sentinel" and the job would see an exit-1 finding about a binary it cannot read at all.
    const out = await darwinOut(withMagic('cafebabe', Buffer.alloc(100, 0x41)));
    const { deps, lines } = makeDeps({ readFuseWire: readPackagedFuseWire });
    expect(await main(['fuses', '--out', out], deps)).toBe(2);
    expect(lines.join('\n')).toContain('fat (universal) Mach-O');
  });

  it('fuses: a reader that reports the check could not run still exits 2', async () => {
    const out = await darwinOut();
    const { deps } = makeDeps({
      readFuseWire: async () => {
        throw new CheckError('cannot run');
      },
    });
    expect(await main(['fuses', '--out', out], deps)).toBe(2);
  });

  it('asar-digest: exits 0 for a matching framework and 1 for a zeroed slot', async () => {
    const ok = makeDeps();
    const out = await darwinOut(digestBinary(slotBytes(1, 1, DIGEST)));
    expect(await main(['asar-digest', '--out', out], ok.deps)).toBe(0);
    expect(ok.lines.at(-1)).toMatch(/^::notice::Asar integrity digest/);

    await rm(out, { recursive: true, force: true });
    const zeroed = makeDeps();
    const brokenOut = await darwinOut(digestBinary(Buffer.alloc(34)));
    expect(await main(['asar-digest', '--out', brokenOut], zeroed.deps)).toBe(1);
    expect(zeroed.lines.some((l) => l.startsWith('::error::') && l.includes('unused'))).toBe(true);
  });

  it('asar-digest: a fat framework exits 2, not the exit 1 of a finding', async () => {
    const out = await darwinOut(withMagic('cafebabe', digestBinary(slotBytes(1, 1, DIGEST))));
    const { deps, lines } = makeDeps();
    expect(await main(['asar-digest', '--out', out], deps)).toBe(2);
    expect(lines.join('\n')).toContain('could not run');
    expect(lines.join('\n')).toContain('fat (universal) Mach-O');
  });

  it('asar-digest: refuses to run anywhere but macOS instead of passing', async () => {
    const { deps, lines } = makeDeps({ platform: 'linux' });
    expect(await main(['asar-digest', '--out', await darwinOut()], deps)).toBe(2);
    expect(lines.join('\n')).toContain('only runs on macOS');
  });

  it('asar-digest: exits 2 when plutil is missing, because nothing was verified', async () => {
    const out = await darwinOut(digestBinary(slotBytes(1, 1, DIGEST)));
    const { deps } = makeDeps({
      readIntegrity: () => {
        throw new CheckError('plutil missing');
      },
    });
    expect(await main(['asar-digest', '--out', out], deps)).toBe(2);
  });

  it('keeps a finding that contains newlines on one ::error:: line', async () => {
    // A plutil stderr line or an error message can carry a newline. Left alone, its second line
    // would be read by the runner as a workflow command of its own.
    const out = await darwinOut(digestBinary(slotBytes(1, 1, DIGEST)));
    const { deps, lines } = makeDeps({
      readIntegrity: () => {
        throw new Error('boom\n::notice::forged');
      },
    });
    expect(await main(['asar-digest', '--out', out], deps)).toBe(1);
    expect(lines.filter((l) => l.startsWith('::error::'))).toEqual([
      '::error::boom ::notice::forged',
    ]);
    expect(lines.some((l) => l.startsWith('::notice::'))).toBe(false);
  });

  it('prints the frames of an unexpected error, but none for a check that could not run', async () => {
    const unexpected = makeDeps({
      loadForgeConfig: async () => {
        throw new TypeError('plugin shape changed\n::notice::forged');
      },
    });
    expect(await main(['fuses', '--out', await darwinOut()], unexpected.deps)).toBe(2);
    expect(unexpected.lines[0]).toBe(
      '::error::verify-packaged-app could not run: plugin shape changed'
    );
    expect(unexpected.lines.length).toBeGreaterThan(1);
    expect(unexpected.lines.slice(1).every((l) => /^\s+at /.test(l))).toBe(true);

    const usage = makeDeps();
    expect(await main([], usage.deps)).toBe(2);
    expect(usage.lines).toHaveLength(1);
  });
});

describe.runIf(process.platform === 'darwin')('readAsarIntegrity with the real plutil', () => {
  const xml = (inner: string) =>
    '<?xml version="1.0" encoding="UTF-8"?>\n' +
    '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n' +
    `<plist version="1.0"><dict><key>CFBundleName</key><string>x</string>${inner}</dict></plist>`;

  it('reads ElectronAsarIntegrity from a real XML plist', async () => {
    const plist = await writeAt(
      path.join(tmp, 'Info.plist'),
      xml(
        '<key>ElectronAsarIntegrity</key><dict><key>Resources/app.asar</key><dict>' +
          '<key>algorithm</key><string>SHA256</string><key>hash</key><string>abcd</string></dict></dict>'
      )
    );
    expect(readAsarIntegrity(plist)).toEqual({
      'Resources/app.asar': { algorithm: 'SHA256', hash: 'abcd' },
    });
  });

  it('reports a plist without the key as a problem with the artifact', async () => {
    const plist = await writeAt(path.join(tmp, 'Info.plist'), xml(''));
    expect(() => readAsarIntegrity(plist)).toThrow(/no readable ElectronAsarIntegrity/);
  });
});

// ─── CLI entry ──────────────────────────────────────────────────────────────────────────────

// verify-packaged-app.mts has no main-module guard on purpose: a verifier that can be skipped by
// a path mismatch exits 0 having checked nothing. These run the real file under tsx, as CI does.
describe('CLI entry', () => {
  const scripts = fileURLToPath(new URL('.', import.meta.url));
  const desktop = path.resolve(scripts, '..');
  // The timeout sits under the 30 s test limit so a hung child fails here, with a reason.
  const runEntry = (entry: string, args: string[] = []) => {
    const result = spawnSync(process.execPath, ['--import', 'tsx', entry, ...args], {
      cwd: desktop,
      encoding: 'utf8',
      timeout: 25_000,
    });
    // A spawn failure or a timeout leaves `status` null, which would read as a bare mismatch.
    if (result.error || result.signal) {
      throw new Error(
        `the entry did not run to completion: ${result.error?.message ?? result.signal}`
      );
    }
    return result;
  };

  it('runs main() when invoked: no arguments is a usage error, never a silent pass', () => {
    const result = runEntry(path.join(scripts, 'verify-packaged-app.mts'));
    expect(result.status).toBe(2);
    expect(result.stdout).toContain('::error::verify-packaged-app could not run: usage:');
  }, 30_000);

  it.skipIf(process.platform === 'win32')(
    'still runs main() when reached through a symlinked directory',
    async () => {
      // The case the old argv[1] === import.meta.url guard turned into a silent exit 0.
      const link = path.join(tmp, 'scripts-link');
      await symlink(scripts, link, 'dir');
      const result = runEntry(path.join(link, 'verify-packaged-app.mts'));
      expect(result.status).toBe(2);
      expect(result.stdout).toContain('usage:');
    },
    30_000
  );

  it('loads forge.config.ts under tsx and fails on a missing out directory', () => {
    // The production path end to end: entry -> lib -> default loader -> locatePackagedApp.
    const result = runEntry(path.join(scripts, 'verify-packaged-app.mts'), [
      'fuses',
      '--out',
      path.join(tmp, 'no-such-out'),
    ]);
    expect(result.status).toBe(2);
    expect(result.stdout).toContain('::error::verify-packaged-app could not run:');
    expect(result.stdout).toContain('no-such-out');
  }, 30_000);
});

// ─── workflow wiring ────────────────────────────────────────────────────────────────────────

// The public mirror publishes client/ but none of the private workflows parsed below, and its
// overlay adds a CI workflow of its own to that tree, which makes the overlay's file the marker.
// There the wiring tests have nothing to read, so they are skipped. Anywhere else a missing
// workflow must fail loudly: a wiring test that skips itself is the silent loss it exists to stop.
const WORKFLOWS = new URL('../../../.github/workflows/', import.meta.url);
const inPublicMirror = existsSync(new URL('public-ci-frontend.yml', WORKFLOWS));

describe('workflow wiring: the skip is confined to the public mirror', () => {
  it('never skips where the private workflows are present', () => {
    expect(existsSync(new URL('build-desktop.yml', WORKFLOWS)) && inPublicMirror).toBe(false);
  });
});

// The script only protects anything while the steps that run it stay in the two workflows, run
// every time, and fail the job. Deleting a step, adding `continue-on-error`, or skipping it on a
// leg are each a silent loss of the check, so the wiring is pinned here.
describe.skipIf(inPublicMirror)('workflow wiring', () => {
  interface Step {
    name?: string;
    if?: string;
    run?: string;
    shell?: string;
    'working-directory'?: string;
    'continue-on-error'?: unknown;
  }
  interface Job {
    'continue-on-error'?: unknown;
    steps: Step[];
  }
  const workflowJob = (file: string, jobId: string): Job => {
    const workflow = parseYaml(readFileSync(new URL(file, WORKFLOWS), 'utf8')) as {
      jobs: Record<string, Job>;
    };
    return workflow.jobs[jobId];
  };

  const ASAR_STEP = 'Verify asar integrity and preload bundle';
  const cases = [
    { file: 'desktop-pr-smoke.yml', job: 'pr-smoke-build', digestGuard: undefined },
    { file: 'build-desktop.yml', job: 'release-build', digestGuard: "matrix.platform == 'darwin'" },
  ];

  describe.each(cases)('$job in $file', ({ file, job, digestGuard }) => {
    // Read when the suite runs rather than when it is collected, so a skipped suite reads nothing.
    let steps: Step[];
    let jobFields: Omit<Job, 'steps'>;
    beforeAll(() => {
      ({ steps, ...jobFields } = workflowJob(file, job));
    });
    const stepRunning = (mode: string) =>
      steps.filter((s) => s.run?.includes(`verify-packaged-app.mts ${mode}`));

    it.each([
      ['fuses', undefined],
      ['asar-digest', digestGuard],
    ])('runs `%s` once, from client/desktop, and lets it fail the job', (mode, guard) => {
      const found = stepRunning(mode);
      expect(found).toHaveLength(1);
      expect(found[0].run?.trim()).toBe(
        `npx tsx scripts/verify-packaged-app.mts ${mode} --out out`
      );
      expect(found[0]['working-directory']).toBe('client/desktop');
      expect(found[0].shell).toBe('bash');
      expect(found[0].if).toBe(guard);
      expect(found[0]['continue-on-error']).toBeUndefined();
      expect(jobFields['continue-on-error']).toBeUndefined();
    });

    it('runs both after the asar payload verification, on the packaged output', () => {
      const index = (mode: string) => steps.findIndex((s) => s.run?.includes(mode));
      const asar = steps.findIndex((s) => s.name === ASAR_STEP);
      expect(asar).toBeGreaterThan(-1);
      expect(index('verify-packaged-app.mts fuses')).toBeGreaterThan(asar);
      expect(index('verify-packaged-app.mts asar-digest')).toBeGreaterThan(asar);
    });
  });
});
