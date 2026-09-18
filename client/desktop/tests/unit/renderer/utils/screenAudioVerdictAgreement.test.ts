import { describe, expect, it } from 'vitest';
import {
  canCarryScreenAudio,
  verdictOffersAudio,
  type ScreenAudioVerdict,
} from '@/renderer/utils/policy/screenAudioCapability';
import { AUDIO_PILL_LABEL, audioToggleHint } from '@/renderer/components/Voice/ScreenSharePicker';

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
  // TRUE since #3198 PR 3. `setScreenAudioEnabled`'s `'per-process'` arm no longer refuses
  // with a message — it `break`s into the shared re-capture, which routes through
  // `captureScreenElectron`'s arm and starts a real capture.
  'per-process': true,
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

  // THE COPY LEG. The two cases above pin affordance<->enforcement; nothing
  // pinned copy<->affordance. `AUDIO_PILL_LABEL['per-process']` (imported from
  // ScreenSharePicker.tsx -- NOT mirrored as a local literal, which is what the
  // first version did and which asserts a constant against itself) resolves to
  // 'App', and
  // `audioToggleHint(...,'per-process',...)` resolves to "Shares only this
  // app's sound" -- both answer AFFIRMATIVELY for a verdict `verdictOffersAudio`
  // says is FALSE. The hint is rendered PERSISTENTLY and is not guarded by
  // `audioCapable` at its call site, so it is the one that would actually reach
  // a user if PR 3 flips only enforcement and not copy. If a future change
  // flips any one of the three without the others, this goes red.
  it('copy and affordance move together for per-process', () => {
    expect(AUDIO_PILL_LABEL['per-process']).toBe('App');
    expect(audioToggleHint('window:12:0', 'per-process', 'darwin', true)).toBe(
      'Sharing only this app’s sound.'
    );
    // TRUE since #3198 PR 3. The three legs — pill, hint, affordance — now all answer
    // affirmatively, which is the agreement this case exists to pin. It pinned them
    // agreeing at FALSE before; the property is the agreement, not the value.
    expect(verdictOffersAudio('per-process')).toBe(true);
  });

  // POSITIVE CONTROL. Without it the two cases above pass against a `verdictOffersAudio`
  // that returns false for everything — green, and the control permanently dark.
  it('does offer audio for a real screen target (the negatives are not vacuous)', () => {
    expect(verdictOffersAudio(canCarryScreenAudio('screen:0:0', 'darwin'))).toBe(true);
  });

  // The picker's seam, end to end, INVERTED BY #3198 PR 3. It asserted `false` while the
  // capture wiring was absent; PR 3 landed that wiring, so a window target on a capable
  // machine is now exactly the case the feature exists to serve.
  it('offers audio for a window target on a capable machine', () => {
    expect(verdictOffersAudio(canCarryScreenAudio('window:12:0', 'darwin', true))).toBe(true);
  });

  // THE NEGATIVE THAT MUST SURVIVE THE FLIP. The case above stops discriminating the
  // moment `verdictOffersAudio` returns true for everything, so the machine-capability
  // half needs its own case: the SAME window target on a machine that reported NO
  // per-process backend must still be refused. Without this, PR 3's flip and a blanket
  // `return true` are indistinguishable.
  it('does not offer audio for a window target on an INCAPABLE machine', () => {
    expect(verdictOffersAudio(canCarryScreenAudio('window:12:0', 'darwin', false))).toBe(false);
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
