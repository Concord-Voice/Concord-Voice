import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

const SOURCE = readFileSync(join(__dirname, '..', 'src', 'index.ts'), 'utf8')
  .replace(/\/\*[\s\S]*?\*\//g, '')
  .replace(/\/\/[^\n]*/g, '');

describe('channel promotion rollback call-site contract', () => {
  it('clears socket state and leaves the room when membership commit fails', () => {
    const channelCommit = SOURCE.slice(
      SOURCE.indexOf('function commitChannelAdmission'),
      SOURCE.indexOf('function commitDMAdmission')
    );

    const promoteAt = channelCommit.indexOf('roomManager.promoteChannelParticipant(');
    const rollbackAt = channelCommit.indexOf('rollbackSocketMembership(socket, data, roomId);');
    const throwAt = channelCommit.indexOf('throw error;');

    // The behavioural promotion/retry assertions live beside RoomManager's
    // promotion tests; this guards the integration seam that those tests cannot
    // reach because registerJoinRoomHandler is intentionally private.
    expect(channelCommit).toContain('try {');
    expect(promoteAt).toBeGreaterThanOrEqual(0);
    expect(rollbackAt).toBeGreaterThan(promoteAt);
    expect(throwAt).toBeGreaterThan(rollbackAt);
  });
});
