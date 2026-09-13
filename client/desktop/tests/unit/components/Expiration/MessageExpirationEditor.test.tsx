import { act, render, screen, waitFor, within } from '../../../test-utils';
import userEvent from '@testing-library/user-event';
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { mockChannel, mockUser } from '../../../mocks/fixtures';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { Permissions } from '@/renderer/utils/policy/permissions';
import {
  useExpirationPolicy,
  type ExpirationPolicyControls,
} from '@/renderer/hooks/messaging/useExpirationPolicy';
import type {
  ExpirationMutationResult,
  ExpirationPolicy,
  ExpirationPolicyReadResult,
  ExpirationScope,
} from '@/renderer/services/messaging/expirationPolicyApi';
import MessageExpirationEditor from '@/renderer/components/Expiration/MessageExpirationEditor';

const policy: ExpirationPolicy = {
  windowSeconds: 86400,
  updatedAt: '2026-09-08T05:00:00Z',
  revision: 4,
  backfillPending: false,
};
const onApplyPolicy = vi.fn(async (): Promise<ExpirationMutationResult> => ({
  kind: 'ok',
  policy,
}));
const props = () => ({
  scope: { kind: 'channel' as const, id: 'channel-1' },
  policy,
  policyState: 'ready' as const,
  canEdit: true,
  lockedDescription: 'Only moderators can change this timer.',
  onRefresh: vi.fn(async () => ({ kind: 'fresh' as const, policy })),
  onApplyPolicy,
  onMarkSeen: vi.fn(),
  onClose: vi.fn(),
});

function HookEditorFixture({
  scope,
  serverId,
  onAdapter,
  onApplySettled,
}: {
  scope: ExpirationScope;
  serverId: string;
  onAdapter?: (adapter: ExpirationPolicyControls) => void;
  onApplySettled?: () => void;
}) {
  const adapter = useExpirationPolicy(scope, serverId);
  onAdapter?.(adapter);
  const controls = onApplySettled
    ? {
        ...adapter,
        onApplyPolicy: async (...args: Parameters<typeof adapter.onApplyPolicy>) => {
          try {
            return await adapter.onApplyPolicy(...args);
          } finally {
            onApplySettled();
          }
        },
      }
    : adapter;
  return <MessageExpirationEditor {...controls} scope={scope} onClose={vi.fn()} />;
}

beforeEach(() => {
  resetAllStores();
  vi.clearAllMocks();
  useUserStore.getState().setUser({ id: mockUser.id, username: mockUser.username });
  server.resetHandlers();
});

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());

describe('MessageExpirationEditor', () => {
  it('renders all five native stops and marks the current duration', () => {
    render(<MessageExpirationEditor {...props()} />);
    for (const label of ['Off', '1 hour', '24 hours', '7 days', '30 days']) {
      expect(screen.getByRole('button', { name: label })).toBeInTheDocument();
    }
    expect(screen.getByRole('button', { name: '24 hours' })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
  });

  it('does not open confirmation or request a PATCH when the selected window is unchanged', async () => {
    const user = userEvent.setup();
    render(<MessageExpirationEditor {...props()} />);
    await user.click(screen.getByRole('button', { name: '24 hours' }));
    expect(
      screen.queryByRole('dialog', { name: 'Change message expiration' })
    ).not.toBeInTheDocument();
    expect(onApplyPolicy).not.toHaveBeenCalled();
  });

  it('keeps locked controls focusable but activation disabled and explains the lock', async () => {
    const user = userEvent.setup();
    render(<MessageExpirationEditor {...props()} canEdit={false} />);
    const button = screen.getByRole('button', { name: '24 hours' });
    expect(button).toHaveAttribute('aria-disabled', 'true');
    expect(button).not.toBeDisabled();
    expect(button).toHaveAttribute('aria-describedby');
    button.focus();
    expect(document.activeElement).toBe(button);
    expect(screen.getByText('Only moderators can change this timer.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    expect(
      screen.queryByRole('dialog', { name: 'Change message expiration' })
    ).not.toBeInTheDocument();
  });

  it('requires a radio and acknowledgement before applying a selected timer', async () => {
    const user = userEvent.setup();
    render(<MessageExpirationEditor {...props()} />);
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    const dialog = await screen.findByRole('dialog', { name: 'Change message expiration' });
    expect(dialog).toBeInTheDocument();
    const apply = screen.getByRole('button', { name: 'Apply timer' });
    expect(apply).toBeDisabled();
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    expect(apply).toBeDisabled();
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    expect(apply).toBeEnabled();
    await user.click(apply);
    await waitFor(() =>
      expect(onApplyPolicy).toHaveBeenCalledWith({
        mode: 'set',
        window_seconds: 3600,
        retroactive: 'new_only',
      })
    );
  });

  it.each([
    ['1 hour', 3600],
    ['24 hours', 86400],
    ['7 days', 604800],
    ['30 days', 2592000],
  ] as const)('shows the chosen %s duration in the confirmation disclosure', async (label) => {
    const user = userEvent.setup();
    render(<MessageExpirationEditor {...props()} policy={{ ...policy, windowSeconds: null }} />);

    await user.click(screen.getByRole('button', { name: label }));
    const dialog = await screen.findByRole('dialog', { name: 'Change message expiration' });

    expect(within(dialog).getByText(new RegExp(`after ${label}`))).toBeInTheDocument();
  });

  it('uses the Off-specific confirmation choices and sends the exact clear body', async () => {
    const user = userEvent.setup();
    render(<MessageExpirationEditor {...props()} />);
    await user.click(screen.getByRole('button', { name: 'Off' }));
    await screen.findByRole('dialog', { name: 'Turn off message expiration' });
    expect(screen.getByRole('radio', { name: 'Cancel scheduled deletions' })).not.toBeChecked();
    expect(screen.getByRole('radio', { name: 'Keep scheduled deletions' })).not.toBeChecked();
    await user.click(screen.getByRole('radio', { name: 'Cancel scheduled deletions' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(screen.getByRole('button', { name: 'Turn off timer' }));
    await waitFor(() =>
      expect(onApplyPolicy).toHaveBeenCalledWith({ mode: 'clear', retroactive: 'clear_pending' })
    );
  });

  it.each([
    ['Cancel scheduled deletions', 'clear_pending'],
    ['Keep scheduled deletions', 'leave_pending'],
  ] as const)(
    'clears with %s and renders Off after the real mutation',
    async (choiceLabel, retroactive) => {
      const user = userEvent.setup();
      useAuthStore.getState().setAccessToken('mock-token');
      useAuthStore.getState().setSessionId('session-1');
      useChannelStore.setState({ currentServerId: 'server-1' });
      usePermissionStore.setState({
        channelPermissions: { 'channel-1': Permissions.MANAGE_CHANNELS },
      });
      let requestBody: unknown;
      server.use(
        http.get('http://localhost:8080/api/v1/servers/server-1/channels', () =>
          HttpResponse.json({
            channels: [
              {
                ...mockChannel,
                expiration_window_seconds: 86400,
                expiration_updated_at: policy.updatedAt,
                expiration_revision: 4,
                expiration_backfill_pending: false,
              },
            ],
          })
        ),
        http.patch(
          'http://localhost:8080/api/v1/channels/channel-1/expiration',
          async ({ request }) => {
            requestBody = await request.json();
            return HttpResponse.json({
              window_seconds: null,
              updated_at: policy.updatedAt,
              revision: 5,
              backfill_pending: false,
            });
          }
        )
      );
      render(
        <HookEditorFixture scope={{ kind: 'channel', id: 'channel-1' }} serverId="server-1" />
      );
      await waitFor(() =>
        expect(screen.getByRole('button', { name: '24 hours' })).toHaveAttribute(
          'aria-pressed',
          'true'
        )
      );
      await user.click(screen.getByRole('button', { name: 'Off' }));
      await user.click(screen.getByRole('radio', { name: choiceLabel }));
      await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
      expect(screen.getByText(/already deleted cannot be recovered/i)).toBeInTheDocument();
      await user.click(screen.getByRole('button', { name: 'Turn off timer' }));
      await waitFor(() => expect(requestBody).toEqual({ mode: 'clear', retroactive }));
      await waitFor(() =>
        expect(screen.getByRole('button', { name: 'Off' })).toHaveAttribute('aria-pressed', 'true')
      );
    }
  );

  it('holds the PATCH behind a completed fresh read and rejects a changed baseline', async () => {
    const user = userEvent.setup();
    const fresh = deferred<{ kind: 'fresh'; policy: ExpirationPolicy }>();
    const refresh = vi.fn(() => fresh.promise);
    const apply = vi.fn(async (): Promise<ExpirationMutationResult> => ({
      kind: 'ok' as const,
      policy: { ...policy, windowSeconds: 3600 },
    }));
    const view = render(
      <MessageExpirationEditor {...props()} onRefresh={refresh} onApplyPolicy={apply} />
    );
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    const submit = user.click(screen.getByRole('button', { name: 'Apply timer' }));
    try {
      await waitFor(() => expect(refresh).toHaveBeenCalledOnce());
      expect(apply).not.toHaveBeenCalled();
      expect(screen.getByRole('radio', { name: 'Only new messages' })).toBeDisabled();
      expect(screen.getByRole('checkbox', { name: /cannot be recovered/i })).toBeDisabled();
      fresh.resolve({ kind: 'fresh', policy });
      await act(async () => {
        await submit;
      });
      await waitFor(() =>
        expect(apply).toHaveBeenCalledWith({
          mode: 'set',
          window_seconds: 3600,
          retroactive: 'new_only',
        })
      );
      expect(apply).toHaveBeenCalledOnce();
    } finally {
      fresh.resolve({ kind: 'fresh', policy });
    }

    const changed: ExpirationPolicy = { ...policy, revision: 5, windowSeconds: 604800 };
    const changedRefresh = vi.fn(async () => ({ kind: 'fresh' as const, policy: changed }));
    view.rerender(
      <MessageExpirationEditor
        {...props()}
        policy={policy}
        onRefresh={changedRefresh}
        onApplyPolicy={apply}
      />
    );
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        'The policy changed. Review the refreshed timer before continuing.'
      )
    );
    expect(apply).toHaveBeenCalledTimes(1);
    view.rerender(
      <MessageExpirationEditor
        {...props()}
        policy={changed}
        onRefresh={changedRefresh}
        onApplyPolicy={apply}
      />
    );
    expect(screen.getByRole('button', { name: '7 days' })).toHaveAttribute('aria-pressed', 'true');
  });

  it('clears old consent when the displayed policy changes before confirmation', async () => {
    const user = userEvent.setup();
    const apply = vi.fn(async () => ({ kind: 'ok' as const, policy }));
    const view = render(<MessageExpirationEditor {...props()} onApplyPolicy={apply} />);
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    const changed: ExpirationPolicy = { ...policy, revision: 5, windowSeconds: 604800 };
    view.rerender(<MessageExpirationEditor {...props()} policy={changed} onApplyPolicy={apply} />);
    const confirmation = screen.queryByRole('button', { name: 'Apply timer' });
    if (confirmation) expect(confirmation).toBeDisabled();
    else expect(screen.queryByRole('dialog', { name: 'Change message expiration' })).toBeNull();
    expect(apply).not.toHaveBeenCalled();
  });

  it('requires an explicit refresh after a conflict instead of reusing the confirmation', async () => {
    const user = userEvent.setup();
    const apply = vi.fn(async () => ({
      kind: 'conflict' as const,
      policy: { ...policy, revision: 5 },
    }));
    render(<MessageExpirationEditor {...props()} onApplyPolicy={apply} />);
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        'This policy changed. Refresh the policy before continuing.'
      )
    );
    expect(screen.getByRole('button', { name: 'Refresh policy' })).toBeInTheDocument();
    expect(apply).toHaveBeenCalledOnce();
  });

  it('requires separate acknowledgement and same-revision fresh read before Resume', async () => {
    const user = userEvent.setup();
    const fresh = deferred<{ kind: 'fresh'; policy: ExpirationPolicy }>();
    const refresh = vi.fn(() => fresh.promise);
    const apply = vi.fn(async () => ({
      kind: 'ok' as const,
      policy: { ...policy, backfillPending: false },
    }));
    render(
      <MessageExpirationEditor
        {...props()}
        policy={{ ...policy, backfillPending: true }}
        onRefresh={refresh}
        onApplyPolicy={apply}
      />
    );
    await user.click(screen.getByRole('button', { name: 'Resume processing' }));
    const dialog = screen.getByRole('dialog', { name: 'Resume message expiration processing' });
    expect(dialog).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Resume processing' })).toBeDisabled();
    await user.click(within(dialog).getByRole('checkbox', { name: /cannot be recovered/i }));
    const submit = user.click(within(dialog).getByRole('button', { name: 'Resume processing' }));
    try {
      await waitFor(() => expect(refresh).toHaveBeenCalledOnce());
      expect(within(dialog).getByRole('checkbox', { name: /cannot be recovered/i })).toBeDisabled();
      fresh.resolve({ kind: 'fresh', policy: { ...policy, backfillPending: true } });
      await act(async () => {
        await submit;
      });
      await waitFor(() => expect(apply).toHaveBeenCalledWith({ mode: 'resume', revision: 4 }));
      expect(apply).toHaveBeenCalledOnce();
    } finally {
      fresh.resolve({ kind: 'fresh', policy: { ...policy, backfillPending: true } });
    }
    expect(refresh).toHaveBeenCalledOnce();
    expect(
      screen.queryByRole('dialog', { name: 'Resume message expiration processing' })
    ).not.toBeInTheDocument();
  });

  it('accepts a same-revision completed policy after a Resume 503 reread', async () => {
    const user = userEvent.setup();
    const pendingPolicy = { ...policy, backfillPending: true };
    const completed = { ...pendingPolicy, backfillPending: false };
    const refresh = vi
      .fn()
      .mockResolvedValueOnce({ kind: 'fresh' as const, policy: pendingPolicy })
      .mockResolvedValueOnce({ kind: 'fresh' as const, policy: completed });
    const apply = vi.fn(async () => ({
      kind: 'partial' as const,
      candidate: pendingPolicy,
    }));
    const onMarkSeen = vi.fn();
    render(
      <MessageExpirationEditor
        {...props()}
        policy={pendingPolicy}
        onRefresh={refresh}
        onApplyPolicy={apply}
        onMarkSeen={onMarkSeen}
      />
    );
    await user.click(screen.getByRole('button', { name: 'Resume processing' }));
    const dialog = screen.getByRole('dialog', { name: 'Resume message expiration processing' });
    await user.click(within(dialog).getByRole('checkbox', { name: /cannot be recovered/i }));
    await act(async () => {
      await user.click(within(dialog).getByRole('button', { name: 'Resume processing' }));
    });

    expect(refresh).toHaveBeenCalledTimes(2);
    expect(apply).toHaveBeenCalledWith({ mode: 'resume', revision: policy.revision });
    expect(onMarkSeen).toHaveBeenCalledWith(completed.revision);
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Refresh policy' })).toBeNull();
  });

  it.each([
    [
      'changed revision',
      { kind: 'fresh', policy: { ...policy, revision: 5, backfillPending: true } },
    ],
    ['completed', { kind: 'fresh', policy: { ...policy, backfillPending: false } }],
    ['unavailable', { kind: 'unavailable' }],
    ['superseded', { kind: 'superseded' }],
  ] as const)('does not Resume after %s revalidation', async (_label, outcome) => {
    const user = userEvent.setup();
    const refresh = vi.fn(async () => outcome);
    const apply = vi.fn(async () => ({ kind: 'ok' as const, policy }));
    render(
      <MessageExpirationEditor
        {...props()}
        policy={{ ...policy, backfillPending: true }}
        onRefresh={refresh}
        onApplyPolicy={apply}
      />
    );
    await user.click(screen.getByRole('button', { name: 'Resume processing' }));
    const dialog = screen.getByRole('dialog', { name: 'Resume message expiration processing' });
    await user.click(within(dialog).getByRole('checkbox', { name: /cannot be recovered/i }));
    await act(async () => {
      await user.click(within(dialog).getByRole('button', { name: 'Resume processing' }));
    });
    expect(refresh).toHaveBeenCalledOnce();
    if (outcome.kind === 'fresh' && !outcome.policy.backfillPending) {
      expect(
        screen.queryByRole('dialog', { name: 'Resume message expiration processing' })
      ).toBeNull();
    } else {
      expect(screen.getByRole('alert')).toHaveTextContent(/Refresh|policy changed/i);
    }
    expect(apply).not.toHaveBeenCalled();
  });

  it('accepts a same-revision completed policy after a 503 reread', async () => {
    const user = userEvent.setup();
    const completed = { ...policy, revision: 5, windowSeconds: 3600, backfillPending: false };
    const refresh = vi
      .fn()
      .mockResolvedValueOnce({ kind: 'fresh' as const, policy })
      .mockResolvedValueOnce({ kind: 'fresh' as const, policy: completed });
    const apply = vi.fn(async () => ({
      kind: 'partial' as const,
      candidate: { ...completed, backfillPending: true },
    }));
    const onMarkSeen = vi.fn();
    render(
      <MessageExpirationEditor
        {...props()}
        onRefresh={refresh}
        onApplyPolicy={apply}
        onMarkSeen={onMarkSeen}
      />
    );
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    await act(async () => {
      await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    });
    expect(refresh).toHaveBeenCalledTimes(2);
    expect(apply).toHaveBeenCalledWith({
      mode: 'set',
      window_seconds: 3600,
      retroactive: 'new_only',
    });
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Refresh policy' })).toBeNull();
  });

  it.each([
    ['1 hour', { mode: 'set', window_seconds: 3600, retroactive: 'apply' }],
    ['24 hours', { mode: 'set', window_seconds: 86400, retroactive: 'apply' }],
    ['7 days', { mode: 'set', window_seconds: 604800, retroactive: 'apply' }],
    ['30 days', { mode: 'set', window_seconds: 2592000, retroactive: 'apply' }],
  ])('opens the %s confirmation and sends the exact apply body', async (label, expected) => {
    const user = userEvent.setup();
    const offPolicy = { ...policy, windowSeconds: null };
    const onRefresh = vi.fn(async () => ({ kind: 'fresh' as const, policy: offPolicy }));
    render(<MessageExpirationEditor {...props()} policy={offPolicy} onRefresh={onRefresh} />);
    await user.click(screen.getByRole('button', { name: label }));
    await screen.findByRole('dialog', { name: 'Change message expiration' });
    expect(screen.getByRole('radio', { name: 'Only new messages' })).not.toBeChecked();
    expect(screen.getByRole('checkbox', { name: /cannot be recovered/i })).not.toBeChecked();
    await user.click(screen.getByRole('radio', { name: 'Apply to existing messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() => expect(onApplyPolicy).toHaveBeenCalledWith(expected));
  });

  it('clears acknowledgement when the choice changes, supports new-only, and never patches until fresh read', async () => {
    const user = userEvent.setup();
    const fresh = vi.fn(async () => ({ kind: 'fresh' as const, policy }));
    render(<MessageExpirationEditor {...props()} onRefresh={fresh} />);
    await user.click(screen.getByRole('button', { name: '30 days' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(screen.getByRole('radio', { name: 'Apply to existing messages' }));
    expect(screen.getByRole('button', { name: 'Apply timer' })).toBeDisabled();
    expect(onApplyPolicy).not.toHaveBeenCalled();
  });

  it('shows loading/unavailable retry and pending processing recovery states', async () => {
    const refresh = vi.fn(async () => ({ kind: 'unavailable' as const }));
    const view = render(
      <MessageExpirationEditor
        {...props()}
        policy={null}
        policyState="unavailable"
        onRefresh={refresh}
      />
    );
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(refresh).toHaveBeenCalledOnce());
    view.rerender(
      <MessageExpirationEditor {...props()} policy={{ ...policy, backfillPending: true }} />
    );
    expect(screen.getByText('Still processing existing messages.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Resume processing' })).toBeInTheDocument();
  });

  it.each([
    ['loading', 'Loading message expiration…', false],
    ['unavailable', 'Message expiration unavailable', false],
    ['ready', 'Still processing existing messages.', true],
  ] as const)('keeps cached pending policy honest while %s', (policyState, text, canResume) => {
    render(
      <MessageExpirationEditor
        {...props()}
        policy={{ ...policy, backfillPending: true }}
        policyState={policyState}
      />
    );
    expect(screen.getByText(text)).toBeInTheDocument();
    if (canResume) {
      expect(screen.getByRole('button', { name: 'Resume processing' })).toBeInTheDocument();
    } else {
      expect(screen.queryByText('Still processing existing messages.')).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Resume processing' })).not.toBeInTheDocument();
    }
  });

  it('does not let an older Retry result clear successor recovery state', async () => {
    const user = userEvent.setup();
    const oldRefresh = deferred<ExpirationPolicyReadResult>();
    const oldRefreshCall = vi.fn(() => oldRefresh.promise);
    const successorRefresh = vi.fn(async () => ({ kind: 'fresh' as const, policy }));
    const successorApply = vi.fn(async () => ({ kind: 'ambiguous' as const }));
    const view = render(
      <MessageExpirationEditor
        {...props()}
        scope={{ kind: 'channel', id: 'channel-1' }}
        policy={null}
        policyState="unavailable"
        onRefresh={oldRefreshCall}
      />
    );
    try {
      await user.click(screen.getByRole('button', { name: 'Retry' }));
      await waitFor(() => expect(oldRefreshCall).toHaveBeenCalledOnce());
      view.rerender(
        <MessageExpirationEditor
          {...props()}
          scope={{ kind: 'channel', id: 'channel-2' }}
          policy={policy}
          policyState="ready"
          canEdit
          onRefresh={successorRefresh}
          onApplyPolicy={successorApply}
        />
      );
      await user.click(screen.getByRole('button', { name: '1 hour' }));
      await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
      await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
      await user.click(screen.getByRole('button', { name: 'Apply timer' }));
      await waitFor(() =>
        expect(screen.getByRole('alert')).toHaveTextContent(
          /confirm whether this change was applied/i
        )
      );
      expect(screen.getByRole('button', { name: 'Refresh policy' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: '1 hour' })).toHaveAttribute(
        'aria-disabled',
        'true'
      );
      oldRefresh.resolve({ kind: 'fresh', policy });
      await act(async () => {
        await oldRefresh.promise;
      });
      expect(screen.getByRole('alert')).toHaveTextContent(
        /confirm whether this change was applied/i
      );
      expect(screen.getByRole('button', { name: 'Refresh policy' })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: '1 hour' })).toHaveAttribute(
        'aria-disabled',
        'true'
      );
    } finally {
      oldRefresh.resolve({ kind: 'fresh', policy });
      view.unmount();
    }
  });

  it('renders the irreversibility disclosure for every named message type and can cancel', async () => {
    const user = userEvent.setup();
    render(<MessageExpirationEditor {...props()} />);
    await user.click(screen.getByRole('button', { name: '7 days' }));
    expect(screen.getByText(/text, images, GIFs, attachments, and emoji/i)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onApplyPolicy).not.toHaveBeenCalled();
  });

  it('holds a 503 candidate outside the store until a matching fresh read authorizes recovery', async () => {
    const user = userEvent.setup();
    useAuthStore.getState().setAccessToken('mock-token');
    useAuthStore.getState().setSessionId('session-1');
    useChannelStore.setState({ currentServerId: 'server-1' });
    usePermissionStore.setState({
      channelPermissions: { 'channel-1': Permissions.MANAGE_CHANNELS },
    });
    const recoveryRead = deferred<Response>();
    const recoveryStarted = deferred<void>();
    let readCount = 0;
    let patchCount = 0;
    let patchBody: unknown;
    const candidateWire = {
      window_seconds: 3600,
      updated_at: policy.updatedAt,
      revision: 5,
      backfill_pending: true,
    };
    const row = (
      value:
        | typeof candidateWire
        | { window_seconds: number; revision: number; backfill_pending: boolean }
    ) => ({
      ...mockChannel,
      expiration_window_seconds: value.window_seconds,
      expiration_updated_at: policy.updatedAt,
      expiration_revision: value.revision,
      expiration_backfill_pending: value.backfill_pending,
    });
    server.use(
      http.get('http://localhost:8080/api/v1/servers/server-1/channels', async () => {
        readCount += 1;
        if (readCount === 3) {
          recoveryStarted.resolve();
          return recoveryRead.promise;
        }
        return HttpResponse.json({
          channels: [
            row(
              readCount >= 4
                ? candidateWire
                : {
                    window_seconds: 86400,
                    revision: 4,
                    backfill_pending: false,
                  }
            ),
          ],
        });
      }),
      http.patch(
        'http://localhost:8080/api/v1/channels/channel-1/expiration',
        async ({ request }) => {
          patchCount += 1;
          patchBody = await request.json();
          if (patchCount === 1) return HttpResponse.json(candidateWire, { status: 503 });
          return HttpResponse.json({ ...candidateWire, backfill_pending: false });
        }
      )
    );
    try {
      render(
        <HookEditorFixture scope={{ kind: 'channel', id: 'channel-1' }} serverId="server-1" />
      );
      await waitFor(() =>
        expect(screen.getByRole('button', { name: '24 hours' })).toHaveAttribute(
          'aria-pressed',
          'true'
        )
      );
      await user.click(screen.getByRole('button', { name: '1 hour' }));
      await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
      await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
      await user.click(screen.getByRole('button', { name: 'Apply timer' }));
      await waitFor(() =>
        expect(patchBody).toEqual({ mode: 'set', window_seconds: 3600, retroactive: 'new_only' })
      );
      await recoveryStarted.promise;
      expect(
        useChannelStore.getState().channels.find((item) => item.id === 'channel-1')
          ?.expirationPolicy?.revision
      ).toBe(4);
      expect(screen.queryByRole('button', { name: 'Resume processing' })).not.toBeInTheDocument();
      recoveryRead.resolve(
        HttpResponse.json({
          channels: [
            {
              ...mockChannel,
              expiration_window_seconds: 3600,
              expiration_updated_at: policy.updatedAt,
              expiration_revision: 5,
              expiration_backfill_pending: true,
            },
          ],
        })
      );
      await waitFor(() =>
        expect(screen.getByRole('button', { name: 'Resume processing' })).toBeInTheDocument()
      );
      expect(
        useChannelStore.getState().channels.find((item) => item.id === 'channel-1')
          ?.expirationPolicy
      ).toMatchObject({ revision: 5, backfillPending: true });
      await user.click(screen.getByRole('button', { name: 'Resume processing' }));
      const dialog = screen.getByRole('dialog', { name: 'Resume message expiration processing' });
      await user.click(within(dialog).getByRole('checkbox', { name: /cannot be recovered/i }));
      await user.click(within(dialog).getByRole('button', { name: 'Resume processing' }));
      await waitFor(() => expect(patchCount).toBe(2));
      expect(patchBody).toEqual({ mode: 'resume', revision: 5 });
      await waitFor(() =>
        expect(screen.getByRole('button', { name: '1 hour' })).toHaveAttribute(
          'aria-pressed',
          'true'
        )
      );
      await waitFor(() =>
        expect(
          useChannelStore.getState().channels.find((item) => item.id === 'channel-1')
            ?.expirationPolicy
        ).toMatchObject({ revision: 5, backfillPending: false })
      );
      expect(
        screen.queryByRole('dialog', { name: 'Resume message expiration processing' })
      ).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: '1 hour' })).toHaveAttribute(
        'aria-disabled',
        'false'
      );
    } finally {
      recoveryRead.resolve(
        HttpResponse.json({
          channels: [
            {
              ...mockChannel,
              expiration_window_seconds: 3600,
              expiration_updated_at: policy.updatedAt,
              expiration_revision: 5,
              expiration_backfill_pending: true,
            },
          ],
        })
      );
    }
  });

  it.each([
    [
      'mismatched candidate',
      6,
      { window_seconds: 3600, updated_at: policy.updatedAt, revision: 5, backfill_pending: true },
    ],
    ['no candidate', 5, undefined],
  ] as const)(
    'does not enable automatic Resume for a real 503 %s',
    async (_label, rereadRevision, candidate) => {
      const user = userEvent.setup();
      useAuthStore.getState().setAccessToken('mock-token');
      useAuthStore.getState().setSessionId('session-1');
      useChannelStore.setState({ currentServerId: 'server-1' });
      usePermissionStore.setState({
        channelPermissions: { 'channel-1': Permissions.MANAGE_CHANNELS },
      });
      let reads = 0;
      let patches = 0;
      let patchBody: unknown;
      let patchSeen: (() => void) | undefined;
      const patchStarted = new Promise<void>((resolve) => {
        patchSeen = resolve;
      });
      let explicitRead = false;
      const pendingWire = {
        window_seconds: rereadRevision === 6 ? 3600 : 3600,
        updated_at: policy.updatedAt,
        revision: rereadRevision,
        backfill_pending: true,
      };
      server.use(
        http.get('http://localhost:8080/api/v1/servers/server-1/channels', () => {
          reads += 1;
          const current = explicitRead
            ? pendingWire
            : reads > 2
              ? pendingWire
              : {
                  window_seconds: 86400,
                  updated_at: policy.updatedAt,
                  revision: 4,
                  backfill_pending: false,
                };
          return HttpResponse.json({
            channels: [
              {
                ...mockChannel,
                expiration_window_seconds: current.window_seconds,
                expiration_updated_at: current.updated_at,
                expiration_revision: current.revision,
                expiration_backfill_pending: current.backfill_pending,
              },
            ],
          });
        }),
        http.patch(
          'http://localhost:8080/api/v1/channels/channel-1/expiration',
          async ({ request }) => {
            patches += 1;
            patchBody = await request.json();
            patchSeen?.();
            return candidate
              ? HttpResponse.json(candidate, { status: 503 })
              : HttpResponse.json({ malformed: true }, { status: 503 });
          }
        )
      );
      render(
        <HookEditorFixture scope={{ kind: 'channel', id: 'channel-1' }} serverId="server-1" />
      );
      await waitFor(() =>
        expect(screen.getByRole('button', { name: '24 hours' })).toHaveAttribute(
          'aria-pressed',
          'true'
        )
      );
      await user.click(screen.getByRole('button', { name: '1 hour' }));
      await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
      await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
      await user.click(screen.getByRole('button', { name: 'Apply timer' }));
      await patchStarted;
      expect(patchBody).toEqual({ mode: 'set', window_seconds: 3600, retroactive: 'new_only' });
      await waitFor(() =>
        expect(screen.getByText('Still processing existing messages.')).toBeInTheDocument()
      );
      const resume = screen.queryByRole('button', { name: 'Resume processing' });
      if (resume) expect(resume).toBeDisabled();
      explicitRead = true;
      const readsBeforeExplicitRefresh = reads;
      await user.click(screen.getByRole('button', { name: 'Refresh policy' }));
      await waitFor(() => expect(reads).toBeGreaterThan(readsBeforeExplicitRefresh));
      await waitFor(() =>
        expect(screen.getByRole('button', { name: 'Resume processing' })).toBeEnabled()
      );
      expect(patches).toBe(1);
      await user.click(screen.getByRole('button', { name: 'Resume processing' }));
      const resumeDialog = screen.getByRole('dialog', {
        name: 'Resume message expiration processing',
      });
      await user.click(
        within(resumeDialog).getByRole('checkbox', { name: /cannot be recovered/i })
      );
      await user.click(within(resumeDialog).getByRole('button', { name: 'Resume processing' }));
      await waitFor(() => expect(patches).toBe(2));
      expect(patchBody).toEqual({ mode: 'resume', revision: rereadRevision });
      expect(
        useChannelStore.getState().channels.find((item) => item.id === 'channel-1')
          ?.expirationPolicy?.revision
      ).toBe(rereadRevision);
    }
  );

  it('shows the exact ambiguity copy and blocks further mutation after an uncertain result', async () => {
    const user = userEvent.setup();
    const apply = vi
      .fn()
      .mockResolvedValueOnce({ kind: 'ambiguous' as const })
      .mockResolvedValueOnce({ kind: 'ok' as const, policy });
    const refresh = vi.fn(async () => ({ kind: 'fresh' as const, policy }));
    render(<MessageExpirationEditor {...props()} onRefresh={refresh} onApplyPolicy={apply} />);
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        'We couldn’t confirm whether this change was applied. Refresh the policy before continuing.'
      )
    );
    expect(screen.getByRole('button', { name: 'Refresh policy' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '30 days' })).toHaveAttribute(
      'aria-disabled',
      'true'
    );
    expect(apply).toHaveBeenCalledOnce();
    await user.click(screen.getByRole('button', { name: 'Refresh policy' }));
    await waitFor(() => expect(refresh).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: '1 hour' })).toHaveAttribute(
        'aria-disabled',
        'false'
      )
    );
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() => expect(apply).toHaveBeenCalledTimes(2));
  });

  it('makes a forbidden editor read-only and cannot repeat the still-open confirmation', async () => {
    const user = userEvent.setup();
    const apply = vi.fn(async () => ({ kind: 'rejected' as const, reason: 'forbidden' as const }));
    render(<MessageExpirationEditor {...props()} onApplyPolicy={apply} />);
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('server refused'));
    expect(screen.getByRole('button', { name: '1 hour' })).toHaveAttribute('aria-disabled', 'true');
    const confirm = screen.queryByRole('button', { name: 'Apply timer' });
    if (confirm) expect(confirm).toBeDisabled();
    else expect(screen.queryByRole('dialog', { name: 'Change message expiration' })).toBeNull();
    expect(apply).toHaveBeenCalledOnce();
  });

  it('keeps a forbidden editor read-only when a newer policy revision is displayed', async () => {
    const user = userEvent.setup();
    const apply = vi.fn(async () => ({ kind: 'rejected' as const, reason: 'forbidden' as const }));
    const view = render(<MessageExpirationEditor {...props()} onApplyPolicy={apply} />);
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('server refused'));
    const successor: ExpirationPolicy = { ...policy, revision: 5, windowSeconds: 2592000 };
    view.rerender(
      <MessageExpirationEditor {...props()} policy={successor} onApplyPolicy={apply} />
    );
    expect(screen.getByRole('button', { name: '30 days' })).toHaveAttribute(
      'aria-disabled',
      'true'
    );
    await user.click(screen.getByRole('button', { name: '30 days' }));
    expect(
      screen.queryByRole('dialog', { name: 'Change message expiration' })
    ).not.toBeInTheDocument();
    expect(apply).toHaveBeenCalledOnce();
  });

  it('closes and refreshes after not found without changing the displayed policy', async () => {
    const user = userEvent.setup();
    const apply = vi.fn(async () => ({ kind: 'rejected' as const, reason: 'notFound' as const }));
    const refresh = vi.fn(async () => ({ kind: 'fresh' as const, policy }));
    const onClose = vi.fn();
    render(
      <MessageExpirationEditor
        {...props()}
        onRefresh={refresh}
        onApplyPolicy={apply}
        onClose={onClose}
      />
    );
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
    expect(refresh).toHaveBeenCalledTimes(2);
    expect(screen.getByRole('button', { name: '24 hours' })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('shows a session expiry alert and preserves the canonical policy', async () => {
    const user = userEvent.setup();
    const apply = vi.fn(async () => ({
      kind: 'rejected' as const,
      reason: 'sessionExpired' as const,
    }));
    render(<MessageExpirationEditor {...props()} onApplyPolicy={apply} />);
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('session has expired'));
    expect(screen.getByRole('button', { name: '24 hours' })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
  });

  it.each([
    [{ kind: 'rejected', reason: 'rateLimited', retryAfterSeconds: 5 }, 'Try again in 5 seconds.'],
    [{ kind: 'rejected', reason: 'rateLimited' }, 'Try again later.'],
  ] as const)('shows valid rate-limit guidance (%s)', async (result, copy) => {
    const user = userEvent.setup();
    const apply = vi.fn(async () => result);
    render(<MessageExpirationEditor {...props()} onApplyPolicy={apply} />);
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(copy));
  });

  it('retains the selected draft after an invalid request rejection', async () => {
    const user = userEvent.setup();
    const apply = vi.fn(async () => ({
      kind: 'rejected' as const,
      reason: 'invalidRequest' as const,
    }));
    render(<MessageExpirationEditor {...props()} onApplyPolicy={apply} />);
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent('could not be applied')
    );
    expect(screen.getByRole('dialog', { name: 'Change message expiration' })).toBeInTheDocument();
    expect(screen.getByRole('radio', { name: 'Only new messages' })).toBeChecked();
    expect(screen.getByRole('checkbox', { name: /cannot be recovered/i })).toBeChecked();
  });

  it('fences a held confirmation read when the editor scope changes', async () => {
    const user = userEvent.setup();
    const fresh = deferred<{ kind: 'fresh'; policy: ExpirationPolicy }>();
    const refresh = vi.fn(() => fresh.promise);
    const apply = vi.fn(async () => ({ kind: 'ok' as const, policy }));
    const view = render(
      <MessageExpirationEditor {...props()} onRefresh={refresh} onApplyPolicy={apply} />
    );
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    const submit = user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() => expect(refresh).toHaveBeenCalledOnce());
    view.rerender(
      <MessageExpirationEditor
        {...props()}
        scope={{ kind: 'channel', id: 'channel-2' }}
        onRefresh={refresh}
        onApplyPolicy={apply}
      />
    );
    fresh.resolve({ kind: 'fresh', policy });
    await submit;
    expect(apply).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: '24 hours' })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
  });

  it.each([
    ['scope success', 'scope', false],
    ['scope ordinary error', 'scope', true],
    ['auth-generation success', 'auth', false],
    ['newer canonical revision', 'policy', false],
  ] as const)(
    'keeps a successor confirmation open when an older real PATCH completes (%s)',
    async (_label, continuation, rejectPatch) => {
      const user = userEvent.setup();
      const patchStarted = deferred<void>();
      const patchResponse = deferred<Response>();
      const applySettled = deferred<void>();
      useAuthStore.getState().setAccessToken('mock-token');
      useAuthStore.getState().setSessionId('session-1');
      useChannelStore.setState({ currentServerId: 'server-1' });
      usePermissionStore.setState({
        channelPermissions: {
          'channel-1': Permissions.MANAGE_CHANNELS,
          'channel-2': Permissions.MANAGE_CHANNELS,
        },
      });
      const row = (id: string) => ({
        ...mockChannel,
        id,
        expiration_window_seconds: policy.windowSeconds,
        expiration_updated_at: policy.updatedAt,
        expiration_revision: policy.revision,
        expiration_backfill_pending: policy.backfillPending,
      });
      server.use(
        http.get('http://localhost:8080/api/v1/servers/server-1/channels', () =>
          HttpResponse.json({ channels: [row('channel-1'), row('channel-2')] })
        ),
        http.patch('http://localhost:8080/api/v1/channels/channel-1/expiration', async () => {
          patchStarted.resolve();
          return patchResponse.promise;
        })
      );
      const view = render(
        <HookEditorFixture
          scope={{ kind: 'channel', id: 'channel-1' }}
          serverId="server-1"
          onApplySettled={applySettled.resolve}
        />
      );
      try {
        await waitFor(() =>
          expect(screen.getByRole('button', { name: '24 hours' })).toHaveAttribute(
            'aria-pressed',
            'true'
          )
        );
        await user.click(screen.getByRole('button', { name: '1 hour' }));
        await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
        await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
        await user.click(screen.getByRole('button', { name: 'Apply timer' }));
        await patchStarted.promise;

        if (continuation === 'auth')
          act(() => useAuthStore.getState().beginAuthLifecycle('successor-token', 'session-2'));
        if (continuation === 'policy')
          act(() =>
            useChannelStore.getState().applyExpirationPolicy('channel-1', {
              ...policy,
              revision: 6,
              windowSeconds: 604800,
            })
          );
        const successorScope =
          continuation === 'scope'
            ? ({ kind: 'channel', id: 'channel-2' } as const)
            : ({ kind: 'channel', id: 'channel-1' } as const);
        view.rerender(<HookEditorFixture scope={successorScope} serverId="server-1" />);
        const currentLabel = continuation === 'policy' ? '7 days' : '24 hours';
        await waitFor(() =>
          expect(screen.getByRole('button', { name: currentLabel })).toHaveAttribute(
            'aria-pressed',
            'true'
          )
        );
        await user.click(screen.getByRole('button', { name: '1 hour' }));
        await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
        await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
        expect(screen.getByRole('button', { name: 'Apply timer' })).toBeEnabled();
        expect(
          screen.getByRole('dialog', { name: 'Change message expiration' })
        ).toBeInTheDocument();

        if (rejectPatch) patchResponse.reject(new Error('ordinary transport error'));
        else
          patchResponse.resolve(
            HttpResponse.json({
              window_seconds: 3600,
              updated_at: policy.updatedAt,
              revision: 5,
              backfill_pending: false,
            })
          );
        await act(async () => {
          await applySettled.promise;
        });
        expect(
          screen.getByRole('dialog', { name: 'Change message expiration' })
        ).toBeInTheDocument();
        expect(screen.getByRole('checkbox', { name: /cannot be recovered/i })).toBeChecked();
        expect(screen.getByRole('button', { name: 'Apply timer' })).toBeEnabled();
        expect(
          useChannelStore.getState().channels.find((item) => item.id === successorScope.id)
            ?.expirationPolicy
        ).toMatchObject({
          revision: continuation === 'policy' ? 6 : 4,
        });
      } finally {
        patchResponse.resolve(
          HttpResponse.json({
            window_seconds: 3600,
            updated_at: policy.updatedAt,
            revision: 5,
            backfill_pending: false,
          })
        );
        applySettled.resolve();
        view.unmount();
      }
    }
  );

  it('ignores a held PATCH after the editor unmounts', async () => {
    const user = userEvent.setup();
    const applyResponse = deferred<ExpirationMutationResult>();
    const onClose = vi.fn();
    const apply = vi.fn(() => applyResponse.promise);
    const view = render(
      <MessageExpirationEditor {...props()} onApplyPolicy={apply} onClose={onClose} />
    );
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    const submit = user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() => expect(apply).toHaveBeenCalledOnce());
    view.unmount();
    applyResponse.resolve({ kind: 'ok', policy: { ...policy, windowSeconds: 3600 } });
    await submit;
    expect(onClose).not.toHaveBeenCalled();
  });

  it('does not submit a confirmation after permission is revoked', async () => {
    const user = userEvent.setup();
    const apply = vi.fn(async () => ({ kind: 'ok' as const, policy }));
    const view = render(<MessageExpirationEditor {...props()} onApplyPolicy={apply} />);
    await user.click(screen.getByRole('button', { name: '1 hour' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    view.rerender(<MessageExpirationEditor {...props()} canEdit={false} onApplyPolicy={apply} />);
    const confirm = screen.queryByRole('button', { name: 'Apply timer' });
    if (confirm) expect(confirm).toBeDisabled();
    else expect(screen.queryByRole('dialog', { name: 'Change message expiration' })).toBeNull();
    expect(apply).not.toHaveBeenCalled();
  });

  it('uses the real hook and stores for a PATCH, then displays and stores the resulting policy', async () => {
    const user = userEvent.setup();
    useAuthStore.getState().setAccessToken('mock-token');
    useAuthStore.getState().setSessionId('session-1');
    useChannelStore.setState({ currentServerId: 'server-1' });
    usePermissionStore.setState({
      channelPermissions: { 'channel-1': Permissions.MANAGE_CHANNELS },
    });
    let patchBody: unknown;
    server.use(
      http.get('http://localhost:8080/api/v1/servers/server-1/channels', () =>
        HttpResponse.json({
          channels: [
            {
              ...mockChannel,
              expiration_window_seconds: 86400,
              expiration_updated_at: policy.updatedAt,
              expiration_revision: 4,
              expiration_backfill_pending: false,
            },
          ],
        })
      ),
      http.patch(
        'http://localhost:8080/api/v1/channels/channel-1/expiration',
        async ({ request }) => {
          patchBody = await request.json();
          return HttpResponse.json({
            window_seconds: 2592000,
            updated_at: policy.updatedAt,
            revision: 5,
            backfill_pending: false,
          });
        }
      )
    );
    render(<HookEditorFixture scope={{ kind: 'channel', id: 'channel-1' }} serverId="server-1" />);
    await waitFor(() =>
      expect(screen.getByRole('button', { name: '24 hours' })).toHaveAttribute(
        'aria-pressed',
        'true'
      )
    );
    await user.click(screen.getByRole('button', { name: '30 days' }));
    await user.click(screen.getByRole('radio', { name: 'Only new messages' }));
    await user.click(screen.getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() =>
      expect(patchBody).toEqual({ mode: 'set', window_seconds: 2592000, retroactive: 'new_only' })
    );
    await waitFor(() =>
      expect(
        useChannelStore.getState().channels.find((item) => item.id === 'channel-1')
          ?.expirationPolicy?.windowSeconds
      ).toBe(2592000)
    );
    expect(screen.getByRole('button', { name: '30 days' })).toHaveAttribute('aria-pressed', 'true');
  });
});
