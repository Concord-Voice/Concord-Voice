import { createRef } from 'react';
import { render, screen } from '../../../test-utils';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import MessageExpirationPolicySummary from '@/renderer/components/Expiration/MessageExpirationPolicySummary';

const policy = {
  windowSeconds: 86400 as const,
  updatedAt: '2026-09-08T05:00:00Z',
  revision: 4,
  backfillPending: false,
};
const base = () => ({
  policy,
  policyState: 'ready' as const,
  showChangedNotice: false,
  onReview: vi.fn(),
  onDismissNotice: vi.fn(),
});

beforeEach(() => resetAllStores());

describe('MessageExpirationPolicySummary', () => {
  it('shows the valid policy and shared scope', () => {
    render(<MessageExpirationPolicySummary {...base()} />);
    expect(screen.getByText('Messages expire after 24 hours')).toBeInTheDocument();
    expect(screen.getByText('Shared with everyone in this conversation.')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Manage messages' })).not.toBeInTheDocument();
  });

  it.each([
    ['loading', null, 'Loading message expiration…'],
    ['unavailable', null, 'Message expiration unavailable'],
    ['ready', { ...policy, windowSeconds: null }, 'Message expiration: Off'],
  ] as const)('renders %s accurately without inventing Off', (policyState, current, text) => {
    render(
      <MessageExpirationPolicySummary {...base()} policyState={policyState} policy={current} />
    );
    expect(screen.getByText(text)).toBeInTheDocument();
  });

  it('focuses the named summary section directly', () => {
    const ref = createRef<HTMLElement>();
    render(<MessageExpirationPolicySummary {...base()} ref={ref} />);
    const summary = screen.getByRole('region', { name: 'Message expiration' });
    expect(ref.current).toBe(summary);
    summary.focus();
    expect(document.activeElement).toBe(summary);
  });

  it('shows review and dismissal only for a changed notice', async () => {
    const user = userEvent.setup();
    const props = { ...base(), showChangedNotice: true };
    render(<MessageExpirationPolicySummary {...props} />);
    expect(
      screen.getByText('Message expiration settings changed in this conversation.')
    ).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Review policy' }));
    await user.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(props.onReview).toHaveBeenCalledOnce();
    expect(props.onDismissNotice).toHaveBeenCalledOnce();
  });

  it('conditionally exposes Manage messages and uses supplied permission explanation', async () => {
    const onManageMessages = vi.fn();
    const userExplanation = 'You do not have permission to manage messages.';
    const { rerender } = render(
      <MessageExpirationPolicySummary {...base()} onManageMessages={onManageMessages} />
    );
    await userEvent.setup().click(screen.getByRole('button', { name: 'Manage messages' }));
    expect(onManageMessages).toHaveBeenCalledOnce();
    rerender(
      <MessageExpirationPolicySummary
        {...base()}
        manageMessagesUnavailableDescription={userExplanation}
      />
    );
    expect(screen.queryByRole('button', { name: 'Manage messages' })).not.toBeInTheDocument();
    expect(screen.getByText(userExplanation)).toBeInTheDocument();
  });

  it('shows pending status and offers review/manage without inventing authority', () => {
    render(
      <MessageExpirationPolicySummary
        {...base()}
        policy={{ ...policy, backfillPending: true }}
        onManageMessages={vi.fn()}
      />
    );
    expect(screen.getByText('Still processing existing messages.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Manage messages' })).toBeInTheDocument();
  });

  it.each([
    ['loading', 'Loading message expiration…', false],
    ['unavailable', 'Message expiration unavailable', false],
    ['ready', 'Still processing existing messages.', true],
  ] as const)('does not claim cached processing while policy is %s', (policyState, text, ready) => {
    render(
      <MessageExpirationPolicySummary
        {...base()}
        policy={{ ...policy, backfillPending: true }}
        policyState={policyState}
      />
    );
    expect(screen.getByText(text)).toBeInTheDocument();
    if (ready) {
      expect(screen.getByText('Still processing existing messages.')).toBeInTheDocument();
    } else {
      expect(screen.queryByText('Still processing existing messages.')).not.toBeInTheDocument();
    }
  });
});
