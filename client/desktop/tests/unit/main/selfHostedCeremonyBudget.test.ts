// @vitest-environment node
import { describe, it, expect, beforeEach } from 'vitest';
import {
  CEREMONY_BUDGET,
  CEREMONY_WINDOW_MS,
  consumeCeremonyToken,
  _resetCeremonyBudgetForTesting,
} from '../../../src/main/selfHostedCeremonyBudget';

describe('self-hosted approval ceremony budget (#2354)', () => {
  beforeEach(() => {
    _resetCeremonyBudgetForTesting();
  });

  it('admits exactly the budget within one window, then refuses', () => {
    const t0 = 1_000_000;
    for (let i = 0; i < CEREMONY_BUDGET; i++) {
      expect(consumeCeremonyToken(t0 + i)).toBe(true);
    }
    expect(consumeCeremonyToken(t0 + CEREMONY_BUDGET)).toBe(false);
  });

  it('does not consume a token when it refuses', () => {
    const t0 = 1_000_000;
    for (let i = 0; i < CEREMONY_BUDGET; i++) consumeCeremonyToken(t0);
    // A refused call must not extend the window by stamping itself; the first
    // stamp still ages out on schedule.
    expect(consumeCeremonyToken(t0 + CEREMONY_WINDOW_MS - 1)).toBe(false);
    expect(consumeCeremonyToken(t0 + CEREMONY_WINDOW_MS)).toBe(true);
  });

  it('rolls: a stamp older than the window frees exactly one slot', () => {
    const t0 = 1_000_000;
    expect(consumeCeremonyToken(t0)).toBe(true);
    for (let i = 1; i < CEREMONY_BUDGET; i++) {
      expect(consumeCeremonyToken(t0 + 1000 + i)).toBe(true);
    }
    expect(consumeCeremonyToken(t0 + 2000)).toBe(false);

    // t0 has now aged out, but the later stamps have not.
    expect(consumeCeremonyToken(t0 + CEREMONY_WINDOW_MS)).toBe(true);
    expect(consumeCeremonyToken(t0 + CEREMONY_WINDOW_MS)).toBe(false);
  });

  it('is in-memory only — a fresh module state starts with a full budget', () => {
    const t0 = 1_000_000;
    for (let i = 0; i < CEREMONY_BUDGET; i++) consumeCeremonyToken(t0);
    expect(consumeCeremonyToken(t0)).toBe(false);
    _resetCeremonyBudgetForTesting();
    expect(consumeCeremonyToken(t0)).toBe(true);
  });

  it('defaults to the wall clock when no timestamp is supplied', () => {
    expect(consumeCeremonyToken()).toBe(true);
  });
});
