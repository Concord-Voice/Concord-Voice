import { MakerBase, type MakerOptions } from '@electron-forge/maker-base';
import type { ForgePlatform } from '@electron-forge/shared-types';

export interface MakerNsisConfig {
  appId: string;
  productName: string;
  publisherName: string[];
  nsis: Record<string, unknown>;
}

/**
 * Forge 7 maker producing an NSIS installer via app-builder-lib.
 *
 * The published `electron-forge-maker-nsis` package CANNOT be used: it exports the
 * Forge-5 interface (module-level `isSupportedOnCurrentPlatform` + a default-export
 * function) and throws under Forge 7, which discriminates class makers on
 * `__isElectronForgeMaker` (@electron-forge/core/dist/api/make.js:38) and then does
 * `new MakerClass(...)` at :98 — applying `new` to a function that returns a Promise,
 * leaving `maker.platforms` undefined and throwing TypeError at :99.
 *
 * We also do NOT use app-builder-lib's `buildForge()` helper: it spreads caller options
 * AFTER setting `config.directories.output` (out/forge-maker.js:12-21), so passing our
 * own `config` (required for nsis.include / artifactName) would clobber the output
 * directory and scatter artifacts outside out/make, breaking the CI signing glob.
 *
 * See [internal]specs/2026-07-25-2402-windows-nsis-migration-design.md §3.3.1.
 */
export class MakerNsis extends MakerBase<MakerNsisConfig> {
  name = 'nsis';
  defaultPlatforms: ForgePlatform[] = ['win32'];

  isSupportedOnCurrentPlatform(): boolean {
    return true;
  }

  async make(opts: MakerOptions): Promise<string[]> {
    // MakerBase stores the constructor argument on a PRIVATE `configOrConfigFetcher`
    // and only populates the public `this.config` inside `prepareConfig()`
    // (@electron-forge/maker-base/dist/Maker.js:33-40). Forge calls that itself at
    // core/dist/api/make.js:185, one line before make() at :186 — but calling it here
    // makes make() self-sufficient rather than order-dependent on an external caller.
    // Re-preparing is benign: for an object config it is a plain re-assignment, and
    // our own registration in forge.config.ts passes an object, never a fetcher fn.
    await this.prepareConfig(opts.targetArch);

    const { build } = await import('app-builder-lib');
    return build({
      prepackaged: opts.dir,
      config: {
        appId: this.config.appId,
        productName: this.config.productName,
        directories: { output: opts.makeDir },
        // publisherName lives at win.signtoolOptions.publisherName — NOT at the config
        // root, and NOT directly on `win`. WindowsConfiguration is
        // `additionalProperties: false` in app-builder-lib's scheme.json, so either wrong
        // level makes the schema validator reject the ENTIRE config and the make dies
        // before writing a byte, failing every Windows release leg. Both wrong levels were
        // shipped and caught only by running a real build (plan Task 8) — a mocked build()
        // accepts any shape.
        //
        // It is inert in Concord's flow: signing is a post-build step over out/make using
        // Azure Trusted Signing (ADR-0032), so electron-builder never runs signtool and
        // never reads this. Passed anyway to keep forge.config.ts's single
        // ALLOWED_WINDOWS_PUBLISHERS import honest — but the #2020 lockstep is actually
        // enforced by generate-app-update.mts and the signing certificate's subject, not
        // by this maker. Do not add azureSignOptions here: the schema forbids it alongside
        // signtoolOptions, and signing is not this maker's job.
        win: { signtoolOptions: { publisherName: this.config.publisherName } },
        nsis: this.config.nsis,
      },
      win: [`nsis:${opts.targetArch}`],
      // MANDATORY — do not remove or leave undefined.
      //
      // app-builder-lib's PublishManager (out/publish/PublishManager.js:46-64) treats
      // `publish === undefined` as "decide for me", and on a CI runner that resolves to
      // `onTagOrDraft`, setting isPublish = true. That would (a) construct a
      // GitHubPublisher, which throws InvalidConfigurationError because the Windows
      // packaging step has no GH_TOKEN in scope — failing both release legs and
      // stranding the release; (b) if a token ever were in scope, make the maker a
      // second uncontrolled writer to the GitHub Release the `release` job owns; and
      // (c) emit its own latest.yml into out/make, which the release attach glob
      // (`-name "*.yml"`) would pick up alongside the canonical manifest.
      //
      // Publication is owned entirely by build-desktop.yml. The maker only builds.
      publish: 'never',
    } as never) as Promise<string[]>;
  }
}
