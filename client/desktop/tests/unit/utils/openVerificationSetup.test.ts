import { beforeEach, describe, expect, it, vi } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';
import { deferred } from '../../helpers/deferred';
import {
  openVerificationSetup,
  returnFromVerificationSetup,
  verificationReturnLabel,
} from '@/renderer/utils/ui/openVerificationSetup';
import { gracefulReset } from '@/renderer/services/system/resetService';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useSettingsNavStore } from '@/renderer/stores/ui/settingsNavStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import {
  hasPendingDrafts,
  useDraftSettingsStore,
  type DraftOverlays,
} from '@/renderer/stores/ui/draftSettingsStore';

// The "Set up verification" route (#3456 §3.6a) and its "Back to ..." return.
// The stores are real; only the permission refetch is replaced, since it is the
// network edge and the order against `openSettings` is what D-6 pins.
//
// "Mutant:" comments name the production change each case exists to turn red.

const overlay = () => useSettingsOverlayStore.getState();
const nav = () => useSettingsNavStore.getState();

/** What a signed-in session looks like to `captureApiRequestContext`. */
function switchAccount(): void {
  useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 });
}

function stubPermissionFetch(impl: (serverId: string) => Promise<void> = async () => undefined) {
  const fetchServerPermissions = vi.fn(impl);
  usePermissionStore.setState({ fetchServerPermissions });
  return fetchServerPermissions;
}

beforeEach(() => {
  resetAllStores();
});

const NO_DRAFTS: DraftOverlays = { appearance: {}, audio: {}, video: {}, tts: {} };

/** A change made in App Settings and not yet applied. */
function stageDraft(drafts: Partial<DraftOverlays> = { appearance: { uiScale: 1.25 } }): void {
  useDraftSettingsStore.setState({ drafts: { ...NO_DRAFTS, ...drafts } });
}

describe('openVerificationSetup', () => {
  // Mutant: closeHost called before the guard, or the guard's verdict not read (`!(await ...)` dropped).
  it('a declined discard returns false and leaves the dialog and Settings untouched (D-4)', async () => {
    const closeHost = vi.fn();
    const confirmDiscard = vi.fn(async () => false);

    const opened = await openVerificationSetup({
      returnTo: { kind: 'serverSettings', serverId: 's1', section: 'roles' },
      confirmDiscard,
      closeHost,
    });

    expect(opened).toBe(false);
    expect(confirmDiscard).toHaveBeenCalledTimes(1);
    expect(closeHost).not.toHaveBeenCalled();
    expect(overlay().open).toBeNull();
    expect(overlay().verificationReturn).toBeNull();
    expect(nav().focusRequest).toBeNull();
  });

  // Mutant: the guard awaited after closeHost, or closeHost run unconditionally first.
  it('asks the guard while the dialog is still open, and closes it only afterwards', async () => {
    const gate = deferred<boolean>();
    const closeHost = vi.fn();
    const pending = openVerificationSetup({
      returnTo: { kind: 'chat' },
      confirmDiscard: () => gate.promise,
      closeHost,
    });

    // The guard has not answered: the dialog is as the user left it.
    await Promise.resolve();
    expect(closeHost).not.toHaveBeenCalled();
    expect(overlay().open).toBeNull();

    gate.resolve(true);
    await expect(pending).resolves.toBe(true);
    expect(closeHost).toHaveBeenCalledTimes(1);
  });

  // Mutant: closeHost called after openSettings, or openSettings('app') changed to 'server',
  // or the focus request pointed at another section or control.
  it('an accepted discard closes the host, then opens App Settings at the MFA section', async () => {
    const order: string[] = [];
    const closeHost = vi.fn(() => {
      order.push(`close:${overlay().open}`);
    });

    const opened = await openVerificationSetup({
      returnTo: { kind: 'serverSettings', serverId: 's1', section: 'members' },
      confirmDiscard: () => true,
      closeHost,
    });

    expect(opened).toBe(true);
    expect(order).toEqual(['close:null']);
    expect(overlay().open).toBe('app');
    expect(nav().focusRequest).toEqual({ section: 'privacy', controlId: 'section-mfa' });
  });

  // Mutant: setVerificationReturn not called, or called with a fixed `{ kind: 'chat' }`.
  it('records exactly the return the caller named', async () => {
    await openVerificationSetup({
      returnTo: { kind: 'serverSettings', serverId: 's1', section: 'members' },
    });

    expect(overlay().verificationReturn).toEqual({
      kind: 'serverSettings',
      serverId: 's1',
      section: 'members',
    });
  });

  // Mutant: `confirmDiscard !== undefined &&` inverted, or closeHost required rather than optional.
  it('works with neither a guard nor a host to close', async () => {
    await expect(openVerificationSetup({ returnTo: { kind: 'chat' } })).resolves.toBe(true);
    expect(overlay().open).toBe('app');
    expect(overlay().verificationReturn).toEqual({ kind: 'chat' });
  });

  // Mutant: the second apiRequestContextIsCurrent check deleted: the return would name the old account's chat.
  it('opens nothing when the account changed while the discard was being asked', async () => {
    const gate = deferred<boolean>();
    const closeHost = vi.fn();
    const pending = openVerificationSetup({
      returnTo: { kind: 'chat' },
      confirmDiscard: () => gate.promise,
      closeHost,
    });

    switchAccount();
    gate.resolve(true);

    await expect(pending).resolves.toBe(false);
    expect(closeHost).not.toHaveBeenCalled();
    expect(overlay().open).toBeNull();
    expect(overlay().verificationReturn).toBeNull();
    expect(nav().focusRequest).toBeNull();
  });

  // Mutant: the context captured AFTER the guard instead of before it (the switch would go unseen).
  it('a guard that answers synchronously still sees a switch made before it returned', async () => {
    const closeHost = vi.fn();
    const opened = await openVerificationSetup({
      returnTo: { kind: 'chat' },
      confirmDiscard: () => {
        switchAccount();
        return true;
      },
      closeHost,
    });

    expect(opened).toBe(false);
    expect(closeHost).not.toHaveBeenCalled();
    expect(overlay().open).toBeNull();
  });
});

describe('verificationReturnLabel', () => {
  // Mutant: the two labels swapped or collapsed.
  it('names where each return goes', () => {
    expect(verificationReturnLabel({ kind: 'chat' })).toBe('Back to chat');
    expect(
      verificationReturnLabel({ kind: 'serverSettings', serverId: 's1', section: 'general' })
    ).toBe('Back to Server Settings');
  });
});

describe('returnFromVerificationSetup', () => {
  // Mutant: `target === null` guard dropped (it would close or open something with no return pending).
  it('does nothing when no return is pending', async () => {
    const fetchServerPermissions = stubPermissionFetch();
    overlay().openSettings('app');

    await returnFromVerificationSetup();

    expect(fetchServerPermissions).not.toHaveBeenCalled();
    expect(overlay().open).toBe('app');
  });

  // Mutant: the chat arm opening Server Settings, or not closing, or refetching permissions.
  it('chat closes the overlay and refetches nothing', async () => {
    const fetchServerPermissions = stubPermissionFetch();
    await openVerificationSetup({ returnTo: { kind: 'chat' } });
    expect(overlay().open).toBe('app');

    await returnFromVerificationSetup();

    expect(overlay().open).toBeNull();
    expect(overlay().payload).toBeNull();
    expect(overlay().verificationReturn).toBeNull();
    expect(fetchServerPermissions).not.toHaveBeenCalled();
  });

  // Mutant: openSettings called before the refetch resolves (D-6), so Server Settings mounts on the masked snapshot.
  it('serverSettings refetches that server first, and only then reopens it (D-6)', async () => {
    const gate = deferred<void>();
    const fetchServerPermissions = stubPermissionFetch(() => gate.promise);
    await openVerificationSetup({
      returnTo: { kind: 'serverSettings', serverId: 's2', section: 'roles' },
    });

    const returning = returnFromVerificationSetup();
    await Promise.resolve();

    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith('s2');
    // Still on App Settings while the refetch is out.
    expect(overlay().open).toBe('app');

    gate.resolve();
    await returning;

    expect(overlay().open).toBe('server');
    expect(overlay().payload).toEqual({ serverId: 's2', section: 'roles' });
  });

  // Mutant: the post-refetch apiRequestContextIsCurrent check deleted: another account lands in this server's settings.
  it('opens nothing when the account changed during the refetch', async () => {
    const gate = deferred<void>();
    stubPermissionFetch(() => gate.promise);
    await openVerificationSetup({
      returnTo: { kind: 'serverSettings', serverId: 's2', section: 'roles' },
    });

    const returning = returnFromVerificationSetup();
    switchAccount();
    gate.resolve();
    await returning;

    expect(overlay().open).not.toBe('server');
    expect(overlay().payload).toBeNull();
  });

  // Mutant: `getState().verificationReturn` read instead of `takeVerificationReturn()`: a second press acts again.
  it('reads the slot once: a second call finds nothing pending', async () => {
    const fetchServerPermissions = stubPermissionFetch();
    await openVerificationSetup({
      returnTo: { kind: 'serverSettings', serverId: 's2', section: 'general' },
    });

    await returnFromVerificationSetup();
    expect(overlay().verificationReturn).toBeNull();
    overlay().close();
    await returnFromVerificationSetup();

    expect(fetchServerPermissions).toHaveBeenCalledTimes(1);
    expect(overlay().open).toBeNull();
  });

  // Mutant: the leading hasPendingDrafts check dropped: closing the overlay tears the draft down.
  it('an unapplied change holds the chat return: nothing closes and the slot stays', async () => {
    await openVerificationSetup({ returnTo: { kind: 'chat' } });
    stageDraft();

    const left = await returnFromVerificationSetup();

    expect(left).toBe(false);
    expect(overlay().open).toBe('app');
    expect(overlay().verificationReturn).toEqual({ kind: 'chat' });
    expect(useDraftSettingsStore.getState().drafts.appearance).toEqual({ uiScale: 1.25 });
  });

  // Mutant: the leading check moved after takeVerificationReturn or the refetch.
  it('an unapplied change holds the Server Settings return before anything is fetched', async () => {
    const fetchServerPermissions = stubPermissionFetch();
    await openVerificationSetup({
      returnTo: { kind: 'serverSettings', serverId: 's2', section: 'roles' },
    });
    stageDraft({ contentProtection: true });

    const left = await returnFromVerificationSetup();

    expect(left).toBe(false);
    expect(fetchServerPermissions).not.toHaveBeenCalled();
    expect(overlay().open).toBe('app');
    expect(overlay().verificationReturn).toEqual({
      kind: 'serverSettings',
      serverId: 's2',
      section: 'roles',
    });
  });

  // Mutant: the post-refetch hasPendingDrafts check dropped, or the slot not put back.
  it('a change made during the refetch holds the return, and puts the slot back', async () => {
    const gate = deferred<void>();
    stubPermissionFetch(() => gate.promise);
    await openVerificationSetup({
      returnTo: { kind: 'serverSettings', serverId: 's2', section: 'roles' },
    });

    const returning = returnFromVerificationSetup();
    await Promise.resolve();
    expect(overlay().verificationReturn).toBeNull();
    stageDraft();
    gate.resolve();

    expect(await returning).toBe(false);
    expect(overlay().open).toBe('app');
    expect(overlay().verificationReturn).toEqual({
      kind: 'serverSettings',
      serverId: 's2',
      section: 'roles',
    });
  });

  // Control for the three above: with nothing staged the same returns go through.
  it('resolves true once it has left App Settings', async () => {
    stubPermissionFetch();
    await openVerificationSetup({ returnTo: { kind: 'chat' } });
    expect(await returnFromVerificationSetup()).toBe(true);

    await openVerificationSetup({
      returnTo: { kind: 'serverSettings', serverId: 's2', section: 'members' },
    });
    expect(await returnFromVerificationSetup()).toBe(true);
    expect(overlay().open).toBe('server');
  });

  // Mutant: the slot taken only after the refetch: a reentrant press during it acts twice.
  it('takes the slot before the refetch, so a press during it does nothing', async () => {
    const gate = deferred<void>();
    const fetchServerPermissions = stubPermissionFetch(() => gate.promise);
    await openVerificationSetup({
      returnTo: { kind: 'serverSettings', serverId: 's2', section: 'roles' },
    });

    const first = returnFromVerificationSetup();
    const second = returnFromVerificationSetup();
    gate.resolve();
    await Promise.all([first, second]);

    expect(fetchServerPermissions).toHaveBeenCalledTimes(1);
  });
});

describe('the pending return is session-scoped', () => {
  // Mutant: `clearVerificationReturn()` removed from gracefulReset: the next account inherits the return.
  it('gracefulReset clears it', async () => {
    await openVerificationSetup({
      returnTo: { kind: 'serverSettings', serverId: 's2', section: 'roles' },
    });
    expect(overlay().verificationReturn).not.toBeNull();

    gracefulReset();

    expect(overlay().verificationReturn).toBeNull();
  });

  // Mutant: `close` leaving the slot in place: a stale return outlives the Settings excursion.
  it('closing the overlay drops it', async () => {
    await openVerificationSetup({ returnTo: { kind: 'chat' } });

    overlay().close();

    expect(overlay().verificationReturn).toBeNull();
  });
});

describe('hasPendingDrafts', () => {
  // Mutant: any one layer dropped from the predicate: that layer's change is lost on the way out.
  it.each<[string, Partial<DraftOverlays>]>([
    ['appearance', { appearance: { uiScale: 1.25 } }],
    ['audio', { audio: { inputVolume: 50 } }],
    ['video', { video: { supportSvc: false } }],
    ['tts', { tts: { ttsEnabled: true } }],
    ['screen-capture protection', { contentProtection: false }],
  ])('counts a %s draft', (_layer, drafts) => {
    expect(hasPendingDrafts({ ...NO_DRAFTS, ...drafts })).toBe(true);
  });

  it('is false with nothing staged', () => {
    expect(hasPendingDrafts(NO_DRAFTS)).toBe(false);
  });
});
