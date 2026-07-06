import React from 'react';
import Modal from '../ui/Modal';
import { type FeedbackDiagnostics } from './feedbackTypes';
import './DiagnosticsPreviewModal.css';

/**
 * Read-only preview of the EXACT diagnostics bundle a bug report would carry
 * (#2078). Purely presentational — it renders a `FeedbackDiagnostics` bundle
 * the container assembled by calling the SAME `buildDiagnostics()` the submit
 * path uses, so the preview can never drift from what is actually sent. Owns
 * no state, no data-collection, and no IPC/network surface.
 */

const CONSENT_HEADER =
  'This is what would be sent if you submit now — a point-in-time snapshot; more log ' +
  'lines may be added before you actually submit. Identifying details ' +
  '(emails, usernames, IPs, tokens, file paths) are already removed; IDs are shown as ' +
  '<id:N> placeholders.';

export interface DiagnosticsPreviewModalProps {
  isOpen: boolean;
  onClose: () => void;
  diagnostics: FeedbackDiagnostics | null;
  loading?: boolean;
  error?: boolean;
}

function formatGpu(gpu: FeedbackDiagnostics['gpu']): string {
  return gpu ? `${gpu.vendor} / ${gpu.renderer}` : '(unavailable)';
}

function formatDisplay(d: FeedbackDiagnostics['display']): string {
  if (!d) return '(unavailable)';
  const hz = d.refreshRate ? ` (${d.refreshRate}Hz)` : '';
  return `${d.width}×${d.height} @ ${d.scaleFactor}x${hz}`;
}

const DiagnosticsPreviewModal: React.FC<DiagnosticsPreviewModalProps> = ({
  isOpen,
  onClose,
  diagnostics,
  loading = false,
  error = false,
}) => {
  return (
    <Modal isOpen={isOpen} onClose={onClose} title="What's in the logs?" width="large">
      <p className="diagnostics-preview-consent">{CONSENT_HEADER}</p>
      {loading && <p className="diagnostics-preview-status">Collecting diagnostics…</p>}
      {error && (
        <p className="diagnostics-preview-status">
          Couldn&apos;t collect diagnostics to preview. You can still submit the report.
        </p>
      )}
      {!loading && !error && diagnostics && (
        <>
          <dl className="diagnostics-preview-fields">
            <dt>App version</dt>
            <dd>{diagnostics.appVersion}</dd>
            <dt>Platform</dt>
            <dd>{diagnostics.platform}</dd>
            <dt>Machine ID prefix</dt>
            <dd>{diagnostics.machineIdPrefix}</dd>
            <dt>GPU</dt>
            <dd>{formatGpu(diagnostics.gpu)}</dd>
            <dt>Display</dt>
            <dd>{formatDisplay(diagnostics.display)}</dd>
            <dt>Connection phase</dt>
            <dd>{diagnostics.connectionPhase}</dd>
          </dl>
          <h4 className="diagnostics-preview-logs-heading">Recent logs</h4>
          <pre className="diagnostics-preview-logs">
            {diagnostics.logs.trim().length > 0 ? diagnostics.logs : '(no logs captured yet)'}
          </pre>
        </>
      )}
    </Modal>
  );
};

export default DiagnosticsPreviewModal;
