import { describe, expect, it, vi } from 'vitest';
import { VoiceCommandDispatcher } from '../src/lib/voiceCommandDispatcher.js';
import type { RoomManager } from '../src/lib/roomManager.js';
import type { Server } from 'socket.io';

const channelId = '22222222-2222-4222-8222-222222222222';
const userId = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const legacySnapshot = {
  version: 1,
  channelId,
  userId,
  action: 'mute',
  authorizationRevision: '42',
  serverMuted: true,
  serverDeafened: false,
};
const durableOnlyFields = [
  'schema',
  'commandId',
  'streamGeneration',
  'target',
  'sequence',
  'scope',
  'kind',
  'serverId',
  'reason',
] as const;

function dispatcher(legacy: (payload: Record<string, unknown>) => Promise<void>) {
  return new VoiceCommandDispatcher({
    roomManager: {} as RoomManager,
    io: {} as Server,
    nodeBootId: '55555555-5555-4555-8555-555555555555',
    streamGeneration: '11111111-1111-4111-8111-111111111111',
    ensureAuthority: vi.fn(),
    handleLegacy: legacy,
  });
}

describe('VoiceCommandDispatcher compatibility boundary', () => {
  it.each(durableOnlyFields)(
    'rejects incomplete durable %s before legacy mutation',
    async (property) => {
      const handleLegacy = vi.fn(async (_payload: Record<string, unknown>) => undefined);
      const payload = { ...legacySnapshot, [property]: 'durable-marker' };

      await dispatcher(handleLegacy).handleCompatibility(payload);

      expect(handleLegacy, `${property} must never reach legacy mutation`).not.toHaveBeenCalled();
    }
  );

  it('sends a known valid version:1 snapshot to legacy exactly once', async () => {
    const handleLegacy = vi.fn(async (_payload: Record<string, unknown>) => undefined);

    await dispatcher(handleLegacy).handleCompatibility(legacySnapshot);

    expect(handleLegacy).toHaveBeenCalledTimes(1);
    expect(handleLegacy).toHaveBeenCalledWith(legacySnapshot);
  });
});
