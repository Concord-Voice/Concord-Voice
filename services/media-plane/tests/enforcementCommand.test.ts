import { describe, expect, it, vi } from 'vitest';
import { handleNatsEnforcementCommand } from '../src/lib/enforcementCommand.js';

const CHANNEL_ID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const USER_ID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';

describe('handleNatsEnforcementCommand', () => {
  it('accepts a valid action and invokes the mutation once', async () => {
    const mutate = vi.fn();
    const emit = vi.fn();
    await handleNatsEnforcementCommand(
      { channelId: CHANNEL_ID, userId: USER_ID, action: 'mute' },
      ['mute', 'unmute'],
      mutate,
      emit
    );
    expect(mutate).toHaveBeenCalledWith({ channelId: CHANNEL_ID, userId: USER_ID, action: 'mute' });
    expect(emit).not.toHaveBeenCalled();
  });

  it('accepts a valid action-less command when actions are not required', async () => {
    const mutate = vi.fn();
    await handleNatsEnforcementCommand(
      { channelId: CHANNEL_ID, userId: USER_ID },
      undefined,
      mutate
    );
    expect(mutate).toHaveBeenCalledWith({ channelId: CHANNEL_ID, userId: USER_ID });
  });

  it('emits one closed failure event without replacing the enforcement error', async () => {
    const enforcementError = new Error('enforcement failed');
    const mutate = vi.fn().mockRejectedValue(enforcementError);
    const emit = vi.fn(() => {
      throw new Error('telemetry failed');
    });

    await expect(
      handleNatsEnforcementCommand(
        { channelId: CHANNEL_ID, userId: USER_ID, action: 'mute' },
        ['mute', 'unmute'],
        mutate,
        emit
      )
    ).rejects.toBe(enforcementError);

    expect(emit).toHaveBeenCalledTimes(1);
    expect(emit).toHaveBeenCalledWith({
      eventType: 'media_integrity',
      outcome: 'failure',
      severity: 'medium',
      reasonCode: 'socket_handler_failed',
    });
  });

  it.each([
    ['missing target', { action: 'mute' }],
    ['wrong target type', { channelId: 1, userId: USER_ID, action: 'mute' }],
    ['socket-like user', { channelId: CHANNEL_ID, userId: 'socket-abc', action: 'mute' }],
    ['whitespace target', { channelId: ` ${CHANNEL_ID}`, userId: USER_ID, action: 'mute' }],
    ['socket-like target', { channelId: 'socket-abc', userId: USER_ID, action: 'mute' }],
    ['uppercase target', { channelId: CHANNEL_ID.toUpperCase(), userId: USER_ID, action: 'mute' }],
    [
      'nil target',
      { channelId: '00000000-0000-0000-0000-000000000000', userId: USER_ID, action: 'mute' },
    ],
    [
      'invalid UUID variant',
      { channelId: '11111111-1111-4111-7111-111111111111', userId: USER_ID, action: 'mute' },
    ],
    [
      'invalid UUID version',
      { channelId: '11111111-1111-0111-8111-111111111111', userId: USER_ID, action: 'mute' },
    ],
    ['oversized target', { channelId: `${CHANNEL_ID}0`, userId: USER_ID, action: 'mute' }],
    ['unknown action', { channelId: CHANNEL_ID, userId: USER_ID, action: 'escalate' }],
  ])('rejects %s with one verdict before a room mutation', async (_label, payload) => {
    const mutate = vi.fn();
    const emit = vi.fn();

    await handleNatsEnforcementCommand(payload, ['mute', 'unmute'], mutate, emit);

    expect(mutate).not.toHaveBeenCalled();
    expect(emit).toHaveBeenCalledTimes(1);
    expect(emit).toHaveBeenCalledWith({
      eventType: 'media_integrity',
      outcome: 'denied',
      severity: 'medium',
      reasonCode: 'media_schema_rejected',
    });
  });
});
