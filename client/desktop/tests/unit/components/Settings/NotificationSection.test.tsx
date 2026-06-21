import { render, screen, fireEvent } from '../../../test-utils';
import { vi, type Mock } from 'vitest';

// ── Mocks ──────────────────────────────────────────────────────────────

const mockNotificationState = {
  enabled: true,
  volume: 80,
  messageSound: true,
  messageVolume: 100,
  mentionSound: true,
  mentionVolume: 100,
  dmSound: true,
  dmVolume: 100,
  friendRequestSound: true,
  friendRequestVolume: 100,
  voiceEventSounds: true,
  voiceEventVolume: 100,
  suppressWhenFocused: true,
  desktopNotificationsEnabled: true,
  desktopNotifyDMs: true,
  desktopNotifyMentions: true,
  desktopNotifyAllMessages: false,
  doNotDisturb: false,
  quietHoursEnabled: false,
  quietHoursStart: '22:00',
  quietHoursEnd: '08:00',
  setEnabled: vi.fn(),
  setVolume: vi.fn(),
  setMessageSound: vi.fn(),
  setMessageVolume: vi.fn(),
  setMentionSound: vi.fn(),
  setMentionVolume: vi.fn(),
  setDmSound: vi.fn(),
  setDmVolume: vi.fn(),
  setFriendRequestSound: vi.fn(),
  setFriendRequestVolume: vi.fn(),
  setVoiceEventSounds: vi.fn(),
  setVoiceEventVolume: vi.fn(),
  setSuppressWhenFocused: vi.fn(),
  setDesktopNotificationsEnabled: vi.fn(),
  setDesktopNotifyDMs: vi.fn(),
  setDesktopNotifyMentions: vi.fn(),
  setDesktopNotifyAllMessages: vi.fn(),
  setDoNotDisturb: vi.fn(),
  setQuietHoursEnabled: vi.fn(),
  setQuietHoursStart: vi.fn(),
  setQuietHoursEnd: vi.fn(),
};

vi.mock('@/renderer/stores/notificationStore', () => ({
  useNotificationStore: vi.fn((selector?: (s: Record<string, unknown>) => unknown) => {
    const state = { ...mockNotificationState };
    return selector ? selector(state) : state;
  }),
}));

const mockRequestOne = vi.fn();
const mockOpenSettings = vi.fn();

vi.mock('@/renderer/stores/osPermissionStore', () => ({
  useOsPermissionStore: vi.fn((selector?: (s: Record<string, unknown>) => unknown) => {
    const state = {
      notifications: 'granted' as string,
      requestOne: mockRequestOne,
      openSettings: mockOpenSettings,
    };
    return selector ? selector(state) : state;
  }),
}));

import NotificationSection from '@/renderer/components/Settings/NotificationSection';
import { useOsPermissionStore } from '@/renderer/stores/osPermissionStore';

// Helper to override OS permission status
function setPermissionStatus(status: string) {
  (useOsPermissionStore as unknown as Mock).mockImplementation(
    (selector?: (s: Record<string, unknown>) => unknown) => {
      const state = {
        notifications: status,
        requestOne: mockRequestOne,
        openSettings: mockOpenSettings,
      };
      return selector ? selector(state) : state;
    }
  );
}

describe('NotificationSection', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // Reset to granted by default
    setPermissionStatus('granted');
  });

  // ── Desktop Notification toggles ──────────────────────────────────

  it('renders all desktop notification toggles', () => {
    render(<NotificationSection />);
    expect(screen.getByText('Enable Desktop Notifications')).toBeInTheDocument();
    expect(screen.getByText('Direct Messages')).toBeInTheDocument();
    expect(screen.getByText('@Mentions')).toBeInTheDocument();
    expect(screen.getByText('All Messages')).toBeInTheDocument();
    expect(screen.getByText('Do Not Disturb')).toBeInTheDocument();
    expect(screen.getByText('Suppress when focused')).toBeInTheDocument();
  });

  // ── Sound toggles ────────────────────────────────────────────────

  it('renders all sound toggles', () => {
    render(<NotificationSection />);
    expect(screen.getByText('Enable Notification Sounds')).toBeInTheDocument();
    expect(screen.getByText('Message sounds')).toBeInTheDocument();
    expect(screen.getByText('Mention sounds')).toBeInTheDocument();
    expect(screen.getByText('DM sounds')).toBeInTheDocument();
    expect(screen.getByText('Friend request sounds')).toBeInTheDocument();
    expect(screen.getByText('Voice event sounds')).toBeInTheDocument();
  });

  // ── Master desktop toggle ────────────────────────────────────────

  it('master desktop toggle calls setDesktopNotificationsEnabled', () => {
    render(<NotificationSection />);
    const row = screen.getByText('Enable Desktop Notifications').closest('.settings-row');
    const checkbox = row?.querySelector('input[type="checkbox"]');
    expect(checkbox).toBeInTheDocument();
    fireEvent.click(checkbox!);
    expect(mockNotificationState.setDesktopNotificationsEnabled).toHaveBeenCalledWith(
      expect.any(Boolean)
    );
  });

  // ── Volume slider ────────────────────────────────────────────────

  it('volume slider calls setVolume', () => {
    render(<NotificationSection />);
    const slider = document.querySelector('.settings-slider');
    expect(slider).toBeInTheDocument();
    fireEvent.change(slider!, { target: { value: '50' } });
    expect(mockNotificationState.setVolume).toHaveBeenCalledWith(50);
  });

  // ── Permission banner: not-determined ─────────────────────────────

  it('permission banner shows when status is not-determined', () => {
    setPermissionStatus('not-determined');
    render(<NotificationSection />);
    expect(screen.getByText('Desktop notifications require permission.')).toBeInTheDocument();
    expect(screen.getByText('Enable Notifications')).toBeInTheDocument();
  });

  // ── Permission banner: granted ────────────────────────────────────

  it('permission banner hidden when status is granted', () => {
    setPermissionStatus('granted');
    render(<NotificationSection />);
    expect(screen.queryByText('Desktop notifications require permission.')).not.toBeInTheDocument();
  });

  // ── DND toggle ────────────────────────────────────────────────────

  it('DND toggle calls setDoNotDisturb', () => {
    render(<NotificationSection />);
    const row = screen.getByText('Do Not Disturb').closest('.settings-row');
    const checkbox = row?.querySelector('input[type="checkbox"]');
    expect(checkbox).toBeInTheDocument();
    fireEvent.click(checkbox!);
    expect(mockNotificationState.setDoNotDisturb).toHaveBeenCalledWith(expect.any(Boolean));
  });

  // ── Quiet Hours ───────────────────────────────────────────────────

  it('quiet hours section renders start/end time inputs when enabled', () => {
    mockNotificationState.quietHoursEnabled = true;
    render(<NotificationSection />);
    expect(screen.getByText('Start Time')).toBeInTheDocument();
    expect(screen.getByText('End Time')).toBeInTheDocument();
    const timeInputs = document.querySelectorAll('input[type="time"]');
    expect(timeInputs.length).toBe(2);
    expect(timeInputs[0]).toHaveValue('22:00');
    expect(timeInputs[1]).toHaveValue('08:00');
    // Reset for other tests
    mockNotificationState.quietHoursEnabled = false;
  });

  it('quiet hours time inputs are hidden when disabled', () => {
    mockNotificationState.quietHoursEnabled = false;
    render(<NotificationSection />);
    expect(screen.queryByText('Start Time')).not.toBeInTheDocument();
    expect(screen.queryByText('End Time')).not.toBeInTheDocument();
  });

  // ── Permission banner: denied ──────────────────────────────────────

  it('permission banner shows denied message when status is denied', () => {
    setPermissionStatus('denied');
    render(<NotificationSection />);
    expect(
      screen.getByText('Notification permission was denied. Enable in System Settings.')
    ).toBeInTheDocument();
    // The "Enable Notifications" button should NOT appear for denied state
    expect(screen.queryByText('Enable Notifications')).not.toBeInTheDocument();
  });

  // ── Enable Notifications button ────────────────────────────────────

  it('clicking Enable Notifications calls requestOne', () => {
    setPermissionStatus('not-determined');
    render(<NotificationSection />);
    const btn = screen.getByText('Enable Notifications');
    fireEvent.click(btn);
    expect(mockRequestOne).toHaveBeenCalledWith('notifications');
  });

  // ── Hint text branches ─────────────────────────────────────────────

  it('desktop notification hint shows disabled text when toggled off', () => {
    mockNotificationState.desktopNotificationsEnabled = false;
    render(<NotificationSection />);
    expect(
      screen.getByText('Disabled. No desktop notifications will be shown.')
    ).toBeInTheDocument();
    // Reset for other tests
    mockNotificationState.desktopNotificationsEnabled = true;
  });

  it('sound hint shows disabled text when toggled off', () => {
    mockNotificationState.enabled = false;
    render(<NotificationSection />);
    expect(screen.getByText('Disabled. All notification sounds are muted.')).toBeInTheDocument();
    // Reset for other tests
    mockNotificationState.enabled = true;
  });

  it('DND hint shows enabled text', () => {
    mockNotificationState.doNotDisturb = true;
    render(<NotificationSection />);
    expect(screen.getByText('Enabled. All notifications are suppressed.')).toBeInTheDocument();
    // Reset for other tests
    mockNotificationState.doNotDisturb = false;
  });

  it('quiet hours hint shows enabled text', () => {
    mockNotificationState.quietHoursEnabled = true;
    render(<NotificationSection />);
    expect(
      screen.getByText('Enabled. Notifications are suppressed during the configured time window.')
    ).toBeInTheDocument();
    // Reset for other tests
    mockNotificationState.quietHoursEnabled = false;
  });

  it('suppress when focused hint shows disabled text when toggled off', () => {
    mockNotificationState.suppressWhenFocused = false;
    render(<NotificationSection />);
    expect(
      screen.getByText('Disabled. Sounds and notifications play even when the app is focused.')
    ).toBeInTheDocument();
    mockNotificationState.suppressWhenFocused = true;
  });

  it('quiet hours time inputs call setters on change', () => {
    mockNotificationState.quietHoursEnabled = true;
    render(<NotificationSection />);
    const timeInputs = document.querySelectorAll('input[type="time"]');
    expect(timeInputs.length).toBe(2);
    fireEvent.change(timeInputs[0], { target: { value: '23:00' } });
    expect(mockNotificationState.setQuietHoursStart).toHaveBeenCalledWith('23:00');
    fireEvent.change(timeInputs[1], { target: { value: '07:00' } });
    expect(mockNotificationState.setQuietHoursEnd).toHaveBeenCalledWith('07:00');
    mockNotificationState.quietHoursEnabled = false;
  });

  // ── Per-category volume sliders ───────────────────────────────────

  it('renders a volume slider for each of the 5 sound categories', () => {
    render(<NotificationSection />);
    // 5 per-category sliders + 1 master = 6 total
    expect(screen.getByLabelText('Message sounds volume')).toBeInTheDocument();
    expect(screen.getByLabelText('Mention sounds volume')).toBeInTheDocument();
    expect(screen.getByLabelText('DM sounds volume')).toBeInTheDocument();
    expect(screen.getByLabelText('Friend request sounds volume')).toBeInTheDocument();
    expect(screen.getByLabelText('Voice event sounds volume')).toBeInTheDocument();
  });

  it('message volume slider calls setMessageVolume on change', () => {
    render(<NotificationSection />);
    fireEvent.change(screen.getByLabelText('Message sounds volume'), { target: { value: '42' } });
    expect(mockNotificationState.setMessageVolume).toHaveBeenCalledWith(42);
  });

  it('mention volume slider calls setMentionVolume on change', () => {
    render(<NotificationSection />);
    fireEvent.change(screen.getByLabelText('Mention sounds volume'), { target: { value: '55' } });
    expect(mockNotificationState.setMentionVolume).toHaveBeenCalledWith(55);
  });

  it('DM volume slider calls setDmVolume on change', () => {
    render(<NotificationSection />);
    fireEvent.change(screen.getByLabelText('DM sounds volume'), { target: { value: '60' } });
    expect(mockNotificationState.setDmVolume).toHaveBeenCalledWith(60);
  });

  it('friend request volume slider calls setFriendRequestVolume on change', () => {
    render(<NotificationSection />);
    fireEvent.change(screen.getByLabelText('Friend request sounds volume'), {
      target: { value: '25' },
    });
    expect(mockNotificationState.setFriendRequestVolume).toHaveBeenCalledWith(25);
  });

  it('voice event volume slider calls setVoiceEventVolume on change', () => {
    render(<NotificationSection />);
    fireEvent.change(screen.getByLabelText('Voice event sounds volume'), {
      target: { value: '70' },
    });
    expect(mockNotificationState.setVoiceEventVolume).toHaveBeenCalledWith(70);
  });

  it('category volume slider is disabled when the category toggle is off', () => {
    mockNotificationState.messageSound = false;
    render(<NotificationSection />);
    const slider = screen.getByLabelText('Message sounds volume') as HTMLInputElement;
    expect(slider.disabled).toBe(true);
    mockNotificationState.messageSound = true; // reset
  });

  it('category volume slider is disabled when master Enable is off', () => {
    mockNotificationState.enabled = false;
    render(<NotificationSection />);
    const slider = screen.getByLabelText('Message sounds volume') as HTMLInputElement;
    expect(slider.disabled).toBe(true);
    mockNotificationState.enabled = true; // reset
  });

  it('displays the current per-category volume value in the slider label', () => {
    mockNotificationState.messageVolume = 37;
    render(<NotificationSection />);
    // The value appears next to the slider as "37%"
    expect(screen.getAllByText('37%').length).toBeGreaterThanOrEqual(1);
    mockNotificationState.messageVolume = 100; // reset
  });

  // ── "Open System Settings" button when denied ─────────────────────

  it('clicking Open System Settings (denied banner) calls openSettings', () => {
    setPermissionStatus('denied');
    render(<NotificationSection />);
    fireEvent.click(screen.getByText('Open System Settings'));
    expect(mockOpenSettings).toHaveBeenCalledWith('notifications');
  });
});
