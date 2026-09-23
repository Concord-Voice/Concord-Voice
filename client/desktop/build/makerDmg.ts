import { MakerBase, type MakerOptions } from '@electron-forge/maker-base';
import type { ForgePlatform } from '@electron-forge/shared-types';
import type { DmgOptions } from 'app-builder-lib';
import path from 'node:path';

export interface MakerDmgConfig {
  appId: string;
  productName: string;
  dmg: DmgOptions;
}

/**
 * Forge 7 maker producing a DMG through the already-pinned app-builder-lib.
 * The Forge package's older electron-installer-dmg chain pulls appdmg and its
 * vulnerable image-size parser; app-builder-lib already owns the maintained
 * dmg-builder implementation used here.
 */
export class MakerDmg extends MakerBase<MakerDmgConfig> {
  name = 'dmg';
  defaultPlatforms: ForgePlatform[] = ['darwin'];

  isSupportedOnCurrentPlatform(): boolean {
    return process.platform === 'darwin';
  }

  async make(opts: MakerOptions): Promise<string[]> {
    await this.prepareConfig(opts.targetArch);

    const appPath = path.resolve(opts.dir, `${opts.appName}.app`);
    const { build } = await import('app-builder-lib');

    return build({
      // Forge passes the parent package directory; app-builder-lib expects the
      // prepackaged .app itself and preserves its existing signature.
      prepackaged: appPath,
      config: {
        appId: this.config.appId,
        productName: this.config.productName,
        directories: { output: opts.makeDir },
        dmg: {
          ...this.config.dmg,
          artifactName: `${opts.appName}-${opts.packageJSON.version}-${opts.targetArch}.dmg`,
          // The workflow signs, notarizes, and staples the outer DMG after Forge.
          sign: false,
          writeUpdateInfo: false,
        },
      },
      mac: [`dmg:${opts.targetArch}`],
      // Publishing and update metadata are owned by build-desktop.yml.
      publish: 'never',
    } as never) as Promise<string[]>;
  }
}
