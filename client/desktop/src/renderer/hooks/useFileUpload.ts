import React, { useState, useCallback, useEffect, useMemo, useRef } from 'react';
import { apiFetch, safeJson } from '../services/apiClient';
import { e2eeService } from '../services/e2eeService';
import {
  encryptFile,
  classifyFileType,
  isImageType,
  MAX_ATTACHMENTS,
} from '../utils/attachmentCrypto';
import { stripFileMetadata, sniffHandledImage, SNIFF_BYTES } from '../utils/imageMetadata';
import {
  formatLimitBytes,
  resolveAttachmentLimit,
  type AttachmentLimit,
  IMAGE_STRIP_MAX_BYTES,
} from '../utils/entitlementLimits';
import { useEntitlement } from './useEntitlement';
import { useClientConfigStore } from '../stores/clientConfigStore';
import {
  uploadAttachmentChunked,
  abandonSessionOnUnload,
  UploadAbortedError,
} from '../services/attachmentUploadSession';
import { CHUNK_PLAINTEXT_BYTES } from '../utils/attachmentChunkedCrypto';
import type { AttachmentSummary } from '../types/chat';

const DEFAULT_MIME = 'application/octet-stream';

export interface FileUploadState {
  file: File;
  /** Stable identity for the lifetime of this queue entry, minted at queue time.
   *
   *  Progress and completion used to be written by ARRAY INDEX against live
   *  state, while the upload loop iterated a snapshot. Removing an earlier file
   *  mid-upload shifted the survivors down, so the completion write landed on
   *  whichever entry moved into that slot -- stamping it with the removed
   *  file's file_id and sending it as an attachment pointing at someone else's
   *  ciphertext. Keying every write on this makes that unrepresentable. */
  uploadId: string;
  id?: string;
  progress: number;
  /** `preparing` covers the whole-file image strip ONLY. The 64 KiB sniff is
   *  deliberately not a status: it runs inside addFiles, before any row exists
   *  to carry one. `cancelled` is terminal and user-caused, which is why it is
   *  distinct from `error` -- a cancelled row must not read as a failure. */
  status: 'pending' | 'preparing' | 'uploading' | 'done' | 'error' | 'cancelled';
  /** Plaintext bytes the server has accepted, advanced on chunk commit. Stored
   *  rather than derived from `progress`, because `progress` is a rounded
   *  percent and back-computing bytes from it would present rounding error as a
   *  measurement. */
  bytesSent?: number;
  /** Set when no chunk has committed for max(30s, 2x median chunk time). The
   *  threshold is history-derived, not a fixed timer: one 8 MiB chunk on a
   *  1 Mbps link takes ~67s, so any fixed short timer false-fires constantly. */
  stalled?: boolean;
  error?: string;
  previewUrl?: string;
  /** Natural pixel dimensions for image files. Captured locally before upload
   *  so the optimistic message can render with reserved vertical space and
   *  avoid layout shift when the bytes finish loading from the server. */
  width?: number;
  height?: number;
}

interface UploadResult {
  ids: string[];
  summaries: AttachmentSummary[];
}

/** What the server actually sends back from BOTH upload paths.
 *
 *  `mime_type` used to be declared here and has never been sent by either
 *  endpoint -- see the legacy handler and the session commit, which return the
 *  same four fields. safeJson casts without validating, so it was `undefined`
 *  at runtime while typed as a string, and nothing noticed because nothing
 *  reads it: buildSummary takes the MIME from the local File, which is the
 *  correct source since only the client knows it. Removed rather than added to
 *  the server: a field nobody reads is not worth sending. */
interface UploadResponse {
  file_id: string;
  file_type: string;
  file_size: number;
}

export type AttachmentRejectionKind = 'over-limit' | 'too-many' | 'empty' | 'image-too-large';

export interface AttachmentRejection {
  kind: AttachmentRejectionKind;
  fileName?: string;
  fileSize?: number;
  /** Carries limitBytes + source + entitlementBytes so the copy layer can pick
   *  its branch and name both numbers without re-resolving the limit. */
  limit: AttachmentLimit;
}

/**
 * Partition a selection against the resolved limit.
 *
 * Returns NO strings: copy lives in `AttachmentNotice`, so this stays pure,
 * copy-agnostic, and testable without a renderer.
 *
 * Accepted files are queued even when siblings are rejected. Before #2157 a
 * single oversized file made the whole selection bounce — a five-file drop with
 * one 40 MB video in it silently discarded all five.
 */
/** Whether a file's LEADING BYTES look like an image this build strips.
 *
 *  Reads only the sniff window, never the whole file. Dispatching on the
 *  declared MIME instead would let a JPEG uploaded as application/octet-stream
 *  skip the strip path entirely — a privacy regression against #2469, not a
 *  miscategorisation. */
async function sniffsAsHandledImage(file: File): Promise<boolean> {
  const head = new Uint8Array(await file.slice(0, SNIFF_BYTES).arrayBuffer());
  return sniffHandledImage(head);
}

/**
 * Async because validation now has to READ bytes: the image ceiling applies to
 * what a file IS, not to what it claims to be, and that question cannot be
 * answered from metadata. The previous purity was incidental rather than a
 * design goal.
 */
export async function validateFiles(
  newFiles: File[],
  existingCount: number,
  limit: AttachmentLimit
): Promise<{ accepted: File[]; rejections: AttachmentRejection[] }> {
  const accepted: File[] = [];
  const rejections: AttachmentRejection[] = [];
  let capacity = MAX_ATTACHMENTS - existingCount;
  let reportedTooMany = false;

  for (const file of newFiles) {
    if (capacity <= 0) {
      // Reported once for the selection, not once per surplus file.
      if (!reportedTooMany) {
        rejections.push({ kind: 'too-many', limit });
        reportedTooMany = true;
      }
      continue;
    }
    if (file.size === 0) {
      rejections.push({ kind: 'empty', fileName: file.name, fileSize: 0, limit });
      continue;
    }
    if (file.size > limit.limitBytes) {
      rejections.push({ kind: 'over-limit', fileName: file.name, fileSize: file.size, limit });
      continue;
    }
    // The image ceiling is the LAST gate, and applies only to files that
    // actually sniff as a handled image. Non-images are chunk-read and bounded
    // to about two chunks regardless of size, so no ceiling applies to them.
    //
    // Refused here rather than at upload time so the row never appears: guards
    // run before the allocating operation, and the allocation in question is
    // the whole-file read the strip path needs.
    if (file.size > IMAGE_STRIP_MAX_BYTES && (await sniffsAsHandledImage(file))) {
      rejections.push({
        kind: 'image-too-large',
        fileName: file.name,
        fileSize: file.size,
        limit,
      });
      continue;
    }

    accepted.push(file);
    capacity -= 1;
  }

  return { accepted, rejections };
}

function fileMime(entry: FileUploadState): string {
  return entry.file.type || DEFAULT_MIME;
}

function buildSummary(id: string, entry: FileUploadState): AttachmentSummary {
  return {
    id,
    file_type: classifyFileType(entry.file.type),
    mime_type: fileMime(entry),
    file_size: entry.file.size,
    width: entry.width,
    height: entry.height,
  };
}

type SetFilesFn = React.Dispatch<React.SetStateAction<FileUploadState[]>>;

/** Patches a single matching pending entry with newly-known image dimensions.
 *  Extracted from `addFiles` to keep that callback under SonarQube's nesting
 *  depth limit and to make the dim-hydration flow easy to test. */
function applyDimensionsToEntry(
  setFiles: SetFilesFn,
  file: File,
  dims: { width: number; height: number }
): void {
  setFiles((prev) =>
    prev.map((f) =>
      f.file === file && f.width === undefined
        ? { ...f, width: dims.width, height: dims.height }
        : f
    )
  );
}

/** Fire-and-forget: read the image's natural dimensions and patch the
 *  matching FileUploadState entry. Used by `addFiles` so the optimistic
 *  send-time summary can carry width/height for layout-shift-free rendering. */
function hydrateDimensions(file: File, setFiles: SetFilesFn): void {
  readImageDimensions(file).then((dims) => {
    if (dims) applyDimensionsToEntry(setFiles, file, dims);
  });
}

/** Reads natural pixel dimensions of an image file by loading it into an
 *  off-screen Image element. Returns null for non-images or load failures so
 *  callers can fall back to renderer-side onLoad sizing. */
async function readImageDimensions(file: File): Promise<{ width: number; height: number } | null> {
  if (!isImageType(file.type)) return null;
  const url = URL.createObjectURL(file);
  try {
    return await new Promise<{ width: number; height: number }>((resolve, reject) => {
      const img = new Image();
      img.onload = () => resolve({ width: img.naturalWidth, height: img.naturalHeight });
      img.onerror = () => reject(new Error('failed to read image dimensions'));
      img.src = url;
    });
  } catch {
    return null;
  } finally {
    URL.revokeObjectURL(url);
  }
}

/** Everything an upload pass needs that does not vary per file. Bundled so the
 *  two upload helpers take a context rather than a parameter list nobody can
 *  read at the call site. */
interface UploadContext {
  /** Non-nullable, and `keyVersion` likewise required — #2843/#2848 removed the
   *  fail-open `channelKey ? encrypt : plaintext` branch and narrowed these
   *  across the upload path so `tsc` re-proves the plaintext case unreachable
   *  on every build. Bundling them into this context must not re-widen them:
   *  that would restore the representability of a plaintext upload while
   *  compiling perfectly cleanly, since widening is not a type error. */
  channelKey: CryptoKey;
  keyVersion: number;
  channelId: string;
  conversationId: string | undefined;
  /** Whether the connected control plane exposes the chunked upload session.
   *  Fail-closed: false means the legacy single-shot path, which is what every
   *  server predating #2157 PR 2 gets. */
  chunkedUploadSupported: boolean;
  /** One controller per in-flight upload, keyed by uploadId. Cancel is per-file
   *  and the shared `signal` below cannot express that: aborting it would kill
   *  every queued upload, not the one whose X was clicked. */
  fileAborts: Map<string, AbortController>;
  /** Mirrors live server-side session ids so an unmount can abandon them. */
  liveSessions: Set<string>;
  /** Set once at unmount. The unmount drain aborts what is IN FLIGHT, but
   *  uploadOnePending catches an abort and returns null, so the loop would
   *  march on to the next file -- registering a controller nothing will abort
   *  and opening a session against the Set that was just cleared, which is a
   *  session no client-side path can ever abandon. This is what stops the
   *  loop, and it is the wiring the old dead abortRef never had. */
  stopped: { current: boolean };
  limit: AttachmentLimit;
}

/** Marks an entry refused because it no longer fits the current limit. */
function markOverLimit(setFiles: SetFilesFn, uploadId: string, limit: AttachmentLimit): void {
  setFiles((prev) =>
    prev.map((f) =>
      f.uploadId === uploadId
        ? {
            ...f,
            status: 'error' as const,
            error: `${f.file.name} exceeds the ${formatLimitBytes(limit.limitBytes)} limit`,
          }
        : f
    )
  );
}

/** Upload one pending entry, moving it through uploading → done | error.
 *  Returns its id + summary, or null when the upload failed. Extracted so the
 *  loop below reads as a dispatcher rather than carrying every branch itself. */
/** Floor under the stall threshold. One 8 MiB chunk on a 1 Mbps link takes
 *  ~67 s, so anything in the low seconds would false-fire on every slow link
 *  and train the user to ignore it. */
const STALL_FLOOR_MS = 30_000;

/** History-derived stall detection. Says nothing about the network and never
 *  claims "reconnecting" -- a silent token refresh must stay invisible, and
 *  this cannot tell one apart from a slow chunk. It reports only the thing it
 *  can actually observe: no chunk has committed for a while. */
function createStallWatch(uploadId: string, setFiles: SetFilesFn) {
  const gaps: number[] = [];
  let last = Date.now();
  let timer: ReturnType<typeof setTimeout> | undefined;
  let stalled = false;

  const write = (next: boolean) => {
    if (stalled === next) return; // no render per chunk for an unchanged flag
    stalled = next;
    setFiles((prev) => prev.map((f) => (f.uploadId === uploadId ? { ...f, stalled: next } : f)));
  };

  // Called ONLY from commit(), which is what makes "no stall before the first
  // chunk" true: with no observed chunk time there is nothing to derive a
  // threshold from, so no timer is ever armed and a first chunk that never
  // lands shows as plain progress at 0. Honest, if unhelpful, and better than a
  // guessed timeout. Arming this from anywhere else would break that.
  const arm = () => {
    if (timer) clearTimeout(timer);
    const sorted = [...gaps].sort((a, b) => a - b);
    // Upper median on an even count. This is a heuristic threshold, not a
    // statistic anyone reads; interpolating would be false precision.
    const median = sorted[Math.floor(sorted.length / 2)] ?? 0;
    timer = setTimeout(() => write(true), Math.max(STALL_FLOOR_MS, 2 * median));
  };

  return {
    commit() {
      const now = Date.now();
      gaps.push(now - last);
      last = now;
      write(false);
      arm();
    },
    stop() {
      if (timer) clearTimeout(timer);
      timer = undefined;
      write(false);
    },
  };
}

async function uploadOnePending(
  entry: FileUploadState,
  uploadId: string,
  setFiles: SetFilesFn,
  ctx: UploadContext
): Promise<{ id: string; summary: AttachmentSummary } | null> {
  // Per-file, because cancel is per-file. Aborting ctx.signal would kill every
  // queued upload rather than the one whose control was clicked.
  const controller = new AbortController();
  ctx.fileAborts.set(uploadId, controller);
  const stall = createStallWatch(uploadId, setFiles);

  setFiles((prev) =>
    prev.map((f) =>
      f.uploadId === uploadId
        ? {
            // On the chunked path nothing is confirmed until chunk 0 commits,
            // so the row opens as `preparing` -- which covers the whole-file
            // image strip and the session open. The legacy path is one request
            // with no observable phase inside it, so it goes straight to
            // `uploading` rather than showing a state it can never leave.
            ...f,
            status: ctx.chunkedUploadSupported ? ('preparing' as const) : ('uploading' as const),
            progress: 0,
            bytesSent: 0,
            stalled: false,
            error: undefined,
          }
        : f
    )
  );

  try {
    // Granularity is exactly 1/total: `fetch` exposes no intra-request upload
    // progress, and moving to XMLHttpRequest to get it would fork apiClient's
    // 401/403 recovery. Nothing is interpolated -- a fabricated bar would be a
    // guess presented as a measurement.
    const result = await uploadSingleFile(entry, ctx, controller.signal, (index, total) => {
      stall.commit();
      const pct = Math.round(((index + 1) / total) * 100);
      // Plaintext bytes the server has ACCEPTED, not bytes handed to fetch.
      const bytes = Math.min(entry.file.size, (index + 1) * CHUNK_PLAINTEXT_BYTES);
      setFiles((prev) =>
        prev.map((f) =>
          f.uploadId === uploadId
            ? { ...f, status: 'uploading' as const, progress: pct, bytesSent: bytes }
            : f
        )
      );
    });
    setFiles((prev) =>
      prev.map((f) =>
        f.uploadId === uploadId
          ? {
              ...f,
              status: 'done' as const,
              progress: 100,
              bytesSent: entry.file.size,
              stalled: false,
              id: result.file_id,
            }
          : f
      )
    );
    return { id: result.file_id, summary: buildSummary(result.file_id, entry) };
  } catch (err) {
    // A cancel is not a failure. Reporting one as `error` would put a red row
    // and a retry affordance in front of a user who just asked it to stop.
    const cancelled = controller.signal.aborted || err instanceof UploadAbortedError;
    const errorMsg = err instanceof Error ? err.message : 'Upload failed';
    const terminal = cancelled
      ? { status: 'cancelled' as const, stalled: false, error: undefined }
      : { status: 'error' as const, stalled: false, error: errorMsg };
    setFiles((prev) => prev.map((f) => (f.uploadId === uploadId ? { ...f, ...terminal } : f)));
    return null;
  } finally {
    stall.stop();
    ctx.fileAborts.delete(uploadId);
  }
}

/**
 * Uploads every still-'pending' entry in the React-state files array (what the
 * user added via the picker or drag-and-drop), collecting already-done ones
 * as-is. Extracted from uploadAll alongside uploadAdditionalFiles to keep its
 * cognitive complexity inside SonarQube's budget.
 */
async function uploadPendingFiles(
  files: FileUploadState[],
  setFiles: SetFilesFn,
  ctx: UploadContext
): Promise<{ ids: string[]; summaries: AttachmentSummary[] }> {
  const { limit } = ctx;
  const ids: string[] = [];
  const summaries: AttachmentSummary[] = [];

  for (const entry of files) {
    if (ctx.stopped.current) break;

    // Already-done files: collect without re-uploading.
    if (entry.status !== 'pending') {
      if (isDoneWithId(entry)) {
        ids.push(entry.id);
        summaries.push(buildSummary(entry.id, entry));
      }
      continue;
    }

    // Re-check against the CURRENT limit, not the one in force when the file
    // was queued. An entitlement can drop between queueing and sending, and a
    // premium file left pending through a downgrade would otherwise be
    // encrypted and uploaded only for the server to 413 it.
    if (entry.file.size > limit.limitBytes) {
      markOverLimit(setFiles, entry.uploadId, limit);
      continue;
    }

    const uploaded = await uploadOnePending(entry, entry.uploadId, setFiles, ctx);
    if (uploaded) {
      ids.push(uploaded.id);
      summaries.push(uploaded.summary);
    }
  }

  return { ids, summaries };
}

/**
 * Uploads a list of extra files that are NOT in React state (e.g., the
 * overflow .md synthesized by MessageInput). Extracted from uploadAll to keep
 * its cognitive complexity within SonarQube's budget.
 */
async function uploadAdditionalFiles(
  files: File[],
  ctx: UploadContext
): Promise<{ ids: string[]; summaries: AttachmentSummary[] }> {
  const { limit } = ctx;
  const ids: string[] = [];
  const summaries: AttachmentSummary[] = [];
  for (const file of files) {
    if (ctx.stopped.current) break;
    // This path builds FileUploadState directly and so never met validateFiles.
    // Same boundary, enforced here — safe to throw only because uploadAll wraps
    // the call in a try/finally that resets isUploading.
    if (file.size > limit.limitBytes) {
      throw new Error(`${file.name} exceeds the ${formatLimitBytes(limit.limitBytes)} limit`);
    }
    // Not in React state, so nothing ever reads this id back — but the type
    // requires it, and a synthesized entry with no identity would be the one
    // shape that could still be written to by position.
    const entry: FileUploadState = {
      file,
      uploadId: crypto.randomUUID(),
      progress: 0,
      status: 'pending',
    };
    // Registered in fileAborts like any other upload. There is no cancel
    // control for a synthesized overflow file, but unmount aborts everything in
    // that map -- and this path previously held only the run-wide signal, which
    // nothing ever aborted, so leaving the composer mid-upload left its request
    // running.
    const controller = new AbortController();
    ctx.fileAborts.set(entry.uploadId, controller);
    try {
      const result = await uploadSingleFile(entry, ctx, controller.signal);
      ids.push(result.file_id);
      summaries.push(buildSummary(result.file_id, entry));
    } finally {
      ctx.fileAborts.delete(entry.uploadId);
    }
  }
  return { ids, summaries };
}

function isDoneWithId(f: FileUploadState): f is FileUploadState & { id: string } {
  return f.status === 'done' && f.id != null;
}

function collectDoneFiles(files: FileUploadState[]): UploadResult {
  const done = files.filter(isDoneWithId);
  return {
    ids: done.map((f) => f.id),
    summaries: done.map((f) => buildSummary(f.id, f)),
  };
}

/**
 * Metadata stripping and chunked transport are INDEPENDENT concerns, and the
 * plan conflated them.
 *
 * Stripping rewrites EXIF/XMP and so needs the whole image in hand; that is why
 * images carry a ceiling. But an image under that ceiling can still be UPLOADED
 * in chunks -- the whole-file read is the strip, not the transport. A non-image
 * is returned untouched, so its bytes are never read here at all and the
 * chunked path reads it one chunk at a time.
 */
async function stripToUploadable(file: File): Promise<File> {
  const head = new Uint8Array(await file.slice(0, SNIFF_BYTES).arrayBuffer());
  if (!sniffHandledImage(head)) return file;

  const raw = await file.arrayBuffer();
  // Throws on a file that sniffs as a handled image but will not parse. That
  // propagates and marks the upload errored rather than silently sending
  // unstripped bytes (#2469).
  const { data } = stripFileMetadata(raw, file.type);
  return new File([data], file.name, { type: file.type });
}

async function uploadSingleFile(
  entry: FileUploadState,
  ctx: UploadContext,
  signal: AbortSignal,
  onChunkCommitted: (index: number, total: number) => void = () => {}
): Promise<UploadResponse> {
  const { channelKey, keyVersion, channelId, conversationId } = ctx;

  if (ctx.chunkedUploadSupported) {
    return uploadAttachmentChunked(
      await stripToUploadable(entry.file),
      channelKey,
      {
        // EXACTLY ONE. A DM carries the conversation id in BOTH `channelId` and
        // `conversationId` (MessageInput computes `conversationId || channelId`),
        // so forwarding both sent the same UUID twice and the server refused it.
        ...(conversationId ? { conversationId } : { channelId }),
        keyVersion,
        fileType: classifyFileType(entry.file.type),
        mimeType: entry.file.type || DEFAULT_MIME,
      },
      signal,
      {
        onChunkCommitted,
        onSessionOpened: (id) => ctx.liveSessions.add(id),
        onSessionClosed: (id) => ctx.liveSessions.delete(id),
      }
    );
  }

  const formData = await encryptAndBuildForm(
    entry,
    channelKey,
    keyVersion,
    channelId,
    conversationId
  );
  const response = await apiFetch('/api/v1/media/upload/attachment', {
    method: 'POST',
    body: formData,
  });
  if (!response.ok) {
    const errBody = await response.text();
    throw new Error(errBody || `Upload failed (${response.status})`);
  }
  return safeJson<UploadResponse>(response);
}

/**
 * Encrypts one attachment and builds its upload form.
 *
 * #2843: `channelKey` is non-nullable and there is deliberately no unencrypted
 * path. This previously read `channelKey ? await encryptFile(...) : fileData`,
 * with `channelKey: CryptoKey | null` — residue from before #1024, when an
 * `is_encrypted = false` channel legitimately uploaded plaintext and the null
 * key meant "this channel is not encrypted". #1031 removed the `isEncrypted`
 * gate that selected that branch but left the branch itself, so a null key
 * would have silently uploaded the file in the clear.
 *
 * `e2eeService.getChannelKey` returns `Promise<CryptoKey>` and throws rather
 * than resolving null, so the null case is unrepresentable — expressing that in
 * the type is what keeps it unrepresentable. `key_version` is likewise always
 * sent: `getCurrentKeyVersion` returns a number, never undefined.
 */
async function encryptAndBuildForm(
  entry: FileUploadState,
  channelKey: CryptoKey,
  keyVersion: number,
  channelId: string,
  conversationId?: string
): Promise<FormData> {
  const raw = await entry.file.arrayBuffer();

  // #2469: strip identifying metadata BEFORE encryption. These attachments are
  // end-to-end encrypted, so the server never sees plaintext and cannot strip
  // anything — a GPS-tagged photo would reach every recipient intact and could
  // not be remediated afterwards. Only the sending client can remove it.
  //
  // It sits ahead of the encrypt call rather than inside encryptFile, which
  // takes an ArrayBuffer with no MIME context to dispatch on.
  //
  // stripFileMetadata THROWS on a file that sniffs as a handled image but will
  // not parse. That propagates to uploadPendingFiles, which marks the file
  // errored — the upload fails rather than silently sending unstripped bytes.
  const { data: fileData } = stripFileMetadata(raw, entry.file.type);

  const uploadData = await encryptFile(fileData, channelKey);

  const formData = new FormData();
  formData.append('file', new Blob([uploadData]), entry.file.name);
  formData.append('file_type', classifyFileType(entry.file.type));
  formData.append('mime_type', entry.file.type || DEFAULT_MIME);
  formData.append('key_version', String(keyVersion));
  if (conversationId) {
    formData.append('conversation_id', conversationId);
  } else {
    formData.append('channel_id', channelId);
  }
  return formData;
}

export function useFileUpload() {
  const [files, setFiles] = useState<FileUploadState[]>([]);
  const [isUploading, setIsUploading] = useState(false);

  // Read the entitlement UNCONDITIONALLY — no `hydrated`/`degraded` branch. The
  // store is already FREE when unhydrated and preserves last-known-good on a
  // reconnect blip (#2172), so a hydration gate here could only ever escalate
  // above the store's current value. See `resolveAttachmentLimit`.
  const userMaxAttachmentBytes = useEntitlement((e) => e.maxAttachmentBytes);
  // Fail-closed: a null capability set, a missing `features`, or an absent flag
  // all read as false, which keeps the legacy ceiling. Only an explicit `true`
  // from a server that advertises the session routes lifts it — the direction
  // that can only ever narrow the limit, never escalate it.
  const capabilityUnknown = useClientConfigStore(
    (s) => s.chunkedUploadCapability.status === 'error'
  );
  const chunkedUploadSupported = useClientConfigStore(
    // Only an explicit 'supported' takes the chunked path. 'error' and
    // 'confirmed-unsupported' both fail closed, and the DIFFERENCE between them
    // is carried into the limit below so the notice can say which one happened.
    (s) => s.chunkedUploadCapability.status === 'supported'
  );
  const limit = useMemo(
    // #1556 SEAM: pass `serverMaxUploadBytes` here once the server composes the
    // Mach axis. Nothing else in this file changes when it does.
    () =>
      resolveAttachmentLimit({ userMaxAttachmentBytes, chunkedUploadSupported, capabilityUnknown }),
    [userMaxAttachmentBytes, chunkedUploadSupported, capabilityUnknown]
  );

  // `addFiles` is a useCallback with an EMPTY dep array whose body runs inside a
  // setFiles updater. Putting `limit` in its deps would churn its identity every
  // render and propagate into every memoized consumer, so the current value is
  // handed over by ref instead. Do not "simplify" this into a dep.
  const limitRef = useRef(limit);
  limitRef.current = limit;
  /** Latched at unmount so an in-progress uploadAll stops between files rather
   *  than starting one nothing can cancel. Never reset: this component instance
   *  is gone, and a remount gets a fresh ref. */
  const stoppedRef = useRef(false);
  /** Live per-file controllers, keyed by uploadId. EVERY cancel path goes
   *  through this map -- per-file cancel and unmount alike. A run-wide
   *  controller alongside it was dead code: nothing aborted it, and aborting it
   *  would have killed every queued upload rather than the one asked to stop. */
  const fileAbortsRef = useRef(new Map<string, AbortController>());
  /** Server-side session ids with bytes staged against them. The unmount effect
   *  below is the only thing that can free them from the client side; the
   *  server sweeper is the authority when it cannot. */
  const liveSessionsRef = useRef(new Set<string>());

  // Mirror of the queue, kept current so addFiles can validate SYNCHRONOUSLY.
  // Computing inside a setFiles updater and reading a ref straight after is
  // unsound: React may defer the updater, so the read can return a PREVIOUS
  // selection's result — the composer then shows a stale notice, or none.
  // Written eagerly by every path that mutates `files` (addFiles, removeFile,
  // clearFiles) so back-to-back calls compose within a single tick. The effect
  // below reconciles it against committed state for async patches such as
  // hydrateDimensions and upload-status updates.
  const filesRef = useRef<FileUploadState[]>(files);
  useEffect(() => {
    filesRef.current = files;
  }, [files]);

  // Async because validateFiles now reads each file's leading bytes to decide
  // whether the image ceiling applies. Callers do not await it — a drop handler
  // has nothing to do with the promise — but the rejections still land through
  // the same setFiles path, one tick later.
  const addFiles = useCallback(async (newFiles: FileList | File[]) => {
    const fileArray = Array.from(newFiles);
    const { accepted, rejections } = await validateFiles(
      fileArray,
      filesRef.current.length,
      limitRef.current
    );

    if (accepted.length > 0) {
      // Object URLs are minted HERE rather than inside the updater: React may
      // invoke an updater more than once (StrictMode does so deliberately), and
      // every extra invocation would mint a url nothing ever revokes.
      const newEntries: FileUploadState[] = accepted.map((file) => ({
        file,
        uploadId: crypto.randomUUID(),
        progress: 0,
        status: 'pending' as const,
        previewUrl: isImageType(file.type) ? URL.createObjectURL(file) : undefined,
      }));
      const next = [...filesRef.current, ...newEntries];
      filesRef.current = next;
      setFiles(next);

      // Asynchronously read image dimensions and patch the matching entries.
      // Iterates ACCEPTED only: hydrating a rejected file would mint an object
      // url for an entry that never enters the queue.
      for (const file of accepted) {
        if (isImageType(file.type)) hydrateDimensions(file, setFiles);
      }
    }

    // The accepted COUNT is reported rather than derived: a `too-many`
    // rejection is emitted once for a whole surplus, so
    // `total - rejections.length` over-reports. Dropping 8 files on an empty
    // queue accepts 5 and discards 3, but yields one rejection.
    return { accepted: accepted.length, rejections };
  }, []);

  // These write `filesRef` eagerly, the same way addFiles does. Leaving the
  // mirror to the passive effect above is not enough: passive effects run after
  // paint, so an addFiles in that window rebuilt the queue from a stale mirror
  // and RESURRECTED the entry just removed. Removing an attachment and adding
  // another in the same tick is an ordinary composer action, not a rare
  // interleaving.
  //
  // The write must sit OUTSIDE the updater. React may defer an updater, so
  // assigning the ref inside one lands after a same-tick addFiles has already
  // read the mirror — the exact unsoundness addFiles documents above. Revoking
  // out here is the safer half of the same move: an updater may run more than
  // once, and revoking twice on a re-invocation is not idempotent.
  const removeFile = useCallback((index: number) => {
    const mirror = filesRef.current;
    const removed = mirror[index];
    if (removed?.previewUrl) {
      URL.revokeObjectURL(removed.previewUrl);
    }
    // Removing the card must also stop the upload behind it. Without this the
    // chunk PUT loop keeps running against a session the UI no longer shows --
    // burning the user's bandwidth and their ingress budget on a file they
    // just deleted, and leaving the staged bytes to the sweeper's hard TTL.
    //
    // Abort is the WHOLE fix: uploadAttachmentChunked cancels the server
    // session on any non-completing exit, which DELETEs it and fires
    // onSessionClosed. No delete from the map here, matching cancelUpload --
    // uploadOnePending's finally already removes the entry.
    //
    // Harmless before this PR: there were no per-file controllers and no
    // server-side sessions to strand. The chunked path is what gave the
    // omission consequences.
    if (removed) {
      fileAbortsRef.current.get(removed.uploadId)?.abort();
    }
    filesRef.current = mirror.filter((_, i) => i !== index);
    // The ref write is eager; the STATE write stays functional. Rebuilding state
    // from the ref instead would clobber an async patch — hydrateDimensions
    // commits through a functional update, and the ref only catches up in the
    // effect. Functional-from-committed-`prev` keeps that patch; the effect then
    // reconciles the ref back to committed state. hydrateDimensions patches
    // fields in place without reordering or removing, so the index means the
    // same thing in both.
    setFiles((prev) => prev.filter((_, i) => i !== index));
  }, []);

  /** Per-file cancel. One click, no confirmation: the work being discarded is
   *  the user's own upload, and a confirm dialog on a 100 px card costs more
   *  than the mistake it prevents. Re-adding the file is the undo. */
  const cancelUpload = useCallback((uploadId: string) => {
    // Abort first: uploadOnePending reads signal.aborted to decide `cancelled`
    // vs `error`, so a status write racing ahead of the abort would be
    // overwritten by the catch as a failure.
    fileAbortsRef.current.get(uploadId)?.abort();
    setFiles((prev) =>
      prev.map((f) =>
        f.uploadId === uploadId && (f.status === 'uploading' || f.status === 'preparing')
          ? { ...f, status: 'cancelled' as const, stalled: false }
          : f
      )
    );
  }, []);

  // Unmount is the last chance to release staged bytes from the client side.
  // `keepalive` lets the DELETE outlive the document; sendBeacon cannot be used
  // because it carries no Authorization header. NEITHER is guaranteed to
  // arrive, which is precisely why the server-side sweeper is load-bearing for
  // correctness and not defence in depth.
  useEffect(() => {
    const aborts = fileAbortsRef.current;
    const sessions = liveSessionsRef.current;
    return () => {
      // Ordering against the abort loop below does not matter -- this whole
      // cleanup is synchronous, so no abort rejection can be delivered until
      // after it returns. (Verified: moving this line below the loop leaves the
      // test green.) First for reading order, not for correctness.
      stoppedRef.current = true;
      for (const controller of aborts.values()) controller.abort();
      for (const sessionId of sessions) abandonSessionOnUnload(sessionId);
      sessions.clear();
    };
  }, []);

  const clearFiles = useCallback(() => {
    for (const f of filesRef.current) {
      if (f.previewUrl) URL.revokeObjectURL(f.previewUrl);
    }
    filesRef.current = [];
    setFiles([]);
  }, []);

  const uploadAll = useCallback(
    async (
      channelId: string,
      conversationId?: string,
      /** Extra files to upload in the same call without going through React state.
       *  Used by the overflow path in MessageInput to avoid the addFiles→setFiles
       *  async-state-update race: the overflow .md File is passed here directly so
       *  it is uploaded in the same uploadAll invocation rather than via a stale
       *  closure snapshot. */
      additionalFiles?: File[]
    ): Promise<UploadResult> => {
      const hasPending = files.some((f) => f.status === 'pending');
      const hasAdditional = additionalFiles && additionalFiles.length > 0;
      if (!hasPending && !hasAdditional) {
        return collectDoneFiles(files);
      }

      setIsUploading(true);

      // Without this finally, ANY throw below leaves isUploading === true and
      // the composer's send button disabled for the rest of the session — a
      // network blip during one upload bricks the composer until reload. The
      // size guard in uploadAdditionalFiles is only safe because of it.
      try {
        const keyChannelId = conversationId || channelId;
        const channelKey = await e2eeService.getChannelKey(keyChannelId);
        const keyVersion = e2eeService.getCurrentKeyVersion(keyChannelId);

        // Upload pending files from React state (user-added via picker / drag-drop).
        const ctx: UploadContext = {
          channelKey,
          keyVersion,
          channelId,
          conversationId,
          chunkedUploadSupported,
          fileAborts: fileAbortsRef.current,
          liveSessions: liveSessionsRef.current,
          stopped: stoppedRef,
          limit: limitRef.current,
        };
        const pending = await uploadPendingFiles(files, setFiles, ctx);

        // Upload any additional files passed synchronously (e.g., overflow .md).
        // These are NOT in React state so they bypass the addFiles→setFiles
        // async-update race entirely.
        const extra =
          hasAdditional && additionalFiles
            ? await uploadAdditionalFiles(additionalFiles, ctx)
            : { ids: [], summaries: [] };

        return {
          ids: [...pending.ids, ...extra.ids],
          summaries: [...pending.summaries, ...extra.summaries],
        };
      } finally {
        setIsUploading(false);
      }
    },
    // chunkedUploadSupported selects the upload PATH, so a stale value here
    // would keep sending single-shot uploads after a reconnect told us the
    // server supports sessions -- silently, and only for premium-sized files.
    [files, chunkedUploadSupported]
  );

  const hasFiles = files.length > 0;

  return {
    files,
    addFiles,
    removeFile,
    cancelUpload,
    clearFiles,
    uploadAll,
    isUploading,
    hasFiles,
    /** The resolved per-file limit. Consumers render copy from this rather than
     *  re-deriving it, so the enforced number and the named number cannot drift
     *  apart — which is exactly how #2157 happened. */
    limit,
  };
}
