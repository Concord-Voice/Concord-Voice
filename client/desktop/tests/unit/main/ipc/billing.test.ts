import { describe, it, expect, vi, beforeEach } from 'vitest';

// frameValidation is real (pure URL parsing): localhost:3001 is trusted, an arbitrary https
// origin is not. The billing window is mocked so no BaseWindow is built; validateBillingUrl
// stays real so the URL rule is the one production runs.
vi.mock('electron', () => ({
  ipcMain: { handle: vi.fn() },
}));
vi.mock('@/main/billing/billingWindow', async (importActual) => {
  const actual = await importActual<typeof import('@/main/billing/billingWindow')>();
  return { ...actual, openBillingWindow: vi.fn() };
});

import { ipcMain } from 'electron';
import { openBillingWindow } from '@/main/billing/billingWindow';
import { registerBillingIpc } from '@/main/ipc/billing';

const mockIpcMain = ipcMain as unknown as { handle: ReturnType<typeof vi.fn> };
const mockOpen = openBillingWindow as unknown as ReturnType<typeof vi.fn>;

interface FakeEvent {
  senderFrame: { url: string } | null;
}
type Handler = (event: FakeEvent, req: unknown) => Promise<{ result: string }>;

const TRUSTED: FakeEvent = { senderFrame: { url: 'http://localhost:3001' } };
const CHECKOUT = 'https://pay.concordvoice.com/c/pay/cs_test_a1';
const MAIN_WINDOW = { id: 'main' };

describe('billing:open IPC handler', () => {
  let handler: Handler;

  beforeEach(() => {
    vi.clearAllMocks();
    mockOpen.mockResolvedValue('success');
    registerBillingIpc(
      () => null,
      () => MAIN_WINDOW as never
    );
    expect(mockIpcMain.handle).toHaveBeenCalledWith('billing:open', expect.any(Function));
    handler = mockIpcMain.handle.mock.calls[0][1];
  });

  it('rejects an untrusted sender frame, logs no part of its URL, and opens nothing', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    await expect(
      handler(
        { senderFrame: { url: 'https://evil.example/page?token=abc' } },
        { kind: 'checkout', url: CHECKOUT }
      )
    ).rejects.toThrow('billing:open rejected: untrusted sender');
    expect(mockOpen).not.toHaveBeenCalled();
    const logged = warn.mock.calls.flat().join(' ');
    expect(logged).toContain('untrusted sender');
    expect(logged).not.toContain('evil.example');
    warn.mockRestore();
  });

  it('rejects a missing sender frame and opens nothing', async () => {
    await expect(
      handler({ senderFrame: null }, { kind: 'checkout', url: CHECKOUT })
    ).rejects.toThrow('billing:open rejected: untrusted sender');
    expect(mockOpen).not.toHaveBeenCalled();
  });

  it.each([
    ['a non-Stripe host', { kind: 'checkout', url: 'https://evil.example/pay' }],
    ['a stripe.com host', { kind: 'checkout', url: 'https://checkout.stripe.com/c/pay/x' }],
    ['an unknown kind', { kind: 'refund', url: CHECKOUT }],
    ['a null payload', null],
    ['a forged kind', { kind: 'x\nFAKE log line', url: CHECKOUT }],
  ])('rejects %s', async (_label, req) => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    await expect(handler(TRUSTED, req)).rejects.toThrow('billing:open rejected: invalid url');
    expect(mockOpen).not.toHaveBeenCalled();
    expect(warn).toHaveBeenCalled();
    expect(warn.mock.calls.flat().join(' ')).not.toContain('FAKE');
    warn.mockRestore();
  });

  it('opens a valid Checkout URL over the main window and returns its result', async () => {
    await expect(handler(TRUSTED, { kind: 'checkout', url: CHECKOUT })).resolves.toEqual({
      result: 'success',
    });
    expect(mockOpen).toHaveBeenCalledTimes(1);
    const [parent, target] = mockOpen.mock.calls[0];
    expect(parent).toBe(MAIN_WINDOW);
    expect((target as URL).href).toBe(CHECKOUT);
  });
});
