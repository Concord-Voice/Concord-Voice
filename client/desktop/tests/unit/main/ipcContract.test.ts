// #3394 PR 2 Task T4 — the `audiocap:interrupted` push, contract 30.
//
// This is deliberately its own file rather than folded into `main.test.ts`: the two
// exported numbers are the whole contract, and pinning them beside the 5,000-line
// suite would bury a one-line regression under unrelated noise. See
// `[internal]rules/electron.md` § "The two contract numbers" for what each governs.
import { describe, expect, it } from 'vitest';

import { IPC_CONTRACT_VERSION, SPA_MIN_CONTRACT } from '../../../src/main/ipcContract';

describe('IPC contract numbers (#3394 PR 2, T4)', () => {
  it('ships contract 30 and still demands only 19', () => {
    // `audiocap:interrupted` is additive/feature-detected (a shell without it simply
    // never delivers the notice — see the preload bridge tests), so the SPA's own
    // minimum demand does not move even though the shell's own capability does.
    expect(IPC_CONTRACT_VERSION).toBe(30);
    expect(SPA_MIN_CONTRACT).toBe(19);
  });
});
