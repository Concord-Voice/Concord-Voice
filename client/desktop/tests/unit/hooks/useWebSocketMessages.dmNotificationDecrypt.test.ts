/**
 * Regression guard for #1715 — macOS DM notifications must show the DECRYPTED
 * message preview, never the E2EE ciphertext.
 *
 * DM notifications flow: dm_message handler -> addEncryptedMessage (decrypts via
 * e2eeService) -> onPlaintext(text) -> notifyDMMessagePreview(..., text, ...) ->
 * desktopNotificationService.notify({ body: text }). The raw wire content
 * (ciphertext) is only ever *input* to the decrypt; it must never reach notify().
 * A decrypt failure degrades to a safe (non-ciphertext) placeholder.
 *
 * The runtime fix landed incidentally in #1991 (commit cba0e3910, 2026-07-01);
 * this is the missing regression guard for that privacy-critical behavior
 * (risk: privacy / domain: e2ee), per the #1715 acceptance criteria. It asserts
 * the notify() boundary directly, so a future refactor of the notify path
 * re-leaking ciphertext fails here loudly.
 */
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useDMStore } from '@/renderer/stores/chat/dmStore';
import { useNotificationStore } from '@/renderer/stores/ui/notificationStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel } from '../../mocks/fixtures';

const mockDecryptForChannel = vi.fn();
const mockDecryptForChannelWithVersion = vi.fn();

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    invalidateChannelKey: vi.fn(),
    revokeChannelAccess: vi.fn(),
    processPendingKeyRequests: vi.fn().mockResolvedValue(undefined),
    decryptForChannel: (...args: unknown[]) => mockDecryptForChannel(...args),
    decryptForChannelWithVersion: (...args: unknown[]) => mockDecryptForChannelWithVersion(...args),
  },
}));

vi.mock('@/renderer/services/system/ttsService', () => ({ speak: vi.fn() }));
vi.mock('@/renderer/services/system/preferencesSync', () => ({
  preferencesSyncService: { fetchAndApply: vi.fn() },
}));
vi.mock('@/renderer/services/system/notificationSoundService', () => ({
  notificationSoundService: {
    play: vi.fn(),
    playLoop: vi.fn(),
    stopLoop: vi.fn(),
    stopAllLoops: vi.fn(),
    isLooping: vi.fn().mockReturnValue(false),
    init: vi.fn(),
  },
}));
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi
    .fn()
    .mockResolvedValue({ ok: true, json: () => Promise.resolve({ participants: [] }) }),
}));

import { useWebSocketMessages } from '@/renderer/hooks/messaging/useWebSocketMessages';
import { createMockWsService } from '../../helpers/wsServiceMock';
import {
  desktopNotificationService,
  applyContentPrivacy,
} from '@/renderer/services/system/desktopNotificationService';

// Stands in for the encrypted wire content — if any part of it reaches
// notify(), the privacy contract is broken. Deliberately a low-entropy
// hyphenated phrase (not a base64-looking blob) so detect-secrets doesn't
// flag it; the assertions only need a value distinct from the plaintext.
const CIPHERTEXT = 'encrypted-dm-wire-content-that-must-never-be-shown';
const PLAINTEXT = 'Hey, are we still on for lunch?';

function dmEvent(content: string, keyVersion?: number) {
  return {
    type: 'dm_message',
    data: {
      id: 'dm-msg-1',
      conversation_id: 'conv-1',
      user_id: 'user-2', // not self -> notification surfaces
      username: 'alice',
      display_name: 'Alice',
      content,
      // key_version > 1 selects the versioned decrypt path (CSK rotation); omitted
      // (undefined) selects the plain decryptForChannel path.
      ...(keyVersion !== undefined ? { key_version: keyVersion } : {}),
      created_at: '2025-01-01T00:00:00Z',
    },
  };
}

/** Renders the hook, dispatches one dm_message, and flushes the async decrypt. */
async function receiveDM(keyVersion?: number): Promise<void> {
  const ws = createMockWsService();
  renderHook(() => useWebSocketMessages(ws as never));
  const handler = ws.handlers.get('dm_message');
  expect(handler).toBeDefined();
  // dm_message decrypts asynchronously — async act flushes the promise microtasks.
  await act(async () => {
    handler!(dmEvent(CIPHERTEXT, keyVersion));
  });
}

let notifySpy: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
  useChannelStore.getState().addChannel(mockChannel);
  useChatStore.setState({ isConnected: true });
  useUserStore.getState().setUser({ id: 'user-1', username: 'testuser' });
  useDMStore.getState().addConversation({
    id: 'conv-1',
    isGroup: false,
    isPersonal: false,
    name: null,
    participants: [],
    iconUrl: undefined,
    createdBy: 'user-2',
    lastMessage: null,
    unreadCount: 0,
    createdAt: '2025-01-01T00:00:00Z',
  });
  // Explicit: the gate that lets a DM desktop notification fire. Kept explicit
  // (not defaults-reliant) because it is load-bearing for this test.
  useNotificationStore.setState({
    desktopNotificationsEnabled: true,
    desktopNotifyDMs: true,
    doNotDisturb: false,
    quietHoursEnabled: false,
    suppressWhenFocused: true,
    notificationContent: 'full',
  });
  vi.clearAllMocks();
  notifySpy = vi.spyOn(desktopNotificationService, 'notify').mockImplementation(() => {});
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('useWebSocketMessages — DM notification decryption (regression #1715)', () => {
  it('(a) shows the DECRYPTED plaintext as the notification body on success', async () => {
    mockDecryptForChannel.mockResolvedValue(PLAINTEXT);

    await receiveDM();

    expect(notifySpy).toHaveBeenCalledTimes(1);
    const opts = notifySpy.mock.calls[0][0] as { body: string; targetType: string };
    expect(opts.body).toBe(PLAINTEXT);
    expect(opts.targetType).toBe('dm');
  });

  it('(b) degrades to a safe, non-ciphertext placeholder when decryption fails', async () => {
    mockDecryptForChannel.mockRejectedValue(new Error('pending key'));

    await receiveDM();

    expect(notifySpy).toHaveBeenCalledTimes(1);
    const opts = notifySpy.mock.calls[0][0] as {
      body: string;
      title: string;
      senderDisplayName: string;
      targetType: 'channel' | 'dm';
      targetId: string;
      senderId: string;
    };
    // The failure path passes an empty body — never ciphertext.
    expect(opts.body).toBe('');
    expect(opts.body).not.toContain(CIPHERTEXT);
    // ...which notify() renders as the safe placeholder via applyContentPrivacy.
    expect(applyContentPrivacy(opts).body).toBe('New encrypted message');
  });

  it('(c) never passes ciphertext to notify() — across success and failure paths', async () => {
    mockDecryptForChannel.mockResolvedValue(PLAINTEXT);
    await receiveDM();

    mockDecryptForChannel.mockRejectedValue(new Error('no key'));
    await receiveDM();

    expect(notifySpy).toHaveBeenCalledTimes(2);
    for (const call of notifySpy.mock.calls) {
      const opts = call[0] as { body: string; title: string };
      expect(opts.body).not.toContain(CIPHERTEXT);
      expect(opts.title).not.toContain(CIPHERTEXT);
    }
    // Sanity: the ciphertext WAS the wire input the decrypt consumed.
    expect(mockDecryptForChannel).toHaveBeenCalledWith('conv-1', CIPHERTEXT);
  });

  it('(d) uses the versioned decrypt path for key_version > 1 and stays fail-closed', async () => {
    // A DM sent after a channel-secret-key rotation carries key_version > 1, so
    // addEncryptedMessage routes through decryptForChannelWithVersion, NOT the
    // plain decryptForChannel branch. This is the higher-risk leak surface: a
    // removed member holding a stale epoch gets a server-side 403 from
    // getChannelKeyByVersion, so the decrypt must reject and the notification
    // must fail closed — never surfacing ciphertext.

    // Success on the versioned path -> decrypted body, versioned decrypt used.
    mockDecryptForChannelWithVersion.mockResolvedValue(PLAINTEXT);
    await receiveDM(2);
    expect(mockDecryptForChannelWithVersion).toHaveBeenCalledWith('conv-1', CIPHERTEXT, 2);
    expect(mockDecryptForChannel).not.toHaveBeenCalled();
    expect(notifySpy).toHaveBeenCalledTimes(1);
    expect((notifySpy.mock.calls[0][0] as { body: string }).body).toBe(PLAINTEXT);

    // Stale-epoch / removed-member rejection on the versioned path -> fail closed.
    mockDecryptForChannelWithVersion.mockRejectedValue(new Error('stale epoch (403)'));
    await receiveDM(2);
    expect(notifySpy).toHaveBeenCalledTimes(2);
    const failBody = (notifySpy.mock.calls[1][0] as { body: string }).body;
    expect(failBody).toBe('');
    expect(failBody).not.toContain(CIPHERTEXT);
  });
});
