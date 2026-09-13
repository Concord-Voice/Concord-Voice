import { act, fireEvent, render, screen, userEvent, waitFor, within } from '../../../test-utils';
import { http, HttpResponse } from 'msw';
import { beforeAll, afterAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';
import { mockChannel, mockUser } from '../../../mocks/fixtures';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useDMStore, type DMConversation } from '@/renderer/stores/chat/dmStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { Permissions } from '@/renderer/utils/policy/permissions';
import EditChannelModal from '@/renderer/components/Channels/EditChannelModal';
import EditGroupModal from '@/renderer/components/DirectMessages/EditGroupModal';
import DMConversationContextMenu from '@/renderer/components/DirectMessages/DMConversationContextMenu';
import ConversationList from '@/renderer/components/DirectMessages/ConversationList';

const API = 'http://localhost:8080';
const policy = {
  expiration_window_seconds: 86400,
  expiration_updated_at: '2026-09-08T05:00:00.000Z',
  expiration_revision: 4,
  expiration_backfill_pending: false,
};

const conversation = (overrides: Partial<DMConversation> = {}): DMConversation => ({
  id: 'dm-1',
  isGroup: false,
  isPersonal: false,
  name: null,
  participants: [
    { userId: mockUser.id, username: mockUser.username, role: 'member' },
    { userId: 'user-2', username: 'alex', role: 'member' },
  ],
  lastMessage: null,
  unreadCount: 0,
  createdAt: policy.expiration_updated_at,
  ...overrides,
});

const policyForStore = {
  windowSeconds: 86400 as const,
  updatedAt: policy.expiration_updated_at,
  revision: 4,
  backfillPending: false,
};

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
  useAuthStore.getState().setSessionId('session-1');
  useUserStore.getState().setUser({ id: mockUser.id, username: mockUser.username });
  server.resetHandlers(
    http.get(`${API}/api/v1/servers/server-1/channels`, () =>
      HttpResponse.json({ channels: [{ ...mockChannel, ...policy }] })
    ),
    http.get(`${API}/api/v1/dm/conversations`, () =>
      HttpResponse.json({
        conversations: [
          {
            id: 'dm-1',
            is_group: false,
            is_personal: false,
            name: 'Alex',
            participants: [
              { user_id: mockUser.id, username: mockUser.username, role: 'member' },
              { user_id: 'user-2', username: 'alex', role: 'member' },
            ],
            last_message: null,
            unread_count: 0,
            created_at: policy.expiration_updated_at,
            ...policy,
          },
        ],
      })
    )
  );
});

describe('message expiration host entry points', () => {
  it('writes the exact channel timer request while preserving an unsaved name draft', async () => {
    const channel = { ...mockChannel, expirationPolicy: policyForStore };
    let timerBody: unknown;
    let nameRequests = 0;
    useChannelStore.getState().addChannel(channel);
    useChannelStore.setState({ currentServerId: 'server-1' });
    usePermissionStore.setState({
      channelPermissions: { [mockChannel.id]: Permissions.MANAGE_CHANNELS },
    });
    server.use(
      http.patch(`${API}/api/v1/channels/${channel.id}/expiration`, async ({ request }) => {
        timerBody = await request.json();
        return HttpResponse.json({
          window_seconds: 604800,
          updated_at: policy.expiration_updated_at,
          revision: 5,
          backfill_pending: false,
        });
      }),
      http.patch(`${API}/api/v1/channels/${channel.id}`, () => {
        nameRequests += 1;
        return HttpResponse.json({ channel });
      })
    );
    render(<EditChannelModal isOpen channel={channel} onClose={() => undefined} />);
    const user = userEvent.setup();
    fireEvent.change(screen.getByDisplayValue('general'), { target: { value: 'draft-name' } });
    const readyStop = await screen.findByRole('button', { name: '7 days' });
    await waitFor(() => expect(readyStop).toHaveAttribute('aria-disabled', 'false'));
    await user.click(readyStop);
    expect(
      await screen.findByRole('dialog', { name: 'Change message expiration' })
    ).toBeInTheDocument();
    expect(screen.getByDisplayValue('draft-name')).toBeInTheDocument();
    await user.click(screen.getByLabelText('Apply to existing messages'));
    await user.click(screen.getByLabelText('I understand deleted messages cannot be recovered.'));
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() =>
      expect(
        screen.queryByRole('dialog', { name: 'Change message expiration' })
      ).not.toBeInTheDocument()
    );
    expect(timerBody).toEqual({ mode: 'set', window_seconds: 604800, retroactive: 'apply' });
    expect(nameRequests).toBe(0);
    expect(
      useChannelStore.getState().channels.find((item) => item.id === channel.id)?.expirationPolicy
    ).toMatchObject({
      revision: 5,
      windowSeconds: 604800,
    });
    expect(screen.getByDisplayValue('draft-name')).toBeInTheDocument();
  });

  it('writes the exact group timer request and preserves the name draft', async () => {
    const group = conversation({
      id: 'group-1',
      isGroup: true,
      name: 'Team',
      participants: [
        { userId: mockUser.id, username: mockUser.username, role: 'admin' },
        { userId: 'user-2', username: 'alex', role: 'member' },
      ],
    });
    useDMStore.setState({ conversations: [group] });
    let timerBody: unknown;
    server.use(
      http.get(`${API}/api/v1/dm/conversations`, () =>
        HttpResponse.json({
          conversations: [
            {
              id: 'group-1',
              is_group: true,
              is_personal: false,
              name: 'Team',
              participants: [
                { user_id: mockUser.id, username: mockUser.username, role: 'admin' },
                { user_id: 'user-2', username: 'alex', role: 'member' },
              ],
              last_message: null,
              unread_count: 0,
              created_at: policy.expiration_updated_at,
              ...policy,
            },
          ],
        })
      ),
      http.patch(`${API}/api/v1/dm/conversations/group-1/expiration`, async ({ request }) => {
        timerBody = await request.json();
        return HttpResponse.json({
          window_seconds: 604800,
          updated_at: policy.expiration_updated_at,
          revision: 5,
          backfill_pending: false,
        });
      })
    );
    render(
      <EditGroupModal
        isOpen
        conversationId={group.id}
        currentName={group.name}
        onClose={() => undefined}
      />
    );
    const user = userEvent.setup();
    fireEvent.change(screen.getByPlaceholderText('Group Name (optional)'), {
      target: { value: 'draft-group' },
    });
    const readyStop = await screen.findByRole('button', { name: '7 days' });
    await waitFor(() => expect(readyStop).toHaveAttribute('aria-disabled', 'false'));
    await user.click(readyStop);
    expect(
      await screen.findByRole('dialog', { name: 'Change message expiration' })
    ).toBeInTheDocument();
    expect(screen.getByDisplayValue('draft-group')).toBeInTheDocument();
    await user.click(screen.getByLabelText('Apply to existing messages'));
    await user.click(screen.getByLabelText('I understand deleted messages cannot be recovered.'));
    await user.click(screen.getByRole('button', { name: 'Apply timer' }));
    await waitFor(() =>
      expect(
        screen.queryByRole('dialog', { name: 'Change message expiration' })
      ).not.toBeInTheDocument()
    );
    expect(timerBody).toEqual({ mode: 'set', window_seconds: 604800, retroactive: 'apply' });
    expect(
      useDMStore.getState().conversations.find((item) => item.id === group.id)?.expirationPolicy
    ).toMatchObject({
      revision: 5,
      windowSeconds: 604800,
    });
    expect(screen.getByDisplayValue('draft-group')).toBeInTheDocument();
  });

  it('keeps channel timer controls locked until the success close runs', async () => {
    const channel = {
      ...mockChannel,
      id: 'channel-delayed-close',
      name: 'general',
      expirationPolicy: policyForStore,
    };
    const onClose = vi.fn();
    useChannelStore.getState().addChannel(channel);
    useChannelStore.setState({ currentServerId: 'server-1' });
    usePermissionStore.setState({
      channelPermissions: { [channel.id]: Permissions.MANAGE_CHANNELS },
    });
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () =>
        HttpResponse.json({ channels: [{ ...channel, ...policy }] })
      ),
      http.patch(`${API}/api/v1/channels/${channel.id}`, () =>
        HttpResponse.json({ channel: { ...channel, name: 'renamed' } })
      )
    );
    render(<EditChannelModal isOpen channel={channel} onClose={onClose} />);
    const user = userEvent.setup();
    fireEvent.change(screen.getByDisplayValue('general'), { target: { value: 'renamed' } });
    await user.click(screen.getByRole('button', { name: 'Save Changes' }));

    await screen.findByText('Channel updated successfully!');
    const stop = await screen.findByRole('button', { name: '7 days' });
    expect(stop).toHaveAttribute('aria-disabled', 'true');
    await user.click(stop);
    expect(
      screen.queryByRole('dialog', { name: 'Change message expiration' })
    ).not.toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('locks channel timer controls while a name save is held', async () => {
    const channel = {
      ...mockChannel,
      id: 'channel-held',
      name: 'general',
      expirationPolicy: policyForStore,
    };
    const nameResponse = deferred<Response>();
    const onClose = vi.fn();
    useChannelStore.getState().addChannel(channel);
    useChannelStore.setState({ currentServerId: 'server-1' });
    usePermissionStore.setState({
      channelPermissions: { [channel.id]: Permissions.MANAGE_CHANNELS },
    });
    let timerRequests = 0;
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () =>
        HttpResponse.json({ channels: [{ ...channel, ...policy }] })
      ),
      http.patch(`${API}/api/v1/channels/${channel.id}`, () => nameResponse.promise),
      http.patch(`${API}/api/v1/channels/${channel.id}/expiration`, () => {
        timerRequests += 1;
        return HttpResponse.json({ error: 'not found' }, { status: 404 });
      })
    );
    render(<EditChannelModal isOpen channel={channel} onClose={onClose} />);
    const user = userEvent.setup();
    fireEvent.change(screen.getByDisplayValue('general'), { target: { value: 'renamed' } });
    await user.click(screen.getByRole('button', { name: 'Save Changes' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Saving...' })).toBeDisabled());
    const stop = await screen.findByRole('button', { name: '7 days' });
    await waitFor(() => expect(stop).toHaveAttribute('aria-disabled', 'true'));
    await user.click(stop);

    expect(
      screen.queryByRole('dialog', { name: 'Change message expiration' })
    ).not.toBeInTheDocument();
    expect(timerRequests).toBe(0);
    expect(onClose).not.toHaveBeenCalled();
    nameResponse.resolve(
      new Response(JSON.stringify({ channel: { ...channel, name: 'renamed' } }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await nameResponse.promise;
  });

  it('locks group timer controls while a name save is held', async () => {
    const group = conversation({
      id: 'group-held',
      isGroup: true,
      name: 'Team',
      expirationPolicy: policyForStore,
      participants: [
        { userId: mockUser.id, username: mockUser.username, role: 'admin' },
        { userId: 'user-2', username: 'alex', role: 'member' },
      ],
    });
    const nameResponse = deferred<Response>();
    const onClose = vi.fn();
    useDMStore.setState({ conversations: [group] });
    let timerRequests = 0;
    server.use(
      http.get(`${API}/api/v1/dm/conversations`, () =>
        HttpResponse.json({
          conversations: [
            {
              id: group.id,
              is_group: true,
              is_personal: false,
              name: group.name,
              participants: group.participants.map((member) => ({
                user_id: member.userId,
                username: member.username,
                role: member.role,
              })),
              last_message: null,
              unread_count: 0,
              created_at: policy.expiration_updated_at,
              ...policy,
            },
          ],
        })
      ),
      http.patch(`${API}/api/v1/dm/conversations/${group.id}`, () => nameResponse.promise),
      http.patch(`${API}/api/v1/dm/conversations/${group.id}/expiration`, () => {
        timerRequests += 1;
        return HttpResponse.json({ error: 'not found' }, { status: 404 });
      })
    );
    render(
      <EditGroupModal isOpen conversationId={group.id} currentName={group.name} onClose={onClose} />
    );
    const user = userEvent.setup();
    fireEvent.change(screen.getByPlaceholderText('Group Name (optional)'), {
      target: { value: 'Renamed team' },
    });
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Saving...' })).toBeDisabled());
    const stop = await screen.findByRole('button', { name: '7 days' });
    await waitFor(() => expect(stop).toHaveAttribute('aria-disabled', 'true'));
    await user.click(stop);

    expect(
      screen.queryByRole('dialog', { name: 'Change message expiration' })
    ).not.toBeInTheDocument();
    expect(timerRequests).toBe(0);
    expect(onClose).not.toHaveBeenCalled();
    nameResponse.resolve(
      new Response(JSON.stringify({ conversation: { ...group, name: 'Renamed team' } }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await nameResponse.promise;
  });

  it('shows a group member the policy while keeping timer stops read-only', async () => {
    const group = conversation({
      id: 'group-member',
      isGroup: true,
      name: 'Team',
      participants: [
        { userId: mockUser.id, username: mockUser.username, role: 'member' },
        { userId: 'user-2', username: 'alex', role: 'admin' },
      ],
    });
    useDMStore.setState({ conversations: [group] });
    server.use(
      http.get(`${API}/api/v1/dm/conversations`, () =>
        HttpResponse.json({
          conversations: [
            {
              id: 'group-member',
              is_group: true,
              is_personal: false,
              name: 'Team',
              participants: [
                { user_id: mockUser.id, username: mockUser.username, role: 'member' },
                { user_id: 'user-2', username: 'alex', role: 'admin' },
              ],
              last_message: null,
              unread_count: 0,
              created_at: policy.expiration_updated_at,
              ...policy,
            },
          ],
        })
      )
    );
    render(
      <EditGroupModal
        isOpen
        conversationId={group.id}
        currentName={group.name}
        onClose={() => undefined}
      />
    );
    const stop = await screen.findByRole('button', { name: '7 days' });
    expect(stop).toHaveAttribute('aria-disabled', 'true');
    expect(
      screen.getByText('Only group administrators can change this timer.')
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '24 hours' })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
  });

  it.each([
    ['personal', conversation({ id: 'personal-1', isPersonal: true })],
    ['group', conversation({ id: 'group-1', isGroup: true })],
  ] as const)(
    'excludes the 1:1 context-menu timer entry for %s conversations',
    (_label, target) => {
      useDMStore.setState({ conversations: [target] });
      const targetMenu = render(
        <DMConversationContextMenu
          conversation={target}
          currentUserId={mockUser.id}
          position={{ x: 0, y: 0 }}
          onClose={() => undefined}
          onMessageExpiration={vi.fn()}
        />
      );
      expect(screen.queryByRole('button', { name: 'Message expiration' })).not.toBeInTheDocument();
      targetMenu.unmount();
    }
  );

  it('does not render timer controls for a voice channel editor', async () => {
    const voiceChannel = { ...mockChannel, id: 'voice-1', type: 'voice' as const };
    useChannelStore.getState().addChannel(voiceChannel);
    render(<EditChannelModal isOpen channel={voiceChannel} onClose={() => undefined} />);
    expect(screen.getByText('Edit Channel')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '7 days' })).not.toBeInTheDocument();
  });

  it('lifts a 1:1 timer action after the context menu closes', async () => {
    const selected = conversation();
    const onExpiration = vi.fn();
    render(
      <DMConversationContextMenu
        conversation={selected}
        currentUserId={mockUser.id}
        position={{ x: 0, y: 0 }}
        onClose={() => undefined}
        onMessageExpiration={onExpiration}
      />
    );
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Message expiration' }));
    expect(onExpiration).toHaveBeenCalledWith(selected);
  });

  it('keeps two selected 1:1 targets isolated across held timer completion', async () => {
    const policyA = { ...policyForStore };
    const policyB = {
      ...policyForStore,
      windowSeconds: 3600 as const,
      revision: 6,
    };
    const targetA = conversation({
      id: 'dm-a',
      participants: [
        { userId: mockUser.id, username: mockUser.username, role: 'member' },
        { userId: 'user-a', username: 'alex', role: 'member' },
      ],
      expirationPolicy: policyA,
    });
    const targetB = conversation({
      id: 'dm-b',
      participants: [
        { userId: mockUser.id, username: mockUser.username, role: 'member' },
        { userId: 'user-b', username: 'bea', role: 'member' },
      ],
      expirationPolicy: policyB,
    });
    const pendingA = deferred<Response>();
    let patchA: unknown;
    let patchB: unknown;
    useDMStore.setState({
      conversations: [targetA, targetB],
      seenExpirationRevisionsByAccount: { [mockUser.id]: { 'dm-a': 4, 'dm-b': 4 } },
    });
    server.use(
      http.get(`${API}/api/v1/dm/conversations`, () =>
        HttpResponse.json({
          conversations: [
            {
              id: 'dm-a',
              is_group: false,
              is_personal: false,
              name: 'Alex',
              participants: [
                { user_id: mockUser.id, username: mockUser.username, role: 'member' },
                { user_id: 'user-a', username: 'alex', role: 'member' },
              ],
              last_message: null,
              unread_count: 0,
              created_at: policy.expiration_updated_at,
              ...policy,
            },
            {
              id: 'dm-b',
              is_group: false,
              is_personal: false,
              name: 'Bea',
              participants: [
                { user_id: mockUser.id, username: mockUser.username, role: 'member' },
                { user_id: 'user-b', username: 'bea', role: 'member' },
              ],
              last_message: null,
              unread_count: 0,
              created_at: policy.expiration_updated_at,
              ...policy,
              expiration_window_seconds: 3600,
              expiration_revision: 6,
            },
          ],
        })
      ),
      http.patch(`${API}/api/v1/dm/conversations/dm-a/expiration`, async ({ request }) => {
        patchA = await request.json();
        return pendingA.promise;
      }),
      http.patch(`${API}/api/v1/dm/conversations/dm-b/expiration`, async ({ request }) => {
        patchB = await request.json();
        return HttpResponse.json({
          window_seconds: 2592000,
          updated_at: policy.expiration_updated_at,
          revision: 7,
          backfill_pending: false,
        });
      })
    );
    const { rerender } = render(
      <ConversationList selectedThreadId="dm-a" onSelectThread={() => undefined} />
    );
    const user = userEvent.setup();
    await user.pointer({
      keys: '[MouseRight]',
      target: screen.getByRole('button', { name: 'alex' }),
    });
    await user.click(screen.getByRole('button', { name: 'Message expiration' }));
    expect(screen.queryByRole('button', { name: 'Close Conversation' })).not.toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByRole('button', { name: '24 hours' })).toHaveAttribute(
        'aria-disabled',
        'false'
      )
    );
    await user.click(screen.getByRole('button', { name: '7 days' }));
    const dialogA = await screen.findByRole('dialog', { name: 'Change message expiration' });
    await user.click(within(dialogA).getByRole('radio', { name: 'Only new messages' }));
    await user.click(within(dialogA).getByRole('checkbox', { name: /cannot be recovered/i }));
    await user.click(within(dialogA).getByRole('button', { name: 'Apply timer' }));
    await waitFor(() =>
      expect(patchA).toEqual({ mode: 'set', window_seconds: 604800, retroactive: 'new_only' })
    );
    rerender(<ConversationList selectedThreadId="dm-b" onSelectThread={() => undefined} />);
    await waitFor(() =>
      expect(
        screen.queryByRole('dialog', { name: 'Change message expiration' })
      ).not.toBeInTheDocument()
    );
    await user.pointer({
      keys: '[MouseRight]',
      target: screen.getByRole('button', { name: 'bea' }),
    });
    await user.click(screen.getByRole('button', { name: 'Message expiration' }));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: '1 hour' })).toHaveAttribute(
        'aria-disabled',
        'false'
      )
    );
    await user.click(screen.getByRole('button', { name: '30 days' }));
    const dialogB = await screen.findByRole('dialog', { name: 'Change message expiration' });
    await user.click(within(dialogB).getByRole('radio', { name: 'Only new messages' }));
    await user.click(within(dialogB).getByRole('checkbox', { name: /cannot be recovered/i }));
    try {
      await act(async () => {
        pendingA.resolve(
          new Response(
            JSON.stringify({
              window_seconds: 604800,
              updated_at: policy.expiration_updated_at,
              revision: 5,
              backfill_pending: false,
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
        await pendingA.promise;
      });
    } finally {
      pendingA.resolve(new Response('{}', { status: 200 }));
      await pendingA.promise;
    }
    expect(screen.getByRole('dialog', { name: 'Change message expiration' })).toBeInTheDocument();
    expect(screen.getByRole('radio', { name: 'Only new messages' })).toBeChecked();
    expect(screen.getByRole('checkbox', { name: /cannot be recovered/i })).toBeChecked();
    expect(
      useDMStore.getState().conversations.find((item) => item.id === 'dm-b')?.expirationPolicy
    ).toMatchObject({
      windowSeconds: 3600,
      revision: 6,
    });
    await user.click(
      within(screen.getByRole('dialog', { name: 'Change message expiration' })).getByRole(
        'button',
        { name: 'Apply timer' }
      )
    );
    await waitFor(() =>
      expect(
        screen.queryByRole('dialog', { name: 'Change message expiration' })
      ).not.toBeInTheDocument()
    );
    expect(patchB).toEqual({ mode: 'set', window_seconds: 2592000, retroactive: 'new_only' });
    expect(
      useDMStore.getState().conversations.find((item) => item.id === 'dm-b')?.expirationPolicy
    ).toMatchObject({
      windowSeconds: 2592000,
      revision: 7,
    });
    expect(
      useDMStore.getState().conversations.find((item) => item.id === 'dm-a')?.expirationPolicy
    ).toMatchObject({
      windowSeconds: 86400,
      revision: 4,
    });
  });
});
