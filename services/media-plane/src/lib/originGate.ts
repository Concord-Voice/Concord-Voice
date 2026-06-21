/**
 * createOriginGate — Socket.IO `cors.origin` callback factory.
 *
 * Returns a function that can be passed directly to Socket.IO's `cors.origin`
 * option.  Extracted from index.ts for testability.
 *
 * Policy (mirrors the control-plane CORS middleware in cors.go:11-12):
 *  - No-origin requests (curl, internal test harnesses) → allowed.
 *  - 'null' origin (sandboxed iframes, data: URLs) → rejected.
 *  - 'file://' origin (legacy Electron pre-#830) → rejected.
 *  - Origins in allowedOrigins (or '*') → allowed.
 *  - Everything else → rejected.
 *
 * The reject check is case-insensitive and whitespace-tolerant: 'NULL', 'File://',
 * ' file:// ' all hit the explicit-reject branch. Browsers emit canonical
 * lowercase per the Fetch spec, but non-browser clients (custom Socket.IO clients,
 * HTTP libraries with header munging) could craft variant casing — defense in
 * depth against bypass. The allowlist check remains exact-match for parity with
 * control-plane CORS (cors.go:32).
 */
export function createOriginGate(allowedOrigins: string[]) {
  return (origin: string | undefined, callback: (err: Error | null, allow?: boolean) => void) => {
    // Allow no-origin requests (native clients like curl, internal test harnesses).
    // Browser-based clients always carry an Origin header.
    if (!origin) {
      return callback(null, true);
    }
    // Reject 'null' (sandboxed iframes, data: URLs) and 'file://' (legacy Electron).
    // Mirrors the control-plane CORS middleware policy (cors.go:11-12) for parity.
    // Post-#830, the bundled desktop renderer runs at app://concord — it should be
    // in config.allowedOrigins, NOT exempt-allowed here. Case/whitespace normalized
    // on this check only to defend against non-browser-client casing variants.
    const normalized = origin.trim().toLowerCase();
    if (normalized === 'null' || normalized === 'file://') {
      return callback(new Error(`Origin ${origin} not allowed`));
    }
    if (allowedOrigins.includes('*') || allowedOrigins.includes(origin)) {
      return callback(null, true);
    }
    callback(new Error(`Origin ${origin} not allowed`));
  };
}
