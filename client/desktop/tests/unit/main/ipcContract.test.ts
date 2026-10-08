// The two exported contract numbers, pinned (#3394 PR 2 introduced this file at 30;
// v31 adds `RefreshResult.mfaWebauthnOptions`).
//
// This is deliberately its own file rather than folded into `main.test.ts`: the two
// exported numbers are the whole contract, and pinning them beside the 5,000-line
// suite would bury a one-line regression under unrelated noise. See
// `[internal]rules/electron.md` § "The two contract numbers" for what each governs.
import { describe, expect, it } from 'vitest';

import { IPC_CONTRACT_VERSION, SPA_MIN_CONTRACT } from '../../../src/main/ipcContract';

describe('IPC contract numbers', () => {
  it('ships contract 31 and still demands only 19', () => {
    // `mfaWebauthnOptions` is optional (a shell without it leaves the refresh
    // challenge without a security key, as before), so the SPA's own minimum demand
    // does not move even though the shell's own capability does.
    expect(IPC_CONTRACT_VERSION).toBe(31);
    expect(SPA_MIN_CONTRACT).toBe(19);
  });
});
