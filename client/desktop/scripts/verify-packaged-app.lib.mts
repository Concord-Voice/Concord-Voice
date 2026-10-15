// Read back what the PRODUCED desktop app actually carries, not the config that produced it.
//
//   npx tsx scripts/verify-packaged-app.mts fuses       [--out out]
//   npx tsx scripts/verify-packaged-app.mts asar-digest [--out out]    (macOS only)
//
// Run from client/desktop after `electron-forge make|package`, from both
// .github/workflows/desktop-pr-smoke.yml (pr-smoke-build) and build-desktop.yml
// (release-build). Exit 0 = verified, 1 = the artifact is wrong, 2 = the check could not
// run (nothing was verified). Both non-zero codes fail the job.
//
// WHY THIS EXISTS. Two hardening layers fail OPEN, and nothing else reads the artifact:
//   * Fuses. FusesPlugin flips them in a packageAfterCopy hook. A hook that does not run, or a
//     Forge / @electron/fuses bump that changes where it writes, ships stock Electron while every
//     other check stays green. forge.config.ts proves what we ASKED for, not what shipped.
//   * Asar integrity digest. @electron/packager >= 20 writes a SHA-256 over the Info.plist's
//     ElectronAsarIntegrity entries into the Electron Framework binary, and only WARNS when it
//     cannot (no sentinel, unreadable plist, non-darwin host). Electron then treats an unwritten
//     slot as "no digest to enforce" (shell/common/asar/integrity_digest.mm returns true when
//     `slot.used` is false), so a tampered plist + asar pair launches.
// Same principle as verify-asar-payload.sh: assert on the produced artifact.
//
// This file is the library: everything is exported for scripts/verify-packaged-app.test.ts and
// nothing runs on import. The CLI is verify-packaged-app.mts, whose only job is to call main().

import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { open, readdir, stat } from 'node:fs/promises';
import path from 'node:path';
import { FuseV1Options, getCurrentFuseWire } from '@electron/fuses';

/** The check could not run (as opposed to the artifact failing it): exit 2. */
export class CheckError extends Error {}

// ─── Shared: locating the packaged app ──────────────────────────────────────────────────────

// @electron/packager names its output `<name>-<platform>-<arch>`; `out/` also holds make/ and
// make-results/, which must not match. `universal` is left out on purpose: no leg builds one and
// its layout has not been checked end to end, so it fails closed here (no match, exit 2). A fat
// binary under any other name is covered by readPackagedFuseWire, which compares every wire.
const PACKAGED_DIR = /^.+-(darwin|mas|win32|linux)-(?:x64|arm64|ia32|armv7l)$/;

export interface PackagedApp {
  platform: string;
  /**
   * The file that carries the fuse wire / digest slot, in the form @electron/fuses accepts:
   * the `.app` bundle on macOS, the executable elsewhere.
   */
  target: string;
}

export async function locatePackagedApp(
  outDir: string,
  executableName?: string
): Promise<PackagedApp> {
  const entries = await readdir(outDir, { withFileTypes: true });
  const candidates = entries.flatMap((entry) => {
    const match = entry.isDirectory() ? PACKAGED_DIR.exec(entry.name) : null;
    return match ? [{ name: entry.name, platform: match[1] }] : [];
  });
  // Count-assertion, not `head -1`: a leftover directory from another leg is a defect that
  // `head -1` would silently mask (same rule as the app.asar lookup in the workflows).
  if (candidates.length !== 1) {
    throw new CheckError(
      `expected exactly 1 packaged app directory under ${outDir}, found ${candidates.length}: ` +
        (candidates.map((c) => c.name).join(', ') || '(none)')
    );
  }
  const { name, platform } = candidates[0];
  const dir = path.resolve(outDir, name);

  if (platform === 'darwin' || platform === 'mas') {
    const bundles = (await readdir(dir, { withFileTypes: true })).filter(
      (e) => e.isDirectory() && e.name.endsWith('.app')
    );
    if (bundles.length !== 1) {
      throw new CheckError(`expected exactly 1 .app bundle in ${dir}, found ${bundles.length}`);
    }
    return { platform, target: path.join(dir, bundles[0].name) };
  }

  if (!executableName) {
    throw new CheckError('forge.config.ts sets no packagerConfig.executableName');
  }
  const target = path.join(dir, platform === 'win32' ? `${executableName}.exe` : executableName);
  if (
    !(await stat(target).then(
      (s) => s.isFile(),
      () => false
    ))
  ) {
    throw new CheckError(`packaged executable not found: ${target}`);
  }
  return { platform, target };
}

// ─── Shared: finding a marker in a large binary ─────────────────────────────────────────────

const SCAN_CHUNK_SIZE = 4 * 1024 * 1024;

interface MarkerHit {
  /** Offset of the first byte after the marker. */
  offset: number;
  /** The bytes behind the marker, zero-padded to the length asked for when the file ends first. */
  payload: Buffer;
  /** How many of those bytes the file actually held. */
  bytesRead: number;
}

// A fat (universal) Mach-O opens with one of these. Its slices are separate binaries and Apple
// Silicon runs the arm64 one, but a scan finds markers, not slices: a slice with no marker at all
// (a plugin that missed it, a packager that could not patch it) is invisible, and the slice that
// does hold one passes the check on its behalf. Reproduced on a real lipo-built framework with one
// slice's sentinel destroyed: both modes exited 0. No leg builds one, so it is refused rather than
// guessed at; supporting one means parsing the fat header and requiring a wire and a digest slot in
// every slice.
const FAT_MACHO_MAGICS = [Buffer.from('cafebabe', 'hex'), Buffer.from('cafebabf', 'hex')];

/**
 * Every occurrence of `marker` in `file`, in file order, with the bytes behind it. Read in chunks
 * so a 200 MB Electron binary is never held in memory. Refuses a fat Mach-O (a CheckError, so the
 * job fails with exit 2 instead of reading a binary it cannot account for slice by slice).
 */
async function scanMarker(
  file: string,
  marker: string,
  payloadLen: number,
  chunkSize: number
): Promise<MarkerHit[]> {
  const needle = Buffer.from(marker);
  const overlap = needle.length - 1;
  const handle = await open(file, 'r');
  try {
    const head = Buffer.alloc(4);
    await handle.read(head, 0, head.length, 0);
    if (FAT_MACHO_MAGICS.some((magic) => magic.equals(head))) {
      throw new CheckError(
        `${file} is a fat (universal) Mach-O binary: this check reads markers, not slices, so a slice with no marker at all would pass unseen; no leg builds one`
      );
    }
    const { size } = await handle.stat();
    const buf = Buffer.allocUnsafe(chunkSize + overlap);
    const positions: number[] = [];
    for (let start = 0; start < size; start += chunkSize) {
      const { bytesRead } = await handle.read(
        buf,
        0,
        Math.min(chunkSize + overlap, size - start),
        start
      );
      const haystack = buf.subarray(0, bytesRead);
      // Reading `overlap` bytes past the chunk catches a marker that straddles a boundary. A whole
      // marker cannot also start in the next chunk, so no match is counted twice.
      for (let i = haystack.indexOf(needle); i !== -1; i = haystack.indexOf(needle, i + 1)) {
        positions.push(start + i);
      }
    }
    const hits: MarkerHit[] = [];
    for (const position of positions) {
      const offset = position + needle.length;
      const payload = Buffer.alloc(payloadLen);
      const { bytesRead } = await handle.read(payload, 0, payloadLen, offset);
      hits.push({ offset, payload, bytesRead });
    }
    return hits;
  } finally {
    await handle.close();
  }
}

/** The Electron Framework binary inside a .app: where the fuse wire and the digest slot live. */
function frameworkBinary(appBundle: string): string {
  return path.join(
    appBundle,
    'Contents',
    'Frameworks',
    'Electron Framework.framework',
    'Electron Framework'
  );
}

// ─── fuses ──────────────────────────────────────────────────────────────────────────────────

// Wire bytes are ASCII: '0' disabled, '1' enabled, 'r' removed. These are @electron/fuses'
// FuseState values; 1.x (which this script must also run under) does not re-export them.
const FUSE_DISABLE = 0x30;
const FUSE_ENABLE = 0x31;
const FUSE_REMOVED = 0x72;

// Keys of the FusesPlugin config that are flip options rather than fuse indices.
const FUSE_OPTION_KEYS = new Set([
  'version',
  'strictlyRequireAllFuses',
  'resetAdHocDarwinSignature',
]);

/** What getCurrentFuseWire returns: `version` as a string, then one raw byte per fuse index. */
export type FuseWire = Record<string, string | number>;

export interface ExpectedFuses {
  version: string;
  fuses: Array<{ index: number; name: string; enabled: boolean }>;
}

function fuseName(index: number): string {
  return (FuseV1Options as unknown as Record<number, string | undefined>)[index] ?? `fuse#${index}`;
}

function stateLabel(state: string | number | undefined): string {
  switch (state) {
    case FUSE_ENABLE:
      return 'ENABLE';
    case FUSE_DISABLE:
      return 'DISABLE';
    case FUSE_REMOVED:
      return 'REMOVED';
    case undefined:
      return 'absent';
    default:
      return `0x${Number(state).toString(16)}`;
  }
}

/**
 * Derive the expectation from the FusesPlugin config itself (never a hand-copied list), so the
 * verifier cannot drift from forge.config.ts. A fuse the config leaves unset is not asserted:
 * it keeps whatever the shipped Electron defaults to.
 */
export function deriveExpectedFuses(fusesConfig: Record<string, unknown>): ExpectedFuses {
  const { version } = fusesConfig;
  if (typeof version !== 'string') {
    throw new CheckError('the FusesPlugin config has no string `version`');
  }
  const fuses: ExpectedFuses['fuses'] = [];
  for (const [key, value] of Object.entries(fusesConfig)) {
    if (FUSE_OPTION_KEYS.has(key) || value === undefined) continue;
    // Anything else must be `<fuse index>: boolean`. Guessing at an unknown shape would let a
    // plugin API change silently turn this into a check that asserts nothing.
    if (!/^\d+$/.test(key) || typeof value !== 'boolean') {
      throw new CheckError(`unrecognised FusesPlugin config entry ${key}=${String(value)}`);
    }
    const index = Number(key);
    fuses.push({ index, name: fuseName(index), enabled: value });
  }
  fuses.sort((a, b) => a.index - b.index);
  if (fuses.length === 0) {
    throw new CheckError('the FusesPlugin config sets no fuses, so there is nothing to verify');
  }
  return { version, fuses };
}

interface ForgeConfigLike {
  plugins?: unknown[];
  packagerConfig?: { executableName?: string };
}

/** Same lookup packagingIdentity.test.ts uses: the plugin named `fuses`, its `fusesConfig`. */
export function findFusesConfig(config: ForgeConfigLike): Record<string, unknown> {
  const plugin = config.plugins?.find(
    (p) => typeof p === 'object' && p !== null && (p as { name?: unknown }).name === 'fuses'
  ) as { fusesConfig?: unknown } | undefined;
  const fusesConfig = plugin?.fusesConfig;
  if (typeof fusesConfig !== 'object' || fusesConfig === null) {
    throw new CheckError('forge.config.ts has no FusesPlugin with a fusesConfig');
  }
  return fusesConfig as Record<string, unknown>;
}

export function verifyFuseWire(expected: ExpectedFuses, wire: FuseWire): string[] {
  const violations: string[] = [];
  if (expected.fuses.length === 0) {
    violations.push('the FusesPlugin config sets no fuses, so there is nothing to verify');
  }
  if (wire.version !== expected.version) {
    violations.push(
      `the packaged fuse wire is version ${String(wire.version)} but forge.config.ts configures version ${expected.version}`
    );
  }
  for (const { index, name, enabled } of expected.fuses) {
    const want = enabled ? FUSE_ENABLE : FUSE_DISABLE;
    const actual = wire[String(index)];
    if (actual === want) continue;
    if (actual === undefined) {
      violations.push(`${name} (index ${index}) is beyond the end of the packaged fuse wire`);
    } else if (actual === FUSE_REMOVED) {
      violations.push(
        `${name} is marked REMOVED in this Electron build, so the configured ${stateLabel(want)} cannot take effect`
      );
    } else {
      violations.push(
        `${name} is ${stateLabel(actual)} in the packaged app but forge.config.ts sets ${stateLabel(want)}`
      );
    }
  }
  return violations;
}

/** One line per fuse in the wire, so the CI log shows the fuses nobody configured too. */
export function describeFuseWire(expected: ExpectedFuses, wire: FuseWire): string[] {
  const configured = new Map(expected.fuses.map((f) => [f.index, f]));
  return Object.keys(wire)
    .filter((key) => key !== 'version')
    .map(Number)
    .sort((a, b) => a - b)
    .map((index) => {
      const want = configured.get(index);
      const note = want
        ? `forge.config.ts sets ${stateLabel(want.enabled ? FUSE_ENABLE : FUSE_DISABLE)}`
        : 'not configured (Electron default)';
      return `${fuseName(index).padEnd(40)} ${stateLabel(wire[String(index)]).padEnd(8)} ${note}`;
    });
}

/** Marks the fuse wire in the Electron binary; the wire is { version: u8, length: u8, state: u8[length] }. */
const FUSE_SENTINEL = 'dL7pKGdnNz796PbbjQWNKmHXBZaB9tsX'; // pragma: allowlist secret  (public marker string from @electron/fuses, not a credential)
const FUSE_WIRE_MAX_LEN = 2 + 0xff;

export interface FuseWireHit {
  /** Offset of the wire (the byte after the sentinel). */
  offset: number;
  /** The raw wire, or null when the file ended inside it. */
  bytes: Buffer | null;
}

/**
 * Every fuse wire in the binary, in file order. A fat (universal) Mach-O would carry one per slice
 * and is refused by scanMarker, so a binary that gets this far holds more than one only when
 * something planted a second.
 */
export async function scanFuseWires(
  file: string,
  chunkSize: number = SCAN_CHUNK_SIZE
): Promise<FuseWireHit[]> {
  const hits = await scanMarker(file, FUSE_SENTINEL, FUSE_WIRE_MAX_LEN, chunkSize);
  return hits.map(({ offset, payload, bytesRead }) => {
    const length = 2 + payload[1];
    return { offset, bytes: bytesRead >= length ? payload.subarray(0, length) : null };
  });
}

/** The file that carries the fuse wire: the framework inside a .app, the executable elsewhere. */
function fuseFile(target: string): string {
  return target.endsWith('.app') ? frameworkBinary(target) : target;
}

/**
 * The fuse wire of a packaged app. getCurrentFuseWire returns only the FIRST wire in the file, so
 * every wire in the file must be byte-identical to the first, the same rule asar-digest applies to
 * its slots. The scan runs first because it refuses a fat (universal) binary: whatever such a file
 * holds, it is exit 2 and never a pass.
 */
export async function readPackagedFuseWire(target: string): Promise<FuseWire> {
  const file = fuseFile(target);
  const found = await scanFuseWires(file);
  const wire = (await getCurrentFuseWire(target)) as unknown as FuseWire;
  const reference = found[0]?.bytes;
  if (!reference) {
    // @electron/fuses read a wire this scan cannot see (say, a changed sentinel): nothing to compare.
    throw new Error(`cannot cross-check the fuse wire: no complete fuse wire found in ${file}`);
  }
  if (found.some((hit) => !hit.bytes?.equals(reference))) {
    throw new Error(
      `${file} carries ${found.length} fuse wires (at byte offsets ${found.map((hit) => hit.offset).join(', ')}) ` +
        'and they differ: an architecture slice the plugin did not flip would ship fuses other than the ones checked'
    );
  }
  return wire;
}

// ─── asar-digest ────────────────────────────────────────────────────────────────────────────

/**
 * Marks the integrity-digest slot in the Electron Framework binary. Mirrors
 * MacApp.INTEGRITY_DIGEST_SENTINEL in @electron/packager's dist/mac.js; the slot after it is
 * { used: u8, version: u8, digest: u8[32] }.
 * @see https://github.com/electron/electron/blob/2d5597b1b0fa697905380184e26c9f0947e05c5d/shell/common/asar/integrity_digest.mm#L24
 */
export const INTEGRITY_DIGEST_SENTINEL = 'AGbevlPCksUGKNL8TSn7wGmJEuJsXb2A'; // pragma: allowlist secret  (public marker string from Electron, not a credential)
const SLOT_PAYLOAD_LEN = 34;
// Absolute, so the lookup cannot be redirected by a PATH entry earlier in the job's environment.
const PLUTIL = '/usr/bin/plutil';

export type AsarIntegrity = Record<string, { algorithm: string; hash: string }>;

/** Same acceptance test as the packager's isAsarIntegrity(): it skips the write when this fails. */
export function isAsarIntegrity(value: unknown): value is AsarIntegrity {
  if (typeof value !== 'object' || value === null) return false;
  const entries = Object.values(value);
  return (
    entries.length > 0 &&
    entries.every(
      (entry) =>
        typeof entry === 'object' &&
        entry !== null &&
        typeof (entry as { algorithm?: unknown }).algorithm === 'string' &&
        (entry as { algorithm: string }).algorithm.length > 0 &&
        typeof (entry as { hash?: unknown }).hash === 'string' &&
        (entry as { hash: string }).hash.length > 0
    )
  );
}

/**
 * SHA-256 over the (key, algorithm, hash) tuples in sorted key order, exactly as
 * MacApp.setIntegrityDigest does (@electron/packager dist/mac.js) and as Electron's
 * integrity_digest.mm recomputes it. Default `.sort()` on purpose: it is what the writer uses.
 */
export function computeIntegrityDigest(integrity: AsarIntegrity): Buffer {
  const hash = createHash('SHA256');
  for (const key of Object.keys(integrity).sort()) {
    const { algorithm, hash: entryHash } = integrity[key];
    hash.update(key);
    hash.update(algorithm);
    hash.update(entryHash);
  }
  return hash.digest();
}

export interface DigestSlot {
  /** Offset of the slot payload (the byte after the sentinel). */
  offset: number;
  /** False when the file ended before a full payload. */
  complete: boolean;
  used: number;
  version: number;
  digest: Buffer;
}

/**
 * Every sentinel in the binary, with the slot behind it. All of them, because the packager
 * patches every one it finds and a slot left unpatched enforces nothing while its sibling does.
 * A universal binary (one slot per slice) is refused by scanMarker: a slice with no sentinel at
 * all would leave nothing here to check.
 */
export async function scanDigestSlots(
  file: string,
  chunkSize: number = SCAN_CHUNK_SIZE
): Promise<DigestSlot[]> {
  const hits = await scanMarker(file, INTEGRITY_DIGEST_SENTINEL, SLOT_PAYLOAD_LEN, chunkSize);
  return hits.map(({ offset, payload, bytesRead }) => ({
    offset,
    complete: bytesRead === SLOT_PAYLOAD_LEN,
    used: payload[0],
    version: payload[1],
    digest: payload.subarray(2),
  }));
}

export function verifyDigestSlots(slots: DigestSlot[], expected: Buffer): string[] {
  if (slots.length === 0) {
    return [
      'no integrity-digest sentinel in the Electron Framework binary: the packager did not write ' +
        'a digest (it only warns), and Electron enforces nothing against an unwritten slot',
    ];
  }
  const violations: string[] = [];
  for (const slot of slots) {
    if (!slot.complete) {
      violations.push(`digest slot at offset ${slot.offset} is truncated by the end of the file`);
    } else if (slot.used !== 1) {
      violations.push(
        `digest slot at offset ${slot.offset} is unused (used=${slot.used}): Electron fails open on it`
      );
    } else if (slot.version !== 1) {
      violations.push(
        `digest slot at offset ${slot.offset} has version ${slot.version}, expected 1`
      );
    } else if (!slot.digest.equals(expected)) {
      violations.push(
        `digest slot at offset ${slot.offset} holds ${slot.digest.toString('hex')} but SHA-256 ` +
          `over Info.plist's sorted ElectronAsarIntegrity entries is ${expected.toString('hex')}`
      );
    }
  }
  return violations;
}

type PlutilRunner = (file: string, args: string[]) => string;

const runPlutil: PlutilRunner = (file, args) =>
  execFileSync(file, args, {
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
    maxBuffer: 1024 * 1024,
  });

/** ElectronAsarIntegrity from an Info.plist, via the system plutil (no new dependency). */
export function readAsarIntegrity(infoPlist: string, run: PlutilRunner = runPlutil): AsarIntegrity {
  let json: string;
  try {
    json = run(PLUTIL, ['-extract', 'ElectronAsarIntegrity', 'json', '-o', '-', '--', infoPlist]);
  } catch (err) {
    // A string errno (ENOENT, EACCES, ...) means plutil never ran; a plutil that ran and refused
    // the plist has an exit status instead, and that is a finding about the artifact.
    const code = (err as NodeJS.ErrnoException).code;
    if (typeof code === 'string') {
      throw new CheckError(`${PLUTIL} could not be run (${code}), so Info.plist cannot be read`);
    }
    const stderr = String((err as { stderr?: unknown }).stderr ?? '');
    throw new Error(
      `${infoPlist} has no readable ElectronAsarIntegrity: ${firstLine(stderr || String(err))}`
    );
  }
  const parsed: unknown = JSON.parse(json);
  if (!isAsarIntegrity(parsed)) {
    throw new Error(
      `${infoPlist} ElectronAsarIntegrity is not a non-empty map of { algorithm, hash }`
    );
  }
  return parsed;
}

function firstLine(text: string): string {
  return text.trim().split('\n')[0];
}

export async function verifyAsarDigest(
  appBundle: string,
  readIntegrity: (infoPlist: string) => AsarIntegrity = readAsarIntegrity
): Promise<string[]> {
  // A plist the packager could not read is the same silent skip as a missing sentinel, so every
  // failure to obtain the entries is a finding about the artifact, not a reason to pass.
  let expected: Buffer;
  try {
    expected = computeIntegrityDigest(
      readIntegrity(path.join(appBundle, 'Contents', 'Info.plist'))
    );
  } catch (err) {
    if (err instanceof CheckError) throw err;
    return [(err as Error).message];
  }
  // The path @electron/packager patches: the framework's top-level symlink.
  const framework = frameworkBinary(appBundle);
  let slots: DigestSlot[];
  try {
    slots = await scanDigestSlots(framework);
  } catch (err) {
    // A refused (fat) binary is a check that could not run, not a finding about a slot.
    if (err instanceof CheckError) throw err;
    return [`cannot read ${framework}: ${(err as Error).message}`];
  }
  return verifyDigestSlots(slots, expected);
}

// ─── CLI ────────────────────────────────────────────────────────────────────────────────────

export interface Deps {
  loadForgeConfig: () => Promise<ForgeConfigLike>;
  readFuseWire: (target: string) => Promise<FuseWire>;
  readIntegrity: (infoPlist: string) => AsarIntegrity;
  platform: NodeJS.Platform;
  log: (line: string) => void;
}

// A variable specifier, on purpose: forge.config.ts is only ever run by Forge and does not
// type-check under scripts/tsconfig.json, so a literal import would pull it into that project
// and report errors that are not this script's. Loaded lazily too: only `fuses` needs it, and it
// constructs every maker.
const FORGE_CONFIG_MODULE = '../forge.config.ts';

export async function loadForgeConfig(): Promise<ForgeConfigLike> {
  return (await import(FORGE_CONFIG_MODULE)).default as ForgeConfigLike;
}

const defaultDeps: Deps = {
  loadForgeConfig,
  readFuseWire: readPackagedFuseWire,
  readIntegrity: readAsarIntegrity,
  platform: process.platform,
  log: (line) => console.log(line),
};

const USAGE = 'usage: verify-packaged-app.mts <fuses|asar-digest> [--out <dir>]';

function report(violations: string[], ok: string, deps: Deps): number {
  if (violations.length === 0) {
    deps.log(`::notice::${ok}`);
    return 0;
  }
  for (const violation of violations) deps.log(`::error::${violation.replace(/[\r\n]+/g, ' ')}`);
  return 1;
}

async function runFuses(outDir: string, deps: Deps): Promise<number> {
  const config = await deps.loadForgeConfig();
  const expected = deriveExpectedFuses(findFusesConfig(config));
  const app = await locatePackagedApp(outDir, config.packagerConfig?.executableName);
  let wire: FuseWire;
  try {
    wire = await deps.readFuseWire(app.target);
  } catch (err) {
    if (err instanceof CheckError) throw err;
    // Same rule as asar-digest: no readable fuse wire is a finding about the artifact.
    return report(
      [`cannot read the fuse wire from ${app.target}: ${(err as Error).message}`],
      '',
      deps
    );
  }
  deps.log(`Fuse wire (version ${String(wire.version)}) read from ${app.target}:`);
  for (const line of describeFuseWire(expected, wire)) deps.log(`  ${line}`);
  return report(
    verifyFuseWire(expected, wire),
    `Packaged fuses match forge.config.ts (${expected.fuses.length} configured fuses verified)`,
    deps
  );
}

async function runAsarDigest(outDir: string, deps: Deps): Promise<number> {
  // Not a silent skip: the workflow runs this on macOS legs only, and a call anywhere else is a
  // wiring mistake that would otherwise read as a pass.
  if (deps.platform !== 'darwin') {
    throw new CheckError('asar-digest verifies a macOS app bundle and only runs on macOS');
  }
  const app = await locatePackagedApp(outDir);
  deps.log(`Verifying asar integrity digest in ${app.target}`);
  return report(
    await verifyAsarDigest(app.target, deps.readIntegrity),
    'Asar integrity digest in Electron Framework matches Info.plist ElectronAsarIntegrity',
    deps
  );
}

export async function main(argv: string[], deps: Deps = defaultDeps): Promise<number> {
  try {
    const [mode, ...flags] = argv;
    let outDir = 'out';
    if (flags.length > 0) {
      if (flags.length !== 2 || flags[0] !== '--out') throw new CheckError(USAGE);
      outDir = flags[1];
    }
    if (mode === 'fuses') return await runFuses(outDir, deps);
    if (mode === 'asar-digest') return await runAsarDigest(outDir, deps);
    throw new CheckError(USAGE);
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    deps.log(`::error::verify-packaged-app could not run: ${firstLine(message)}`);
    // The one-line annotation drops where an unexpected error (say a @electron/fuses bump) came
    // from. Frames only: a message can span lines, and a line starting `::` is a workflow command.
    if (err instanceof Error && !(err instanceof CheckError)) {
      for (const frame of (err.stack ?? '')
        .split('\n')
        .filter((l) => /^\s+at /.test(l))
        .slice(0, 5)) {
        deps.log(frame);
      }
    }
    return 2;
  }
}
