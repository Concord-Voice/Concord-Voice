/**
 * Regression test for the #3104 NC-1 `no-restricted-syntax` selector and for the
 * flat-config shadowing class that a `files:`-scoped block introduces.
 *
 * Two independent properties are locked here:
 *
 *  1. **NC-1 reach.** The selector must fire anywhere under `src/renderer/**`,
 *     not only under `src/renderer/services/voice/**`. `getIceServersForPip()`
 *     is public on the exported `voiceService` singleton, so any component,
 *     store, hook, or error boundary can import it and log the credentialed
 *     list. It must still permit the one sanctioned reducer,
 *     `describeIceServers(...)`.
 *
 *  2. **No narrower block silently drops a selector.** Flat-config
 *     `files:`-scoped blocks replace a rule key WHOLESALE, and the last matching
 *     block wins. A nested block that re-lists only some of the broad
 *     `src/renderer/**` selectors turns the rest OFF for its subtree with no
 *     diagnostic anywhere. That is what happened to the two #754 bare-anchor
 *     selectors and the two #1586 `resolveMediaUrl` selectors under
 *     `src/renderer/services/voice/**`.
 *
 * Property 2 is checked against the EFFECTIVE config (`calculateConfigForFile`),
 * not against the config array, so it also catches the placement half of the
 * trap — a superset block positioned BEFORE the broad block is still discarded.
 */
import { ESLint } from 'eslint';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

const CLIENT_DESKTOP_ROOT = path.resolve(__dirname, '../../..');

/** A plain renderer file that no narrower block targets — the baseline. */
const BASELINE_RENDERER_FILE = 'src/renderer/App.tsx';

/**
 * Renderer files that a narrower `files:` block has targeted, or plausibly
 * could. Each must carry EVERY selector the broad renderer block declares.
 * `voiceService.ts` is the #3104 regression itself.
 */
const NESTED_RENDERER_FILES = [
  'src/renderer/services/voice/voiceService.ts',
  'src/renderer/services/voice/pipVoiceClient.ts',
  'src/renderer/services/system/logBufferService.ts',
];

async function effectiveSelectors(relPath: string): Promise<string[]> {
  const eslint = new ESLint({ cwd: CLIENT_DESKTOP_ROOT });
  const config = await eslint.calculateConfigForFile(path.join(CLIENT_DESKTOP_ROOT, relPath));
  const rule = config.rules?.['no-restricted-syntax'];
  if (!Array.isArray(rule)) return [];
  return rule
    .slice(1)
    .map((entry: unknown) =>
      typeof entry === 'string' ? entry : ((entry as { selector?: string }).selector ?? '')
    )
    .filter((s): s is string => s.length > 0);
}

async function lintRenderer(
  code: string
): Promise<Array<{ ruleId: string | null; message: string }>> {
  const eslint = new ESLint({ cwd: CLIENT_DESKTOP_ROOT });
  const [result] = await eslint.lintText(code, {
    filePath: path.join(CLIENT_DESKTOP_ROOT, 'src/renderer/__lint-fixture__.tsx'),
  });
  return result.messages.map((m) => ({ ruleId: m.ruleId, message: m.message }));
}

function hasNc1Violation(messages: Array<{ ruleId: string | null; message: string }>): boolean {
  return messages.some((m) => m.ruleId === 'no-restricted-syntax' && /NC-1/.test(m.message));
}

describe('no-restricted-syntax — NC-1 ICE-server console guard (#3104)', () => {
  it.each([
    ['getPipSession', `console.debug(await globalThis.electron.getPipSession('pip-1'));`],
    ['pipSessionChannelName', `console.log(pipSessionChannelName(t));`],
  ])('flags the #3104 D6 PiP session capability logged via %s', async (_name, line) => {
    const messages = await lintRenderer(`
      import { pipSessionChannelName } from '@/renderer/services/voice/pipSignalingTypes';
      export const Panel = async (t: string) => {
        ${line}
        return null;
      };
    `);
    expect(hasNc1Violation(messages)).toBe(true);
  });

  it('flags voiceService.getIceServersForPip() logged from OUTSIDE services/voice', async () => {
    const messages = await lintRenderer(`
      import { voiceService } from '@/renderer/services/voice/voiceService';
      export const Panel = () => {
        console.debug(voiceService.getIceServersForPip());
        return null;
      };
    `);
    expect(hasNc1Violation(messages)).toBe(true);
  });

  it('flags an ICE list nested inside an object argument', async () => {
    const messages = await lintRenderer(`
      export const Panel = ({ iceServers }: { iceServers: unknown }) => {
        console.debug('transport', { opts: { iceServers } });
        return null;
      };
    `);
    expect(hasNc1Violation(messages)).toBe(true);
  });

  it('flags an ICE list passed through JSON.stringify', async () => {
    const messages = await lintRenderer(`
      export const Panel = ({ pipIceServers }: { pipIceServers: unknown }) => {
        console.warn(JSON.stringify(pipIceServers));
        return null;
      };
    `);
    expect(hasNc1Violation(messages)).toBe(true);
  });

  it('flags a raw list logged ALONGSIDE the permitted reducer', async () => {
    const messages = await lintRenderer(`
      declare function describeIceServers(x: unknown): string;
      export const Panel = ({ iceServers }: { iceServers: unknown }) => {
        console.debug(describeIceServers(iceServers), iceServers);
        return null;
      };
    `);
    expect(hasNc1Violation(messages)).toBe(true);
  });

  it('permits the sanctioned describeIceServers(...) reducer', async () => {
    const messages = await lintRenderer(`
      declare function describeIceServers(x: unknown): string;
      export const Panel = ({ iceServers }: { iceServers: unknown }) => {
        console.debug('[ice] servers', describeIceServers(iceServers));
        return null;
      };
    `);
    expect(hasNc1Violation(messages)).toBe(false);
  });
});

describe('flat-config shadowing — nested renderer blocks keep every broad selector', () => {
  it('the baseline renderer file declares a non-empty selector set', async () => {
    const baseline = await effectiveSelectors(BASELINE_RENDERER_FILE);
    // Vacuity guard: if the broad block ever stops declaring selectors, the
    // superset assertions below become tautologies.
    expect(baseline.length).toBeGreaterThanOrEqual(6);
  });

  it.each(NESTED_RENDERER_FILES)(
    'effective no-restricted-syntax for %s is a superset of the broad renderer block',
    async (relPath) => {
      const baseline = await effectiveSelectors(BASELINE_RENDERER_FILE);
      const nested = await effectiveSelectors(relPath);
      expect(nested).toEqual(expect.arrayContaining(baseline));
    }
  );
});
