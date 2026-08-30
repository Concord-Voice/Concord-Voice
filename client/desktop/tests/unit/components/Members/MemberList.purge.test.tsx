import { act, render, screen, fireEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useMemberStore } from '@/renderer/stores/chat/memberStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { server as mswServer } from '../../../mocks/server';
import { mockUser, mockServer } from '../../../mocks/fixtures';
import {
  MODERATION_API_BASE,
  PURGE_CHECKBOX,
  moderationMember as alice,
  stubBan as stubBanFor,
  stubKick as stubKickFor,
  stubBanNonJsonFailure,
  type ModerationPurgeOutcome,
} from '../../../helpers/moderationPurge';
import { http, HttpResponse } from 'msw';
import MemberList from '@/renderer/components/Members/MemberList';

const MEMBERS_URL = `${MODERATION_API_BASE}/api/v1/servers/${mockServer.id}/members`;

// Dynamic import inside the factory: `vi.mock` is hoisted above the imports, so
// a factory referencing a top-level binding would read it before initialization.
vi.mock('@/renderer/components/Members/MemberContextMenu', async () => {
  const { memberContextMenuDouble } = await import('../../../helpers/moderationPurge');
  return memberContextMenuDouble();
});

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => mswServer.resetHandlers());

const stubBan = (purge?: ModerationPurgeOutcome) => stubBanFor(mockServer.id, purge);
const stubKick = (purge?: ModerationPurgeOutcome) => stubKickFor(mockServer.id, purge);

/** Renders the sidebar with Alice already in the roster and opens one dialog. */
async function renderAndOpen(which: 'ban' | 'kick') {
  render(<MemberList />);
  await screen.findByText('Alice');
  fireEvent.contextMenu(screen.getByText('Alice'));
  fireEvent.click(screen.getByRole('button', { name: `Open ${which} dialog` }));
}

describe('MemberList purge-on-moderation (#1354)', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    useUserStore.setState({ user: mockUser });
    useServerStore.getState().addServer(mockServer);
    useServerStore.getState().setActiveServer(mockServer.id);
    usePermissionStore.setState({ serverPermissions: {}, serverRoles: {} });
    mswServer.use(http.get(MEMBERS_URL, () => HttpResponse.json({ members: [alice] })));
  });

  it('offers an unchecked purge checkbox and no range control on the ban dialog', async () => {
    await renderAndOpen('ban');

    expect(screen.getByRole('checkbox', { name: PURGE_CHECKBOX })).not.toBeChecked();
    // The kick/ban request structs carry only `purge_messages bool` — a range
    // picker here would promise a choice the API does not accept.
    expect(screen.queryAllByRole('radio')).toHaveLength(0);
  });

  it('offers the same unchecked purge checkbox on the kick dialog', async () => {
    await renderAndOpen('kick');

    expect(screen.getByRole('checkbox', { name: PURGE_CHECKBOX })).not.toBeChecked();
  });

  it('reveals the purge helper only once the box is checked', async () => {
    await renderAndOpen('ban');

    const helper =
      /Their messages will be permanently removed from every channel you can moderate\./;
    expect(screen.queryByText(helper)).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    expect(screen.getByText(helper)).toBeInTheDocument();
  });

  it('degrades the confirm label to Ban when unchecked, and never disables it', async () => {
    await renderAndOpen('ban');

    expect(screen.getByRole('button', { name: 'Ban' })).toBeEnabled();

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    expect(screen.getByRole('button', { name: 'Ban and purge' })).toBeEnabled();
  });

  it('sends purge_messages true when the box is checked', async () => {
    const bodies = stubBan({ requested: true, status: 'completed', purged_count: 4 });
    await renderAndOpen('ban');

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ purge_messages: true });
  });

  it('sends purge_messages false when the box is unchecked', async () => {
    const bodies = stubBan();
    await renderAndOpen('ban');

    fireEvent.click(screen.getByRole('button', { name: 'Ban' }));

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ purge_messages: false });
    // No purge was requested, so the server returns no purge fragment and there
    // is nothing to announce.
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
  });

  it('resets the checkbox after the dialog is dismissed', async () => {
    await renderAndOpen('ban');
    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    fireEvent.contextMenu(screen.getByText('Alice'));
    fireEvent.click(screen.getByRole('button', { name: 'Open ban dialog' }));
    expect(screen.getByRole('checkbox', { name: PURGE_CHECKBOX })).not.toBeChecked();
  });

  it('announces the completed purge outcome and still removes the member', async () => {
    stubBan({ requested: true, status: 'completed', purged_count: 9 });
    await renderAndOpen('ban');

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));

    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent(
        'Alice was banned and their messages were purged.'
      )
    );
    // Never render a server-origin count.
    expect(screen.getByRole('status').textContent).not.toContain('9');
    // The primary ban still committed.
    expect(useMemberStore.getState().members.find((m) => m.user_id === 'u1')).toBeUndefined();
  });

  it('announces the unauthorized purge outcome', async () => {
    stubBan({ requested: true, status: 'skipped_unauthorized', purged_count: 0 });
    await renderAndOpen('ban');

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));

    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent(
        'Alice was banned. Their messages were not purged — you do not have permission to purge messages in this server.'
      )
    );
  });

  it('renders the ban success even when the purge was rate-limited', async () => {
    stubBan({ requested: true, status: 'skipped_rate_limited', purged_count: 0 });
    await renderAndOpen('ban');

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));

    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent(
        'Alice was banned. Their messages were not purged — the purge limit was not available just now. You can purge them from a channel later.'
      )
    );
    // The rate-limit budget is operator-tunable, so the copy must not name one.
    expect(screen.getByRole('status').textContent).not.toMatch(/\d/);
  });

  it('announces the failed purge outcome without demoting the ban', async () => {
    stubBan({ requested: true, status: 'failed', purged_count: 0 });
    await renderAndOpen('ban');

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));

    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent(
        'Alice was banned. Their messages could not be purged. You can try again from a channel.'
      )
    );
    // A notice, never an error.
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('sends purge_messages true on the kick path and reports the kick outcome', async () => {
    const bodies = stubKick({ requested: true, status: 'completed', purged_count: 2 });
    await renderAndOpen('kick');

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Kick and purge' }));

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ purge_messages: true });
    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent(
        'Alice was kicked and their messages were purged.'
      )
    );
  });

  it('sends purge_messages false on an unchecked kick', async () => {
    const bodies = stubKick();
    await renderAndOpen('kick');

    fireEvent.click(screen.getByRole('button', { name: 'Kick' }));

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ purge_messages: false });
  });

  it('clears the notice when the active server changes', async () => {
    const otherServer = { ...mockServer, id: 's-other', name: 'Other Server' };
    mswServer.use(
      http.get(`${MODERATION_API_BASE}/api/v1/servers/${otherServer.id}/members`, () =>
        HttpResponse.json({ members: [] })
      )
    );
    stubBan({ requested: true, status: 'completed', purged_count: 3 });
    await renderAndOpen('ban');

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));
    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent(
        'Alice was banned and their messages were purged.'
      )
    );

    // The sidebar derives its server from the store instead of being keyed on
    // it, so this re-renders in place — the notice must not outlive the roster
    // it describes.
    act(() => {
      useServerStore.getState().addServer(otherServer);
      useServerStore.getState().setActiveServer(otherServer.id);
    });

    await waitFor(() => expect(screen.getByRole('status')).toBeEmptyDOMElement());
  });

  it('reports our own failure message when the ban endpoint returns non-JSON', async () => {
    // A proxy HTML 502: `safeJson` throws, and the raw "Expected JSON but got
    // text/html…" must never reach the modal's error surface.
    stubBanNonJsonFailure(mockServer.id);
    await renderAndOpen('ban');

    fireEvent.click(screen.getByRole('button', { name: 'Ban' }));

    expect(await screen.findByText('Ban failed')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('text/html');
  });

  it('leaves the notice empty when the server reports an unknown purge status', async () => {
    stubBan({ requested: true, status: 'something_new', purged_count: 0 });
    await renderAndOpen('ban');

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));

    await waitFor(() =>
      expect(screen.queryByRole('checkbox', { name: PURGE_CHECKBOX })).not.toBeInTheDocument()
    );
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
  });
});
