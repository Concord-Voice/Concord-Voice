import type { Ref } from 'react';
import type { ExpirationPolicy } from '../../services/messaging/expirationPolicyApi';
import './messageExpiration.css';

export interface MessageExpirationPolicySummaryProps {
  policy: ExpirationPolicy | null;
  policyState: 'loading' | 'ready' | 'unavailable';
  showChangedNotice: boolean;
  onReview: () => void;
  onDismissNotice: () => void;
  onManageMessages?: () => void;
  manageMessagesUnavailableDescription?: string;
  ref?: Ref<HTMLElement>;
}

const labels: Record<NonNullable<ExpirationPolicy['windowSeconds']>, string> = {
  3600: '1 hour',
  86400: '24 hours',
  604800: '7 days',
  2592000: '30 days',
};

function expirationPolicyText(
  policy: ExpirationPolicy | null,
  policyState: MessageExpirationPolicySummaryProps['policyState']
): string {
  if (policyState === 'loading') return 'Loading message expiration…';
  if (policyState === 'unavailable' || !policy) return 'Message expiration unavailable';
  if (policy.windowSeconds === null) return 'Message expiration: Off';
  return `Messages expire after ${labels[policy.windowSeconds]}`;
}

export default function MessageExpirationPolicySummary({
  policy,
  policyState,
  showChangedNotice,
  onReview,
  onDismissNotice,
  onManageMessages,
  manageMessagesUnavailableDescription,
  ref,
}: Readonly<MessageExpirationPolicySummaryProps>) {
  const policyText = expirationPolicyText(policy, policyState);
  return (
    <section
      ref={ref}
      className="message-expiration-summary"
      aria-label="Message expiration"
      tabIndex={-1}
    >
      <p>{policyText}</p>
      <p>Shared with everyone in this conversation.</p>
      {policyState === 'ready' && policy?.backfillPending && (
        <p>Still processing existing messages.</p>
      )}
      {showChangedNotice && (
        <div className="message-expiration-notice">
          <span>Message expiration settings changed in this conversation.</span>
          <button type="button" className="message-expiration-notice-action" onClick={onReview}>
            Review policy
          </button>
          <button
            type="button"
            className="message-expiration-notice-action"
            onClick={onDismissNotice}
          >
            Dismiss
          </button>
          {onManageMessages && (
            <button
              type="button"
              className="message-expiration-notice-action"
              onClick={onManageMessages}
            >
              Manage messages
            </button>
          )}
        </div>
      )}
      {!onManageMessages && manageMessagesUnavailableDescription && (
        <p>{manageMessagesUnavailableDescription}</p>
      )}
      {!showChangedNotice && onManageMessages && (
        <button
          type="button"
          className="message-expiration-notice-action"
          onClick={onManageMessages}
        >
          Manage messages
        </button>
      )}
    </section>
  );
}
