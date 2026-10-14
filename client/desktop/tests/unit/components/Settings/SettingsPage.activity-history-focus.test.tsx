import { act, fireEvent, render, screen } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { useSettingsNavStore } from '@/renderer/stores/ui/settingsNavStore';

vi.mock('@/renderer/components/Settings/PrivacySection', () => ({
  default: () => (
    <>
      <details id="section-privacy-settings">
        <summary>Privacy Controls</summary>
      </details>
      <details id="section-presence-history">
        <summary>Activity History</summary>
        <h3 id="presence-history-heading" tabIndex={-1}>
          Activity History
        </h3>
      </details>
    </>
  ),
}));

vi.mock('@/renderer/components/Settings/AccountSection', () => ({
  default: () => (
    <>
      <section id="section-profile" />
      <details id="section-subscription">
        <summary>Subscription</summary>
      </details>
      <section id="section-nsfw-content" />
    </>
  ),
}));

import SettingsPage from '@/renderer/components/Settings/SettingsPage';

beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

beforeEach(() => {
  resetAllStores();
  useSettingsNavStore.getState().clearFocusRequest();
  vi.mocked(Element.prototype.scrollIntoView).mockClear();
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('SettingsPage Activity History focus integration', () => {
  it('routes the Privacy sidebar item through the Activity History disclosure summary', () => {
    useClientConfigStore.getState().setActivityHistoryCapability({ status: 'supported' });
    render(<SettingsPage />);

    fireEvent.click(screen.getByRole('button', { name: 'Activity History' }));
    act(() => {
      vi.advanceTimersByTime(60);
    });

    const section = document.getElementById('section-presence-history');
    const summary = section?.querySelector(':scope > summary');
    expect(summary).not.toBeNull();
    expect(document.activeElement).toBe(summary);
    expect(section).toHaveAttribute('open');
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
  });

  it.each([
    ['loading', { status: 'loading' } as const],
    ['unknown error', { status: 'error', lastConfirmedSupported: false } as const],
    ['stale supported error', { status: 'error', lastConfirmedSupported: true } as const],
    ['supported', { status: 'supported' } as const],
  ])('keeps Activity History navigation during %s capability state', (_label, capability) => {
    useClientConfigStore.getState().setActivityHistoryCapability(capability);

    render(<SettingsPage />);

    expect(screen.getByRole('button', { name: 'Activity History' })).toBeInTheDocument();
  });

  it('keeps other Privacy destinations discoverable without an unprobed unsupported History child', () => {
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    render(<SettingsPage />);

    expect(screen.queryByRole('button', { name: 'Activity History' })).toBeNull();
    const privacyButton = screen.getByRole('button', { name: 'Privacy Controls' });
    expect(screen.getByRole('button', { name: 'Active Sessions' })).toBeInTheDocument();

    fireEvent.click(privacyButton);
    act(() => {
      vi.advanceTimersByTime(60);
    });

    expect(document.getElementById('section-privacy-settings')).not.toBeNull();
    expect(Element.prototype.scrollIntoView).toHaveBeenCalledWith({
      behavior: 'smooth',
      block: 'center',
    });
  });

  it('honors the cross-pane View history focus contract from the privacy card', () => {
    render(<SettingsPage />);

    act(() => {
      useSettingsNavStore.getState().requestFocus('privacy', 'presence-history-heading');
    });
    act(() => {
      vi.advanceTimersByTime(60);
    });

    const heading = document.getElementById('presence-history-heading');
    expect(document.activeElement).toBe(heading);
    expect(heading?.closest('details')).toHaveAttribute('open');
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
  });
  it('focuses the summary when a deep-link targets the Account Subscription disclosure', () => {
    render(<SettingsPage />);

    act(() => {
      useSettingsNavStore.getState().requestFocus('account', 'section-subscription');
    });
    act(() => {
      vi.advanceTimersByTime(60);
    });

    const details = document.getElementById('section-subscription');
    expect(details).not.toBeNull();
    // focus() on a <details> is a no-op, so the effect must redirect to its <summary>.
    // Without that redirect this silently lands nowhere and clearFocusRequest() still
    // runs, so the failure mode is invisible — which is why it needs an explicit test.
    expect(document.activeElement).toBe(details?.querySelector(':scope > summary'));
    expect(details).toHaveAttribute('open');
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
  });
});
