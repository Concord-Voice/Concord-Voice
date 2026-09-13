import { canCarryScreenAudio } from '@/renderer/utils/policy/screenAudioCapability';

/**
 * THE D6 REGRESSION TEST (ADR-0043 D6 final row; #2161's invariant restated).
 *
 * WHY IT IS RENDERER-SIDE, AND WHY THAT MATTERS MORE THAN IT LOOKS. The system-mix
 * track comes from a renderer-local
 * `getUserMedia({ audio: { mandatory: { chromeMediaSource: 'desktop' } } })` that NEVER
 * TRANSITS MAIN. So for the exact path that produced #2161, the verdict function and the
 * exhaustive switches over it are not one layer of four — they are the WHOLE enforcement.
 * Naming four layers elsewhere in the design would imply a fence that does not exist
 * where the risk actually is.
 *
 * HOSTILE RENDERER, not a well-behaved one. These cases model a caller that ASKS for a
 * system mix on a window target, which is the only way the invariant can be violated. A
 * test that exercises only the honest path is vacuous here.
 */
describe('D6: a window target cannot obtain a system mix', () => {
  it.each([
    ['capable machine', true],
    ['incapable machine', false],
    ['unknown machine', null],
    ['shell below contract 28', undefined],
  ])('never returns system-loopback for a window id — %s', (_label, machine) => {
    for (const platform of ['darwin', 'win32', 'linux', 'freebsd', null]) {
      expect(
        canCarryScreenAudio('window:12:0', platform, machine as boolean | null | undefined)
      ).not.toBe('system-loopback');
    }
  });

  it.each([
    'window:12:0',
    'window:4294967295:9',
    'window:0:0',
    'window:007:0',
    'window:-1:0',
    'window:99999999999999999999:0',
    'window:12:0\n',
    'WINDOW:12:0',
    'display:12:0',
    'nonsense',
    '',
  ])('never returns system-loopback for hostile id %j', (sourceId) => {
    expect(canCarryScreenAudio(sourceId, 'darwin', true)).not.toBe('system-loopback');
  });

  // A screen id is the ONLY thing that may reach the loopback. Stated as its own
  // assertion so the negatives above cannot pass by the function returning 'none' for
  // everything — which would be green and useless.
  it('still returns system-loopback for a screen id (the negatives above are not vacuous)', () => {
    expect(canCarryScreenAudio('screen:0:0', 'darwin', true)).toBe('system-loopback');
    expect(canCarryScreenAudio('screen:0:0', 'darwin', null)).toBe('system-loopback');
  });
});

/**
 * I-D6's mechanical guard.
 *
 * The invariant is that no PRODUCTION code outside the declaring header ever ASSIGNS
 * `CaptureScope::kSystemMix` — so `rt::buildTarget` stays the single admission rule, the
 * monitor / full-screen row stays on the Electron loopback, and a system mix is never
 * reachable from native code.
 *
 * It is deliberately NOT "the identifier appears nowhere else". That stronger form was
 * tried first and is wrong in a way worth recording: the native suites
 * (`test/macos_tap_test.cc`, `test/rt_contract_test.cc`) hand-assign `kSystemMix`
 * precisely to prove that `buildTarget` never emits it and that the macOS backend refuses
 * it, and two production hits are comments explaining the refusal. Banning the NAME would
 * delete the tests that prove the invariant and forbid documenting it.
 */
describe('kSystemMix containment', () => {
  // `(^|[^=!<>])=` -- an ASSIGNMENT, not a comparison. The pattern was a bare `=` until the
  // #3198 Phase-8 review measured what that actually matched: the second `=` of `==`. Two of
  // the six hits it found were pure comparisons (`rt_contract_test.cc:1523`, `:1525`), and
  // the mutant is ugly -- change the pattern to `'=='` and the main assertion returns empty
  // (green) while the positive control still matches those two comparison lines (green). The
  // guard detects nothing it claims to detect and the suite says nothing. A control that
  // proves the pattern matches SOMETHING is not a control that proves it matches an
  // ASSIGNMENT, which is why the control below now names a specific assignment line and a
  // third case asserts the comparisons are excluded.
  const ASSIGNMENT = '(^|[^=!<>])=[[:space:]]*(rt::)?CaptureScope::kSystemMix';
  const COMPARISON = '==[[:space:]]*(rt::)?CaptureScope::kSystemMix';

  async function gitGrep(args: string[]): Promise<string> {
    const { execFileSync } = await import('node:child_process');
    try {
      return execFileSync('git', ['grep', '-nE', ...args], { encoding: 'utf8' });
    } catch (err) {
      // `git grep` exits 1 with no output when there are no matches — that is a valid
      // empty result. Any other status is a real failure and must not be swallowed.
      if ((err as { status?: number }).status !== 1) throw err;
      return '';
    }
    // NOTE: `git grep` searches TRACKED files only, so a new, unstaged `.cc` is invisible
    // to this guard locally. Harmless in CI (the checkout is fully tracked) but do not read
    // a local green as covering work in progress.
  }

  it('is never assigned by production code outside rt/capture_backend.h', async () => {
    const hits = await gitGrep([
      ASSIGNMENT,
      '--',
      'native',
      ':!*capture_backend.h',
      ':!native/concord-audiocap/test/*',
    ]);
    expect(hits.trim()).toBe('');
  });

  // POSITIVE CONTROL, NAMING A SPECIFIC ASSIGNMENT. Without a control the main assertion
  // would pass against a typo, a renamed enum, or a moved directory — green because it
  // matched nothing anywhere. Without naming an actual assignment LINE, it would also pass
  // against a pattern that had stopped recognising assignments altogether, because the
  // comparison lines in the same files keep it fed.
  it('the same pattern DOES match a known hand-assignment in the native tests', async () => {
    const hits = await gitGrep([ASSIGNMENT, '--', 'native/concord-audiocap/test']);
    expect(hits).toContain('mix.scope = rt::CaptureScope::kSystemMix');
    // Every hit is an assignment; none is a comparison.
    expect(hits).not.toContain('==');
  });

  // NEGATIVE CONTROL. The comparisons exist and the pattern must NOT claim them — this is
  // what makes the `[^=!<>]` prefix load-bearing rather than decorative.
  it('excludes comparisons, which do exist and must not count as assignments', async () => {
    const comparisons = await gitGrep([COMPARISON, '--', 'native/concord-audiocap/test']);
    expect(comparisons).toContain('kSystemMix');

    const assignments = await gitGrep([ASSIGNMENT, '--', 'native/concord-audiocap/test']);
    for (const line of comparisons.trim().split('\n')) {
      expect(assignments).not.toContain(line);
    }
  });
});
