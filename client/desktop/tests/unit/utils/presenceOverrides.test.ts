import { parsePresenceOverrides } from '@/renderer/utils/presenceOverrides';

const UUID_A = '11111111-1111-4111-8111-111111111111';
const UUID_B = '22222222-2222-4222-8222-222222222222';

describe('parsePresenceOverrides', () => {
  it('accepts the exact v1 document and canonicalizes IDs', () => {
    expect(
      parsePresenceOverrides({
        v: 1,
        excludedUserIds: [UUID_B, UUID_A, UUID_B],
      })
    ).toEqual({
      v: 1,
      excludedUserIds: [UUID_A, UUID_B],
    });
  });

  it.each([
    { v: 2, excludedUserIds: [] },
    { v: 1, excludedUserIds: 'not-an-array' },
    { v: 1, excludedUserIds: ['not-a-uuid'] },
    { v: 1, excludedUserIds: [], extra: true },
    { v: 1, excludedUserIds: Array.from({ length: 1_001 }, () => UUID_A) },
  ])('rejects a malformed or non-exact document', (raw) => {
    expect(() => parsePresenceOverrides(raw)).toThrow('Invalid presence override document');
  });

  it('keeps validation errors free of identifiers and ciphertext', () => {
    const sentinelId = '33333333-3333-4333-8333-333333333333';
    const sentinelCiphertext = 'sentinel-private-ciphertext';

    let message = '';
    try {
      parsePresenceOverrides({
        v: 1,
        excludedUserIds: [sentinelId, 'malformed'],
        ciphertext: sentinelCiphertext,
      });
    } catch (error) {
      message = error instanceof Error ? error.message : String(error);
    }

    expect(message).toBe('Invalid presence override document');
    expect(message).not.toContain(sentinelId);
    expect(message).not.toContain(sentinelCiphertext);
  });
});
