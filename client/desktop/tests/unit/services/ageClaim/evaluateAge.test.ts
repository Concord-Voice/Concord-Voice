import { describe, it, expect } from 'vitest';
import { evaluateAge, type AgeSignal } from '@/renderer/services/ageClaim/evaluateAge';

const NOW = new Date(Date.UTC(2026, 5, 19)); // 2026-06-19

function bd(year: number, month: number, day: number): AgeSignal {
  return { kind: 'birthdate', year, month, day };
}

describe('evaluateAge', () => {
  it.each<[AgeSignal, boolean, boolean]>([
    [bd(2011, 6, 19), false, false], // turns 15 today -> <16
    [bd(2010, 6, 19), true, false], // turns 16 today -> 16+, <18
    [bd(2010, 6, 20), false, false], // 16 tomorrow -> still 15 today
    [bd(2008, 6, 19), true, true], // turns 18 today -> 18+
    [bd(2008, 6, 20), true, false], // 18 tomorrow -> 17 today
    [{ kind: 'ageBand', minAge: 18 }, true, true],
    [{ kind: 'ageBand', minAge: 16 }, true, false],
    [{ kind: 'ageBand', minAge: 15 }, false, false],
  ])('signal %o -> validAge %s nsfwAuth %s', (signal, validAge, nsfwAuth) => {
    expect(evaluateAge(signal, NOW)).toEqual({ validAge, nsfwAuth });
  });

  it('leap-year birthday (Feb 29) is not yet a year older on Feb 28', () => {
    const now = new Date(Date.UTC(2026, 1, 28)); // 2026-02-28
    // turns 16 on Mar 1 (non-leap year), so on Feb 28 still 15
    expect(evaluateAge(bd(2010, 2, 29), now)).toEqual({ validAge: false, nsfwAuth: false });
  });

  it('leap-year birthday (Feb 29) counts on Mar 1 of a non-leap year', () => {
    const now = new Date(Date.UTC(2026, 2, 1)); // 2026-03-01
    expect(evaluateAge(bd(2010, 2, 29), now)).toEqual({ validAge: true, nsfwAuth: false });
  });
});
