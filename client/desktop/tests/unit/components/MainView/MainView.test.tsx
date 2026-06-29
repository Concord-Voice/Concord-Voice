import { render, screen, fireEvent } from '../../../test-utils';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { ADMIN_PERMISSIONS } from '@/renderer/utils/permissions';
import { mockServer, mockChannel } from '../../../mocks/fixtures';
import { resetAllStores } from '../../../helpers/store-helpers';

// ── Layout mocks ───────────────────────────────────────────────────────────────

vi.mock('@/renderer/components/Layout/AppLayout', () => ({
  default: ({
    serverBar,
    folderBar,
    channelPanel,
    chatArea,
    memberSpace,
  }: {
    serverBar: React.ReactNode;
    folderBar: React.ReactNode;
    channelPanel: React.ReactNode;
    chatArea: React.ReactNode;
    memberSpace: React.ReactNode;
  }) => (
    <div data-testid="app-layout">
      <div data-testid="server-bar">{serverBar}</div>
      <div data-testid="folder-bar">{folderBar}</div>
      <div data-testid="channel-panel">{channelPanel}</div>
      <div data-testid="chat-area">{chatArea}</div>
      <div data-testid="member-space">{memberSpace}</div>
    </div>
  ),
}));

vi.mock('@/renderer/components/Layout/ServerBar', () => ({
  default: ({ onOpenActionModal, onContextMenu }: any) => (
    <>
      <div>ServerBar</div>
      <button data-testid="sb-open-action" onClick={onOpenActionModal} />
      <button
        data-testid="sb-open-context"
        onClick={() =>
          onContextMenu?.(
            {
              id: 'server-1',
              name: 'Test Server',
              role: 'owner',
              owner_id: 'user-1',
              created_at: '',
              updated_at: '',
              member_count: 2,
              online_count: 1,
            },
            { x: 0, y: 0 }
          )
        }
      />
    </>
  ),
}));

vi.mock('@/renderer/components/Layout/FolderBar', () => ({
  default: () => <div>FolderBar</div>,
}));
vi.mock('@/renderer/components/Layout/ChannelPanel', () => ({
  default: ({ header, children }: { header: React.ReactNode; children: React.ReactNode }) => (
    <div>
      {header}
      {children}
    </div>
  ),
}));
vi.mock('@/renderer/components/Layout/MemberFlexSpace', () => ({
  default: () => <div>MemberFlexSpace</div>,
}));

// ── Chat / Voice mocks ─────────────────────────────────────────────────────────

vi.mock('@/renderer/components/Chat', () => ({
  ChatView: () => <div data-testid="chat-view">ChatView</div>,
}));
vi.mock('@/renderer/components/Voice/VoiceView', () => ({
  default: () => <div data-testid="voice-view">VoiceView</div>,
}));
vi.mock('@/renderer/components/Voice/PersistentVoiceBar', () => ({
  default: () => <div data-testid="persistent-voice-bar">PersistentVoiceBar</div>,
}));
vi.mock('@/renderer/components/Voice/VoiceTextChat', () => ({
  default: () => <div data-testid="voice-text-chat">VoiceTextChat</div>,
}));
vi.mock('@/renderer/services/pipSignalingProxy', () => ({
  // Regular function (not arrow) so `new PipSignalingProxy(...)` works.
  // Returns the mock object so proxy.dispose() is callable.
  PipSignalingProxy: vi.fn(function MockPipProxy() {
    return { dispose: vi.fn(), onPipClosed: vi.fn() };
  }),
}));
vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: {},
}));

// ── Channel list / action bar ──────────────────────────────────────────────────

vi.mock('@/renderer/components/Channels/ChannelList', () => ({
  default: ({ onContextMenu, onCategoryContextMenu, onEmptyContextMenu }: any) => (
    <>
      <div>ChannelList</div>
      <button
        data-testid="cl-ch-ctx"
        onClick={() =>
          onContextMenu?.(
            {
              id: 'channel-1',
              server_id: 'server-1',
              name: 'general',
              type: 'text',
              position: 0,
              created_at: '',
              updated_at: '',
            },
            { x: 0, y: 0 }
          )
        }
      />
      <button
        data-testid="cl-cat-ctx"
        onClick={() =>
          onCategoryContextMenu?.(
            { id: 'cat-1', name: 'Category', server_id: 'server-1', position: 0 },
            { x: 0, y: 0 }
          )
        }
      />
      <button data-testid="cl-empty-ctx" onClick={() => onEmptyContextMenu?.({ x: 0, y: 0 })} />
    </>
  ),
}));
vi.mock('@/renderer/components/Channels/ServerActionBar', () => ({
  default: ({ onOpenCreateModal, onOpenCreateCategoryModal }: any) => (
    <>
      <div>ServerActionBar</div>
      <button data-testid="sab-create-channel" onClick={onOpenCreateModal} />
      <button data-testid="sab-create-category" onClick={onOpenCreateCategoryModal} />
    </>
  ),
}));
vi.mock('@/renderer/components/ConnectionStatus/ConnectionStatus', () => ({
  default: () => <div>ConnectionStatus</div>,
}));
vi.mock('@/renderer/hooks/useServerChannelSubscriptions', () => ({
  useServerChannelSubscriptions: vi.fn(),
}));

// ── Server modals ──────────────────────────────────────────────────────────────

vi.mock('@/renderer/components/Servers/ServerContextMenu', () => ({
  default: ({ onClose, onEditServer, onDeleteServer, onLeaveServer, onInvite, server }: any) => (
    <div data-testid="server-context-menu">
      <button data-testid="ctx-close" onClick={onClose} />
      <button data-testid="ctx-edit" onClick={() => onEditServer(server)} />
      <button data-testid="ctx-delete" onClick={() => onDeleteServer(server)} />
      <button data-testid="ctx-leave" onClick={() => onLeaveServer(server)} />
      <button data-testid="ctx-invite" onClick={() => onInvite(server)} />
    </div>
  ),
}));
vi.mock('@/renderer/components/Servers/DeleteServerModal', () => ({
  default: ({ isOpen }: any) => (isOpen ? <div data-testid="delete-server-modal" /> : null),
}));
vi.mock('@/renderer/components/Servers/LeaveServerModal', () => ({
  default: ({ isOpen }: any) => (isOpen ? <div data-testid="leave-server-modal" /> : null),
}));
vi.mock('@/renderer/components/Servers/CreateServerModal', () => ({
  default: ({ isOpen, onSuccess }: any) =>
    isOpen ? (
      <button
        data-testid="csm-success"
        onClick={() =>
          onSuccess({
            id: 'new-server',
            name: 'New Server',
            role: 'owner',
            owner_id: 'user-1',
            created_at: '',
            updated_at: '',
            member_count: 1,
            online_count: 1,
          })
        }
      />
    ) : null,
}));
vi.mock('@/renderer/components/Servers/ServerActionModal', () => ({
  default: ({ isOpen, onCreateServer, onJoinServer }: any) =>
    isOpen ? (
      <div data-testid="server-action-modal">
        <button data-testid="sam-create" onClick={onCreateServer} />
        <button data-testid="sam-join" onClick={onJoinServer} />
      </div>
    ) : null,
}));
vi.mock('@/renderer/components/Servers/JoinServerModal', () => ({
  default: ({ isOpen, onSuccess }: any) =>
    isOpen ? (
      <button
        data-testid="jsm-success"
        onClick={() =>
          onSuccess({
            id: 'joined-server',
            name: 'Joined Server',
            role: 'member',
            owner_id: 'user-2',
            created_at: '',
            updated_at: '',
            member_count: 5,
            online_count: 2,
          })
        }
      />
    ) : null,
}));
vi.mock('@/renderer/components/Servers/InviteToServerModal', () => ({
  default: ({ isOpen }: any) => (isOpen ? <div data-testid="invite-modal" /> : null),
}));

// ── Channel modals ─────────────────────────────────────────────────────────────

vi.mock('@/renderer/components/Channels/ChannelContextMenu', () => ({
  default: ({ onClose, onEditChannel, onDeleteChannel, onChannelPermissions, channel }: any) => (
    <div data-testid="channel-context-menu">
      <button data-testid="cctx-edit" onClick={() => onEditChannel(channel)} />
      <button data-testid="cctx-delete" onClick={() => onDeleteChannel(channel)} />
      <button data-testid="cctx-perms" onClick={() => onChannelPermissions(channel)} />
      <button data-testid="cctx-close" onClick={onClose} />
    </div>
  ),
}));
vi.mock('@/renderer/components/Channels/CategoryContextMenu', () => ({
  default: ({ onClose, onEditCategory, onDeleteCategory, onCategoryPermissions, group }: any) => (
    <div data-testid="category-context-menu">
      <button data-testid="catctx-edit" onClick={() => onEditCategory(group)} />
      <button data-testid="catctx-delete" onClick={() => onDeleteCategory(group)} />
      <button data-testid="catctx-perms" onClick={() => onCategoryPermissions(group)} />
      <button data-testid="catctx-close" onClick={onClose} />
    </div>
  ),
}));
vi.mock('@/renderer/components/Channels/ChannelListContextMenu', () => ({
  default: () => <div data-testid="channel-list-context-menu" />,
}));
vi.mock('@/renderer/components/Channels/CreateChannelModal', () => ({
  default: ({ isOpen, onSuccess }: any) =>
    isOpen ? (
      <button
        data-testid="ccm-success"
        onClick={() =>
          onSuccess({
            id: 'new-ch',
            server_id: 'server-1',
            name: 'new-channel',
            type: 'text',
            position: 1,
            created_at: '',
            updated_at: '',
          })
        }
      />
    ) : null,
}));
vi.mock('@/renderer/components/Channels/CreateCategoryModal', () => ({ default: () => null }));
vi.mock('@/renderer/components/Channels/EditChannelModal', () => ({
  default: ({ isOpen }: any) => (isOpen ? <div data-testid="edit-channel-modal" /> : null),
}));
vi.mock('@/renderer/components/Channels/EditCategoryModal', () => ({
  default: ({ isOpen }: any) => (isOpen ? <div data-testid="edit-category-modal" /> : null),
}));
vi.mock('@/renderer/components/Channels/DeleteChannelModal', () => ({
  default: ({ isOpen }: any) => (isOpen ? <div data-testid="delete-channel-modal" /> : null),
}));
vi.mock('@/renderer/components/Channels/DeleteCategoryModal', () => ({
  default: ({ isOpen }: any) => (isOpen ? <div data-testid="delete-category-modal" /> : null),
}));
vi.mock('@/renderer/components/Channels/ChannelSettingsModal', () => ({
  default: ({ isOpen }: any) => (isOpen ? <div data-testid="channel-settings-modal" /> : null),
}));
vi.mock('@/renderer/components/Channels/CategorySettingsModal', () => ({
  default: ({ isOpen }: any) => (isOpen ? <div data-testid="category-settings-modal" /> : null),
}));
vi.mock('@/renderer/components/User/UserPanel', () => ({ default: () => null }));

// ── API mock ───────────────────────────────────────────────────────────────────

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({ ok: true, json: async () => ({}) }),
  API_BASE: 'http://localhost:8080',
}));

import { useSettingsOverlayStore } from '@/renderer/stores/settingsOverlayStore';
import MainView from '@/renderer/components/MainView/MainView';

describe('MainView', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useServerStore.setState({
      servers: [mockServer],
      activeServerId: 'server-1',
    });
    useChannelStore.setState({
      channels: [mockChannel],
      activeChannelId: 'channel-1',
    });
    // Reset voice state so voice-persistence tests start from a known baseline
    useVoiceStore.setState({
      activeChannelId: null,
      connectionState: 'disconnected',
    });
  });

  it('renders the app layout', () => {
    render(<MainView />);
    expect(screen.getByTestId('app-layout')).toBeInTheDocument();
  });

  it('renders server bar section', () => {
    render(<MainView />);
    expect(screen.getByTestId('server-bar')).toBeInTheDocument();
    expect(screen.getByText('ServerBar')).toBeInTheDocument();
  });

  it('renders folder bar section', () => {
    render(<MainView />);
    expect(screen.getByTestId('folder-bar')).toBeInTheDocument();
    expect(screen.getByText('FolderBar')).toBeInTheDocument();
  });

  it('renders channel panel with header', () => {
    render(<MainView />);
    // Server name should appear in the channel header
    expect(screen.getByText(mockServer.name)).toBeInTheDocument();
    expect(screen.getByText('ConnectionStatus')).toBeInTheDocument();
  });

  it('renders chat view when active channel exists', () => {
    render(<MainView />);
    expect(screen.getByTestId('chat-view')).toBeInTheDocument();
  });

  // E2EE indicator tooltip — exercises computeE2eeTooltipPos (#1516 refactor).
  // Right-aligned branch: the trigger sits far enough from the left edge that a
  // 240px tooltip still clears the 8px padding.
  it('shows the E2EE tooltip on hover, right-aligned to the indicator', () => {
    const { container } = render(<MainView />);
    const indicator = container.querySelector('.server-e2ee-indicator') as HTMLElement;
    expect(indicator).not.toBeNull();
    indicator.getBoundingClientRect = () =>
      ({
        right: 500,
        left: 260,
        bottom: 40,
        top: 20,
        width: 20,
        height: 20,
        x: 260,
        y: 20,
        toJSON: () => ({}),
      }) as DOMRect;
    fireEvent.mouseEnter(indicator);
    expect(screen.getByText('E2EE Enabled')).toBeInTheDocument();
    fireEvent.mouseLeave(indicator);
    expect(screen.queryByText('E2EE Enabled')).not.toBeInTheDocument();
  });

  // Clamped branch: the trigger is near the left edge, so the right-aligned
  // position would underflow the padding and the helper clamps instead.
  it('shows the E2EE tooltip clamped when the indicator is near the left edge', () => {
    const { container } = render(<MainView />);
    const indicator = container.querySelector('.server-e2ee-indicator') as HTMLElement;
    indicator.getBoundingClientRect = () =>
      ({
        right: 100,
        left: 10,
        bottom: 40,
        top: 20,
        width: 20,
        height: 20,
        x: 10,
        y: 20,
        toJSON: () => ({}),
      }) as DOMRect;
    fireEvent.mouseEnter(indicator);
    expect(screen.getByText('E2EE Enabled')).toBeInTheDocument();
  });

  it('shows welcome message when no server is active', () => {
    useServerStore.setState({ servers: [], activeServerId: null });
    useChannelStore.setState({ channels: [], activeChannelId: null });
    render(<MainView />);
    expect(screen.getByText('Welcome to Concord Voice')).toBeInTheDocument();
  });

  it('shows channel selection prompt when server active but no channel', () => {
    useChannelStore.setState({ channels: [mockChannel], activeChannelId: null });
    render(<MainView />);
    expect(screen.getByText('Select a channel to start chatting.')).toBeInTheDocument();
  });

  it('shows empty server message when server has no channels', () => {
    useChannelStore.setState({ channels: [], activeChannelId: null });
    render(<MainView />);
    expect(screen.getByText('This server appears empty.')).toBeInTheDocument();
    expect(screen.getByText('Add some channels to start chatting!')).toBeInTheDocument();
  });

  it('renders member space section', () => {
    render(<MainView />);
    expect(screen.getByTestId('member-space')).toBeInTheDocument();
    expect(screen.getByText('MemberFlexSpace')).toBeInTheDocument();
  });

  it('renders the view container with correct class', () => {
    const { container } = render(<MainView />);
    expect(container.querySelector('.main-view')).toBeInTheDocument();
  });

  // ── Voice routing ───────────────────────────────────────────────────────────

  it('renders VoiceView when active channel type is voice', () => {
    const voiceChannel = { ...mockChannel, id: 'voice-ch-1', type: 'voice' as const };
    useChannelStore.setState({
      channels: [voiceChannel],
      activeChannelId: 'voice-ch-1',
    });
    render(<MainView />);
    expect(screen.getByTestId('voice-view')).toBeInTheDocument();
    expect(screen.queryByTestId('chat-view')).not.toBeInTheDocument();
  });

  it('renders PersistentVoiceBar when in voice but viewing a different channel', () => {
    useVoiceStore.setState({
      activeChannelId: 'voice-ch-1',
      connectionState: 'connected',
    });
    // User is viewing a text channel (not the voice channel)
    useChannelStore.setState({
      channels: [mockChannel],
      activeChannelId: 'channel-1',
    });
    render(<MainView />);
    expect(screen.getByTestId('persistent-voice-bar')).toBeInTheDocument();
  });

  it('does not render PersistentVoiceBar when viewing own voice channel', () => {
    const voiceChannel = { ...mockChannel, id: 'voice-ch-1', type: 'voice' as const };
    useVoiceStore.setState({
      activeChannelId: 'voice-ch-1',
      connectionState: 'connected',
    });
    useChannelStore.setState({
      channels: [voiceChannel],
      activeChannelId: 'voice-ch-1',
    });
    render(<MainView />);
    expect(screen.queryByTestId('persistent-voice-bar')).not.toBeInTheDocument();
  });

  // ── Server action modal ────────────────────────────────────────────────────

  it('opens ServerActionModal when action button is clicked', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('sb-open-action'));
    expect(screen.getByTestId('server-action-modal')).toBeInTheDocument();
  });

  it('opens CreateServerModal from ServerActionModal', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('sb-open-action'));
    fireEvent.click(screen.getByTestId('sam-create'));
    expect(screen.getByTestId('csm-success')).toBeInTheDocument();
  });

  it('sets active server after successful server creation', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('sb-open-action'));
    fireEvent.click(screen.getByTestId('sam-create'));
    fireEvent.click(screen.getByTestId('csm-success'));
    expect(useServerStore.getState().activeServerId).toBe('new-server');
  });

  it('opens JoinServerModal from ServerActionModal', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('sb-open-action'));
    fireEvent.click(screen.getByTestId('sam-join'));
    expect(screen.getByTestId('jsm-success')).toBeInTheDocument();
  });

  it('sets active server after successful server join', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('sb-open-action'));
    fireEvent.click(screen.getByTestId('sam-join'));
    fireEvent.click(screen.getByTestId('jsm-success'));
    expect(useServerStore.getState().activeServerId).toBe('joined-server');
  });

  // ── Channel creation ───────────────────────────────────────────────────────

  it('opens CreateChannelModal from ServerActionBar', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('sab-create-channel'));
    expect(screen.getByTestId('ccm-success')).toBeInTheDocument();
  });

  it('sets active channel after successful channel creation', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('sab-create-channel'));
    fireEvent.click(screen.getByTestId('ccm-success'));
    expect(useChannelStore.getState().activeChannelId).toBe('new-ch');
  });

  // ── Server context menu ────────────────────────────────────────────────────

  it('shows ServerContextMenu on server context menu trigger', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('sb-open-context'));
    expect(screen.getByTestId('server-context-menu')).toBeInTheDocument();
  });

  it('closes ServerContextMenu on close callback', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('sb-open-context'));
    fireEvent.click(screen.getByTestId('ctx-close'));
    expect(screen.queryByTestId('server-context-menu')).not.toBeInTheDocument();
  });

  it('closes ServerContextMenu when edit server is triggered', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('sb-open-context'));
    fireEvent.click(screen.getByTestId('ctx-edit'));
    expect(screen.queryByTestId('server-context-menu')).not.toBeInTheDocument();
  });

  it('shows DeleteServerModal when delete server is triggered', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('sb-open-context'));
    fireEvent.click(screen.getByTestId('ctx-delete'));
    expect(screen.getByTestId('delete-server-modal')).toBeInTheDocument();
  });

  it('shows LeaveServerModal when leave server is triggered', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('sb-open-context'));
    fireEvent.click(screen.getByTestId('ctx-leave'));
    expect(screen.getByTestId('leave-server-modal')).toBeInTheDocument();
  });

  it('shows InviteToServerModal when invite server is triggered', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('sb-open-context'));
    fireEvent.click(screen.getByTestId('ctx-invite'));
    expect(screen.getByTestId('invite-modal')).toBeInTheDocument();
  });

  // ── Channel context menu ───────────────────────────────────────────────────

  it('shows ChannelContextMenu on channel context menu trigger', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('cl-ch-ctx'));
    expect(screen.getByTestId('channel-context-menu')).toBeInTheDocument();
  });

  it('shows EditChannelModal when edit channel is triggered', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('cl-ch-ctx'));
    fireEvent.click(screen.getByTestId('cctx-edit'));
    expect(screen.getByTestId('edit-channel-modal')).toBeInTheDocument();
  });

  it('shows DeleteChannelModal when delete channel is triggered', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('cl-ch-ctx'));
    fireEvent.click(screen.getByTestId('cctx-delete'));
    expect(screen.getByTestId('delete-channel-modal')).toBeInTheDocument();
  });

  it('shows ChannelSettingsModal when channel permissions is triggered', () => {
    render(<MainView />);
    fireEvent.click(screen.getByTestId('cl-ch-ctx'));
    fireEvent.click(screen.getByTestId('cctx-perms'));
    expect(screen.getByTestId('channel-settings-modal')).toBeInTheDocument();
  });

  // ── Category context menu ──────────────────────────────────────────────────

  it('shows CategoryContextMenu on category context menu trigger (with manage permission)', () => {
    usePermissionStore.setState({ serverPermissions: { 'server-1': ADMIN_PERMISSIONS } });
    render(<MainView />);
    fireEvent.click(screen.getByTestId('cl-cat-ctx'));
    expect(screen.getByTestId('category-context-menu')).toBeInTheDocument();
  });

  it('shows EditCategoryModal when edit category is triggered', () => {
    usePermissionStore.setState({ serverPermissions: { 'server-1': ADMIN_PERMISSIONS } });
    render(<MainView />);
    fireEvent.click(screen.getByTestId('cl-cat-ctx'));
    fireEvent.click(screen.getByTestId('catctx-edit'));
    expect(screen.getByTestId('edit-category-modal')).toBeInTheDocument();
  });

  it('shows DeleteCategoryModal when delete category is triggered', () => {
    usePermissionStore.setState({ serverPermissions: { 'server-1': ADMIN_PERMISSIONS } });
    render(<MainView />);
    fireEvent.click(screen.getByTestId('cl-cat-ctx'));
    fireEvent.click(screen.getByTestId('catctx-delete'));
    expect(screen.getByTestId('delete-category-modal')).toBeInTheDocument();
  });

  it('shows CategorySettingsModal when category permissions is triggered', () => {
    usePermissionStore.setState({ serverPermissions: { 'server-1': ADMIN_PERMISSIONS } });
    render(<MainView />);
    fireEvent.click(screen.getByTestId('cl-cat-ctx'));
    fireEvent.click(screen.getByTestId('catctx-perms'));
    expect(screen.getByTestId('category-settings-modal')).toBeInTheDocument();
  });

  it('shows ChannelListContextMenu on empty area context menu (with manage permission)', () => {
    usePermissionStore.setState({ serverPermissions: { 'server-1': ADMIN_PERMISSIONS } });
    render(<MainView />);
    fireEvent.click(screen.getByTestId('cl-empty-ctx'));
    expect(screen.getByTestId('channel-list-context-menu')).toBeInTheDocument();
  });

  // ── renderPrimaryContent helper coverage ────────────────────────────────

  it('renders empty server graphic SVG when server has no channels', () => {
    useChannelStore.setState({ channels: [], activeChannelId: null });
    const { container } = render(<MainView />);
    const svg = container.querySelector('.empty-server-graphic svg');
    expect(svg).toBeInTheDocument();
  });

  it('renders server name and channel selection prompt when no channel selected', () => {
    useChannelStore.setState({ channels: [mockChannel], activeChannelId: null });
    render(<MainView />);
    // Server name may appear multiple times (header + placeholder); just verify the prompt
    expect(screen.getByText('Select a channel to start chatting.')).toBeInTheDocument();
    // The server name should be present at least once
    expect(screen.getAllByText(mockServer.name).length).toBeGreaterThanOrEqual(1);
  });

  it('renders welcome message with privacy tagline when no server', () => {
    useServerStore.setState({ servers: [], activeServerId: null });
    useChannelStore.setState({ channels: [], activeChannelId: null });
    render(<MainView />);
    expect(screen.getByText('Welcome to Concord Voice')).toBeInTheDocument();
    expect(
      screen.getByText('Privacy-first, self-hostable voice communication.')
    ).toBeInTheDocument();
  });

  it('renders main-content with data-has-persistent-bar when in voice viewing text', () => {
    useVoiceStore.setState({
      activeChannelId: 'voice-ch-1',
      connectionState: 'connected',
    });
    useChannelStore.setState({
      channels: [mockChannel],
      activeChannelId: 'channel-1',
    });
    const { container } = render(<MainView />);
    const mainContent = container.querySelector('.main-content');
    expect(mainContent).toHaveAttribute('data-has-persistent-bar');
  });

  it('does not set data-has-persistent-bar when not in voice', () => {
    useVoiceStore.setState({
      activeChannelId: null,
      connectionState: 'disconnected',
    });
    useChannelStore.setState({
      channels: [mockChannel],
      activeChannelId: 'channel-1',
    });
    const { container } = render(<MainView />);
    const mainContent = container.querySelector('.main-content');
    expect(mainContent).not.toHaveAttribute('data-has-persistent-bar');
  });

  // ── Create category modal ─────────────────────────────────────────────

  it('opens CreateCategoryModal from ServerActionBar without errors', () => {
    render(<MainView />);
    // The CreateCategoryModal mock returns null, so observable state is limited
    // to: (a) no React error during render after the state flip, and (b) the
    // click handler itself didn't throw. The console.error spy catches both —
    // React logs to console.error on render errors, and a synchronous throw
    // in the handler would surface there too.
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      fireEvent.click(screen.getByTestId('sab-create-category'));
      expect(errorSpy).not.toHaveBeenCalled();
    } finally {
      errorSpy.mockRestore();
    }
  });

  // ── Settings overlay ───────────────────────────────────────────────────────

  it('opens server settings overlay with serverId when edit server is triggered', () => {
    useSettingsOverlayStore.setState({ open: null, payload: null });
    render(<MainView />);
    fireEvent.click(screen.getByTestId('sb-open-context'));
    fireEvent.click(screen.getByTestId('ctx-edit'));
    expect(useSettingsOverlayStore.getState().open).toBe('server');
    expect(useSettingsOverlayStore.getState().payload?.serverId).toBe('server-1');
  });
});
