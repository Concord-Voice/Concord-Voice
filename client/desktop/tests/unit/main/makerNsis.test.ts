import { describe, it, expect, vi, beforeEach } from 'vitest';

const buildMock = vi.fn().mockResolvedValue(['/out/make/ConcordVoiceSetup.exe']);
vi.mock('app-builder-lib', () => ({ build: buildMock }));

import { MakerNsis } from '../../../build/makerNsis';

const config = {
  appId: 'com.concordvoice.desktop',
  productName: 'Concord Voice',
  publisherName: ['Concord Voice LLC'],
  nsis: { oneClick: true, perMachine: false },
};

describe('MakerNsis', () => {
  beforeEach(() => buildMock.mockClear());

  it('declares the interface Forge 7 requires', () => {
    const maker = new MakerNsis(config);
    // __isElectronForgeMaker is the discriminator at core/api/make.js:38.
    // Without it Forge takes the legacy branch and throws on maker.platforms.
    expect((maker as unknown as { __isElectronForgeMaker: boolean }).__isElectronForgeMaker).toBe(
      true
    );
    expect(maker.name).toBe('nsis');
    expect(maker.platforms).toEqual(['win32']);
    expect(maker.isSupportedOnCurrentPlatform()).toBe(true);
  });

  it('calls build() with prepackaged dir, nsis config, and the target arch', async () => {
    const maker = new MakerNsis(config);
    await maker.make({
      dir: '/work/out/ConcordVoice-win32-x64',
      makeDir: '/work/out/make',
      targetArch: 'x64',
      targetPlatform: 'win32',
      appName: 'Concord Voice',
      forgeConfig: {},
      packageJSON: { version: '0.2.34' },
    } as never);

    expect(buildMock).toHaveBeenCalledTimes(1);
    const arg = buildMock.mock.calls[0][0];
    expect(arg.prepackaged).toBe('/work/out/ConcordVoice-win32-x64');
    expect(arg.win).toEqual(['nsis:x64']);
    expect(arg.config.directories.output).toBe('/work/out/make');
    expect(arg.config.nsis.oneClick).toBe(true);
  });

  it('nests publisherName under win.signtoolOptions, not at the config root', async () => {
    // Shipped at the root first. electron-builder's schema validator rejected the whole
    // config ("unknown property 'publisherName'") and the make failed before writing a
    // byte — so every Windows release leg would have died. Found only by running a real
    // build (plan Task 8), because a mocked build() accepts any shape at all.
    //
    // Asserted in BOTH directions on purpose: the positive alone would still pass if a
    // future edit re-added the root key alongside the nested one, which is the exact
    // config the validator rejects.
    const maker = new MakerNsis(config);
    await maker.make({
      dir: '/work/out/ConcordVoice-win32-x64',
      makeDir: '/work/out/make',
      targetArch: 'x64',
      targetPlatform: 'win32',
      appName: 'Concord Voice',
      forgeConfig: {},
      packageJSON: { version: '0.2.35' },
    } as never);

    const cfg = buildMock.mock.calls[0][0].config;
    expect(cfg.win.signtoolOptions.publisherName).toEqual(['Concord Voice LLC']);
    // WindowsConfiguration is additionalProperties:false, so BOTH wrong levels are fatal.
    // Each was shipped in turn; assert against both.
    expect(cfg).not.toHaveProperty('publisherName');
    expect(cfg.win).not.toHaveProperty('publisherName');
  });

  it('propagates a build() failure rather than resolving empty', async () => {
    // Error-path coverage per [internal]rules/tests.md. A swallowed rejection here would
    // let Forge believe the Windows leg produced artifacts when it produced none — the
    // same silent-success class as the upload glob that matched zero files.
    buildMock.mockRejectedValueOnce(new Error('makensis exited 1'));
    const maker = new MakerNsis(config);
    await expect(
      maker.make({
        dir: '/work/out/ConcordVoice-win32-x64',
        makeDir: '/work/out/make',
        targetArch: 'x64',
        targetPlatform: 'win32',
        appName: 'Concord Voice',
        forgeConfig: {},
        packageJSON: { version: '0.2.35' },
      } as never)
    ).rejects.toThrow('makensis exited 1');
  });

  it("passes publish: 'never' so CI cannot trigger implicit publishing", async () => {
    // app-builder-lib's PublishManager treats undefined as "decide for me" and resolves
    // to onTagOrDraft on a CI runner (out/publish/PublishManager.js:46-64), setting
    // isPublish = true — which constructs a GitHubPublisher with no token in scope and
    // fails the release leg. Publication belongs to build-desktop.yml, not the maker.
    const maker = new MakerNsis(config);
    await maker.make({
      dir: '/work/out/ConcordVoice-win32-x64',
      makeDir: '/work/out/make',
      targetArch: 'x64',
      targetPlatform: 'win32',
      appName: 'Concord Voice',
      forgeConfig: {},
      packageJSON: { version: '0.2.35' },
    } as never);
    expect(buildMock.mock.calls[0][0].publish).toBe('never');
  });

  it('returns the artifact paths build() produced', async () => {
    const maker = new MakerNsis(config);
    const result = await maker.make({
      dir: '/work/out/ConcordVoice-win32-arm64',
      makeDir: '/work/out/make',
      targetArch: 'arm64',
      targetPlatform: 'win32',
      appName: 'Concord Voice',
      forgeConfig: {},
      packageJSON: { version: '0.2.34' },
    } as never);
    expect(result).toEqual(['/out/make/ConcordVoiceSetup.exe']);
  });
});
