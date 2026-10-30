// @vitest-environment node
import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { compileFunction, createContext, runInContext } from 'node:vm';

const require = createRequire(import.meta.url);
const helpers = () =>
  require('./renderer-probe.js') as {
    assertPreloadEnvironment(value: unknown): void;
    runRendererProbe(
      contents: { executeJavaScript(code: string, gesture: boolean): Promise<unknown> },
      label: string,
      source: string,
      gesture?: boolean
    ): Promise<unknown>;
  };

function captureFixture(processInfo: Record<string, unknown>) {
  const page: Record<string, unknown> = {};
  const RendererContext = createContext({ window: page });
  let frozen = false;
  const preload = compileFunction(
    readFileSync(new URL('./environment-preload.js', import.meta.url), 'utf8'),
    ['require', 'process', 'exports', 'module', 'global'],
    { parsingContext: RendererContext }
  );
  preload(
    (name: string) => {
      expect(name).toBe('electron');
      return {
        contextBridge: {
          exposeInMainWorld(key: string, value: object) {
            frozen = Object.isFrozen(value);
            page[key] = structuredClone(value);
          },
        },
      };
    },
    processInfo,
    {},
    {},
    {}
  );
  return { page, world: RendererContext, frozen };
}

describe('actual fixture preload in Electron parameter-only process scope', () => {
  it('captures real supplied flags without requiring or exposing a global process', async () => {
    const { page, world, frozen } = captureFixture({
      sandboxed: true,
      contextIsolated: true,
      type: 'renderer',
    });
    expect(runInContext('typeof process', world)).toBe('undefined');
    expect(frozen).toBe(true);
    const value = await helpers().runRendererProbe(
      {
        async executeJavaScript(code) {
          return structuredClone(await runInContext(code, world));
        },
      },
      'fixture preload',
      'window.engineProbeEnvironment'
    );
    expect(() => helpers().assertPreloadEnvironment(value)).not.toThrow();
    expect(value).toEqual({ sandboxed: true, contextIsolated: true, type: 'renderer' });
    expect(Object.keys(page)).toEqual(['engineProbeEnvironment']);
  });

  it.each([
    {},
    { sandboxed: false, contextIsolated: true, type: 'renderer' },
    { sandboxed: true, contextIsolated: false, type: 'renderer' },
    { sandboxed: true, contextIsolated: true, type: 'browser' },
    { sandboxed: 'true', contextIsolated: true, type: 'renderer' },
  ])(
    'the actual fixture must preserve unsafe observed flags so the consumer refuses them: %j',
    (processInfo) => {
      const { page } = captureFixture(processInfo);
      expect(() => helpers().assertPreloadEnvironment(page.engineProbeEnvironment)).toThrow(
        'actual sandboxed preload'
      );
    }
  );

  it.each([
    undefined,
    {},
    { sandboxed: false, contextIsolated: true, type: 'renderer' },
    { sandboxed: true, contextIsolated: false, type: 'renderer' },
    { sandboxed: true, contextIsolated: true, type: 'browser' },
    { sandboxed: 'true', contextIsolated: true, type: 'renderer' },
    { sandboxed: true, contextIsolated: true, type: 'renderer', require: 'unexpected' },
  ])('refuses missing, mistyped or unsafe preload observations: %j', (value) => {
    expect(() => helpers().assertPreloadEnvironment(value)).toThrow('actual sandboxed preload');
  });
});

describe('renderer failures preserve their stage and bounded actual cause', () => {
  const FailureContext = createContext({});
  const contents = {
    async executeJavaScript(code: string) {
      return structuredClone(await runInContext(code, FailureContext));
    },
  };
  it('returns a successful renderer value', async () => {
    await expect(
      helpers().runRendererProbe(contents, 'positive control', 'Promise.resolve(42)')
    ).resolves.toBe(42);
  });
  it('reports the old isolated-world global-process failure with its stage', async () => {
    await expect(
      helpers().runRendererProbe(contents, 'preload isolation', 'process.sandboxed')
    ).rejects.toThrow('preload isolation: process is not defined');
  });
  it('bounds a renderer error rather than forwarding arbitrary console data', async () => {
    try {
      await helpers().runRendererProbe(
        contents,
        'bounded',
        'Promise.reject(new Error("x".repeat(10000)))'
      );
      expect.unreachable();
    } catch (error) {
      expect(error).toBeInstanceOf(Error);
      expect((error as Error).message).toHaveLength('bounded: '.length + 512);
    }
  });
  it('refuses an absent renderer result', async () => {
    await expect(
      helpers().runRendererProbe(
        {
          async executeJavaScript() {
            return undefined;
          },
        },
        'missing',
        '1'
      )
    ).rejects.toThrow('missing: renderer returned no probe result');
  });
});
