/**
 * Regression (#2199 / CWE-212): fresh-login init vs. teardown race.
 *
 * The fresh-login e2eeService.initialize() callers (Login / Register /
 * SSOEagerUnlock / SSOPassphraseSetup) pass NO E2EEInitializationGuard, so the
 * only thing that can abort an in-flight commit is the internal
 * keyClearGeneration fence that clearKeys() raises (destruction-only —
 * keySessionGeneration deliberately does NOT abort commits: recoveryReset()
 * bumps it while preserving the session; see the third test case).
 *
 * A 401 -> nuclearReset() landing in the ~hundreds-of-ms Argon2id derivation
 * window must NOT let finalizeKeys() commit the keyset + setReady(true) AFTER
 * the session was torn down — that resurrects the (now orphaned) account's key
 * material and readiness (CWE-212). nuclearReset() reaches clearKeys() via
 * gracefulReset() (#2327); this test locks that the e2eeService commit path
 * actually honors that teardown. See [internal]rules/frontend.md
 * § "Recovery-A teardown is not logout teardown".
 */
import { vi, describe, it, expect, beforeEach } from 'vitest';

// e2eeService imports apiFetch/safeJson; resetService imports stopProactiveRefresh.
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
  safeJson: vi.fn(),
  API_BASE: 'http://localhost:8080',
  stopProactiveRefresh: vi.fn(),
  refreshAccessToken: vi.fn(),
}));

// gracefulReset() stops these account-bound watchers — stub them so the reset
// is a pure in-memory state operation with no timers/network in the test.
vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: { stopWatching: vi.fn(), pushPreferences: vi.fn() },
}));
vi.mock('@/renderer/services/savedGifsSync', () => ({
  savedGifsSyncService: { stopWatching: vi.fn() },
}));
vi.mock('@/renderer/services/friendOrgSync', () => ({
  friendOrgSyncService: { stopWatching: vi.fn() },
}));
vi.mock('@/renderer/services/presenceOverrideSync', () => ({
  presenceOverrideSyncService: { reset: vi.fn() },
}));
vi.mock('@/renderer/services/notificationPrefsService', () => ({
  stopExpirySweep: vi.fn(),
}));

// Real e2eeService, real resetService, real crypto — the race is genuine.
import { e2eeService } from '@/renderer/services/e2eeService';
import { E2EEInitTeardownError } from '@/renderer/services/e2eeErrors';
import { nuclearReset, recoveryReset } from '@/renderer/services/resetService';
import { useE2EEStore } from '@/renderer/stores/e2eeStore';
import { generateRegistrationKeys } from '@/renderer/utils/crypto';
import { resetAllStores } from '../../helpers/store-helpers';

describe('e2eeService fresh-login init/teardown race (#2199 / CWE-212)', () => {
  const password = 'TestPassword123!'; // pragma: allowlist secret

  beforeEach(() => {
    // resetAllStores() per [internal]rules/tests.md — nuclearReset()/recoveryReset()
    // in these cases mutate many stores beyond e2ee. clearKeys() additionally
    // resets the e2eeService singleton (not a store).
    resetAllStores();
    e2eeService.clearKeys();
    useE2EEStore.getState().reset();
  });

  // Assert EVERY sensitive publication surface stayed cleared — isInitialized
  // covers only wrappingKey; a partial resurrection (e.g. sessionKeys or
  // wrappedPrivateKey re-published without wrappingKey) must also fail here
  // (CodeRabbit, PR #2337).
  async function expectFullyCleared(): Promise<void> {
    expect(e2eeService.isInitialized).toBe(false);
    expect(useE2EEStore.getState().ready).toBe(false);
    expect(e2eeService.getSessionKeys()).toBeNull();
    expect(e2eeService.getWrappedPrivateKey()).toBe('');
    await expect(e2eeService.encryptPreferences({ probe: true })).rejects.toThrow();
  }

  it('nuclearReset() during a guardless initialize() rejects and does not resurrect keys or ready', async () => {
    const { wrappedPrivateKey, keyDerivationSalt } = await generateRegistrationKeys(password);

    // Guardless fresh-login init — kick it off but do NOT await. It suspends at
    // the first Argon2id derivation; finalizeKeys() always awaits exportKey()
    // before its synchronous commit, so the synchronous nuclearReset() below is
    // guaranteed to run before any key/ready commit.
    const initPromise = e2eeService.initialize(password, wrappedPrivateKey, keyDerivationSalt);

    // The 401 recovery path tears the session down mid-derivation.
    nuclearReset();

    // The fenced commit must REJECT with the typed teardown error — a silent
    // void-success would let Login continue past the teardown (store the
    // refresh token, restore the access token, call onSuccess) with E2EE dead
    // (Codex P1-A, PR #2337).
    await expect(initPromise).rejects.toBeInstanceOf(E2EEInitTeardownError);

    // And no resurrected key material or readiness, on ANY publication surface.
    await expectFullyCleared();
  });

  it('teardown BEFORE initialize() is caught by a caller-captured epoch (Codex P1-B, PR #2337)', async () => {
    const { wrappedPrivateKey, keyDerivationSalt } = await generateRegistrationKeys(password);

    // Login captures the epoch BEFORE publishing the access token / starting
    // key work. If a teardown lands in that window (e.g. during
    // unwrapLoginKeys), initialize()'s own entry snapshot would postdate the
    // clearKeys() bump and every gate would pass — so the caller-captured
    // epoch is what closes this window.
    const teardownEpoch = e2eeService.captureTeardownEpoch();

    nuclearReset();

    await expect(
      e2eeService.initialize(
        password,
        wrappedPrivateKey,
        keyDerivationSalt,
        'argon2id',
        undefined,
        teardownEpoch
      )
    ).rejects.toBeInstanceOf(E2EEInitTeardownError);

    await expectFullyCleared();
  });

  it('nuclearReset() during initializeFromStoredKeys() does not resurrect keys or ready', async () => {
    // Produce a valid stored-keys blob from a real initialize(), then reset to a
    // clean pre-restore state (as at cold launch before session-restore runs).
    const { wrappedPrivateKey, keyDerivationSalt } = await generateRegistrationKeys(password);
    await e2eeService.initialize(password, wrappedPrivateKey, keyDerivationSalt);
    const stored = e2eeService.getSessionKeys();
    if (!stored) throw new Error('expected session keys after initialize()');
    e2eeService.clearKeys();
    useE2EEStore.getState().reset();

    // Kick off restore but do NOT await — it suspends at the first importKey().
    // The synchronous nuclearReset() below runs before either importKey resolves,
    // so without the fence the interleaved commits leave a ready=true /
    // wrappingKey=null split-brain plus resurrected sessionKeys (CWE-212).
    const restorePromise = e2eeService.initializeFromStoredKeys(stored);

    nuclearReset();

    await expect(restorePromise).rejects.toBeInstanceOf(E2EEInitTeardownError);

    await expectFullyCleared();
  });

  it('a SUPERSEDED initialize() cannot commit over a newer attempt (Codex P1, PR #2337)', async () => {
    // A token-only session invalidation (handleRefreshFailure's phase !==
    // 'stable' path) clears the access token WITHOUT clearKeys(), so
    // keyClearGeneration never advances. If the user rapidly signs in again,
    // the stale first attempt's Argon2id can resolve AFTER the successor's
    // commit — last-writer-wins would overwrite the successor's keyset with
    // the stale one (cross-session key confusion). The attempt fence admits
    // only the newest initializer; the superseded one aborts with the typed
    // teardown error (same abort contract as destruction).
    const stalePassword = 'StalePassword123!'; // pragma: allowlist secret
    const staleKeys = await generateRegistrationKeys(stalePassword);
    const successorKeys = await generateRegistrationKeys(password);

    // Stale attempt starts first and suspends in Argon2id.
    const stalePromise = e2eeService.initialize(
      stalePassword,
      staleKeys.wrappedPrivateKey,
      staleKeys.keyDerivationSalt
    );

    // Successor attempt starts (bumping the attempt sequence synchronously at
    // entry) and runs to completion while the stale one is still pending.
    await e2eeService.initialize(
      password,
      successorKeys.wrappedPrivateKey,
      successorKeys.keyDerivationSalt
    );
    const successorSession = e2eeService.getSessionKeys();
    if (!successorSession) throw new Error('expected successor session keys');

    // The stale attempt rejects and must NOT have overwritten the successor.
    await expect(stalePromise).rejects.toBeInstanceOf(E2EEInitTeardownError);
    expect(e2eeService.getSessionKeys()).toEqual(successorSession);
    expect(e2eeService.isInitialized).toBe(true);
    expect(useE2EEStore.getState().ready).toBe(true);
  });

  it('a stale continuation must NOT cancel a valid newer sign-in (Codex P1, PR #2337)', async () => {
    // The inverse of the supersession test: a torn-down login's continuation
    // (carrying an ALREADY-STALE caller epoch) must abort WITHOUT claiming the
    // newest-attempt slot. If it bumped initAttemptSequence before validating
    // its stale epoch, it would invalidate a valid successor that started first
    // and already captured an earlier attempt number — cancelling a good
    // sign-in (and its Login abort path would then revoke the successor's
    // freshly-issued session).
    const successorKeys = await generateRegistrationKeys(password);
    const stalePassword = 'StalePassword123!'; // pragma: allowlist secret
    const staleKeys = await generateRegistrationKeys(stalePassword);

    // Make `staleEpoch` genuinely stale: capture it, then bump keyClearGeneration.
    const staleEpoch = e2eeService.captureTeardownEpoch();
    e2eeService.clearKeys();
    useE2EEStore.getState().reset();

    // Successor starts FIRST with a fresh epoch and suspends in Argon2id,
    // capturing the current attempt number.
    const successorPromise = e2eeService.initialize(
      password,
      successorKeys.wrappedPrivateKey,
      successorKeys.keyDerivationSalt
    );

    // The stale continuation resumes with its now-stale epoch. It must reject
    // at the entry gate WITHOUT advancing the attempt sequence.
    const stalePromise = e2eeService.initialize(
      stalePassword,
      staleKeys.wrappedPrivateKey,
      staleKeys.keyDerivationSalt,
      'argon2id',
      undefined,
      staleEpoch
    );

    await expect(stalePromise).rejects.toBeInstanceOf(E2EEInitTeardownError);

    // The valid successor must still complete — the stale continuation did not
    // cancel it.
    await expect(successorPromise).resolves.toBeUndefined();
    expect(e2eeService.isInitialized).toBe(true);
    expect(useE2EEStore.getState().ready).toBe(true);
  });

  it('recoveryReset() during initialize() does NOT abort the commit (Codex P1, PR #2337)', async () => {
    // Recovery-A (recoveryReset) is a SAME-SESSION fence: it rejects stale
    // decrypt continuations but deliberately preserves key custody. It must NOT
    // fence an in-flight init commit — the password-change re-init
    // (userStore changePassword) runs mid-session where a transport blip can
    // land recoveryReset during Argon2id. If the commit were aborted, the OLD
    // keyset stays resident and pushPreferences uploads old-key ciphertext
    // against the server's new password-derived keys — undecryptable after
    // relogin (data loss). The commit gate keys on keyClearGeneration
    // (destruction-only), not keySessionGeneration (continuation fence).
    const { wrappedPrivateKey, keyDerivationSalt } = await generateRegistrationKeys(password);

    const initPromise = e2eeService.initialize(password, wrappedPrivateKey, keyDerivationSalt);

    recoveryReset();

    await initPromise;

    expect(e2eeService.isInitialized).toBe(true);
    expect(useE2EEStore.getState().ready).toBe(true);
  });
});
