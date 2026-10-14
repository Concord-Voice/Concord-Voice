import { vi, describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useDMStore } from '@/renderer/stores/chat/dmStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel } from '../../mocks/fixtures';

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    decryptMessage: vi.fn((content: string) => Promise.resolve(content)),
    hasKey: vi.fn().mockReturnValue(false),
    invalidateChannelKey: vi.fn(),
    revokeChannelAccess: vi.fn(),
    isInitialized: false,
  },
}));
vi.mock('@/renderer/services/system/ttsService', () => ({ speak: vi.fn() }));
vi.mock('@/renderer/services/system/preferencesSync', () => ({
  preferencesSyncService: { fetchAndApply: vi.fn() },
}));
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({
    ok: true,
    json: () => Promise.resolve({ participants: [] }),
  }),
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

import { useWebSocketMessages } from '@/renderer/hooks/messaging/useWebSocketMessages';
import { createMockWsService } from '../../helpers/wsServiceMock';

const channelEvent = (over: Record<string, unknown> = {}) => ({
  type: 'expiration_event' as const,
  data: {
    id: '11111111-1111-4111-8111-111111111111',
    channel_id: '33333333-3333-4333-8333-333333333333',
    actor_user_id: '66666666-6666-4666-8666-666666666666',
    actor_username: 'alice',
    actor_display_name: 'Alice',
    kind: 'set' as const,
    window_seconds: 86400,
    created_at: '2026-09-16T12:00:00.000Z',
    revision: 5,
    updated_at: '2026-09-16T12:00:00.000Z',
    backfill_pending: false,
    ...over,
  },
});

const dmEvent = (over: Record<string, unknown> = {}) => ({
  type: 'dm_expiration_event' as const,
  data: {
    id: '22222222-2222-4222-8222-222222222222',
    conversation_id: '44444444-4444-4444-8444-444444444444',
    actor_user_id: '66666666-6666-4666-8666-666666666666',
    actor_username: 'alice',
    actor_display_name: 'Alice',
    kind: 'cleared' as const,
    window_seconds: null,
    created_at: '2026-09-16T12:00:00.000Z',
    revision: 6,
    updated_at: '2026-09-16T12:00:00.000Z',
    backfill_pending: false,
    ...over,
  },
});

const rowFor = (scopeId: string, id: string) =>
  useChatStore
    .getState()
    .messagesByChannel.get(scopeId)
    ?.find((message) => message.id === id);

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
  // The channel id must be UUID-shaped because the event fixtures are: the wire schema
  // types channel_id as z.string().uuid(), and a fixture the real boundary would reject
  // proves nothing about whether a server payload reaches the handler.
  useChannelStore
    .getState()
    .addChannel({ ...mockChannel, id: '33333333-3333-4333-8333-333333333333' });
  useChatStore.setState({ isConnected: true });
  useUserStore.setState({
    user: {
      id: '55555555-5555-4555-8555-555555555555',
      username: 'testuser',
      email: 'test@test.com',
    } as never,
  });
});

describe('useWebSocketMessages — expiration system rows', () => {
  it('registers both scopes', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    expect(ws.handlers.has('expiration_event')).toBe(true);
    expect(ws.handlers.has('dm_expiration_event')).toBe(true);
  });

  it('inserts a channel system row carrying BOTH the discriminator and the payload', () => {
    // MessageList's dispatch branch requires both. A row with one of them falls
    // through to the ordinary renderer, which then attempts an E2EE decrypt on
    // content that was never encrypted.
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    act(() => ws.handlers.get('expiration_event')!(channelEvent()));

    const row = rowFor(
      '33333333-3333-4333-8333-333333333333',
      '11111111-1111-4111-8111-111111111111'
    );
    expect(row?.type).toBe('expiration_event');
    expect(row?.expiration_event_payload).toEqual({
      kind: 'set',
      actor_user_id: '66666666-6666-4666-8666-666666666666',
      window_seconds: 86400,
      changed_at: '2026-09-16T12:00:00.000Z',
    });
    expect(row?.content).toBe('');
  });

  it('carries the actor name the broadcast resolved, since the row has no users JOIN', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    act(() => ws.handlers.get('expiration_event')!(channelEvent()));
    expect(
      rowFor('33333333-3333-4333-8333-333333333333', '11111111-1111-4111-8111-111111111111')
        ?.display_name
    ).toBe('Alice');
    expect(
      rowFor('33333333-3333-4333-8333-333333333333', '11111111-1111-4111-8111-111111111111')
        ?.username
    ).toBe('alice');
  });

  it('degrades an unresolved actor to undefined rather than an empty name', () => {
    // The server returns empty strings when its lookup fails; '' would render as
    // a blank author instead of the renderer's "Someone" fallback.
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    act(() =>
      ws.handlers.get('expiration_event')!(
        channelEvent({ actor_username: '', actor_display_name: '' })
      )
    );
    expect(
      rowFor('33333333-3333-4333-8333-333333333333', '11111111-1111-4111-8111-111111111111')
        ?.display_name
    ).toBeUndefined();
  });

  it('advances the channel policy so the composer indicator matches the row', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    act(() => ws.handlers.get('expiration_event')!(channelEvent()));
    const channel = useChannelStore
      .getState()
      .channels.find((item) => item.id === '33333333-3333-4333-8333-333333333333');
    expect(channel?.expirationPolicy).toEqual({
      windowSeconds: 86400,
      updatedAt: '2026-09-16T12:00:00.000Z',
      revision: 5,
      backfillPending: false,
    });
  });

  it('ignores a replayed event rather than regressing the indicator', () => {
    // mergeExpirationPolicy fences on revision, which is the whole reason the
    // policy travels with the event instead of being inferred from window_seconds.
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    act(() => ws.handlers.get('expiration_event')!(channelEvent({ revision: 9 })));
    act(() =>
      ws.handlers.get('expiration_event')!(
        channelEvent({ id: 'evt-old', revision: 2, window_seconds: 3600 })
      )
    );
    const channel = useChannelStore
      .getState()
      .channels.find((item) => item.id === '33333333-3333-4333-8333-333333333333');
    expect(channel?.expirationPolicy?.revision).toBe(9);
    expect(channel?.expirationPolicy?.windowSeconds).toBe(86400);
  });

  it('does not double the row when the actor receives their own broadcast', () => {
    // The server excludes nobody from this broadcast, and the actor also refetches.
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    act(() => ws.handlers.get('expiration_event')!(channelEvent()));
    act(() => ws.handlers.get('expiration_event')!(channelEvent()));
    const rows = useChatStore
      .getState()
      .messagesByChannel.get('33333333-3333-4333-8333-333333333333')
      ?.filter((message) => message.id === '11111111-1111-4111-8111-111111111111');
    expect(rows).toHaveLength(1);
  });

  it('inserts a DM clear row and turns the conversation policy off', () => {
    useDMStore.setState({
      conversations: [
        {
          id: '44444444-4444-4444-8444-444444444444',
          name: 'Alice',
          isGroup: false,
          isPersonal: false,
          participants: [
            {
              userId: '55555555-5555-4555-8555-555555555555',
              username: 'testuser',
              role: 'member',
            },
          ],
          expirationPolicy: {
            windowSeconds: 86400,
            updatedAt: '2026-09-15T12:00:00.000Z',
            revision: 5,
            backfillPending: false,
          },
        } as never,
      ],
    });
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    act(() => ws.handlers.get('dm_expiration_event')!(dmEvent()));

    expect(
      rowFor('44444444-4444-4444-8444-444444444444', '22222222-2222-4222-8222-222222222222')?.type
    ).toBe('expiration_event');
    expect(
      rowFor('44444444-4444-4444-8444-444444444444', '22222222-2222-4222-8222-222222222222')
        ?.expiration_event_payload?.kind
    ).toBe('cleared');
    const conversation = useDMStore
      .getState()
      .conversations.find((item) => item.id === '44444444-4444-4444-8444-444444444444');
    expect(conversation?.expirationPolicy?.windowSeconds).toBeNull();
    expect(conversation?.expirationPolicy?.revision).toBe(6);
  });

  // ── #3459: the pin fields reach BOTH the row and the policy, from the same event ──
  describe('pin fields (#3459)', () => {
    const CHANNEL = '33333333-3333-4333-8333-333333333333';
    const ROW = '11111111-1111-4111-8111-111111111111';
    const policyOf = () =>
      useChannelStore.getState().channels.find((item) => item.id === CHANNEL)?.expirationPolicy;

    it('copies the three fields onto the row so MessageList can tell a pins-only change apart', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));
      act(() =>
        ws.handlers.get('expiration_event')!(
          channelEvent({
            kind: 'changed',
            include_pinned: false,
            previous_include_pinned: true,
            previous_window_seconds: 86400,
          })
        )
      );
      expect(rowFor(CHANNEL, ROW)?.expiration_event_payload).toEqual({
        kind: 'changed',
        actor_user_id: '66666666-6666-4666-8666-666666666666',
        window_seconds: 86400,
        previous_window_seconds: 86400,
        include_pinned: false,
        previous_include_pinned: true,
        changed_at: '2026-09-16T12:00:00.000Z',
      });
    });

    it('advances the policy WITH the claim in the same tick (so the composer cannot lag the row)', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));
      act(() => ws.handlers.get('expiration_event')!(channelEvent({ include_pinned: false })));
      expect(policyOf()).toStrictEqual({
        windowSeconds: 86400,
        updatedAt: '2026-09-16T12:00:00.000Z',
        revision: 5,
        backfillPending: false,
        includePinned: false,
      });
    });

    it('leaves every pin key ABSENT for an event from a server that predates #3459', () => {
      // An explicit `includePinned: undefined` would be a second shape for "the server did not
      // say", and the equal-revision merge reads absence.
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));
      act(() => ws.handlers.get('expiration_event')!(channelEvent()));
      expect(policyOf()).not.toHaveProperty('includePinned');
      const payload = rowFor(CHANNEL, ROW)?.expiration_event_payload;
      expect(payload).not.toHaveProperty('include_pinned');
      expect(payload).not.toHaveProperty('previous_include_pinned');
      expect(payload).not.toHaveProperty('previous_window_seconds');
    });

    it('a REPLAYED event at the same revision cannot re-add a claim the policy lost (X14)', () => {
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));
      // A claim arrives, then an older server answers at the SAME revision without it.
      act(() => ws.handlers.get('expiration_event')!(channelEvent({ include_pinned: false })));
      expect(policyOf()?.includePinned).toBe(false);
      act(() => ws.handlers.get('expiration_event')!(channelEvent({ id: 'rollback' })));
      expect(policyOf()).not.toHaveProperty('includePinned');
      // Presence is never added at an equal revision: replaying the claimed event does nothing.
      act(() => ws.handlers.get('expiration_event')!(channelEvent({ include_pinned: false })));
      expect(policyOf()).not.toHaveProperty('includePinned');
      // The NEXT revision carries it again.
      act(() =>
        ws.handlers.get('expiration_event')!(channelEvent({ revision: 6, include_pinned: false }))
      );
      expect(policyOf()?.includePinned).toBe(false);
    });

    it('carries the same fields on the DM scope', () => {
      useDMStore.setState({
        conversations: [
          {
            id: '44444444-4444-4444-8444-444444444444',
            name: 'Alice',
            isGroup: false,
            isPersonal: false,
            participants: [],
            expirationPolicy: {
              windowSeconds: 86400,
              updatedAt: '2026-09-15T12:00:00.000Z',
              revision: 5,
              backfillPending: false,
              includePinned: true,
            },
          } as never,
        ],
      });
      const ws = createMockWsService();
      renderHook(() => useWebSocketMessages(ws as never));
      act(() =>
        ws.handlers.get('dm_expiration_event')!(
          dmEvent({
            kind: 'changed',
            window_seconds: 86400,
            revision: 6,
            include_pinned: false,
            previous_include_pinned: true,
            previous_window_seconds: 86400,
          })
        )
      );
      const conversation = useDMStore
        .getState()
        .conversations.find((item) => item.id === '44444444-4444-4444-8444-444444444444');
      expect(conversation?.expirationPolicy?.includePinned).toBe(false);
      expect(
        rowFor('44444444-4444-4444-8444-444444444444', '22222222-2222-4222-8222-222222222222')
          ?.expiration_event_payload
      ).toMatchObject({ include_pinned: false, previous_include_pinned: true });
    });
  });
});
