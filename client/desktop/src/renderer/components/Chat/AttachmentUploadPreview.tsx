import React from 'react';
import { X, FileText, Film, Music, File } from 'lucide-react';
import type { FileUploadState } from '../../hooks/useFileUpload';
import { classifyFileType, formatFileSize } from '../../utils/attachmentCrypto';
import './AttachmentUploadPreview.css';

interface AttachmentUploadPreviewProps {
  files: FileUploadState[];
  onRemove: (index: number) => void;
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

const AttachmentUploadPreview: React.FC<AttachmentUploadPreviewProps> = ({ files, onRemove }) => {
  if (files.length === 0) return null;

  return (
    <div className="attachment-upload-preview">
      {files.map((entry, index) => {
        const previewUrl = entry.previewUrl;
        return (
          <div
            // eslint-disable-next-line @eslint-react/no-array-index-key -- FileUploadState has no stable unique id; list only grows/shrinks from the tail, never reorders, so index is safe here
            key={`${entry.file.name}-${index}`}
            className={`attachment-preview-item ${entry.status}`}
          >
            <button
              className="attachment-remove-btn"
              onClick={() => onRemove(index)}
              aria-label={`Remove ${entry.file.name}`}
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
              <span className="attachment-preview-size">{formatFileSize(entry.file.size)}</span>
            </div>

            {entry.status === 'uploading' && (
              <div className="attachment-progress-bar">
                <div className="attachment-progress-fill" style={{ width: `${entry.progress}%` }} />
              </div>
            )}

            {entry.status === 'error' && (
              <div className="attachment-error-label">{entry.error || 'Failed'}</div>
            )}
          </div>
        );
      })}
    </div>
  );
};

export default AttachmentUploadPreview;
