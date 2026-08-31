import React from 'react';
import { X, FileText, Film, Music, File } from 'lucide-react';
import type { FileUploadState } from '../../hooks/messaging/useFileUpload';
import { classifyFileType, formatFileSize } from '../../utils/crypto/attachmentCrypto';
import './AttachmentUploadPreview.css';

interface AttachmentUploadPreviewProps {
  files: FileUploadState[];
  onRemove: (index: number) => void;
  /** Aborts an in-flight upload and fires the session DELETE. Optional so the
   *  component keeps working for callers that only queue files. */
  onCancel?: (uploadId: string) => void;
}

function getFileIcon(mimeType: string) {
  const type = classifyFileType(mimeType);
  switch (type) {
    case 'video':
      return <Film size={24} />;
    case 'audio':
      return <Music size={24} />;
    case 'file':
      return <File size={24} />;
    default:
      return <FileText size={24} />;
  }
}

/** True while the row owns work that a click can stop. */
function isLive(status: FileUploadState['status']): boolean {
  return status === 'preparing' || status === 'uploading';
}

/** A percent is not available for every live state, and inventing one would be
 *  the same dishonesty as an interpolated bar. `preparing` has committed
 *  nothing yet; a stalled upload has committed something but cannot say when
 *  the next chunk lands. Both render indeterminate. */
function isIndeterminate(entry: FileUploadState): boolean {
  return entry.status === 'preparing' || entry.stalled === true;
}

function progressText(entry: FileUploadState): string {
  if (entry.status === 'preparing') return 'Preparing…';
  // Names no cause. This cannot tell a slow chunk from a silent token refresh
  // from a dropped link, so it reports only what it observed: nothing has
  // committed for a while. Saying "reconnecting" would be a guess about the
  // network stated as fact.
  if (entry.stalled) return 'Still uploading…';
  return `${formatFileSize(entry.bytesSent ?? 0)} / ${formatFileSize(entry.file.size)}`;
}

const AttachmentUploadPreview: React.FC<AttachmentUploadPreviewProps> = ({
  files,
  onRemove,
  onCancel,
}) => {
  if (files.length === 0) return null;

  return (
    <div className="attachment-upload-preview">
      {files.map((entry, index) => {
        const previewUrl = entry.previewUrl;
        const live = isLive(entry.status);
        const indeterminate = isIndeterminate(entry);
        return (
          <div
            // The disable that used to sit here asserted the list "only
            // grows/shrinks from the tail, never reorders". That was false:
            // removeFile filters by index, so removing anything but the last
            // entry shifts every survivor down. FileUploadState now carries a
            // stable uploadId minted at queue time.
            key={entry.uploadId}
            className={`attachment-preview-item ${entry.status}`}
          >
            <button
              className="attachment-remove-btn"
              // Cancel and remove are one control because they are one intent:
              // "I do not want this file." Splitting them would put two 20 px
              // targets on a 100 px card. Focus is not moved after a cancel --
              // the row survives as `cancelled` and this button stays under the
              // cursor, so there is nothing to move focus to and no focus loss
              // to recover from.
              onClick={() => (live && onCancel ? onCancel(entry.uploadId) : onRemove(index))}
              aria-label={
                live ? `Cancel upload of ${entry.file.name}` : `Remove ${entry.file.name}`
              }
              type="button"
            >
              <X size={14} />
            </button>

            {previewUrl ? (
              <img
                src={previewUrl}
                alt={entry.file.name}
                className="attachment-preview-thumbnail"
              />
            ) : (
              <div className="attachment-preview-icon">{getFileIcon(entry.file.type)}</div>
            )}

            <div className="attachment-preview-info">
              <span className="attachment-preview-name" title={entry.file.name}>
                {entry.file.name}
              </span>
              <span className="attachment-preview-size">
                {live ? progressText(entry) : formatFileSize(entry.file.size)}
              </span>
            </div>

            {live && (
              // Native <progress>, not a div with role="progressbar". The
              // element carries the semantics itself, and OMITTING `value` is
              // how HTML spells "indeterminate" -- the same statement the
              // hand-rolled version made by omitting aria-valuenow, but one the
              // platform enforces rather than one this component has to
              // remember. Emitting a stale number instead would make a frozen
              // upload read as a live one.
              <progress
                className="attachment-progress-bar"
                max={100}
                value={indeterminate ? undefined : entry.progress}
                aria-valuetext={progressText(entry)}
                aria-label={`Upload progress for ${entry.file.name}`}
              />
            )}

            {entry.status === 'error' && (
              <div className="attachment-error-label">{entry.error || 'Failed'}</div>
            )}

            {entry.status === 'cancelled' && (
              <div className="attachment-cancelled-label">Cancelled</div>
            )}
          </div>
        );
      })}
    </div>
  );
};

export default AttachmentUploadPreview;
