import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

/**
 * A source-scan guard, not a behavioural test — and deliberately so.
 *
 * The NATS enforcement handlers live in `src/index.ts`, which is the server
 * entrypoint: it is excluded from coverage in `vitest.config.ts` and cannot be
 * imported without standing the server up. There is no vantage from which to
 * assert on these `logger.error` calls at runtime, so the choice is a source
 * scan or nothing.
 *
 * It earns its place because the thing it guards ALREADY REGRESSED SILENTLY.
 * #3136 refactored validation into `handleNatsEnforcementCommand` and the catch
 * blocks lost `channelId` / `userId` / `action` with it — every test stayed
 * green, because nothing anywhere asserted on the shape of a log line. Gitar
 * caught it on PR #3157, one merge later.
 *
 * Known bounds, stated rather than discovered later:
 *  1. It proves the identifiers are PASSED to the log call, never that they hold
 *     the right values — `handleNatsEnforcementCommand` validates before calling
 *     `handle`, and only `handle` throws, so population is structural.
 *  2. Renaming the local `cmd` breaks it even though behaviour is unchanged.
 *  3. Reformatting the log call across lines breaks it; the regex tolerates
 *     internal whitespace but not an argument reordering.
 * All three fail LOUD and are a one-line fix, which is the trade being made.
 */
const source = readFileSync(join(__dirname, '..', 'src', 'index.ts'), 'utf8');

describe('NATS enforcement failure logs carry command context (#3136 regression)', () => {
  it('has exactly three handleNatsEnforcementCommand call sites', () => {
    // Anchors the two counts below. If a fourth handler is added, this reds
    // first and says so, rather than the counts drifting silently apart.
    expect(source.match(/handleNatsEnforcementCommand\(/g)).toHaveLength(3);
  });

  it('each site captures the validated command fields', () => {
    // The capture is what makes the spread below non-empty. Without it the
    // log-shape assertion would pass against `...{}` and prove nothing.
    expect(source.match(/cmd = \{ channelId, userId(?:, action)? \}/g)).toHaveLength(3);
  });

  it('each site logs those fields on failure', () => {
    expect(source.match(/logger\.error\([^;]*\{ error: err, \.\.\.cmd \}\)/g)).toHaveLength(3);
  });

  it('no enforcement-command catch logs the bare error with no context', () => {
    // Scoped to the `${subject}` template form and the force-disconnect
    // literal -- the three handleNatsEnforcementCommand sites.
    //
    // `voice.enforce.permissions` is deliberately NOT covered: it goes through
    // handleEnforcePermissionsMessage, which surfaces no validated fields to
    // its caller, and it logged `{ error: err }` before #3136 as well. It is a
    // pre-existing gap, not this regression, and pretending otherwise here
    // would make the guard assert something it cannot enforce.
    expect(source).not.toMatch(
      /logger\.error\(\s*(?:`Failed to handle \$\{subject\}`|'Failed to handle voice\.enforce\.disconnect'),\s*\{\s*error:\s*err\s*\}\s*\)/
    );
  });
});
