import { describe, it, expect } from 'vitest';
import type { ScreenAudioInterruptReason } from '@/shared/audiocapProtocol';

/**
 * `src/renderer/utils/policy/screenAudioInterruptCopy.ts` does not exist yet
 * (#3394 PR 2 T5b — written test-first). A literal import specifier would fail
 * `tsc --noEmit` for the whole project, which another concurrent agent's work
 * depends on staying green. TypeScript does not statically resolve a dynamic
 * `import()` whose specifier is a variable, so this file type-checks today and
 * fails only AT RUNTIME (module not found) — the correct RED state.
 */
const MODULE_PATH = '../../../../src/renderer/utils/policy/screenAudioInterruptCopy';

interface ScreenAudioInterruptCopyModule {
  screenAudioInterruptMessage: (reason: ScreenAudioInterruptReason) => string;
}

async function loadCopyModule(): Promise<ScreenAudioInterruptCopyModule> {
  return (await import(/* @vite-ignore */ MODULE_PATH)) as ScreenAudioInterruptCopyModule;
}

const REASONS: ScreenAudioInterruptReason[] = [
  'capture-interrupted',
  'child-crash',
  'protocol-fault',
];

const EXPECTED =
  'App sound stopped. Your screen is still being shared. Turn on Share sound to try again.';

describe('screenAudioInterruptMessage (#3394 PR 2 T5b)', () => {
  it.each(REASONS)(
    '%s reads the one interrupted sentence and never blames permissions',
    async (reason) => {
      const { screenAudioInterruptMessage } = await loadCopyModule();
      const text = screenAudioInterruptMessage(reason);
      expect(text).toBe(EXPECTED);
      expect(text).not.toMatch(/denied|blocked|permission|failed/i);
      expect(text).not.toMatch(/whole screen/i);
    }
  );

  // F3: a bare index lookup would hand back an inherited member for a key like
  // `'constructor'` (a function, not a string) or silently return `undefined` for
  // any other unrecognised string. The guard (`typeof reason === 'string' &&
  // Object.hasOwn(...)`) must fall back to the same INTERRUPTED sentence for both,
  // and the result must be a string — never a function or `undefined`.
  it('an inherited key falls back to the interrupted sentence, not the inherited member', async () => {
    const { screenAudioInterruptMessage } = await loadCopyModule();

    const text = screenAudioInterruptMessage('constructor' as never);

    expect(text).toBe(EXPECTED);
    expect(typeof text).toBe('string');
  });

  it('an unrecognised reason string falls back to the interrupted sentence', async () => {
    const { screenAudioInterruptMessage } = await loadCopyModule();

    const text = screenAudioInterruptMessage('some-unknown-reason' as never);

    expect(text).toBe(EXPECTED);
    expect(typeof text).toBe('string');
  });
});
