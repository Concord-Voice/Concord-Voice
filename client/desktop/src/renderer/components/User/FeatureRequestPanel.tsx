import React, { useCallback, useId, useState } from 'react';
import { type FeedbackSubmission } from './feedbackTypes';
import './FeatureRequestPanel.css';

/**
 * Feature Request panel (#160) — the feature-mode body of the feedback modal.
 *
 * Owns the feature-request form fields (title, "what would you like?", optional
 * category) and assembles a {@link FeedbackSubmission} that it hands to the
 * modal's shared `onSubmit` pipe. The panel never sees a network primitive — it
 * only describes what to send.
 *
 * Deliberately lightweight: feature requests collect NO system diagnostics or
 * logs (unlike {@link BugReportPanel}) — the user is asking for something new,
 * so environment data is noise. This also keeps the panel free of any
 * IPC / service dependency.
 */

// Field caps per #160 spec (stricter than the control-plane's 200B / 8000B
// guards — the server is defense-in-depth; these are the UX limits, matching
// the bug panel).
const TITLE_MAX = 120;
const DESCRIPTION_MAX = 5000;

// Guided-template prompts shown as the textarea placeholder. Placeholder (not
// pre-filled value) so the prompts guide without polluting the GitHub issue
// body with boilerplate the user forgot to delete.
const DESCRIPTION_PLACEHOLDER = [
  "Describe the feature, fix, or change you'd like to see.",
  '',
  'Why is this important to you?',
  '',
  'How would this improve your experience?',
].join('\n');

// Optional category options (#160 spec). The submitted value is the
// human-readable label — the control-plane renders `**Category:** <value>`
// verbatim into the issue body (matching the issue's example output). An empty
// selection omits the category field entirely.
const CATEGORY_OPTIONS = [
  'New Feature',
  'Improvement to Existing Feature',
  'UI/UX Change',
  'Performance',
  'Other',
] as const;

interface FeatureRequestPanelProps {
  /**
   * Shared submit pipe from {@link FeedbackModal}. Receives the assembled
   * payload; the modal owns the network call and the submit-state surface.
   */
  onSubmit: (payload: FeedbackSubmission) => Promise<void> | void;
  /** True while the modal's submit pipe is in flight — disables the form. */
  isSubmitting: boolean;
}

const FeatureRequestPanel: React.FC<FeatureRequestPanelProps> = ({ onSubmit, isSubmitting }) => {
  const [title, setTitle] = useState('');
  const [description, setDescription] = useState('');
  const [category, setCategory] = useState('');

  const titleId = useId();
  const descriptionId = useId();
  const categoryId = useId();

  const titleValid = title.trim().length > 0 && title.length <= TITLE_MAX;
  const descriptionValid = description.trim().length > 0 && description.length <= DESCRIPTION_MAX;
  const canSubmit = titleValid && descriptionValid && !isSubmitting;

  const handleSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      // No synchronous double-submit ref is needed here (unlike BugReportPanel):
      // this panel has no `await` before `onSubmit`, so the parent's
      // synchronous setState('submitting') re-renders the disabled button
      // before a second event can dispatch — the same safe shape as the bug
      // panel's unchecked-logs path.
      if (!titleValid || !descriptionValid || isSubmitting) return;
      const payload: FeedbackSubmission = {
        type: 'feature',
        title: title.trim(),
        description: description.trim(),
      };
      if (category) payload.category = category;
      await onSubmit(payload);
    },
    [title, description, category, titleValid, descriptionValid, isSubmitting, onSubmit]
  );

  return (
    <form className="feature-request-panel" onSubmit={handleSubmit} noValidate>
      {/* Title */}
      <div className="feature-request-field">
        <label className="feature-request-label" htmlFor={titleId}>
          Title <span className="feature-request-required">*</span>
        </label>
        <input
          id={titleId}
          type="text"
          className="feature-request-input"
          value={title}
          maxLength={TITLE_MAX}
          placeholder="Short summary of the request"
          disabled={isSubmitting}
          onChange={(e) => setTitle(e.target.value)}
          required
        />
        <div className="feature-request-counter" aria-hidden="true">
          {title.length}/{TITLE_MAX}
        </div>
      </div>

      {/* What would you like? (description) */}
      <div className="feature-request-field">
        <label className="feature-request-label" htmlFor={descriptionId}>
          What would you like? <span className="feature-request-required">*</span>
        </label>
        <textarea
          id={descriptionId}
          className="feature-request-textarea"
          value={description}
          maxLength={DESCRIPTION_MAX}
          placeholder={DESCRIPTION_PLACEHOLDER}
          rows={8}
          disabled={isSubmitting}
          onChange={(e) => setDescription(e.target.value)}
          required
        />
        <div className="feature-request-counter" aria-hidden="true">
          {description.length}/{DESCRIPTION_MAX}
        </div>
      </div>

      {/* Category (optional) */}
      <div className="feature-request-field">
        <label className="feature-request-label" htmlFor={categoryId}>
          Category <span className="feature-request-optional">(optional)</span>
        </label>
        <select
          id={categoryId}
          className="feature-request-select"
          value={category}
          disabled={isSubmitting}
          onChange={(e) => setCategory(e.target.value)}
        >
          <option value="">— Select a category —</option>
          {CATEGORY_OPTIONS.map((opt) => (
            <option key={opt} value={opt}>
              {opt}
            </option>
          ))}
        </select>
      </div>

      {/* Submit */}
      <div className="feature-request-actions">
        <button type="submit" className="feature-request-submit" disabled={!canSubmit}>
          {isSubmitting ? 'Submitting…' : 'Submit Feature Request'}
        </button>
      </div>
    </form>
  );
};

export default FeatureRequestPanel;
