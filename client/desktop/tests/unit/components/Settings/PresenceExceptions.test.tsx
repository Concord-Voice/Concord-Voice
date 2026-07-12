import { act, fireEvent, render, screen, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import PresenceExceptions from '@/renderer/components/Settings/PresenceExceptions';
import { presenceOverrideSyncService } from '@/renderer/services/presenceOverrideSync';
import { useFriendStore, type Friend } from '@/renderer/stores/friendStore';
import { usePresenceOverrideStore } from '@/renderer/stores/presenceOverrideStore';

const UUID_A = '11111111-1111-4111-8111-111111111111';
const UUID_B = '22222222-2222-4222-8222-222222222222';
const UUID_UNKNOWN = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const UUID_UNKNOWN_B = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';

const friend = (userId: string, username: string, displayName?: string): Friend => ({
  id: `friendship-${username}`,
  userId,
  username,
  displayName,
  status: 'online',
});

function renderExceptions() {
  return render(<PresenceExceptions categoryManagerOpen={false} onOpenCategoryManager={vi.fn()} />);
}

function deferred() {
  let resolve: () => void = () => {};
  const promise = new Promise<void>((resolvePromise) => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}

describe('PresenceExceptions', () => {
  beforeEach(() => {
    resetAllStores();
    vi.restoreAllMocks();
    useFriendStore.setState({
      friends: [friend(UUID_A, 'alex', 'Alex Rivera'), friend(UUID_B, 'bea', 'Bea Kim')],
      friendsHydrated: true,
      isLoading: false,
      error: null,
    });
  });

  it('renders exact counts and explicit loading, empty, and populated states', () => {
    const { rerender } = renderExceptions();

    expect(screen.getByText('Exceptions - 0 people')).toBeInTheDocument();
    expect(screen.getByText('No exceptions yet.')).toBeInTheDocument();

    act(() => usePresenceOverrideStore.getState().setLoading(true));
    expect(screen.getByText('Loading exceptions...')).toBeInTheDocument();

    act(() => {
      usePresenceOverrideStore.getState().setLoading(false);
      usePresenceOverrideStore.getState().apply([UUID_A], 1);
    });
    expect(screen.getByText('Exceptions - 1 person')).toBeInTheDocument();
    expect(screen.getByText('Alex Rivera')).toBeInTheDocument();

    act(() => usePresenceOverrideStore.getState().apply([UUID_A, UUID_B], 2));
    rerender(<PresenceExceptions categoryManagerOpen={false} onOpenCategoryManager={vi.fn()} />);
    expect(screen.getByText('Exceptions - 2 people')).toBeInTheDocument();
    expect(screen.getByText('Bea Kim')).toBeInTheDocument();
  });

  it('redacts unknown identifiers while keeping them removable', () => {
    usePresenceOverrideStore.getState().apply([UUID_A, UUID_UNKNOWN], 1);
    renderExceptions();

    expect(screen.getByText('Unavailable person')).toBeInTheDocument();
    expect(document.body).not.toHaveTextContent(UUID_UNKNOWN);
    expect(screen.getByRole('button', { name: 'Remove unavailable person' })).toBeInTheDocument();
  });

  it('waits for an authoritative friend roster before identifying or removing people', async () => {
    const rosterReady = deferred();
    usePresenceOverrideStore.getState().apply([UUID_A], 1);
    useFriendStore.setState({
      friends: [],
      isLoading: false,
      error: null,
      friendsHydrated: false,
    });
    const fetchFriends = vi
      .spyOn(useFriendStore.getState(), 'fetchFriends')
      .mockImplementation(async () => {
        useFriendStore.setState({ isLoading: true });
        await rosterReady.promise;
        useFriendStore.setState({
          friends: [friend(UUID_A, 'alex', 'Alex Rivera')],
          isLoading: false,
          error: null,
          friendsHydrated: true,
        });
      });
    const save = vi.spyOn(presenceOverrideSyncService, 'save');
    renderExceptions();

    const details = screen.getByText('Exceptions - 1 person').closest('details');
    expect(details).not.toBeNull();
    details!.open = true;
    fireEvent(details!, new Event('toggle'));

    await waitFor(() => expect(fetchFriends).toHaveBeenCalledTimes(1));
    expect(screen.queryByText('Unavailable person')).not.toBeInTheDocument();
    expect(screen.getByText('Loading person details...')).toBeInTheDocument();
    const remove = screen.getByRole('button', { name: 'Remove person while details load' });
    expect(remove).toBeDisabled();
    fireEvent.click(remove);
    expect(save).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Add exceptions' })).toBeDisabled();

    await act(async () => rosterReady.resolve());

    await waitFor(() => expect(screen.getByText('Alex Rivera')).toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'Remove Alex Rivera' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Add exceptions' })).toBeEnabled();
  });

  it('keeps unidentified rows neutral and offers retry after friend roster failure', () => {
    usePresenceOverrideStore.getState().apply([UUID_UNKNOWN], 1);
    useFriendStore.setState({
      friends: [],
      friendsHydrated: false,
      isLoading: false,
      error: 'Failed to load friends',
    });
    const fetchFriends = vi
      .spyOn(useFriendStore.getState(), 'fetchFriends')
      .mockResolvedValue(undefined);
    const save = vi.spyOn(presenceOverrideSyncService, 'save');
    renderExceptions();

    expect(screen.queryByText('Unavailable person')).not.toBeInTheDocument();
    expect(screen.getByText('Person details unavailable')).toBeInTheDocument();
    const remove = screen.getByRole('button', {
      name: 'Remove person while details are unavailable',
    });
    expect(remove).toBeDisabled();
    fireEvent.click(remove);
    expect(save).not.toHaveBeenCalled();
    expect(screen.getByRole('alert')).toHaveTextContent(
      'Friend list unavailable. Retry before editing exceptions.'
    );

    fireEvent.click(screen.getByRole('button', { name: 'Retry friend list' }));
    expect(fetchFriends).toHaveBeenCalled();
  });

  it('keeps a previously hydrated roster usable after a refresh failure', () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 1);
    useFriendStore.setState({ error: 'Failed to refresh friends' });
    const fetchFriends = vi
      .spyOn(useFriendStore.getState(), 'fetchFriends')
      .mockResolvedValue(undefined);
    renderExceptions();

    expect(screen.getByText('Alex Rivera')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Remove Alex Rivera' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Add exceptions' })).toBeEnabled();
    expect(screen.getByRole('alert')).toHaveTextContent(
      'Friend list refresh failed. Showing the last loaded list.'
    );

    fireEvent.click(screen.getByRole('button', { name: 'Retry friend list' }));
    expect(fetchFriends).toHaveBeenCalled();
  });

  it('preserves unclicked unavailable exceptions while removing one at a time', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A, UUID_UNKNOWN, UUID_UNKNOWN_B], 3);
    const save = vi.spyOn(presenceOverrideSyncService, 'save').mockImplementation(async (ids) => {
      usePresenceOverrideStore.getState().apply(ids, 4);
    });
    renderExceptions();

    const removeUnavailable = screen.getAllByRole('button', {
      name: 'Remove unavailable person',
    });
    removeUnavailable[0].focus();
    fireEvent.click(removeUnavailable[0]);

    await waitFor(() => expect(save).toHaveBeenCalledWith([UUID_A, UUID_UNKNOWN_B]));
    expect(save).toHaveBeenCalledTimes(1);
    expect(screen.getAllByRole('button', { name: 'Remove unavailable person' })).toHaveLength(1);
    expect(screen.getByText('Alex Rivera')).toBeInTheDocument();
    expect(screen.getByText('Exceptions - 2 people')).toBeInTheDocument();
    expect(document.body).not.toHaveTextContent(UUID_UNKNOWN);
    expect(document.body).not.toHaveTextContent(UUID_UNKNOWN_B);
    expect(screen.getByRole('status')).toHaveTextContent(
      'Removed exception for Unavailable person.'
    );
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Remove unavailable person' })).toHaveFocus()
    );
  });

  it('removes through an explicit encrypted save and announces success', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A, UUID_B], 3);
    const save = vi.spyOn(presenceOverrideSyncService, 'save').mockImplementation(async (ids) => {
      usePresenceOverrideStore.getState().apply(ids, 4);
    });
    renderExceptions();

    const removeAlex = screen.getByRole('button', { name: 'Remove Alex Rivera' });
    removeAlex.focus();
    fireEvent.click(removeAlex);

    await waitFor(() => expect(save).toHaveBeenCalledWith([UUID_B]));
    expect(screen.queryByText('Alex Rivera')).not.toBeInTheDocument();
    expect(screen.getByRole('status')).toHaveTextContent('Removed exception for Alex Rivera.');
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Remove Bea Kim' })).toHaveFocus()
    );
  });

  it('moves focus to Add exceptions after removing the final row', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 3);
    vi.spyOn(presenceOverrideSyncService, 'save').mockImplementation(async (ids) => {
      usePresenceOverrideStore.getState().apply(ids, 4);
    });
    renderExceptions();
    const removeAlex = screen.getByRole('button', { name: 'Remove Alex Rivera' });
    removeAlex.focus();

    fireEvent.click(removeAlex);

    await waitFor(() => expect(screen.queryByText('Alex Rivera')).not.toBeInTheDocument());
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Add exceptions' })).toHaveFocus()
    );
  });

  it('keeps a safe retry snapshot after remove failure and retries only on request', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A, UUID_B], 3);
    const save = vi
      .spyOn(presenceOverrideSyncService, 'save')
      .mockImplementationOnce(async () => {
        usePresenceOverrideStore.getState().setError('Failed to save presence exceptions');
      })
      .mockImplementationOnce(async (ids) => {
        usePresenceOverrideStore.getState().apply(ids, 4);
      });
    renderExceptions();

    fireEvent.click(screen.getByRole('button', { name: 'Remove Alex Rivera' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Failed to save presence exceptions'
    );
    expect(save).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole('button', { name: 'Retry removal' }));

    await waitFor(() => expect(save).toHaveBeenCalledTimes(2));
    expect(save).toHaveBeenLastCalledWith([UUID_B]);
    expect(screen.getByRole('status')).toHaveTextContent('Removed exception for Alex Rivera.');
  });

  it('turns an unexpected rejected removal into a retryable error', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A, UUID_B], 3);
    const save = vi
      .spyOn(presenceOverrideSyncService, 'save')
      .mockRejectedValueOnce(new Error('unexpected transport rejection'))
      .mockImplementationOnce(async (ids) => {
        usePresenceOverrideStore.getState().apply(ids, 4);
      });
    renderExceptions();

    fireEvent.click(screen.getByRole('button', { name: 'Remove Alex Rivera' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Failed to save presence exceptions'
    );
    expect(save).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole('button', { name: 'Retry removal' }));

    await waitFor(() => expect(save).toHaveBeenCalledTimes(2));
    expect(save).toHaveBeenLastCalledWith([UUID_B]);
    expect(screen.getByRole('status')).toHaveTextContent('Removed exception for Alex Rivera.');
  });

  it('clears an older removal retry when opening a new editor intent', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 3);
    vi.spyOn(presenceOverrideSyncService, 'save').mockImplementation(async () => {
      usePresenceOverrideStore.getState().setError('Failed to save presence exceptions');
    });
    renderExceptions();
    fireEvent.click(screen.getByRole('button', { name: 'Remove Alex Rivera' }));
    await screen.findByRole('button', { name: 'Retry removal' });

    fireEvent.click(screen.getByRole('button', { name: 'Add exceptions' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Cancel' }));

    expect(screen.queryByRole('button', { name: 'Retry removal' })).not.toBeInTheDocument();
  });

  it('clears a prior success announcement before a new editor conflict', async () => {
    const save = vi
      .spyOn(presenceOverrideSyncService, 'save')
      .mockImplementationOnce(async (ids) => {
        usePresenceOverrideStore.getState().apply(ids, 1);
      })
      .mockImplementationOnce(async () => {
        usePresenceOverrideStore.getState().apply([UUID_B], 2);
        usePresenceOverrideStore.getState().setConflict(true);
      });
    renderExceptions();

    fireEvent.click(screen.getByRole('button', { name: 'Add exceptions' }));
    fireEvent.click(await screen.findByRole('checkbox', { name: /Alex Rivera/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Save exceptions' }));
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    expect(screen.getByRole('status')).toHaveTextContent('Saved Custom Status exceptions.');

    fireEvent.click(screen.getByRole('button', { name: 'Add exceptions' }));
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
    fireEvent.click(await screen.findByRole('checkbox', { name: /Bea Kim/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Save exceptions' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Exceptions changed on another device. Review the current list and try again.'
    );
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
  });

  it('guards repeated actions while a save is pending', () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 3);
    usePresenceOverrideStore.getState().setSaving(true);
    const save = vi.spyOn(presenceOverrideSyncService, 'save');
    renderExceptions();

    expect(screen.getByRole('button', { name: 'Remove Alex Rivera' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Add exceptions' })).toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: 'Remove Alex Rivera' }));
    expect(save).not.toHaveBeenCalled();
  });

  it('does not open an empty draft while authoritative exceptions are loading', () => {
    usePresenceOverrideStore.getState().setLoading(true);
    renderExceptions();

    expect(screen.getByRole('button', { name: 'Add exceptions' })).toBeDisabled();
  });

  it('discards stale removal intent on conflict and asks for explicit review and retry', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 3);
    vi.spyOn(presenceOverrideSyncService, 'save').mockImplementation(async () => {
      usePresenceOverrideStore.getState().apply([UUID_B], 4);
      usePresenceOverrideStore.getState().setConflict(true);
    });
    renderExceptions();

    const removeAlex = screen.getByRole('button', { name: 'Remove Alex Rivera' });
    removeAlex.focus();
    fireEvent.click(removeAlex);

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Exceptions changed on another device. Review the current list and try again.'
    );
    expect(screen.getByText('Bea Kim')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Retry removal' })).not.toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Remove Bea Kim' })).toHaveFocus()
    );
  });

  it('keeps removal locked and rejects success when a deferred refetch replaces its result', async () => {
    const releaseAuthoritativeRefetch = deferred();
    usePresenceOverrideStore.getState().apply([UUID_A, UUID_B], 3);
    vi.spyOn(presenceOverrideSyncService, 'save').mockImplementation(async (ids) => {
      usePresenceOverrideStore.getState().setSaving(true);
      usePresenceOverrideStore.getState().apply(ids, 4);
      usePresenceOverrideStore.getState().setSaving(false);
      await releaseAuthoritativeRefetch.promise;
      usePresenceOverrideStore.getState().apply([UUID_A], 5);
    });
    renderExceptions();

    fireEvent.click(screen.getByRole('button', { name: 'Remove Alex Rivera' }));
    await act(async () => Promise.resolve());

    expect(screen.getByRole('button', { name: 'Remove Bea Kim' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Add exceptions' })).toBeDisabled();

    await act(async () => releaseAuthoritativeRefetch.resolve());
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Exceptions changed on another device. Review the current list and try again.'
    );
    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [UUID_A],
      appliedVersion: 5,
      conflict: true,
    });
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
    expect(screen.queryByRole('button', { name: 'Retry removal' })).not.toBeInTheDocument();
  });

  it('does not mutate reset state after a cancelled prior-account removal resumes', async () => {
    const pending = deferred();
    usePresenceOverrideStore.getState().apply([UUID_A], 3);
    vi.spyOn(presenceOverrideSyncService, 'save').mockImplementation(async () => {
      await pending.promise;
      return false;
    });
    renderExceptions();
    fireEvent.click(screen.getByRole('button', { name: 'Remove Alex Rivera' }));

    act(() => usePresenceOverrideStore.getState().reset());
    await act(async () => pending.resolve());

    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [],
      appliedVersion: 0,
      saving: false,
      conflict: false,
      error: null,
    });
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
  });

  it('discards a failed removal retry when its authoritative base version changes', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 3);
    const save = vi
      .spyOn(presenceOverrideSyncService, 'save')
      .mockImplementationOnce(async () => {
        usePresenceOverrideStore.getState().setError('Failed to save presence exceptions');
      })
      .mockImplementationOnce(async (ids) => {
        usePresenceOverrideStore.getState().apply(ids, 5);
      });
    renderExceptions();
    fireEvent.click(screen.getByRole('button', { name: 'Remove Alex Rivera' }));
    await screen.findByRole('button', { name: 'Retry removal' });

    act(() => {
      usePresenceOverrideStore.getState().apply([UUID_B], 4);
      usePresenceOverrideStore.getState().setError('Failed to save presence exceptions');
    });
    fireEvent.click(screen.getByRole('button', { name: 'Retry removal' }));

    await waitFor(() => expect(usePresenceOverrideStore.getState().conflict).toBe(true));
    expect(save).toHaveBeenCalledTimes(1);
    expect(usePresenceOverrideStore.getState().excludedUserIds).toEqual([UUID_B]);
    expect(screen.getByRole('alert')).toHaveTextContent(
      'Exceptions changed on another device. Review the current list and try again.'
    );
  });

  it('returns focus to Add exceptions when the editor closes', async () => {
    renderExceptions();
    const add = screen.getByRole('button', { name: 'Add exceptions' });

    fireEvent.click(add);
    expect(
      await screen.findByRole('dialog', { name: 'Custom Status exceptions' })
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    await waitFor(() => expect(add).toHaveFocus());
  });
});
