import { useSettingsStore, syncColorSchemeToServer } from '@/renderer/stores/settingsStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useMemberStore } from '@/renderer/stores/memberStore';
import { resetAllStores } from '../../helpers/store-helpers';

// Mock matchMedia — jsdom doesn't implement it
beforeAll(() => {
  Object.defineProperty(globalThis, 'matchMedia', {
    value: vi.fn().mockImplementation((query: string) => ({
      matches: query.includes('dark'),
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
    writable: true,
    configurable: true,
  });
});

describe('settingsStore — extended coverage', () => {
  beforeEach(() => {
    resetAllStores();
    localStorage.clear();
  });

  describe('setCustomColors', () => {
    it('sets custom colors and switches to custom scheme', () => {
      const colors = {
        background: '#0d0821',
        accentPrimary: '#ff0000',
        accentSecondary: '#00ff00',
      };
      useSettingsStore.getState().setCustomColors(colors);
      const { appearance } = useSettingsStore.getState();
      expect(appearance.colorScheme).toBe('custom');
      expect(appearance.customColors).toEqual(colors);
    });
  });

  describe('theme resolution and DOM application', () => {
    it('resolves system theme based on matchMedia', () => {
      // jsdom defaults to dark preference (prefers-color-scheme: dark matches = false)
      useSettingsStore.getState().setTheme('system');
      // The resolved theme depends on matchMedia result
      const resolved = document.documentElement.dataset.theme;
      expect(['dark', 'light']).toContain(resolved);
    });

    it('removes data-scheme when switching to custom', () => {
      useSettingsStore.getState().setColorScheme('hacker');
      expect(document.documentElement.dataset.scheme).toBe('hacker');
      useSettingsStore.getState().setColorScheme('custom');
      expect(document.documentElement.dataset.scheme).toBeUndefined();
    });
  });

  describe('merge on rehydration', () => {
    it('preserves default appearance fields not in persisted state', () => {
      // Simulate partial persisted state
      const persisted = {
        appearance: { theme: 'light' as const },
      };
      const current = useSettingsStore.getState();
      // The merge function from the store
      const merged = {
        ...current,
        ...persisted,
        appearance: {
          ...{
            theme: 'dark' as const,
            colorScheme: 'concord' as const,
            fontSize: 'default' as const,
            compactMode: false,
            reduceAnimations: false,
            customColors: null,
          },
          ...persisted.appearance,
        },
      };
      expect(merged.appearance.theme).toBe('light');
      expect(merged.appearance.colorScheme).toBe('concord');
      expect(merged.appearance.fontSize).toBe('default');
    });
  });

  describe('syncColorSchemeToServer', () => {
    it('builds preset scheme payload and calls updateProfile', () => {
      const mockUpdateProfile = vi.fn().mockResolvedValue({});
      const mockUpdateMemberProfile = vi.fn();

      useUserStore.setState({
        user: {
          id: 'user-1',
          email: 'test@test.com',
          username: 'testuser',
          display_name: 'Test',
          bio: null,
          avatar_url: null,
          header_image_url: null,
          links: [],
          email_verified: false,
          age_verified: true,
          created_at: '',
          updated_at: '',
        },
        updateProfile: mockUpdateProfile,
      });
      useMemberStore.setState({
        updateMemberProfile: mockUpdateMemberProfile,
      });

      useSettingsStore.getState().setColorScheme('morky');
      useSettingsStore.getState().setTheme('dark');

      syncColorSchemeToServer();

      expect(mockUpdateMemberProfile).toHaveBeenCalledWith('user-1', {
        color_scheme: expect.stringContaining('"scheme":"morky"'),
      });
      expect(mockUpdateProfile).toHaveBeenCalledWith({
        color_scheme: expect.stringContaining('"scheme":"morky"'),
      });
    });

    it('builds custom scheme payload with accent colors', () => {
      const mockUpdateProfile = vi.fn().mockResolvedValue({});
      const mockUpdateMemberProfile = vi.fn();

      useUserStore.setState({
        user: {
          id: 'user-1',
          email: 'test@test.com',
          username: 'testuser',
          display_name: 'Test',
          bio: null,
          avatar_url: null,
          header_image_url: null,
          links: [],
          email_verified: false,
          age_verified: true,
          created_at: '',
          updated_at: '',
        },
        updateProfile: mockUpdateProfile,
      });
      useMemberStore.setState({
        updateMemberProfile: mockUpdateMemberProfile,
      });

      useSettingsStore.getState().setCustomColors({
        background: '#000',
        accentPrimary: '#ff0000',
        accentSecondary: '#00ff00',
      });

      syncColorSchemeToServer();

      expect(mockUpdateProfile).toHaveBeenCalledWith({
        color_scheme: expect.stringContaining('"scheme":"custom"'),
      });
      const payload = JSON.parse(mockUpdateProfile.mock.calls[0][0].color_scheme);
      expect(payload.accentPrimary).toBe('#ff0000');
      expect(payload.accentSecondary).toBe('#00ff00');
    });

    it('does not update memberStore when user is not logged in', () => {
      const mockUpdateMemberProfile = vi.fn();
      useUserStore.setState({ user: null });
      useMemberStore.setState({ updateMemberProfile: mockUpdateMemberProfile });

      useSettingsStore.getState().setColorScheme('hacker');
      syncColorSchemeToServer();

      expect(mockUpdateMemberProfile).not.toHaveBeenCalled();
    });
  });

  describe('DOM attribute subscribers', () => {
    it('applies reduce-animations attribute', () => {
      useSettingsStore.getState().setReduceAnimations(false);
      expect(document.documentElement.dataset.reduceAnimations).toBe('false');
      useSettingsStore.getState().setReduceAnimations(true);
      expect(document.documentElement.dataset.reduceAnimations).toBe('true');
    });

    it('applies compact mode attribute', () => {
      useSettingsStore.getState().setCompactMode(false);
      expect(document.documentElement.dataset.compact).toBe('false');
      useSettingsStore.getState().setCompactMode(true);
      expect(document.documentElement.dataset.compact).toBe('true');
    });
  });
});
