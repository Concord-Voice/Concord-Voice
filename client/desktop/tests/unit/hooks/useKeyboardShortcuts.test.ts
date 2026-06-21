import { describe, it, expect, beforeEach, vi } from 'vitest';
import { renderHook } from '@testing-library/react';

// ── Mocks ──────────────────────────────────────────────────────────────

const { mockInit, mockDestroy, mockRegisterHandler } = vi.hoisted(() => ({
  mockInit: vi.fn(),
  mockDestroy: vi.fn(),
  mockRegisterHandler: vi.fn(),
}));

vi.mock('../../../src/renderer/services/keyboardShortcutService', () => ({
  keyboardShortcutService: {
    init: mockInit,
    destroy: mockDestroy,
    registerHandler: mockRegisterHandler,
    unregisterHandler: vi.fn(),
  },
}));

const mockNavigate = vi.fn();
vi.mock('react-router-dom', () => ({
  useNavigate: () => mockNavigate,
}));

// Store mocks — using stable mock functions so we can assert on them
const mockOpenChannelSwitcher = vi.fn();
const mockCloseOverlay = vi.fn();
const mockCloseChannelSwitcher = vi.fn();
const mockToggleOverlay = vi.fn();
const mockSetActiveChannel = vi.fn();
const mockSetMuted = vi.fn();
const mockSetDeafened = vi.fn();

let mockShortcutStoreState: Record<string, unknown> = {};
let mockChannelStoreState: Record<string, unknown> = {};
let mockUnreadStoreState: Record<string, unknown> = {};
let mockVoiceStoreState: Record<string, unknown> = {};

vi.mock('../../../src/renderer/stores/keyboardShortcutStore', () => ({
  useKeyboardShortcutStore: {
    getState: () => mockShortcutStoreState,
  },
}));

vi.mock('../../../src/renderer/stores/channelStore', () => ({
  useChannelStore: {
    getState: () => mockChannelStoreState,
  },
}));

vi.mock('../../../src/renderer/stores/unreadStore', () => ({
  useUnreadStore: {
    getState: () => mockUnreadStoreState,
  },
}));

vi.mock('../../../src/renderer/stores/voiceStore', () => ({
  useVoiceStore: {
    getState: () => mockVoiceStoreState,
  },
}));

import { useKeyboardShortcuts } from '../../../src/renderer/hooks/useKeyboardShortcuts';

// ── Helpers ────────────────────────────────────────────────────────────

/** Get the handler registered for a given shortcut ID */
function getHandler(id: string): () => void {
  const call = mockRegisterHandler.mock.calls.find((c: [string, () => void]) => c[0] === id);
  if (!call) throw new Error(`No handler registered for "${id}"`);
  return call[1];
}

// ── Tests ──────────────────────────────────────────────────────────────

describe('useKeyboardShortcuts', () => {
  beforeEach(() => {
    vi.clearAllMocks();

    mockShortcutStoreState = {
      openChannelSwitcher: mockOpenChannelSwitcher,
      closeOverlay: mockCloseOverlay,
      closeChannelSwitcher: mockCloseChannelSwitcher,
      toggleOverlay: mockToggleOverlay,
      overlayOpen: false,
      channelSwitcherOpen: false,
    };

    mockChannelStoreState = {
      channels: [
        { id: 'ch-1', name: 'general', type: 'text' },
        { id: 'ch-2', name: 'random', type: 'text' },
        { id: 'ch-3', name: 'dev', type: 'text' },
      ],
      activeChannelId: 'ch-2',
      setActiveChannel: mockSetActiveChannel,
    };

    mockUnreadStoreState = {
      unreadCounts: new Map([
        ['ch-1', 3],
        ['ch-3', 1],
      ]),
    };

    mockVoiceStoreState = {
      activeChannelId: 'voice-1',
      isMuted: false,
      isDeafened: false,
      setMuted: mockSetMuted,
      setDeafened: mockSetDeafened,
    };
  });

  // ── Lifecycle ──────────────────────────────────────────────────────

  it('calls init on mount', () => {
    renderHook(() => useKeyboardShortcuts());
    expect(mockInit).toHaveBeenCalledOnce();
  });

  it('calls destroy on unmount', () => {
    const { unmount } = renderHook(() => useKeyboardShortcuts());
    unmount();
    expect(mockDestroy).toHaveBeenCalledOnce();
  });

  it('registers exactly 11 handlers', () => {
    renderHook(() => useKeyboardShortcuts());
    expect(mockRegisterHandler).toHaveBeenCalledTimes(11);
  });

  // ── Handler: channel-switcher ──────────────────────────────────────

  it('channel-switcher opens channel switcher', () => {
    renderHook(() => useKeyboardShortcuts());
    getHandler('channel-switcher')();
    expect(mockOpenChannelSwitcher).toHaveBeenCalled();
  });

  // ── Handler: nav-channel-up ────────────────────────────────────────

  it('nav-channel-up moves to previous text channel', () => {
    renderHook(() => useKeyboardShortcuts());
    getHandler('nav-channel-up')();
    expect(mockSetActiveChannel).toHaveBeenCalledWith('ch-1');
  });

  it('nav-channel-up does nothing when already at first channel', () => {
    mockChannelStoreState.activeChannelId = 'ch-1';
    renderHook(() => useKeyboardShortcuts());
    getHandler('nav-channel-up')();
    expect(mockSetActiveChannel).not.toHaveBeenCalled();
  });

  // ── Handler: nav-channel-down ──────────────────────────────────────

  it('nav-channel-down moves to next text channel', () => {
    renderHook(() => useKeyboardShortcuts());
    getHandler('nav-channel-down')();
    expect(mockSetActiveChannel).toHaveBeenCalledWith('ch-3');
  });

  it('nav-channel-down does nothing when at last channel', () => {
    mockChannelStoreState.activeChannelId = 'ch-3';
    renderHook(() => useKeyboardShortcuts());
    getHandler('nav-channel-down')();
    expect(mockSetActiveChannel).not.toHaveBeenCalled();
  });

  // ── Handler: nav-unread-up ─────────────────────────────────────────

  it('nav-unread-up moves to previous unread channel', () => {
    renderHook(() => useKeyboardShortcuts());
    getHandler('nav-unread-up')();
    // Active is ch-2 (idx 1), ch-1 (idx 0) has unreads
    expect(mockSetActiveChannel).toHaveBeenCalledWith('ch-1');
  });

  it('nav-unread-up wraps around when no unread before current', () => {
    mockChannelStoreState.activeChannelId = 'ch-1';
    renderHook(() => useKeyboardShortcuts());
    getHandler('nav-unread-up')();
    // Active is ch-1 (idx 0), no unread before — wraps to ch-3 (idx 2)
    expect(mockSetActiveChannel).toHaveBeenCalledWith('ch-3');
  });

  // ── Handler: nav-unread-down ───────────────────────────────────────

  it('nav-unread-down moves to next unread channel', () => {
    renderHook(() => useKeyboardShortcuts());
    getHandler('nav-unread-down')();
    // Active is ch-2 (idx 1), ch-3 (idx 2) has unreads
    expect(mockSetActiveChannel).toHaveBeenCalledWith('ch-3');
  });

  it('nav-unread-down wraps around when no unread after current', () => {
    mockChannelStoreState.activeChannelId = 'ch-3';
    renderHook(() => useKeyboardShortcuts());
    getHandler('nav-unread-down')();
    // Active is ch-3 (idx 2), no unread after — wraps to ch-1 (idx 0)
    expect(mockSetActiveChannel).toHaveBeenCalledWith('ch-1');
  });

  // ── Handler: close-modal ───────────────────────────────────────────

  it('close-modal closes overlay when open', () => {
    mockShortcutStoreState.overlayOpen = true;
    renderHook(() => useKeyboardShortcuts());
    getHandler('close-modal')();
    expect(mockCloseOverlay).toHaveBeenCalled();
  });

  it('close-modal closes channel switcher when open', () => {
    mockShortcutStoreState.channelSwitcherOpen = true;
    renderHook(() => useKeyboardShortcuts());
    getHandler('close-modal')();
    expect(mockCloseChannelSwitcher).toHaveBeenCalled();
  });

  // ── Handler: shortcut-overlay ──────────────────────────────────────

  it('shortcut-overlay toggles overlay', () => {
    renderHook(() => useKeyboardShortcuts());
    getHandler('shortcut-overlay')();
    expect(mockToggleOverlay).toHaveBeenCalled();
  });

  // ── Handler: search ─────────────────────────────────────────────────

  it('search dispatches concord:toggle-search event', () => {
    const handler = vi.fn();
    globalThis.addEventListener('concord:toggle-search', handler);
    renderHook(() => useKeyboardShortcuts());
    getHandler('search')();
    expect(handler).toHaveBeenCalled();
    globalThis.removeEventListener('concord:toggle-search', handler);
  });

  // ── Handler: open-settings ─────────────────────────────────────────

  it('open-settings opens the settings overlay', async () => {
    const { useSettingsOverlayStore } = await import('@/renderer/stores/settingsOverlayStore');
    useSettingsOverlayStore.getState().close();
    renderHook(() => useKeyboardShortcuts());
    getHandler('open-settings')();
    expect(useSettingsOverlayStore.getState().open).toBe('app');
  });

  // ── Handler: toggle-mute ───────────────────────────────────────────

  it('toggle-mute calls setMuted when in voice channel', () => {
    renderHook(() => useKeyboardShortcuts());
    getHandler('toggle-mute')();
    expect(mockSetMuted).toHaveBeenCalledWith(true); // isMuted=false → true
  });

  it('toggle-mute does nothing when not in voice', () => {
    mockVoiceStoreState.activeChannelId = null;
    renderHook(() => useKeyboardShortcuts());
    getHandler('toggle-mute')();
    expect(mockSetMuted).not.toHaveBeenCalled();
  });

  // ── Handler: toggle-deafen ─────────────────────────────────────────

  it('toggle-deafen calls setDeafened when in voice channel', () => {
    renderHook(() => useKeyboardShortcuts());
    getHandler('toggle-deafen')();
    expect(mockSetDeafened).toHaveBeenCalledWith(true); // isDeafened=false → true
  });

  it('toggle-deafen does nothing when not in voice', () => {
    mockVoiceStoreState.activeChannelId = null;
    renderHook(() => useKeyboardShortcuts());
    getHandler('toggle-deafen')();
    expect(mockSetDeafened).not.toHaveBeenCalled();
  });
});
