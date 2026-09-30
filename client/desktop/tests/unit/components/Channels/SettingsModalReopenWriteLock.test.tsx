// Regression for #3406 (Codex review, round 6): closing a channel or category
// settings modal while an override write or a category sync is in flight, then
// reopening it, released the write lock. The lock lived in component state that
// the close destroyed, but the abandoned request could still commit, so a Delete,
// re-save or sync started in the reopened modal raced it on the server.
//
// Oracle: after a close and reopen, every override write, row selection and the
// category-sync switch stay locked until the request the closed modal started
// settles, and only that scope's writes are held.
//
// These drive the REAL store actions over MSW (not mocked actions), because the
// write the reopened modal must respect was started by an instance that no
// longer exists.
import { render, screen, fireEvent, act, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { usePermissionStore, type ChannelOverride } from '@/renderer/stores/chat/permissionStore';
import { useMemberStore } from '@/renderer/stores/chat/memberStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { server } from '../../../mocks/server';
import { http, HttpResponse } from 'msw';
import type { Channel, ChannelGroup } from '@/renderer/types/chat';
import type { Role } from '@/renderer/types/server';

vi.mock('@/renderer/components/Permissions/PermissionGrid', () => ({
  default: ({ mode, disabled: isDisabled }: { mode: string; disabled?: boolean }) => (
    <div data-testid="permission-grid" data-mode={mode} data-disabled={isDisabled} />
  ),
}));

import ChannelSettingsModal from '@/renderer/components/Channels/ChannelSettingsModal';
import CategorySettingsModal from '@/renderer/components/Channels/CategorySettingsModal';

const API_BASE = 'http://localhost:8080';

const channel: Channel = {
  id: 'channel-1',
  server_id: 'server-1',
  name: 'general',
  type: 'text',
  position: 0,
  group_id: 'group-1',
  sync_permissions: false,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const category: ChannelGroup = {
  id: 'cat-1',
  server_id: 'server-1',
  name: 'General',
  position: 0,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const role: Role = {
  id: 'role-1',
  server_id: 'server-1',
  name: 'Moderator',
  color: '#ff0000',
  position: 1,
  permissions: '0',
  is_default: false,
  is_managed: false,
  display_separately: false,
  mentionable: false,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

function override(scopeId: string): ChannelOverride {
  return {
    id: 'override-1',
    channel_id: scopeId,
    target_type: 'role',
    target_id: 'role-1',
    allow: '1',
    deny: '2',
    created_at: '2025-01-01T00:00:00Z',
    updated_at: '2025-01-01T00:00:00Z',
  };
}

/** A network response held until `open()` is called. */
function gate(): { held: Promise<void>; open: () => void } {
  let open!: () => void;
  const held = new Promise<void>((resolve) => {
    open = resolve;
  });
  return { held, open };
}

const channelModal = (isOpen = true) => (
  <ChannelSettingsModal isOpen={isOpen} channel={channel} serverId="server-1" onClose={vi.fn()} />
);
const categoryModal = () => (
  <CategorySettingsModal isOpen category={category} serverId="server-1" onClose={vi.fn()} />
);

function selectButton(): HTMLButtonElement {
  const btn = document.querySelector<HTMLButtonElement>('.override-item-select');
  expect(btn).not.toBeNull();
  return btn as HTMLButtonElement;
}

function expectOverrideWritesLocked() {
  expect(selectButton()).toBeDisabled();
  expect(screen.getByLabelText('Delete override')).toBeDisabled();
  expect(screen.getByLabelText('Override target')).toBeDisabled();
}

function expectOverrideWritesLive() {
  expect(selectButton()).not.toBeDisabled();
  expect(screen.getByLabelText('Delete override')).not.toBeDisabled();
  expect(screen.getByLabelText('Override target')).not.toBeDisabled();
}

describe('settings modals keep the write lock across a close and reopen (#3406)', () => {
  beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
  afterAll(() => server.close());
  afterEach(() => server.resetHandlers());

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    usePermissionStore.setState({
      fetchRoles: vi.fn().mockResolvedValue(true),
      serverRoles: { 'server-1': [role] },
      channelOverrides: {
        'channel-1': [override('channel-1')],
        'category:cat-1': [override('cat-1')],
      },
    });
    useMemberStore.setState({ members: [] });
    server.use(
      http.get(`${API_BASE}/api/v1/channels/channel-1/overrides`, () =>
        HttpResponse.json({ overrides: [override('channel-1')] })
      ),
      http.get(`${API_BASE}/api/v1/categories/cat-1/overrides`, () =>
        HttpResponse.json({ overrides: [override('cat-1')] })
      )
    );
  });

  it('a channel Save in flight when the modal closes still locks the reopened modal', async () => {
    const put = gate();
    server.use(
      http.put(`${API_BASE}/api/v1/channels/channel-1/overrides`, async () => {
        await put.held;
        return HttpResponse.json({});
      })
    );

    const first = render(channelModal());
    fireEvent.click(selectButton());
    act(() => {
      fireEvent.click(screen.getByText('Save Override'));
    });
    first.unmount();

    render(channelModal());
    await act(async () => {});
    expectOverrideWritesLocked();
    expect(screen.getByRole('switch')).toHaveAttribute('aria-disabled', 'true');

    await act(async () => {
      put.open();
    });
    await waitFor(() =>
      expect(screen.getByRole('switch')).not.toHaveAttribute('aria-disabled', 'true')
    );
    expectOverrideWritesLive();
  });

  it('a channel Delete in flight when the modal closes still locks the reopened modal', async () => {
    const del = gate();
    server.use(
      http.delete(`${API_BASE}/api/v1/channels/channel-1/overrides/override-1`, async () => {
        await del.held;
        return new HttpResponse(null, { status: 500 });
      })
    );

    const first = render(channelModal());
    act(() => {
      fireEvent.click(screen.getByLabelText('Delete override'));
    });
    first.unmount();

    render(channelModal());
    await act(async () => {});
    expect(selectButton()).toBeDisabled();
    expect(screen.getByLabelText('Delete override')).toBeDisabled();
    expect(screen.getByRole('switch')).toHaveAttribute('aria-disabled', 'true');

    await act(async () => {
      del.open();
    });
    await waitFor(() =>
      expect(screen.getByRole('switch')).not.toHaveAttribute('aria-disabled', 'true')
    );
    expectOverrideWritesLive();
  });

  it('a category sync in flight when the modal closes still locks the reopened modal', async () => {
    const sync = gate();
    server.use(
      http.put(`${API_BASE}/api/v1/channels/channel-1/permission-sync`, async () => {
        await sync.held;
        return new HttpResponse(null, { status: 500 });
      })
    );

    const first = render(channelModal());
    act(() => {
      fireEvent.click(screen.getByRole('switch'));
    });
    first.unmount();

    render(channelModal());
    await act(async () => {});
    expectOverrideWritesLocked();
    expect(screen.getByRole('switch')).toHaveAttribute('aria-disabled', 'true');

    await act(async () => {
      sync.open();
    });
    await waitFor(() =>
      expect(screen.getByRole('switch')).not.toHaveAttribute('aria-disabled', 'true')
    );
    expectOverrideWritesLive();
  });

  it('a Save in flight still locks the panel when the modal is closed and reopened without unmounting', async () => {
    const put = gate();
    server.use(
      http.put(`${API_BASE}/api/v1/channels/channel-1/overrides`, async () => {
        await put.held;
        return HttpResponse.json({});
      })
    );

    const view = render(channelModal());
    fireEvent.click(selectButton());
    act(() => {
      fireEvent.click(screen.getByText('Save Override'));
    });
    view.rerender(channelModal(false));
    view.rerender(channelModal(true));
    await act(async () => {});
    expectOverrideWritesLocked();
    expect(screen.getByRole('switch')).toHaveAttribute('aria-disabled', 'true');

    await act(async () => {
      put.open();
    });
    await waitFor(() =>
      expect(screen.getByRole('switch')).not.toHaveAttribute('aria-disabled', 'true')
    );
    expectOverrideWritesLive();
  });

  it('a category Save in flight when the modal closes still locks the reopened modal', async () => {
    const put = gate();
    server.use(
      http.put(`${API_BASE}/api/v1/categories/cat-1/overrides`, async () => {
        await put.held;
        return HttpResponse.json({});
      })
    );

    const first = render(categoryModal());
    fireEvent.click(selectButton());
    act(() => {
      fireEvent.click(screen.getByText('Save Override'));
    });
    first.unmount();

    render(categoryModal());
    await act(async () => {});
    expectOverrideWritesLocked();

    await act(async () => {
      put.open();
    });
    await waitFor(() => expect(screen.getByLabelText('Delete override')).not.toBeDisabled());
    expectOverrideWritesLive();
  });

  it('a category Delete in flight when the modal closes still locks the reopened modal', async () => {
    const del = gate();
    server.use(
      http.delete(`${API_BASE}/api/v1/categories/cat-1/overrides/override-1`, async () => {
        await del.held;
        return new HttpResponse(null, { status: 500 });
      })
    );

    const first = render(categoryModal());
    act(() => {
      fireEvent.click(screen.getByLabelText('Delete override'));
    });
    first.unmount();

    render(categoryModal());
    await act(async () => {});
    expectOverrideWritesLocked();

    await act(async () => {
      del.open();
    });
    await waitFor(() => expect(screen.getByLabelText('Delete override')).not.toBeDisabled());
    expectOverrideWritesLive();
  });

  // The panel's own writes are not inherited: only those in flight when it
  // mounted are, so deletes of different rows still overlap in one modal.
  it("a panel's own Delete still lets another row's Delete run alongside it", async () => {
    const second: ChannelOverride = {
      ...override('channel-1'),
      id: 'override-2',
      target_type: 'user',
      target_id: 'user-1',
    };
    usePermissionStore.setState({
      channelOverrides: { 'channel-1': [override('channel-1'), second] },
    });
    const del = gate();
    server.use(
      http.get(`${API_BASE}/api/v1/channels/channel-1/overrides`, () =>
        HttpResponse.json({ overrides: [override('channel-1'), second] })
      ),
      http.delete(`${API_BASE}/api/v1/channels/channel-1/overrides/override-1`, async () => {
        await del.held;
        return new HttpResponse(null, { status: 204 });
      })
    );

    render(channelModal());
    await act(async () => {});
    act(() => {
      fireEvent.click(screen.getAllByLabelText('Delete override')[0]);
    });

    const [deleting, other] = screen.getAllByLabelText('Delete override');
    expect(deleting).toBeDisabled();
    expect(other).not.toBeDisabled();

    await act(async () => {
      del.open();
    });
  });

  // Codex's eighth round: the lock survived a close and reopen, but a sync that
  // succeeded after the close was recorded only by the modal that started it.
  // The server sends no channel update for a sync, so the reopened modal, which
  // opens on a snapshot of the channel, kept showing the old state.
  it('a sync that succeeds after a close and reopen shows in the reopened modal', async () => {
    useChannelStore.setState({ channels: [channel] });
    const sync = gate();
    let overrideReads = 0;
    server.use(
      http.put(`${API_BASE}/api/v1/channels/channel-1/permission-sync`, async () => {
        await sync.held;
        return HttpResponse.json({ sync_permissions: true });
      }),
      http.get(`${API_BASE}/api/v1/channels/channel-1/overrides`, () => {
        overrideReads += 1;
        return HttpResponse.json({ overrides: [override('channel-1')] });
      })
    );
    // As MainView does: the modal opens on the channel as the store holds it then.
    const opened = () => useChannelStore.getState().channels.find((c) => c.id === 'channel-1')!;

    const first = render(
      <ChannelSettingsModal isOpen channel={opened()} serverId="server-1" onClose={vi.fn()} />
    );
    act(() => {
      fireEvent.click(screen.getByRole('switch'));
    });
    first.unmount();

    render(
      <ChannelSettingsModal isOpen channel={opened()} serverId="server-1" onClose={vi.fn()} />
    );
    await act(async () => {});
    expect(screen.getByRole('switch')).toHaveAttribute('aria-checked', 'false');
    const readsBeforeSettle = overrideReads;

    await act(async () => {
      sync.open();
    });
    await waitFor(() => expect(screen.getByRole('switch')).toHaveAttribute('aria-checked', 'true'));
    // Turning sync on replaces the channel's overrides on the server.
    await waitFor(() => expect(overrideReads).toBeGreaterThan(readsBeforeSettle));
  });

  it('a write held on another scope does not lock this one', async () => {
    const put = gate();
    server.use(
      http.put(`${API_BASE}/api/v1/categories/cat-1/overrides`, async () => {
        await put.held;
        return HttpResponse.json({});
      })
    );

    const first = render(categoryModal());
    fireEvent.click(selectButton());
    act(() => {
      fireEvent.click(screen.getByText('Save Override'));
    });
    first.unmount();

    render(channelModal());
    await act(async () => {});
    expectOverrideWritesLive();
    expect(screen.getByRole('switch')).not.toHaveAttribute('aria-disabled', 'true');

    await act(async () => {
      put.open();
    });
  });
});
