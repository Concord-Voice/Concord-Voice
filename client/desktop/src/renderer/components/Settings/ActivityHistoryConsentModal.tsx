import React, { useEffect, useId, useRef, useState } from 'react';
import SafeLink from '../Markdown/SafeLink';
import Modal from '../ui/Modal';
import {
  ACTIVITY_HISTORY_RETENTION_DAYS,
  type ActivityHistoryRetentionDays,
  type PresenceHistoryRequiredConsent,
} from '../../services/presenceHistoryService';
import './ActivityHistory.css';

export interface ActivityHistoryConsentSelection {
  retentionDays: ActivityHistoryRetentionDays;
  consentVersion: number;
  consentCopyHash: string;
}

export interface ActivityHistoryConsentModalProps {
  isOpen: boolean;
  mode: 'enable' | 'reconsent';
  disclosure: PresenceHistoryRequiredConsent;
  retentionDays: ActivityHistoryRetentionDays;
  onClose: () => void;
  onSubmit: (selection: ActivityHistoryConsentSelection) => Promise<void>;
}

interface IdentityBoundAcknowledgement {
  disclosureIdentity: string;
  checked: boolean;
}

interface IdentityBoundRetention {
  disclosureIdentity: string;
  value: ActivityHistoryRetentionDays;
}

interface IdentityBoundSubmission {
  disclosureIdentity: string;
  error: string | null;
  submitting: boolean;
}

function parseRetentionDays(value: string): ActivityHistoryRetentionDays | null {
  switch (value) {
    case '7':
      return 7;
    case '30':
      return 30;
    case '90':
      return 90;
    case '365':
      return 365;
    default:
      return null;
  }
}

function modalTitle(mode: ActivityHistoryConsentModalProps['mode']): string {
  return mode === 'reconsent' ? 'Review updated Activity History terms' : 'Enable Activity History';
}

function confirmLabel(mode: ActivityHistoryConsentModalProps['mode'], submitting: boolean): string {
  if (submitting) {
    return mode === 'reconsent' ? 'Accepting updated terms…' : 'Enabling Activity History…';
  }
  return mode === 'reconsent' ? 'Accept updated terms' : 'Enable Activity History';
}

function handleSettledPromise(promise: Promise<unknown>): void {
  promise.catch(() => undefined);
}

const ActivityHistoryConsentModal: React.FC<ActivityHistoryConsentModalProps> = ({
  isOpen,
  mode,
  disclosure,
  retentionDays,
  onClose,
  onSubmit,
}) => {
  const headingRef = useRef<HTMLHeadingElement>(null);
  const previousDisclosureIdentityRef = useRef<string | null>(null);
  const currentDisclosureIdentityRef = useRef('');
  const retentionId = useId();
  const acknowledgementId = useId();
  const disclosureIdentity = `${disclosure.version}:${disclosure.copyHash}`;
  currentDisclosureIdentityRef.current = disclosureIdentity;

  const [acknowledgement, setAcknowledgement] = useState<IdentityBoundAcknowledgement>({
    disclosureIdentity,
    checked: false,
  });
  const [retention, setRetention] = useState<IdentityBoundRetention>({
    disclosureIdentity,
    value: retentionDays,
  });
  const [submission, setSubmission] = useState<IdentityBoundSubmission>({
    disclosureIdentity,
    error: null,
    submitting: false,
  });

  const acknowledged =
    acknowledgement.disclosureIdentity === disclosureIdentity && acknowledgement.checked;
  const selectedRetention =
    retention.disclosureIdentity === disclosureIdentity ? retention.value : retentionDays;
  const currentSubmission =
    submission.disclosureIdentity === disclosureIdentity
      ? submission
      : { disclosureIdentity, error: null, submitting: false };

  useEffect(() => {
    const previousIdentity = previousDisclosureIdentityRef.current;
    previousDisclosureIdentityRef.current = disclosureIdentity;
    if (isOpen && previousIdentity !== null && previousIdentity !== disclosureIdentity) {
      headingRef.current?.focus();
    }
  }, [disclosureIdentity, isOpen]);

  const resetCurrentDisclosureState = (): void => {
    setAcknowledgement({ disclosureIdentity, checked: false });
    setRetention({ disclosureIdentity, value: retentionDays });
    setSubmission({ disclosureIdentity, error: null, submitting: false });
  };

  const handleClose = (): void => {
    if (currentSubmission.submitting) return;
    resetCurrentDisclosureState();
    onClose();
  };

  const handleSubmit = async (): Promise<void> => {
    if (!acknowledged || currentSubmission.submitting) return;
    const submittedIdentity = disclosureIdentity;
    setSubmission({ disclosureIdentity: submittedIdentity, error: null, submitting: true });

    try {
      await onSubmit({
        retentionDays: selectedRetention,
        consentVersion: disclosure.version,
        consentCopyHash: disclosure.copyHash,
      });
    } catch {
      if (currentDisclosureIdentityRef.current !== submittedIdentity) return;
      setSubmission({
        disclosureIdentity: submittedIdentity,
        error: 'Activity History settings could not be saved. Try again.',
        submitting: false,
      });
      return;
    }

    if (currentDisclosureIdentityRef.current === submittedIdentity) {
      setSubmission({ disclosureIdentity: submittedIdentity, error: null, submitting: false });
    }
  };

  return (
    <Modal
      isOpen={isOpen}
      onClose={handleClose}
      title={modalTitle(mode)}
      width="large"
      dismissable={!currentSubmission.submitting}
      initialFocusRef={headingRef}
    >
      <div className="activity-history-consent">
        <section
          className="activity-history-consent__disclosure"
          aria-labelledby="activity-history-disclosure-heading"
        >
          <h4
            id="activity-history-disclosure-heading"
            ref={headingRef}
            tabIndex={-1}
            className="activity-history-consent__heading"
          >
            {disclosure.requiredText}
          </h4>
          <p className="activity-history-consent__operator">
            <span>Instance operator</span>
            <strong>{disclosure.operatorName}</strong>
          </p>
          <ul className="activity-history-consent__details">
            {disclosure.details.map((detail) => (
              <li key={detail}>{detail}</li>
            ))}
          </ul>
          <SafeLink href={disclosure.privacyPolicyUrl}>
            {disclosure.operatorName} privacy policy
          </SafeLink>
        </section>

        <div className="activity-history-consent__retention">
          <label htmlFor={retentionId}>Keep Activity History for</label>
          <select
            id={retentionId}
            value={selectedRetention}
            disabled={currentSubmission.submitting}
            onChange={(event) => {
              const nextRetention = parseRetentionDays(event.currentTarget.value);
              if (nextRetention === null) return;
              setRetention({ disclosureIdentity, value: nextRetention });
            }}
          >
            {ACTIVITY_HISTORY_RETENTION_DAYS.map((days) => (
              <option key={days} value={days}>
                {days} days
              </option>
            ))}
          </select>
        </div>

        <div className="activity-history-consent__acknowledgement">
          <input
            id={acknowledgementId}
            type="checkbox"
            checked={acknowledged}
            disabled={currentSubmission.submitting}
            onChange={(event) => {
              setAcknowledgement({
                disclosureIdentity,
                checked: event.currentTarget.checked,
              });
            }}
          />
          <label htmlFor={acknowledgementId}>{disclosure.acknowledgementLabel}</label>
        </div>

        {currentSubmission.error !== null && (
          <div
            className="activity-history-consent__error"
            role="alert"
            aria-label={currentSubmission.error}
          >
            {currentSubmission.error}
          </div>
        )}

        <div className="activity-history-consent__actions">
          <button
            type="button"
            className="secondary-button"
            disabled={currentSubmission.submitting}
            onClick={handleClose}
          >
            Cancel
          </button>
          <button
            type="button"
            className="primary-button"
            disabled={!acknowledged || currentSubmission.submitting}
            onClick={() => handleSettledPromise(handleSubmit())}
          >
            {confirmLabel(mode, currentSubmission.submitting)}
          </button>
        </div>
      </div>
    </Modal>
  );
};

export default ActivityHistoryConsentModal;
