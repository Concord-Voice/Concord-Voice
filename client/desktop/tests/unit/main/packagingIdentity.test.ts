// @vitest-environment node
/**
 * Packaging identity validation tests (#382).
 *
 * These tests read the actual forge config and package.json to verify that
 * all app identity fields are correctly set and consistent across platforms.
 * Prevents regressions where a name change in one place silently creates
 * shared-resource conflicts with other Electron apps.
 */
import { describe, it, expect, vi, beforeAll, afterAll } from 'vitest';
import type { ForgeConfig } from '@electron-forge/shared-types';
import fs from 'node:fs';
import path from 'node:path';
import { ALLOWED_WINDOWS_PUBLISHERS } from '../../../src/shared/allowedWindowsPublishers';
import { FuseVersion, FuseV1Options } from '@electron/fuses';

// ── Constants ────────────────────────────────────────────────────────────

const EXPECTED_DISPLAY_NAME = 'Concord Voice';
const LINUX_ICON_SIZES = ['128x128', '256x256', '512x512'] as const;

// ── Load configs ─────────────────────────────────────────────────────────

const desktopRoot = path.resolve(__dirname, '../../../');
const pkgJsonPath = path.join(desktopRoot, 'package.json');
const pkgJson = JSON.parse(fs.readFileSync(pkgJsonPath, 'utf-8'));

// Dynamic import for the ES module forge config
async function loadForgeConfig() {
  const mod = await import('../../../forge.config');
  return mod.default;
}

// Captured at module-load time — the pristine, un-mutated property descriptor
// for process.platform. Used by the defense-in-depth Layer 2 restoration in
// every describe.each block's afterAll so that recovery does not depend on
// the previous test having left a clean state. See loadForgeConfigForPlatform
// JSDoc below for the two-layer pattern.
const PRISTINE_PLATFORM_DESCRIPTOR = Object.getOwnPropertyDescriptor(process, 'platform')!;

/**
 * Load forge.config.ts with a specific process.platform value forced via
 * Object.defineProperty + vi.resetModules() + dynamic re-import. Required for
 * exercising the forge.config.ts:121 per-platform executableName conditional
 * across linux/darwin/win32 in a single PR CI run on ubuntu-latest.
 *
 * Restoration discipline (defense-in-depth):
 *   Layer 1 (helper-internal): try/finally captures the FULL property
 *     descriptor before mutation and restores it before the function returns,
 *     even if dynamic import throws. Capturing the descriptor (not just .value)
 *     preserves enumerable/writable/configurable attributes — Object.defineProperty
 *     in modify-mode preserves omitted attributes per ECMAScript spec, but
 *     descriptor-capture-and-restore is strictly stronger defense against
 *     future Node.js or ECMAScript semantic drift.
 *   Layer 2 (caller-side): describe.each blocks use afterAll() to restore
 *     PRISTINE_PLATFORM_DESCRIPTOR — independent of whatever Layer 1 left in
 *     place — as a safety net if a future refactor bypasses this helper's
 *     try/finally.
 *
 * Side effect: vi.resetModules() invalidates the ENTIRE module cache, not just
 * forge.config.ts. The next dynamic import of any module (including loadForgeConfig's
 * import of forge.config.ts elsewhere in this file) re-evaluates the module graph
 * with the now-restored process.platform. This is intentional — the cache reset is
 * what allows the per-platform re-import — but adding cached state to forge.config.ts
 * or its imports would silently break here.
 *
 * Dependency: forge.config.ts:67-81 buildtag.json fail-loud guard stays
 * filtered by isForgePackaging (process.argv-driven). Vitest runs do not have
 * 'electron-forge' in process.argv, so the guard does not trip during these
 * re-imports. If a future change removes the isForgePackaging filter, this
 * helper will start throwing on CI runners (CI=true is set there).
 *
 * See [internal]specs/2026-05-27-1096-forge-config-platform-tests-design.md.
 */
async function loadForgeConfigForPlatform(platform: NodeJS.Platform): Promise<ForgeConfig> {
  const originalDescriptor = Object.getOwnPropertyDescriptor(process, 'platform')!;
  Object.defineProperty(process, 'platform', { ...originalDescriptor, value: platform });
  try {
    vi.resetModules();
    const mod = await import('../../../forge.config');
    return mod.default;
  } finally {
    Object.defineProperty(process, 'platform', originalDescriptor);
  }
}

// ── Helpers ──────────────────────────────────────────────────────────────

/**
 * Extract maker options from a forge config maker instance.
 * electron-forge stores constructor args in `configOrConfigFetcher`.
 * MakerSquirrel stores options directly; Deb/Rpm/AppImage wrap in `options`.
 */
function getMakerOptions(config: any, platform: string) {
  for (const maker of config.makers ?? []) {
    const name = maker.constructor?.name ?? '';
    if (name.toLowerCase().includes(platform.toLowerCase())) {
      const cfg = maker.configOrConfigFetcher ?? {};
      // Deb, Rpm, AppImage use { options: { ... } } wrapper
      return cfg.options ?? cfg;
    }
  }
  return null;
}

function expectLinuxIconConfig(icon: unknown) {
  expect(icon).toEqual(
    expect.objectContaining({
      '128x128': './build/icons/128x128.png',
      '256x256': './build/icons/256x256.png',
      '512x512': './build/icons/512x512.png',
    })
  );
}

// ── Tests ────────────────────────────────────────────────────────────────

describe('Packaging Identity (#382)', () => {
  it('configures the five Electron hardening fuses', async () => {
    const config = await loadForgeConfig();
    const fuses = config.plugins?.find((plugin) => plugin.name === 'fuses') as
      { fusesConfig?: unknown } | undefined;
    expect(fuses?.fusesConfig).toEqual({
      version: FuseVersion.V1,
      [FuseV1Options.RunAsNode]: false,
      [FuseV1Options.EnableNodeCliInspectArguments]: false,
      [FuseV1Options.EnableEmbeddedAsarIntegrityValidation]: true,
      [FuseV1Options.OnlyLoadAppFromAsar]: true,
      [FuseV1Options.EnableCookieEncryption]: true,
    });
  });
  describe('package.json', () => {
    it('has productName set to "Concord Voice" (display name with space)', () => {
      expect(pkgJson.productName).toBe(EXPECTED_DISPLAY_NAME);
    });

    it('has name scoped to @concordvoice/', () => {
      expect(pkgJson.name).toMatch(/^@concordvoice\//);
    });

    it('has author set', () => {
      expect(pkgJson.author).toBeTruthy();
    });

    it('has description containing "Concord"', () => {
      expect(pkgJson.description).toContain('Concord');
    });

    it('does not use generic placeholder names', () => {
      const name = pkgJson.name.toLowerCase();
      const product = (pkgJson.productName ?? '').toLowerCase();
      expect(name).not.toContain('electron');
      expect(name).not.toContain('my-app');
      expect(product).not.toContain('electron');
      expect(product).not.toBe('');
    });
  });

  describe('forge.config.ts — packagerConfig', () => {
    it('has name set to "Concord Voice"', async () => {
      const config = await loadForgeConfig();
      expect(config.packagerConfig?.name).toBe(EXPECTED_DISPLAY_NAME);
    });

    it('has appBundleId set to com.concordvoice.desktop', async () => {
      const config = await loadForgeConfig();
      expect(config.packagerConfig?.appBundleId).toBe('com.concordvoice.desktop');
    });

    it('has appCopyright set and containing "Concord"', async () => {
      const config = await loadForgeConfig();
      expect(config.packagerConfig?.appCopyright).toBeTruthy();
      expect(config.packagerConfig?.appCopyright).toContain('Concord');
    });

    it('has appCategoryType set to a macOS UTI category', async () => {
      const config = await loadForgeConfig();
      expect(config.packagerConfig?.appCategoryType).toMatch(/^public\.app-category\./);
    });

    it('registers the concord invite-link protocol', async () => {
      const config = await loadForgeConfig();
      expect(config.packagerConfig?.protocols).toEqual([
        { name: 'Concord Voice invite links', schemes: ['concord'] },
      ]);
      expect(config.packagerConfig?.extendInfo).toMatchObject({
        CFBundleURLTypes: [
          {
            CFBundleURLName: 'Concord Voice invite links',
            CFBundleURLSchemes: ['concord'],
          },
        ],
      });
    });

    // ADR-0043 D7. Electron 43's desktopCapturer docs: on macOS 14.2+, audio capture
    // without this key "will create a dead audio stream without warnings or errors".
    // captureScreenElectron's try/catch -> video-only fallback cannot observe a
    // live-but-silent track, so the client publishes SILENCE to the SFU and nobody
    // learns anything is wrong. Pinned here because the failure mode is invisible:
    // a config refactor could drop the key and no other test would notice.
    it('declares the media-capture usage descriptions macOS requires', async () => {
      const config = await loadForgeConfig();
      const info = config.packagerConfig?.extendInfo as Record<string, unknown> | undefined;
      expect(info?.NSMicrophoneUsageDescription).toEqual(expect.any(String));
      expect(info?.NSCameraUsageDescription).toEqual(expect.any(String));
      expect(info?.NSAudioCaptureUsageDescription).toEqual(expect.any(String));
    });

    // The PACKAGED app gets these keys from forge.config.ts; a `npm start` DEV launch
    // gets them only from scripts/patch-electron-plist.sh, which patches Electron's
    // helper plists. They are a file PAIR, and the audio key shipped in the first half
    // alone -- so screen-audio was dead on the machine it was being written on, with no
    // exception and no warning to say why. Assert both halves together.
    it('patches the same usage descriptions into the dev helper plists', async () => {
      const { readFile } = await import('node:fs/promises');
      const script = await readFile(
        new URL('../../../scripts/patch-electron-plist.sh', import.meta.url),
        'utf8'
      );
      for (const key of [
        'NSMicrophoneUsageDescription',
        'NSCameraUsageDescription',
        'NSAudioCaptureUsageDescription',
      ]) {
        expect(script).toContain(`Add :${key} string`);
      }
    });

    it('has win32metadata.CompanyName set', async () => {
      const config = await loadForgeConfig();
      expect(config.packagerConfig?.win32metadata?.CompanyName).toBeTruthy();
    });

    it('has win32metadata.FileDescription set', async () => {
      const config = await loadForgeConfig();
      expect(config.packagerConfig?.win32metadata?.FileDescription).toBeTruthy();
    });

    it('has win32metadata.ProductName set to "Concord Voice"', async () => {
      const config = await loadForgeConfig();
      expect(config.packagerConfig?.win32metadata?.ProductName).toBe(EXPECTED_DISPLAY_NAME);
    });
  });

  // app.asar payload boundary.
  //
  // @electron/packager's DEFAULT_IGNORES cover only lockfiles, .git,
  // node_modules/.bin, .o files and the out/ directory — an app's OWN source
  // tree is not among them. Before packagerConfig.ignore grew the entries
  // below, the entire working directory shipped inside app.asar: the released
  // 0.2.45 archive contains /src, /tests, /docs, /scripts and the build
  // machine's own .env. A config refactor could drop the entries again and
  // nothing else in this suite would notice, so pin them here — the same
  // rationale as the identity fields above.
  describe('forge.config.ts — app.asar exclusions', () => {
    /**
     * Decide a path the way @electron/packager's copy filter does: the array of
     * RegExps is applied with String.prototype.match against an app-dir-relative
     * path carrying a LEADING SLASH ('/src/main/main.ts'), and any match excludes
     * the file. See node_modules/@electron/packager/dist/copy-filter.js.
     */
    function isIgnored(config: ForgeConfig, appRelativePath: string): boolean {
      const ignore = config.packagerConfig?.ignore;
      // The RegExp[] form is load-bearing: populateIgnoredPaths() merges
      // DEFAULT_IGNORES only when `ignore` is NOT a function, so switching to a
      // filter function would silently re-admit package-lock.json and .git.
      if (!Array.isArray(ignore)) {
        throw new Error('packagerConfig.ignore must stay an array of RegExps');
      }
      return (ignore as RegExp[]).some((re) => appRelativePath.match(re) !== null);
    }

    // Nothing here is read at runtime. public/ is copied into dist/renderer by
    // Vite and assets/tray reaches <Resources>/tray via extraResource, so both
    // are duplicates rather than dead weight; build/ and scripts/ are consumed
    // from the source directory at packaging time, never from the archive.
    it.each([
      '/src',
      '/src/main/main.ts',
      '/tests',
      '/tests/unit/main/packagingIdentity.test.ts',
      '/docs/STATE_MANAGEMENT.md',
      '/scripts/build-preload.mjs',
      '/schemas/update-manifest.json',
      '/functions/assets/[[path]].js',
      '/public/favicon.ico',
      '/build/splash.gif',
      '/build/dmg-background.png',
      '/build/makerNsis.ts',
      '/build/installer.nsh',
      '/build/icons/512x512.png',
      '/build/icon.ico',
      '/assets/tray/icon.png',
      '/coverage/index.html',
      '/playwright-report/index.html',
      '/test-results/results.json',
      '/.env',
      '/.env.example',
      '/.env.staging',
      '/.prettierrc',
      '/forge.config.ts',
      '/vite.config.ts',
      '/playwright.config.ts',
      '/eslint.config.mjs',
      '/tsconfig.json',
      '/tsconfig.main.json',
      '/index.html',
      '/README.md',
      '/wrangler.toml',
      '/dist/main/main.js.map',
      // TypeScript declarations. tsconfig.main.json inherits declaration +
      // declarationMap, so `npm run build:main` writes these beside the .js it
      // runs; /\.map$/ took only their .d.ts.map companions and left 1035
      // declarations (~13.4 MiB) in the archive. The last two are the point of
      // making this a suffix rule rather than an ^-anchored one: 957 of those
      // 1035 came from production dependencies.
      '/dist/main/main.d.ts',
      '/dist/shared/allowedWindowsPublishers.d.ts',
      '/dist/main/main.d.ts.map',
      '/node_modules/zod/index.d.ts',
      '/node_modules/mediasoup-client/lib/enhancedEvents.d.ts',
      // TypeScript's dual-publish declaration variants. 127 of these (~4.96 MiB)
      // survived a `.d.ts`-only rule, from zod, lucide-react, minisearch and
      // others — so the [cm]? is load-bearing, not defensive.
      '/node_modules/zod/v3/external.d.cts',
      '/node_modules/lucide-react/dynamic.d.mts',
      '/node_modules/minisearch/dist/cjs/index.d.cts',
    ])('excludes %s from app.asar', async (appRelativePath) => {
      expect(isIgnored(await loadForgeConfig(), appRelativePath)).toBe(true);
    });

    // The complete runtime surface: main resolves preload and renderer from
    // __dirname inside dist/ (main.ts's '../preload/preload.js' and
    // '../renderer/index.html'), and nothing outside these three entries is
    // read once packaged.
    it.each([
      '/dist/main/main.js',
      '/dist/preload/preload.js',
      '/dist/renderer/index.html',
      '/dist/renderer/assets/index.js',
      '/dist/shared/allowedWindowsPublishers.js',
      '/node_modules/zod/package.json',
      '/package.json',
      // The runtime sibling of an excluded declaration must survive. This exact
      // pair is why the .d.ts rule was checked against every shipped
      // package.json before it was widened to node_modules: upstream
      // mediasoup-client's "./enhancedEvents" export map reads
      // `"ortc": "./lib/enhancedEvents.d.ts"` where it means `"types"`. `ortc`
      // is not a Node export condition, so resolution skips that key and lands
      // on `"default": "./lib/enhancedEvents.js"` — this file.
      '/node_modules/mediasoup-client/lib/enhancedEvents.js',
      // Only the `.d.` form is excluded. Node cannot execute a bare .cts/.mts —
      // it runs .cjs/.mjs — but the rule still must not reach a TypeScript source
      // file, and these two pin that boundary.
      '/node_modules/some-pkg/dist/index.cts',
      '/node_modules/some-pkg/dist/index.mts',
    ])('does not exclude %s', async (appRelativePath) => {
      expect(isIgnored(await loadForgeConfig(), appRelativePath)).toBe(false);
    });

    // ADR-0043 PR 5 (#3194). The addon's SOURCE stays out of the archive, but the
    // loader `index.js` ships INSIDE it, so executable JS entering a
    // Node-privileged process stays under EnableEmbeddedAsarIntegrityValidation.
    // The opaque .node goes the other way, via extraResource, because dlopen
    // needs a real path.
    //
    // THE DESCEND TRAP IS THE WHOLE POINT OF THIS TABLE. The copy filter is asked
    // about a DIRECTORY before it descends, so a pattern matching `/native` or
    // `/native/concord-audiocap` suppresses the entire subtree and index.js is
    // never copied — while every per-file assertion above stays green. Same
    // mechanic the `/^\/build\/(?!icon\.(?:png|icns)$)/` rule documents, and the
    // reason the first two rows below assert NOT-excluded on bare directories.
    //
    // This table is the SPECIFICATION; the regex is one implementation of it.
    // Rewrite the pattern freely, but every row must still hold.
    it.each<[string, boolean]>([
      ['/native', false],
      ['/native/concord-audiocap', false],
      ['/native/concord-audiocap/index.js', false],
      ['/native/concord-audiocap/rt', true],
      ['/native/concord-audiocap/rt/quantum_ring.h', true],
      ['/native/concord-audiocap/napi/addon.cc', true],
      ['/native/concord-audiocap/test/ring_test.cc', true],
      ['/native/concord-audiocap/binding.gyp', true],
      ['/native/concord-audiocap/README.md', true],
      // Ships via extraResource; a second, unloadable copy inside the archive is
      // dead weight and would also defeat the asar-list assertion pair in CI.
      ['/native/concord-audiocap/build/Release/concord_audiocap.node', true],
      // A future second addon must not become implicitly shippable by sitting
      // next to this one.
      ['/native/some-future-addon', true],
      ['/native/some-future-addon/index.js', true],
    ])('native lookahead: %s → excluded=%s', async (appRelativePath, expected) => {
      expect(isIgnored(await loadForgeConfig(), appRelativePath)).toBe(expected);
    });

    // The copy filter runs the SAME patterns over files under /node_modules/,
    // and String.match is unanchored. An unanchored /tests/ or /build/ would
    // quietly gut any production dependency shipping a directory of that name —
    // a node-gyp addon's build/Release/*.node above all — and the failure would
    // surface as a runtime require() error in a released build, not here.
    it('leaves production dependencies intact despite sharing excluded names', async () => {
      const config = await loadForgeConfig();
      for (const appRelativePath of [
        '/node_modules/pkg/src/index.js',
        '/node_modules/pkg/tests/fixture.json',
        '/node_modules/pkg/build/Release/binding.node',
        '/node_modules/pkg/public/logo.svg',
        '/node_modules/pkg/docs/api.md',
        '/node_modules/pkg/scripts/install.js',
        '/node_modules/pkg/.env',
        '/node_modules/pkg/index.html',
        '/node_modules/pkg/tsconfig.json',
        '/node_modules/pkg/assets/logo.svg',
        '/node_modules/pkg/rollup.config.js',
        '/node_modules/pkg/README.md',
      ]) {
        expect(isIgnored(config, appRelativePath), appRelativePath).toBe(false);
      }
    });

    // Regression guard for the defect Codex caught on PR #3156. `build/` is
    // overwhelmingly packaging-time input, but TWO files inside it are read
    // from within app.asar at runtime via app.getAppPath():
    //   src/main/main.ts:1648                -> build/icon.png  (splash; the
    //                                           call is inside `if
    //                                           (app.isPackaged)`, so it runs
    //                                           ONLY where the exclusion bites)
    //   src/main/applicationsFolderGate.ts:94 -> build/icon.icns (macOS
    //                                           move-to-Applications dialog)
    // Both go through nativeImage.createFromPath, which never throws — a
    // missing file yields an empty image, so an over-broad exclusion loses the
    // branding silently in release builds and in no test.
    it('keeps the two build/ icons the packaged main process loads', async () => {
      const config = await loadForgeConfig();
      for (const appRelativePath of ['/build/icon.png', '/build/icon.icns']) {
        expect(isIgnored(config, appRelativePath), appRelativePath).toBe(false);
      }
    });

    // The copy filter is asked about a DIRECTORY before it descends into it, so
    // a pattern matching the bare '/build' entry would stop the two icons above
    // from ever being copied — while every path-level assertion still passed.
    it('does not exclude the /build directory entry itself', async () => {
      const config = await loadForgeConfig();
      expect(isIgnored(config, '/build')).toBe(false);
    });

    // The same directory-before-descent hazard as the /build case, one directory
    // over, and far worse: a pattern matching the bare `/node_modules` entry stops
    // the ENTIRE dependency tree from being copied, and every `/node_modules/pkg/**`
    // assertion above stays green because the filter is never asked about those
    // paths once the directory is refused. The packaged app then fails to launch.
    // CI's required-file probe would not catch it either — it only opens three
    // files under dist/. Surfaced by @pr-review-toolkit:pr-test-analyzer on #3156.
    it.each(['/node_modules', '/dist'])('does not exclude the %s directory entry', async (dir) => {
      expect(isIgnored(await loadForgeConfig(), dir)).toBe(false);
    });

    // Structural backstop for the case above: a pattern added later for a name
    // nobody thought to list is still forced to anchor at the app root.
    //
    // The exemption is an ALLOWLIST of exact sources, not a test of regex shape.
    // An earlier draft exempted anything slash-free ending in `$`, reasoning that
    // it exempted by behaviour and so would keep working if the rule were
    // rewritten. That is the wrong direction on the one axis that matters: it
    // also hands a free pass to a future `/\.node$/`, which would strip native
    // addons out of production dependencies — silently, because the archive
    // verifier's depth check knows only these same two suffixes and its runtime
    // probe opens three files under dist/. Rewriting a listed rule now reds this
    // test, which is a prompt to re-review an unanchored rule rather than a
    // hazard. Surfaced by Codex on #3156.
    const COMPILE_ONLY_SUFFIX_RULES = new Set(['\\.map$', '\\.d\\.[cm]?ts$']);

    it('anchors every path pattern at the app root', async () => {
      const config = await loadForgeConfig();
      const ignore = config.packagerConfig?.ignore as RegExp[];
      for (const pattern of ignore) {
        // @electron/packager accepts strings here too (its own DEFAULT_IGNORES are
        // strings), so assert the shape before reading `.source` — otherwise a
        // future string entry fails with an unreadable TypeError.
        expect(pattern, String(pattern)).toBeInstanceOf(RegExp);
        if (COMPILE_ONLY_SUFFIX_RULES.has(pattern.source)) continue;
        expect(pattern.source.startsWith('^\\/'), pattern.source).toBe(true);
      }
    });

    // Both allowlisted rules must still BE in the config. Without this the
    // allowlist above degrades quietly: delete `\.d\.[cm]?ts$` and the anchor
    // test goes green because there is simply nothing left to exempt.
    it('still carries both compile-only suffix rules', async () => {
      const config = await loadForgeConfig();
      const sources = (config.packagerConfig?.ignore as RegExp[]).map((p) => p.source);
      for (const allowed of COMPILE_ONLY_SUFFIX_RULES) {
        expect(sources, allowed).toContain(allowed);
      }
    });
  });

  // Per-platform executableName branch coverage in PR CI (#1096).
  //
  // CI test shards run exclusively on ubuntu-latest, so the existing tests
  // that branch on the runtime process.platform only ever exercise the Linux
  // branch. This describe.each uses loadForgeConfigForPlatform() to force-load
  // forge.config.ts under each NodeJS.Platform value in turn, exercising all
  // three branches in a single PR CI run.
  //
  // Defense-in-depth restoration:
  //   Layer 1 (in helper): try/finally restores process.platform inside the
  //     helper before it returns.
  //   Layer 2 (here): afterAll re-restores from the captured originalPlatform
  //     in case a future refactor bypasses the helper's try/finally.
  describe.each([
    ['linux', 'concord-voice'],
    ['darwin', 'Concord Voice'],
    ['win32', 'Concord Voice'],
  ] as const)('forge.config.ts — executableName on %s', (platform, expected) => {
    let config: ForgeConfig;

    beforeAll(async () => {
      config = await loadForgeConfigForPlatform(platform);
    });

    afterAll(() => {
      // Layer 2 defense-in-depth: restore the pristine descriptor captured at
      // module load (independent of whatever Layer 1's helper-internal finally
      // left in place).
      Object.defineProperty(process, 'platform', PRISTINE_PLATFORM_DESCRIPTOR);
    });

    it(`has executableName === '${expected}'`, () => {
      expect(config.packagerConfig?.executableName).toBe(expected);
    });
  });

  // Windows packaging migrated from Squirrel.Windows to NSIS in #2402. Squirrel's
  // Setup.exe recursive-deleted its own install root before writing, which meant
  // deleting the very installer executing from %LOCALAPPDATA%\ConcordVoice\pending\
  // — Windows locks a running image, so the delete threw UnauthorizedAccessException
  // and the installer died.
  //
  // Assertions that survived the migration are the IDENTITY ones this file exists to
  // guard (#382): a branded, spaceless, non-generic installer name. Dropped with the
  // Squirrel maker are `description`, `copyright`, `owners`, `loadingGif`, and
  // `iconUrl` — those were Squirrel maker OPTIONS with no NSIS equivalent in our
  // config; electron-builder derives the equivalent metadata from package.json.
  // Their loss is intentional, not an oversight.
  describe('forge.config.ts — MakerNsis (Windows)', () => {
    it('registers an NSIS maker (and no Squirrel maker)', async () => {
      const config = await loadForgeConfig();
      expect(getMakerOptions(config, 'Nsis')).not.toBeNull();
      expect(getMakerOptions(config, 'Squirrel')).toBeNull();
    });

    it('has artifactName containing "ConcordVoice"', async () => {
      const config = await loadForgeConfig();
      const nsis = getMakerOptions(config, 'Nsis');
      expect(nsis.nsis.artifactName).toBeTruthy();
      expect(nsis.nsis.artifactName).toContain('ConcordVoice');
    });

    it('artifactName does not use generic names', async () => {
      const config = await loadForgeConfig();
      const nsis = getMakerOptions(config, 'Nsis');
      const exe = nsis.nsis.artifactName?.toLowerCase() ?? '';
      expect(exe).not.toBe('setup.exe');
      expect(exe).not.toContain('electron');
    });

    it('artifactName is spaceless (registry- and shell-safe)', async () => {
      const config = await loadForgeConfig();
      const nsis = getMakerOptions(config, 'Nsis');
      expect(nsis.nsis.artifactName).not.toMatch(/\s/);
    });

    it('artifactName ends in Setup.exe (release verification glob)', async () => {
      // build-desktop.yml:1163 globs *Setup.exe and fails the release when nothing
      // matches. electron-builder's default artifactName does NOT end in Setup.exe.
      const config = await loadForgeConfig();
      const nsis = getMakerOptions(config, 'Nsis');
      expect(nsis.nsis.artifactName).toMatch(/Setup\.exe$/);
    });

    it('pins the install directory via the customInit include', async () => {
      // build/installer.nsh sets $INSTDIR to $LOCALAPPDATA\ConcordVoice\app so the
      // install root and the updater cache (…\ConcordVoice\pending) are SIBLINGS.
      const config = await loadForgeConfig();
      const nsis = getMakerOptions(config, 'Nsis');
      expect(nsis.nsis.include).toBe('./build/installer.nsh');
    });

    it('the referenced NSIS include and icons actually exist on disk', async () => {
      // Asserting the STRING alone passes whether or not the file is there. If
      // installer.nsh is renamed, moved, or excluded from packaging, app-builder-lib
      // falls back to the sanitizedName branch, $INSTDIR reverts to a directory
      // derived from @concordvoice/desktop, and the updater cache nests back INSIDE
      // the install root — silently reinstating the exact failure this PR fixes, on
      // users' machines, with no CI signal (there is no Windows CI).
      const config = await loadForgeConfig();
      const nsis = getMakerOptions(config, 'Nsis');
      for (const rel of [nsis.nsis.include, nsis.nsis.installerIcon, nsis.nsis.uninstallerIcon]) {
        expect(rel).toBeTruthy();
        expect(fs.existsSync(path.resolve(desktopRoot, rel as string))).toBe(true);
      }
    });

    it('has publisherName matching the Authenticode allowlist', async () => {
      // Asserted against the CONSTANT, not a re-hardcoded literal. generate-app-update.mts
      // spreads the same constant into app-update.yml, and electron-updater reads the
      // Windows allowlist ONLY from that file — so a literal here would let a
      // cert-rotation LLC rename update the constant and app-update.yml while
      // forge.config.ts kept stamping the old name, with this test still green (#2020).
      const config = await loadForgeConfig();
      const nsis = getMakerOptions(config, 'Nsis');
      expect(nsis.publisherName).toEqual([...ALLOWED_WINDOWS_PUBLISHERS]);
    });

    it('uses a one-click per-user install', async () => {
      // oneClick drives app-builder-lib's install-dir naming branch
      // (targetUtil.js:40) and perMachine keeps the install per-user under
      // $LOCALAPPDATA — both load-bearing for the ConcordVoice\app pin.
      const config = await loadForgeConfig();
      const nsis = getMakerOptions(config, 'Nsis');
      expect(nsis.nsis.oneClick).toBe(true);
      expect(nsis.nsis.perMachine).toBe(false);
    });

    it('productName is the display form of packagerConfig.name', async () => {
      const config = await loadForgeConfig();
      const nsis = getMakerOptions(config, 'Nsis');
      expect(nsis.productName).toBe(config.packagerConfig?.name);
    });
  });

  describe('forge.config.ts — MakerDeb (Linux .deb)', () => {
    // NOTE: Linux package conventions require lowercase, no-space binary names
    // (debian-policy §5.6.7). The display name (executableName) carries spaces
    // for macOS/Windows; Linux maker bin/name use the kebab-case form
    // 'concord-voice'. The intentional asymmetry is verified in the
    // 'Linux maker bin: contract on %s' describe.each block below (post-#1096).
    it('has name set to "concord-voice" (Linux kebab-case convention)', async () => {
      const config = await loadForgeConfig();
      const deb = getMakerOptions(config, 'Deb');
      expect(deb).not.toBeNull();
      expect(deb.name).toBe('concord-voice');
    });

    it('has bin set to "concord-voice" (Linux kebab-case convention)', async () => {
      const config = await loadForgeConfig();
      const deb = getMakerOptions(config, 'Deb');
      expect(deb.bin).toBe('concord-voice');
    });

    it('has productName set', async () => {
      const config = await loadForgeConfig();
      const deb = getMakerOptions(config, 'Deb');
      expect(deb.productName).toBe(EXPECTED_DISPLAY_NAME);
    });

    it('has maintainer set with email', async () => {
      const config = await loadForgeConfig();
      const deb = getMakerOptions(config, 'Deb');
      expect(deb.maintainer).toBeTruthy();
      expect(deb.maintainer).toContain('@');
    });

    it('has homepage set to HTTPS URL', async () => {
      const config = await loadForgeConfig();
      const deb = getMakerOptions(config, 'Deb');
      expect(deb.homepage).toMatch(/^https:\/\//);
    });

    it('has description set', async () => {
      const config = await loadForgeConfig();
      const deb = getMakerOptions(config, 'Deb');
      expect(deb.description).toBeTruthy();
    });

    it('has section set', async () => {
      const config = await loadForgeConfig();
      const deb = getMakerOptions(config, 'Deb');
      expect(deb.section).toBeTruthy();
    });

    it('uses hicolor icon sizes instead of a pixmap-only string', async () => {
      const config = await loadForgeConfig();
      const deb = getMakerOptions(config, 'Deb');
      expectLinuxIconConfig(deb.icon);
    });
  });

  describe('forge.config.ts — MakerRpm (Linux .rpm)', () => {
    // Same Linux kebab-case convention as Deb — see comment in MakerDeb block.
    it('has name set to "concord-voice" (Linux kebab-case convention)', async () => {
      const config = await loadForgeConfig();
      const rpm = getMakerOptions(config, 'Rpm');
      expect(rpm).not.toBeNull();
      expect(rpm.name).toBe('concord-voice');
    });

    it('has bin set to "concord-voice" (Linux kebab-case convention)', async () => {
      const config = await loadForgeConfig();
      const rpm = getMakerOptions(config, 'Rpm');
      expect(rpm.bin).toBe('concord-voice');
    });

    it('has productName set', async () => {
      const config = await loadForgeConfig();
      const rpm = getMakerOptions(config, 'Rpm');
      expect(rpm.productName).toBe(EXPECTED_DISPLAY_NAME);
    });

    it('has homepage set to HTTPS URL', async () => {
      const config = await loadForgeConfig();
      const rpm = getMakerOptions(config, 'Rpm');
      expect(rpm.homepage).toMatch(/^https:\/\//);
    });

    it('has license set to LicenseRef-CVSL-1.0', async () => {
      const config = await loadForgeConfig();
      const rpm = getMakerOptions(config, 'Rpm');
      expect(rpm.license).toBe('LicenseRef-CVSL-1.0');
    });

    it('has description set', async () => {
      const config = await loadForgeConfig();
      const rpm = getMakerOptions(config, 'Rpm');
      expect(rpm.description).toBeTruthy();
    });

    it('has group set', async () => {
      const config = await loadForgeConfig();
      const rpm = getMakerOptions(config, 'Rpm');
      expect(rpm.group).toBeTruthy();
    });

    it('uses hicolor icon sizes instead of a pixmap-only string', async () => {
      const config = await loadForgeConfig();
      const rpm = getMakerOptions(config, 'Rpm');
      expectLinuxIconConfig(rpm.icon);
    });
  });

  describe('forge.config.ts — MakerAppImage (Linux AppImage)', () => {
    // Same Linux kebab-case convention as Deb/Rpm — see comment in MakerDeb block.
    it('has bin set to "concord-voice" (Linux kebab-case convention)', async () => {
      const config = await loadForgeConfig();
      const appImage = getMakerOptions(config, 'AppImage');
      expect(appImage).not.toBeNull();
      expect(appImage.bin).toBe('concord-voice');
    });

    it('has productName set', async () => {
      const config = await loadForgeConfig();
      const appImage = getMakerOptions(config, 'AppImage');
      expect(appImage.productName).toBe(EXPECTED_DISPLAY_NAME);
    });

    it('uses hicolor icon sizes instead of a pixmap-only string', async () => {
      const config = await loadForgeConfig();
      const appImage = getMakerOptions(config, 'AppImage');
      expectLinuxIconConfig(appImage.icon);
    });
  });

  it('keeps Linux maker icon maps isolated from AppImage normalization', async () => {
    const config = await loadForgeConfig();
    const deb = getMakerOptions(config, 'Deb');
    const rpm = getMakerOptions(config, 'Rpm');
    const appImage = getMakerOptions(config, 'AppImage');

    (appImage.icon as Record<string, string>).default = '512x512';

    expect(deb.icon.default).toBeUndefined();
    expect(rpm.icon.default).toBeUndefined();
  });

  describe('icon files', () => {
    it('icon.ico exists (Windows)', () => {
      expect(fs.existsSync(path.join(desktopRoot, 'build', 'icon.ico'))).toBe(true);
    });

    it('icon.icns exists (macOS)', () => {
      expect(fs.existsSync(path.join(desktopRoot, 'build', 'icon.icns'))).toBe(true);
    });

    it('icon.png exists (Linux)', () => {
      expect(fs.existsSync(path.join(desktopRoot, 'build', 'icon.png'))).toBe(true);
    });

    it.each(LINUX_ICON_SIZES)('%s hicolor source exists (Linux)', (size) => {
      expect(fs.existsSync(path.join(desktopRoot, 'build', 'icons', `${size}.png`))).toBe(true);
    });

    it('splash.gif exists (Windows installer splash)', () => {
      expect(fs.existsSync(path.join(desktopRoot, 'build', 'splash.gif'))).toBe(true);
    });
  });

  describe('cross-platform identity consistency', () => {
    it('all Linux makers use the same bin name', async () => {
      const config = await loadForgeConfig();
      const deb = getMakerOptions(config, 'Deb');
      const rpm = getMakerOptions(config, 'Rpm');
      const appImage = getMakerOptions(config, 'AppImage');
      const bins = [deb?.bin, rpm?.bin, appImage?.bin].filter(Boolean);
      expect(new Set(bins).size).toBe(1);
      expect(bins[0]).toBe('concord-voice');
    });

    it('productName matches packagerConfig.name (display name consistency)', async () => {
      const config = await loadForgeConfig();
      expect(pkgJson.productName).toBe(config.packagerConfig?.name);
    });

    it('packagerConfig.name contains a space (display name)', async () => {
      const config = await loadForgeConfig();
      expect(config.packagerConfig?.name).toContain(' ');
    });

    it('no maker config contains "electron" or "my-app" in identity fields', async () => {
      const config = await loadForgeConfig();
      for (const maker of config.makers ?? []) {
        const cfg = maker.configOrConfigFetcher ?? {};
        // Deb/Rpm/AppImage nest under options; Squirrel is flat
        const opts = cfg.options ?? cfg;
        const name = (opts.name ?? '').toLowerCase();
        expect(name).not.toContain('my-app');
        // 'electron' in description is OK (e.g. "Electron-based"), but not in name
        expect(name).not.toBe('electron');
      }
    });
  });

  // Linux maker bin: alignment + asymmetry contract per platform (#1096).
  //
  // Locks in the platform-specific contract for Linux maker bin: vs.
  // packagerConfig.executableName, exercising ALL three platforms in one PR
  // CI run via loadForgeConfigForPlatform() rather than only the runner's
  // native platform.
  //
  // On Linux builds:
  //   The Linux maker `bin:` ('concord-voice') MUST equal executableName
  //   ('concord-voice' via the per-platform conditional in forge.config.ts).
  //   This equality is the load-bearing runtime invariant —
  //   @reforged/maker-appimage performs a literal-string `bin:` lookup
  //   inside the packaged app at make-time and only finds the binary
  //   because executableName produces it with the matching name. A future
  //   refactor that broke this equality (e.g., renaming the Linux maker
  //   bins to 'concordvoice' without updating executableName) would fail
  //   the next Linux build with "Could not find executable 'X' in
  //   packaged application" — the exact failure mode PR #1084 fixed.
  //   See the inline comment in forge.config.ts:93-120 for the contract;
  //   ADR-0004 documents the release-job gating that surfaced this bug
  //   class at push:main time, not the bin:/executableName contract itself.
  //
  // On macOS/Windows builds:
  //   The asymmetry IS the design — executableName is 'Concord Voice'
  //   (proper-name format visible in Activity Monitor / Task Manager /
  //   crash reports) while Linux maker bin: stays at 'concord-voice'
  //   per debian-policy §5.6.7. A future refactor that re-collapsed
  //   these would silently pass the literal-value assertions in the
  //   Deb/Rpm/AppImage blocks without this guard.
  describe.each([
    ['linux', 'aligns'],
    ['darwin', 'diverges'],
    ['win32', 'diverges'],
  ] as const)('Linux maker bin: contract on %s', (platform, expectation) => {
    let config: ForgeConfig;

    beforeAll(async () => {
      config = await loadForgeConfigForPlatform(platform);
    });

    afterAll(() => {
      // Layer 2 defense-in-depth — see executableName describe.each above.
      Object.defineProperty(process, 'platform', PRISTINE_PLATFORM_DESCRIPTOR);
    });

    it(`maker bin: ${expectation} executableName`, () => {
      const deb = getMakerOptions(config, 'Deb');
      const rpm = getMakerOptions(config, 'Rpm');
      const appImage = getMakerOptions(config, 'AppImage');
      if (expectation === 'aligns') {
        expect(deb.bin).toBe(config.packagerConfig?.executableName);
        expect(rpm.bin).toBe(config.packagerConfig?.executableName);
        expect(appImage.bin).toBe(config.packagerConfig?.executableName);
      } else {
        expect(deb.bin).not.toBe(config.packagerConfig?.executableName);
        expect(rpm.bin).not.toBe(config.packagerConfig?.executableName);
        expect(appImage.bin).not.toBe(config.packagerConfig?.executableName);
      }
    });
  });
});

describe('loadForgeConfigForPlatform helper integrity', () => {
  afterAll(() => {
    // Layer 2 defense-in-depth — symmetric with the describe.each blocks above.
    Object.defineProperty(process, 'platform', PRISTINE_PLATFORM_DESCRIPTOR);
  });

  it('returns DIFFERENT executableName values across platforms (proves vi.resetModules + mutation actually works)', async () => {
    const linuxConfig = await loadForgeConfigForPlatform('linux');
    const darwinConfig = await loadForgeConfigForPlatform('darwin');
    expect(linuxConfig.packagerConfig?.executableName).not.toBe(
      darwinConfig.packagerConfig?.executableName
    );
  });

  it('restores process.platform descriptor exactly after each call (all attributes preserved)', async () => {
    const before = Object.getOwnPropertyDescriptor(process, 'platform');
    await loadForgeConfigForPlatform('darwin');
    const after = Object.getOwnPropertyDescriptor(process, 'platform');
    // Tests descriptor-equality, not just value-equality. Catches a regression
    // where future "simplification" of the helper drops { ...originalDescriptor }
    // and reverts to { value, configurable: true }-only — which would lose
    // enumerable: true on the first call.
    expect(after).toEqual(before);
  });

  it('restores process.platform if dynamic import throws (Layer 1 try/finally works on rejection)', async () => {
    const before = Object.getOwnPropertyDescriptor(process, 'platform');

    // Force the helper's dynamic import to throw on next evaluation.
    // Vitest wraps factory throws in a module-mock-error envelope, so
    // assert on rejection alone (any throw counts) rather than message text.
    // The substantive assertion is the descriptor-equality check below — the
    // helper's try/finally must restore process.platform even when the
    // import rejects.
    vi.doMock('../../../forge.config', () => {
      throw new Error('simulated import failure (test)');
    });

    await expect(loadForgeConfigForPlatform('linux')).rejects.toThrow();

    const after = Object.getOwnPropertyDescriptor(process, 'platform');
    expect(after).toEqual(before);

    // Cleanup: remove the mock + reset modules so subsequent imports get the
    // real forge.config.ts. (vi.doUnmock alone doesn't invalidate the module
    // cache — both are needed to fully restore.)
    vi.doUnmock('../../../forge.config');
    vi.resetModules();
  });
});

// ── concord-audiocap extraResource + CI guard (#3194) ─────────────────────
//
// ADR-0043 PR 5. The .node ships OUTSIDE app.asar via extraResource because
// dlopen/LoadLibrary need a real path; the loader JS stays inside (see the
// `native lookahead` table above).
describe('native addon extraResource + CI guard (#3194)', () => {
  const NODE_REL = './native/concord-audiocap/build/Release/concord_audiocap.node';

  /**
   * Load forge.config.ts with existsSync, process.argv, CI and platform forced.
   *
   * RESTORATION COVERS FOUR PIECES OF STATE, and every one matters. Note the wording:
   * elsewhere in this file "Layer 1 / Layer 2" means two INDEPENDENT mechanisms (a
   * helper `finally` plus a caller-side `afterAll` re-restoring
   * PRISTINE_PLATFORM_DESCRIPTOR). This helper has ONE mechanism restoring four values,
   * so calling it "four-layer" borrowed the louder word for a weaker property and read
   * as more defended than it is.
   *
   * This is the first test in the repo to force `electron-forge` into process.argv, and
   * forge.config.ts has one OTHER fail-loud guard gated on exactly that — buildtag.json.
   * Leaking argv would make it throw in every later re-import in this file on a CI
   * runner, where CI=true. That is precisely the failure mode
   * loadForgeConfigForPlatform's own JSDoc warns about, and it is why the restore lives
   * in a `finally` rather than an afterEach.
   *
   * The googleClientSecret.json guard is deliberately NOT in that list: it is gated on
   * `CI === 'true' && GOOGLE_SSO_DESKTOP_REQUIRED === 'true'` (forge.config.ts:107), not
   * on argv. An earlier revision of this comment grouped it with buildtag.json as
   * "gated on exactly that", contradicting the inline note further down which had it
   * right. Because that guard reads an ambient variable, this helper PINS it rather than
   * assuming it is unset — otherwise a shell or CI job that exports it turns every case
   * in this describe into an unrelated throw.
   */
  async function loadWith(opts: {
    platform: NodeJS.Platform;
    ci: boolean;
    nodePresent: boolean;
  }): Promise<ForgeConfig> {
    const argv = process.argv;
    const ci = process.env.CI;
    // PINNED, not assumed. The googleClientSecret guard reads this variable, so a
    // caller's shell (or a future CI job) that exports it would arm a guard these cases
    // do not name and turn every one of them into an unrelated throw — a fixture with a
    // second way to fail pins none of them ([internal]rules/tests.md § Vacuity).
    const ssoRequired = process.env.GOOGLE_SSO_DESKTOP_REQUIRED;
    const platformDescriptor = Object.getOwnPropertyDescriptor(process, 'platform')!;
    const realExists = fs.existsSync;
    try {
      process.argv = [...argv, 'electron-forge'];
      process.env.CI = opts.ci ? 'true' : 'false';
      delete process.env.GOOGLE_SSO_DESKTOP_REQUIRED;
      Object.defineProperty(process, 'platform', {
        ...platformDescriptor,
        value: opts.platform,
      });
      vi.spyOn(fs, 'existsSync').mockImplementation((p) => {
        const s = String(p);
        if (s.endsWith('concord_audiocap.node')) return opts.nodePresent;
        // NEUTRALISE THE OTHER TWO CI FAIL-LOUD GUARDS, or none of the cases
        // below pins the branch it names. Forcing CI=true plus `electron-forge`
        // in argv arms all three guards at once — buildtag.json,
        // googleClientSecret.json and this PR's addon guard — so the fixture
        // acquires more than one way to throw and the assertions stop being
        // attached to the code they name ([internal]rules/tests.md § Vacuity).
        //
        // Measured, not theorised: without this the buildtag guard fired first
        // and reddened the LINUX case, which expects no throw at all.
        // googleClientSecret needs no stub — its guard additionally requires
        // GOOGLE_SSO_DESKTOP_REQUIRED='true', which this helper PINS unset above
        // (it used to merely assume the ambient environment had it unset).
        if (s.endsWith('buildtag.json')) return true;
        return realExists(s);
      });
      vi.resetModules();
      return (await import('../../../forge.config')).default;
    } finally {
      vi.restoreAllMocks();
      process.argv = argv;
      if (ci === undefined) delete process.env.CI;
      else process.env.CI = ci;
      if (ssoRequired === undefined) delete process.env.GOOGLE_SSO_DESKTOP_REQUIRED;
      else process.env.GOOGLE_SSO_DESKTOP_REQUIRED = ssoRequired;
      Object.defineProperty(process, 'platform', platformDescriptor);
      vi.resetModules();
    }
  }

  it('pushes the .node onto extraResource when it is present', async () => {
    const config = await loadWith({ platform: 'darwin', ci: false, nodePresent: true });
    expect(config.packagerConfig?.extraResource).toContain(NODE_REL);
  });

  // THE GUARD MUST BE PROVEN TO FIRE. The defect this whole PR closes is that a
  // guard-only existsSync push ships NOTHING, silently, on every leg, with every
  // leg green. Replacing it with a hard-fail guard that is itself never exercised
  // reproduces that one level up — nobody learns the guard is inert until a
  // release ships with no addon. A guard nobody has watched fail is
  // indistinguishable from no guard.
  it.each(['darwin', 'win32'] as const)(
    'hard-fails in CI packaging on %s when the .node is missing',
    async (platform) => {
      await expect(loadWith({ platform, ci: true, nodePresent: false })).rejects.toThrow(
        /concord_audiocap\.node missing/
      );
    }
  );

  // The row people skip, and the expensive one to get wrong. release-build is a
  // static six-platform matrix including linux x64/arm64, where the addon's
  // ABSENCE IS CORRECT — ADR-0043 puts Linux/PipeWire out of scope. A guard
  // written one clause too broadly turns every Linux release leg red for being
  // right, and it surfaces at release time rather than in the PR that caused it.
  it('does NOT fail on linux in CI when the .node is missing', async () => {
    const config = await loadWith({ platform: 'linux', ci: true, nodePresent: false });
    expect(config.packagerConfig?.extraResource).not.toContain(NODE_REL);
  });

  it('does not fail locally when the .node is missing', async () => {
    const config = await loadWith({ platform: 'darwin', ci: false, nodePresent: false });
    expect(config.packagerConfig?.extraResource).not.toContain(NODE_REL);
  });

  // Restoration check, in the shape of the platform-descriptor test above. If
  // argv leaked, the NEXT re-import in this file would trip the buildtag guard.
  it('restores argv, CI and platform after a throwing load', async () => {
    const argvBefore = [...process.argv];
    const ciBefore = process.env.CI;
    const platformBefore = Object.getOwnPropertyDescriptor(process, 'platform');
    await expect(loadWith({ platform: 'darwin', ci: true, nodePresent: false })).rejects.toThrow();
    expect(process.argv).toEqual(argvBefore);
    expect(process.env.CI).toBe(ciBefore);
    expect(Object.getOwnPropertyDescriptor(process, 'platform')).toEqual(platformBefore);
  });
});
