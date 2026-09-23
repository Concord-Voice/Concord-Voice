import { describe, it, expect, vi, beforeEach } from 'vitest';

const buildMock = vi.fn().mockResolvedValue(['/out/make/Concord Voice-0.2.47-arm64.dmg']);
vi.mock('app-builder-lib', () => ({ build: buildMock }));

import { MakerDmg } from '../../../build/makerDmg';
import type { MakerDmgConfig } from '../../../build/makerDmg';
import { validateConfiguration } from 'app-builder-lib/out/util/config/config';
import { DebugLogger } from 'builder-util';

const config = {
  appId: 'com.concordvoice.desktop',
  productName: 'Concord Voice',
  dmg: {
    background: './build/dmg-background.png',
    icon: './build/icon.icns',
    format: 'ULFO',
    contents: [
      { x: 130, y: 200, type: 'file' },
      { x: 410, y: 200, type: 'link', path: '/Applications' },
    ],
    window: { width: 540, height: 380 },
  },
} satisfies MakerDmgConfig;

describe('MakerDmg', () => {
  beforeEach(() => buildMock.mockClear());

  it('declares the interface Forge 7 requires', () => {
    const maker = new MakerDmg(config);
    expect((maker as unknown as { __isElectronForgeMaker: boolean }).__isElectronForgeMaker).toBe(
      true
    );
    expect(maker.name).toBe('dmg');
    expect(maker.platforms).toEqual(['darwin']);
    expect(maker.isSupportedOnCurrentPlatform()).toBe(process.platform === 'darwin');
  });

  it.each(['x64', 'arm64'] as const)(
    'passes the %s target and output directory',
    async (targetArch) => {
      const maker = new MakerDmg(config);
      await maker.make({
        dir: `/work/out/Concord Voice-darwin-${targetArch}`,
        makeDir: '/work/out/make',
        targetArch,
        targetPlatform: 'darwin',
        appName: 'Concord Voice',
        forgeConfig: {},
        packageJSON: { version: '0.2.47' },
      } as never);

      expect(buildMock).toHaveBeenCalledTimes(1);
      const arg = buildMock.mock.calls[0][0];
      expect(arg.prepackaged).toBe(
        `/work/out/Concord Voice-darwin-${targetArch}/Concord Voice.app`
      );
      expect(arg.mac).toEqual([`dmg:${targetArch}`]);
      expect(arg.config.directories.output).toBe('/work/out/make');
      expect(arg.config.appId).toBe('com.concordvoice.desktop');
      expect(arg.config.productName).toBe('Concord Voice');
      expect(arg.config.dmg.artifactName).toBe(`Concord Voice-0.2.47-${targetArch}.dmg`);
      expect(arg.config.dmg.sign).toBe(false);
    }
  );

  it('preserves the branded DMG layout and does not publish', async () => {
    const maker = new MakerDmg(config);
    await maker.make({
      dir: '/work/out/Concord Voice-darwin-arm64',
      makeDir: '/work/out/make',
      targetArch: 'arm64',
      targetPlatform: 'darwin',
      appName: 'Concord Voice',
      forgeConfig: {},
      packageJSON: { version: '0.2.47' },
    } as never);

    const builderConfig = buildMock.mock.calls[0][0];
    expect(builderConfig.publish).toBe('never');
    expect(builderConfig.config.dmg.writeUpdateInfo).toBe(false);
    expect(builderConfig.config.dmg).toMatchObject({
      background: './build/dmg-background.png',
      icon: './build/icon.icns',
      format: 'ULFO',
      window: { width: 540, height: 380 },
    });
    expect(builderConfig.config.dmg.contents).toEqual(config.dmg.contents);
  });

  it('matches the pinned app-builder-lib DMG schema', async () => {
    const maker = new MakerDmg(config);
    await maker.make({
      dir: '/work/out/Concord Voice-darwin-arm64',
      makeDir: '/work/out/make',
      targetArch: 'arm64',
      targetPlatform: 'darwin',
      appName: 'Concord Voice',
      forgeConfig: {},
      packageJSON: { version: '0.2.47' },
    } as never);

    await expect(
      validateConfiguration(buildMock.mock.calls[0][0].config, new DebugLogger(false))
    ).resolves.toBeUndefined();
  });

  it('returns artifact paths and propagates builder failures', async () => {
    const maker = new MakerDmg(config);
    const result = await maker.make({
      dir: '/work/out/Concord Voice-darwin-arm64',
      makeDir: '/work/out/make',
      targetArch: 'arm64',
      targetPlatform: 'darwin',
      appName: 'Concord Voice',
      forgeConfig: {},
      packageJSON: { version: '0.2.47' },
    } as never);
    expect(result).toEqual(['/out/make/Concord Voice-0.2.47-arm64.dmg']);

    buildMock.mockRejectedValueOnce(new Error('hdiutil exited 1'));
    await expect(
      maker.make({
        dir: '/work/out/Concord Voice-darwin-x64',
        makeDir: '/work/out/make',
        targetArch: 'x64',
        targetPlatform: 'darwin',
        appName: 'Concord Voice',
        forgeConfig: {},
        packageJSON: { version: '0.2.47' },
      } as never)
    ).rejects.toThrow('hdiutil exited 1');
  });
});
