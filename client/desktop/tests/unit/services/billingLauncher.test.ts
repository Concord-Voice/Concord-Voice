import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  isBillingLauncherAvailable,
  openBillingSheet,
} from '@/renderer/services/system/billingLauncher';

type Bridge = { openBilling?: unknown };

function bridge(): Bridge {
  return globalThis.electron as unknown as Bridge;
}

afterEach(() => {
  delete bridge().openBilling;
});

describe('billingLauncher (#2033 PR-4)', () => {
  it('reports no launcher on a shell below IPC contract 32', () => {
    expect(isBillingLauncherAvailable()).toBe(false);
  });

  it('reports the launcher when the bridge exposes openBilling', () => {
    bridge().openBilling = vi.fn();
    expect(isBillingLauncherAvailable()).toBe(true);
  });

  it('resolves load_failed without a bridge, so nothing claims to have opened', async () => {
    await expect(openBillingSheet('checkout', 'https://pay.concordvoice.com/c/1')).resolves.toBe(
      'load_failed'
    );
  });

  it.each(['success', 'cancel', 'already_subscribed', 'returned', 'closed', 'load_failed', 'busy'])(
    'passes the %s outcome through',
    async (result) => {
      const open = vi.fn().mockResolvedValue({ result });
      bridge().openBilling = open;
      await expect(openBillingSheet('portal', 'https://pay.concordvoice.com/p/1')).resolves.toBe(
        result
      );
      expect(open).toHaveBeenCalledWith({
        kind: 'portal',
        url: 'https://pay.concordvoice.com/p/1',
      });
    }
  );

  it.each([
    ['an unknown result', { result: 'granted' }],
    ['a missing result', {}],
    ['a non-object reply', 'success'],
  ])('reads %s as load_failed', async (_label, reply) => {
    bridge().openBilling = vi.fn().mockResolvedValue(reply);
    await expect(openBillingSheet('checkout', 'https://pay.concordvoice.com/c/1')).resolves.toBe(
      'load_failed'
    );
  });

  it('reads a rejected invoke as load_failed', async () => {
    bridge().openBilling = vi.fn().mockRejectedValue(new Error('untrusted sender'));
    await expect(openBillingSheet('checkout', 'https://pay.concordvoice.com/c/1')).resolves.toBe(
      'load_failed'
    );
  });
});
