import { render, screen, fireEvent, act } from '../../../test-utils';
import { renderHook } from '@testing-library/react';
import { mockChannel, mockServer, mockServer2, mockUser } from '../../../mocks/fixtures';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { MANAGE_SERVER, BASE_PERMISSIONS } from '@/renderer/utils/policy/permissions';
import ServerContextMenu from '@/renderer/components/Servers/ServerContextMenu';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { useUnreadStore } from '@/renderer/stores/chat/unreadStore';
import {
  isServerUnreadVisible,
  useNotificationPrefsStore,
} from '@/renderer/stores/ui/notificationPrefsStore';
import { apiFetch } from '@/renderer/services/system/apiClient';
import { useServerChannelSubscriptions } from '@/renderer/hooks/messaging/useServerChannelSubscriptions';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({ ok: true }),
}));
vi.mock('@/renderer/services/messaging/websocketService', () => ({
  getWebSocketService: () => ({ subscribeServer: vi.fn(), unsubscribeServer: vi.fn() }),
}));

const SERVER_ID = 'server-1';
const OWNER_ID = 'user-1'; // matches mockServer.owner_id
const OTHER_ID = 'user-2';
// MANAGE_SERVER controls "Server Settings" visibility
const OWNER_PERMS = MANAGE_SERVER | BASE_PERMISSIONS;

describe('ServerContextMenu', () => {
  const mockOnClose = vi.fn();
  const mockOnEditServer = vi.fn();
  const mockOnDeleteServer = vi.fn();
  const mockOnLeaveServer = vi.fn();
  const mockOnInvite = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    // Default: owner-level permissions and identity
    usePermissionStore.setState({
      serverPermissions: { [SERVER_ID]: OWNER_PERMS },
    });
    useUserStore.setState({ user: { ...mockUser, id: OWNER_ID } });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  const renderMenu = (serverOverrides = {}) => {
    const server = { ...mockServer, ...serverOverrides };
    return render(
      <ServerContextMenu
        server={server}
        position={{ x: 100, y: 100 }}
        onClose={mockOnClose}
        onEditServer={mockOnEditServer}
        onDeleteServer={mockOnDeleteServer}
        onLeaveServer={mockOnLeaveServer}
        onInvite={mockOnInvite}
      />
    );
  };

  it('renders server name and role', () => {
    renderMenu();
    expect(screen.getByText('Test Server')).toBeInTheDocument();
    expect(screen.getByText('owner')).toBeInTheDocument();
  });

  it('shows Mark All as Read', () => {
    renderMenu();
    expect(screen.getByText('Mark All as Read')).toBeInTheDocument();
  });

  it('shows Server Settings for owner', () => {
    // beforeEach sets owner identity + MANAGE_SERVER perm
    renderMenu();
    expect(screen.getByText('Server Settings')).toBeInTheDocument();
  });

  it('shows Server Settings for admin', () => {
    // Admin: non-owner identity but has MANAGE_SERVER perm
    useUserStore.setState({ user: { ...mockUser, id: OTHER_ID } });
    renderMenu({ role: 'admin' });
    expect(screen.getByText('Server Settings')).toBeInTheDocument();
  });

  it('hides Server Settings for member', () => {
    // Member: non-owner identity, no MANAGE_SERVER perm
    useUserStore.setState({ user: { ...mockUser, id: OTHER_ID } });
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS } });
    renderMenu({ role: 'member' });
    expect(screen.queryByText('Server Settings')).not.toBeInTheDocument();
  });

  it('shows Delete Server for owner', () => {
    // beforeEach sets user-1 = owner
    renderMenu();
    expect(screen.getByText('Delete Server')).toBeInTheDocument();
    expect(screen.queryByText('Leave Server')).not.toBeInTheDocument();
  });

  it('shows Leave Server for member (not Delete)', () => {
    useUserStore.setState({ user: { ...mockUser, id: OTHER_ID } });
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS } });
    renderMenu({ role: 'member' });
    expect(screen.getByText('Leave Server')).toBeInTheDocument();
    expect(screen.queryByText('Delete Server')).not.toBeInTheDocument();
  });

  it('shows Invite to Server for all roles', () => {
    renderMenu();
    expect(screen.getByText('Invite to Server')).toBeInTheDocument();
  });

  it('calls onEditServer when Server Settings clicked', () => {
    renderMenu();
    fireEvent.click(screen.getByText('Server Settings'));
    expect(mockOnEditServer).toHaveBeenCalledWith(expect.objectContaining({ id: 'server-1' }));
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('calls onDeleteServer when Delete Server clicked', () => {
    renderMenu();
    fireEvent.click(screen.getByText('Delete Server'));
    expect(mockOnDeleteServer).toHaveBeenCalledWith(expect.objectContaining({ id: 'server-1' }));
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('calls onLeaveServer when Leave Server clicked', () => {
    useUserStore.setState({ user: { ...mockUser, id: OTHER_ID } });
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS } });
    renderMenu({ role: 'member' });
    fireEvent.click(screen.getByText('Leave Server'));
    expect(mockOnLeaveServer).toHaveBeenCalledWith(expect.objectContaining({ id: 'server-1' }));
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('calls onInvite when Invite to Server clicked', () => {
    renderMenu();
    fireEvent.click(screen.getByText('Invite to Server'));
    expect(mockOnInvite).toHaveBeenCalledWith(expect.objectContaining({ id: 'server-1' }));
    expect(mockOnClose).toHaveBeenCalled();
  });
});

describe('ServerContextMenu caller regression (#3773)', () => {
  beforeEach(() => {
    useUnreadStore.setState(useUnreadStore.getInitialState(), true);
    useChannelStore.setState(useChannelStore.getInitialState(), true);
    resetAllStores();
    vi.clearAllMocks();
    vi.mocked(apiFetch).mockReset();
    useAuthStore.getState().setAccessToken('test-caller-unread-token');
    useChatStore.setState({ isConnected: true });
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: OWNER_PERMS } });
    useUserStore.setState({ user: { ...mockUser, id: OWNER_ID } });
  });

  describe('per-server seam (#3773)', () => {
    it.each([
      { awaitBoundary: 'response', order: 'per-server-first' },
      { awaitBoundary: 'response', order: 'bulk-first' },
      { awaitBoundary: 'JSON body', order: 'per-server-first' },
      { awaitBoundary: 'JSON body', order: 'bulk-first' },
    ])(
      'rejects the pre-read $awaitBoundary in $order order and admits a fresh A request',
      async ({ awaitBoundary, order }) => {
        function deferred<T>() {
          let resolve!: (value: T) => void;
          const promise = new Promise<T>((fulfill) => {
            resolve = fulfill;
          });
          return { promise, resolve };
        }
        useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: SERVER_ID });
        useChannelStore.setState({
          channels: [mockChannel, { ...mockChannel, id: 'channel-a-other' }],
          activeChannelId: null,
        });
        useUnreadStore.getState().setInitialUnreads(new Map([['channel-b1', 2]]), 'server-2');
        useUnreadStore.getState().setInitialChannelUnreads([
          { channelId: 'channel-1', serverId: SERVER_ID, count: 1 },
          { channelId: 'channel-a-other', serverId: SERVER_ID, count: 3 },
          { channelId: 'channel-b1', serverId: 'server-2', count: 2 },
        ]);
        for (const channelId of ['channel-1', 'channel-a-other', 'channel-b1']) {
          useUnreadStore.getState().incrementMention(channelId);
        }
        for (const serverId of [SERVER_ID, 'server-2']) {
          useUnreadStore.getState().markServerUnread(serverId, true);
          useUnreadStore.getState().markServerMention(serverId);
        }
        const bulk = deferred<Response>();
        const oldA = deferred<Response>();
        const oldBody = deferred<{
          unreads: Array<{ channel_id: string; unread_count: number }>;
        }>();
        const freshA = deferred<Response>();
        const serverB = deferred<Response>();
        const oldResponse = new Response('{}');
        const parseOldBody = vi
          .spyOn(oldResponse, 'json')
          .mockImplementation(() => oldBody.promise);
        const oldUnreads = {
          unreads: [
            { channel_id: 'channel-1', unread_count: 1 },
            { channel_id: 'channel-a-other', unread_count: 3 },
          ],
        };
        let aRequests = 0;
        vi.mocked(apiFetch).mockImplementation((path) => {
          if (path === '/api/v1/servers/unread-status') return bulk.promise;
          if (path === '/api/v1/servers/server-1/unread') {
            aRequests += 1;
            return aRequests === 1 ? oldA.promise : freshA.promise;
          }
          if (path === '/api/v1/servers/server-2/unread') return serverB.promise;
          if (path === '/api/v1/servers/server-1/read') return Promise.resolve(new Response('{}'));
          throw new Error(`Unexpected per-server seam endpoint: ${path}`);
        });
        const hook = renderHook(() => useServerChannelSubscriptions());
        let view: ReturnType<typeof render> | undefined;
        const visible = (serverId: string) => {
          const state = useUnreadStore.getState();
          return isServerUnreadVisible(
            serverId,
            state.serverUnreadSet,
            state.serverUnreadPreciseSet,
            useNotificationPrefsStore.getState().mutedServers
          );
        };
        try {
          expect(aRequests).toBe(1);
          if (awaitBoundary === 'JSON body') {
            await act(async () => {
              oldA.resolve(oldResponse);
            });
            expect(
              parseOldBody,
              'the old owning request must enter its JSON await before the whole-server read'
            ).toHaveBeenCalledTimes(1);
          }
          expect(useChannelStore.getState().activeChannelId).toBeNull();
          expect(visible(SERVER_ID)).toBe(true);
          expect(visible('server-2')).toBe(true);
          view = render(
            <ServerContextMenu
              server={mockServer}
              position={{ x: 100, y: 100 }}
              onClose={vi.fn()}
              onEditServer={vi.fn()}
              onDeleteServer={vi.fn()}
              onLeaveServer={vi.fn()}
              onInvite={vi.fn()}
            />
          );
          fireEvent.click(screen.getByText('Mark All as Read'));
          for (const channelId of ['channel-1', 'channel-a-other']) {
            expect(useUnreadStore.getState().allUnreadCounts.has(channelId)).toBe(false);
            expect(useUnreadStore.getState().mentionCounts.has(channelId)).toBe(false);
          }
          expect(visible(SERVER_ID)).toBe(false);
          expect(useUnreadStore.getState().serverMentionSet.has(SERVER_ID)).toBe(false);
          expect(useUnreadStore.getState().unreadCounts.get('channel-b1')).toBe(2);
          expect(useUnreadStore.getState().allUnreadCounts.get('channel-b1')).toEqual({
            serverId: 'server-2',
            count: 2,
          });
          expect(useUnreadStore.getState().mentionCounts.get('channel-b1')).toBe(1);
          expect(visible('server-2')).toBe(true);
          expect(
            vi.mocked(apiFetch).mock.calls.filter(([, options]) => options?.method === 'POST')
          ).toEqual([['/api/v1/servers/server-1/read', { method: 'POST' }]]);

          let bulkCompleted = false;
          for (const request of order === 'per-server-first'
            ? ['per-server', 'bulk']
            : ['bulk', 'per-server']) {
            await act(async () => {
              if (request === 'per-server') {
                oldA.resolve(oldResponse);
                oldBody.resolve(oldUnreads);
              } else {
                bulk.resolve(
                  new Response(
                    JSON.stringify({
                      server_ids: [SERVER_ID, 'server-2'],
                      channels: [
                        { channel_id: 'channel-1', server_id: SERVER_ID, unread_count: 1 },
                        { channel_id: 'channel-a-other', server_id: SERVER_ID, unread_count: 3 },
                        { channel_id: 'channel-b1', server_id: 'server-2', unread_count: 2 },
                        { channel_id: 'channel-b-control', server_id: 'server-2', unread_count: 4 },
                      ],
                    })
                  )
                );
              }
            });
            if (request === 'bulk') bulkCompleted = true;
            expect
              .soft(
                useUnreadStore.getState().unreadCounts.get('channel-1'),
                'an owning A request begun before Mark All as Read must never restore A1’s old count'
              )
              .toBeUndefined();
            expect
              .soft(
                useUnreadStore.getState().unreadCounts.get('channel-a-other'),
                'an owning A request begun before Mark All as Read must never restore A2’s old count'
              )
              .toBeUndefined();
            expect
              .soft(
                visible(SERVER_ID),
                'an owning A request begun before Mark All as Read must never relight A’s unread dot'
              )
              .toBe(false);
            for (const channelId of ['channel-1', 'channel-a-other']) {
              expect(useUnreadStore.getState().allUnreadCounts.has(channelId)).toBe(false);
              expect(useUnreadStore.getState().mentionCounts.has(channelId)).toBe(false);
            }
            expect(useUnreadStore.getState().serverMentionSet.has(SERVER_ID)).toBe(false);
            expect(useUnreadStore.getState().allUnreadCounts.get('channel-b1')).toEqual({
              serverId: 'server-2',
              count: 2,
            });
            expect(visible('server-2')).toBe(true);
            if (bulkCompleted) {
              expect(
                useUnreadStore.getState().allUnreadCounts.get('channel-b-control'),
                'the mounted bulk request must commit untouched B’s unique positive row in either response order'
              ).toEqual({ serverId: 'server-2', count: 4 });
            }
          }

          // The same hook must admit a genuinely newer owning request. Retiring
          // every per-server response would hide the stale reply but fail this.
          act(() => {
            useServerStore.setState({ activeServerId: 'server-2' });
          });
          await act(async () => {
            serverB.resolve(
              new Response(
                JSON.stringify({ unreads: [{ channel_id: 'channel-b1', unread_count: 2 }] })
              )
            );
          });
          expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-2');
          expect(useUnreadStore.getState().unreadCounts.get('channel-b1')).toBe(2);
          act(() => {
            useServerStore.setState({ activeServerId: SERVER_ID });
          });
          expect(
            aRequests,
            'navigation must start a second A GET after the whole-server read'
          ).toBe(2);
          await act(async () => {
            freshA.resolve(
              new Response(
                JSON.stringify({
                  unreads: [{ channel_id: 'channel-a-current-control', unread_count: 5 }],
                })
              )
            );
          });
          expect(useUnreadStore.getState().unreadCountsServerId).toBe(SERVER_ID);
          expect(
            useUnreadStore.getState().unreadCounts.get('channel-a-current-control'),
            'an owning A request begun after Mark All as Read must commit its unique fresh count'
          ).toBe(5);
          expect(
            visible(SERVER_ID),
            'a valid after-read A response must light A’s unread dot'
          ).toBe(true);
          expect(useUnreadStore.getState().unreadCounts.has('channel-1')).toBe(false);
          expect(useUnreadStore.getState().unreadCounts.has('channel-a-other')).toBe(false);
          expect(useUnreadStore.getState().allUnreadCounts.get('channel-b1')).toEqual({
            serverId: 'server-2',
            count: 2,
          });
          expect(useUnreadStore.getState().allUnreadCounts.get('channel-b-control')).toEqual({
            serverId: 'server-2',
            count: 4,
          });
          expect(visible('server-2')).toBe(true);
          expect(useUnreadStore.getState().mentionCounts.get('channel-b1')).toBe(1);
          expect(
            vi
              .mocked(apiFetch)
              .mock.calls.filter(([path]) => path === '/api/v1/servers/server-1/unread')
          ).toHaveLength(2);
          expect(
            vi
              .mocked(apiFetch)
              .mock.calls.filter(([path]) => path === '/api/v1/servers/unread-status')
          ).toHaveLength(1);
        } finally {
          view?.unmount();
          hook.unmount();
          await act(async () => {
            oldA.resolve(oldResponse);
            oldBody.resolve(oldUnreads);
            bulk.resolve(new Response('{}'));
            serverB.resolve(new Response('{}'));
            freshA.resolve(new Response('{}'));
          });
          parseOldBody.mockRestore();
        }
      }
    );
  });

  it.each([false, true])(
    'marks only selected A read and preserves B while the mounted bulk seed completes (#4266192107; fresh A: %s)',
    async (freshArrival) => {
      useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: SERVER_ID });
      useChannelStore.setState({
        channels: [mockChannel, { ...mockChannel, id: 'channel-a-other' }],
        activeChannelId: null,
      });
      useUnreadStore.getState().setInitialChannelUnreads([
        { channelId: 'channel-1', serverId: SERVER_ID, count: 1 },
        { channelId: 'channel-a-other', serverId: SERVER_ID, count: 2 },
        { channelId: 'channel-b1', serverId: 'server-2', count: 2 },
      ]);
      for (const channelId of ['channel-1', 'channel-a-other', 'channel-b1']) {
        useUnreadStore.getState().incrementMention(channelId);
      }
      for (const serverId of [SERVER_ID, 'server-2']) {
        useUnreadStore.getState().markServerUnread(serverId, true);
        useUnreadStore.getState().markServerMention(serverId);
      }
      let resolveBulk!: (response: Response) => void;
      let resolveA!: (response: Response) => void;
      const bulk = new Promise<Response>((resolve) => {
        resolveBulk = resolve;
      });
      const serverA = new Promise<Response>((resolve) => {
        resolveA = resolve;
      });
      vi.mocked(apiFetch).mockImplementation((path) => {
        if (path === '/api/v1/servers/unread-status') return bulk;
        if (path === '/api/v1/servers/server-1/unread') return serverA;
        if (path === '/api/v1/servers/server-1/read') return Promise.resolve(new Response('{}'));
        throw new Error(`Unexpected caller endpoint: ${path}`);
      });
      const hook = renderHook(() => useServerChannelSubscriptions());
      let view: ReturnType<typeof render> | undefined;
      const visible = (serverId: string) => {
        const state = useUnreadStore.getState();
        return isServerUnreadVisible(
          serverId,
          state.serverUnreadSet,
          state.serverUnreadPreciseSet,
          useNotificationPrefsStore.getState().mutedServers
        );
      };
      try {
        await act(async () => {
          resolveA(
            new Response(
              JSON.stringify({
                unreads: [
                  { channel_id: 'channel-1', unread_count: 1 },
                  { channel_id: 'channel-a-other', unread_count: 2 },
                ],
              })
            )
          );
        });
        expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(1);
        expect(useUnreadStore.getState().unreadCounts.get('channel-a-other')).toBe(2);
        expect(visible('server-2')).toBe(true);
        expect(useUnreadStore.getState().mentionCounts.get('channel-b1')).toBe(1);
        expect(useUnreadStore.getState().serverMentionSet.has('server-2')).toBe(true);
        expect(useUnreadStore.getState().allUnreadCounts.has('channel-a-unknown')).toBe(false);
        expect(useUnreadStore.getState().allUnreadCounts.has('channel-a-fresh')).toBe(false);
        view = render(
          <ServerContextMenu
            server={mockServer}
            position={{ x: 100, y: 100 }}
            onClose={vi.fn()}
            onEditServer={vi.fn()}
            onDeleteServer={vi.fn()}
            onLeaveServer={vi.fn()}
            onInvite={vi.fn()}
          />
        );
        fireEvent.click(screen.getByText('Mark All as Read'));

        for (const channelId of ['channel-1', 'channel-a-other']) {
          expect(useUnreadStore.getState().unreadCounts.has(channelId)).toBe(false);
          expect(useUnreadStore.getState().allUnreadCounts.has(channelId)).toBe(false);
          expect(useUnreadStore.getState().mentionCounts.has(channelId)).toBe(false);
        }
        expect(visible(SERVER_ID)).toBe(false);
        expect(useUnreadStore.getState().serverMentionSet.has(SERVER_ID)).toBe(false);
        expect(
          vi.mocked(apiFetch).mock.calls.filter(([, options]) => options?.method === 'POST')
        ).toEqual([['/api/v1/servers/server-1/read', { method: 'POST' }]]);
        expect
          .soft(
            useUnreadStore.getState().allUnreadCounts.get('channel-b1'),
            'Mark All as Read for A must preserve B’s existing unread count immediately'
          )
          .toEqual({ serverId: 'server-2', count: 2 });
        expect
          .soft(
            visible('server-2'),
            'Mark All as Read for A must preserve B’s unread dot immediately'
          )
          .toBe(true);
        expect
          .soft(
            useUnreadStore.getState().mentionCounts.get('channel-b1'),
            'Mark All as Read for A must preserve B’s channel mentions immediately'
          )
          .toBe(1);
        expect
          .soft(
            useUnreadStore.getState().serverMentionSet.has('server-2'),
            'Mark All as Read for A must preserve B’s server mention immediately'
          )
          .toBe(true);

        if (freshArrival) {
          act(() => {
            // The active-server live path updates both count maps, then marks
            // the concrete unmuted channel's server. This arrives after the read.
            useUnreadStore.getState().incrementUnread('channel-a-fresh');
            useUnreadStore.getState().bumpChannelUnread('channel-a-fresh', SERVER_ID);
            useUnreadStore.getState().markServerUnread(SERVER_ID, true);
          });
          expect(useUnreadStore.getState().allUnreadCounts.get('channel-a-fresh')).toEqual({
            serverId: SERVER_ID,
            count: 1,
          });
          expect(useUnreadStore.getState().unreadCounts.get('channel-a-fresh')).toBe(1);
          expect(visible(SERVER_ID)).toBe(true);
        }

        await act(async () => {
          resolveBulk(
            new Response(
              JSON.stringify({
                server_ids: [SERVER_ID, 'server-2'],
                channels: [
                  { channel_id: 'channel-1', server_id: SERVER_ID, unread_count: 1 },
                  { channel_id: 'channel-a-other', server_id: SERVER_ID, unread_count: 2 },
                  { channel_id: 'channel-a-unknown', server_id: SERVER_ID, unread_count: 5 },
                  { channel_id: 'channel-b1', server_id: 'server-2', unread_count: 2 },
                  { channel_id: 'channel-b-control', server_id: 'server-2', unread_count: 4 },
                ],
              })
            )
          );
        });
        expect(useUnreadStore.getState().allUnreadCounts.has('channel-1')).toBe(false);
        expect(useUnreadStore.getState().allUnreadCounts.has('channel-a-other')).toBe(false);
        expect(
          useUnreadStore.getState().allUnreadCounts.has('channel-a-unknown'),
          'Mark All as Read for A must reject an older unread A row first revealed by the bulk response'
        ).toBe(false);
        expect(
          visible(SERVER_ID),
          'A’s server read must suppress old rows while preserving only a genuine after-read A arrival'
        ).toBe(freshArrival);
        if (freshArrival) {
          expect(
            useUnreadStore.getState().allUnreadCounts.get('channel-a-fresh'),
            'a genuine after-read A arrival must survive the older bulk snapshot'
          ).toEqual({ serverId: SERVER_ID, count: 1 });
          expect(useUnreadStore.getState().unreadCounts.get('channel-a-fresh')).toBe(1);
        }
        expect(useUnreadStore.getState().serverMentionSet.has(SERVER_ID)).toBe(false);
        expect
          .soft(
            useUnreadStore.getState().allUnreadCounts.get('channel-b1'),
            'A’s server read must keep B’s genuine count after the old bulk response'
          )
          .toEqual({ serverId: 'server-2', count: 2 });
        expect
          .soft(
            useUnreadStore.getState().allUnreadCounts.get('channel-b-control'),
            'A’s server read must leave the mounted bulk request able to seed untouched B’s new detail'
          )
          .toEqual({ serverId: 'server-2', count: 4 });
        expect
          .soft(
            visible('server-2'),
            'A’s server read must keep B’s visible dot after the old bulk response'
          )
          .toBe(true);
        expect
          .soft(
            useUnreadStore.getState().mentionCounts.get('channel-b1'),
            'A’s server read must preserve B’s channel mentions after the old bulk response'
          )
          .toBe(1);
        expect
          .soft(
            useUnreadStore.getState().serverMentionSet.has('server-2'),
            'A’s server read must preserve B’s server mention after the old bulk response'
          )
          .toBe(true);
        expect(
          vi
            .mocked(apiFetch)
            .mock.calls.filter(([path]) => path === '/api/v1/servers/unread-status')
        ).toHaveLength(1);
      } finally {
        view?.unmount();
        hook.unmount();
        await act(async () => {
          resolveBulk(new Response('{}'));
          resolveA(new Response('{}'));
        });
      }
    }
  );
});
