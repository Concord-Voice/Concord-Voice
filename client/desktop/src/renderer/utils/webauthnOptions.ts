import { z } from 'zod';
import { base64urlToBuffer } from './crypto/base64url';

// The server's `webauthn_options` for an MFA challenge. Sign-in, session
// refresh and SSO all send the same shape, so every screen that offers a
// security key parses it here. The object schemas strip unknown keys, so a
// server cannot add `extensions`, `hints` or anything else to the ceremony.
// The caps are generous: go-webauthn sends a 43-character challenge, and a
// credential id is at most 1023 bytes (1364 base64url characters).
//
// Every server shares one renderer origin, so the browser's own rpId check
// cannot tell them apart. The rpId is therefore bound to the server the proof
// goes to: it must be that API host exactly. Otherwise a server could name
// another server's relying party and relay the user's assertion there, and a
// parent domain is no exception, since a delegated or compromised subdomain
// could claim its parent's (#3663 review).
const AuthenticatorTransportSchema = z.enum([
  'ble',
  'hybrid',
  'internal',
  'nfc',
  'smart-card',
  'usb',
]);
const WebAuthnRequestSchema = z.object({
  challenge: z.string().min(1).max(1024),
  timeout: z.number().nonnegative().optional(),
  rpId: z.string().min(1).max(253),
  allowCredentials: z
    .array(
      z.object({
        type: z.literal('public-key'),
        id: z.string().min(1).max(1366),
        transports: z.array(AuthenticatorTransportSchema).max(6).optional(),
      })
    )
    .max(64)
    .optional(),
  userVerification: z.enum(['discouraged', 'preferred', 'required']).optional(),
});
const WebAuthnServerOptionsSchema = z.union([
  WebAuthnRequestSchema,
  z.object({ publicKey: WebAuthnRequestSchema }).transform(({ publicKey }) => publicKey),
]);

// The one known deployment whose relying party is not its API host: the
// official service, API `api.concordvoice.chat` (main's PRODUCTION_API_BASE)
// with rpId `concordvoice.chat`. A self-hosted server with that layout offers
// no security key until a server can prove such a mapping.
const OFFICIAL_RP_IDS = new Map([['api.concordvoice.chat', 'concordvoice.chat']]);

/** Whether `rpId` is the relying party of `apiBase`: its host, or the official mapping. */
export function rpIdBelongsTo(rpId: string, apiBase: string): boolean {
  let host: string;
  try {
    host = new URL(apiBase).hostname.toLowerCase();
  } catch {
    return false;
  }
  const id = rpId.toLowerCase();
  return id === host || OFFICIAL_RP_IDS.get(host) === id;
}

/** `apiBase` is the server the assertion will be sent to. */
export function parseWebAuthnOptions(
  serverOptions: unknown,
  apiBase: string
): PublicKeyCredentialRequestOptions {
  const parsed = WebAuthnServerOptionsSchema.safeParse(serverOptions);
  if (!parsed.success) throw new Error('Server returned invalid WebAuthn options.');
  const pk = parsed.data;
  if (!rpIdBelongsTo(pk.rpId, apiBase)) {
    throw new Error('Server returned WebAuthn options for another server.');
  }
  const opts: PublicKeyCredentialRequestOptions = {
    challenge: base64urlToBuffer(pk.challenge),
    timeout: pk.timeout,
    rpId: pk.rpId,
  };
  if (pk.allowCredentials) {
    opts.allowCredentials = pk.allowCredentials.map((cred) => ({
      type: cred.type,
      id: base64urlToBuffer(cred.id),
      // TypeScript's lib.dom omits WebAuthn Level 3's smart-card value. The
      // closed Zod enum above matches the backend's go-webauthn v0.17.4 contract.
      transports: cred.transports?.map((transport) => transport as AuthenticatorTransport),
    }));
  }
  if (pk.userVerification) {
    opts.userVerification = pk.userVerification;
  }
  return opts;
}

/** For every MFA challenge. Missing or malformed options, or options for
 *  another server, leave the security key unavailable; the challenge still
 *  opens with its other methods, where a throw would abort the sign-in. */
export function webauthnOptionsOrNull(
  serverOptions: unknown,
  apiBase: string
): PublicKeyCredentialRequestOptions | null {
  if (serverOptions == null) return null;
  try {
    return parseWebAuthnOptions(serverOptions, apiBase);
  } catch {
    // A fixed string: the payload carries the challenge, so it is never logged.
    console.warn('[MFA] server WebAuthn options failed validation');
    return null;
  }
}

/**
 * The methods a challenge can offer. `webauthn` with no usable options would
 * open on a security-key pane with nothing behind it (an older shell, or
 * options that failed validation), so it is dropped while another method is
 * left. Alone, it stays: dropping it would leave the challenge nothing to offer.
 */
export function methodsForOptions(
  methods: unknown,
  options: PublicKeyCredentialRequestOptions | null
): string[] {
  // An older shell passes the server's lists over IPC unchecked, so a value
  // that is not a string array becomes an empty one here (#3663 review).
  const listed = Array.isArray(methods)
    ? methods.filter((m): m is string => typeof m === 'string')
    : [];
  if (options !== null) return listed;
  const others = listed.filter((m) => m !== 'webauthn');
  return others.length > 0 ? others : listed;
}
