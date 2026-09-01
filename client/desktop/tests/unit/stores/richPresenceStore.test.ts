import { describe, it, expect, beforeEach, vi, afterEach, beforeAll, afterAll } from 'vitest';
import { useRichPresenceStore } from '@/renderer/stores/ui/richPresenceStore';
import * as richPresenceModule from '@/renderer/stores/ui/richPresenceStore';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { WebSocketService } from '@/renderer/services/messaging/websocketService';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';
import { deferred } from '../../helpers/deferred';

const API_BASE = 'http://localhost:8080';
const PRESENCE_SETTINGS_ENDPOINT = `${API_BASE}/api/v1/users/me/presence-settings`;

const completeSettingsResponse = {
  master_enabled: true,
  server_voice_tier: 1,
  server_voice_show_details: true,
  private_call_tier: 0,
  private_call_show_details: false,
  custom_text_tier: 2,
  custom_text: 'heads down',
  custom_text_emoji: '💻',
};

const settingsStore = () => useRichPresenceStore.getState();

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterAll(() => server.close());
afterEach(() => {
  server.resetHandlers();
  resetRuntimeServerBase();
});

describe('richPresenceStore', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('starts empty with default self presence', () => {
    expect(
      (useRichPresenceStore.getState() as unknown as { customTextByUser?: unknown })
        .customTextByUser
    ).toBeUndefined();
    expect(useRichPresenceStore.getState().self).toEqual({ tier: 0 });
  });

  it('preserves empty custom text distinctly from null or absent values', () => {
    const base = {
      master_enabled: true,
      server_voice_tier: 1,
      server_voice_show_details: true,
      private_call_tier: 0,
      private_call_show_details: false,
      custom_text_tier: 0,
      custom_text_emoji: null,
    };
    const empty = richPresenceModule.parsePresenceSettingsResponse({
      ...base,
      custom_text: '',
    });
    const nullable = richPresenceModule.parsePresenceSettingsResponse({
      ...base,
      custom_text: null,
    });
    const absent = richPresenceModule.parsePresenceSettingsResponse(base);
    expect(empty).toHaveProperty('customText', '');
    expect(nullable).not.toHaveProperty('customText');
    expect(absent).toBeNull();
  });

  describe('setCustomText / getCustomText', () => {
    it('stores and retrieves another user custom text', () => {
      useRichPresenceStore.getState().setCustomText('user-2', { emoji: '🎮', text: 'gaming' });
      expect(useRichPresenceStore.getState().getCustomText('user-2')).toEqual({
        emoji: '🎮',
        text: 'gaming',
      });
    });

    it('stores text without an emoji', () => {
      useRichPresenceStore.getState().setCustomText('user-3', { text: 'heads down' });
      expect(useRichPresenceStore.getState().getCustomText('user-3')).toEqual({
        text: 'heads down',
      });
    });

    it('replaces an existing entry for the same user', () => {
      useRichPresenceStore.getState().setCustomText('user-2', { text: 'first' });
      useRichPresenceStore.getState().setCustomText('user-2', { emoji: '🚀', text: 'second' });
      expect(useRichPresenceStore.getState().getCustomText('user-2')).toEqual({
        emoji: '🚀',
        text: 'second',
      });
      expect(useRichPresenceStore.getState().getCustomText('user-2')).toEqual({
        emoji: '🚀',
        text: 'second',
      });
    });

    it('exposes the map for selective subscription', () => {
      useRichPresenceStore.getState().setCustomText('user-2', { text: 'hi' });
      expect(useRichPresenceStore.getState().getCustomText('user-2')).toEqual({ text: 'hi' });
    });

    it('returns undefined for an unknown user', () => {
      expect(useRichPresenceStore.getState().getCustomText('nobody')).toBeUndefined();
    });
  });

  describe('clearCustomText', () => {
    it('removes a stored entry', () => {
      useRichPresenceStore.getState().setCustomText('user-2', { text: 'gaming' });
      useRichPresenceStore.getState().clearCustomText('user-2');
      expect(useRichPresenceStore.getState().getCustomText('user-2')).toBeUndefined();
      expect(
        (useRichPresenceStore.getState() as unknown as { customTextByUser?: unknown })
          .customTextByUser
      ).toBeUndefined();
    });

    it('leaves other users untouched', () => {
      const store = useRichPresenceStore.getState();
      store.setCustomText('user-2', { text: 'a' });
      store.setCustomText('user-3', { text: 'b' });
      store.clearCustomText('user-2');
      expect(useRichPresenceStore.getState().getCustomText('user-2')).toBeUndefined();
      expect(useRichPresenceStore.getState().getCustomText('user-3')).toEqual({ text: 'b' });
    });

    it('is a no-op for an unknown user', () => {
      useRichPresenceStore.getState().clearCustomText('nobody');
      expect(
        (useRichPresenceStore.getState() as unknown as { otherByUser?: unknown }).otherByUser
      ).toEqual({});
    });
  });

  describe('setSelfPresence', () => {
    it('patches tier and custom text fields', () => {
      useRichPresenceStore
        .getState()
        .setSelfPresence({ tier: 2, customText: 'working', customTextEmoji: '💻' });
      expect(useRichPresenceStore.getState().self).toEqual({
        tier: 2,
        customText: 'working',
        customTextEmoji: '💻',
      });
    });

    it('merges partial updates without dropping prior fields', () => {
      useRichPresenceStore.getState().setSelfPresence({ tier: 1, customText: 'hello' });
      useRichPresenceStore.getState().setSelfPresence({ customTextEmoji: '👋' });
      expect(useRichPresenceStore.getState().self).toEqual({
        tier: 1,
        customText: 'hello',
        customTextEmoji: '👋',
      });
    });
  });

  describe('reset / resetAllStores', () => {
    it('reset() clears the map and restores default self', () => {
      useRichPresenceStore.getState().setCustomText('user-2', { text: 'gaming' });
      useRichPresenceStore.getState().setSelfPresence({ tier: 3, customText: 'x' });
      useRichPresenceStore.getState().reset();
      expect(
        (useRichPresenceStore.getState() as unknown as { otherByUser?: unknown }).otherByUser
      ).toEqual({});
      expect(useRichPresenceStore.getState().self).toEqual({ tier: 0 });
    });

    it('resetAllStores() clears everything', () => {
      useRichPresenceStore.getState().setCustomText('user-2', { emoji: '🎮', text: 'gaming' });
      useRichPresenceStore.getState().setSelfPresence({ tier: 2, customText: 'busy' });
      resetAllStores();
      expect(
        (useRichPresenceStore.getState() as unknown as { otherByUser?: unknown }).otherByUser
      ).toEqual({});
      expect(useRichPresenceStore.getState().self).toEqual({ tier: 0 });
    });
  });
});

describe('presence settings contract (#2234)', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('starts with safe unconfirmed defaults and keeps controls unconfirmed', () => {
    expect(settingsStore().presenceSettings).toEqual({
      masterEnabled: true,
      serverVoiceTier: 1,
      serverVoiceShowDetails: true,
      privateCallTier: 0,
      privateCallShowDetails: false,
      customTextTier: 0,
    });
    expect(settingsStore().confirmedPresenceSettings).toBeNull();
    expect(settingsStore().presenceSettingsLoading).toBe(false);
  });

  it('hydrates all known fields and projects Custom Status atomically', async () => {
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, () => HttpResponse.json(completeSettingsResponse))
    );

    await settingsStore().hydratePresenceSettings();

    expect(settingsStore().presenceSettings).toEqual({
      masterEnabled: true,
      serverVoiceTier: 1,
      serverVoiceShowDetails: true,
      privateCallTier: 0,
      privateCallShowDetails: false,
      customTextTier: 2,
      customText: 'heads down',
      customTextEmoji: '💻',
    });
    expect(settingsStore().confirmedPresenceSettings).toEqual(settingsStore().presenceSettings);
    expect(useRichPresenceStore.getState().self).toEqual({
      tier: 2,
      customText: 'heads down',
      customTextEmoji: '💻',
    });
  });

  it('accepts additive response fields while rejecting incomplete or malformed responses atomically', async () => {
    const malformedResponses = [
      { ...completeSettingsResponse, custom_text: undefined },
      { ...completeSettingsResponse, server_voice_tier: '1' },
      { ...completeSettingsResponse, private_call_tier: 3 },
      { ...completeSettingsResponse, custom_text: '😀'.repeat(141) },
      { ...completeSettingsResponse, custom_text_emoji: '😀'.repeat(33) },
      '{not-json',
      [],
      null,
    ];
    for (const body of malformedResponses) {
      resetAllStores();
      useAuthStore.getState().setAccessToken('mock-token');
      server.use(
        http.get(PRESENCE_SETTINGS_ENDPOINT, () =>
          typeof body === 'string'
            ? new HttpResponse(body, { headers: { 'Content-Type': 'application/json' } })
            : HttpResponse.json(body)
        )
      );
      await settingsStore().hydratePresenceSettings();
      expect(settingsStore().confirmedPresenceSettings).toBeNull();
      expect(settingsStore().presenceSettings).toMatchObject({ masterEnabled: true });
      expect(settingsStore().presenceSettingsError).toBeTruthy();
    }

    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, () =>
        HttpResponse.json({ ...completeSettingsResponse, future_field: 'ignored' })
      )
    );
    await settingsStore().hydratePresenceSettings();
    expect(settingsStore().confirmedPresenceSettings).toEqual(
      expect.objectContaining({ customText: 'heads down' })
    );
    expect(settingsStore().confirmedPresenceSettings).not.toHaveProperty('future_field');
  });

  it.each([
    ['masterEnabled', false, { master_enabled: false }],
    ['serverVoiceTier', 2, { server_voice_tier: 2 }],
    ['serverVoiceShowDetails', false, { server_voice_show_details: false }],
    ['privateCallTier', 1, { private_call_tier: 1 }],
    ['privateCallShowDetails', true, { private_call_show_details: true }],
    ['customTextTier', 1, { custom_text_tier: 1 }],
  ] as const)('sends only the changed %s wire field', async (field, value, expectedBody) => {
    let body: unknown;
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, () => HttpResponse.json(completeSettingsResponse)),
      http.patch(PRESENCE_SETTINGS_ENDPOINT, async ({ request }) => {
        body = await request.json();
        const wireField = Object.keys(expectedBody)[0];
        return HttpResponse.json({ ...completeSettingsResponse, [wireField]: value });
      })
    );
    await settingsStore().hydratePresenceSettings();
    const update = settingsStore().updatePresenceSettings({ [field]: value });
    expect(settingsStore().presenceSettings[field]).toBe(value);
    await update;
    expect(body).toEqual(expectedBody);
    expect(settingsStore().confirmedPresenceSettings?.[field]).toBe(value);
  });

  it('does not PATCH when an update matches the normalized settings', async () => {
    let patchCount = 0;
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, () => HttpResponse.json(completeSettingsResponse)),
      http.patch(PRESENCE_SETTINGS_ENDPOINT, () => {
        patchCount += 1;
        return HttpResponse.json(completeSettingsResponse);
      })
    );
    await settingsStore().hydratePresenceSettings();
    await settingsStore().updatePresenceSettings({
      masterEnabled: true,
      serverVoiceTier: 1,
      serverVoiceShowDetails: true,
      privateCallTier: 0,
      privateCallShowDetails: false,
      customTextTier: 2,
    });
    expect(patchCount).toBe(0);
    expect(settingsStore().presenceSettings).toEqual(settingsStore().confirmedPresenceSettings);
    expect(settingsStore().presenceSettingsSaving).toBe(false);
  });

  it('preserves a newer local custom status while hydration settles', async () => {
    const response = deferred<typeof completeSettingsResponse>();
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, async () => {
        await response.promise;
        return HttpResponse.json(completeSettingsResponse);
      })
    );
    const hydration = settingsStore().hydratePresenceSettings();
    useRichPresenceStore.getState().setSelfPresence({
      customText: 'newer local text',
      customTextEmoji: '🆕',
    });
    response.resolve();
    await hydration;
    expect(settingsStore().confirmedPresenceSettings).not.toBeNull();
    expect(settingsStore().presenceSettings).toMatchObject({
      customText: 'newer local text',
      customTextEmoji: '🆕',
    });
    expect(settingsStore().confirmedPresenceSettings).toMatchObject({
      customText: 'newer local text',
      customTextEmoji: '🆕',
    });
    expect(useRichPresenceStore.getState().self).toEqual({
      tier: 2,
      customText: 'newer local text',
      customTextEmoji: '🆕',
    });
    expect(settingsStore().presenceSettingsLoading).toBe(false);
  });

  it.each([
    ['successful response', true],
    ['failed rollback', false],
  ])(
    'preserves a newer local custom status through a deferred PATCH (%s)',
    async (_name, succeeds) => {
      const patchResponse = deferred<void>();
      server.use(
        http.get(PRESENCE_SETTINGS_ENDPOINT, () => HttpResponse.json(completeSettingsResponse)),
        http.patch(PRESENCE_SETTINGS_ENDPOINT, async () => {
          await patchResponse.promise;
          return succeeds
            ? HttpResponse.json({ ...completeSettingsResponse, master_enabled: false })
            : HttpResponse.json({ error: 'save failed' }, { status: 500 });
        })
      );
      await settingsStore().hydratePresenceSettings();
      const baseline = settingsStore().confirmedPresenceSettings;
      const update = settingsStore().updatePresenceSettings({ masterEnabled: false });
      useRichPresenceStore.getState().setSelfPresence({
        customText: 'newer local text',
        customTextEmoji: '🆕',
      });
      patchResponse.resolve();
      await update;
      expect(useRichPresenceStore.getState().self).toMatchObject({
        customText: 'newer local text',
        customTextEmoji: '🆕',
      });
      expect(settingsStore().presenceSettings).toMatchObject({
        customText: 'newer local text',
        customTextEmoji: '🆕',
      });
      expect(settingsStore().confirmedPresenceSettings).toMatchObject({
        customText: 'newer local text',
        customTextEmoji: '🆕',
      });
      expect(settingsStore().presenceSettingsSaving).toBe(false);
      expect(settingsStore().presenceSettingsLoading).toBe(false);
      if (succeeds) {
        expect(settingsStore().confirmedPresenceSettings?.masterEnabled).toBe(false);
      } else {
        expect(settingsStore().presenceSettings?.masterEnabled).toBe(baseline?.masterEnabled);
        expect(settingsStore().confirmedPresenceSettings?.masterEnabled).toBe(
          baseline?.masterEnabled
        );
      }
    }
  );

  it('deduplicates hydration while a PATCH is saving', async () => {
    let getCount = 0;
    const patchResponse = deferred<typeof completeSettingsResponse>();
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, () => {
        getCount += 1;
        return HttpResponse.json(completeSettingsResponse);
      }),
      http.patch(PRESENCE_SETTINGS_ENDPOINT, async () =>
        HttpResponse.json(await patchResponse.promise)
      )
    );
    await settingsStore().hydratePresenceSettings();
    const update = settingsStore().updatePresenceSettings({ masterEnabled: false });
    expect(settingsStore().presenceSettingsSaving).toBe(true);
    await settingsStore().hydratePresenceSettings();
    expect(getCount).toBe(1);
    patchResponse.resolve({ ...completeSettingsResponse, master_enabled: false });
    await update;
    expect(settingsStore().confirmedPresenceSettings?.masterEnabled).toBe(false);
    expect(settingsStore().presenceSettingsLoading).toBe(false);
    expect(settingsStore().presenceSettingsSaving).toBe(false);
  });

  it('fences a same-account auth-adoption PATCH while successor hydration takes ownership', async () => {
    const patchResponse = deferred<typeof completeSettingsResponse>();
    let getCount = 0;
    const successorResponse = {
      ...completeSettingsResponse,
      master_enabled: false,
      custom_text: 'local',
      custom_text_emoji: null,
    };
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, () => {
        getCount += 1;
        return HttpResponse.json(getCount === 1 ? completeSettingsResponse : successorResponse);
      }),
      http.patch(PRESENCE_SETTINGS_ENDPOINT, async () =>
        HttpResponse.json(await patchResponse.promise)
      )
    );
    await settingsStore().hydratePresenceSettings();
    useRichPresenceStore.getState().setSelfPresence({ tier: 2, customText: 'local' });
    useRichPresenceStore.getState().setOtherPresence('remote', {
      category: 'server_voice',
      payload: {} as never,
    } as never);
    const update = settingsStore().updatePresenceSettings({ masterEnabled: false });
    expect(settingsStore().presenceSettings.masterEnabled).toBe(false);

    const currentGeneration = useAuthStore.getState().authGeneration;
    useAuthStore
      .getState()
      .beginAuthLifecycleIfCurrent(currentGeneration, 'adopted-token', 'adopted-session');
    const successorHydration = settingsStore().hydratePresenceSettings();
    await successorHydration;
    expect(getCount).toBe(2);
    expect(settingsStore().presenceSettings.masterEnabled).toBe(false);

    patchResponse.resolve({ ...completeSettingsResponse, master_enabled: true });
    await update;
    expect(settingsStore().presenceSettings.masterEnabled).toBe(false);
    expect(settingsStore().confirmedPresenceSettings?.masterEnabled).toBe(false);
    expect(useRichPresenceStore.getState().self).toEqual({ tier: 2, customText: 'local' });
    expect(useRichPresenceStore.getState().otherByUser).toHaveProperty('remote');
    expect(settingsStore().presenceSettingsLoading).toBe(false);
    expect(settingsStore().presenceSettingsSaving).toBe(false);
  });

  it.each([
    ['5xx', () => HttpResponse.json({ error: 'nope' }, { status: 500 })],
    ['transport', () => HttpResponse.error()],
    ['malformed 2xx', () => new HttpResponse('{bad', { status: 200 })],
    ['schema-invalid 2xx', () => HttpResponse.json({ custom_text_tier: 9 })],
  ])('reconciles a %s write against the authoritative GET', async (_name, response) => {
    let getCount = 0;
    let patchCount = 0;
    const authoritative = { ...completeSettingsResponse, private_call_show_details: false };
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, () => {
        getCount += 1;
        return HttpResponse.json(getCount === 1 ? completeSettingsResponse : authoritative);
      }),
      http.patch(PRESENCE_SETTINGS_ENDPOINT, () => {
        patchCount += 1;
        return response();
      })
    );
    await settingsStore().hydratePresenceSettings();
    await settingsStore().updatePresenceSettings({ privateCallShowDetails: true });
    expect(getCount).toBe(2);
    expect(patchCount).toBe(1);
    expect(settingsStore().presenceSettings.privateCallShowDetails).toBe(false);
    expect(settingsStore().confirmedPresenceSettings?.privateCallShowDetails).toBe(false);
    expect(settingsStore().presenceSettingsError).toBeNull();
  });

  it('rolls back a definite 4xx without reconciliation', async () => {
    let getCount = 0;
    let patchCount = 0;
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, () => {
        getCount += 1;
        return HttpResponse.json(completeSettingsResponse);
      }),
      http.patch(PRESENCE_SETTINGS_ENDPOINT, () => {
        patchCount += 1;
        return HttpResponse.json({ error: 'rejected' }, { status: 400 });
      })
    );
    await settingsStore().hydratePresenceSettings();
    await settingsStore().updatePresenceSettings({ privateCallShowDetails: true });
    expect(patchCount).toBe(1);
    expect(getCount).toBe(1);
    expect(settingsStore().presenceSettings.privateCallShowDetails).toBe(false);
    expect(settingsStore().confirmedPresenceSettings?.privateCallShowDetails).toBe(false);
  });

  it('does not misclassify failed custom reconciliation as a direct custom success', async () => {
    const categoryResponse = deferred<Response>();
    let getCount = 0;
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, () => {
        getCount += 1;
        return getCount === 1
          ? HttpResponse.json(completeSettingsResponse)
          : HttpResponse.json({ error: 'custom reconciliation failed' }, { status: 503 });
      }),
      http.patch(PRESENCE_SETTINGS_ENDPOINT, () => categoryResponse.promise)
    );
    await settingsStore().hydratePresenceSettings();

    const categoryMutation = settingsStore().updatePresenceSettings({ serverVoiceTier: 2 });
    expect(settingsStore().presenceSettingsSaving).toBe(true);

    const customStatusTicket = settingsStore().captureCustomStatusSubmission();
    expect(customStatusTicket).not.toBeNull();
    const reconciliation = settingsStore().reconcileCustomStatusAmbiguousOutcome(
      customStatusTicket!
    );
    await reconciliation;
    expect(settingsStore().presenceSettingsError).toBe('custom reconciliation failed');
    settingsStore().releaseCustomStatusSubmission(customStatusTicket!);

    categoryResponse.resolve(
      new Response(JSON.stringify({ error: 'category rejected' }), {
        status: 400,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await categoryMutation;

    expect(settingsStore().presenceSettingsError).toBe('category rejected');
    expect(settingsStore().presenceSettings.serverVoiceTier).toBe(1);
    expect(settingsStore().presenceSettingsSaving).toBe(false);
    expect(settingsStore().confirmedPresenceSettings).toBeNull();
    expect(getCount).toBe(2);
  });

  it('preserves a direct custom success when a pending category 4xx rolls back', async () => {
    const categoryResponse = deferred<Response>();
    let patchCount = 0;
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, () => HttpResponse.json(completeSettingsResponse)),
      http.patch(PRESENCE_SETTINGS_ENDPOINT, () => {
        patchCount += 1;
        return categoryResponse.promise;
      })
    );
    await settingsStore().hydratePresenceSettings();

    const categoryMutation = settingsStore().updatePresenceSettings({ serverVoiceTier: 2 });
    expect(settingsStore().presenceSettingsSaving).toBe(true);

    const directTicket = settingsStore().captureCustomStatusSubmission();
    expect(directTicket).not.toBeNull();
    const directSettings = richPresenceModule.parsePresenceSettingsResponse({
      ...completeSettingsResponse,
      custom_text: 'direct success',
      custom_text_emoji: '🛰️',
    });
    expect(directSettings).not.toBeNull();
    const directResult = settingsStore().applyCustomStatusSettings(directSettings!, directTicket!);
    expect(directResult).toMatchObject({ activityCurrent: false, customCurrent: true });
    settingsStore().releaseCustomStatusSubmission(directTicket!);
    expect(settingsStore().confirmedPresenceSettings).toBeNull();
    expect(settingsStore().self).toMatchObject({
      customText: 'direct success',
      customTextEmoji: '🛰️',
    });

    categoryResponse.resolve(
      new Response(JSON.stringify({ error: 'category rejected' }), {
        status: 400,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await categoryMutation;

    expect(patchCount).toBe(1);
    expect(settingsStore().presenceSettings.serverVoiceTier).toBe(1);
    expect(settingsStore().self).toMatchObject({
      customText: 'direct success',
      customTextEmoji: '🛰️',
    });
    expect(settingsStore().confirmedPresenceSettings).toBeNull();
    expect(settingsStore().presenceSettingsError).toBe(
      'Settings changed while saving your status. Reload settings to continue.'
    );
  });

  it.each([
    ['definite 4xx', 400],
    ['ambiguous 5xx', 503],
  ] as const)(
    'does not leak a predecessor rollback into a successor auth lifecycle (%s)',
    async (_name, status) => {
      let getCount = 0;
      let patchCount = 0;
      server.use(
        http.get(PRESENCE_SETTINGS_ENDPOINT, () => {
          getCount += 1;
          return HttpResponse.json(completeSettingsResponse);
        }),
        http.patch(PRESENCE_SETTINGS_ENDPOINT, () => {
          patchCount += 1;
          return HttpResponse.json({ error: 'predecessor failure' }, { status });
        })
      );
      await settingsStore().hydratePresenceSettings();
      const generation = useAuthStore.getState().authGeneration;
      let invalidated = false;
      let unsubscribe: (() => void) | undefined;
      unsubscribe = useRichPresenceStore.subscribe((state, previous) => {
        if (!previous.presenceSettingsSaving || state.presenceSettingsSaving) return;
        invalidated = true;
        useAuthStore
          .getState()
          .beginAuthLifecycleIfCurrent(generation, 'successor-token', 'successor-session');
        unsubscribe?.();
      });

      await settingsStore().updatePresenceSettings({ privateCallShowDetails: true });

      expect(invalidated).toBe(true);
      expect(patchCount).toBe(1);
      expect(getCount).toBe(1);
      expect(settingsStore().presenceSettingsError).toBeNull();
    }
  );

  it('keeps an ambiguous write unconfirmed when reconciliation fails, then allows a later hydrate', async () => {
    let getCount = 0;
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, () => {
        getCount += 1;
        if (getCount === 1) return HttpResponse.json(completeSettingsResponse);
        if (getCount === 2) return HttpResponse.json({ error: 'load failed' }, { status: 503 });
        return HttpResponse.json({ ...completeSettingsResponse, private_call_show_details: true });
      }),
      http.patch(PRESENCE_SETTINGS_ENDPOINT, () => HttpResponse.error())
    );
    await settingsStore().hydratePresenceSettings();
    await settingsStore().updatePresenceSettings({ privateCallShowDetails: true });
    expect(settingsStore().confirmedPresenceSettings).toBeNull();
    expect(settingsStore().presenceSettingsError).toBeTruthy();
    await settingsStore().hydratePresenceSettings();
    expect(settingsStore().confirmedPresenceSettings?.privateCallShowDetails).toBe(true);
    expect(settingsStore().presenceSettingsError).toBeNull();
  });

  it('does not let a GET started before a mutation clobber optimistic state', async () => {
    const getResponse = deferred<void>();
    let getCount = 0;
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, async () => {
        getCount += 1;
        if (getCount === 1) return HttpResponse.json(completeSettingsResponse);
        await getResponse.promise;
        return HttpResponse.json(completeSettingsResponse);
      }),
      http.patch(PRESENCE_SETTINGS_ENDPOINT, () =>
        HttpResponse.json({ ...completeSettingsResponse, master_enabled: false })
      )
    );
    await settingsStore().hydratePresenceSettings();
    const hydration = settingsStore().hydratePresenceSettings();
    const mutation = settingsStore().updatePresenceSettings({ masterEnabled: false });
    expect(settingsStore().presenceSettings.masterEnabled).toBe(false);
    await mutation;
    getResponse.resolve();
    await hydration;
    expect(settingsStore().presenceSettings.masterEnabled).toBe(false);
    expect(settingsStore().presenceSettingsLoading).toBe(false);
    expect(settingsStore().presenceSettingsSaving).toBe(false);
  });

  it('keeps safe unconfirmed state when reset supersedes a pending GET', async () => {
    const response = deferred<void>();
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, async () => {
        await response.promise;
        return HttpResponse.json(completeSettingsResponse);
      })
    );
    const hydration = settingsStore().hydratePresenceSettings();
    useRichPresenceStore.getState().reset();
    response.resolve();
    await hydration;
    expect(settingsStore().presenceSettings).toEqual({
      masterEnabled: true,
      serverVoiceTier: 1,
      serverVoiceShowDetails: true,
      privateCallTier: 0,
      privateCallShowDetails: false,
      customTextTier: 0,
    });
    expect(settingsStore().confirmedPresenceSettings).toBeNull();
    expect(useRichPresenceStore.getState().self).toEqual({ tier: 0 });
    expect(settingsStore().presenceSettingsLoading).toBe(false);
    expect(settingsStore().presenceSettingsSaving).toBe(false);
    expect(settingsStore().presenceSettingsError).toBeNull();
  });

  it('ignores stale auth and runtime continuations and reset invalidates pending work', async () => {
    const response = deferred<void>();
    const requestStarted = deferred<void>();
    let getCount = 0;
    const successorResponse = { ...completeSettingsResponse, master_enabled: false };
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, async () => {
        getCount += 1;
        if (getCount === 1) return HttpResponse.json(completeSettingsResponse);
        if (getCount === 2) {
          requestStarted.resolve();
          await response.promise;
          return HttpResponse.json(completeSettingsResponse);
        }
        return HttpResponse.json(successorResponse);
      })
    );
    await settingsStore().hydratePresenceSettings();
    const baseline = settingsStore().presenceSettings;
    const hydration = settingsStore().hydratePresenceSettings();
    await requestStarted.promise;
    useAuthStore.getState().setAccessToken('successor-token');
    const successorHydration = settingsStore().hydratePresenceSettings();
    await successorHydration;
    response.resolve();
    await hydration;
    expect(settingsStore().presenceSettings.masterEnabled).toBe(false);
    expect(settingsStore().confirmedPresenceSettings?.masterEnabled).toBe(false);
    expect(settingsStore().presenceSettings).not.toEqual(baseline);
    expect(settingsStore().presenceSettingsLoading).toBe(false);
    expect(settingsStore().presenceSettingsError).toBeNull();

    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    const runtimeResponse = deferred<void>();
    server.use(
      http.get(PRESENCE_SETTINGS_ENDPOINT, async () => {
        await runtimeResponse.promise;
        return HttpResponse.json(completeSettingsResponse);
      })
    );
    const runtimeHydration = settingsStore().hydratePresenceSettings();
    useRichPresenceStore.getState().setSelfPresence({ tier: 2, customText: 'local' });
    useRichPresenceStore.getState().setOtherPresence('remote', {
      category: 'server_voice',
      payload: {} as never,
    } as never);
    setRuntimeServerBase('http://other-server:8080');
    runtimeResponse.resolve();
    await runtimeHydration;
    expect(settingsStore().presenceSettings).toEqual({
      masterEnabled: true,
      serverVoiceTier: 1,
      serverVoiceShowDetails: true,
      privateCallTier: 0,
      privateCallShowDetails: false,
      customTextTier: 0,
    });
    expect(settingsStore().confirmedPresenceSettings).toBeNull();
    expect(settingsStore().presenceSettingsLoading).toBe(false);
    expect(settingsStore().presenceSettingsError).toBeNull();
    expect(useRichPresenceStore.getState().otherByUser).toEqual({});
    expect(useRichPresenceStore.getState().self).toEqual({ tier: 2, customText: 'local' });
    useRichPresenceStore.getState().reset();
    expect(useRichPresenceStore.getState().self).toEqual({ tier: 0 });
  });
});

describe('rich presence category map and local selector (#2233)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  type Contract = {
    otherByUser: Record<string, Record<string, unknown>>;
    setOtherPresence: (userId: string, entry: { category: string }) => void;
    clearOtherPresence: (userId: string, category: string) => void;
    replaceOtherPresence: (next: Record<string, Record<string, unknown>>) => void;
    clearAllOtherPresence: () => void;
  };
  const contract = () => useRichPresenceStore.getState() as unknown as Contract;

  it('keeps Server Voice and Private Call for one user as distinct category tuples', () => {
    const store = contract();
    expect(typeof store.setOtherPresence).toBe('function');
    if (typeof store.setOtherPresence !== 'function') return;
    store.setOtherPresence('other-user', { category: 'server_voice' });
    store.setOtherPresence('other-user', { category: 'private_call' });
    expect(contract().otherByUser['other-user']).toEqual({
      server_voice: { category: 'server_voice' },
      private_call: { category: 'private_call' },
    });
  });

  it('clears one tuple and prunes an empty user without touching another category', () => {
    const store = contract();
    expect(typeof store.replaceOtherPresence).toBe('function');
    if (typeof store.replaceOtherPresence !== 'function') return;
    store.replaceOtherPresence({
      'other-user': {
        server_voice: { category: 'server_voice' },
        private_call: { category: 'private_call' },
      },
    });
    store.clearOtherPresence('other-user', 'server_voice');
    expect(contract().otherByUser['other-user']).toEqual({
      private_call: { category: 'private_call' },
    });
    store.clearOtherPresence('other-user', 'private_call');
    expect(contract().otherByUser).toEqual({});
  });

  it('replaces the full map atomically and local activity selection is settings-independent', () => {
    const store = contract();
    expect(typeof store.replaceOtherPresence).toBe('function');
    if (typeof store.replaceOtherPresence === 'function') {
      store.replaceOtherPresence({ prior: { custom_text: { category: 'custom_text' } } });
      store.replaceOtherPresence({ current: { server_voice: { category: 'server_voice' } } });
      expect(contract().otherByUser).toEqual({
        current: { server_voice: { category: 'server_voice' } },
      });
    }
    const select = (
      richPresenceModule as unknown as {
        selectLocalRichPresenceActivity?: (state: unknown) => unknown;
      }
    ).selectLocalRichPresenceActivity;
    expect(typeof select).toBe('function');
    if (select) {
      const beforePresence = useRichPresenceStore.getState();
      const send = vi.spyOn(WebSocketService.prototype, 'send');
      try {
        useVoiceStore.setState({
          connectionState: 'connected',
          activeChannelId: 'channel-1',
          activeServerId: 'server-1',
          activeChannelName: 'Lounge',
          isDMCall: false,
          callState: { kind: 'idle' },
        });
        const serverVoiceInput = useVoiceStore.getState();
        const serverVoiceSnapshot = {
          activeChannelId: serverVoiceInput.activeChannelId,
          activeChannelName: serverVoiceInput.activeChannelName,
          activeServerId: serverVoiceInput.activeServerId,
          callStateKind: serverVoiceInput.callState.kind,
          connectionState: serverVoiceInput.connectionState,
          isDMCall: serverVoiceInput.isDMCall,
        };
        expect(select(serverVoiceInput)).toEqual({
          category: 'server_voice',
          channelId: 'channel-1',
          channelName: 'Lounge',
          serverId: 'server-1',
        });
        expect(useVoiceStore.getState()).toBe(serverVoiceInput);
        expect({
          activeChannelId: serverVoiceInput.activeChannelId,
          activeChannelName: serverVoiceInput.activeChannelName,
          activeServerId: serverVoiceInput.activeServerId,
          callStateKind: serverVoiceInput.callState.kind,
          connectionState: serverVoiceInput.connectionState,
          isDMCall: serverVoiceInput.isDMCall,
        }).toEqual(serverVoiceSnapshot);
        for (const kind of [
          'idle',
          'outgoing-ringing',
          'incoming-ringing',
          'joining',
          'ending',
        ] as const) {
          if (kind === 'idle') {
            useVoiceStore.setState({ isDMCall: true, callState: { kind } });
            expect(select(useVoiceStore.getState())).toBeNull();
            useVoiceStore.setState({ isDMCall: false });
            continue;
          }
          useVoiceStore.setState({
            callState: kind === 'ending' ? { kind } : ({ kind, conversationId: 'call-1' } as never),
          });
          expect(select(useVoiceStore.getState())).toBeNull();
        }
        useVoiceStore.setState({
          connectionState: 'connected',
          isDMCall: true,
          callState: { kind: 'in-call' },
          isGroupDM: false,
        });
        expect(select(useVoiceStore.getState())).toEqual({
          category: 'private_call',
          callType: 'dm',
        });
        useVoiceStore.setState({ isGroupDM: true });
        useVoiceStore.setState({
          participants: { a: { userId: 'a' }, b: { userId: 'b' } } as never,
        });
        expect(select(useVoiceStore.getState())).toEqual({
          category: 'private_call',
          callType: 'group',
          participantCount: 2,
        });
        for (const connectionState of ['disconnected', 'reconnecting', 'error'] as const) {
          useVoiceStore.setState({ connectionState });
          expect(select(useVoiceStore.getState())).toBeNull();
        }
        expect(useVoiceStore.getState()).toEqual(
          expect.objectContaining({ connectionState: 'error' })
        );
        expect(useRichPresenceStore.getState()).toBe(beforePresence);
        expect(send).not.toHaveBeenCalled();
      } finally {
        send.mockRestore();
      }
    }
  });

  it('compatibility Custom Status APIs delegate to the canonical map and no second projection exists', () => {
    useRichPresenceStore.getState().setCustomText('other-user', { text: 'hello' });
    expect(useRichPresenceStore.getState().getCustomText('other-user')).toEqual({ text: 'hello' });
    const current = useRichPresenceStore.getState();
    const state = current as unknown as {
      otherByUser?: Record<string, unknown>;
      customTextByUser?: unknown;
    };
    expect(state.otherByUser).toMatchObject({ 'other-user': { custom_text: expect.anything() } });
    expect(state.customTextByUser).toBeUndefined();
    const selector = (
      richPresenceModule as unknown as {
        selectCustomText?: (userId: string) => (s: unknown) => unknown;
      }
    ).selectCustomText;
    expect(typeof selector).toBe('function');
    if (selector) expect(selector('other-user')(current)).toEqual({ text: 'hello' });
  });

  it('clearAllOtherPresence removes every remote category while self survives', () => {
    const store = contract();
    expect(typeof store.clearAllOtherPresence).toBe('function');
    if (typeof store.clearAllOtherPresence !== 'function') return;
    useRichPresenceStore.setState({
      otherByUser: {
        remote: {
          server_voice: { category: 'server_voice' },
          private_call: { category: 'private_call' },
          custom_text: { category: 'custom_text' },
        },
      },
    } as never);
    useRichPresenceStore.getState().setSelfPresence({ tier: 2, customText: 'mine' });
    useRichPresenceStore.getState().clearAllOtherPresence();
    expect(
      (useRichPresenceStore.getState() as unknown as { otherByUser?: unknown }).otherByUser
    ).toEqual({});
    expect(useRichPresenceStore.getState().self.customText).toBe('mine');
  });
});
