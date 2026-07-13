import { act, fireEvent, render, screen } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useClientConfigStore } from '@/renderer/stores/clientConfigStore';
import { useSettingsNavStore } from '@/renderer/stores/settingsNavStore';

vi.mock('@/renderer/components/Settings/AccountSection', () => ({
  default: () => (
    <details>
      <summary>My Profile</summary>
      <section id="section-profile" />
      <section id="section-presence-history" aria-labelledby="presence-history-heading">
        <h3 id="presence-history-heading" tabIndex={-1}>
          Activity History
        </h3>
      </section>
    </details>
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
  it('routes the Account sidebar item through the persistent history heading focus target', () => {
    useClientConfigStore.getState().setActivityHistoryCapability({ status: 'supported' });
    render(<SettingsPage />);

    fireEvent.click(screen.getByRole('button', { name: 'Activity History' }));
    act(() => {
      vi.advanceTimersByTime(60);
    });

    const heading = document.getElementById('presence-history-heading');
    expect(heading).not.toBeNull();
    expect(document.activeElement).toBe(heading);
    expect(heading?.closest('details')).toHaveAttribute('open');
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

  it('hides only unsupported Activity History navigation and preserves ordinary Account subsections', () => {
    useClientConfigStore
      .getState()
      .setActivityHistoryCapability({ status: 'confirmed-unsupported' });
    render(<SettingsPage />);

    expect(screen.queryByRole('button', { name: 'Activity History' })).not.toBeInTheDocument();
    const profileButton = screen.getByRole('button', { name: 'My Profile' });
    expect(screen.getByRole('button', { name: 'NSFW Content Access' })).toBeInTheDocument();

    fireEvent.click(profileButton);
    act(() => {
      vi.advanceTimersByTime(60);
    });

    expect(document.getElementById('section-profile')).not.toBeNull();
    expect(Element.prototype.scrollIntoView).toHaveBeenCalledWith({
      behavior: 'smooth',
      block: 'start',
    });
  });

  it('honors the cross-pane View history focus contract from the privacy card', () => {
    render(<SettingsPage />);

    act(() => {
      useSettingsNavStore.getState().requestFocus('account', 'presence-history-heading');
    });
    act(() => {
      vi.advanceTimersByTime(60);
    });

    const heading = document.getElementById('presence-history-heading');
    expect(document.activeElement).toBe(heading);
    expect(heading?.closest('details')).toHaveAttribute('open');
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
  });
});
