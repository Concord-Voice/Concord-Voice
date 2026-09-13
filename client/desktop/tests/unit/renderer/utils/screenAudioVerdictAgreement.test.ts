import { describe, expect, it } from 'vitest';
import {
  canCarryScreenAudio,
  verdictOffersAudio,
  type ScreenAudioVerdict,
} from '@/renderer/utils/policy/screenAudioCapability';

/**
 * THE FOUR CONSUMERS MUST AGREE (#3198 Phase-8 review).
 *
 * `ScreenAudioVerdict` had four readers and they gave four different answers for
 * `'per-process'`: `canShareScreenAudio` returned true (toolbar ENABLED),
 * `setScreenAudioEnabled` refused with a `videoSlotError` (click ERRORS),
 * `captureScreenElectron` fell back to `videoOnly()` (silent share), and
 * `ScreenSharePicker` tested `=== 'system-loopback'` (control DISABLED). Nothing asserted
 * any of it, because the value was proven RETURNED and never proven OBEYED —
 * `tests.md` § "Test the consumer, not the handshake", whose founding incident is
 * `ScreenShareOptions.streamAudio` on this same surface.
 *
 * The trap the file locks is narrow and specific: the PR's own description argued that
 * making `'per-process'` reachable "would enable the toolbar control on a capable machine
 * and then produce silence" — and `canShareScreenAudio`'s arm ALREADY ENCODED exactly that,
 * in the one arm the toolbar reads. So if PR 2 threads `machineCapability` into the
 * affordance seam before it threads the capture wiring, the described worst case ships and
 * the suite stays green.
 *
 * These assert the RELATIONSHIP rather than each answer in isolation, so PR 2 may flip the
 * verdict freely as long as it flips both seams together.
 */

const ALL_VERDICTS: ScreenAudioVerdict[] = ['none', 'system-loopback', 'per-process'];

/**
 * Mirrors `voiceService.setScreenAudioEnabled`'s verdict switch: which verdicts does the
 * ENFORCEMENT seam accept? Kept as a table rather than importing the service, because the
 * service pulls the whole mediasoup/WebRTC surface in and the property under test is the
 * agreement between two policies, not the service's plumbing.
 *
 * If `setScreenAudioEnabled` gains or loses an accepted verdict, update this table in the
 * same change — the test below then proves the affordance seam moved with it.
 */
const ENFORCEMENT_ACCEPTS: Record<ScreenAudioVerdict, boolean> = {
  none: false,
  'system-loopback': true,
  'per-process': false,
};

describe('screen-audio verdict: affordance and enforcement agree', () => {
  it.each(ALL_VERDICTS)('never OFFERS %s while the enforcement seam would refuse it', (verdict) => {
    // The one-directional invariant that matters: offering implies accepting. The reverse
    // is allowed — a verdict may be accepted without being advertised.
    if (verdictOffersAudio(verdict)) {
      expect(ENFORCEMENT_ACCEPTS[verdict]).toBe(true);
    }
  });

  it('offers audio for exactly the verdicts enforcement accepts', () => {
    const offered = ALL_VERDICTS.filter(verdictOffersAudio);
    const accepted = ALL_VERDICTS.filter((v) => ENFORCEMENT_ACCEPTS[v]);
    expect(offered).toEqual(accepted);
  });

  // POSITIVE CONTROL. Without it the two cases above pass against a `verdictOffersAudio`
  // that returns false for everything — green, and the control permanently dark.
  it('does offer audio for a real screen target (the negatives are not vacuous)', () => {
    expect(verdictOffersAudio(canCarryScreenAudio('screen:0:0', 'darwin'))).toBe(true);
  });

  // The picker's seam, end to end: a window target on a capable machine must not light the
  // control up while PR 2's capture wiring is absent.
  it('does not offer audio for a window target even on a capable machine', () => {
    expect(verdictOffersAudio(canCarryScreenAudio('window:12:0', 'darwin', true))).toBe(false);
  });

  // EXHAUSTIVENESS. `verdictOffersAudio` must answer every member of the union; a fourth
  // rung added without a decision here is a compile error at its `never`, and this asserts
  // the table above stayed in step with the type.
  it('answers every verdict in the union', () => {
    for (const verdict of ALL_VERDICTS) {
      expect(typeof verdictOffersAudio(verdict)).toBe('boolean');
      expect(ENFORCEMENT_ACCEPTS).toHaveProperty(verdict);
    }
  });

  // THE `never` ARM IS REACHABLE FROM UNTYPED CALLERS, so it is worth proving rather than
  // waving at. `tsc` guards the four call sites in this repo; it guards nothing about a
  // value arriving from JS, a cast, or a future deserialiser. The arm must FAIL CLOSED -
  // return false, never a truthy default - because the mistake it exists to absorb is
  // precisely a verdict nobody taught this function about.
  it('fails closed on a verdict outside the union', () => {
    expect(verdictOffersAudio('per-window-magic' as unknown as ScreenAudioVerdict)).toBe(false);
    expect(verdictOffersAudio(undefined as unknown as ScreenAudioVerdict)).toBe(false);
  });
});

describe('canCarryScreenAudio: the platform parameter is type-guarded', () => {
  // The #3198 Phase-8 review added this guard for symmetry with `sourceId`'s, and a guard
  // with no test is the defect class that review was about. A boxed or duck-typed platform
  // slips past `platform === 'linux'` (a strict primitive compare), so without the guard a
  // Linux SCREEN share would attempt a loopback that does not exist.
  it.each([
    ['a boxed String', new String('linux')],
    ['an object with a linux toString', { toString: () => 'linux' }],
    ['an array', ['linux']],
    ['a number', 0],
  ])('refuses a screen target when the platform is %s', (_label, platform) => {
    expect(canCarryScreenAudio('screen:0:0', platform as unknown as string)).toBe('none');
  });

  // NULL AND UNDEFINED STAY PERMISSIVE, and this is the case that makes the guard correct
  // rather than merely strict. They mean "not resolved yet" - the dev/web path and the
  // picker's pre-probe render both pass null - so folding them into the type guard would
  // turn an unresolved platform into a silent audio refusal on every non-Electron share.
  it.each([null, undefined])('still grants a screen target when the platform is %s', (p) => {
    expect(canCarryScreenAudio('screen:0:0', p)).toBe('system-loopback');
  });
});
