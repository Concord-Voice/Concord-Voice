import { formatFileSize } from '../crypto/attachmentCrypto';
import {
  CHUNK_OVERHEAD_BYTES,
  ENVELOPE_HEADER_BYTES,
  ENVELOPE_VERSION_V3,
  totalChunksFor,
} from '../crypto/attachmentChunkedCrypto';

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

/** Renderer-memory ceiling for the LEGACY single-shot upload path.
 *
 *  Renamed from INTERIM_CLIENT_ATTACHMENT_CEILING_BYTES, value unchanged.
 *
 *  PR 2's spec said this constant would be DELETED. It cannot be. Concord is
 *  self-hostable, so a current desktop build can be pointed at a control plane
 *  that predates the upload-session routes; on that fallback the client is still
 *  doing whole-file `arrayBuffer()` -> one `crypto.subtle.encrypt` -> a `Blob`,
 *  and the ~3x transient is still real. What changes is that the clamp is now
 *  CONDITIONAL on the server capability rather than unconditional.
 *
 *  On our own edge the legacy fallback tops out well below this anyway (nginx
 *  40M, Cloudflare ~100 MB); the client surfaces that as the 413 it already
 *  handles. Self-hosted edges vary, and a 413 is the authoritative answer. */
export const LEGACY_UPLOAD_PATH_CEILING_BYTES = 134_217_728;

/** Ceiling for the WHOLE-FILE image path.
 *
 *  Metadata stripping needs the entire image in hand — EXIF and XMP are
 *  rewritten, not skipped — so this is the one place a whole-file transient
 *  survives the chunked format. Worst case is raw + stripped + one chunk, about
 *  264 MiB at this ceiling, which is strictly better than the ~384 MiB PR 1
 *  permitted at the same 128 MiB and never measured.
 *
 *  Non-images are chunk-read and bounded to roughly two chunks regardless of
 *  size, so they are not subject to this at all. Bounded memory at ANY file
 *  size is therefore unreachable as #2157 words it, and the docs say so rather
 *  than implying otherwise.
 *
 *  This is the rollback lever if the §7 harness finds the bound optimistic. */
export const IMAGE_STRIP_MAX_BYTES = 134_217_728;

/** Download-side guard.
 *
 *  Derived, never a magic number. It must admit a maximum-size envelope of the
 *  LARGEST format this build can read: the 28-byte per-file header plus 28 bytes
 *  (IV + tag) per chunk.
 *
 *  DERIVED FROM v3, AND THE VERSION IS LOAD-BEARING. v3 gives 28 bytes of chunk
 *  0's plaintext to the header, and the premium ceiling is an exact multiple of
 *  the chunk size -- so those 28 bytes spill into a whole extra chunk: 33 chunks
 *  (952 bytes of overhead) where v2 needs 32 (924). Sized to v2 this guard is 28
 *  bytes short, and `readBoundedBody` CANCELS the transfer the moment the body
 *  passes it, so a maximum-size premium attachment becomes undownloadable. Both
 *  counts are pinned server-side by TestPremiumCeilingChunkCountsPerVersion.
 *
 *  The previous value was PREMIUM + 4096, with a comment claiming the 4096
 *  mirrored the server's multipart-header allowance. That derivation is wrong
 *  for this format -- it happened to be large enough, which is not the same as
 *  being correct. Expressing the arithmetic means a future chunk-size change
 *  moves this with it instead of silently outgrowing it.
 *
 *  Sized to the PREMIUM entitlement rather than the legacy upload ceiling on
 *  purpose: the download path faces bytes the server already holds, and the
 *  chunked upload path produces files above that ceiling which this build must
 *  still open. Invariant: upload ceiling <= download capability. */
export const MAX_DECRYPTABLE_ATTACHMENT_BYTES =
  PREMIUM_ATTACHMENT_BYTES +
  ENVELOPE_HEADER_BYTES +
  CHUNK_OVERHEAD_BYTES * totalChunksFor(PREMIUM_ATTACHMENT_BYTES, ENVELOPE_VERSION_V3);

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
   *  ServerLimitUnlimited }`) and the header of `utils/policy/serverEntitlements.ts`.
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
  /** Whether the connected control plane exposes the chunked upload session.
   *
   *  REQUIRED, not optional-defaulting-to-false. A default would let a caller
   *  that forgets to wire the capability silently inherit the safe branch --
   *  safe, but it would pin premium users at the legacy ceiling with no signal
   *  that anything was missing. Making it required forces the decision to be
   *  visible at every call site. */
  chunkedUploadSupported: boolean;
  /** True when the capability could not be FETCHED, as opposed to the server
   *  answering no. Both clamp to the legacy ceiling; only the copy differs, and
   *  telling a user their plan is the reason when the real reason is a network
   *  blip is the part worth getting right. */
  capabilityUnknown?: boolean;
}

export interface AttachmentLimit {
  /** The number enforced AND named in copy. */
  limitBytes: number;
  /** Which input won — selects the copy branch. */
  /** Which input won — selects the copy branch.
   *
   *  Does NOT collapse to a single value: the legacy fallback survives, so both
   *  branches remain reachable. */
  source:
    | 'entitlement'
    | 'legacy-upload-path'
    | 'capability-unknown'
    /** Clamped to what this build can DECRYPT. Reachable when a server-wide
     *  Mach grant lifts the composed limit above the renderer's measured memory
     *  ceiling -- the file would upload and then open for nobody. */
    | 'decryptable-ceiling';
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

  // 3. Clamp DOWN to what this build can actually encrypt, but ONLY on the
  //    legacy single-shot path. With the chunked session available the
  //    transient is bounded to about two chunks regardless of file size, so
  //    there is nothing left for a ceiling to protect.
  //
  //    Fail-closed direction: an absent or unknown capability means `false`,
  //    which keeps the ceiling. Never the other way round.
  const pathCeiling = input.chunkedUploadSupported
    ? composed
    : Math.min(composed, LEGACY_UPLOAD_PATH_CEILING_BYTES);

  // 4. Clamp DOWN to what this build can DECRYPT, on every path.
  //
  //    The server-wide Mach grant lifts a member to 512 MiB, but
  //    MAX_DECRYPTABLE_ATTACHMENT_BYTES is derived from the 256 MiB premium
  //    entitlement -- and it is not arbitrary, it is where measurement put the
  //    renderer's memory ceiling. So a 512 MiB attachment could be uploaded and
  //    then opened by nobody, including its own author.
  //
  //    That mismatch was latent while the legacy ceiling capped uploads at
  //    128 MiB. The chunked path is what makes 512 MiB reachable, so it is this
  //    change's job to keep the invariant the constant already claims:
  //    upload ceiling <= download capability.
  //
  //    Deliberately CLIENT-SIDE only. The server tier stays as sold, because a
  //    client that can produce and consume 512 MiB should still get it; this
  //    build reports its own capability rather than redefining the product.
  const limitBytes = Math.min(pathCeiling, MAX_DECRYPTABLE_ATTACHMENT_BYTES);

  // Four-way, and none of the clamped cases are interchangeable: the caller
  // words each differently, so the REASON has to survive to here, not just the
  // number. A server that answered "no", a server we could not ask, and a build
  // that cannot open the file are three different things to tell a user.
  let source: AttachmentLimit['source'] = 'entitlement';
  if (limitBytes < pathCeiling) {
    source = 'decryptable-ceiling';
  } else if (limitBytes < composed) {
    source = input.capabilityUnknown === true ? 'capability-unknown' : 'legacy-upload-path';
  }

  return { limitBytes, source, entitlementBytes: composed };
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
