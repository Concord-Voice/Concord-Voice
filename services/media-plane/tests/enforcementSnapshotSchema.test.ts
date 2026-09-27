import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';

const SOURCE = readFileSync(new URL('../src/index.ts', import.meta.url), 'utf8')
  .replace(/\/\*[\s\S]*?\*\//g, '')
  .replace(/\/\/[^\n]*/g, '');

describe('versioned server enforcement snapshots', () => {
  it('accepts only the supported version while retaining action-only compatibility', () => {
    const section = (startMarker: string, endMarker: string) => {
      const start = SOURCE.indexOf(startMarker);
      const end = SOURCE.indexOf(endMarker);
      expect(start, `missing source marker: ${startMarker}`).toBeGreaterThanOrEqual(0);
      expect(end, `missing source marker: ${endMarker}`).toBeGreaterThan(start);
      return SOURCE.slice(start, end);
    };
    const handler = section(
      'async function applyServerEnforcementCommand',
      'function getKeyframeSenderUserId'
    );
    const registration = section(
      'function createServerEnforcementHandler',
      'function createUserEnforcementHandler'
    );
    expect(registration).toContain('await applyServerEnforcementCommand({');

    expect(SOURCE).toContain('const SERVER_ENFORCEMENT_SNAPSHOT_VERSION = 1;');
    expect(handler).toContain('natsData.version !== undefined');
    expect(handler).toContain('natsData.version === SERVER_ENFORCEMENT_SNAPSHOT_VERSION');
    expect(handler).toContain("logger.warn('Rejecting malformed server enforcement snapshot'");
    const malformedSnapshot = (() => {
      const startMarker = 'if (carriesSnapshot && !hasSnapshot)';
      const endMarker = 'if (hasSnapshot)';
      const start = handler.indexOf(startMarker);
      const end = handler.indexOf(endMarker);
      expect(start, `missing source marker: ${startMarker}`).toBeGreaterThanOrEqual(0);
      expect(end, `missing source marker: ${endMarker}`).toBeGreaterThan(start);
      return handler.slice(start, end);
    })();
    expect(malformedSnapshot).toContain(
      'await handleForceDisconnect(roomManager, io, channelId, userId, undefined, {\n'
    );
    expect(malformedSnapshot).toContain("reason: 'access_revoked'");
    expect(handler).toContain('const applied = await removeFn(channelId, userId);');
    expect(handler).toContain('if (!applied) return;');
    expect(SOURCE).toContain('const serverEnforcementChains = new Map<string, Promise<void>>();');
    expect(handler).toContain(
      'enqueueServerEnforcement(serverEnforcementChains, channelId, userId'
    );
    expect(handler).toContain(
      'await handleForceDisconnect(roomManager, io, channelId, userId, undefined, {\n'
    );
    expect(handler).toContain("reason: 'access_revoked'");
  });
});
