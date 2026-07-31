import { act, fireEvent } from '@testing-library/react';
import { render, screen, userEvent, waitFor, within } from '../../../test-utils';
import ActivityHistoryCard from '@/renderer/components/Settings/ActivityHistoryCard';
import {
  deletePresenceHistory,
  getPresenceHistorySettings,
  patchPresenceHistorySettings,
  PresenceHistoryRequestError,
  type PresenceHistorySettings,
} from '@/renderer/services/presenceHistoryService';
import { clientConfigService } from '@/renderer/services/clientConfigService';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useClientConfigStore } from '@/renderer/stores/clientConfigStore';
import { useSettingsNavStore } from '@/renderer/stores/settingsNavStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useUserStore } from '@/renderer/stores/userStore';
import { vi } from 'vitest';

vi.mock('@/renderer/services/presenceHistoryService', async (importOriginal) => {
  const actual =
    await importOriginal<typeof import('@/renderer/services/presenceHistoryService')>();
  return {
    ...actual,
    getPresenceHistorySettings: vi.fn(),
    patchPresenceHistorySettings: vi.fn(),
    deletePresenceHistory: vi.fn(),
  };
});

vi.mock('@/renderer/services/clientConfigService', () => ({
  clientConfigService: {
    refreshServerCapabilities: vi.fn().mockResolvedValue(undefined),
  },
}));

const FIRST_HASH = 'a'.repeat(64);
const UPDATED_HASH = 'b'.repeat(64);
const DISCLOSURE = {
  version: 4,
  copyHash: FIRST_HASH,
  operatorName: 'Concord Voice, Inc.',
  requiredText: 'Activity History stores future Custom Status changes on this server.',
  details: ['No backfill occurs.', 'Expired records leave the API immediately.'],
  privacyPolicyUrl: 'https://concordvoice.com/privacy#activity-history',
  acknowledgementLabel:
    'I understand and consent to server-readable Activity History under the terms above.',
};
const UPDATED_DISCLOSURE = {
  ...DISCLOSURE,
  version: 5,
  copyHash: UPDATED_HASH,
  requiredText: 'Updated Activity History terms apply to future Custom Status changes.',
};

const OFF_SETTINGS: PresenceHistorySettings = {
  available: true,
  enabled: false,
  reconsentRequired: false,
  retentionDays: 30,
  consentVersion: null,
  consentCopyHash: null,
  consentedAt: null,
  requiredConsent: DISCLOSURE,
};
const ON_SETTINGS: PresenceHistorySettings = {
  ...OFF_SETTINGS,
  enabled: true,
  consentVersion: DISCLOSURE.version,
  consentCopyHash: DISCLOSURE.copyHash,
  consentedAt: '2026-07-12T14:00:00Z',
};
const PAUSED_SETTINGS: PresenceHistorySettings = {
  ...OFF_SETTINGS,
  reconsentRequired: true,
};
const UPDATED_PAUSED_SETTINGS: PresenceHistorySettings = {
  ...PAUSED_SETTINGS,
  requiredConsent: UPDATED_DISCLOSURE,
};
const UNAVAILABLE_SETTINGS: PresenceHistorySettings = {
  ...OFF_SETTINGS,
  available: false,
  requiredConsent: null,
};

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

function setOwner(userId = 'user-a', sessionId = 'session-a'): void {
  useUserStore.setState({
    user: { id: userId, username: userId },
    isLoading: false,
    error: null,
  });
  useAuthStore.setState({
    accessToken: `token-${userId}`,
    sessionId,
  });
}

function setCapability(
  capability:
    | { status: 'loading' }
    | { status: 'supported' }
    | { status: 'confirmed-unsupported' }
    | { status: 'error'; lastConfirmedSupported: boolean }
): void {
  useClientConfigStore.setState({ activityHistoryCapability: capability });
}

describe('ActivityHistoryCard', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    setOwner();
    setCapability({ status: 'supported' });
    useSettingsNavStore.setState({ focusRequest: null });
    vi.mocked(getPresenceHistorySettings).mockResolvedValue(OFF_SETTINGS);
    vi.mocked(patchPresenceHistorySettings).mockResolvedValue(OFF_SETTINGS);
    vi.mocked(deletePresenceHistory).mockResolvedValue(undefined);
  });

  it('shows labeled capability loading without guessing Off', () => {
    setCapability({ status: 'loading' });
    render(<ActivityHistoryCard />);

    expect(screen.getByRole('heading', { name: 'Activity History' })).toBeInTheDocument();
    expect(screen.getByText('Checking Activity History availability…')).toBeInTheDocument();
    expect(screen.queryByText('Off')).not.toBeInTheDocument();
    expect(getPresenceHistorySettings).not.toHaveBeenCalled();
  });

  it('keeps validated Off settings visible but blocks opt-in when support is absent', async () => {
    setCapability({ status: 'confirmed-unsupported' });
    render(<ActivityHistoryCard />);

    await waitFor(() => expect(getPresenceHistorySettings).toHaveBeenCalledTimes(1));
    expect(await screen.findByText('Off')).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Activity History' })).toBeDisabled();
    expect(screen.getByText(/new opt-ins and retention increases are paused/i)).toBeInTheDocument();
  });

  it('keeps the card hidden when an old server has no self-settings route', async () => {
    vi.mocked(getPresenceHistorySettings).mockRejectedValue(
      new PresenceHistoryRequestError(404, 'not_found', null)
    );
    setCapability({ status: 'confirmed-unsupported' });
    const { container } = render(<ActivityHistoryCard />);

    await waitFor(() => expect(getPresenceHistorySettings).toHaveBeenCalledTimes(1));
    expect(container).toBeEmptyDOMElement();
  });

  it('shows a retry surface for a transient gate-false settings failure', async () => {
    const user = userEvent.setup();
    vi.mocked(getPresenceHistorySettings)
      .mockRejectedValueOnce(new PresenceHistoryRequestError(500, 'internal_error', null))
      .mockResolvedValueOnce(OFF_SETTINGS);
    setCapability({ status: 'confirmed-unsupported' });
    render(<ActivityHistoryCard />);

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Activity History settings could not be refreshed.'
    );
    await user.click(screen.getByRole('button', { name: 'Retry settings' }));
    expect(await screen.findByText('Off')).toBeInTheDocument();
  });

  it('keeps existing-user view and deletion controls available during a gate-false drain', async () => {
    const user = userEvent.setup();
    vi.mocked(getPresenceHistorySettings).mockResolvedValue(ON_SETTINGS);
    setCapability({ status: 'confirmed-unsupported' });
    render(<ActivityHistoryCard />);

    expect(await screen.findByText('On')).toBeInTheDocument();
    expect(screen.getByText(/new opt-ins and retention increases are paused/i)).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Activity History' })).toBeChecked();
    expect(
      within(screen.getByRole('combobox', { name: 'Retention period' })).getByRole('option', {
        name: '7 days',
      })
    ).toBeEnabled();
    expect(
      within(screen.getByRole('combobox', { name: 'Retention period' })).getByRole('option', {
        name: '90 days',
      })
    ).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Delete history and turn off' })).toBeEnabled();

    await user.click(screen.getByRole('button', { name: 'View history' }));
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'privacy',
      controlId: 'presence-history-heading',
    });
  });

  it('keeps confirmed existing-user controls when a supported instance enters a drain', async () => {
    vi.mocked(getPresenceHistorySettings).mockResolvedValue(ON_SETTINGS);
    render(<ActivityHistoryCard />);

    expect(await screen.findByText('On')).toBeInTheDocument();
    act(() => setCapability({ status: 'confirmed-unsupported' }));

    expect(screen.getByText(/new opt-ins and retention increases are paused/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'View history' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Delete history and turn off' })).toBeEnabled();
  });

  it('refetches drain settings when capability support is activated', async () => {
    vi.mocked(getPresenceHistorySettings)
      .mockResolvedValueOnce(ON_SETTINGS)
      .mockResolvedValueOnce({ ...ON_SETTINGS, retentionDays: 90 });
    setCapability({ status: 'confirmed-unsupported' });
    render(<ActivityHistoryCard />);

    expect(await screen.findByRole('combobox', { name: 'Retention period' })).toHaveValue('30');
    act(() => setCapability({ status: 'supported' }));

    await waitFor(() =>
      expect(screen.getByRole('combobox', { name: 'Retention period' })).toHaveValue('90')
    );
    expect(getPresenceHistorySettings).toHaveBeenCalledTimes(2);
  });

  it('defers a capability refetch until an active retention mutation settles', async () => {
    const user = userEvent.setup();
    const pendingMutation = deferred<PresenceHistorySettings>();
    vi.mocked(getPresenceHistorySettings)
      .mockResolvedValueOnce(ON_SETTINGS)
      .mockResolvedValueOnce({ ...ON_SETTINGS, retentionDays: 90 });
    vi.mocked(patchPresenceHistorySettings).mockReturnValue(pendingMutation.promise);
    render(<ActivityHistoryCard />);

    await user.selectOptions(
      await screen.findByRole('combobox', { name: 'Retention period' }),
      '90'
    );
    act(() => setCapability({ status: 'confirmed-unsupported' }));
    expect(getPresenceHistorySettings).toHaveBeenCalledTimes(1);

    pendingMutation.resolve({ ...ON_SETTINGS, retentionDays: 90 });
    await waitFor(() => expect(getPresenceHistorySettings).toHaveBeenCalledTimes(2));
    expect(screen.getByRole('combobox', { name: 'Retention period' })).toHaveValue('90');
    expect(screen.getByRole('button', { name: 'Delete history and turn off' })).toBeEnabled();
  });

  it('hides confirmed settings when a drain probe proves the routes are absent', async () => {
    vi.mocked(getPresenceHistorySettings)
      .mockResolvedValueOnce(ON_SETTINGS)
      .mockRejectedValueOnce(new PresenceHistoryRequestError(404, 'not_found', null));
    const { container } = render(<ActivityHistoryCard />);

    expect(await screen.findByText('On')).toBeInTheDocument();
    act(() => setCapability({ status: 'confirmed-unsupported' }));

    await waitFor(() => expect(container).toBeEmptyDOMElement());
    expect(getPresenceHistorySettings).toHaveBeenCalledTimes(2);
  });

  it('restarts the initial settings request after StrictMode effect replay', async () => {
    const signals: AbortSignal[] = [];
    vi.mocked(getPresenceHistorySettings).mockImplementation((signal) => {
      signals.push(signal);
      return Promise.resolve(OFF_SETTINGS);
    });

    render(<ActivityHistoryCard />, { reactStrictMode: true });

    expect(await screen.findByText('Off')).toBeInTheDocument();
    expect(getPresenceHistorySettings).toHaveBeenCalledTimes(2);
    expect(signals[0]?.aborted).toBe(true);
    expect(signals[1]?.aborted).toBe(false);
  });

  it('shows a named capability error and retries discovery without guessing state', async () => {
    const user = userEvent.setup();
    setCapability({ status: 'error', lastConfirmedSupported: false });
    render(<ActivityHistoryCard />);

    expect(
      screen.getByText('Activity History availability could not be confirmed.')
    ).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent(
      'Activity History availability could not be confirmed.'
    );
    expect(screen.queryByText('Off')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Retry availability check' }));
    expect(clientConfigService.refreshServerCapabilities).toHaveBeenCalledTimes(1);
  });

  it('renders confirmed Off only after validated settings load and keeps retention inactive', async () => {
    render(<ActivityHistoryCard />);

    expect(await screen.findByText('Off')).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Activity History' })).not.toBeChecked();
    expect(screen.getByRole('combobox', { name: 'Retention period' })).toHaveValue('30');
    expect(screen.getByRole('combobox', { name: 'Retention period' })).toBeDisabled();
  });

  it('renders confirmed On with enabled controls and navigates to self history', async () => {
    const user = userEvent.setup();
    vi.mocked(getPresenceHistorySettings).mockResolvedValue(ON_SETTINGS);
    render(<ActivityHistoryCard />);

    expect(await screen.findByText('On')).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Activity History' })).toBeChecked();
    expect(screen.getByRole('combobox', { name: 'Retention period' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Delete history and turn off' })).toBeEnabled();

    await user.click(screen.getByRole('button', { name: 'View history' }));
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'privacy',
      controlId: 'presence-history-heading',
    });
  });

  it('renders operator-disclosure unavailability without an enable switch', async () => {
    vi.mocked(getPresenceHistorySettings).mockResolvedValue(UNAVAILABLE_SETTINGS);
    render(<ActivityHistoryCard />);

    expect(await screen.findByText('Unavailable')).toBeInTheDocument();
    expect(
      screen.getByText(
        'This instance operator has not configured the disclosure required for Activity History.'
      )
    ).toBeInTheDocument();
    expect(screen.queryByRole('switch', { name: 'Activity History' })).not.toBeInTheDocument();
  });

  it('renders re-consent as Paused with review, history, deletion, and narrowing controls', async () => {
    vi.mocked(getPresenceHistorySettings).mockResolvedValue(PAUSED_SETTINGS);
    render(<ActivityHistoryCard />);

    expect(await screen.findByText('Paused')).toBeInTheDocument();
    expect(
      screen.getByText('Recording is paused because the Activity History terms changed.')
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Review updated terms' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'View history' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Delete history and turn off' })).toBeEnabled();
    expect(screen.queryByRole('switch', { name: 'Activity History' })).not.toBeInTheDocument();
    expect(
      within(screen.getByRole('combobox', { name: 'Retention period' })).getByRole('option', {
        name: '90 days',
      })
    ).toBeDisabled();
  });

  it('opens consent from Off and submits the exact selection without optimistic On state', async () => {
    const user = userEvent.setup();
    const pendingMutation = deferred<PresenceHistorySettings>();
    vi.mocked(patchPresenceHistorySettings).mockReturnValue(pendingMutation.promise);
    render(<ActivityHistoryCard />);
    await screen.findByText('Off');

    await user.click(screen.getByRole('switch', { name: 'Activity History' }));
    expect(screen.getByRole('dialog', { name: 'Enable Activity History' })).toBeInTheDocument();
    await user.selectOptions(
      screen.getByRole('combobox', { name: 'Keep Activity History for' }),
      '90'
    );
    await user.click(screen.getByRole('checkbox', { name: DISCLOSURE.acknowledgementLabel }));
    await user.click(screen.getByRole('button', { name: 'Enable Activity History' }));

    expect(patchPresenceHistorySettings).toHaveBeenCalledWith(
      {
        kind: 'enable',
        retentionDays: 90,
        consentVersion: DISCLOSURE.version,
        consentCopyHash: DISCLOSURE.copyHash,
      },
      expect.any(AbortSignal)
    );
    expect(screen.getByText('Off')).toBeInTheDocument();

    pendingMutation.resolve({ ...ON_SETTINGS, retentionDays: 90 });
    await waitFor(() => expect(screen.getByText('On')).toBeInTheDocument());
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('focuses the persistent heading after successful re-consent removes its invoker', async () => {
    const user = userEvent.setup();
    vi.mocked(getPresenceHistorySettings).mockResolvedValue(PAUSED_SETTINGS);
    vi.mocked(patchPresenceHistorySettings).mockResolvedValue(ON_SETTINGS);
    render(<ActivityHistoryCard />);

    await user.click(await screen.findByRole('button', { name: 'Review updated terms' }));
    await user.click(screen.getByRole('checkbox', { name: DISCLOSURE.acknowledgementLabel }));
    await user.click(screen.getByRole('button', { name: 'Accept updated terms' }));

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.getByRole('heading', { name: 'Activity History' })).toHaveFocus();
  });

  it('closes enable consent and blocks its stale submit when capability leaves supported', async () => {
    const user = userEvent.setup();
    render(<ActivityHistoryCard />);
    await screen.findByText('Off');

    const enableSwitch = screen.getByRole('switch', { name: 'Activity History' });
    await user.click(enableSwitch);
    await user.click(screen.getByRole('checkbox', { name: DISCLOSURE.acknowledgementLabel }));
    const submit = screen.getByRole('button', { name: 'Enable Activity History' });

    act(() => {
      setCapability({ status: 'loading' });
    });

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    fireEvent.click(submit);
    expect(patchPresenceHistorySettings).not.toHaveBeenCalled();
    expect(
      screen.getByText(
        'Activity History availability changed. Enabling and retention increases are paused until support is confirmed.'
      )
    ).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: 'Activity History' })).toHaveFocus()
    );
  });

  it('closes re-consent and blocks a longer stale selection when support becomes stale', async () => {
    const user = userEvent.setup();
    vi.mocked(getPresenceHistorySettings).mockResolvedValue(PAUSED_SETTINGS);
    render(<ActivityHistoryCard />);
    await screen.findByText('Paused');

    await user.click(screen.getByRole('button', { name: 'Review updated terms' }));
    await user.selectOptions(
      screen.getByRole('combobox', { name: 'Keep Activity History for' }),
      '90'
    );
    await user.click(screen.getByRole('checkbox', { name: DISCLOSURE.acknowledgementLabel }));
    const submit = screen.getByRole('button', { name: 'Accept updated terms' });

    act(() => {
      setCapability({ status: 'error', lastConfirmedSupported: true });
      fireEvent.click(submit);
    });

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(patchPresenceHistorySettings).not.toHaveBeenCalled();
    expect(
      screen.getByText(
        'Activity History availability changed. Enabling and retention increases are paused until support is confirmed.'
      )
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Delete history and turn off' })).toBeEnabled();
    const retention = screen.getByRole('combobox', { name: 'Retention period' });
    expect(within(retention).getByRole('option', { name: '7 days' })).toBeEnabled();
    expect(within(retention).getByRole('option', { name: '90 days' })).toBeDisabled();
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: 'Activity History' })).toHaveFocus()
    );
  });

  it('does not reopen invalidated consent when support recovers before deferred cleanup', async () => {
    const user = userEvent.setup();
    const pendingMutation = deferred<PresenceHistorySettings>();
    let mutationSignal: AbortSignal | undefined;
    vi.mocked(patchPresenceHistorySettings).mockImplementation((_request, signal) => {
      mutationSignal = signal;
      return pendingMutation.promise;
    });
    render(<ActivityHistoryCard />);
    await screen.findByText('Off');

    await user.click(screen.getByRole('switch', { name: 'Activity History' }));
    await user.click(screen.getByRole('checkbox', { name: DISCLOSURE.acknowledgementLabel }));
    await user.click(screen.getByRole('button', { name: 'Enable Activity History' }));

    const queuedMicrotasks: VoidFunction[] = [];
    const queueMicrotaskSpy = vi
      .spyOn(globalThis, 'queueMicrotask')
      .mockImplementation((callback) => {
        queuedMicrotasks.push(callback);
      });

    try {
      act(() => {
        setCapability({ status: 'loading' });
      });
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(mutationSignal?.aborted).toBe(true);

      act(() => {
        setCapability({ status: 'supported' });
      });
      act(() => {
        const pendingCallbacks = queuedMicrotasks.splice(0);
        pendingCallbacks.forEach((callback) => callback());
      });

      await screen.findByText('Off');
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      const enableSwitch = screen.getByRole('switch', { name: 'Activity History' });
      expect(enableSwitch).toBeEnabled();

      await user.click(enableSwitch);
      expect(screen.getByRole('dialog', { name: 'Enable Activity History' })).toBeInTheDocument();
      expect(patchPresenceHistorySettings).toHaveBeenCalledTimes(1);
    } finally {
      queueMicrotaskSpy.mockRestore();
      await act(async () => {
        pendingMutation.resolve(ON_SETTINGS);
        await Promise.resolve();
      });
    }
  });

  it('applies a retention increase directly and confirms a decrease naming the cutoff', async () => {
    const user = userEvent.setup();
    vi.mocked(getPresenceHistorySettings).mockResolvedValue(ON_SETTINGS);
    vi.mocked(patchPresenceHistorySettings)
      .mockResolvedValueOnce({ ...ON_SETTINGS, retentionDays: 90 })
      .mockResolvedValueOnce({ ...ON_SETTINGS, retentionDays: 7 });
    render(<ActivityHistoryCard />);
    const retention = await screen.findByRole('combobox', { name: 'Retention period' });

    await user.selectOptions(retention, '90');
    await waitFor(() =>
      expect(patchPresenceHistorySettings).toHaveBeenCalledWith(
        { kind: 'retention', retentionDays: 90 },
        expect.any(AbortSignal)
      )
    );
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();

    await user.selectOptions(retention, '7');
    const dialog = screen.getByRole('dialog', { name: 'Shorten Activity History retention?' });
    expect(within(dialog).getByText(/older than 7 days/i)).toBeInTheDocument();
    expect(patchPresenceHistorySettings).toHaveBeenCalledTimes(1);
    await user.click(within(dialog).getByRole('button', { name: 'Delete older history' }));

    await waitFor(() =>
      expect(patchPresenceHistorySettings).toHaveBeenLastCalledWith(
        { kind: 'retention', retentionDays: 7 },
        expect.any(AbortSignal)
      )
    );
  });

  it('blocks a stale captured retention increase while preserving a stale decrease', async () => {
    const user = userEvent.setup();
    vi.mocked(getPresenceHistorySettings).mockResolvedValue(ON_SETTINGS);
    vi.mocked(patchPresenceHistorySettings).mockResolvedValue({
      ...ON_SETTINGS,
      retentionDays: 7,
    });
    render(<ActivityHistoryCard />);
    const retention = await screen.findByRole('combobox', { name: 'Retention period' });

    act(() => {
      setCapability({ status: 'error', lastConfirmedSupported: true });
      fireEvent.change(retention, { target: { value: '90' } });
    });

    expect(patchPresenceHistorySettings).not.toHaveBeenCalled();
    expect(
      screen.getByText(
        'Activity History availability changed. Enabling and retention increases are paused until support is confirmed.'
      )
    ).toBeInTheDocument();

    await user.selectOptions(screen.getByRole('combobox', { name: 'Retention period' }), '7');
    await user.click(screen.getByRole('button', { name: 'Delete older history' }));
    await waitFor(() =>
      expect(patchPresenceHistorySettings).toHaveBeenCalledWith(
        { kind: 'retention', retentionDays: 7 },
        expect.any(AbortSignal)
      )
    );
  });

  it('confirms disable, refetches before announcing, and focuses the persistent heading', async () => {
    const user = userEvent.setup();
    vi.mocked(getPresenceHistorySettings)
      .mockResolvedValueOnce(ON_SETTINGS)
      .mockResolvedValueOnce(OFF_SETTINGS);
    vi.mocked(patchPresenceHistorySettings).mockResolvedValue(OFF_SETTINGS);
    render(<ActivityHistoryCard />);

    await user.click(await screen.findByRole('switch', { name: 'Activity History' }));
    const dialog = screen.getByRole('dialog', { name: 'Turn off Activity History?' });
    expect(within(dialog).getByText(/all active history will be deleted/i)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Turn off and delete history' }));

    await waitFor(() => expect(getPresenceHistorySettings).toHaveBeenCalledTimes(2));
    expect(
      await screen.findByText('Activity History was turned off and history was deleted.')
    ).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: 'Activity History' })).toHaveFocus()
    );
  });

  it('keeps the Off card and focus after deletion completes during a gate-false drain', async () => {
    const user = userEvent.setup();
    vi.mocked(getPresenceHistorySettings)
      .mockResolvedValueOnce(ON_SETTINGS)
      .mockResolvedValueOnce(OFF_SETTINGS);
    setCapability({ status: 'confirmed-unsupported' });
    render(<ActivityHistoryCard />);

    await user.click(await screen.findByRole('button', { name: 'Delete history and turn off' }));
    const dialog = screen.getByRole('dialog', { name: 'Delete Activity History?' });
    await user.click(within(dialog).getByRole('button', { name: 'Delete history and turn off' }));

    await waitFor(() =>
      expect(deletePresenceHistory).toHaveBeenCalledWith(expect.any(AbortSignal))
    );
    expect(getPresenceHistorySettings).toHaveBeenCalledTimes(2);
    expect(
      await screen.findByText('Activity History was deleted and recording was turned off.')
    ).toBeInTheDocument();
    expect(screen.getByText('Off')).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Activity History' })).toBeDisabled();
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: 'Activity History' })).toHaveFocus()
    );
  });

  it.each(['delete request', 'confirmation readback'] as const)(
    'hides stale settings when a drain loses its routes during the %s',
    async (failureStage) => {
      const user = userEvent.setup();
      vi.mocked(getPresenceHistorySettings).mockResolvedValueOnce(ON_SETTINGS);
      if (failureStage === 'delete request') {
        vi.mocked(deletePresenceHistory).mockRejectedValueOnce(
          new PresenceHistoryRequestError(404, 'not_found', null)
        );
      } else {
        vi.mocked(getPresenceHistorySettings).mockRejectedValueOnce(
          new PresenceHistoryRequestError(404, 'not_found', null)
        );
      }
      setCapability({ status: 'confirmed-unsupported' });
      const { container } = render(<ActivityHistoryCard />);

      await user.click(await screen.findByRole('button', { name: 'Delete history and turn off' }));
      await user.click(
        within(screen.getByRole('dialog', { name: 'Delete Activity History?' })).getByRole(
          'button',
          { name: 'Delete history and turn off' }
        )
      );

      await waitFor(() => expect(container).toBeEmptyDOMElement());
    }
  );

  it.each([
    { label: 'On', readback: ON_SETTINGS },
    { label: 'Paused', readback: PAUSED_SETTINGS },
  ])(
    'does not announce destructive success when authoritative readback is $label',
    async ({ label, readback }) => {
      const user = userEvent.setup();
      vi.mocked(getPresenceHistorySettings)
        .mockResolvedValueOnce(ON_SETTINGS)
        .mockResolvedValueOnce(readback);
      render(<ActivityHistoryCard />);

      await user.click(await screen.findByRole('button', { name: 'Delete history and turn off' }));
      await user.click(
        within(screen.getByRole('dialog', { name: 'Delete Activity History?' })).getByRole(
          'button',
          { name: 'Delete history and turn off' }
        )
      );

      await waitFor(() => expect(getPresenceHistorySettings).toHaveBeenCalledTimes(2));
      expect(await screen.findByText(label)).toBeInTheDocument();
      expect(
        screen.getByText(
          'Activity History changed in another session. Review the current settings and try again.'
        )
      ).toBeInTheDocument();
      expect(
        screen.queryByText('Activity History was deleted and recording was turned off.')
      ).not.toBeInTheDocument();
      expect(screen.getByRole('heading', { name: 'Activity History' })).not.toHaveFocus();
    }
  );

  it('preserves stale last-confirmed settings and disables mutations after a mutation failure', async () => {
    const user = userEvent.setup();
    vi.mocked(getPresenceHistorySettings).mockResolvedValue(ON_SETTINGS);
    vi.mocked(patchPresenceHistorySettings).mockRejectedValue(
      new Error('Activity History settings could not be saved.')
    );
    render(<ActivityHistoryCard />);

    await user.selectOptions(
      await screen.findByRole('combobox', { name: 'Retention period' }),
      '90'
    );

    const errorNotice = await screen.findByRole('alert');
    expect(errorNotice).toHaveTextContent('Activity History settings could not be saved.');
    expect(screen.getByRole('status')).not.toHaveTextContent(
      'Activity History settings could not be saved.'
    );
    expect(screen.getByText('Not refreshed')).toBeInTheDocument();
    expect(screen.getByText('On')).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Activity History' })).toBeDisabled();
    expect(screen.getByRole('combobox', { name: 'Retention period' })).toBeDisabled();
  });

  it('refetches a 409, keeps re-consent open, resets acknowledgement, and focuses new terms', async () => {
    const user = userEvent.setup();
    vi.mocked(getPresenceHistorySettings)
      .mockResolvedValueOnce(OFF_SETTINGS)
      .mockResolvedValueOnce(UPDATED_PAUSED_SETTINGS);
    vi.mocked(patchPresenceHistorySettings).mockRejectedValue(
      new PresenceHistoryRequestError(409, 'stale_activity_history_consent', null)
    );
    render(<ActivityHistoryCard />);

    await user.click(await screen.findByRole('switch', { name: 'Activity History' }));
    await user.click(screen.getByRole('checkbox', { name: DISCLOSURE.acknowledgementLabel }));
    expect(screen.getByRole('checkbox', { name: DISCLOSURE.acknowledgementLabel })).toBeChecked();
    await user.click(screen.getByRole('button', { name: 'Enable Activity History' }));

    expect(
      await screen.findByText(
        'Updated Activity History terms apply to future Custom Status changes.'
      )
    ).toBeInTheDocument();
    expect(
      screen.getByRole('checkbox', { name: UPDATED_DISCLOSURE.acknowledgementLabel })
    ).not.toBeChecked();
    expect(screen.getByRole('heading', { name: UPDATED_DISCLOSURE.requiredText })).toHaveFocus();
  });

  it('allows privacy narrowing but blocks enable/increases after a capability error with prior support', async () => {
    const user = userEvent.setup();
    vi.mocked(getPresenceHistorySettings).mockResolvedValue(ON_SETTINGS);
    render(<ActivityHistoryCard />);
    await screen.findByText('On');

    act(() => {
      setCapability({ status: 'error', lastConfirmedSupported: true });
    });

    expect(screen.getByText('Availability not refreshed')).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Activity History' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Delete history and turn off' })).toBeEnabled();
    const retention = screen.getByRole('combobox', { name: 'Retention period' });
    expect(within(retention).getByRole('option', { name: '7 days' })).toBeEnabled();
    expect(within(retention).getByRole('option', { name: '90 days' })).toBeDisabled();

    await user.click(screen.getByRole('button', { name: 'Retry availability check' }));
    expect(clientConfigService.refreshServerCapabilities).toHaveBeenCalledTimes(1);
  });

  it('reloads validated enabled settings on a stale-supported remount without allowing widening', async () => {
    vi.mocked(getPresenceHistorySettings).mockResolvedValue(ON_SETTINGS);
    const firstMount = render(<ActivityHistoryCard />);

    expect(await screen.findByText('On')).toBeInTheDocument();
    firstMount.unmount();

    act(() => {
      setCapability({ status: 'error', lastConfirmedSupported: true });
    });
    render(<ActivityHistoryCard />);

    expect(await screen.findByText('On')).toBeInTheDocument();
    expect(getPresenceHistorySettings).toHaveBeenCalledTimes(2);
    expect(screen.getByRole('switch', { name: 'Activity History' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Delete history and turn off' })).toBeEnabled();
    const retention = screen.getByRole('combobox', { name: 'Retention period' });
    expect(within(retention).getByRole('option', { name: '7 days' })).toBeEnabled();
    expect(within(retention).getByRole('option', { name: '90 days' })).toBeDisabled();
  });

  it('announces a transition to stale-supported availability as a polite status', async () => {
    vi.mocked(getPresenceHistorySettings).mockResolvedValue(ON_SETTINGS);
    render(<ActivityHistoryCard />);
    await screen.findByText('On');

    act(() => {
      setCapability({ status: 'error', lastConfirmedSupported: true });
    });

    const liveStatus = screen.getByText('Availability not refreshed.', { selector: 'output' });
    expect(screen.getByRole('status')).toBe(liveStatus);
    expect(liveStatus).not.toHaveAttribute('role');
    expect(liveStatus).toHaveAttribute('aria-live', 'polite');
  });

  it('aborts the initial request on unmount', () => {
    let capturedSignal: AbortSignal | undefined;
    vi.mocked(getPresenceHistorySettings).mockImplementation((signal) => {
      capturedSignal = signal;
      return new Promise(() => undefined);
    });
    const { unmount } = render(<ActivityHistoryCard />);

    expect(capturedSignal).toBeDefined();
    unmount();
    expect(capturedSignal?.aborted).toBe(true);
  });

  it('clears owner state synchronously and rejects stale account-A fetch completion', async () => {
    const accountA = deferred<PresenceHistorySettings>();
    const accountB = deferred<PresenceHistorySettings>();
    vi.mocked(getPresenceHistorySettings)
      .mockReturnValueOnce(accountA.promise)
      .mockReturnValueOnce(accountB.promise);
    render(<ActivityHistoryCard />);

    act(() => {
      setOwner('user-b', 'session-b');
    });

    expect(screen.getByText('Loading Activity History settings…')).toBeInTheDocument();
    expect(screen.queryByText('On')).not.toBeInTheDocument();

    await act(async () => {
      accountA.resolve(ON_SETTINGS);
      await accountA.promise;
    });
    expect(screen.queryByText('On')).not.toBeInTheDocument();

    await act(async () => {
      accountB.resolve(OFF_SETTINGS);
      await accountB.promise;
    });
    expect(await screen.findByText('Off')).toBeInTheDocument();
  });

  it('rejects stale account-A mutation completion and never announces it for account B', async () => {
    const user = userEvent.setup();
    const deletion = deferred<void>();
    vi.mocked(getPresenceHistorySettings)
      .mockResolvedValueOnce(ON_SETTINGS)
      .mockResolvedValueOnce(OFF_SETTINGS);
    vi.mocked(deletePresenceHistory).mockReturnValue(deletion.promise);
    render(<ActivityHistoryCard />);

    await user.click(await screen.findByRole('button', { name: 'Delete history and turn off' }));
    await user.click(
      within(screen.getByRole('dialog', { name: 'Delete Activity History?' })).getByRole('button', {
        name: 'Delete history and turn off',
      })
    );

    act(() => {
      setOwner('user-b', 'session-b');
    });
    expect(screen.getByText('Loading Activity History settings…')).toBeInTheDocument();

    await act(async () => {
      deletion.resolve();
      await deletion.promise;
    });

    expect(await screen.findByText('Off')).toBeInTheDocument();
    expect(
      screen.queryByText('Activity History was deleted and recording was turned off.')
    ).not.toBeInTheDocument();
  });
});
