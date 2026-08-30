import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, userEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { mockChannel, mockServer, mockUser } from '../../../mocks/fixtures';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import {
  MANAGE_ALL_MESSAGES,
  MANAGE_CHANNELS,
  MANAGE_OWN_MESSAGES,
  READ_MESSAGE_HISTORY,
} from '@/renderer/utils/permissions';
import ChannelContextMenu from '@/renderer/components/Channels/ChannelContextMenu';
import ServerContextMenu from '@/renderer/components/Servers/ServerContextMenu';
import DMConversationContextMenu from '@/renderer/components/DirectMessages/DMConversationContextMenu';
import GroupInfoPanel from '@/renderer/components/DirectMessages/GroupInfoPanel';
import PurgeMessagesModal from '@/renderer/components/Purge/PurgeMessagesModal';
import type { DMConversation } from '@/renderer/stores/chat/dmStore';

const noop = () => {};
const SERVER_ID = mockServer.id;
const CURRENT_USER_ID = 'user-1';

beforeEach(() => {
  resetAllStores();
  // resetAllStores does not cover permissionStore, and the gate reads both
  // permission maps — a leftover channel grant would mask the server fallback.
  usePermissionStore.setState({ channelPermissions: {}, serverPermissions: {} });
  useUserStore.setState({ user: { ...mockUser, id: CURRENT_USER_ID } });
});

/** ContextMenu.Item renders a native <button>, not role="menuitem". */
function menuLabels(): string[] {
  return screen.getAllByRole('button').map((node) => node.textContent ?? '');
}

function renderChannelMenu(onPurgeMessages = vi.fn()) {
  render(
    <ChannelContextMenu
      channel={mockChannel}
      position={{ x: 10, y: 10 }}
      serverId={SERVER_ID}
      onClose={noop}
      onEditChannel={noop}
      onDeleteChannel={noop}
      onPurgeMessages={onPurgeMessages}
    />
  );
  return onPurgeMessages;
}

function renderServerMenu(onPurgeMessages = vi.fn()) {
  render(
    <ServerContextMenu
      server={mockServer}
      position={{ x: 10, y: 10 }}
      onClose={noop}
      onEditServer={noop}
      onDeleteServer={noop}
      onLeaveServer={noop}
      onInvite={noop}
      onPurgeMessages={onPurgeMessages}
    />
  );
  return onPurgeMessages;
}

function makeConversation(overrides: Partial<DMConversation> = {}): DMConversation {
  return {
    id: 'conv-1',
    isGroup: false,
    isPersonal: false,
    name: null,
    participants: [
      { userId: CURRENT_USER_ID, username: 'alice' },
      { userId: 'user-2', username: 'bob' },
    ],
    lastMessage: null,
    unreadCount: 0,
    createdAt: '2025-01-01T00:00:00Z',
    ...overrides,
  };
}

describe('Channel purge entry point', () => {
  it('shows Purge Messages to a ManageOwn-only actor', () => {
    usePermissionStore.setState({ channelPermissions: { [mockChannel.id]: MANAGE_OWN_MESSAGES } });
    renderChannelMenu();
    expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeInTheDocument();
  });

  it('hides Purge Messages from an actor with neither manage bit', () => {
    usePermissionStore.setState({ channelPermissions: { [mockChannel.id]: READ_MESSAGE_HISTORY } });
    renderChannelMenu();
    expect(screen.queryByRole('button', { name: 'Purge Messages' })).not.toBeInTheDocument();
  });

  it('falls back to server permissions when channel effective perms are unknown', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: MANAGE_ALL_MESSAGES } });
    renderChannelMenu();
    expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeInTheDocument();
  });

  it('orders the destructive cluster by ascending severity', () => {
    usePermissionStore.setState({
      channelPermissions: { [mockChannel.id]: MANAGE_ALL_MESSAGES },
      serverPermissions: { [SERVER_ID]: MANAGE_CHANNELS },
    });
    renderChannelMenu();
    const labels = menuLabels();
    expect(labels.indexOf('Purge Messages')).toBeGreaterThan(-1);
    expect(labels.indexOf('Purge Messages')).toBeLessThan(labels.indexOf('Delete Channel'));
  });

  it('hands the channel to the parent-owned modal host', async () => {
    const user = userEvent.setup();
    usePermissionStore.setState({ channelPermissions: { [mockChannel.id]: MANAGE_ALL_MESSAGES } });
    const onPurgeMessages = renderChannelMenu();

    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    expect(onPurgeMessages).toHaveBeenCalledWith(mockChannel);
  });
});

describe('Server purge entry point', () => {
  it('shows Purge Messages to a ManageOwn-only actor', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: MANAGE_OWN_MESSAGES } });
    renderServerMenu();
    expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeInTheDocument();
  });

  it('hides Purge Messages from an actor with neither manage bit', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: READ_MESSAGE_HISTORY } });
    renderServerMenu();
    expect(screen.queryByRole('button', { name: 'Purge Messages' })).not.toBeInTheDocument();
  });

  it('sits above Delete Server in the destructive cluster', () => {
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: MANAGE_ALL_MESSAGES } });
    renderServerMenu();
    const labels = menuLabels();
    expect(labels.indexOf('Purge Messages')).toBeLessThan(labels.indexOf('Delete Server'));
  });
});

describe('DM purge entry point', () => {
  it('always shows Purge Messages in a 1:1 DM (participant-based, not RBAC)', () => {
    render(
      <DMConversationContextMenu
        conversation={makeConversation()}
        currentUserId={CURRENT_USER_ID}
        position={{ x: 10, y: 10 }}
        onClose={noop}
        onPurgeMessages={noop}
      />
    );
    expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeInTheDocument();
  });

  it('always shows Purge Messages in a group DM', () => {
    render(
      <DMConversationContextMenu
        conversation={makeConversation({ isGroup: true, name: 'Study Group' })}
        currentUserId={CURRENT_USER_ID}
        position={{ x: 10, y: 10 }}
        onClose={noop}
        onPurgeMessages={noop}
      />
    );
    expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeInTheDocument();
  });
});

describe('Group DM purge entry point', () => {
  const groupConversation = makeConversation({
    isGroup: true,
    name: 'Study Group',
    createdBy: 'user-2',
    participants: [
      { userId: CURRENT_USER_ID, username: 'alice', role: 'admin' },
      { userId: 'user-2', username: 'bob', role: 'member' },
    ],
  });

  it('opens the purge modal from the group danger zone with the admin role copy', async () => {
    const user = userEvent.setup();
    render(<GroupInfoPanel conversation={groupConversation} onClose={noop} />);

    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    expect(screen.getAllByRole('radio')).toHaveLength(9);

    await user.click(screen.getByRole('radio', { name: 'Last 7 days' }));
    expect(screen.getByText(/removed for everyone in the group/i)).toBeInTheDocument();
  });

  it('passes the member role for a non-admin participant', async () => {
    const user = userEvent.setup();
    render(
      <GroupInfoPanel
        conversation={{
          ...groupConversation,
          participants: [
            { userId: CURRENT_USER_ID, username: 'alice', role: 'member' },
            { userId: 'user-2', username: 'bob', role: 'admin' },
          ],
        }}
        onClose={noop}
      />
    );

    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    await user.click(screen.getByRole('radio', { name: 'Last 7 days' }));
    expect(screen.getByText(/hidden only for you/i)).toBeInTheDocument();
  });
});

describe('ManageOwn-only self-scope copy', () => {
  it('narrows the channel body to the actors own messages', async () => {
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="channel"
        isOpen
        scopeId="c1"
        scopeName="general"
        onClose={noop}
        selfScopeOnly
      />
    );

    await user.click(screen.getByRole('radio', { name: 'Last 7 days' }));
    expect(screen.getByText(/purge your messages from the last 7 days/i)).toBeInTheDocument();
  });

  it('acknowledges the moderated-channel subset in server context', async () => {
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="server"
        isOpen
        scopeId="s1"
        scopeName="Test Server"
        onClose={noop}
        selfScopeOnly
      />
    );

    await user.click(screen.getByRole('radio', { name: 'Last 7 days' }));
    expect(screen.getByText(/in channels you moderate/i)).toBeInTheDocument();
  });
});
