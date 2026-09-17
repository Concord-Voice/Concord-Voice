import { describe, it, expect } from 'vitest';
import { audioToggleHint } from '@/renderer/components/Voice/ScreenSharePicker';
import { screenAudioTitle } from '@/renderer/components/Voice/VoiceControls';
import { screenAudioRefusalMessage } from '@/renderer/services/voice/voiceService';

/**
 * #3198 PR 2 §6 copy rules, pinned against the FUNCTIONS, not the file -- so a wording
 * edit inside any of the three cannot silently drift from the other two.
 *
 * The 'per-process' arms exercised below are WRITTEN, EXHAUSTIVE, and UNREACHABLE in
 * production for this PR: PR 2 is split, and Tasks 10/12/12a (the capture seam) move to
 * PR 3. These tests still pin the strings, because PR 3 needs them correct on day one
 * and a function's own exhaustiveness is real coverage independent of what production
 * currently wires into it.
 */
describe('screen-audio copy (#3198 §6)', () => {
  // THE STATE AXIS (#3198 PR 2 Phase-8 review, M1). The two CAPABLE arms are
  // present-indicative claims about what the share is carrying, so they must vary
  // with the toggle: at `on: false` the copy told a user the share was sending all
  // computer sound at the exact moment they switched it off -- on the one element
  // this PR made persistent and AT-reachable. A verdict-only table is structurally
  // unable to see a state defect, which is why this is a CROSS-PRODUCT and not two
  // more rows. The `none` and no-selection arms are state-independent by
  // construction (a locked control has no on state) and are pinned separately.
  it.each([
    ['system-loopback', true, 'Sharing all computer sound, not just this screen.'],
    ['system-loopback', false, 'Turning this on shares all computer sound, not just this screen.'],
    ['per-process', true, 'Sharing only this app’s sound.'],
    ['per-process', false, 'Turning this on shares only this app’s sound.'],
  ] as const)('audioToggleHint hint for %s (on=%s)', (verdict, on, expected) => {
    expect(audioToggleHint('window:42:0', verdict, 'darwin', on)).toBe(expected);
  });

  // The no-selection arm had no exact-string pin before #3192 -- a gap on the arm a user
  // sees FIRST and most often. It is also the one arm whose verdict argument is ignored,
  // so it is pinned across all three to prove the `selected === null` guard precedes the
  // switch rather than coinciding with one arm's value.
  it.each(['none', 'system-loopback', 'per-process'] as const)(
    'audioToggleHint answers the no-selection arm regardless of verdict (%s)',
    (verdict) => {
      expect(audioToggleHint(null, verdict, 'darwin', false)).toBe(
        'Choose what to share to include sound.'
      );
    }
  );

  it('audioToggleHint collapses the three per-process causes to one string', () => {
    expect(audioToggleHint('window:42:0', 'none', 'darwin', false)).toBe(
      'App sound isn’t available on this computer. Share a whole screen instead.'
    );
  });

  // OQ1 ruling: the Linux arm is UNTOUCHED, not part of the collapse above, and carries
  // its own remedy because "share a whole screen" cannot work on Linux either.
  it('audioToggleHint leaves the Linux arm exactly as it was (OQ1)', () => {
    expect(audioToggleHint('window:42:0', 'none', 'linux', false)).toBe(
      'Computer sound isn’t supported on Linux yet.'
    );
  });

  it('never says "window" in user-facing copy (§6 vocabulary rule)', () => {
    const all = [
      audioToggleHint('window:42:0', 'none', 'darwin', false),
      // BOTH states of each capable arm -- the vocabulary rule has to hold for the
      // off-state strings too, and those did not exist when this list was written.
      audioToggleHint('window:42:0', 'per-process', 'darwin', true),
      audioToggleHint('window:42:0', 'per-process', 'darwin', false),
      audioToggleHint('window:42:0', 'system-loopback', 'darwin', true),
      audioToggleHint('window:42:0', 'system-loopback', 'darwin', false),
      screenAudioTitle('none', false),
      screenAudioTitle('per-process', false),
      screenAudioTitle('per-process', true),
      screenAudioTitle('system-loopback', false),
      screenAudioTitle('system-loopback', true),
      screenAudioRefusalMessage('window:42:0'),
      screenAudioRefusalMessage('window:42:0', true),
    ];
    for (const s of all) expect(s.toLowerCase()).not.toContain('window');
  });

  it('deletes the platform claim from the toolbar tooltip', () => {
    expect(screenAudioTitle('none', false)).not.toContain('Windows');
    expect(screenAudioTitle('none', false)).not.toContain('macOS');
    expect(screenAudioTitle('none', false)).toBe('This share can’t carry sound');
  });

  // The mirror of the Linux + whole-screen fix this docblock already records:
  // Linux has no loopback for ANY target, so a window target must get the same
  // Linux answer as a screen target rather than falling through to "share a
  // whole screen to include sound" -- a remedy that is false on Linux and,
  // followed, costs the user the share they had.
  it('screenAudioRefusalMessage: Linux + a window target gets the Linux message, not the whole-screen remedy', () => {
    expect(screenAudioRefusalMessage('window:1:0', false, 'linux')).toBe(
      'Sharing computer sound is not supported on Linux yet, so your screen is being shared without it.'
    );
  });

  it('screenAudioRefusalMessage: the startRefused arm ignores sourceId shape', () => {
    // A screen: id would normally hit the Linux-only fallthrough arm; startRefused
    // short-circuits ahead of that regardless of the id it is given.
    expect(screenAudioRefusalMessage('screen:0', true)).toBe(
      'Couldn’t start app sound for this share — share a whole screen to include sound.'
    );
  });
});
