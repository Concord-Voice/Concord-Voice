import { describe, it, expect, vi, beforeEach } from 'vitest';
import { useSettingsStore, migratePersistedSettings } from '@/renderer/stores/settingsStore';
import { DEFAULT_CLIENT_BEHAVIOR } from '@/shared/clientBehavior';

const { mockSetClientBehavior } = vi.hoisted(() => ({
  mockSetClientBehavior: vi.fn().mockResolvedValue(undefined),
}));

beforeEach(() => {
  vi.clearAllMocks();
  (window as unknown as { electron: unknown }).electron = {
    window: { setClientBehavior: mockSetClientBehavior },
  };
  // Reset the clientBehavior slice to defaults
  useSettingsStore.setState({ clientBehavior: DEFAULT_CLIENT_BEHAVIOR });
});

describe('settingsStore — clientBehavior slice', () => {
  it('has default clientBehavior on init', () => {
    // #1099: the tray exists — default [X] hides to it (close-to-tray, the
    // behavior #806 designed for). The #1383 interim default is retired.
    const cb = useSettingsStore.getState().clientBehavior;
    expect(cb).toEqual({ toTray: 'close', toToolbar: 'minimize' });
  });

  it('updates clientBehavior when setClientBehavior is called', () => {
    useSettingsStore.getState().setClientBehavior({
      toTray: 'minimize',
      toToolbar: 'close',
    });
    expect(useSettingsStore.getState().clientBehavior).toEqual({
      toTray: 'minimize',
      toToolbar: 'close',
    });
  });

  it('pushes the new value to main via IPC', () => {
    useSettingsStore.getState().setClientBehavior({
      toTray: 'none',
      toToolbar: 'minimize',
    });
    expect(mockSetClientBehavior).toHaveBeenCalledWith({
      toTray: 'none',
      toToolbar: 'minimize',
    });
  });
});

describe('settingsStore — persisted-state migration v0→v1 (#1099)', () => {
  it('migrates the exact #1383 interim-default snapshot to the new default', () => {
    const persisted = {
      clientBehavior: { toTray: 'none', toToolbar: 'minimize' },
    };
    const result = migratePersistedSettings(persisted, 0) as {
      clientBehavior: { toTray: string; toToolbar: string };
    };
    expect(result.clientBehavior).toEqual({ toTray: 'close', toToolbar: 'minimize' });
  });

  it('passes a customized combo through untouched (deliberate user choice)', () => {
    const persisted = {
      clientBehavior: { toTray: 'minimize', toToolbar: 'close' },
    };
    const result = migratePersistedSettings(persisted, 0) as {
      clientBehavior: { toTray: string; toToolbar: string };
    };
    expect(result.clientBehavior).toEqual({ toTray: 'minimize', toToolbar: 'close' });
  });

  it('does not rewrite state already at version 1+', () => {
    const persisted = {
      clientBehavior: { toTray: 'none', toToolbar: 'minimize' },
    };
    const result = migratePersistedSettings(persisted, 1) as {
      clientBehavior: { toTray: string; toToolbar: string };
    };
    expect(result.clientBehavior).toEqual({ toTray: 'none', toToolbar: 'minimize' });
  });

  it('passes persisted state without a clientBehavior slice through unchanged', () => {
    const persisted = { appearance: { theme: 'dark' } };
    expect(migratePersistedSettings(persisted, 0)).toEqual({ appearance: { theme: 'dark' } });
  });
});
