/**
 * Frame-origin validation for main-process IPC handlers.
 *
 * Every main-process IPC handler that performs sensitive work (reading the
 * clientId, opening the browser, touching the keychain, ...) MUST validate
 * `event.senderFrame.url` against this allowlist before acting. Accepting an
 * IPC call from an attacker-controlled frame undoes context isolation.
 *
 * The four legitimate renderer frame origins are:
 *   - file://           legacy packaged bundled build (pre-#830, retained
 *                       for backward compatibility with older shells)
 *   - app://concord     packaged bundled build post-#830 (privileged
 *                       custom scheme registered in main.ts via
 *                       protocol.registerSchemesAsPrivileged)
 *   - http://localhost: dev server (Vite)
 *   - https://<validated-remote-spa-origin>  packaged remote SPA (dynamic)
 *
 * file:// with any path is acceptable here because Electron's scheme gating
 * prevents a renderer from loading arbitrary file:// origins at runtime —
 * loadFile only serves from the packaged app bundle. The app:// scheme is
 * similarly gated by the protocol handler in main.ts (appProtocol.ts
 * validates host==='concord' and rejects path traversal), so an attacker
 * cannot forge an `app://concord` frame from a remote page — the protocol
 * handler only serves it from the asar bundle.
 *
 * The localhost regex anchors on a numeric port followed by `/` or end-of-
 * string to close scheme-injection bypasses like http://localhost:3000@evil/
 * or http://localhost:3000.evil.com/.
 */

import { SPA_CACHE_HOST, SPA_CACHE_SCHEME } from '../spaCache/manifestSchema';

const LOCAL_DEV_PATTERN = /^http:\/\/localhost:\d+(\/|$)/;
const BUNDLED_PREFIX = 'file://';
const BUNDLED_APP_HOST = 'concord';

/** True if the frame URL is the post-#830 bundled renderer (`app://concord`).
 *
 * Mirrors `isValidPipOpenSender` (pipUrl.ts): parse and compare
 * protocol + host, NOT origin. Per the WHATWG URL spec, `URL.origin` returns
 * the literal string "null" for non-special schemes like `app:`, so an
 * origin comparison would never match. Exact `host === 'concord'` rejects
 * look-alikes (`app://concord.evil.com`, `app://concordX`); non-special
 * schemes preserve host case, so the comparison is intentionally
 * case-sensitive against the lowercase host the protocol handler registers.
 * Fail-closed on malformed URLs. */
function isBundledAppUrl(url: string): boolean {
  try {
    const parsed = new URL(url);
    if (parsed.protocol === 'app:' && parsed.host === BUNDLED_APP_HOST) {
      return true;
    }
    // #1870: the verified last-known-good SPA cache serves from the dedicated
    // privileged spa-cache://concord scheme - the same class of first-party,
    // protocol-gated local origin as app://concord (cacheProtocol.ts serves only
    // signature+hash-verified bytes), so it is trusted identically.
    return parsed.protocol === `${SPA_CACHE_SCHEME}:` && parsed.host === SPA_CACHE_HOST;
  } catch {
    return false;
  }
}

/** True if the frame URL matches any permitted origin.
 *
 * Permitted frames: legacy `file://` bundled build (pre-#830), the post-#830
 * `app://concord` bundled build, the `http://localhost:*` dev server, and the
 * validated remote-SPA origin (when one is active). `app://concord` is a
 * first-party, protocol-gated origin (see the module header) and is trusted
 * here exactly as `file://` was — it is the post-#830 replacement for it.
 * This resolves the #806 deferral: the bundled-SPA renderer IS allowed to
 * invoke the attestation IPC, shell.openExternal, and the SSO loopback,
 * because those are legitimate operations the bundled build needs and the
 * frame can only originate from the asar bundle the protocol handler serves. */
export function isPermittedFrameUrl(url: string, remoteSpaOrigin: string | null): boolean {
  if (url.startsWith(BUNDLED_PREFIX)) return true;
  if (isBundledAppUrl(url)) return true;
  if (LOCAL_DEV_PATTERN.test(url)) return true;
  if (remoteSpaOrigin && url.startsWith(remoteSpaOrigin + '/')) return true;
  return false;
}
