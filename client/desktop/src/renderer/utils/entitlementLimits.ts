import { formatFileSize } from './attachmentCrypto';

export const FREE_MESSAGE_CHARS = 5120;
export const PREMIUM_MESSAGE_CHARS = 10240;

/** 32 MiB — mirrors the Go free floor (`entitlements.go:111`) and the public
 *  Groundspeed "32 MB per file" pricing row. */
export const FREE_ATTACHMENT_BYTES = 33_554_432;

/** 256 MiB — the premium USER axis (`entitlements.go:138`).
 *
 *  Was `536_870_912` until #2157. That is the Mach 3 SERVER-wide number, a
 *  different axis of the ladder entirely, so the composer told free users that
 *  Premium raises their per-file limit to 512 MB when the personal ceiling it
 *  buys is 256 MB. */
export const PREMIUM_ATTACHMENT_BYTES = 268_435_456;

/** Interim renderer-memory ceiling (#2157 PR 1, provisional).
 *
 *  `encryptAndBuildForm` does a whole-file `arrayBuffer()` → one
 *  `crypto.subtle.encrypt` → a `Blob` copy, so the transient is ~3x file size
 *  and the premium 256 MiB entitlement is not reachable on this path.
 *
 *  DELETED BY PR 2, which replaces the single-shot path with a chunked wire
 *  format and removes the reason for a ceiling at all. */
export const INTERIM_CLIENT_ATTACHMENT_CEILING_BYTES = 134_217_728;

/** Download-side guard.
 *
 *  Sized to the PREMIUM entitlement, deliberately NOT to the interim upload
 *  ceiling: the download path faces bytes the server already holds, and PR 2
 *  will produce files above the interim ceiling that this build must still be
 *  able to open. Invariant: upload ceiling <= download capability.
 *  The +4096 slack mirrors the server's multipart-header allowance
 *  (`media/handlers.go:222`). */
export const MAX_DECRYPTABLE_ATTACHMENT_BYTES = PREMIUM_ATTACHMENT_BYTES + 4096;

export function clampMessageCharsForTier(tier: string, value: number): number {
  const ceiling = tier === 'premium' ? PREMIUM_MESSAGE_CHARS : FREE_MESSAGE_CHARS;
  if (!Number.isFinite(value)) return ceiling;
  return Math.max(1, Math.min(Math.trunc(value), ceiling));
}

export interface AttachmentLimitInput {
  /** `entitlement.maxAttachmentBytes`, read unconditionally.
   *
   *  Never gate this on `hydrated`/`degraded`. The store is already
   *  `FREE_ENTITLEMENT` when unhydrated and preserves last-known-good on a
   *  reconnect blip (#2172), so a hydration branch here could only ever
   *  ESCALATE above the store's current value — which is exactly what the
   *  monetization invariant forbids. */
  userMaxAttachmentBytes: number;
  /** #1556 SEAM — not supplied by any caller in PR 1.
   *
   *  A NEGATIVE value is a real sentinel meaning "unlimited (selfhost)" — see
   *  `entitlements.go:211-215` (`if server.MaxServerUploadBytes < 0 { return
   *  ServerLimitUnlimited }`) and the header of `utils/serverEntitlements.ts`.
   *  PR 1 deliberately does NOT honour it: `isUsable` treats anything
   *  non-finite or `<= 0` as ABSENT, so a selfhost server falls back to the
   *  user axis rather than to unlimited. That is fail-closed and costs nothing
   *  today, because the server enforces the user axis alone
   *  (`media/handlers.go:218-222`) — honouring "unlimited" here would let the
   *  client accept a file the server answers with 413.
   *
   *  #1556 MUST translate the sentinel to a real byte ceiling before passing it
   *  in; the Go caller contract says the same thing in the same words. */
  serverMaxUploadBytes?: number;
}

export interface AttachmentLimit {
  /** The number enforced AND named in copy. */
  limitBytes: number;
  /** Which input won — selects the copy branch. */
  source: 'entitlement' | 'client-ceiling';
  /** The composed entitlement before the ceiling clamp, so the interim-ceiling
   *  copy can honestly name both numbers ("your plan allows X, this version
   *  sends up to Y"). */
  entitlementBytes: number;
}

function isUsable(v: number | undefined): v is number {
  return typeof v === 'number' && Number.isFinite(v) && v > 0;
}

/**
 * Resolve the effective client-side attachment limit.
 *
 * Pure: no store access, no hydration awareness, no I/O. The server remains
 * authoritative (`http.MaxBytesReader` at `media/handlers.go:222`); this limit
 * exists so the user learns the answer before spending an encrypt and an
 * upload on a 413.
 */
export function resolveAttachmentLimit(input: AttachmentLimitInput): AttachmentLimit {
  // 1. Sanitize. A nonsense entitlement falls back to the free floor, never up.
  const user = isUsable(input.userMaxAttachmentBytes)
    ? input.userMaxAttachmentBytes
    : FREE_ATTACHMENT_BYTES;

  // 2. Compose the axes. #1556 supplies `serverMaxUploadBytes` at the call
  //    site; this branch already exists, so nothing here reshapes when it does.
  //
  //    `max` is the product rule, not a bug: the server-wide Mach grant LIFTS
  //    every member (`entitlement-matrix.md` §2 item 4 — a Supersonic user on a
  //    Mach 3 server gets 512 MB). Do not "harden" this into `min`; that would
  //    silently delete the server axis the moment #1556 wires it.
  //
  //    Read it as an OWN property. The caller passes an object literal, so a
  //    bare `input.serverMaxUploadBytes` resolves up the prototype chain and a
  //    polluted `Object.prototype` could hand a free user a composed limit far
  //    above their entitlement (#2157 adversarial review, VULN-003).
  const serverAxis = Object.hasOwn(input, 'serverMaxUploadBytes')
    ? input.serverMaxUploadBytes
    : undefined;
  const composed = isUsable(serverAxis) ? Math.max(user, serverAxis) : user;

  // 3. Clamp DOWN to what this build can actually encrypt without exhausting
  //    the renderer. Only ever narrows.
  const limitBytes = Math.min(composed, INTERIM_CLIENT_ATTACHMENT_CEILING_BYTES);

  return {
    limitBytes,
    source: limitBytes < composed ? 'client-ceiling' : 'entitlement',
    entitlementBytes: composed,
  };
}

/**
 * `formatFileSize` with a trailing `.0` trimmed, so a LIMIT renders the way the
 * pricing page writes it ("32 MB", "256 MB") while a MEASURED file size keeps
 * its precision ("180.5 MB"). Copy must not lie about precision in either
 * direction, so the two formatters stay distinct.
 */
export function formatLimitBytes(bytes: number): string {
  return formatFileSize(bytes).replace(/\.0(?= )/, '');
}
