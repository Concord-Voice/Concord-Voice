import React, { useState, useCallback, useEffect, useMemo, useRef } from 'react';
import { apiFetch, safeJson } from '../services/apiClient';
import { e2eeService } from '../services/e2eeService';
import {
  encryptFile,
  classifyFileType,
  isImageType,
  MAX_ATTACHMENTS,
} from '../utils/attachmentCrypto';
import {
  formatLimitBytes,
  resolveAttachmentLimit,
  type AttachmentLimit,
} from '../utils/entitlementLimits';
import { useEntitlement } from './useEntitlement';
import type { AttachmentSummary } from '../types/chat';

const DEFAULT_MIME = 'application/octet-stream';

export interface FileUploadState {
  file: File;
  id?: string;
  progress: number;
  status: 'pending' | 'uploading' | 'done' | 'error';
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

interface UploadResponse {
  file_id: string;
  file_type: string;
  mime_type: string;
  file_size: number;
}

export type AttachmentRejectionKind = 'over-limit' | 'too-many' | 'empty';

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
export function validateFiles(
  newFiles: File[],
  existingCount: number,
  limit: AttachmentLimit
): { accepted: File[]; rejections: AttachmentRejection[] } {
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

/**
 * Iterates over the files-in-React-state array (the files the user added via
 * the file picker or drag-and-drop) and uploads any that are still 'pending'.
 * Already-done files are collected as-is. Mirrors the shape of
 * uploadAdditionalFiles to keep uploadAll's cognitive complexity ≤ 15.
 *
 * @param files   - Snapshot of the React state files array at call time.
 * @param setFiles - React setState dispatcher for per-file progress updates.
 * @param channelKey - Encryption key for the channel.
 * @param keyVersion - Key epoch for the upload metadata.
 * @param channelId - Channel ID (used for server channels).
 * @param conversationId - Conversation ID (used for DMs, takes precedence).
 * @param abortRef - Shared abort flag; if set mid-loop, remaining files are skipped.
 */
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
  abortRef: React.MutableRefObject<boolean>;
  limit: AttachmentLimit;
}

/** Marks an entry refused because it no longer fits the current limit. */
function markOverLimit(setFiles: SetFilesFn, index: number, limit: AttachmentLimit): void {
  setFiles((prev) =>
    prev.map((f, idx) =>
      idx === index
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
async function uploadOnePending(
  entry: FileUploadState,
  index: number,
  setFiles: SetFilesFn,
  ctx: UploadContext
): Promise<{ id: string; summary: AttachmentSummary } | null> {
  const { channelKey, keyVersion, channelId, conversationId } = ctx;

  setFiles((prev) =>
    prev.map((f, idx) => (idx === index ? { ...f, status: 'uploading' as const, progress: 0 } : f))
  );

  try {
    const result = await uploadSingleFile(entry, channelKey, keyVersion, channelId, conversationId);
    setFiles((prev) =>
      prev.map((f, idx) =>
        idx === index ? { ...f, status: 'done' as const, progress: 100, id: result.file_id } : f
      )
    );
    return { id: result.file_id, summary: buildSummary(result.file_id, entry) };
  } catch (err) {
    const errorMsg = err instanceof Error ? err.message : 'Upload failed';
    setFiles((prev) =>
      prev.map((f, idx) =>
        idx === index ? { ...f, status: 'error' as const, error: errorMsg } : f
      )
    );
    return null;
  }
}

async function uploadPendingFiles(
  files: FileUploadState[],
  setFiles: SetFilesFn,
  ctx: UploadContext
): Promise<{ ids: string[]; summaries: AttachmentSummary[] }> {
  const { abortRef, limit } = ctx;
  const ids: string[] = [];
  const summaries: AttachmentSummary[] = [];

  for (let i = 0; i < files.length; i++) {
    if (abortRef.current) break;
    const entry = files[i];

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
      markOverLimit(setFiles, i, limit);
      continue;
    }

    const uploaded = await uploadOnePending(entry, i, setFiles, ctx);
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
  channelKey: CryptoKey,
  keyVersion: number,
  channelId: string,
  conversationId: string | undefined,
  abortRef: React.MutableRefObject<boolean>,
  limit: AttachmentLimit
): Promise<{ ids: string[]; summaries: AttachmentSummary[] }> {
  const ids: string[] = [];
  const summaries: AttachmentSummary[] = [];
  for (const file of files) {
    if (abortRef.current) break;
    // This path builds FileUploadState directly and so never met validateFiles.
    // Same boundary, enforced here — safe to throw only because uploadAll wraps
    // the call in a try/finally that resets isUploading.
    if (file.size > limit.limitBytes) {
      throw new Error(`${file.name} exceeds the ${formatLimitBytes(limit.limitBytes)} limit`);
    }
    const entry: FileUploadState = { file, progress: 0, status: 'pending' };
    const result = await uploadSingleFile(entry, channelKey, keyVersion, channelId, conversationId);
    ids.push(result.file_id);
    summaries.push(buildSummary(result.file_id, entry));
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

async function uploadSingleFile(
  entry: FileUploadState,
  channelKey: CryptoKey,
  keyVersion: number,
  channelId: string,
  conversationId?: string
): Promise<UploadResponse> {
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
  const fileData = await entry.file.arrayBuffer();
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
  const limit = useMemo(
    // #1556 SEAM: pass `serverMaxUploadBytes` here once the server composes the
    // Mach axis. Nothing else in this file changes when it does.
    () => resolveAttachmentLimit({ userMaxAttachmentBytes }),
    [userMaxAttachmentBytes]
  );

  // `addFiles` is a useCallback with an EMPTY dep array whose body runs inside a
  // setFiles updater. Putting `limit` in its deps would churn its identity every
  // render and propagate into every memoized consumer, so the current value is
  // handed over by ref instead. Do not "simplify" this into a dep.
  const limitRef = useRef(limit);
  limitRef.current = limit;
  const abortRef = useRef(false);

  // Mirror of the queue, kept current so addFiles can validate SYNCHRONOUSLY.
  // Computing inside a setFiles updater and reading a ref straight after is
  // unsound: React may defer the updater, so the read can return a PREVIOUS
  // selection's result — the composer then shows a stale notice, or none.
  // Written eagerly by addFiles (so back-to-back calls compose) and reconciled
  // by the effect below for every other path that mutates `files`.
  const filesRef = useRef<FileUploadState[]>(files);
  useEffect(() => {
    filesRef.current = files;
  }, [files]);

  const addFiles = useCallback((newFiles: FileList | File[]) => {
    const fileArray = Array.from(newFiles);
    const { accepted, rejections } = validateFiles(
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

  const removeFile = useCallback((index: number) => {
    setFiles((prev) => {
      const removed = prev[index];
      if (removed?.previewUrl) {
        URL.revokeObjectURL(removed.previewUrl);
      }
      return prev.filter((_, i) => i !== index);
    });
  }, []);

  const clearFiles = useCallback(() => {
    setFiles((prev) => {
      for (const f of prev) {
        if (f.previewUrl) URL.revokeObjectURL(f.previewUrl);
      }
      return [];
    });
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
      abortRef.current = false;

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
          abortRef,
          limit: limitRef.current,
        };
        const pending = await uploadPendingFiles(files, setFiles, ctx);

        // Upload any additional files passed synchronously (e.g., overflow .md).
        // These are NOT in React state so they bypass the addFiles→setFiles
        // async-update race entirely.
        const extra =
          hasAdditional && additionalFiles
            ? await uploadAdditionalFiles(
                additionalFiles,
                channelKey,
                keyVersion,
                channelId,
                conversationId,
                abortRef,
                limitRef.current
              )
            : { ids: [], summaries: [] };

        return {
          ids: [...pending.ids, ...extra.ids],
          summaries: [...pending.summaries, ...extra.summaries],
        };
      } finally {
        setIsUploading(false);
      }
    },
    [files]
  );

  const hasFiles = files.length > 0;

  return {
    files,
    addFiles,
    removeFile,
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
