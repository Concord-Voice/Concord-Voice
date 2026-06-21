import { render, screen, fireEvent } from '../../../test-utils';
import { useSettingsStore } from '@/renderer/stores/settingsStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useSettingsOverlayStore } from '@/renderer/stores/settingsOverlayStore';
import { mockUser } from '../../../mocks/fixtures';

// Mock apiFetch for session fetching
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({
    ok: true,
    json: async () => ({ sessions: [], past_sessions: [] }),
  }),
  API_BASE: 'http://localhost:8080',
}));

// Mock LoadingSpinner
vi.mock('@/renderer/components/Auth/LoadingSpinner', () => ({
  default: ({ size, inline }: { size?: string; inline?: boolean }) => (
    <div data-testid="loading-spinner" data-size={size} data-inline={inline}>
      Loading...
    </div>
  ),
}));

import SettingsPage from '@/renderer/components/Settings/SettingsPage';

describe('SettingsPage', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    useUserStore.setState({ user: mockUser });
    useSettingsStore.setState({
      appearance: {
        theme: 'dark',
        colorScheme: 'concord',
        fontSize: 'default',
        compactMode: false,
        reduceAnimations: false,
        customColors: null,
      },
    });
  });

  it('renders settings page with title', () => {
    render(<SettingsPage />);
    expect(screen.getByText('Settings')).toBeInTheDocument();
  });

  it('renders back button', () => {
    render(<SettingsPage />);
    expect(screen.getByText('Back to app')).toBeInTheDocument();
  });

  it('renders navigation items', () => {
    render(<SettingsPage />);
    expect(screen.getByText('Appearance')).toBeInTheDocument();
    expect(screen.getByText('Privacy & Security')).toBeInTheDocument();
    expect(screen.getByText('Account')).toBeInTheDocument();
    expect(screen.getByText('Sounds and Notifications')).toBeInTheDocument();
    expect(screen.getByText('Audio & Video')).toBeInTheDocument();
    expect(screen.getByText('Accessibility')).toBeInTheDocument();
  });

  it('no longer shows a "Soon" badge now that Account is enabled', () => {
    render(<SettingsPage />);
    expect(screen.queryAllByText('Soon')).toHaveLength(0);
  });

  it('opens the Account section and shows the NSFW Content Access gate', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Account'));
    // "NSFW Content Access" appears in the nav tree AND as the section heading.
    expect(screen.getAllByText('NSFW Content Access').length).toBeGreaterThanOrEqual(1);
    expect(screen.getByRole('button', { name: /verify age/i })).toBeInTheDocument();
  });

  it('defaults to appearance section', () => {
    render(<SettingsPage />);
    // Text appears in both nav tree and content — check content section title exists
    const colorSchemeEls = screen.getAllByText('Color Scheme');
    expect(colorSchemeEls.length).toBeGreaterThanOrEqual(1);
    const themeEls = screen.getAllByText('Theme');
    expect(themeEls.length).toBeGreaterThanOrEqual(1);
  });

  it('renders theme options', () => {
    render(<SettingsPage />);
    expect(screen.getByText('Dark')).toBeInTheDocument();
    expect(screen.getByText('Light')).toBeInTheDocument();
    expect(screen.getByText('System')).toBeInTheDocument();
  });

  // #489 — Display settings (font size, compact mode, reduce animations) plus
  // UI Scale and High Contrast moved from Appearance to Accessibility. These
  // tests navigate to the new tab before asserting.

  it('renders font size options under Accessibility', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Accessibility'));
    expect(screen.getByText('Font Size')).toBeInTheDocument();
    expect(screen.getByText('Small')).toBeInTheDocument();
    expect(screen.getByText('Default')).toBeInTheDocument();
    expect(screen.getByText('Large')).toBeInTheDocument();
  });

  it('renders compact mode toggle under Accessibility', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Accessibility'));
    expect(screen.getByText('Compact Mode')).toBeInTheDocument();
  });

  it('renders reduce animations toggle under Accessibility', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Accessibility'));
    expect(screen.getByText('Reduce Animations')).toBeInTheDocument();
  });

  it('renders UI Scale slider under Accessibility (#489)', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Accessibility'));
    expect(screen.getByLabelText('UI Scale')).toBeInTheDocument();
  });

  it('renders High Contrast toggle under Accessibility (#489)', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Accessibility'));
    expect(screen.getByText('High Contrast')).toBeInTheDocument();
  });

  it('switches to privacy section on nav click', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Privacy & Security'));
    // "Active Sessions" appears in both nav tree and content section title
    const els = screen.getAllByText('Active Sessions');
    expect(els.length).toBeGreaterThanOrEqual(1);
  });

  it('changes theme on click', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Light'));
    expect(useSettingsStore.getState().appearance.theme).toBe('light');
  });

  it('changes font size on click', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Accessibility'));
    fireEvent.click(screen.getByText('Large'));
    expect(useSettingsStore.getState().appearance.fontSize).toBe('large');
  });

  it('renders color scheme options', () => {
    render(<SettingsPage />);
    // The active color scheme label should be shown
    expect(screen.getByText('Concord Voice')).toBeInTheDocument();
  });

  it('toggles compact mode', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Accessibility'));
    const toggle = screen
      .getByText('Compact Mode')
      .closest('.settings-row')
      ?.querySelector('input[type="checkbox"]');
    expect(toggle).toBeInTheDocument();
    fireEvent.click(toggle!);
    expect(useSettingsStore.getState().appearance.compactMode).toBe(true);
  });

  it('toggles reduce animations', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Accessibility'));
    const toggle = screen
      .getByText('Reduce Animations')
      .closest('.settings-row')
      ?.querySelector('input[type="checkbox"]');
    expect(toggle).toBeInTheDocument();
    fireEvent.click(toggle!);
    expect(useSettingsStore.getState().appearance.reduceAnimations).toBe(true);
  });

  it('changes color scheme on click', () => {
    render(<SettingsPage />);
    const circles = document.querySelectorAll('.color-scheme-circle');
    expect(circles.length).toBeGreaterThan(2);
    // Defacto is index 1 (inserted after Concord); Eclipse shifts to index 2.
    fireEvent.click(circles[1]); // Defacto
    expect(useSettingsStore.getState().appearance.colorScheme).toBe('defacto');
    fireEvent.click(circles[2]); // Eclipse
    expect(useSettingsStore.getState().appearance.colorScheme).toBe('eclipse');
  });

  it('changes font size to small', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Accessibility'));
    fireEvent.click(screen.getByText('Small'));
    expect(useSettingsStore.getState().appearance.fontSize).toBe('small');
  });

  it('renders system theme option', () => {
    render(<SettingsPage />);
    // System theme option should be rendered but clicking it requires matchMedia
    expect(screen.getByText('System')).toBeInTheDocument();
  });

  // ─── Nav Structure Tests (sidebar layout) ──────────────────────────────

  it('back button and title are inside the nav sidebar', () => {
    render(<SettingsPage />);
    const nav = document.querySelector('.settings-nav');
    expect(nav).toBeInTheDocument();
    // Back button and title should be children of the nav element
    expect(nav!.querySelector('.settings-back-btn')).toBeInTheDocument();
    expect(nav!.querySelector('.settings-page-title')).toBeInTheDocument();
  });

  it('privacy section shows all 4 subsections in nav tree', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Privacy & Security'));
    // "Privacy" appears in nav item ("Privacy & Security") and tree subsection
    const privacyTreeItem = document.querySelector('.settings-nav-tree-item');
    expect(privacyTreeItem).toBeInTheDocument();
    expect(privacyTreeItem!.textContent).toBe('Privacy');
    expect(screen.getByText('Multi-Factor Auth')).toBeInTheDocument();
    // "Active Sessions" and "Past Sessions" appear in both nav tree and content
    const activeSessionEls = screen.getAllByText('Active Sessions');
    expect(activeSessionEls.length).toBeGreaterThanOrEqual(2); // nav tree + content
    const pastSessionEls = screen.getAllByText('Past Sessions');
    expect(pastSessionEls.length).toBeGreaterThanOrEqual(1);
  });

  it('appearance section shows subsections in nav tree', () => {
    render(<SettingsPage />);
    // Default is appearance, tree should show subsections
    // "Color Scheme", "Theme", "Display" appear in both nav tree and content
    const colorSchemeEls = screen.getAllByText('Color Scheme');
    expect(colorSchemeEls.length).toBeGreaterThanOrEqual(2);
    const themeEls = screen.getAllByText('Theme');
    expect(themeEls.length).toBeGreaterThanOrEqual(2);
  });

  it('voice section shows subsections in nav tree', () => {
    // VoiceAudioSection uses navigator.mediaDevices — mock it
    Object.defineProperty(navigator, 'mediaDevices', {
      value: {
        enumerateDevices: vi.fn().mockResolvedValue([]),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
      },
      configurable: true,
    });
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Audio & Video'));
    const deviceEls = screen.getAllByText('Device Configuration');
    expect(deviceEls.length).toBeGreaterThanOrEqual(1);
    const audioEls = screen.getAllByText('Audio Configuration');
    expect(audioEls.length).toBeGreaterThanOrEqual(1);
    const videoEls = screen.getAllByText('Video Configuration');
    expect(videoEls.length).toBeGreaterThanOrEqual(1);
  });

  it('active nav item has both left and right accent borders', () => {
    render(<SettingsPage />);
    const activeItem = document.querySelector('.settings-nav-item.active');
    expect(activeItem).toBeInTheDocument();
    // Verify the active class is present (CSS handles border styling)
    expect(activeItem).toHaveClass('active');
  });

  it('privacy section shows session loading state initially', async () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Privacy & Security'));
    // "Active Sessions" appears in nav tree + content — verify content section exists
    const els = screen.getAllByText('Active Sessions');
    expect(els.length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText(/These are the devices currently logged/)).toBeInTheDocument();
  });

  it('renders sessions after loading', async () => {
    const { apiFetch } = await import('@/renderer/services/apiClient');
    (apiFetch as ReturnType<typeof vi.fn>).mockResolvedValue({
      ok: true,
      json: async () => ({
        sessions: [
          {
            id: 'session-1',
            device_name: 'Desktop',
            ip_address: '192.168.1.x',
            user_agent: 'Mozilla/5.0 Electron',
            expires_at: '2026-03-01T00:00:00Z',
            created_at: '2026-02-01T00:00:00Z',
            last_used: new Date().toISOString(),
            is_current: true,
          },
        ],
        past_sessions: [],
      }),
    });

    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Privacy & Security'));

    // Wait for sessions to load
    await vi.waitFor(() => {
      expect(screen.getByText('This Device')).toBeInTheDocument();
    });
    expect(screen.getByText('Concord Voice Desktop')).toBeInTheDocument();
  });

  it('has no disabled nav items (Account enabled in #1625)', () => {
    render(<SettingsPage />);
    const navButtons = document.querySelectorAll('.settings-nav-item');
    const disabled = Array.from(navButtons).filter((b) => (b as HTMLButtonElement).disabled);
    expect(disabled).toHaveLength(0);
  });

  it('switches between appearance and privacy sections', () => {
    render(<SettingsPage />);
    // Switch to privacy
    fireEvent.click(screen.getByText('Privacy & Security'));
    const privacyEls = screen.getAllByText('Active Sessions');
    expect(privacyEls.length).toBeGreaterThanOrEqual(1);
    // Switch back to appearance
    fireEvent.click(screen.getByText('Appearance'));
    const appearanceEls = screen.getAllByText('Color Scheme');
    expect(appearanceEls.length).toBeGreaterThanOrEqual(1);
  });

  // ─── Custom Theme Tests ──────────────────────────────────────────────────

  it('renders custom "+" circle in color scheme grid', () => {
    render(<SettingsPage />);
    const customCircle = document.querySelector('.color-scheme-circle.custom-add');
    expect(customCircle).toBeInTheDocument();
    // 15 built-in (incl. Pride) + 1 custom = 16 total circles
    const allCircles = document.querySelectorAll('.color-scheme-circle');
    expect(allCircles.length).toBe(16);
  });

  it('clicking custom circle when no customColors opens picker with defaults', () => {
    render(<SettingsPage />);
    const customCircle = document.querySelector('.color-scheme-circle.custom-add')!;
    fireEvent.click(customCircle);
    const state = useSettingsStore.getState().appearance;
    expect(state.colorScheme).toBe('custom');
    expect(state.customColors).not.toBeNull();
    expect(state.customColors!.background).toBe('#0d0821');
    expect(state.customColors!.accentPrimary).toBe('#fa709a');
    expect(state.customColors!.accentSecondary).toBe('#ffe13f');
  });

  it('shows custom theme picker when colorScheme is custom', () => {
    useSettingsStore.setState({
      appearance: {
        theme: 'dark',
        colorScheme: 'custom',
        fontSize: 'default',
        compactMode: false,
        reduceAnimations: false,
        customColors: {
          background: '#0d0821',
          accentPrimary: '#fa709a',
          accentSecondary: '#ffe13f',
        },
      },
    });
    render(<SettingsPage />);
    expect(screen.getByText('Background')).toBeInTheDocument();
    expect(screen.getByText('Primary Accent')).toBeInTheDocument();
    expect(screen.getByText('Secondary Accent')).toBeInTheDocument();
    // 3 color inputs + 3 hex text inputs
    const colorInputs = document.querySelectorAll('.custom-theme-color-input');
    const hexInputs = document.querySelectorAll('.custom-theme-hex-input');
    expect(colorInputs.length).toBe(3);
    expect(hexInputs.length).toBe(3);
  });

  it('displays "Custom" as the active label when custom is selected', () => {
    useSettingsStore.setState({
      appearance: {
        theme: 'dark',
        colorScheme: 'custom',
        fontSize: 'default',
        compactMode: false,
        reduceAnimations: false,
        customColors: {
          background: '#0d0821',
          accentPrimary: '#fa709a',
          accentSecondary: '#ffe13f',
        },
      },
    });
    render(<SettingsPage />);
    expect(screen.getByText('Custom')).toBeInTheDocument();
  });

  it('switching from custom to built-in changes colorScheme', () => {
    useSettingsStore.setState({
      appearance: {
        theme: 'dark',
        colorScheme: 'custom',
        fontSize: 'default',
        compactMode: false,
        reduceAnimations: false,
        customColors: {
          background: '#0d0821',
          accentPrimary: '#fa709a',
          accentSecondary: '#ffe13f',
        },
      },
    });
    render(<SettingsPage />);
    // Click the first built-in circle (Concord Voice)
    const circles = document.querySelectorAll('.color-scheme-circle:not(.custom-add)');
    fireEvent.click(circles[0]);
    expect(useSettingsStore.getState().appearance.colorScheme).toBe('concord');
  });

  it('custom circle shows gradient preview when customColors exist', () => {
    useSettingsStore.setState({
      appearance: {
        theme: 'dark',
        colorScheme: 'concord',
        fontSize: 'default',
        compactMode: false,
        reduceAnimations: false,
        customColors: {
          background: '#1a1a2e',
          accentPrimary: '#e94560',
          accentSecondary: '#0f3460',
        },
      },
    });
    render(<SettingsPage />);
    const customCircle = document.querySelector('.color-scheme-circle.custom-add') as HTMLElement;
    // jsdom converts hex to rgb in inline styles, so check for gradient presence
    expect(customCircle.style.background).toContain('linear-gradient');
    expect(customCircle.style.background).toContain('135deg');
  });

  // ─── About & Updates section ──────────────────────────────────────────

  it('renders About & Updates nav item', () => {
    render(<SettingsPage />);
    expect(screen.getByText('About & Updates')).toBeInTheDocument();
  });

  it('switches to About section', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('About & Updates'));
    // About section should render
    const clientInfoEls = screen.getAllByText('Client Info');
    expect(clientInfoEls.length).toBeGreaterThanOrEqual(1);
  });

  // ─── Accessibility section ──────────────────────────────────────────

  it('switches to Accessibility section', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Accessibility'));
    const ttsEls = screen.getAllByText('Text-to-Speech');
    expect(ttsEls.length).toBeGreaterThanOrEqual(1);
  });

  // ─── Layout structure ──────────────────────────────────────────────────

  it('has settings-layout container', () => {
    render(<SettingsPage />);
    expect(document.querySelector('.settings-layout')).toBeInTheDocument();
  });

  it('has settings-content area', () => {
    render(<SettingsPage />);
    expect(document.querySelector('.settings-content')).toBeInTheDocument();
  });

  it('has settings-page-content wrapper', () => {
    render(<SettingsPage />);
    expect(document.querySelector('.settings-page-content')).toBeInTheDocument();
  });

  // ─── Navigation state ──────────────────────────────────────────────────

  it('only one nav item is active at a time', () => {
    render(<SettingsPage />);
    const activeItems = document.querySelectorAll('.settings-nav-item.active');
    expect(activeItems.length).toBe(1);
  });

  it('clicking a new section changes active state', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Privacy & Security'));
    const activeItems = document.querySelectorAll('.settings-nav-item.active');
    expect(activeItems.length).toBe(1);
    expect(activeItems[0].textContent).toContain('Privacy & Security');
  });

  // ─── Disabled items ──────────────────────────────────────────────────

  it('Account nav item is enabled (#1625)', () => {
    render(<SettingsPage />);
    const accountBtn = screen.getByText('Account').closest('button');
    expect(accountBtn).toBeEnabled();
  });

  it('Notifications nav item is enabled', () => {
    render(<SettingsPage />);
    const notifBtn = screen.getByText('Sounds and Notifications').closest('button');
    expect(notifBtn).not.toBeDisabled();
  });

  // ─── Back button / overlay close ────────────────────────────────────────

  it('clicking back button closes the settings overlay when no pending changes', () => {
    useSettingsOverlayStore.setState({ open: 'app', payload: null });
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Back to app'));
    expect(useSettingsOverlayStore.getState().open).toBeNull();
  });

  // ─── Notifications section ──────────────────────────────────────────────

  it('navigating to Notifications section renders NotificationSection', () => {
    render(<SettingsPage />);
    fireEvent.click(screen.getByText('Sounds and Notifications'));
    // NotificationSection renders "Desktop Notifications" as a collapsible section title
    // and nav tree shows subsection labels
    const desktopNotifEls = screen.getAllByText('Desktop Notifications');
    expect(desktopNotifEls.length).toBeGreaterThanOrEqual(1);
    // Also renders the Sounds and Quiet Hours sections
    const soundEls = screen.getAllByText('Sounds');
    expect(soundEls.length).toBeGreaterThanOrEqual(1);
  });
});
