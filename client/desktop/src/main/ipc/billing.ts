/**
 * The `billing:open` invoke (#1305, #2033 PR-3b, IPC contract 32).
 *
 * Two fences, in this order: the sender frame must be a trusted SPA frame, and
 * the URL must be exactly the kind's Stripe host (`validateBillingUrl`). Only
 * then does main open the launcher window; the renderer never chooses where
 * the view may navigate after that.
 */
import { ipcMain, type BrowserWindow, type IpcMainInvokeEvent } from 'electron';

import {
  openBillingWindow,
  validateBillingUrl,
  type BillingResult,
} from '../billing/billingWindow';

import { requireTrustedSender } from './frameValidation';

export function registerBillingIpc(
  getRemoteSpaBaseUrl: () => string | null,
  getMainWindow: () => BrowserWindow | null
): void {
  ipcMain.handle(
    'billing:open',
    async (event: IpcMainInvokeEvent, req: unknown): Promise<{ result: BillingResult }> => {
      if (!requireTrustedSender(event, getRemoteSpaBaseUrl())) {
        console.warn('billing:open rejected an untrusted sender');
        throw new Error('billing:open rejected: untrusted sender');
      }
      const r = req as { kind?: unknown; url?: unknown } | null;
      const url = validateBillingUrl(r?.kind, r?.url);
      if (!url) {
        // A closed category only: the raw field is renderer-controlled.
        const kind = r?.kind === 'checkout' || r?.kind === 'portal' ? r.kind : 'unknown';
        console.warn(`billing:open rejected an invalid url for kind ${kind}`);
        throw new Error('billing:open rejected: invalid url');
      }
      return { result: await openBillingWindow(getMainWindow(), url) };
    }
  );
}
