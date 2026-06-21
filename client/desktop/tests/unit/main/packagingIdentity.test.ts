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

// ── Constants ────────────────────────────────────────────────────────────

const EXPECTED_DISPLAY_NAME = 'Concord Voice';

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

// ── Tests ────────────────────────────────────────────────────────────────

describe('Packaging Identity (#382)', () => {
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

  describe('forge.config.ts — MakerSquirrel (Windows)', () => {
    it('has name set to "ConcordVoice" (no spaces — registry safe)', async () => {
      const config = await loadForgeConfig();
      const squirrel = getMakerOptions(config, 'Squirrel');
      expect(squirrel).not.toBeNull();
      expect(squirrel.name).toBe('ConcordVoice');
    });

    it('has setupExe set and containing "ConcordVoice"', async () => {
      const config = await loadForgeConfig();
      const squirrel = getMakerOptions(config, 'Squirrel');
      expect(squirrel.setupExe).toBeTruthy();
      expect(squirrel.setupExe).toContain('ConcordVoice');
    });

    it('setupExe does not use generic names', async () => {
      const config = await loadForgeConfig();
      const squirrel = getMakerOptions(config, 'Squirrel');
      const exe = squirrel.setupExe?.toLowerCase() ?? '';
      expect(exe).not.toBe('setup.exe');
      expect(exe).not.toContain('electron');
    });

    it('has description set', async () => {
      const config = await loadForgeConfig();
      const squirrel = getMakerOptions(config, 'Squirrel');
      expect(squirrel.description).toBeTruthy();
      expect(squirrel.description.length).toBeGreaterThan(10);
    });

    it('has copyright set', async () => {
      const config = await loadForgeConfig();
      const squirrel = getMakerOptions(config, 'Squirrel');
      expect(squirrel.copyright).toBeTruthy();
      expect(squirrel.copyright).toContain('Concord');
    });

    it('has owners set', async () => {
      const config = await loadForgeConfig();
      const squirrel = getMakerOptions(config, 'Squirrel');
      expect(squirrel.owners).toBeTruthy();
    });

    it('has loadingGif set for branded installer splash', async () => {
      const config = await loadForgeConfig();
      const squirrel = getMakerOptions(config, 'Squirrel');
      expect(squirrel.loadingGif).toBeTruthy();
    });

    it('has iconUrl set to HTTPS URL', async () => {
      const config = await loadForgeConfig();
      const squirrel = getMakerOptions(config, 'Squirrel');
      expect(squirrel.iconUrl).toBeTruthy();
      expect(squirrel.iconUrl).toMatch(/^https:\/\//);
    });

    it('name is the spaceless form of packagerConfig.name', async () => {
      const config = await loadForgeConfig();
      const squirrel = getMakerOptions(config, 'Squirrel');
      const expected = config.packagerConfig?.name?.replaceAll(/\s+/g, '');
      expect(squirrel.name).toBe(expected);
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
