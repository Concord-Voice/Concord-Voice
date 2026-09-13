import { canCarryScreenAudio } from '@/renderer/utils/policy/screenAudioCapability';

// The assertions below pin the EXACT verdict string, never truthiness: every verdict
// but 'none' is a non-empty and therefore truthy string, so a truthiness assertion
// would keep passing for a mechanism this capture path cannot request (#3198).
describe('canCarryScreenAudio', () => {
  it('allows a screen target on a platform with loopback', () => {
    expect(canCarryScreenAudio('screen:0', 'darwin')).toBe('system-loopback');
    expect(canCarryScreenAudio('screen:0', 'win32')).toBe('system-loopback');
  });

  // #2161: Electron's desktop audio capture ignores chromeMediaSourceId, so a window
  // target asking for audio ships every application's sound to the channel.
  // The ids are CANONICAL since #3198 PR 1. This case predates the window rung and used
  // `window:1`, which is not a canonical `window:<handle>:<index>` and so never reaches
  // that rung at all -- it lands on the terminal fail-closed fall-through. Measured: mutate
  // the window rung to return 'system-loopback' unconditionally and all three assertions
  // stayed GREEN. That is `tests.md` § Vacuity's "a new branch can make an OLD test
  // vacuous, so mutate what you did not touch", arrived at from the other direction. The
  // non-canonical id is KEPT as a fourth assertion, because refusing it is also real
  // behaviour -- it just is not what this case's name claims to pin.
  it('refuses every window target regardless of platform', () => {
    expect(canCarryScreenAudio('window:12:0', 'darwin')).toBe('none');
    expect(canCarryScreenAudio('window:12:0', 'win32')).toBe('none');
    expect(canCarryScreenAudio('window:12:0', 'linux')).toBe('none');
    expect(canCarryScreenAudio('window:1', 'darwin')).toBe('none');
  });

  // The spec's capability ladder makes every Linux target audio-incapable: this
  // capture path has no Linux loopback and falls back to silent video, so offering
  // the control there advertises an operation that cannot succeed.
  it('refuses a screen target on Linux', () => {
    expect(canCarryScreenAudio('screen:0', 'linux')).toBe('none');
  });

  it('refuses when nothing is selected', () => {
    expect(canCarryScreenAudio(null, 'darwin')).toBe('none');
  });

  // An unresolved platform is the dev/web path, which reaches getDisplayMedia --
  // OS-mediated consent, not the #2161 whole-desktop loopback. Allowing it keeps the
  // control honest there; the capture path still gates on the prefix.
  it('allows a screen target when the platform is not yet known', () => {
    expect(canCarryScreenAudio('screen:0', null)).toBe('system-loopback');
  });

  it('refuses an id shape it does not recognise, rather than guessing', () => {
    expect(canCarryScreenAudio('tab:7', 'darwin')).toBe('none');
    expect(canCarryScreenAudio('', 'darwin')).toBe('none');
  });
  // ---------------------------------------------------------------------------
  // The third rung (#3198). `machineCapability` is the machine's per-process claim,
  // pushed from main over IPC contract 28.
  // ---------------------------------------------------------------------------

  it.each([
    ['screen:0:0', 'darwin', true, 'system-loopback'],
    ['screen:0:0', 'win32', false, 'system-loopback'],
    ['screen:0:0', 'linux', true, 'none'],
    ['window:12:0', 'darwin', true, 'per-process'],
    ['window:12:0', 'win32', true, 'per-process'],
    ['window:12:0', 'darwin', false, 'none'],
    ['window:12:0', 'darwin', null, 'none'],
    ['window:12:0', 'darwin', undefined, 'none'],
    ['window:12:0', 'linux', true, 'none'],
    ['window:0:0', 'darwin', true, 'none'],
    ['window:007:0', 'darwin', true, 'none'],
    ['nonsense', 'darwin', true, 'none'],
    ['', 'darwin', true, 'none'],
    ['screen:0:0', 'freebsd', true, 'system-loopback'],
    ['window:12:0', 'freebsd', true, 'per-process'],
  ])('canCarryScreenAudio(%s, %s, %s) === %s', (sourceId, platform, machine, expected) => {
    expect(
      canCarryScreenAudio(
        sourceId as string,
        platform as string,
        machine as boolean | null | undefined
      )
    ).toBe(expected);
  });

  it.each([null, undefined])('refuses a %s source id regardless of machine capability', (id) => {
    expect(canCarryScreenAudio(id, 'darwin', true)).toBe('none');
  });

  // TYPE CONFUSION. `.startsWith()` is a method call on a renderer-supplied value and runs
  // before the type-guarded parse, so a duck-typed non-string reached the one verdict that
  // grants the whole-system mix. Unreachable from honest input -- every caller passes a
  // string from main-authoritative IPC -- but the asymmetry (helper guarded, granting
  // function not) is the wrong way round. Found by the pre-PR adversarial pass.
  it.each([
    ['object that duck-types startsWith', { startsWith: () => true }],
    ['object with a screen-ish toString', { toString: () => 'screen:0:0' }],
    ['array', ['screen:0:0']],
    ['number', 12],
    ['boolean true', true],
  ])('refuses a %s rather than calling a method on it', (_label, input) => {
    expect(canCarryScreenAudio(input as unknown as string, 'darwin', true)).toBe('none');
  });

  // MONOTONICITY. Adding the third parameter must not move any EXISTING answer. Only a
  // `window:` id may change, and only to 'per-process'. This is what proves the rung was
  // ADDED rather than the ladder rewritten.
  //
  // TWO CORRECTIONS FROM THE #3198 PHASE-8 REVIEW, both measured rather than argued:
  //
  // 1. This comment used to claim a reordering "that, say, moved the Linux check below the
  //    window rung would pass the table above and fail here." Backwards in both halves --
  //    measured, that mutant reddens 2 rows of the TABLE and leaves this loop entirely
  //    green (it runs its assertions and every one is satisfied). The table is the guard;
  //    this loop is blind to a reorder. A reader trusting the old sentence would delete the
  //    test that actually catches it.
  //
  // 2. The loop had NO FLOOR. `if (withMachine === base) continue;` means a mutant that
  //    ignores `machineCapability` entirely diverges nowhere, executes ZERO assertions, and
  //    passes -- coverage-shaped and pinning nothing. The divergence count below is the
  //    floor: it asserts the parameter is not merely accepted but READ, and pins exactly
  //    which inputs may move (`window:` ids, non-Linux platform, `machine === true`).
  it('changes no verdict except window ids gaining per-process', () => {
    const ids = [
      'screen:0:0',
      'screen:1:2',
      'window:12:0',
      'window:99:3',
      'nonsense',
      '',
      'display:1:0',
    ];
    const platforms = ['darwin', 'win32', 'linux', 'freebsd', null];

    let divergences = 0;
    for (const id of ids) {
      for (const platform of platforms) {
        const base = canCarryScreenAudio(id, platform);
        for (const machine of [undefined, null, false, true] as const) {
          const withMachine = canCarryScreenAudio(id, platform, machine);
          if (withMachine === base) continue;
          divergences += 1;
          expect(id.startsWith('window:')).toBe(true);
          expect(base).toBe('none');
          expect(withMachine).toBe('per-process');
          expect(machine).toBe(true);
        }
      }
    }

    // THE FLOOR. Without it the loop is vacuous against a mutant that ignores the third
    // parameter (0 assertions, green). The expected set is exactly: each `window:` id, on
    // each non-Linux platform, with `machine === true` -- one divergence apiece.
    const windowIds = ids.filter((id) => id.startsWith('window:'));
    const nonLinuxPlatforms = platforms.filter((p) => p !== 'linux');
    expect(divergences).toBe(windowIds.length * nonLinuxPlatforms.length);
    expect(divergences).toBeGreaterThan(0);
  });
});
