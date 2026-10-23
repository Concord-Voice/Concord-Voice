// @vitest-environment node
/**
 * Billing launcher (#2033 PR-3b, spec §12; plan Task 2). The electron mock
 * gives each window and view an event emitter, so a case drives the same
 * events Electron would and asserts what the module did with them.
 */
import { EventEmitter } from 'node:events';
import { describe, it, expect, vi, beforeEach } from 'vitest';

const openExternal = vi.fn(async () => undefined);
const windows: FakeWindow[] = [];
const views: FakeView[] = [];
let loadUrlHook: ((view: FakeView) => void) | null = null;
// Real Electron emits `closed` after close() returns; most cases close synchronously.
let deferClose = false;

class FakeSession extends EventEmitter {
  permissionRequest: ((wc: unknown, p: string, cb: (ok: boolean) => void) => void) | null = null;
  permissionCheck: (() => boolean) | null = null;
  clearStorageData = vi.fn(async () => undefined);
  setPermissionRequestHandler(h: FakeSession['permissionRequest']) {
    this.permissionRequest = h;
  }
  setPermissionCheckHandler(h: FakeSession['permissionCheck']) {
    this.permissionCheck = h;
  }
}
const billingSession = new FakeSession();

class FakeContents extends EventEmitter {
  session = billingSession;
  windowOpen: ((d: { url: string }) => { action: string }) | null = null;
  loadURL = vi.fn(async (_url: string) => {
    loadUrlHook?.(views.at(-1)!);
  });
  close = vi.fn();
  setWindowOpenHandler(h: FakeContents['windowOpen']) {
    this.windowOpen = h;
  }
}

const viewCtor: { impl: (() => void) | null } = { impl: null };
const windowCtor: { impl: (() => void) | null } = { impl: null };

class FakeView {
  webContents = new FakeContents();
  setBounds = vi.fn();
  constructor(public opts: { webPreferences: Record<string, unknown> }) {
    viewCtor.impl?.();
    views.push(this);
  }
}

class FakeWindow extends EventEmitter {
  contentView = { addChildView: vi.fn() };
  focus = vi.fn();
  destroyed = false;
  closing = false;
  constructor(public opts: Record<string, unknown>) {
    super();
    windowCtor.impl?.();
    windows.push(this);
  }
  close() {
    if (this.destroyed || this.closing) return;
    const done = () => {
      this.destroyed = true;
      this.emit('closed');
    };
    if (!deferClose) return done();
    this.closing = true;
    setTimeout(done, 0);
  }
  destroy() {
    if (this.destroyed) return;
    this.destroyed = true;
    this.emit('closed');
  }
  isDestroyed() {
    return this.destroyed;
  }
  getContentBounds() {
    return { x: 0, y: 0, width: 520, height: 720 };
  }
}

vi.mock('electron', () => ({
  BaseWindow: FakeWindow,
  WebContentsView: FakeView,
  shell: { openExternal: (...a: unknown[]) => openExternal(...(a as [])) },
}));

const mod = await import('@/main/billing/billingWindow');
const { validateBillingUrl, mapReturnUrl, isBillingWebContents, openBillingWindow } = mod;

function nav(
  view: FakeView,
  kind: 'will-navigate' | 'will-redirect',
  url: string,
  isMainFrame = true
) {
  const event = { url, isMainFrame, preventDefault: vi.fn() };
  view.webContents.emit(kind, event);
  return event;
}

const CHECKOUT = new URL('https://pay.concordvoice.com/c/pay/cs_test_1');

// Teardown releases the launcher only after the partition wipe settles.
const flush = () => new Promise((r) => setTimeout(r, 0));

beforeEach(async () => {
  for (const w of windows) w.close();
  await flush();
  windows.length = 0;
  views.length = 0;
  loadUrlHook = null;
  deferClose = false;
  openExternal.mockClear();
  billingSession.clearStorageData.mockReset();
  billingSession.clearStorageData.mockImplementation(async () => undefined);
});

describe('validateBillingUrl', () => {
  it('accepts either kind on the Stripe custom domain only', () => {
    expect(validateBillingUrl('checkout', 'https://pay.concordvoice.com/c/pay/x')?.hostname).toBe(
      'pay.concordvoice.com'
    );
    expect(validateBillingUrl('portal', 'https://pay.concordvoice.com/p/session/x')?.hostname).toBe(
      'pay.concordvoice.com'
    );
    expect(validateBillingUrl('checkout', 'https://checkout.stripe.com/c/pay/x')).toBeNull();
    expect(validateBillingUrl('portal', 'https://billing.stripe.com/p/session/x')).toBeNull();
  });

  it.each([
    'http://pay.concordvoice.com/c/pay/x',
    'https://pay.concordvoice.com.evil.example/c/pay/x',
    'https://evil.concordvoice.com/c/pay/x',
    'https://concordvoice.com/c/pay/x',
    'https://user@pay.concordvoice.com/c/pay/x',
    'https://u:p@pay.concordvoice.com/c/pay/x',
    'https://pay.concordvoice.com:8443/c/pay/x',
    'not a url',
  ])('rejects %s', (url) => {
    expect(validateBillingUrl('checkout', url)).toBeNull();
  });

  it('rejects a non-string url and an unknown kind', () => {
    expect(validateBillingUrl('checkout', 42)).toBeNull();
    expect(validateBillingUrl('refund', 'https://pay.concordvoice.com/c/pay/x')).toBeNull();
  });
});

describe('mapReturnUrl', () => {
  const ret = (q: string) => new URL(`https://concordvoice.com/billing/return${q}`);
  it('maps each known result', () => {
    expect(mapReturnUrl(ret('?result=success'))).toBe('success');
    expect(mapReturnUrl(ret('?result=cancel'))).toBe('cancel');
    expect(mapReturnUrl(ret('?result=portal'))).toBe('returned');
    expect(mapReturnUrl(ret('?result=already_subscribed'))).toBe('already_subscribed');
  });
  it('maps an unknown, missing, or prototype-member result to closed', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    expect(mapReturnUrl(ret('?result=bogus'))).toBe('closed');
    expect(mapReturnUrl(ret(''))).toBe('closed');
    expect(mapReturnUrl(ret('?result=constructor'))).toBe('closed');
    expect(mapReturnUrl(ret('?result=__proto__'))).toBe('closed');
    expect(mapReturnUrl(ret('?result=toString'))).toBe('closed');
    expect(warn).toHaveBeenCalled();
    warn.mockRestore();
  });
  it('is null for anything that is not the return URL', () => {
    expect(
      mapReturnUrl(new URL('https://concordvoice.com/billing/other?result=success'))
    ).toBeNull();
    expect(
      mapReturnUrl(new URL('http://concordvoice.com/billing/return?result=success'))
    ).toBeNull();
    expect(mapReturnUrl(new URL('https://evil.example/billing/return?result=success'))).toBeNull();
  });
});

describe('openBillingWindow', () => {
  it('builds a modal child with a locked-down view, exempted before it loads', async () => {
    const parent = new EventEmitter() as never;
    let exemptAtLoad = false;
    loadUrlHook = (view) => {
      exemptAtLoad = isBillingWebContents(view.webContents as never);
    };
    const done = openBillingWindow(parent, CHECKOUT);
    const [win] = windows;
    const [view] = views;
    expect(win.opts).toMatchObject({ parent, modal: true, title: 'pay.concordvoice.com' });
    expect(view.opts.webPreferences).toEqual({
      sandbox: true,
      contextIsolation: true,
      nodeIntegration: false,
      partition: 'billing',
    });
    expect(view.webContents.loadURL).toHaveBeenCalledWith(CHECKOUT.toString());
    expect(exemptAtLoad).toBe(true);
    win.close();
    await expect(done).resolves.toBe('closed');
  });

  it('denies every permission request and check', () => {
    void openBillingWindow(null, CHECKOUT);
    const cb = vi.fn();
    billingSession.permissionRequest?.({}, 'media', cb);
    expect(cb).toHaveBeenCalledWith(false);
    expect(billingSession.permissionCheck?.()).toBe(false);
  });

  it('resolves the mapped result on the return URL and closes', async () => {
    const done = openBillingWindow(null, CHECKOUT);
    const event = nav(
      views[0],
      'will-navigate',
      'https://concordvoice.com/billing/return?result=success'
    );
    expect(event.preventDefault).toHaveBeenCalled();
    await expect(done).resolves.toBe('success');
    expect(windows[0].isDestroyed()).toBe(true);
    expect(views[0].webContents.close).toHaveBeenCalled();
    expect(billingSession.clearStorageData).toHaveBeenCalled();
  });

  it('keeps the host title when the page sets its own', () => {
    void openBillingWindow(null, CHECKOUT);
    const event = { preventDefault: vi.fn() };
    views[0].webContents.emit('page-title-updated', event, 'Spoofed Bank');
    expect(event.preventDefault).toHaveBeenCalled();
  });

  it('treats will-redirect like will-navigate', async () => {
    const done = openBillingWindow(null, CHECKOUT);
    nav(views[0], 'will-redirect', 'https://concordvoice.com/billing/return?result=cancel');
    await expect(done).resolves.toBe('cancel');
  });

  it('lets the custom domain and the 3DS host through, and no other stripe.com host', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    void openBillingWindow(null, CHECKOUT);
    for (const host of ['checkout.stripe.com', 'billing.stripe.com']) {
      expect(nav(views[0], 'will-navigate', `https://${host}/x`).preventDefault).toHaveBeenCalled();
    }
    warn.mockRestore();
    for (const host of ['pay.concordvoice.com', 'hooks.stripe.com']) {
      expect(
        nav(views[0], 'will-navigate', `https://${host}/x`).preventDefault
      ).not.toHaveBeenCalled();
    }
  });

  it('blocks any other main-frame host, logs the host only, and stays open', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    void openBillingWindow(null, CHECKOUT);
    const event = nav(views[0], 'will-navigate', 'https://bank.example/acs/secret-path?token=abc');
    expect(event.preventDefault).toHaveBeenCalled();
    expect(openExternal).not.toHaveBeenCalled();
    const logged = warn.mock.calls.flat().join(' ');
    expect(logged).toContain('bank.example');
    expect(logged).not.toContain('secret-path');
    expect(logged).not.toContain('token');
    expect(windows[0].isDestroyed()).toBe(false);
    warn.mockRestore();
  });

  it.each(['https://203.0.113.7/x', 'https://[2001:db8::1]/x'])(
    'blocks an IP literal and logs only its category: %s',
    (target) => {
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
      void openBillingWindow(null, CHECKOUT);
      expect(nav(views[0], 'will-navigate', target).preventDefault).toHaveBeenCalled();
      const logged = warn.mock.calls.flat().join(' ');
      expect(logged).toContain('(ip literal)');
      expect(logged).not.toMatch(/203\.0|2001:db8/);
      warn.mockRestore();
    }
  );

  it('blocks a foreign host on will-redirect too', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    void openBillingWindow(null, CHECKOUT);
    expect(
      nav(views[0], 'will-redirect', 'https://bank.example/acs').preventDefault
    ).toHaveBeenCalled();
    expect(warn.mock.calls.flat().join(' ')).toContain('bank.example');
    expect(windows[0].isDestroyed()).toBe(false);
    warn.mockRestore();
  });

  it('blocks http: even on an allowlisted host, and a malformed URL', () => {
    void openBillingWindow(null, CHECKOUT);
    expect(
      nav(views[0], 'will-navigate', 'http://pay.concordvoice.com/x').preventDefault
    ).toHaveBeenCalled();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    expect(nav(views[0], 'will-navigate', 'not a url').preventDefault).toHaveBeenCalled();
    expect(warn).toHaveBeenCalledWith('billing: blocked navigation to an unparseable URL');
    warn.mockRestore();
  });

  it('does not gate subframe navigations', () => {
    void openBillingWindow(null, CHECKOUT);
    expect(
      nav(views[0], 'will-navigate', 'https://bank.example/acs', false).preventDefault
    ).not.toHaveBeenCalled();
  });

  it('cancels downloads and sends https ones to the OS browser', () => {
    void openBillingWindow(null, CHECKOUT);
    const item = { getURL: () => 'https://pay.stripe.com/invoice/x/pdf', cancel: vi.fn() };
    const event = { preventDefault: vi.fn() };
    billingSession.emit('will-download', event, item);
    expect(event.preventDefault).toHaveBeenCalled();
    expect(openExternal).toHaveBeenCalledWith('https://pay.stripe.com/invoice/x/pdf');
    const other = { getURL: () => 'data:application/pdf,x', cancel: vi.fn() };
    billingSession.emit('will-download', { preventDefault: vi.fn() }, other);
    expect(openExternal).toHaveBeenCalledTimes(1);
  });

  it('logs a failed OS-browser hand-off by hostname only', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    openExternal.mockRejectedValueOnce(new Error('no handler'));
    void openBillingWindow(null, CHECKOUT);
    views[0].webContents.windowOpen!({ url: 'https://stripe.com/legal?token=abc' });
    await flush();
    const logged = error.mock.calls.flat().join(' ');
    expect(logged).toContain('stripe.com');
    expect(logged).not.toContain('token');
    error.mockRestore();
  });

  it('removes its will-download listener when the window closes', async () => {
    const done = openBillingWindow(null, CHECKOUT);
    expect(billingSession.listenerCount('will-download')).toBe(1);
    windows[0].close();
    await done;
    expect(billingSession.listenerCount('will-download')).toBe(0);
  });

  it('externalizes https window.open and denies every popup', () => {
    void openBillingWindow(null, CHECKOUT);
    const open = views[0].webContents.windowOpen!;
    expect(open({ url: 'https://stripe.com/legal' })).toEqual({ action: 'deny' });
    expect(openExternal).toHaveBeenCalledWith('https://stripe.com/legal');
    expect(open({ url: 'javascript:alert(1)' })).toEqual({ action: 'deny' });
    expect(openExternal).toHaveBeenCalledTimes(1);
  });

  it('resolves load_failed on a main-frame load failure only', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const done = openBillingWindow(null, CHECKOUT);
    views[0].webContents.emit('did-fail-load', {}, -105, 'NAME_NOT_RESOLVED', 'https://x', false);
    views[0].webContents.emit('did-fail-load', {}, -3, 'ABORTED', 'https://x', true);
    expect(windows[0].isDestroyed()).toBe(false);
    views[0].webContents.emit('did-fail-load', {}, -105, 'NAME_NOT_RESOLVED', 'https://x', true);
    await expect(done).resolves.toBe('load_failed');
    expect(error.mock.calls.flat()).toEqual(expect.arrayContaining([-105, 'NAME_NOT_RESOLVED']));
    error.mockRestore();
  });

  it('settles load_failed when the renderer process goes away', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const done = openBillingWindow(null, CHECKOUT);
    views[0].webContents.emit('render-process-gone', {}, { reason: 'crashed', exitCode: 1 });
    await expect(done).resolves.toBe('load_failed');
    expect(error.mock.calls.flat()).toEqual(['billing: renderer gone', 'crashed']);
    error.mockRestore();
  });

  it('closes on Cmd/Ctrl+W and ignores Escape', async () => {
    const done = openBillingWindow(null, CHECKOUT);
    const esc = { preventDefault: vi.fn() };
    views[0].webContents.emit('before-input-event', esc, {
      type: 'keyDown',
      key: 'Escape',
      meta: false,
      control: false,
    });
    expect(windows[0].isDestroyed()).toBe(false);
    const w = { preventDefault: vi.fn() };
    views[0].webContents.emit('before-input-event', w, {
      type: 'keyDown',
      key: 'w',
      meta: true,
      control: true,
    });
    expect(w.preventDefault).toHaveBeenCalled();
    await expect(done).resolves.toBe('closed');
  });

  it('allows one window at a time and settles each call once', async () => {
    const first = openBillingWindow(null, CHECKOUT);
    await expect(openBillingWindow(null, CHECKOUT)).resolves.toBe('busy');
    expect(windows).toHaveLength(1);
    expect(windows[0].focus).toHaveBeenCalled();
    nav(views[0], 'will-navigate', 'https://concordvoice.com/billing/return?result=success');
    windows[0].close();
    await expect(first).resolves.toBe('success');
    const second = openBillingWindow(null, CHECKOUT);
    expect(windows).toHaveLength(2);
    windows[1].close();
    await expect(second).resolves.toBe('closed');
  });

  it('closes the sheet and settles closed when the parent closes', async () => {
    const parent = new EventEmitter();
    const done = openBillingWindow(parent as never, CHECKOUT);
    parent.emit('closed');
    await expect(done).resolves.toBe('closed');
    expect(windows[0].isDestroyed()).toBe(true);
    expect(isBillingWebContents(views[0].webContents as never)).toBe(false);
    expect(parent.listenerCount('closed')).toBe(0);
  });

  it('detaches from the parent when the sheet closes first', async () => {
    const parent = new EventEmitter();
    const done = openBillingWindow(parent as never, CHECKOUT);
    windows[0].close();
    await expect(done).resolves.toBe('closed');
    expect(parent.listenerCount('closed')).toBe(0);
  });

  it('keeps a success result when the redirect aborts the pending load', async () => {
    deferClose = true;
    loadUrlHook = (view) => {
      nav(view, 'will-redirect', 'https://concordvoice.com/billing/return?result=success');
      throw Object.assign(new Error('ERR_ABORTED (-3)'), { errno: -3, code: 'ERR_ABORTED' });
    };
    await expect(openBillingWindow(null, CHECKOUT)).resolves.toBe('success');
  });

  it('latches the first result against any later failure', async () => {
    deferClose = true;
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    loadUrlHook = (view) => {
      nav(view, 'will-navigate', 'https://concordvoice.com/billing/return?result=cancel');
      throw new Error('ERR_FAILED');
    };
    await expect(openBillingWindow(null, CHECKOUT)).resolves.toBe('cancel');
    error.mockRestore();
  });

  it('stays busy until the partition wipe settles', async () => {
    let wiped!: () => void;
    billingSession.clearStorageData.mockImplementationOnce(
      () => new Promise<undefined>((r) => (wiped = () => r(undefined)))
    );
    const first = openBillingWindow(null, CHECKOUT);
    windows[0].close();
    await flush();
    await expect(openBillingWindow(null, CHECKOUT)).resolves.toBe('busy');
    let settled = false;
    void first.then(() => (settled = true));
    await flush();
    expect(settled).toBe(false);
    wiped();
    await expect(first).resolves.toBe('closed');
    const second = openBillingWindow(null, CHECKOUT);
    expect(windows).toHaveLength(2);
    windows[1].close();
    await expect(second).resolves.toBe('closed');
  });

  it('logs a failed wipe and moves the next launch to a fresh partition', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    billingSession.clearStorageData.mockRejectedValueOnce(new Error('disk'));
    const done = openBillingWindow(null, CHECKOUT);
    windows[0].close();
    await expect(done).resolves.toBe('closed');
    expect(error.mock.calls.flat()).toContain('billing: partition wipe failed');
    // The next launch must not reuse the partition that kept the old cookies.
    const next = openBillingWindow(null, CHECKOUT);
    expect(views[1].opts.webPreferences.partition).not.toBe(views[0].opts.webPreferences.partition);
    windows[1].close();
    await expect(next).resolves.toBe('closed');
    error.mockRestore();
  });

  it('releases the launcher even when a teardown step throws', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const done = openBillingWindow(null, CHECKOUT);
    views[0].webContents.close.mockImplementationOnce(() => {
      throw new Error('already gone');
    });
    windows[0].close();
    await expect(done).resolves.toBe('closed');
    expect(billingSession.clearStorageData).toHaveBeenCalled();
    expect(error.mock.calls.flat()).toContain('billing: window teardown failed');
    error.mockRestore();
  });

  it('releases the launcher and settles load_failed when setup throws', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const bounds = vi.spyOn(FakeWindow.prototype, 'getContentBounds').mockImplementationOnce(() => {
      throw new Error('window gone');
    });
    await expect(openBillingWindow(null, CHECKOUT)).resolves.toBe('load_failed');
    expect(windows[0].isDestroyed()).toBe(true);
    expect(billingSession.clearStorageData).toHaveBeenCalledTimes(1);
    expect(billingSession.listenerCount('will-download')).toBe(0);
    expect(isBillingWebContents(views[0].webContents as never)).toBe(false);
    expect(error.mock.calls.flat()).toContain('billing: window setup failed');
    bounds.mockRestore();
    const next = openBillingWindow(null, CHECKOUT);
    expect(windows).toHaveLength(2);
    windows[1].close();
    await expect(next).resolves.toBe('closed');
    error.mockRestore();
  });

  it('settles load_failed when the window cannot be built', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    windowCtor.impl = () => {
      throw new Error('no display');
    };
    await expect(openBillingWindow(null, CHECKOUT)).resolves.toBe('load_failed');
    windowCtor.impl = null;
    expect(views).toHaveLength(0);
    expect(error.mock.calls.flat()).toContain('billing: window creation failed');
    const next = openBillingWindow(null, CHECKOUT);
    expect(windows).toHaveLength(1);
    windows[0].close();
    await expect(next).resolves.toBe('closed');
    error.mockRestore();
  });

  it('closes the bare window and settles load_failed when the view cannot be built', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    viewCtor.impl = () => {
      throw new Error('renderer allocation failed');
    };
    await expect(openBillingWindow(null, CHECKOUT)).resolves.toBe('load_failed');
    viewCtor.impl = null;
    expect(windows[0].isDestroyed()).toBe(true);
    expect(error.mock.calls.flat()).toContain('billing: view creation failed');
    const next = openBillingWindow(null, CHECKOUT);
    expect(windows).toHaveLength(2);
    windows[1].close();
    await expect(next).resolves.toBe('closed');
    error.mockRestore();
  });

  it('settles load_failed when loadURL rejects', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    loadUrlHook = (view) => {
      throw Object.assign(new Error(`ERR_NAME_NOT_RESOLVED (-105) loading '${CHECKOUT}'`), {
        view,
        code: 'ERR_NAME_NOT_RESOLVED',
      });
    };
    await expect(openBillingWindow(null, CHECKOUT)).resolves.toBe('load_failed');
    expect(error.mock.calls.flat()).toEqual([
      'billing: initial load failed',
      'ERR_NAME_NOT_RESOLVED',
    ]);
    error.mockRestore();
  });
});
