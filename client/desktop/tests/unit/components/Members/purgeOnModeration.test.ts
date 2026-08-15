import { purgeNotice } from '@/renderer/components/Members/purgeOnModeration';

/**
 * The notice string alone cannot distinguish "no purge was requested" from
 * "a purge happened that this client cannot describe" — both are empty. These
 * lock the `unknownStatus` discriminator that keeps the two apart (#1354).
 */
describe('purgeNotice (#1354)', () => {
  it.each([
    ['completed', 'Alice was banned and their messages were purged.'],
    [
      'skipped_unauthorized',
      'Alice was banned. Their messages were not purged — you do not have permission to purge messages in this server.',
    ],
    [
      'skipped_rate_limited',
      'Alice was banned. Their messages were not purged — the purge limit was not available just now. You can purge them from a channel later.',
    ],
    [
      'failed',
      'Alice was banned. Their messages could not be purged. You can try again from a channel.',
    ],
  ])('describes %s as a known outcome', (status, expected) => {
    expect(purgeNotice('Alice', 'banned', status)).toEqual({
      notice: expected,
      unknownStatus: false,
    });
  });

  it('substitutes the kick verb without changing the shape', () => {
    expect(purgeNotice('Alice', 'kicked', 'completed')).toEqual({
      notice: 'Alice was kicked and their messages were purged.',
      unknownStatus: false,
    });
  });

  it('flags an unrecognized status rather than silently returning an empty notice', () => {
    expect(purgeNotice('Alice', 'banned', 'something_new')).toEqual({
      notice: '',
      unknownStatus: true,
    });
  });

  it('never names the rate-limit budget', () => {
    expect(purgeNotice('Alice', 'banned', 'skipped_rate_limited').notice).not.toMatch(/\d/);
  });
});
