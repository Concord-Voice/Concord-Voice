import React, { useCallback, useRef, useState } from 'react';
import Modal from '../ui/Modal';
import { apiFetch, safeJson } from '../../services/apiClient';
import BugReportPanel from './BugReportPanel';
import FeatureRequestPanel from './FeatureRequestPanel';
import { type FeedbackMode, type FeedbackSubmission } from './feedbackTypes';
import './FeedbackModal.css';

/**
 * Feedback modal entry point (#158).
 *
 * Hosts the mode switcher (Bug Report / Feature Request) and the shared
 * submit pipe to `POST /api/v1/feedback`. Bug mode renders `BugReportPanel`
 * (#159); feature mode renders `FeatureRequestPanel` (#160). The shell,
 * switching, submit wiring, and submit-state surface live here so each panel
 * only owns its form fields and hands an assembled payload to `submit`.
 */

interface FeedbackModalProps {
  isOpen: boolean;
  onClose: () => void;
  /** Optional initial mode. Defaults to 'bug'. */
  initialMode?: FeedbackMode;
}

type SubmitState =
  | { status: 'idle' }
  | { status: 'submitting' }
  | { status: 'success'; issueUrl?: string; dev: boolean }
  | { status: 'error'; message: string };

// ─── Tab pattern (WAI-ARIA tabs) ────────────────────────────────────────────
const TABS: ReadonlyArray<{ id: FeedbackMode; label: string }> = [
  { id: 'bug', label: 'Bug Report' },
  { id: 'feature', label: 'Feature Request' },
];
const PANEL_ID = 'feedback-tabpanel';
const tabIdFor = (mode: FeedbackMode): string => `feedback-tab-${mode}`;
// Arrow-key → index-delta map for roving-tabindex navigation. Collapses the
// four directional keys into one branch to keep handleTabKeyDown flat.
const ARROW_DELTA: Readonly<Record<string, number>> = {
  ArrowRight: 1,
  ArrowDown: 1,
  ArrowLeft: -1,
  ArrowUp: -1,
};

/**
 * Inner component for the success-state issue URL. Extracted so the URL is
 * captured into a local const at the consumer's site (where TS narrowing
 * works), avoiding the non-null assertion lint pushback at the closure call
 * site.
 */
const SubmitSuccessLink: React.FC<{ url: string }> = ({ url }) => (
  <p>
    Tracked at:{' '}
    <a
      href={url}
      onClick={(e) => {
        e.preventDefault();
        void globalThis.electron?.openExternal?.(url);
      }}
    >
      {url}
    </a>
  </p>
);

const FeedbackModal: React.FC<FeedbackModalProps> = ({ isOpen, onClose, initialMode = 'bug' }) => {
  const [mode, setMode] = useState<FeedbackMode>(initialMode);
  const [submitState, setSubmitState] = useState<SubmitState>({ status: 'idle' });
  const tabsRef = useRef<Partial<Record<FeedbackMode, HTMLButtonElement | null>>>({});

  /**
   * Shared submit handler. The panels (in #159 / #160) call this with their
   * assembled `FeedbackSubmission`. Centralizing the network call here means
   * the panels never see a fetch primitive — they only describe what they
   * want to send.
   */
  const submit = useCallback(async (payload: FeedbackSubmission) => {
    setSubmitState({ status: 'submitting' });
    try {
      const res = await apiFetch('/api/v1/feedback', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (res.status === 429) {
        setSubmitState({
          status: 'error',
          message: 'Rate limit reached. Please wait an hour before submitting again.',
        });
        return;
      }
      if (!res.ok) {
        const body = await res.text().catch(() => '');
        const detail = body ? `: ${body.slice(0, 200)}` : '';
        setSubmitState({
          status: 'error',
          message: `Submission failed (HTTP ${res.status})${detail}`,
        });
        return;
      }
      const json = await safeJson<{ issueUrl?: string; dev?: boolean }>(res);
      setSubmitState({
        status: 'success',
        issueUrl: json.issueUrl,
        dev: json.dev === true,
      });
    } catch (err) {
      setSubmitState({
        status: 'error',
        message: err instanceof Error ? err.message : 'Network error while submitting feedback.',
      });
    }
  }, []);

  /**
   * Reset and close. Called from the modal's close button and after the user
   * acknowledges a success/error message.
   */
  const handleClose = useCallback(() => {
    setSubmitState({ status: 'idle' });
    onClose();
  }, [onClose]);

  /**
   * Switch mode. Resets the submit-state surface so the destination tab shows
   * a fresh form — otherwise a post-success (or post-error) surface would keep
   * both panel bodies hidden and the tabs would read as a dead no-op. An
   * in-flight submit is preserved (the pipe resolves and overwrites it).
   */
  const handleModeChange = useCallback((next: FeedbackMode) => {
    setMode(next);
    setSubmitState((s) => (s.status === 'submitting' ? s : { status: 'idle' }));
  }, []);

  /**
   * Roving-tabindex keyboard navigation for the tablist (WAI-ARIA tabs).
   * Left/Up and Right/Down move between tabs (wrapping); Home/End jump to the
   * ends. Selection follows focus (automatic activation) since the panels are
   * cheap to render.
   */
  const handleTabKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLButtonElement>) => {
      const idx = TABS.findIndex((t) => t.id === mode);
      let nextIdx: number | null = null;
      if (e.key in ARROW_DELTA) {
        nextIdx = (idx + ARROW_DELTA[e.key] + TABS.length) % TABS.length;
      } else if (e.key === 'Home') {
        nextIdx = 0;
      } else if (e.key === 'End') {
        nextIdx = TABS.length - 1;
      }
      if (nextIdx === null) return;
      e.preventDefault();
      const next = TABS[nextIdx].id;
      handleModeChange(next);
      tabsRef.current[next]?.focus();
    },
    [mode, handleModeChange]
  );

  return (
    <Modal isOpen={isOpen} onClose={handleClose} title="Send Feedback" width="xlarge">
      <div className="feedback-modal">
        {/* Mode switcher — WAI-ARIA tabs with roving tabindex */}
        <div className="feedback-mode-switch" role="tablist" aria-label="Feedback type">
          {TABS.map((t) => (
            <button
              key={t.id}
              ref={(el) => {
                tabsRef.current[t.id] = el;
              }}
              id={tabIdFor(t.id)}
              type="button"
              role="tab"
              aria-selected={mode === t.id}
              aria-controls={PANEL_ID}
              tabIndex={mode === t.id ? 0 : -1}
              className={`feedback-mode-tab ${mode === t.id ? 'active' : ''}`}
              onClick={() => handleModeChange(t.id)}
              onKeyDown={handleTabKeyDown}
            >
              {t.label}
            </button>
          ))}
        </div>

        {/* Panel body */}
        <div
          className="feedback-panel-body"
          role="tabpanel"
          id={PANEL_ID}
          aria-labelledby={tabIdFor(mode)}
        >
          {/* Bug mode — real form (#159). Hidden on success so the "thank
              you" surface reads cleanly; on error the form stays mounted so
              the user can retry without re-typing. */}
          {mode === 'bug' && submitState.status !== 'success' && (
            <BugReportPanel onSubmit={submit} isSubmitting={submitState.status === 'submitting'} />
          )}

          {/* Feature mode — real form (#160). Same success/error visibility
              rules as bug mode: hidden on success, kept mounted on error. */}
          {mode === 'feature' && submitState.status !== 'success' && (
            <FeatureRequestPanel
              onSubmit={submit}
              isSubmitting={submitState.status === 'submitting'}
            />
          )}

          {/* Submit-state surface. Progress + success live in a polite
              region (always-present stable container so the announcement
              fires when content appears); errors use a separate assertive
              alert so they interrupt. Splitting them avoids nesting live
              regions. */}
          <div className="feedback-submit-region" aria-live="polite">
            {submitState.status === 'submitting' && (
              <p className="feedback-submit-status">Submitting…</p>
            )}
            {submitState.status === 'success' && (
              <div className="feedback-submit-success">
                <p>
                  Thank you for the feedback!
                  {submitState.dev && ' (Stubbed in dev — no GitHub issue created.)'}
                </p>
                {submitState.issueUrl && <SubmitSuccessLink url={submitState.issueUrl} />}
              </div>
            )}
          </div>
          {submitState.status === 'error' && (
            <p className="feedback-submit-error" role="alert">
              {submitState.message}
            </p>
          )}
        </div>

        {/* Footer — close + submit (submit visible to keep the contract
            obvious for the panel branches; they'll wire their assembled
            payload into the existing `submit` handler exported below.) */}
        <div className="feedback-modal-footer">
          <button type="button" className="feedback-btn-secondary" onClick={handleClose}>
            Close
          </button>
        </div>
      </div>
    </Modal>
  );
};

export default FeedbackModal;
// Exported for the panel branches (#159 / #160). They import this from the
// same module path the modal does.
export { FeedbackModal };
