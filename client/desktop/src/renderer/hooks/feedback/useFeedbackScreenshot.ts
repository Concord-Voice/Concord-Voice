import { useCallback, useEffect, useRef, useState } from 'react';
import { apiFetch, safeJson } from '../../services/system/apiClient';

/**
 * Upload hook for bug-report screenshots (#1747).
 *
 * Unlike `useImageUpload` (profile crop — local state only) and `useFileUpload`
 * (E2EE chat attachments), this uploads a plain image to the authenticated
 * Tier-1 endpoint `POST /api/v1/media/upload/feedback-screenshot` and keeps the
 * returned relative media URL. The server re-encodes (stripping EXIF/GPS) and
 * returns an unguessable `/api/v1/media/feedback-screenshots/<uuid>` path that
 * `BugReportPanel` hands to the feedback submit as `attachments[].url`.
 *
 * Caps (count / size / type) are enforced here for UX; the control-plane
 * re-enforces them as defense-in-depth (`internal/feedback` validate + the
 * Tier-1 upload pipeline).
 */

const UPLOAD_ENDPOINT = '/api/v1/media/upload/feedback-screenshot';

export type ScreenshotStatus = 'uploading' | 'done' | 'error';

export interface FeedbackScreenshot {
  /** Stable local id (not the server id). */
  id: string;
  file: File;
  /** Local object URL for the thumbnail; revoked on remove/clear/unmount. */
  previewUrl: string;
  status: ScreenshotStatus;
  /** The relative media path, set when status === 'done'. */
  url?: string;
}

export interface UseFeedbackScreenshotOptions {
  maxCount: number;
  /** Per-file cap in bytes. */
  maxSize: number;
  allowedTypes: string[];
  /** Surfaces a user-facing validation/upload message (transient). */
  onError?: (message: string) => void;
}

export interface UseFeedbackScreenshotReturn {
  screenshots: FeedbackScreenshot[];
  addFiles: (files: FileList | File[]) => void;
  remove: (id: string) => void;
  clear: () => void;
  /** Refs for the `attachments` payload — only successfully-uploaded items. */
  uploadedRefs: () => { url: string }[];
  /** True while any screenshot is still uploading (submit should wait). */
  isUploading: boolean;
  atCapacity: boolean;
}

export function useFeedbackScreenshot(
  opts: UseFeedbackScreenshotOptions
): UseFeedbackScreenshotReturn {
  const { maxCount, maxSize, allowedTypes, onError } = opts;
  const [screenshots, setScreenshots] = useState<FeedbackScreenshot[]>([]);

  // Mirror for synchronous reads in addFiles (capacity) and uploadedRefs /
  // unmount cleanup without stale closures.
  const ref = useRef<FeedbackScreenshot[]>(screenshots);
  ref.current = screenshots;

  // Revoke any outstanding object URLs when the panel unmounts.
  useEffect(() => {
    return () => {
      ref.current.forEach((s) => URL.revokeObjectURL(s.previewUrl));
    };
  }, []);

  const upload = useCallback(
    async (item: FeedbackScreenshot) => {
      try {
        const form = new FormData();
        form.append('file', item.file, item.file.name);
        // No Content-Type header — the browser sets the multipart boundary.
        const res = await apiFetch(UPLOAD_ENDPOINT, { method: 'POST', body: form });
        if (!res.ok) throw new Error(`upload failed (${res.status})`);
        const data = await safeJson<{ url?: string }>(res);
        if (!data.url) throw new Error('upload response missing url');
        const uploadedUrl = data.url;
        setScreenshots((prev) =>
          prev.map((s) => (s.id === item.id ? { ...s, status: 'done', url: uploadedUrl } : s))
        );
      } catch {
        setScreenshots((prev) =>
          prev.map((s) => (s.id === item.id ? { ...s, status: 'error' } : s))
        );
        onError?.('A screenshot failed to upload. Remove it and try again.');
      }
    },
    [onError]
  );

  const addFiles = useCallback(
    (files: FileList | File[]) => {
      const incoming = Array.from(files);
      let remaining = maxCount - ref.current.length;
      const accepted: FeedbackScreenshot[] = [];
      for (const file of incoming) {
        if (remaining <= 0) {
          onError?.(`You can attach up to ${maxCount} screenshots.`);
          break;
        }
        if (!allowedTypes.includes(file.type)) {
          onError?.('Only PNG, JPEG, and WebP images can be attached.');
          continue;
        }
        if (file.size > maxSize) {
          onError?.(`Each screenshot must be under ${Math.round(maxSize / (1024 * 1024))} MB.`);
          continue;
        }
        accepted.push({
          id: crypto.randomUUID(),
          file,
          previewUrl: URL.createObjectURL(file),
          status: 'uploading',
        });
        remaining--;
      }
      if (accepted.length === 0) return;
      setScreenshots((prev) => [...prev, ...accepted]);
      accepted.forEach((item) => void upload(item));
    },
    [maxCount, maxSize, allowedTypes, onError, upload]
  );

  const remove = useCallback((id: string) => {
    setScreenshots((prev) => {
      const found = prev.find((s) => s.id === id);
      if (found) URL.revokeObjectURL(found.previewUrl);
      return prev.filter((s) => s.id !== id);
    });
  }, []);

  const clear = useCallback(() => {
    setScreenshots((prev) => {
      prev.forEach((s) => URL.revokeObjectURL(s.previewUrl));
      return [];
    });
  }, []);

  const uploadedRefs = useCallback(
    () =>
      ref.current
        .filter((s) => s.status === 'done' && s.url)
        .map((s) => ({ url: s.url as string })),
    []
  );

  return {
    screenshots,
    addFiles,
    remove,
    clear,
    uploadedRefs,
    isUploading: screenshots.some((s) => s.status === 'uploading'),
    atCapacity: screenshots.length >= maxCount,
  };
}
