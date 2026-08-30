/**
 * Chunked attachment upload session client (#2157 PR 2).
 *
 * Transport only. All crypto lives in `utils/attachmentChunkedCrypto`; this
 * module never touches a key beyond handing it to `buildUploadPart`.
 *
 * WHY PER-CHUNK REQUESTS RATHER THAN ONE STREAMED BODY — two independent
 * reasons, either of which alone would decide it:
 *
 *  1. `apiFetch` retries the SAME `init` on a 401 refresh and on a 403
 *     attestation refresh. A `ReadableStream` body is single-consumption and
 *     cannot be replayed at all; a `FormData` body replays by re-sending the
 *     whole file. Access tokens are 15 minutes and a 256 MiB upload on a slow
 *     link routinely exceeds that, so a single-request transport has a
 *     designed-in failure mode. Only per-chunk requests survive their own
 *     duration.
 *  2. Cloudflare caps request bodies at ~100 MB and `api.concordvoice.chat` is
 *     CF-proxied by standing requirement, so a single 256 MiB request is
 *     refused at the edge regardless of what the origin would accept.
 *
 * WIRE VERSION. `envelope_version` rides on every init and comes from the
 * version-bearing capability selected before encryption. A control plane that
 * predates that capability receives v2 geometry; only an explicit v3 offer
 * authorizes v3. This avoids a version-skew 400 without retrying or downgrading
 * ciphertext after encryption starts.
 */
import { apiFetch } from '../system/apiClient';
import {
  buildUploadPart,
  expectedBlobLength,
  fileChunkSource,
  newEnvelopeHeader,
  CHUNK_PLAINTEXT_BYTES,
  type AttachmentEnvelopeHeader,
  type AttachmentEnvelopeVersion,
  type ChunkSource,
} from '../../utils/attachmentChunkedCrypto';

const SESSION_PATH = '/api/v1/media/upload/attachment/session';

/** A server that keeps reporting the same part missing must not make us spin. */
const MAX_COMMIT_ATTEMPTS = 3;
/** A 410 restart re-encrypts the whole file; one retry, then give up. */
const MAX_SESSION_RESTARTS = 1;

/** The upload target. A DISCRIMINATED UNION, not two optional fields.
 *
 *  The server refuses a request carrying both ids ("Exactly one of channel_id
 *  or conversation_id must be provided"), and the first version of this type
 *  made that shape representable: MessageInput computes
 *  `targetId = conversationId || channelId` and passes BOTH, so in a DM the two
 *  fields held the same UUID and every DM attachment upload 400'd at init. The
 *  legacy path had an `if (conversationId) … else …` that this path dropped.
 *
 *  Making it a union means `tsc` rejects the both-present shape rather than
 *  leaving it to a convention every call site has to remember. */
export type UploadSessionTarget =
  { channelId: string; conversationId?: never } | { conversationId: string; channelId?: never };

export type UploadSessionContext = UploadSessionTarget & {
  keyVersion: number;
  fileType: string;
  mimeType: string;
  envelopeVersion: AttachmentEnvelopeVersion;
};

export interface UploadSessionCallbacks {
  /** Fires once per chunk the server has accepted. Granularity is exactly
   *  1/total: `fetch` exposes no intra-request upload progress, and moving to
   *  XMLHttpRequest to get it would fork apiClient's 401/403 recovery. */
  onChunkCommitted(index: number, total: number): void;
  /** Fires when a session id exists server-side, and again when it no longer
   *  does. The hook mirrors these into a live-session set so an unmount can
   *  fire the abandon DELETE -- the id lives in this closure and the composer
   *  cannot otherwise see it. */
  onSessionOpened?(sessionId: string): void;
  onSessionClosed?(sessionId: string): void;
}

/** Byte-identical to the legacy single-shot upload response, so callers parse
 *  one shape regardless of which path produced the file. */
export interface UploadedAttachment {
  file_id: string;
  storage_key: string;
  file_type: string;
  file_size: number;
}

export class UploadAbortedError extends Error {
  constructor() {
    super('Upload aborted');
    this.name = 'UploadAbortedError';
  }
}

interface OpenSession {
  sessionId: string;
  header: AttachmentEnvelopeHeader;
}

function throwIfAborted(signal: AbortSignal): void {
  if (signal.aborted) throw new UploadAbortedError();
}

async function errorTextOf(res: Response): Promise<string> {
  try {
    return (await res.clone().text()).slice(0, 500);
  } catch {
    return '';
  }
}

/** 43 chars of base64url — the server's own session-id shape. Validating here
 *  keeps a hostile or broken server from steering an authenticated request to an
 *  arbitrary same-origin path. The traversal defence is otherwise accidental: it
 *  holds only because putChunk happens to call buildUploadPart (which rejects a
 *  non-safe-integer index) BEFORE building the URL. */
const SESSION_ID_RE = /^[A-Za-z0-9_-]{43}$/;

async function openSession(
  file: File,
  ctx: UploadSessionContext,
  header: AttachmentEnvelopeHeader,
  signal: AbortSignal
): Promise<string> {
  const res = await apiFetch(SESSION_PATH, {
    method: 'POST',
    signal,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      // Exactly one, never both -- the union above is what keeps this honest.
      ...(ctx.conversationId !== undefined
        ? { conversation_id: ctx.conversationId }
        : { channel_id: ctx.channelId }),
      key_version: ctx.keyVersion,
      file_type: ctx.fileType,
      mime_type: ctx.mimeType,
      chunk_size: CHUNK_PLAINTEXT_BYTES,
      // EVERY size below is derived from the SAME header this upload will seal
      // with, never from the write-version constant. v2 and v3 disagree about
      // the chunk count and the part boundaries for the same file, so a session
      // opened under one version and sealed under the other has every part
      // rejected for length.
      envelope_version: header.version,
      total_chunks: header.totalChunks,
      // The server recomputes this and 400s on disagreement, so a client that
      // guesses fails closed rather than uploading something unreadable.
      declared_ciphertext_bytes: expectedBlobLength(file.size, header.version),
    }),
  });
  if (!res.ok) {
    throw new Error(`Could not start the upload (${res.status}): ${await errorTextOf(res)}`);
  }
  // VALIDATE, do not cast. A 201 whose body lacks session_id (a proxy rewriting
  // the body, a partial write, a future server change) produced `undefined`,
  // which then interpolated into every subsequent URL: the chunk PUT went to
  // /session/undefined/chunk/0 and the user saw "Chunk 0 failed (404): Upload
  // session not found" -- a message that sends whoever debugs it hunting an
  // expiry bug that does not exist. `undefined` also entered liveSessions, so
  // the unmount handler fired a DELETE against /session/undefined.
  //
  // The shape mirrors the server's own validUploadSessionID.
  const body = (await res.json()) as { session_id?: unknown };
  if (typeof body.session_id !== 'string' || !SESSION_ID_RE.test(body.session_id)) {
    throw new Error('The server did not return a usable upload session id');
  }
  return body.session_id;
}

async function putChunk(
  sess: OpenSession,
  source: ChunkSource,
  key: CryptoKey,
  index: number,
  signal: AbortSignal
): Promise<Response> {
  const part = await buildUploadPart(source, key, sess.header, index);
  return apiFetch(`${SESSION_PATH}/${sess.sessionId}/chunk/${index}`, {
    method: 'PUT',
    signal,
    headers: { 'Content-Type': 'application/octet-stream' },
    body: part,
  });
}

/** Deliberately WITHOUT the abort signal. This runs precisely because the upload
 *  was aborted, so forwarding an already-aborted signal would cancel the cleanup
 *  itself and strand the very bytes it exists to release.
 *
 *  Best-effort. A failed cancel is silent: the server's object-store sweeper is
 *  the authority for reclaiming bytes, which is why it is load-bearing for
 *  correctness rather than defence in depth. */
/** Returns true only when the session is PROVABLY gone server-side.
 *
 *  The previous version awaited the fetch and swallowed everything. apiFetch
 *  RESOLVES on a non-2xx, so `res.ok` was never consulted and the whole HTTP
 *  error class read as success -- including the 502 the server returns
 *  specifically to say "the abort failed, the session is still here, retry".
 *  The two halves of this feature disagreed about the contract.
 *
 *  That mattered because the caller then cleared the session from
 *  `liveSessions` on an outcome nobody looked at, removing the last
 *  client-side reclaim path (the unmount keepalive DELETE) and leaving the
 *  bytes to the sweeper's 2h30m floor. */
async function cancelSession(sessionId: string): Promise<boolean> {
  try {
    const res = await apiFetch(`${SESSION_PATH}/${sessionId}`, { method: 'DELETE' });
    if (!res.ok) {
      // console.* is captured by logBufferService and travels in bug reports; a
      // bare catch meant a systematically failing cancel appeared in none.
      console.warn(
        '[attachment] cancel rejected; leaving the session for unload/sweeper',
        res.status
      );
      return false;
    }
    return true;
  } catch (err) {
    console.warn(
      '[attachment] cancel could not be sent',
      err instanceof Error ? `${err.name}: ${err.message}` : 'unknown'
    );
    return false;
  }
}

/** Unmount path. `keepalive` lets the request outlive the document; sendBeacon
 *  cannot be used because it carries no Authorization header. Neither is
 *  guaranteed to arrive, which is exactly why the server sweeper is load-bearing
 *  for correctness rather than defence in depth. */
export function abandonSessionOnUnload(sessionId: string): void {
  void apiFetch(`${SESSION_PATH}/${sessionId}`, {
    method: 'DELETE',
    keepalive: true,
  }).catch(() => {
    /* the sweeper will reclaim it */
  });
}

/** Every phase below can discover the session died under it (410 Gone). Making
 *  that a RETURNED value rather than a thrown error is what keeps expiry --
 *  which is recoverable by restarting -- distinguishable from a real failure,
 *  which is not. The orchestrator then reads as a sequence instead of a tower
 *  of flags. */
type PhaseOutcome = { kind: 'ok' } | { kind: 'expired' };

const OK: PhaseOutcome = { kind: 'ok' };
const EXPIRED: PhaseOutcome = { kind: 'expired' };

/** Sends every chunk once, in order. */
async function sendAllChunks(
  sess: OpenSession,
  source: ChunkSource,
  key: CryptoKey,
  signal: AbortSignal,
  cb: UploadSessionCallbacks
): Promise<PhaseOutcome> {
  for (let i = 0; i < sess.header.totalChunks; i++) {
    throwIfAborted(signal);
    const res = await putChunk(sess, source, key, i, signal);
    if (res.status === 410) return EXPIRED;
    if (!res.ok) {
      throw new Error(`Chunk ${i} failed (${res.status}): ${await errorTextOf(res)}`);
    }
    cb.onChunkCommitted(i, sess.header.totalChunks);
    throwIfAborted(signal);
  }
  return OK;
}

/** Re-sends exactly the indices the object store says it is missing. This is
 *  what turns "restart 230 MiB" into "send 26 MiB". The header, and so the
 *  fileNonce, is reused -- only the per-chunk IV changes. */
async function resendMissingChunks(
  sess: OpenSession,
  source: ChunkSource,
  key: CryptoKey,
  signal: AbortSignal,
  missing: readonly number[]
): Promise<PhaseOutcome> {
  for (const index of missing) {
    throwIfAborted(signal);
    const put = await putChunk(sess, source, key, index, signal);
    if (put.status === 410) return EXPIRED;
    if (!put.ok) {
      throw new Error(`Chunk ${index} failed on retry (${put.status})`);
    }
  }
  return OK;
}

/** Reads the missing-index list off a 409. */
async function missingIndicesFrom(res: Response, total: number): Promise<number[]> {
  const { missing } = (await res.json()) as { missing?: unknown };
  if (!Array.isArray(missing) || missing.length === 0) {
    throw new Error('Server reported the upload incomplete but named no missing parts');
  }
  // Each element is interpolated into a URL and passed to buildUploadPart.
  // Filtering here means a hostile `missing: ["../../users/me"]` cannot become
  // an authenticated request to an arbitrary path if those two statements are
  // ever reordered.
  const indices = missing.filter(
    (n): n is number => Number.isSafeInteger(n) && n >= 0 && n < total
  );
  if (indices.length === 0) {
    throw new Error('Server named missing parts that are not valid chunk indices');
  }
  return indices;
}

/** Commits, repairing from the store's own account of what is missing. The
 *  OBJECT STORE is authoritative: the client's record of what it sent is a
 *  belief, and a 409 is the store correcting it. */
async function commitWithRepair(
  sess: OpenSession,
  source: ChunkSource,
  key: CryptoKey,
  signal: AbortSignal
): Promise<{ kind: 'done'; attachment: UploadedAttachment } | PhaseOutcome> {
  for (let attempt = 0; attempt < MAX_COMMIT_ATTEMPTS; attempt++) {
    throwIfAborted(signal);
    const res = await apiFetch(`${SESSION_PATH}/${sess.sessionId}/commit`, {
      method: 'POST',
      signal,
    });

    if (res.ok) return { kind: 'done', attachment: (await res.json()) as UploadedAttachment };
    if (res.status === 410) return EXPIRED;
    if (res.status !== 409) {
      throw new Error(`Could not finish the upload (${res.status}): ${await errorTextOf(res)}`);
    }

    const repaired = await resendMissingChunks(
      sess,
      source,
      key,
      signal,
      await missingIndicesFrom(res, sess.header.totalChunks)
    );
    if (repaired.kind === 'expired') return EXPIRED;
  }
  throw new Error('The upload could not be completed after several attempts');
}

export async function uploadAttachmentChunked(
  file: File,
  key: CryptoKey,
  ctx: UploadSessionContext,
  signal: AbortSignal,
  cb: UploadSessionCallbacks
): Promise<UploadedAttachment> {
  const source = fileChunkSource(file);

  for (let restart = 0; ; restart++) {
    throwIfAborted(signal);

    // A restart draws a NEW fileNonce: the old session's parts are gone, so
    // reusing it buys nothing. Within a session it must stay stable, because it
    // is bound into every chunk's AAD.
    const header = newEnvelopeHeader(file.size, ctx.envelopeVersion);
    const sess: OpenSession = {
      sessionId: await openSession(file, ctx, header, signal),
      header,
    };
    cb.onSessionOpened?.(sess.sessionId);

    try {
      const sent = await sendAllChunks(sess, source, key, signal, cb);
      if (sent.kind === 'ok') {
        const committed = await commitWithRepair(sess, source, key, signal);
        if (committed.kind === 'done') {
          // Committed: the session id is spent, so the unmount DELETE must not
          // chase it.
          cb.onSessionClosed?.(sess.sessionId);
          return committed.attachment;
        }
      }

      // Only expiry reaches here -- every other failure threw. Gone server-side
      // already, so there is nothing to DELETE, but the hook must stop tracking
      // it or the unmount fires against a dead id.
      cb.onSessionClosed?.(sess.sessionId);
      if (restart >= MAX_SESSION_RESTARTS) {
        throw new Error('The upload session kept expiring; please try again');
      }
      continue; // new session, new fileNonce
    } catch (err) {
      // Cancel on ANY exit that is not a completed upload, including abort --
      // but only stop TRACKING the session when the server confirmed it is
      // gone. Untracking on a failed cancel discards the one remaining
      // client-side reclaim path (the unmount keepalive DELETE) for a session
      // the server deliberately kept alive so the client could retry.
      if (await cancelSession(sess.sessionId)) {
        cb.onSessionClosed?.(sess.sessionId);
      }
      throw err;
    }
  }
}
