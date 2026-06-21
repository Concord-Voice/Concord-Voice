import React, { useState, useCallback, useRef } from 'react';
import { apiFetch, safeJson } from '../services/apiClient';
import { e2eeService } from '../services/e2eeService';
import {
  encryptFile,
  classifyFileType,
  formatFileSize,
  isImageType,
  MAX_FILE_SIZE,
  MAX_ATTACHMENTS,
} from '../utils/attachmentCrypto';
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

/**
 * Validates files before adding to the queue.
 * Returns an error string if validation fails, null if valid.
 */
export function validateFiles(newFiles: File[], existingCount: number): string | null {
  const totalCount = existingCount + newFiles.length;
  if (totalCount > MAX_ATTACHMENTS) {
    return `Maximum ${MAX_ATTACHMENTS} attachments per message`;
  }
  for (const file of newFiles) {
    if (file.size > MAX_FILE_SIZE) {
      return `${file.name} exceeds the ${formatFileSize(MAX_FILE_SIZE)} limit`;
    }
    if (file.size === 0) {
      return `${file.name} is empty`;
    }
  }
  return null;
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
async function uploadPendingFiles(
  files: FileUploadState[],
  setFiles: SetFilesFn,
  channelKey: CryptoKey | null,
  keyVersion: number | undefined,
  channelId: string,
  conversationId: string | undefined,
  abortRef: React.MutableRefObject<boolean>
): Promise<{ ids: string[]; summaries: AttachmentSummary[] }> {
  const ids: string[] = [];
  const summaries: AttachmentSummary[] = [];

  for (let i = 0; i < files.length; i++) {
    if (abortRef.current) break;
    const entry = files[i];

    // Already-done files: collect without re-uploading
    if (entry.status !== 'pending') {
      if (isDoneWithId(entry)) {
        ids.push(entry.id);
        summaries.push(buildSummary(entry.id, entry));
      }
      continue;
    }

    setFiles((prev) =>
      prev.map((f, idx) => (idx === i ? { ...f, status: 'uploading' as const, progress: 0 } : f))
    );

    try {
      const result = await uploadSingleFile(
        entry,
        channelKey,
        keyVersion,
        channelId,
        conversationId
      );
      ids.push(result.file_id);
      summaries.push(buildSummary(result.file_id, entry));
      setFiles((prev) =>
        prev.map((f, idx) =>
          idx === i ? { ...f, status: 'done' as const, progress: 100, id: result.file_id } : f
        )
      );
    } catch (err) {
      const errorMsg = err instanceof Error ? err.message : 'Upload failed';
      setFiles((prev) =>
        prev.map((f, idx) => (idx === i ? { ...f, status: 'error' as const, error: errorMsg } : f))
      );
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
  channelKey: CryptoKey | null,
  keyVersion: number | undefined,
  channelId: string,
  conversationId: string | undefined,
  abortRef: React.MutableRefObject<boolean>
): Promise<{ ids: string[]; summaries: AttachmentSummary[] }> {
  const ids: string[] = [];
  const summaries: AttachmentSummary[] = [];
  for (const file of files) {
    if (abortRef.current) break;
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
  channelKey: CryptoKey | null,
  keyVersion: number | undefined,
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

async function encryptAndBuildForm(
  entry: FileUploadState,
  channelKey: CryptoKey | null,
  keyVersion: number | undefined,
  channelId: string,
  conversationId?: string
): Promise<FormData> {
  const fileData = await entry.file.arrayBuffer();
  const uploadData = channelKey ? await encryptFile(fileData, channelKey) : fileData;

  const formData = new FormData();
  formData.append('file', new Blob([uploadData]), entry.file.name);
  formData.append('file_type', classifyFileType(entry.file.type));
  formData.append('mime_type', entry.file.type || DEFAULT_MIME);
  if (keyVersion !== undefined) {
    formData.append('key_version', String(keyVersion));
  }
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
  const abortRef = useRef(false);
  const validationResultRef = useRef<string | null>(null);

  const addFiles = useCallback((newFiles: FileList | File[]) => {
    const fileArray = Array.from(newFiles);

    setFiles((prev) => {
      const error = validateFiles(fileArray, prev.length);
      validationResultRef.current = error;
      if (error) {
        return prev;
      }

      const newEntries: FileUploadState[] = fileArray.map((file) => ({
        file,
        progress: 0,
        status: 'pending' as const,
        previewUrl: isImageType(file.type) ? URL.createObjectURL(file) : undefined,
      }));
      return [...prev, ...newEntries];
    });

    // Asynchronously read image dimensions and patch the matching entries.
    // Dim-reading races against the user clicking "Send", but uploadAll only
    // builds summaries from the latest state, so a late-arriving dim still
    // ends up on the optimistic message in practice.
    for (const file of fileArray) {
      if (!isImageType(file.type)) continue;
      hydrateDimensions(file, setFiles);
    }

    return validationResultRef.current;
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

      const keyChannelId = conversationId || channelId;
      const channelKey = await e2eeService.getChannelKey(keyChannelId);
      const keyVersion = e2eeService.getCurrentKeyVersion(keyChannelId);

      // Upload pending files from React state (user-added via picker / drag-drop).
      const pending = await uploadPendingFiles(
        files,
        setFiles,
        channelKey,
        keyVersion,
        channelId,
        conversationId,
        abortRef
      );

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
              abortRef
            )
          : { ids: [], summaries: [] };

      setIsUploading(false);
      return {
        ids: [...pending.ids, ...extra.ids],
        summaries: [...pending.summaries, ...extra.summaries],
      };
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
  };
}
