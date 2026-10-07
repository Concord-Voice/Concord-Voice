import { beforeEach, describe, expect, it } from 'vitest';

import { useTotpAcceptedStore } from '@/renderer/stores/auth/totpAcceptedStore';

const ALICE = '11111111-1111-4111-8111-111111111111';
const BOB = '22222222-2222-4222-8222-222222222222';

const acceptedAt = () => useTotpAcceptedStore.getState().acceptedAt;

beforeEach(() => {
  // Not in resetAllStores(), so reset it here.
  useTotpAcceptedStore.getState().reset();
  localStorage.clear();
});

describe('totpAcceptedStore', () => {
  it('starts empty', () => {
    expect(acceptedAt()).toEqual({});
  });

  it('records the acceptance time per account', () => {
    useTotpAcceptedStore.getState().noteTotpAccepted(ALICE, 1_700_000_000_123);
    useTotpAcceptedStore.getState().noteTotpAccepted(BOB, 1_700_000_005_000);

    expect(acceptedAt()).toEqual({ [ALICE]: 1_700_000_000_123, [BOB]: 1_700_000_005_000 });
  });

  it('replaces an account’s earlier acceptance with the latest', () => {
    useTotpAcceptedStore.getState().noteTotpAccepted(ALICE, 1_000);
    useTotpAcceptedStore.getState().noteTotpAccepted(ALICE, 2_000);

    expect(acceptedAt()).toEqual({ [ALICE]: 2_000 });
  });

  it('does not mutate the previous map, so subscribers see a new reference', () => {
    useTotpAcceptedStore.getState().noteTotpAccepted(ALICE, 1_000);
    const before = acceptedAt();

    useTotpAcceptedStore.getState().noteTotpAccepted(BOB, 2_000);

    expect(before).toEqual({ [ALICE]: 1_000 });
    expect(acceptedAt()).not.toBe(before);
  });

  describe('clearAccount', () => {
    // Mutant: clearAccount empties the whole map.
    it('clears only the named account', () => {
      useTotpAcceptedStore.getState().noteTotpAccepted(ALICE, 1_000);
      useTotpAcceptedStore.getState().noteTotpAccepted(BOB, 2_000);

      useTotpAcceptedStore.getState().clearAccount(ALICE);

      expect(acceptedAt()).toEqual({ [BOB]: 2_000 });
      expect(Object.hasOwn(acceptedAt(), ALICE)).toBe(false);
    });

    it('leaves state untouched for an account with no record', () => {
      useTotpAcceptedStore.getState().noteTotpAccepted(ALICE, 1_000);
      const before = acceptedAt();

      useTotpAcceptedStore.getState().clearAccount(BOB);

      expect(acceptedAt()).toBe(before);
    });

    it('does not treat inherited object properties as records', () => {
      const before = acceptedAt();

      useTotpAcceptedStore.getState().clearAccount('toString');
      useTotpAcceptedStore.getState().clearAccount('__proto__');

      expect(acceptedAt()).toBe(before);
    });
  });

  describe('reset', () => {
    it('clears every account', () => {
      useTotpAcceptedStore.getState().noteTotpAccepted(ALICE, 1_000);
      useTotpAcceptedStore.getState().noteTotpAccepted(BOB, 2_000);

      useTotpAcceptedStore.getState().reset();

      expect(acceptedAt()).toEqual({});
    });
  });

  it('is in memory only: nothing reaches localStorage or sessionStorage', () => {
    useTotpAcceptedStore.getState().noteTotpAccepted(ALICE, 1_700_000_000_123);

    expect(localStorage).toHaveLength(0);
    expect(sessionStorage).toHaveLength(0);
  });
});
