/**
 * Billing launcher (#2033 PR-3b, spec §12; IPC contract 32).
 *
 * Opens Stripe's hosted Checkout or Customer Portal page in a modal child
 * `BaseWindow` (a sheet on macOS) hosting one `WebContentsView`. The view has
 * no preload, runs sandboxed on an in-memory `billing` partition that is
 * wiped on close (a failed wipe moves later launches to a fresh partition),
 * and may navigate its main frame only to the Stripe custom
 * domain and Stripe's 3DS host.
 * Stripe's redirect back to `https://concordvoice.com/billing/return` is
 * intercepted before it loads and becomes the call's result.
 *
 * The global `web-contents-created` gate in `main.ts` would externalize every
 * Stripe navigation, so it checks `isBillingWebContents` first and defers to
 * the handlers here. The view joins that set before `loadURL`.
 */
import { isIP } from 'node:net';
import { BaseWindow, WebContentsView, shell, type WebContents } from 'electron';

export type BillingKind = 'checkout' | 'portal';
export type BillingResult =
  'success' | 'cancel' | 'already_subscribed' | 'returned' | 'closed' | 'load_failed' | 'busy';

/**
 * The account's Stripe custom domain (a CNAME to Stripe that we control).
 * Stripe serves both Checkout and the Portal on it, and it is the only host
 * the launcher opens (Mark, 2026-10-23): removing the domain in Stripe
 * needs a launcher change and a shell release first.
 */
const STRIPE_CUSTOM_DOMAIN = 'pay.concordvoice.com';

/**
 * Hosts the view's MAIN frame may navigate to. Stripe's security guide runs
 * 3DS and other redirect flows in a `hooks.stripe.com` frame, so an issuer's
 * challenge page is a subframe and is not gated. Widen only from a host the
 * blocked-navigation log names (spec S12, U9).
 */
export const ALLOWED_NAV_HOSTS: ReadonlySet<string> = new Set([
  STRIPE_CUSTOM_DOMAIN,
  'hooks.stripe.com',
]);

const RETURN_ORIGIN = 'https://concordvoice.com';
const RETURN_PATH = '/billing/return';

const RESULT_FOR_RETURN: Record<string, BillingResult> = {
  success: 'success',
  cancel: 'cancel',
  portal: 'returned',
  already_subscribed: 'already_subscribed',
};

/** Chromium's net::ERR_ABORTED: a navigation we cancelled, not a failure. */
const ERR_ABORTED = -3;

const billingContents = new WeakSet<WebContents>();

let current: { win: BaseWindow } | null = null;
// In-memory partition for the next launch. A failed wipe moves later
// launches to a fresh name, so they never inherit the old Stripe cookies.
let partitionName = 'billing';
let wipeFailures = 0;

/**
 * The URL to open, or null. The host must be exactly the custom domain,
 * over https, with no userinfo and no port. There is no path check: it would
 * not bind the merchant and would break if Stripe changed its URL shape.
 */
export function validateBillingUrl(kind: unknown, url: unknown): URL | null {
  if ((kind !== 'checkout' && kind !== 'portal') || typeof url !== 'string') return null;
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return null;
  }
  const ok =
    parsed.protocol === 'https:' &&
    parsed.hostname === STRIPE_CUSTOM_DOMAIN &&
    parsed.username === '' &&
    parsed.password === '' &&
    parsed.port === '';
  return ok ? parsed : null;
}

/** The result a return URL carries, or null when `url` is not the return URL. */
export function mapReturnUrl(url: URL): BillingResult | null {
  if (url.origin !== RETURN_ORIGIN || url.pathname !== RETURN_PATH) return null;
  const key = url.searchParams.get('result') ?? '';
  // hasOwn, so `constructor` or `__proto__` cannot resolve a prototype member.
  if (Object.hasOwn(RESULT_FOR_RETURN, key)) return RESULT_FOR_RETURN[key];
  console.warn('billing: unknown return result, treating it as closed');
  return 'closed';
}

/** The hostname of `url` for a log line, never the full URL (it can carry tokens). */
function hostOf(url: string): string {
  try {
    return safeHost(new URL(url).hostname);
  } catch {
    return '(unparseable)';
  }
}

/** A DNS name as is; an IP literal is personal data, so only its category. */
function safeHost(hostname: string): string {
  return isIP(hostname.replace(/^\[|\]$/g, '')) ? '(ip literal)' : hostname;
}

/**
 * A failure's Chromium code (`ERR_…`) or name. Never its message: a load
 * error's message quotes the full URL, which carries the Checkout session.
 */
function errCode(err: unknown): string {
  const code = (err as { code?: unknown } | null)?.code;
  if (typeof code === 'string') return code;
  return err instanceof Error ? err.name : typeof err;
}

const errMessage = (err: unknown): string => (err instanceof Error ? err.message : String(err));

function openExternally(url: string): void {
  shell.openExternal(url).catch((err: unknown) => {
    console.error('billing: opening in the OS browser failed for', hostOf(url), errCode(err));
  });
}

/** True for the launcher's own view, which the global nav gate must leave alone. */
export function isBillingWebContents(contents: WebContents): boolean {
  return billingContents.has(contents);
}

/**
 * Opens `target` (already validated) and resolves when the window closes.
 * While a window is open, a second call focuses it and resolves `busy`.
 */
export function openBillingWindow(parent: BaseWindow | null, target: URL): Promise<BillingResult> {
  if (current) {
    // The window is already gone while its partition wipe finishes.
    if (!current.win.isDestroyed()) current.win.focus();
    return Promise.resolve('busy');
  }
  return new Promise((resolve) => {
    let win: BaseWindow;
    try {
      win = new BaseWindow({
        ...(parent ? { parent } : {}),
        modal: true,
        title: target.hostname,
        width: 520,
        height: 720,
      });
    } catch (err) {
      console.error('billing: window creation failed', errMessage(err));
      resolve('load_failed');
      return;
    }
    let view: WebContentsView;
    try {
      view = new WebContentsView({
        webPreferences: {
          sandbox: true,
          contextIsolation: true,
          nodeIntegration: false,
          partition: partitionName,
        },
      });
    } catch (err) {
      // Nothing is wired yet, so only the bare window needs closing.
      console.error('billing: view creation failed', errMessage(err));
      win.destroy();
      resolve('load_failed');
      return;
    }
    const contents = view.webContents;
    billingContents.add(contents);
    current = { win };

    // The first result wins: a success redirect aborts the pending load, and
    // that late rejection must not overwrite it with load_failed.
    let result: BillingResult = 'closed';
    let settled = false;
    const finish = (r: BillingResult): void => {
      if (settled || win.isDestroyed()) return;
      settled = true;
      result = r;
      win.close();
    };

    const session = contents.session;
    const onDownload = (event: Electron.Event, item: Electron.DownloadItem): void => {
      event.preventDefault();
      const url = item.getURL();
      if (url.startsWith('https:')) openExternally(url);
    };

    // The launcher stays busy until the partition wipe settles, so a quick
    // reopen cannot inherit the old Stripe cookies or lose the new session to
    // a late wipe. A throwing teardown step must not leave it busy forever.
    // Idempotent: both `closed` and a failed setup run it.
    // A platform that leaves the child open when its parent goes must not
    // orphan the sheet or leave the call pending.
    const onParentClosed = (): void => {
      if (!win.isDestroyed()) win.close();
    };

    let tornDown = false;
    const teardown = (): void => {
      if (tornDown) return;
      tornDown = true;
      try {
        parent?.removeListener('closed', onParentClosed);
        session.removeListener('will-download', onDownload);
        billingContents.delete(contents);
        contents.close();
      } catch (err) {
        console.error('billing: window teardown failed', errMessage(err));
      }
      Promise.resolve()
        .then(() => session.clearStorageData())
        .catch((err: unknown) => {
          console.error('billing: partition wipe failed', errMessage(err));
          wipeFailures += 1;
          partitionName = `billing-${wipeFailures}`;
        })
        .finally(() => {
          current = null;
          resolve(result);
        });
    };
    win.on('closed', teardown);
    parent?.once('closed', onParentClosed);

    // A synchronous throw in setup must still release the launcher and
    // settle the call, or every later billing:open answers busy.
    try {
      session.setPermissionRequestHandler((_wc, _permission, callback) => callback(false));
      session.setPermissionCheckHandler(() => false);
      session.on('will-download', onDownload);

      contents.setWindowOpenHandler(({ url }) => {
        if (url.startsWith('https:')) openExternally(url);
        return { action: 'deny' };
      });

      const gate = (event: Electron.Event<{ url: string; isMainFrame: boolean }>): void => {
        if (!event.isMainFrame) return;
        let url: URL;
        try {
          url = new URL(event.url);
        } catch {
          event.preventDefault();
          console.warn('billing: blocked navigation to an unparseable URL');
          return;
        }
        const returned = mapReturnUrl(url);
        if (returned) {
          event.preventDefault();
          finish(returned);
          return;
        }
        if (url.protocol === 'https:' && ALLOWED_NAV_HOSTS.has(url.hostname)) return;
        event.preventDefault();
        console.warn('billing: blocked navigation to', safeHost(url.hostname));
      };
      contents.on('will-navigate', gate);
      contents.on('will-redirect', gate);

      // The native title stays the validated host, whatever the page sets.
      contents.on('page-title-updated', (event) => event.preventDefault());

      contents.on('did-fail-load', (_e, code, desc, url, isMainFrame) => {
        if (!isMainFrame || code === ERR_ABORTED) return;
        console.error('billing: load failed', code, desc, 'on', hostOf(url));
        finish('load_failed');
      });

      // A crashed or killed renderer fires no load failure; without this the
      // sheet would sit blank and the call would never settle.
      contents.on('render-process-gone', (_e, details) => {
        console.error('billing: renderer gone', details.reason);
        finish('load_failed');
      });

      // Cmd/Ctrl+W closes. Escape is deliberately not bound (spec §12).
      contents.on('before-input-event', (event, input) => {
        if (
          input.type === 'keyDown' &&
          input.key.toLowerCase() === 'w' &&
          (input.meta || input.control)
        ) {
          event.preventDefault();
          finish('closed');
        }
      });

      const layout = (): void => {
        const { width, height } = win.getContentBounds();
        view.setBounds({ x: 0, y: 0, width, height });
      };
      win.on('resize', layout);

      win.contentView.addChildView(view);
      layout();
      contents.loadURL(target.toString()).catch((err: unknown) => {
        if ((err as { errno?: unknown } | null)?.errno === ERR_ABORTED) return;
        console.error('billing: initial load failed', errCode(err));
        finish('load_failed');
      });
    } catch (err) {
      console.error('billing: window setup failed', errMessage(err));
      settled = true;
      result = 'load_failed';
      teardown();
      if (!win.isDestroyed()) win.destroy();
    }
  });
}
