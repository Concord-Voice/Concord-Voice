/**
 * Chunked AES-256-GCM attachment wire format (v2 + v3) — #2157.
 *
 * Layout:
 *   blob   = header(28) || chunk[0] || … || chunk[n-1]
 *   header = magic:2 "CV" | version:1 | flags:1 | chunkSize:4 BE
 *          | totalChunks:4 BE | fileNonce:16
 *   chunk  = IV:12 | AES-256-GCM(slice) || tag:16
 *
 * THE ONE DIFFERENCE BETWEEN v2 AND v3 IS CHUNK 0'S PLAINTEXT BUDGET.
 *
 *   v2 — every chunk carries `chunkSize` plaintext, and chunk 0's part ALSO
 *        carries the 28-byte header, so upload part 0 is 28 bytes larger than
 *        every other non-trailing part.
 *   v3 — chunk 0 carries `chunkSize - 28`, paying for the header out of its own
 *        plaintext, so every non-trailing part is byte-identical.
 *
 * v3 exists because Cloudflare R2 refuses a multipart upload whose non-trailing
 * parts differ in size (error 10048, S3 code `InvalidPart`: "All non-trailing
 * parts must have the same size"). S3 and MinIO both permit non-uniform parts,
 * which is why v2 shipped and why nothing caught it — but under v2 every
 * attachment over 16 MiB is unuploadable to R2, on every entitlement tier.
 *
 * THIS BUILD READS BOTH, FOREVER, BUT WRITES v2 UNTIL ENVELOPE SUPPORT IS
 * NEGOTIATED. The existing `chunkedAttachmentUpload` capability does not name
 * an envelope version, so writing v3 under that boolean would break old servers
 * and old desktop clients. Every chunked attachment already stored is a v2
 * blob; dropping v2 read support is data loss.
 *
 * The header is a PARSE HINT WITH ZERO TRUST. Concord is self-hostable, so a
 * server that rewrites stored bytes could edit any unauthenticated field — see
 * the note at the top of `boundedResponseBody.ts`. Authority comes from the
 * AAD, which binds version, fileNonce, chunkIndex, totalChunks and chunkSize
 * into every chunk's tag.
 *
 * This is the deliberate divergence from media frame crypto v3/v5
 * (`[internal]rules/e2ee.md:283-340`), which decrypts with NO AAD because a
 * tampered frame degrades to a dropped frame. An attachment cannot take that
 * posture: reorder, truncation and cross-file splicing must fail closed
 * (OWASP A08:2021). Do not "harmonise" the two formats.
 *
 * The predecessor single-shot format lives in `./attachmentCrypto` and is
 * still the format of every attachment already stored in the field.
 */

/** Plaintext bytes per chunk.
 *
 *  Not a tunable preference: S3 multipart enforces a 5 MiB minimum part size
 *  (`minio-go/v7@v7.3.0/constants.go:24`, `absMinPartSize`), last part exempt,
 *  and 8 MiB is the smallest power of two above that floor. It is also bound
 *  into every chunk's GCM AAD, so it can never become operator-tunable —
 *  changing it would invalidate stored ciphertext. */
import { decryptFile } from './attachmentCrypto';

export const CHUNK_PLAINTEXT_BYTES = 8_388_608;

/** magic 2 + version 1 + flags 1 + chunkSize 4 + totalChunks 4 + fileNonce 16 */
export const ENVELOPE_HEADER_BYTES = 28;

/** IV 12 + GCM tag 16, per chunk. */
export const CHUNK_OVERHEAD_BYTES = 28;

const MAGIC_0 = 0x43; // 'C'
const MAGIC_1 = 0x56; // 'V'
const FILE_NONCE_BYTES = 16;

/** The original chunked format. Chunk 0 carries a FULL chunk of plaintext on top
 *  of the header, so part 0 is 28 bytes larger than its siblings. It remains the
 *  write default until envelope support is negotiated, and readable forever. */
export const ENVELOPE_VERSION_V2 = 2;

/** Uniform part geometry: chunk 0 gives up 28 bytes of plaintext to the header.
 *  See the module header for why R2 requires this. */
export const ENVELOPE_VERSION_V3 = 3;

/** The version this build SEALS with. Reading is not restricted to it.
 *  Keep v2 until the rollout has a version-bearing capability. */
export const WRITE_ENVELOPE_VERSION = ENVELOPE_VERSION_V2;

const SUPPORTED_VERSIONS: ReadonlySet<number> = new Set([ENVELOPE_VERSION_V2, ENVELOPE_VERSION_V3]);

/** 34 bytes: tag:4 | version:1 | flags:1 | fileNonce:16 | chunkIndex:4 BE |
 *  totalChunks:4 BE | chunkSize:4 BE. Every header field is bound; see
 *  buildChunkAad for why `flags` joined them before release rather than after. */
const AAD_BYTES = 34;

/** The AAD's leading tag, per version.
 *
 *  The version byte at offset 4 already binds the format, so the tag is
 *  redundant with it — deliberately. Two independent bytes have to be rewritten
 *  in step to even ATTEMPT a version confusion, and neither is reachable without
 *  breaking the tag it is inside. It also keeps the tag self-describing in a hex
 *  dump, which is what the "CVA2" literal was for in the first place. */
const AAD_TAG_BY_VERSION: ReadonlyMap<number, readonly number[]> = new Map([
  [ENVELOPE_VERSION_V2, [0x43, 0x56, 0x41, 0x32]], // "CVA2"
  [ENVELOPE_VERSION_V3, [0x43, 0x56, 0x41, 0x33]], // "CVA3"
]);
const GCM_TAG_BYTES = 16;

/** Both versions recognise exactly one chunk size. The field exists to be bound
 *  into the AAD and to leave room for a future version — never to size an
 *  allocation. v3 does NOT declare a smaller chunkSize: the nominal chunk stays
 *  8 MiB and chunk 0 simply takes less of it, so a v2 and a v3 header of the
 *  same file carry the same value here. */
const ALLOWED_CHUNK_SIZES: ReadonlySet<number> = new Set([CHUNK_PLAINTEXT_BYTES]);

/** Plaintext bytes chunk 0 gives up to the envelope header. THE WHOLE FORMAT
 *  DIFFERENCE, in one function — every geometry helper below reads it. */
function firstChunkReserve(version: number): number {
  return version === ENVELOPE_VERSION_V3 ? ENVELOPE_HEADER_BYTES : 0;
}

/** The plaintext budget of chunk `chunkIndex` under `version`, for a nominal
 *  chunk size of `chunkSize`. The LAST chunk holds the remainder and is smaller;
 *  this is the budget, not the occupancy. */
function chunkPlaintextBudget(version: number, chunkSize: number, chunkIndex: number): number {
  return chunkSize - (chunkIndex === 0 ? firstChunkReserve(version) : 0);
}

/** Byte offset into the PLAINTEXT at which chunk `chunkIndex` starts. */
function chunkPlaintextStart(version: number, chunkSize: number, chunkIndex: number): number {
  if (chunkIndex === 0) return 0;
  return chunkPlaintextBudget(version, chunkSize, 0) + (chunkIndex - 1) * chunkSize;
}

/** Chunk count for a plaintext of `plaintextTotal` bytes. Shared by the writer
 *  and by the classifier's arithmetic discriminator, so the two cannot drift. */
function chunkCountFor(version: number, chunkSize: number, plaintextTotal: number): number {
  const first = chunkPlaintextBudget(version, chunkSize, 0);
  if (plaintextTotal <= first) return 1;
  // For v2 `first` IS a full chunk, so this collapses to ceil(total / chunkSize).
  return 1 + Math.ceil((plaintextTotal - first) / chunkSize);
}

export interface AttachmentEnvelopeHeader {
  version: number;
  flags: number;
  chunkSize: number;
  totalChunks: number;
  /** 16 bytes of CSPRNG, generated per file. */
  fileNonce: Uint8Array;
}

/**
 * The structure is wrong: bad magic, unknown version, reserved flags set,
 * unsupported chunk size, or arithmetic that does not hold.
 *
 * Terminal. Retry cannot succeed, so callers must not offer one.
 */
export class UnsupportedAttachmentFormatError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'UnsupportedAttachmentFormatError';
  }
}

/**
 * The structure is valid but a GCM tag failed — reorder, truncation, splice, or
 * plain corruption.
 *
 * Terminal, and deliberately distinct from the unsupported-format case: the two
 * render different copy, and neither offers a retry control.
 */
export class AttachmentIntegrityError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = 'AttachmentIntegrityError';
  }
}

/**
 * Whether a WebCrypto rejection is evidence ABOUT THE CIPHERTEXT.
 *
 * `crypto.subtle.decrypt` reports a failed GCM tag as `OperationError` and
 * deliberately says nothing more — the tag verified or it did not. Every other
 * rejection is a fault in OUR CALL, not a verdict on the bytes:
 * `InvalidAccessError` for a key lacking the `decrypt` usage or carrying the
 * wrong algorithm, `NotSupportedError` where AES-GCM is unavailable. Reporting
 * those as tampering tells the user their file was altered when what actually
 * happened is that this client asked for the wrong thing.
 *
 * So the two are separated at the only place the distinction survives: here.
 * A non-authentication rejection is rethrown untouched, keeping its name,
 * message, and stack for the report that will be filed about it.
 *
 * Matched on `.name`, NOT `instanceof DOMException`: the exception and the
 * global can come from different realms, and then `instanceof` is false for a
 * genuine `OperationError`. Under jsdom that is not hypothetical — jsdom
 * installs its own `DOMException` while WebCrypto throws Node's, so an
 * `instanceof` gate fails EVERY authentication check there while passing in
 * the renderer. `.name` is what the spec fixes; it holds across realms.
 */
function isAuthenticationFailure(err: unknown): boolean {
  return typeof err === 'object' && err !== null && (err as Error).name === 'OperationError';
}

/**
 * Thrown when the epoch header is PRESENT but unparseable.
 *
 * Distinct from absent on purpose. Falling back to the current key here would
 * decrypt with the wrong CSK, fail GCM, and be reported as tampering -- which
 * is precisely the misdiagnosis the epoch fix exists to remove. A header that
 * arrived mangled is a transport fault, and saying so beats blaming the file.
 */
export class AttachmentKeyEpochError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'AttachmentKeyEpochError';
  }
}

/**
 * Resolves the CSK epoch an attachment was sealed under from its response
 * header. `null` means "no epoch on record -- use the current key".
 *
 * Decimal digits only, and >= 1. `Number()` alone would read "1e3" as 1000 and
 * " 7" as 7, forms our own server never emits (it writes `strconv.Itoa`), so
 * accepting them buys nothing and lets a mangled header name an epoch no
 * uploader attested. There is no epoch 0 in the schema.
 *
 * ABSENT is not an error, but NOT for the reason first written here. Every
 * tier-2 row already carries an epoch -- migration 000042's valid_media_context
 * CHECK makes `key_version IS NOT NULL` a requirement for media_tier = 2, and
 * tier 2 is the only tier this download path selects -- so there are no
 * "rows predating the attestation". The case the fallback actually covers is a
 * ROLLING DEPLOY: an older control-plane instance serves the download without
 * the header at all, and failing closed there would break every attachment it
 * serves for the length of the rollout.
 */
export function parseKeyVersionHeader(raw: string | null): number | null {
  if (raw === null) return null;
  if (!/^[0-9]+$/.test(raw)) {
    throw new AttachmentKeyEpochError(`unparseable attachment key epoch ${JSON.stringify(raw)}`);
  }
  const n = Number(raw);
  if (!Number.isSafeInteger(n) || n < 1) {
    throw new AttachmentKeyEpochError(`attachment key epoch out of range: ${raw}`);
  }
  return n;
}

/** Defaults to the current write format. Callers sizing an existing envelope
 *  pass its header version explicitly, because v2 and v3 can disagree about
 *  the chunk count for the same plaintext. */
export function totalChunksFor(
  plaintextTotal: number,
  version: number = WRITE_ENVELOPE_VERSION
): number {
  if (!Number.isSafeInteger(plaintextTotal) || plaintextTotal < 1) {
    throw new UnsupportedAttachmentFormatError(
      `plaintext length must be a positive safe integer, got ${plaintextTotal}`
    );
  }
  if (!SUPPORTED_VERSIONS.has(version)) {
    throw new UnsupportedAttachmentFormatError(`unsupported envelope version ${version}`);
  }
  return chunkCountFor(version, CHUNK_PLAINTEXT_BYTES, plaintextTotal);
}

/** The normative length identity. The client dispatcher and the server sizing
 *  check must agree on this exactly — it is what closes the plaintext /
 *  ciphertext dead band to zero rather than papering it with a slack constant.
 *
 *  Version-independent in SHAPE — header + n*overhead + plaintext — and only `n`
 *  moves, so v3 costs at most one extra chunk's 28 bytes over v2 for the same
 *  file. It costs exactly that at the premium ceiling, where the plaintext is an
 *  exact multiple of the chunk size and chunk 0's missing 28 bytes spill into a
 *  33rd chunk; MAX_DECRYPTABLE_ATTACHMENT_BYTES is derived from that count. */
export function expectedBlobLength(
  plaintextTotal: number,
  version: number = WRITE_ENVELOPE_VERSION
): number {
  return (
    ENVELOPE_HEADER_BYTES +
    CHUNK_OVERHEAD_BYTES * totalChunksFor(plaintextTotal, version) +
    plaintextTotal
  );
}

export function encodeEnvelopeHeader(h: AttachmentEnvelopeHeader): CryptoBytes {
  if (h.fileNonce.byteLength !== FILE_NONCE_BYTES) {
    throw new UnsupportedAttachmentFormatError(
      `fileNonce must be ${FILE_NONCE_BYTES} bytes, got ${h.fileNonce.byteLength}`
    );
  }
  const out = new Uint8Array(ENVELOPE_HEADER_BYTES);
  const view = new DataView(out.buffer);
  out[0] = MAGIC_0;
  out[1] = MAGIC_1;
  out[2] = h.version;
  out[3] = h.flags;
  view.setUint32(4, h.chunkSize, false);
  view.setUint32(8, h.totalChunks, false);
  out.set(h.fileNonce, 12);
  return out;
}

/** Raw header fields, with NO semantic validation beyond bounds and magic.
 *
 *  Exists so `decodeEnvelopeHeader` (strict, throws) and `classifyEnvelope`
 *  (three-way, never throws) cannot drift apart on field offsets. Returns null
 *  when the bytes are not our envelope at all. */
function readHeaderFields(bytes: Uint8Array): {
  version: number;
  flags: number;
  chunkSize: number;
  totalChunks: number;
  fileNonce: CryptoBytes;
} | null {
  if (bytes.byteLength < ENVELOPE_HEADER_BYTES) return null;
  if (bytes[0] !== MAGIC_0 || bytes[1] !== MAGIC_1) return null;
  // byteOffset/byteLength are load-bearing: bytes read off a stream is normally
  // a subarray of a larger buffer, and a view over the whole buffer would read
  // the wrong bytes rather than fail.
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  return {
    version: bytes[2],
    flags: bytes[3],
    chunkSize: view.getUint32(4, false),
    totalChunks: view.getUint32(8, false),
    fileNonce: new Uint8Array(bytes.slice(12, 12 + FILE_NONCE_BYTES)),
  };
}

export function decodeEnvelopeHeader(bytes: Uint8Array): AttachmentEnvelopeHeader {
  // Guard BEFORE any read, never between two reads.
  const f = readHeaderFields(bytes);
  if (f === null) {
    throw new UnsupportedAttachmentFormatError(
      bytes.byteLength < ENVELOPE_HEADER_BYTES
        ? `envelope shorter than a header: ${bytes.byteLength} < ${ENVELOPE_HEADER_BYTES}`
        : 'bad magic'
    );
  }
  if (!SUPPORTED_VERSIONS.has(f.version)) {
    throw new UnsupportedAttachmentFormatError(`unsupported envelope version ${f.version}`);
  }
  if (f.flags !== 0) {
    // Never silently ignored: a future version may use these bits to mean
    // something this build cannot honour, and guessing would be a downgrade.
    throw new UnsupportedAttachmentFormatError(
      `unsupported envelope flags 0x${f.flags.toString(16)}`
    );
  }
  if (!ALLOWED_CHUNK_SIZES.has(f.chunkSize)) {
    throw new UnsupportedAttachmentFormatError(`unsupported chunk size ${f.chunkSize}`);
  }
  if (f.totalChunks < 1) {
    throw new UnsupportedAttachmentFormatError(`totalChunks must be >= 1, got ${f.totalChunks}`);
  }
  return f;
}

/**
 * The 34-byte AAD sealed into chunk `chunkIndex`.
 *
 *   "CVA<version>":4 | version:1 | flags:1 | fileNonce:16 | chunkIndex:4 BE
 *   | totalChunks:4 BE | chunkSize:4 BE
 *
 * The leading tag and the version byte BOTH move with the format, so a v2 chunk
 * and a v3 chunk of the same file, same index, same nonce authenticate under
 * different AAD and neither can open the other. That is what makes flipping the
 * header's version byte a fail-closed tag error rather than a re-framing.
 *
 * Every field is fixed-width, so there is no length-canonicalisation ambiguity.
 * Each one defeats a specific attack — chunkIndex/reorder, totalChunks/
 * truncation, fileNonce/cross-file splice, chunkSize/re-framing — so removing
 * any of them silently removes a defence. `[internal]rules/e2ee.md` carries the
 * normative table.
 */
export function buildChunkAad(h: AttachmentEnvelopeHeader, chunkIndex: number): CryptoBytes {
  if (!Number.isSafeInteger(chunkIndex) || chunkIndex < 0 || chunkIndex >= h.totalChunks) {
    throw new UnsupportedAttachmentFormatError(
      `chunkIndex ${chunkIndex} outside [0, ${h.totalChunks})`
    );
  }
  const tag = AAD_TAG_BY_VERSION.get(h.version);
  if (tag === undefined) {
    // Unreachable from decodeEnvelopeHeader or classifyEnvelope, both of which
    // gate the version first. Kept because this function is exported: sealing a
    // chunk under a tag we picked by accident is not a failure we want to
    // discover from a decrypt.
    throw new UnsupportedAttachmentFormatError(`unsupported envelope version ${h.version}`);
  }
  const out = new Uint8Array(AAD_BYTES);
  const view = new DataView(out.buffer);
  out.set(tag, 0);
  out[4] = h.version;
  // `flags` was the ONE header field outside the AAD. Unexploitable today --
  // both decodeEnvelopeHeader and classifyEnvelope refuse a non-zero value, so
  // tampering degraded to a fail-closed `unsupported` -- but that defence lived
  // in a validation rule rather than in the tag. The first version that gives a
  // flag bit meaning would inherit an attacker-selectable, unauthenticated
  // selector, and fixing it THEN requires a format version bump because it
  // changes every stored ciphertext's AAD.
  //
  // Bound now, while zero v2 ciphertext exists in the field. This is the last
  // moment it is free.
  out[5] = h.flags;
  out.set(h.fileNonce, 6);
  view.setUint32(22, chunkIndex, false);
  view.setUint32(26, h.totalChunks, false);
  view.setUint32(30, h.chunkSize, false);
  return out;
}

const AES_GCM_IV_LENGTH = 12;

/** Bytes that may be handed to WebCrypto.
 *
 *  TypeScript separates `Uint8Array<ArrayBuffer>` from the looser
 *  `Uint8Array<ArrayBufferLike>` (which admits SharedArrayBuffer), and only the
 *  former satisfies `BufferSource`. Naming it once keeps the crypto call sites
 *  free of casts -- and a cast is exactly what would hide a genuinely shared
 *  buffer reaching an encrypt call. */
export type CryptoBytes = Uint8Array<ArrayBuffer>;

/**
 * A random-access plaintext source.
 *
 * `File` is the production implementation; the buffer one exists so the crypto
 * can be exercised without a DOM `File`. The interface is deliberately narrow —
 * one lazy `slice` — because that is the whole mechanism by which plaintext
 * residency stays bounded regardless of file size.
 */
export interface ChunkSource {
  readonly byteLength: number;
  slice(start: number, end: number): Promise<CryptoBytes>;
}

export function fileChunkSource(file: File): ChunkSource {
  return {
    byteLength: file.size,
    // File.slice is lazy. This is what replaces the whole-file
    // `file.arrayBuffer()` the single-shot path used, and it is the difference
    // between a bounded transient and one proportional to the file.
    async slice(start, end) {
      return new Uint8Array(await file.slice(start, end).arrayBuffer());
    },
  };
}

export function bufferChunkSource(buf: Uint8Array): ChunkSource {
  return {
    byteLength: buf.byteLength,
    async slice(start, end) {
      // Copy into a fresh ArrayBuffer-backed view: the caller's array may be
      // backed by anything, and WebCrypto will not take the loose type.
      return new Uint8Array(buf.slice(start, end));
    },
  };
}

export function newEnvelopeHeader(plaintextTotal: number): AttachmentEnvelopeHeader {
  return {
    version: WRITE_ENVELOPE_VERSION,
    flags: 0,
    chunkSize: CHUNK_PLAINTEXT_BYTES,
    totalChunks: totalChunksFor(plaintextTotal, WRITE_ENVELOPE_VERSION),
    // Client-generated: the server assigns fileID only AFTER the upload
    // completes, so there is no server-side identifier to bind at encrypt time.
    fileNonce: crypto.getRandomValues(new Uint8Array(FILE_NONCE_BYTES)),
  };
}

/**
 * The exact bytes of one upload part. Part 0 carries the envelope header;
 * every other part is a bare sealed chunk.
 *
 * The header rides on part 0 rather than being its own part because S3
 * multipart exempts only the LAST part from its 5 MiB minimum, so a 28-byte
 * header part would be rejected.
 *
 * Safe to call again for the same index -- that is how in-session resume
 * re-sends a missing part. A FRESH IV is drawn on every call. Never derive an
 * IV from the chunk index: a retry would then reuse an (key, IV) pair, which is
 * the one failure AES-GCM does not survive.
 */
export async function buildUploadPart(
  source: ChunkSource,
  key: CryptoKey,
  header: AttachmentEnvelopeHeader,
  chunkIndex: number
): Promise<CryptoBytes> {
  // Validates chunkIndex against the declared range, and does so BEFORE the
  // read below -- guards run ahead of the allocating operation, never between
  // two of them.
  const aad = buildChunkAad(header, chunkIndex);

  // NOT `chunkIndex * chunkSize`: under v3 chunk 0 is 28 bytes short, so every
  // later chunk starts 28 bytes earlier than the naive product.
  const start = chunkPlaintextStart(header.version, header.chunkSize, chunkIndex);
  const end = Math.min(
    start + chunkPlaintextBudget(header.version, header.chunkSize, chunkIndex),
    source.byteLength
  );
  if (start >= end) {
    throw new UnsupportedAttachmentFormatError(
      `chunk ${chunkIndex} is empty for a ${source.byteLength}-byte source`
    );
  }

  const plaintext = await source.slice(start, end);
  const iv: CryptoBytes = crypto.getRandomValues(new Uint8Array(AES_GCM_IV_LENGTH));
  const sealed = new Uint8Array(
    await crypto.subtle.encrypt({ name: 'AES-GCM', iv, additionalData: aad }, key, plaintext)
  );

  const prefix = chunkIndex === 0 ? encodeEnvelopeHeader(header) : new Uint8Array(0);
  const out = new Uint8Array(prefix.byteLength + iv.byteLength + sealed.byteLength);
  out.set(prefix, 0);
  out.set(iv, prefix.byteLength);
  out.set(sealed, prefix.byteLength + iv.byteLength);
  return out;
}

/**
 * What a blob's leading bytes say it is.
 *
 * Three-way, not two, and the split is load-bearing:
 *
 *   `legacy`      — not recognisable as our envelope. Parse it as the v1
 *                   single-shot format, which is what every attachment already
 *                   stored in the field actually is.
 *   `unsupported` — structurally OUR envelope, but this build cannot read it
 *                   (future version, reserved flags set, unknown chunk size).
 *                   The user gets "this build cannot open it", not "corrupt".
 *   `v2` / `v3`   — readable here; `header` says which format.
 *
 * A two-way split would report a genuine v4 blob as corruption, and would also
 * report a legacy blob whose random IV happens to begin with the magic (about
 * 1 in 65,536) as unsupported. Structural recognition and readability are
 * separate questions and are answered separately.
 *
 * Keep readable formats explicit. Existing callers distinguish `v2`, and a
 * version-specific arm makes an accidental v2-only decrypt path visible in
 * TypeScript rather than hiding it behind a broad `chunked` label.
 */
export type EnvelopeKind =
  | { kind: 'v2'; header: AttachmentEnvelopeHeader }
  | { kind: 'v3'; header: AttachmentEnvelopeHeader }
  | { kind: 'unsupported'; reason: string }
  | { kind: 'legacy' };

/**
 * Deterministic. There is NO trial decryption and NO fallback between branches:
 * a blob takes exactly one path, decided before any key is touched.
 *
 * THE ARITHMETIC IS THE DISCRIMINATOR, not the magic. A legacy blob opens with a
 * random 12-byte IV, so roughly 1 in 65,536 of them begins with "CV" by chance --
 * the magic check does nothing for those. What keeps them on the legacy path is
 * the chunk-count identity: their random header bytes parse as an absurd
 * `totalChunks`, which drives `body` negative. The magic is a fast-path hint and
 * a signal to a human reader; do not mistake it for the security boundary.
 *
 * SECURITY — read this before "simplifying" the dispatch. Both candidate parses
 * are AEAD-verified under the same key; legacy used EMPTY AAD while the chunked
 * formats bind a 34-byte AAD whose tag and version byte both move with the
 * format. A malicious server that rewrites the discriminator therefore does not
 * get to choose a WEAKER parser, and cannot re-frame a v2 blob as v3 either: it
 * only chooses which tag check fails. There is no downgrade oracle here. The
 * worst a hostile server achieves is denial of one attachment, which it already
 * had by deleting the file.
 */
export function classifyEnvelope(head: Uint8Array, totalLength: number): EnvelopeKind {
  // Structural recognition first — is this our envelope at all?
  const f = readHeaderFields(head);
  if (f === null) return { kind: 'legacy' };
  const { version, flags, chunkSize, totalChunks } = f;

  if (chunkSize < 1 || totalChunks < 1) return { kind: 'legacy' };

  const body = totalLength - ENVELOPE_HEADER_BYTES - CHUNK_OVERHEAD_BYTES * totalChunks;
  // Redundant with the identity below -- a negative body yields a zero or
  // negative chunk count, which can never equal a positive totalChunks -- and
  // verified so by falsification: removing it changes no outcome. Kept as an
  // explicit statement of the invariant and as a backstop if that identity is
  // ever loosened. Do not cite it as the discriminator.
  if (body < 1) return { kind: 'legacy' };
  if (!envelopeGeometryHolds(version, chunkSize, totalChunks, body)) return { kind: 'legacy' };

  // It IS our envelope. Now: can this build read it?
  if (!SUPPORTED_VERSIONS.has(version)) {
    return { kind: 'unsupported', reason: `unsupported envelope version ${version}` };
  }
  if (flags !== 0) {
    // Never silently ignored — a future version may use these bits to mean
    // something this build cannot honour, and guessing would be a downgrade.
    return { kind: 'unsupported', reason: `unsupported envelope flags 0x${flags.toString(16)}` };
  }
  if (!ALLOWED_CHUNK_SIZES.has(chunkSize)) {
    return { kind: 'unsupported', reason: `unsupported chunk size ${chunkSize}` };
  }

  return {
    kind: version === ENVELOPE_VERSION_V2 ? 'v2' : 'v3',
    header: {
      version,
      flags,
      chunkSize,
      totalChunks,
      fileNonce: f.fileNonce,
    },
  };
}

/**
 * Does the declared geometry reproduce the observed body length?
 *
 * For a KNOWN version there is one right framing and the answer is exact. For an
 * UNKNOWN (future) version we cannot know its framing, so the declaration is
 * accepted if EITHER known family reproduces it. That is deliberately generous:
 * the alternative is routing a genuinely future blob to `legacy`, whose empty
 * AAD can never authenticate it, and telling the user their file is corrupt when
 * it is merely newer than their client.
 *
 * It gives nothing away. The version is bound into every chunk's AAD, so a blob
 * admitted here under the wrong family still fails its first GCM open — the
 * generosity buys a better error message, not a weaker check.
 */
function envelopeGeometryHolds(
  version: number,
  chunkSize: number,
  totalChunks: number,
  body: number
): boolean {
  const families = SUPPORTED_VERSIONS.has(version) ? [version] : [...SUPPORTED_VERSIONS];
  return families.some((v) => chunkCountFor(v, chunkSize, body) === totalChunks);
}

/**
 * Decrypt either format into a Blob.
 *
 * A Blob rather than an ArrayBuffer is deliberate: the browser may page a Blob
 * out, where one 256 MiB ArrayBuffer must stay resident. Chunk plaintexts are
 * appended as separate parts, so no single contiguous copy of the whole file is
 * ever built.
 */
export async function decryptAttachmentBlob(
  bytes: CryptoBytes,
  key: CryptoKey,
  mimeType: string,
  /** Stop decrypting once the accumulated plaintext exceeds this. For callers
   *  whose render ceiling sits far below the download ceiling -- the markdown
   *  overflow path renders at most a few hundred KiB of a file that may be
   *  256 MiB. The returned Blob is then a PREFIX, and the caller must treat a
   *  .size above its limit as "too large" rather than as content. */
  maxPlaintextBytes?: number
): Promise<Blob> {
  const classified = classifyEnvelope(bytes.subarray(0, ENVELOPE_HEADER_BYTES), bytes.byteLength);

  if (classified.kind === 'unsupported') {
    throw new UnsupportedAttachmentFormatError(classified.reason);
  }

  if (classified.kind === 'legacy') {
    // Avoid copying the whole blob when it already owns its buffer outright.
    const whole =
      bytes.byteOffset === 0 && bytes.byteLength === bytes.buffer.byteLength
        ? bytes.buffer
        : bytes.slice().buffer;
    try {
      return new Blob([await decryptFile(whole, key)], { type: mimeType });
    } catch (err) {
      if (!isAuthenticationFailure(err)) throw err;
      // A DAMAGED CHUNKED BLOB ARRIVES HERE, not just a real legacy one. The
      // discriminator is arithmetic over the total length, so truncating a
      // chunked envelope breaks the arithmetic and drops it to `legacy` — where
      // the legacy AAD (empty) can never authenticate one of its chunks. Left
      // unwrapped,
      // that surfaced as the generic load failure, which offers a RETRY: the
      // one thing this must not do, since retrying re-fetches identical bytes
      // and fails identically. It is an integrity failure either way — a
      // genuine legacy blob that will not authenticate is equally terminal.
      throw new AttachmentIntegrityError('attachment failed authentication', { cause: err });
    }
  }

  return decryptChunkedEnvelope(bytes, classified.header, key, mimeType, maxPlaintextBytes);
}

/**
 * Authenticates and concatenates a chunked envelope's chunks, v2 or v3. Split
 * out of decryptAttachmentBlob so the format DISPATCH (three-way, with a
 * distinct failure mode each) and the per-chunk loop are each readable on their
 * own -- together they exceeded the cognitive-complexity budget.
 */
async function decryptChunkedEnvelope(
  bytes: Uint8Array<ArrayBuffer>,
  header: AttachmentEnvelopeHeader,
  key: CryptoKey,
  mimeType: string,
  maxPlaintextBytes?: number
): Promise<Blob> {
  // Blob parts, NOT ArrayBuffer parts. Each authenticated chunk is handed to the
  // browser's blob store immediately and its plaintext reference dropped, so the
  // JS heap holds ONE chunk of plaintext at a time instead of all of them. On a
  // 256 MiB attachment that is ~8 MiB resident rather than ~256 MiB, and the
  // blob store is free to spill to disk where a JS array never can.
  //
  // WHY THE CIPHERTEXT IS STILL BUFFERED WHOLE, deliberately: classifyEnvelope
  // needs the TOTAL length, because the discriminator between a chunked envelope
  // and a legacy blob is the ARITHMETIC, not the magic -- roughly 1 in 65,536 legacy
  // blobs begin with "CV" by chance. A stream cannot supply that length before
  // it has been consumed. Streaming would therefore force either trusting a
  // server-declared length (demoting the discriminator from a computation to a
  // claim) or falling back to magic alone (misrouting those legacy blobs). Both
  // are worse than holding the ciphertext, so the ciphertext bound stays where
  // it is enforced today: readBoundedBody, which measures actual bytes.
  const parts: Blob[] = [];
  let plaintextBytes = 0;
  let offset = ENVELOPE_HEADER_BYTES;

  for (let i = 0; i < header.totalChunks; i++) {
    const isLast = i === header.totalChunks - 1;
    // NOT a flat `chunkSize + tag`: under v3 chunk 0's sealed body is 28 bytes
    // shorter, and reading it at the v2 length would slide every later chunk's
    // IV by 28 bytes.
    const sealedLen = isLast
      ? bytes.byteLength - offset - AES_GCM_IV_LENGTH
      : chunkPlaintextBudget(header.version, header.chunkSize, i) + GCM_TAG_BYTES;
    // UNREACHABLE for anything classifyEnvelope routed here, and verified so by
    // falsification: removing it changes no test outcome. classify's `body` is
    // already net of per-chunk overhead, so ceil(body / chunkSize) ===
    // totalChunks bounds the last chunk at >= 17 bytes (checked numerically
    // across totalChunks 1..199 at every window edge). Kept for the same reason
    // classify keeps its own `body < 1`: an explicit statement of the invariant,
    // and a backstop if that identity is ever loosened. Do not cite it as a
    // truncation defence -- classify is the one doing that work.
    if (sealedLen < 1) {
      throw new AttachmentIntegrityError(
        `attachment chunk ${i} of ${header.totalChunks} is truncated`
      );
    }
    const iv = bytes.subarray(offset, offset + AES_GCM_IV_LENGTH);
    const body = bytes.subarray(offset + AES_GCM_IV_LENGTH, offset + AES_GCM_IV_LENGTH + sealedLen);
    offset += AES_GCM_IV_LENGTH + sealedLen;

    const plaintext = await decryptChunk(body, iv, key, header, i);
    plaintextBytes += plaintext.byteLength;
    parts.push(new Blob([plaintext]));
    // `plaintext` falls out of scope here; only the Blob reference survives.

    // Early exit for callers with a render ceiling far below the download
    // ceiling. Decrypting the remaining chunks would cost real work to produce
    // bytes the caller is about to discard. The partial Blob is returned and
    // the caller compares .size against its own limit, exactly as before.
    if (maxPlaintextBytes !== undefined && plaintextBytes > maxPlaintextBytes) {
      return new Blob(parts, { type: mimeType });
    }
  }

  return new Blob(parts, { type: mimeType });
}

/** One authenticated chunk, with the error triage the loop should not carry. */
async function decryptChunk(
  body: Uint8Array<ArrayBuffer>,
  iv: Uint8Array<ArrayBuffer>,
  key: CryptoKey,
  header: AttachmentEnvelopeHeader,
  index: number
): Promise<ArrayBuffer> {
  try {
    return await crypto.subtle.decrypt(
      { name: 'AES-GCM', iv, additionalData: buildChunkAad(header, index) },
      key,
      body
    );
  } catch (err) {
    // A key or platform fault is NOT evidence of tampering, and saying so
    // would send the user hunting a corrupted file over what is our bug.
    if (!isAuthenticationFailure(err)) throw err;
    // Fail closed. Name the chunk so a report is actionable, and nothing else
    // -- the plaintext is exactly what we could not authenticate. `cause`
    // carries the original rejection for a report; the message stays clean.
    throw new AttachmentIntegrityError(
      `attachment chunk ${index} of ${header.totalChunks} failed authentication`,
      { cause: err }
    );
  }
}
