import React, { useRef, useState } from 'react';
import { act, fireEvent, render, screen, userEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import PresenceExceptionModal from '@/renderer/components/Settings/PresenceExceptionModal';
import { presenceOverrideSyncService } from '@/renderer/services/presenceOverrideSync';
import { useFriendOrgStore } from '@/renderer/stores/chat/friendOrgStore';
import { useFriendStore, type Friend } from '@/renderer/stores/chat/friendStore';
import { usePresenceOverrideStore } from '@/renderer/stores/ui/presenceOverrideStore';
import { deferred } from '../../../helpers/deferred';

const UUID_A = '11111111-1111-4111-8111-111111111111';
const UUID_B = '22222222-2222-4222-8222-222222222222';
const UUID_C = '33333333-3333-4333-8333-333333333333';
const UUID_UNKNOWN = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const PRIVATE_SENTINEL = 'sentinel-private-identifier';

const friend = (userId: string, username: string, displayName?: string): Friend => ({
  id: `friendship-${username}`,
  userId,
  username,
  displayName,
  status: 'online',
});

interface HarnessProps {
  onSaved?: (message: string) => void;
  onOpenCategoryManager?: () => void;
}

function ModalHarness({ onSaved = vi.fn(), onOpenCategoryManager = vi.fn() }: HarnessProps) {
  const [open, setOpen] = useState(true);
  const addButtonRef = useRef<HTMLButtonElement>(null);
  return (
    <>
      <button ref={addButtonRef} type="button">
        Add exceptions
      </button>
      {open && (
        <PresenceExceptionModal
          returnFocusRef={addButtonRef}
          onDismiss={() => setOpen(false)}
          onSaved={onSaved}
          onOpenCategoryManager={onOpenCategoryManager}
        />
      )}
    </>
  );
}

describe('PresenceExceptionModal', () => {
  const originalShowModal = HTMLDialogElement.prototype.showModal;
  const originalClose = HTMLDialogElement.prototype.close;
  let showModalCalls = 0;

  beforeAll(() => {
    HTMLDialogElement.prototype.showModal = function showModal() {
      showModalCalls += 1;
      this.setAttribute('open', '');
    };
    HTMLDialogElement.prototype.close = function close() {
      this.removeAttribute('open');
      this.dispatchEvent(new Event('close'));
    };
  });

  afterAll(() => {
    HTMLDialogElement.prototype.showModal = originalShowModal;
    HTMLDialogElement.prototype.close = originalClose;
  });

  beforeEach(() => {
    resetAllStores();
    vi.restoreAllMocks();
    showModalCalls = 0;
    useFriendStore.setState({
      friends: [
        friend(UUID_A, 'alex', 'Alex Rivera'),
        friend(UUID_B, 'bea', 'Bea Kim'),
        friend(UUID_C, 'cora', 'Cora Lin'),
      ],
    });
  });

  it('selects a friend with the keyboard when there are no categories', async () => {
    const user = userEvent.setup();
    const save = vi.spyOn(presenceOverrideSyncService, 'save').mockImplementation(async (ids) => {
      usePresenceOverrideStore.getState().apply(ids, 1);
    });
    render(<ModalHarness />);

    expect(screen.getByText('You have no friend categories yet.')).toBeInTheDocument();
    const checkbox = screen.getByRole('checkbox', { name: /Bea Kim/ });
    await waitFor(() =>
      expect(screen.getByRole('searchbox', { name: 'Search friends and categories' })).toHaveFocus()
    );
    checkbox.focus();
    await user.keyboard('[Space]');
    expect(checkbox).toBeChecked();
    fireEvent.click(screen.getByRole('button', { name: 'Save exceptions' }));

    await waitFor(() => expect(save).toHaveBeenCalledWith([UUID_B]));
  });

  it('searches category names and friend display names or usernames', () => {
    useFriendOrgStore.getState()._hydrate({
      v: 1,
      categories: [
        {
          id: 'cat_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
          name: 'Gaming crew',
          emoji: '',
          color: null,
          memberIds: [UUID_A],
        },
      ],
      sectionOrder: [],
    });
    render(<ModalHarness />);

    fireEvent.change(screen.getByRole('searchbox', { name: 'Search friends and categories' }), {
      target: { value: 'gaming' },
    });
    expect(screen.getByRole('checkbox', { name: /Gaming crew/ })).toBeInTheDocument();
    expect(screen.queryByRole('checkbox', { name: /Bea Kim/ })).not.toBeInTheDocument();

    fireEvent.change(screen.getByRole('searchbox', { name: 'Search friends and categories' }), {
      target: { value: 'bea' },
    });
    expect(screen.getByRole('checkbox', { name: /Bea Kim/ })).toBeInTheDocument();
    expect(screen.queryByRole('checkbox', { name: /Gaming crew/ })).not.toBeInTheDocument();
  });

  it('includes visible usernames and category counts in checkbox names', () => {
    useFriendStore.setState({
      friends: [friend(UUID_A, 'alex', 'Shared name'), friend(UUID_B, 'bea', 'Shared name')],
    });
    useFriendOrgStore.getState()._hydrate({
      v: 1,
      categories: [
        {
          id: 'cat_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
          name: 'Close friends',
          emoji: '',
          color: null,
          memberIds: [UUID_A, UUID_B],
        },
      ],
      sectionOrder: [],
    });
    render(<ModalHarness />);

    expect(screen.getByRole('checkbox', { name: 'Shared name @alex' })).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'Shared name @bea' })).toBeInTheDocument();
    expect(
      screen.getByRole('checkbox', { name: 'Close friends 2 current members' })
    ).toBeInTheDocument();
  });

  it('exposes a partially selected category as mixed', () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 1);
    useFriendOrgStore.getState()._hydrate({
      v: 1,
      categories: [
        {
          id: 'cat_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
          name: 'Close friends',
          emoji: '',
          color: null,
          memberIds: [UUID_A, UUID_B],
        },
      ],
      sectionOrder: [],
    });
    render(<ModalHarness />);

    const category = screen.getByRole('checkbox', {
      name: 'Close friends 2 current members',
    });
    expect(category).toBePartiallyChecked();

    fireEvent.click(category);

    expect(category).toBeChecked();
    expect(screen.getByText('2 people selected')).toBeInTheDocument();
  });

  it('snapshots category members, deduplicates overlap, and saves in stable order', async () => {
    useFriendOrgStore.getState()._hydrate({
      v: 1,
      categories: [
        {
          id: 'cat_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
          name: 'Close friends',
          emoji: '',
          color: null,
          memberIds: [UUID_B, UUID_A],
        },
      ],
      sectionOrder: [],
    });
    const save = vi.spyOn(presenceOverrideSyncService, 'save').mockImplementation(async (ids) => {
      usePresenceOverrideStore.getState().apply(ids, 1);
    });
    render(<ModalHarness />);

    fireEvent.click(screen.getByRole('checkbox', { name: /Alex Rivera/ }));
    fireEvent.click(screen.getByRole('checkbox', { name: /Close friends/ }));

    act(() => {
      useFriendOrgStore.getState()._hydrate({
        v: 1,
        categories: [
          {
            id: 'cat_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
            name: 'Close friends',
            emoji: '',
            color: null,
            memberIds: [UUID_A, UUID_C],
          },
        ],
        sectionOrder: [],
      });
    });

    expect(screen.getByText(/Category selections add current members/)).toHaveTextContent(
      'Category selections add current members. Later category changes do not update saved exceptions automatically.'
    );

    fireEvent.click(screen.getByRole('button', { name: 'Save exceptions' }));

    await waitFor(() => expect(save).toHaveBeenCalledWith([UUID_A, UUID_B]));
  });

  it('intersects category snapshots with current friends and never saves stale members', async () => {
    useFriendOrgStore.getState()._hydrate({
      v: 1,
      categories: [
        {
          id: 'cat_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
          name: 'Close friends',
          emoji: '',
          color: null,
          memberIds: [UUID_A, UUID_UNKNOWN],
        },
      ],
      sectionOrder: [],
    });
    const save = vi.spyOn(presenceOverrideSyncService, 'save').mockImplementation(async (ids) => {
      usePresenceOverrideStore.getState().apply(ids, 1);
    });
    render(<ModalHarness />);

    expect(screen.getByText('1 current member')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('checkbox', { name: /Close friends/ }));
    expect(screen.getByText('1 person selected')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Save exceptions' }));

    await waitFor(() => expect(save).toHaveBeenCalledWith([UUID_A]));
    expect(save).not.toHaveBeenCalledWith(expect.arrayContaining([UUID_UNKNOWN]));
  });

  it('never renders malformed or unknown identifiers', () => {
    usePresenceOverrideStore.getState().apply([UUID_UNKNOWN, PRIVATE_SENTINEL], 2);
    render(<ModalHarness />);

    expect(screen.getByText('2 unavailable people remain selected.')).toBeInTheDocument();
    expect(document.body).not.toHaveTextContent(UUID_UNKNOWN);
    expect(document.body).not.toHaveTextContent(PRIVATE_SENTINEL);
  });

  it('uses showModal, handles native cancel and backdrop dismissal, and returns focus', async () => {
    render(<ModalHarness />);
    const dialog = screen.getByRole('dialog', { name: 'Custom Status exceptions' });
    expect(showModalCalls).toBe(1);
    expect(dialog).toHaveAttribute('open');

    fireEvent(dialog, new Event('cancel', { bubbles: true, cancelable: true }));

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'Add exceptions' })).toHaveFocus();

    render(<ModalHarness />);
    const backdrop = screen.getByRole('dialog', { name: 'Custom Status exceptions' });
    fireEvent.click(backdrop);
    await waitFor(() => expect(backdrop).not.toBeInTheDocument());
  });

  it('uses the jsdom Escape fallback and returns focus', async () => {
    render(<ModalHarness />);
    fireEvent.keyDown(document, { key: 'Escape' });

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'Add exceptions' })).toHaveFocus();
  });

  it('prevents Escape, cancel, and backdrop dismissal while saving', () => {
    usePresenceOverrideStore.getState().setSaving(true);
    render(<ModalHarness />);
    const dialog = screen.getByRole('dialog', { name: 'Custom Status exceptions' });

    fireEvent.keyDown(document, { key: 'Escape' });
    fireEvent(dialog, new Event('cancel', { bubbles: true, cancelable: true }));
    fireEvent.click(dialog);

    expect(dialog).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Save exceptions' })).toBeDisabled();
  });

  it('does not interpret a same-render second activation as save success', async () => {
    const pending = deferred();
    let callCount = 0;
    const save = vi.spyOn(presenceOverrideSyncService, 'save').mockImplementation(async (ids) => {
      callCount += 1;
      if (callCount > 1) return;
      usePresenceOverrideStore.getState().setSaving(true);
      await pending.promise;
      usePresenceOverrideStore.getState().apply(ids, 1);
      usePresenceOverrideStore.getState().setSaving(false);
    });
    render(<ModalHarness />);
    fireEvent.click(screen.getByRole('checkbox', { name: /Alex Rivera/ }));
    const saveButton = screen.getByRole('button', { name: 'Save exceptions' });

    act(() => {
      saveButton.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      saveButton.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    });
    await act(async () => Promise.resolve());

    expect(save).toHaveBeenCalledTimes(1);
    expect(screen.getByRole('dialog', { name: 'Custom Status exceptions' })).toBeInTheDocument();

    await act(async () => pending.resolve());
    await waitFor(() =>
      expect(
        screen.queryByRole('dialog', { name: 'Custom Status exceptions' })
      ).not.toBeInTheDocument()
    );
  });

  it('keeps every interaction locked until the save promise settles', async () => {
    const pending = deferred();
    const openCategoryManager = vi.fn();
    vi.spyOn(presenceOverrideSyncService, 'save').mockImplementation(async (ids) => {
      await pending.promise;
      usePresenceOverrideStore.getState().apply(ids, 1);
    });
    render(<ModalHarness onOpenCategoryManager={openCategoryManager} />);
    fireEvent.click(screen.getByRole('checkbox', { name: /Alex Rivera/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Save exceptions' }));
    await act(async () => Promise.resolve());

    expect(screen.getByRole('searchbox', { name: 'Search friends and categories' })).toBeDisabled();
    expect(screen.getByRole('checkbox', { name: /Alex Rivera/ })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Manage categories' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Save exceptions' })).toBeDisabled();

    fireEvent.click(screen.getByRole('button', { name: 'Manage categories' }));
    fireEvent.keyDown(document, { key: 'Escape' });
    const dialog = screen.getByRole('dialog', { name: 'Custom Status exceptions' });
    fireEvent.click(dialog);
    expect(openCategoryManager).not.toHaveBeenCalled();
    expect(dialog).toBeInTheDocument();

    await act(async () => pending.resolve());
    await waitFor(() => expect(dialog).not.toBeInTheDocument());
  });

  it('does not report success when a deferred authoritative refetch replaces the draft', async () => {
    const releaseAuthoritativeRefetch = deferred();
    const onSaved = vi.fn();
    vi.spyOn(presenceOverrideSyncService, 'save').mockImplementation(async (ids) => {
      usePresenceOverrideStore.getState().setSaving(true);
      usePresenceOverrideStore.getState().apply(ids, 1);
      // Task 4 lowers `saving` before awaiting a deferred-version refetch.
      usePresenceOverrideStore.getState().setSaving(false);
      await releaseAuthoritativeRefetch.promise;
      usePresenceOverrideStore.getState().apply([UUID_C], 2);
    });
    render(<ModalHarness onSaved={onSaved} />);
    fireEvent.click(screen.getByRole('checkbox', { name: /Alex Rivera/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Save exceptions' }));
    await act(async () => Promise.resolve());

    expect(screen.getByRole('button', { name: 'Save exceptions' })).toBeDisabled();

    await act(async () => releaseAuthoritativeRefetch.resolve());
    await waitFor(() =>
      expect(
        screen.queryByRole('dialog', { name: 'Custom Status exceptions' })
      ).not.toBeInTheDocument()
    );
    expect(onSaved).not.toHaveBeenCalled();
    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [UUID_C],
      appliedVersion: 2,
      conflict: true,
    });
  });

  it('does not mutate reset state after a cancelled prior-account save resumes', async () => {
    const pending = deferred();
    const onSaved = vi.fn();
    vi.spyOn(presenceOverrideSyncService, 'save').mockImplementation(async () => {
      await pending.promise;
      return false;
    });
    render(<ModalHarness onSaved={onSaved} />);
    fireEvent.click(screen.getByRole('checkbox', { name: /Alex Rivera/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Save exceptions' }));

    act(() => usePresenceOverrideStore.getState().reset());
    await act(async () => pending.resolve());

    expect(onSaved).not.toHaveBeenCalled();
    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [],
      appliedVersion: 0,
      saving: false,
      conflict: false,
      error: null,
    });
  });

  it('keeps the draft open with a safe alert after an ordinary save error', async () => {
    const save = vi
      .spyOn(presenceOverrideSyncService, 'save')
      .mockImplementationOnce(async () => {
        usePresenceOverrideStore.getState().setError('Failed to save presence exceptions');
      })
      .mockImplementationOnce(async (ids) => {
        usePresenceOverrideStore.getState().apply(ids, 1);
      });
    render(<ModalHarness />);
    fireEvent.click(screen.getByRole('checkbox', { name: /Alex Rivera/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Save exceptions' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Failed to save presence exceptions. Review your selections and try again.'
    );
    expect(screen.getByRole('dialog', { name: 'Custom Status exceptions' })).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: /Alex Rivera/ })).toBeChecked();

    fireEvent.click(screen.getByRole('button', { name: 'Save exceptions' }));
    await waitFor(() => expect(save).toHaveBeenCalledTimes(2));
    expect(
      screen.queryByRole('dialog', { name: 'Custom Status exceptions' })
    ).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Add exceptions' })).toHaveFocus();
  });

  it('closes and discards its stale draft after a conflict', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 3);
    const dismiss = vi.fn();
    vi.spyOn(presenceOverrideSyncService, 'save').mockImplementation(async () => {
      usePresenceOverrideStore.getState().apply([UUID_C], 4);
      usePresenceOverrideStore.getState().setConflict(true);
    });
    const addButtonRef = React.createRef<HTMLButtonElement>();
    render(
      <>
        <button ref={addButtonRef}>Add exceptions</button>
        <PresenceExceptionModal
          returnFocusRef={addButtonRef}
          onDismiss={dismiss}
          onSaved={vi.fn()}
          onOpenCategoryManager={vi.fn()}
        />
      </>
    );

    fireEvent.click(screen.getByRole('checkbox', { name: /Bea Kim/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Save exceptions' }));

    await waitFor(() => expect(dismiss).toHaveBeenCalledTimes(1));
    expect(usePresenceOverrideStore.getState().excludedUserIds).toEqual([UUID_C]);
  });

  it('discards a draft locally when its authoritative base version changes', async () => {
    usePresenceOverrideStore.getState().apply([UUID_A], 3);
    const save = vi.spyOn(presenceOverrideSyncService, 'save').mockResolvedValue();
    render(<ModalHarness />);
    fireEvent.click(screen.getByRole('checkbox', { name: /Bea Kim/ }));

    act(() => usePresenceOverrideStore.getState().apply([UUID_C], 4));
    fireEvent.click(screen.getByRole('button', { name: 'Save exceptions' }));

    await waitFor(() =>
      expect(
        screen.queryByRole('dialog', { name: 'Custom Status exceptions' })
      ).not.toBeInTheDocument()
    );
    expect(save).not.toHaveBeenCalled();
    expect(usePresenceOverrideStore.getState()).toMatchObject({
      excludedUserIds: [UUID_C],
      appliedVersion: 4,
      conflict: true,
    });
  });
});
