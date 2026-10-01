import React, { useCallback, useEffect, useId, useRef, useState } from 'react';
import { ImagePlus, Loader2, X } from 'lucide-react';
import { collect as collectSystemInfo } from '../../services/system/systemInfoService';
import { formatEntries, getEntries } from '../../services/system/logBufferService';
import { pseudonymizeLogUuids } from '../../utils/runtime/pseudonymizeLogUuids';
import { type FeedbackDiagnostics, type FeedbackSubmission } from './feedbackTypes';
import { useFeedbackScreenshot } from '../../hooks/feedback/useFeedbackScreenshot';
import DiagnosticsPreviewModal from './DiagnosticsPreviewModal';
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
 *
 * Screenshots (#1747): opt-in image attachments upload to the Tier-1 media
 * endpoint, which re-encodes them (stripping EXIF/GPS metadata) and returns an
 * unguessable path. The control-plane embeds that path as a server-authored
 * image link in the filed (public-ish) feedback issue — so an attached
 * screenshot becomes as visible as the report itself. The picker hint states
 * the metadata-stripping and the per-file / type / count caps.
 */

// Field caps per #159 spec (stricter than the control-plane's 200B / 8000B
// guards — the server is defense-in-depth; these are the UX limits).
const TITLE_MAX = 120;
const DESCRIPTION_MAX = 5000;

// Screenshot attachment caps (#1747) — mirrored server-side as defense-in-depth.
const MAX_SCREENSHOTS = 4;
const MAX_SCREENSHOT_BYTES = 5 * 1024 * 1024; // 5 MB
const ALLOWED_SCREENSHOT_TYPES = ['image/png', 'image/jpeg', 'image/webp'];
const SCREENSHOT_ACCEPT = ALLOWED_SCREENSHOT_TYPES.join(',');

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
  'is automatically stripped. Identifiers (channel, message, and account IDs) are ' +
  'replaced with per-report placeholders like <id:1> — consistent within this ' +
  'report, meaningless outside it. No message content, friend lists, or account ' +
  'details are ever included.';

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
    // Submit-time UUID pseudonymization: replace every UUID with a per-report
    // ordinal (`<id:1>`, `<id:2>`, …) — consistent within THIS report (triage
    // correlation) but fresh per report (no cross-report linkability). Kept in
    // lockstep with the DIAGNOSTICS_DISCLOSURE copy below. Capture-time secret
    // scrubbing in logBufferService is unchanged; this is an additional pass.
    logs: pseudonymizeLogUuids(formatEntries(getEntries())),
  };
}

const BugReportPanel: React.FC<BugReportPanelProps> = ({ onSubmit, isSubmitting }) => {
  const [title, setTitle] = useState('');
  const [description, setDescription] = useState('');
  const [includeLogs, setIncludeLogs] = useState(false);
  const [attachError, setAttachError] = useState<string | null>(null);
  const screenshotInputRef = useRef<HTMLInputElement>(null);
  const { screenshots, addFiles, remove, uploadedRefs, isUploading, atCapacity } =
    useFeedbackScreenshot({
      maxCount: MAX_SCREENSHOTS,
      maxSize: MAX_SCREENSHOT_BYTES,
      allowedTypes: ALLOWED_SCREENSHOT_TYPES,
      onError: setAttachError,
    });

  // Focus management for screenshot removal (a11y). Removing a thumbnail
  // deletes its `<li>` — and the focused remove button along with it — so the
  // browser drops focus to `document.body` with no keyboard/SR signal of
  // where it went. `removeButtonRef` mirrors the rendered list by index;
  // `pendingRemoveFocusIndexRef` records which index the user just removed so
  // the effect below (which runs once the shorter list has committed) can
  // land focus on whichever remove button now occupies that slot, fall back
  // to the previous slot, or — once the list is empty — the attach button.
  const removeButtonRef = useRef<(HTMLButtonElement | null)[]>([]);
  const attachButtonRef = useRef<HTMLButtonElement>(null);
  const pendingRemoveFocusIndexRef = useRef<number | null>(null);
  const prevScreenshotCountRef = useRef(screenshots.length);

  useEffect(() => {
    const prevCount = prevScreenshotCountRef.current;
    prevScreenshotCountRef.current = screenshots.length;
    const pendingIndex = pendingRemoveFocusIndexRef.current;
    if (pendingIndex === null || screenshots.length >= prevCount) return;
    pendingRemoveFocusIndexRef.current = null;
    if (screenshots.length === 0) {
      attachButtonRef.current?.focus();
      return;
    }
    const targetIndex = Math.min(pendingIndex, screenshots.length - 1);
    removeButtonRef.current[targetIndex]?.focus();
  }, [screenshots.length]);

  // "What's in the logs?" preview (#2078). The preview renders the EXACT bundle
  // `buildDiagnostics()` would submit — one code path, so the preview cannot
  // drift from the payload. Preview state is fully independent of the submit
  // form: opening/closing it never touches `includeLogs` or the submit path.
  const [previewOpen, setPreviewOpen] = useState(false);
  const [previewData, setPreviewData] = useState<FeedbackDiagnostics | null>(null);
  const [previewLoading, setPreviewLoading] = useState(false);
  const [previewError, setPreviewError] = useState(false);

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
  const canSubmit = titleValid && descriptionValid && !isSubmitting && !isUploading;

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
        const refs = uploadedRefs();
        if (refs.length > 0) {
          payload.attachments = refs;
        }
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
    [
      title,
      description,
      includeLogs,
      titleValid,
      descriptionValid,
      isSubmitting,
      onSubmit,
      uploadedRefs,
    ]
  );

  const handleOpenPreview = useCallback(async () => {
    setPreviewOpen(true);
    setPreviewLoading(true);
    setPreviewError(false);
    setPreviewData(null);
    try {
      // The SAME function the submit path calls — the preview cannot drift
      // from what is actually sent.
      const d = await buildDiagnostics();
      setPreviewData(d);
    } catch {
      setPreviewError(true);
    } finally {
      setPreviewLoading(false);
    }
  }, []);

  return (
    <>
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
          <button type="button" className="bug-report-preview-link" onClick={handleOpenPreview}>
            What&apos;s in the logs?
          </button>
        </div>

        {/* Screenshots (#1747) */}
        <div className="bug-report-field">
          <span className="bug-report-label">Screenshots (optional)</span>
          <input
            ref={screenshotInputRef}
            type="file"
            accept={SCREENSHOT_ACCEPT}
            multiple
            hidden
            disabled={isSubmitting || atCapacity}
            onChange={(e) => {
              if (e.target.files) addFiles(e.target.files);
              e.target.value = ''; // allow re-selecting the same file after removal
            }}
          />
          {screenshots.length > 0 && (
            <ul className="bug-report-screenshots" aria-label="Attached screenshots">
              {screenshots.map((s, index) => (
                <li key={s.id} className="bug-report-screenshot">
                  <img src={s.previewUrl} alt="" className="bug-report-screenshot-thumb" />
                  {s.status === 'uploading' && (
                    <span className="bug-report-screenshot-overlay" aria-hidden="true">
                      <Loader2 size={16} className="spinner" />
                    </span>
                  )}
                  {s.status === 'error' && (
                    <span
                      role="img"
                      className="bug-report-screenshot-overlay bug-report-screenshot-overlay--error"
                      aria-label="Upload failed"
                    >
                      !
                    </span>
                  )}
                  <button
                    type="button"
                    ref={(el) => {
                      removeButtonRef.current[index] = el;
                    }}
                    className="bug-report-screenshot-remove"
                    aria-label={`Remove screenshot ${index + 1}`}
                    disabled={isSubmitting}
                    onClick={() => {
                      setAttachError(null);
                      pendingRemoveFocusIndexRef.current = index;
                      remove(s.id);
                    }}
                  >
                    <X size={14} />
                  </button>
                </li>
              ))}
            </ul>
          )}
          <button
            type="button"
            ref={attachButtonRef}
            className="bug-report-attach-btn"
            disabled={isSubmitting || atCapacity}
            onClick={() => {
              setAttachError(null);
              screenshotInputRef.current?.click();
            }}
          >
            <ImagePlus size={16} />
            {atCapacity ? `Maximum ${MAX_SCREENSHOTS} screenshots` : 'Attach screenshots'}
          </button>
          <p className="bug-report-screenshot-hint">
            PNG, JPEG, or WebP — up to {MAX_SCREENSHOTS}, 5&nbsp;MB each. Image metadata is stripped
            on upload.
          </p>
          {attachError && (
            <p className="bug-report-screenshot-error" role="alert">
              {attachError}
            </p>
          )}
        </div>

        {/* Submit */}
        <div className="bug-report-actions">
          <button type="submit" className="bug-report-submit" disabled={!canSubmit}>
            {isSubmitting ? 'Submitting…' : 'Submit Bug Report'}
          </button>
        </div>
      </form>

      {/*
       * The preview modal MUST be a SIBLING of <form>, not a descendant. The
       * shared `ui/Modal` renders inline (no portal) and its close (X) button has
       * no explicit `type`, so inside a <form> it defaults to `type="submit"` and
       * clicking X would submit the bug report (Gitar HIGH, #2086 review). Kept
       * outside the form, the X can only close the modal.
       */}
      <DiagnosticsPreviewModal
        isOpen={previewOpen}
        onClose={() => setPreviewOpen(false)}
        diagnostics={previewData}
        loading={previewLoading}
        error={previewError}
      />
    </>
  );
};

export default BugReportPanel;
