// Pure local age evaluation for #1624 age claims. No DOM, no store, no I/O, no `any`.
// `now` is injected for deterministic tests. UTC math; leap-year correct.
export type AgeSignal =
  | { kind: 'birthdate'; year: number; month: number; day: number } // month 1..12, day 1..31
  | { kind: 'ageBand'; minAge: number }; // provider asserts "at least minAge"

const AGE_THRESHOLD = 16;
const NSFW_THRESHOLD = 18;

function computeAge(year: number, month: number, day: number, now: Date): number {
  let age = now.getUTCFullYear() - year;
  const beforeBirthday =
    now.getUTCMonth() + 1 < month || (now.getUTCMonth() + 1 === month && now.getUTCDate() < day);
  if (beforeBirthday) age -= 1;
  return age;
}

export function evaluateAge(
  signal: AgeSignal,
  now: Date
): { validAge: boolean; nsfwAuth: boolean } {
  const age =
    signal.kind === 'birthdate'
      ? computeAge(signal.year, signal.month, signal.day, now)
      : signal.minAge;
  return { validAge: age >= AGE_THRESHOLD, nsfwAuth: age >= NSFW_THRESHOLD };
}
