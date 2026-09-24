import { describe, it, expect } from 'vitest';
import { SCREEN_AUDIO_DEGRADE_REASONS } from '@/main/audiocapHost';
import { screenAudioDegradeMessage } from '@/renderer/utils/policy/screenAudioDegradeCopy';

const REMEDY = ' Share a whole screen to include sound.';

/**
 * THE MAPPING, PINNED BY EXACT STRING (#3198 PR 2 review — vacuity fix).
 *
 * The previous version of this suite asserted only shape (`toBeTruthy`,
 * `length > 12`, `not.toContain(...)`) — a map returning ONE sentence for all
 * ten reasons passed every one of those assertions. Proven by executing that
 * suite against a collapsed mutant (`DEGRADE_COPY` reduced to a single string
 * repeated for all ten keys): every case stayed green. `it.each` with `toBe`
 * against the exact current strings is what actually pins the mapping, so a
 * swap between two reasons' sentences goes red.
 */
const EXPECTED: Record<string, string> = {
  'no-backend': `App sound isn’t available on this computer.${REMEDY}`,
  'unsupported-os': `App sound isn’t available on this computer.${REMEDY}`,
  'handshake-timeout': `App sound didn’t start in time.${REMEDY}`,
  'load-fault': `App sound couldn’t start on this computer.${REMEDY}`,
  'capability-fault': `App sound couldn’t start on this computer.${REMEDY}`,
  'child-crash': `App sound stopped unexpectedly.${REMEDY}`,
  'protocol-fault': `App sound stopped unexpectedly.${REMEDY}`,
  'produce-rejected':
    'This call can’t carry another audio track, so your screen is being shared without sound.',
  'target-unresolved': `We couldn’t capture that app’s sound.${REMEDY}`,
};

describe('screenAudioDegradeMessage (#3198 Task 13b)', () => {
  // The exhaustiveness anchor stays on the runtime set, so a union member added
  // without an EXPECTED entry fails here with a clear "no expected string"
  // message rather than a silent undefined-vs-string mismatch.
  //
  // #3394 PR 2: `capture-starved` leaves `ScreenAudioDegradeReason` (a `'run'`
  // fault is now reported through `AudiocapInterrupted`, not a start-time
  // degrade reason), so EXPECTED above is one entry short of the CURRENT
  // (pre-implementation) runtime set on purpose — this assertion is RED until
  // `SCREEN_AUDIO_DEGRADE_REASONS` also drops the key (audiocapHost.ts).
  it('EXPECTED covers exactly the reasons the runtime set declares', () => {
    expect(Object.keys(EXPECTED).sort()).toEqual(Object.keys(SCREEN_AUDIO_DEGRADE_REASONS).sort());
  });

  it.each(Object.keys(SCREEN_AUDIO_DEGRADE_REASONS))('copy for %s matches exactly', (reason) => {
    expect(screenAudioDegradeMessage(reason as never)).toBe(EXPECTED[reason]);
  });

  it('never says "window" (§6 vocabulary rule)', () => {
    for (const reason of Object.keys(SCREEN_AUDIO_DEGRADE_REASONS)) {
      expect(screenAudioDegradeMessage(reason as never).toLowerCase()).not.toContain('window');
    }
  });

  it('does not leak which cause produced target-unresolved', () => {
    // The four collapsed causes are below-floor, snapshot-not-arrived,
    // unresolvable-PID and a closed window. None may be nameable from the string.
    const msg = screenAudioDegradeMessage('target-unresolved').toLowerCase();
    for (const leak of ['closed', 'version', 'snapshot', 'macos', 'windows', 'pid', 'process id']) {
      expect(msg, `target-unresolved copy names "${leak}"`).not.toContain(leak);
    }
  });

  it('carries the whole-screen remedy in every arm except produce-rejected (OQ1)', () => {
    for (const reason of Object.keys(SCREEN_AUDIO_DEGRADE_REASONS)) {
      const msg = screenAudioDegradeMessage(reason as never);
      if (reason === 'produce-rejected') {
        // The one arm whose reader is already sharing a whole screen -- the
        // remedy would tell them to do the thing they are already doing.
        expect(msg, `${reason} unexpectedly carries the remedy`).not.toContain(
          'Share a whole screen to include sound.'
        );
      } else {
        expect(msg, `${reason} is missing the remedy`).toContain(
          'Share a whole screen to include sound.'
        );
      }
    }
  });
});
