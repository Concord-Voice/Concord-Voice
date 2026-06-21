import { vi, describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { resetAllStores } from '../helpers/store-helpers';
import { mockUser } from '../mocks/fixtures';

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: { isInitialized: false },
}));
vi.mock('@/renderer/services/ttsService', () => ({ speak: vi.fn() }));
vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: { fetchAndApply: vi.fn() },
}));
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({
    ok: true,
    json: () => Promise.resolve({ participants: [] }),
  }),
}));
vi.mock('@/renderer/services/notificationSoundService', () => ({
  notificationSoundService: {
    play: vi.fn(),
    playLoop: vi.fn(),
    stopLoop: vi.fn(),
    stopAllLoops: vi.fn(),
    isLooping: vi.fn().mockReturnValue(false),
    init: vi.fn(),
  },
}));
vi.mock('@/renderer/services/savedGifsSync', () => ({
  savedGifsSyncService: { fetchAndApply: vi.fn() },
}));
vi.mock('@/renderer/services/searchService', () => ({
  indexMessage: vi.fn(),
}));
vi.mock('@/renderer/services/desktopNotificationService', () => ({
  desktopNotificationService: {
    shouldNotify: vi.fn().mockReturnValue(false),
    notify: vi.fn(),
    incrementBadge: vi.fn(),
  },
}));

import { useWebSocketMessages } from '@/renderer/hooks/useWebSocketMessages';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyHandler = (...args: any[]) => void;

function createMockWsService() {
  const handlers = new Map<string, AnyHandler>();
  return {
    handlers,
    on: vi.fn((type: string, handler: AnyHandler) => {
      handlers.set(type, handler);
      return () => handlers.delete(type);
    }),
    onConnectionChange: vi.fn(() => () => {}),
  };
}

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
  useUserStore.setState({ user: mockUser as never });
});

describe('Voice mute/deafen WS → store update (integration)', () => {
  it('server_muted updates channelVoiceMembers and participants', () => {
    // Seed: voice member in channel, participant in active call
    useVoiceStore.getState().setActiveChannel('ch-1', 'Voice', 'server-1');
    useVoiceStore.getState().setChannelVoiceMembers('ch-1', [
      {
        userId: 'user-2',
        username: 'other',
        isMuted: false,
        serverMuted: false,
        serverDeafened: false,
      },
    ]);
    useVoiceStore.getState().addParticipant({
      userId: 'user-2',
      username: 'other',
      isMuted: false,
      isDeafened: false,
      serverMuted: false,
      serverDeafened: false,
      isVideoOn: false,
      isScreenSharing: false,
      isSpeaking: false,
    });

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    const handler = wsService.handlers.get('voice_state_update');
    expect(handler).toBeDefined();

    act(() => {
      handler!({
        type: 'voice_state_update',
        data: {
          channel_id: 'ch-1',
          action: 'server_muted',
          user_id: 'user-2',
          server_id: 'server-1',
          username: 'other',
        },
      });
    });

    // Sidebar updated
    const members = useVoiceStore.getState().channelVoiceMembers['ch-1'];
    expect(members[0].serverMuted).toBe(true);

    // Active participant updated
    const participant = useVoiceStore.getState().participants['user-2'];
    expect(participant.serverMuted).toBe(true);
  });

  it('server_deafened updates channelVoiceMembers and participants with both flags', () => {
    useVoiceStore.getState().setActiveChannel('ch-1', 'Voice', 'server-1');
    useVoiceStore.getState().setChannelVoiceMembers('ch-1', [
      {
        userId: 'user-2',
        username: 'other',
        isMuted: false,
        serverMuted: false,
        serverDeafened: false,
      },
    ]);
    useVoiceStore.getState().addParticipant({
      userId: 'user-2',
      username: 'other',
      isMuted: false,
      isDeafened: false,
      serverMuted: false,
      serverDeafened: false,
      isVideoOn: false,
      isScreenSharing: false,
      isSpeaking: false,
    });

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    const handler = wsService.handlers.get('voice_state_update');
    act(() => {
      handler!({
        type: 'voice_state_update',
        data: {
          channel_id: 'ch-1',
          action: 'server_deafened',
          user_id: 'user-2',
          server_id: 'server-1',
          username: 'other',
        },
      });
    });

    // Sidebar: both serverDeafened and serverMuted set
    const members = useVoiceStore.getState().channelVoiceMembers['ch-1'];
    expect(members[0].serverDeafened).toBe(true);
    expect(members[0].serverMuted).toBe(true);

    // Participant: both flags set
    const participant = useVoiceStore.getState().participants['user-2'];
    expect(participant.serverDeafened).toBe(true);
    expect(participant.serverMuted).toBe(true);
  });
});
