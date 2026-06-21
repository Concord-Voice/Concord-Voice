import React, { useCallback, useId, useRef, useState } from 'react';
import { collect as collectSystemInfo } from '../../services/systemInfoService';
import { formatEntries, getEntries } from '../../services/logBufferService';
import { type FeedbackDiagnostics, type FeedbackSubmission } from './feedbackTypes';
import './BugReportPanel.css';

/**
 * Bug Report panel (#159) — the bug-mode body of the feedback modal.
 *
 * Owns the bug-report form fields (title, description, "include diagnostic
 * logs" toggle) and assembles a {@link FeedbackSubmission} that it hands to
 * the modal's shared `onSubmit` pipe. The panel never sees a network
 * primitive — it only describes what to send. Diagnostics are collected
 * lazily and ONLY when the user opts in, so a bug report carries zero
 * environmental data unless the box is checked.
 *
 * Privacy posture: the "what gets sent" disclosure is rendered as
 * always-visible text (not a hover-only tooltip) and wired via
 * `aria-describedby` — a privacy feature should make the data it transmits
 * obvious, not hide it behind a hover the keyboard / screen-reader user may
 * never discover.
 */

// Field caps per #159 spec (stricter than the control-plane's 200B / 8000B
// guards — the server is defense-in-depth; these are the UX limits).
const TITLE_MAX = 120;
const DESCRIPTION_MAX = 5000;

// Guided-template prompts shown as the textarea placeholder. Placeholder
// (not pre-filled value) so the prompts guide without polluting the GitHub
// issue body with boilerplate the user forgot to delete.
const DESCRIPTION_PLACEHOLDER = [
  'What were you trying to do?',
  '',
  'What happened instead?',
  '',
  'Steps to reproduce (if known)?',
].join('\n');

// Disclosure text from the #159 spec, rendered visibly (not a title-attribute
// hover). MUST enumerate every field `buildDiagnostics` actually transmits —
// "current connection state" is included because `connectionPhase` is sent and
// rendered into the (repo-read-visible) GitHub issue body. An under-inclusive
// disclosure on a privacy feature is a consent-accuracy bug; keep this string
// and `buildDiagnostics` in lockstep.
const DIAGNOSTICS_DISCLOSURE =
  'Includes your anonymous machine ID (first 8 characters), app version, OS, ' +
  'GPU info, display resolution, current connection state, and recent application ' +
  'logs. All personally identifiable information (emails, usernames, IPs, tokens) ' +
  'is automatically stripped. No message content, friend lists, or account details ' +
  'are ever included.';

interface BugReportPanelProps {
  /**
   * Shared submit pipe from {@link FeedbackModal}. Receives the assembled
   * payload; the modal owns the network call and the submit-state surface.
   */
  onSubmit: (payload: FeedbackSubmission) => Promise<void> | void;
  /** True while the modal's submit pipe is in flight — disables the form. */
  isSubmitting: boolean;
}

/**
 * Collect the optional diagnostics bundle. Called only when the user opts in.
 * `collectSystemInfo` is best-effort (each probe degrades to a default rather
 * than throwing); `formatEntries`/`getEntries` cannot throw. Defined at module
 * scope — it closes over nothing.
 */
async function buildDiagnostics(): Promise<FeedbackDiagnostics> {
  const info = await collectSystemInfo();
  return {
    appVersion: info.appVersion,
    platform: info.platform,
    machineIdPrefix: info.machineIdPrefix,
    gpu: info.gpu,
    display: info.display,
    connectionPhase: info.connectionPhase,
    logs: formatEntries(getEntries()),
  };
}

const BugReportPanel: React.FC<BugReportPanelProps> = ({ onSubmit, isSubmitting }) => {
  const [title, setTitle] = useState('');
  const [description, setDescription] = useState('');
  const [includeLogs, setIncludeLogs] = useState(false);

  const titleId = useId();
  const descriptionId = useId();
  const includeLogsId = useId();
  const disclosureId = useId();

  // Synchronous in-flight guard. The parent `isSubmitting` prop only flips
  // AFTER onSubmit runs the modal's setState — but when "Include diagnostic
  // logs" is checked we `await buildDiagnostics()` (two IPC round-trips)
  // BEFORE calling onSubmit, leaving a window where the button is still
  // enabled and a second click / Enter would fire a duplicate POST (and a
  // second GitHub issue). This ref, set before the first await, closes the
  // window independently of when the parent prop updates.
  const submittingRef = useRef(false);

  const titleTrimmedLength = title.trim().length;
  const descriptionTrimmedLength = description.trim().length;
  const titleValid = titleTrimmedLength > 0 && title.length <= TITLE_MAX;
  const descriptionValid = descriptionTrimmedLength > 0 && description.length <= DESCRIPTION_MAX;
  const canSubmit = titleValid && descriptionValid && !isSubmitting;

  const handleSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      if (!titleValid || !descriptionValid || isSubmitting || submittingRef.current) return;
      submittingRef.current = true;
      try {
        const payload: FeedbackSubmission = {
          type: 'bug',
          title: title.trim(),
          description: description.trim(),
        };
        if (includeLogs) {
          try {
            payload.diagnostics = await buildDiagnostics();
          } catch {
            // collect() is best-effort and shouldn't throw; if it somehow does,
            // send the report without diagnostics rather than blocking the user.
          }
        }
        await onSubmit(payload);
      } finally {
        // Reset so an error (panel stays mounted) allows a retry. On success
        // the modal unmounts this panel, discarding the ref.
        submittingRef.current = false;
      }
    },
    [title, description, includeLogs, titleValid, descriptionValid, isSubmitting, onSubmit]
  );

  return (
    <form className="bug-report-panel" onSubmit={handleSubmit} noValidate>
      {/* Title */}
      <div className="bug-report-field">
        <label className="bug-report-label" htmlFor={titleId}>
          Title <span className="bug-report-required">*</span>
        </label>
        <input
          id={titleId}
          type="text"
          className="bug-report-input"
          value={title}
          maxLength={TITLE_MAX}
          placeholder="Short summary of the bug"
          disabled={isSubmitting}
          onChange={(e) => setTitle(e.target.value)}
          required
        />
        <div className="bug-report-counter" aria-hidden="true">
          {title.length}/{TITLE_MAX}
        </div>
      </div>

      {/* Description */}
      <div className="bug-report-field">
        <label className="bug-report-label" htmlFor={descriptionId}>
          Description <span className="bug-report-required">*</span>
        </label>
        <textarea
          id={descriptionId}
          className="bug-report-textarea"
          value={description}
          maxLength={DESCRIPTION_MAX}
          placeholder={DESCRIPTION_PLACEHOLDER}
          rows={8}
          disabled={isSubmitting}
          onChange={(e) => setDescription(e.target.value)}
          required
        />
        <div className="bug-report-counter" aria-hidden="true">
          {description.length}/{DESCRIPTION_MAX}
        </div>
      </div>

      {/* Include diagnostic logs */}
      <div className="bug-report-field bug-report-diagnostics">
        <label className="bug-report-checkbox-label" htmlFor={includeLogsId}>
          <input
            id={includeLogsId}
            type="checkbox"
            checked={includeLogs}
            disabled={isSubmitting}
            aria-describedby={disclosureId}
            onChange={(e) => setIncludeLogs(e.target.checked)}
          />
          <span>Include diagnostic logs</span>
        </label>
        <p id={disclosureId} className="bug-report-disclosure">
          {DIAGNOSTICS_DISCLOSURE}
        </p>
      </div>

      {/* Submit */}
      <div className="bug-report-actions">
        <button type="submit" className="bug-report-submit" disabled={!canSubmit}>
          {isSubmitting ? 'Submitting…' : 'Submit Bug Report'}
        </button>
      </div>
    </form>
  );
};

export default BugReportPanel;
