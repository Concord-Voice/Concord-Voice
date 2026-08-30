import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { server as mswServer } from '../../../mocks/server';
import { mockServer } from '../../../mocks/fixtures';
import {
  PURGE_CHECKBOX,
  moderationMember,
  stubBan as stubBanFor,
  stubKick as stubKickFor,
  stubBanNonJsonFailure,
  type ModerationPurgeOutcome,
} from '../../../helpers/moderationPurge';
import MemberListPanel from '@/renderer/components/Servers/MemberListPanel';
import LeaveServerModal from '@/renderer/components/Servers/LeaveServerModal';

const SERVER_ID = 's1';

// Dynamic import inside the factory: `vi.mock` is hoisted above the imports, so
// a factory referencing a top-level binding would read it before initialization.
vi.mock('@/renderer/components/Members/MemberContextMenu', async () => {
  const { memberContextMenuDouble } = await import('../../../helpers/moderationPurge');
  return memberContextMenuDouble();
});

const mockMembers = [moderationMember];

const defaultProps = {
  members: mockMembers,
  assignableRoles: [],
  onToggleRole: vi.fn(),
  serverId: SERVER_ID,
  ownerUserId: 'owner-1',
};

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => mswServer.resetHandlers());

function openDialog(which: 'ban' | 'kick') {
  fireEvent.click(screen.getByRole('button', { name: 'Open context menu for Alice' }));
  fireEvent.click(screen.getByRole('button', { name: `Open ${which} dialog` }));
}

const stubBan = (purge?: ModerationPurgeOutcome) => stubBanFor(SERVER_ID, purge);
const stubKick = (purge?: ModerationPurgeOutcome) => stubKickFor(SERVER_ID, purge);

describe('MemberListPanel purge-on-moderation (#1354)', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('offers an unchecked purge checkbox and no range control on the ban dialog', () => {
    render(<MemberListPanel {...defaultProps} />);
    openDialog('ban');

    expect(screen.getByRole('checkbox', { name: PURGE_CHECKBOX })).not.toBeChecked();
    // The kick/ban request structs carry only `purge_messages bool` — a range
    // picker here would promise a choice the API does not accept.
    expect(screen.queryAllByRole('radio')).toHaveLength(0);
  });

  it('reveals the purge helper only once the box is checked', () => {
    render(<MemberListPanel {...defaultProps} />);
    openDialog('ban');

    const helper =
      /Their messages will be permanently removed from every channel you can moderate\./;
    expect(screen.queryByText(helper)).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    expect(screen.getByText(helper)).toBeInTheDocument();
  });

  it('degrades the confirm label to Ban when unchecked, and never disables it', () => {
    render(<MemberListPanel {...defaultProps} />);
    openDialog('ban');

    expect(screen.getByRole('button', { name: 'Ban' })).toBeEnabled();

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    expect(screen.getByRole('button', { name: 'Ban and purge' })).toBeEnabled();
  });

  it('sends purge_messages true when the box is checked', async () => {
    const bodies = stubBan({ requested: true, status: 'completed', purged_count: 4 });
    render(<MemberListPanel {...defaultProps} />);
    openDialog('ban');

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ purge_messages: true });
  });

  it('sends purge_messages false when the box is unchecked', async () => {
    const bodies = stubBan();
    render(<MemberListPanel {...defaultProps} />);
    openDialog('ban');

    fireEvent.click(screen.getByRole('button', { name: 'Ban' }));

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ purge_messages: false });
    // No purge was requested, so the server returns no purge fragment and there
    // is nothing to announce.
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
  });

  it('resets the checkbox after the dialog is dismissed', () => {
    render(<MemberListPanel {...defaultProps} />);
    openDialog('ban');
    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    openDialog('ban');
    expect(screen.getByRole('checkbox', { name: PURGE_CHECKBOX })).not.toBeChecked();
  });

  it('renders the ban success even when the purge was rate-limited', async () => {
    stubBan({ requested: true, status: 'skipped_rate_limited', purged_count: 0 });
    render(<MemberListPanel {...defaultProps} />);
    openDialog('ban');

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));

    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent(
        /Alice was banned\. Their messages were not purged/i
      )
    );
    expect(screen.getByRole('status')).toHaveTextContent(
      'Alice was banned. Their messages were not purged — the purge limit was not available just now. You can purge them from a channel later.'
    );
    // The rate-limit budget is operator-tunable, so the copy must not name one.
    expect(screen.getByRole('status').textContent).not.toMatch(/\d/);
  });

  it('announces the completed purge outcome', async () => {
    stubBan({ requested: true, status: 'completed', purged_count: 9 });
    render(<MemberListPanel {...defaultProps} />);
    openDialog('ban');

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));

    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent(
        'Alice was banned and their messages were purged.'
      )
    );
    // Never render a server-origin count.
    expect(screen.getByRole('status').textContent).not.toContain('9');
  });

  it('announces the unauthorized purge outcome', async () => {
    stubBan({ requested: true, status: 'skipped_unauthorized', purged_count: 0 });
    render(<MemberListPanel {...defaultProps} />);
    openDialog('ban');

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));

    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent(
        'Alice was banned. Their messages were not purged — you do not have permission to purge messages in this server.'
      )
    );
  });

  it('announces the failed purge outcome without demoting the ban', async () => {
    stubBan({ requested: true, status: 'failed', purged_count: 0 });
    render(<MemberListPanel {...defaultProps} />);
    openDialog('ban');

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
    render(<MemberListPanel {...defaultProps} />);
    openDialog('kick');

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

  it('leaves the notice empty when the server reports an unknown purge status', async () => {
    stubBan({ requested: true, status: 'something_new', purged_count: 0 });
    render(<MemberListPanel {...defaultProps} />);
    openDialog('ban');

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));

    await waitFor(() =>
      expect(screen.queryByRole('checkbox', { name: PURGE_CHECKBOX })).not.toBeInTheDocument()
    );
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
  });

  it('clears the notice when the panel is pointed at another server', async () => {
    stubBan({ requested: true, status: 'completed', purged_count: 3 });
    const { rerender } = render(<MemberListPanel {...defaultProps} />);
    openDialog('ban');

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_CHECKBOX }));
    fireEvent.click(screen.getByRole('button', { name: 'Ban and purge' }));
    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent(
        'Alice was banned and their messages were purged.'
      )
    );

    // This panel normally remounts per server, but nothing structurally
    // guarantees it — symmetric with `Members/MemberList`.
    rerender(<MemberListPanel {...defaultProps} serverId="s2" />);

    expect(screen.getByRole('status')).toBeEmptyDOMElement();
  });

  // `moderateMember` throws on a non-OK response and both surfaces depend on
  // that single path for every moderation error, so the panel needs its own
  // coverage — this suite previously stubbed only 2xx responses.
  it('reports our own failure message when the ban endpoint returns non-JSON', async () => {
    stubBanNonJsonFailure(SERVER_ID);
    render(<MemberListPanel {...defaultProps} />);
    openDialog('ban');

    fireEvent.click(screen.getByRole('button', { name: 'Ban' }));

    expect(await screen.findByText('Ban failed')).toBeInTheDocument();
    // The JSON parse error must never reach the user in place of our message.
    expect(document.body.textContent).not.toContain('text/html');
  });
});

describe('LeaveServerModal (#1354)', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('never offers the purge checkbox — self-removal never purges', () => {
    useUserStore.setState({ user: { id: 'user-1', username: 'me', email: 'me@example.com' } });
    render(<LeaveServerModal isOpen server={mockServer} onClose={() => {}} />);

    expect(screen.queryByRole('checkbox', { name: PURGE_CHECKBOX })).not.toBeInTheDocument();
  });
});
