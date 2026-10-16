import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import {
  captureRuntimeServerSelection,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import { gracefulReset } from '@/renderer/services/system/resetService';
import {
  captureApiRequestContext,
  type ApiRequestContext,
} from '@/renderer/services/system/requestContext';

const BASE = 'http://localhost:8080';
const TOKEN = 'test-token-privacy-owner';
const requests: Array<{
  body: Record<string, unknown>;
  authorization: string | null;
  url: string;
}> = [];
let confirmed: Record<string, unknown>;
let patchResponse: ((body: Record<string, unknown>) => Promise<Response> | Response) | null;
const openResponses = new Set<(response: Response) => void>();

beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterAll(() => server.close());
beforeEach(async () => {
  resetAllStores();
  setRuntimeServerBase(BASE);
  useAuthStore.getState().beginAuthLifecycle(TOKEN, 'test-privacy-session');
  requests.length = 0;
  patchResponse = null;
  confirmed = {
    messages_friends_only: true,
    messages_server_members: true,
    dm_privacy_level: 2,
    dm_friends_of_friends: false,
    auto_accept_friend_codes: false,
    searchable_by_username: false,
    searchable_by_email: false,
    searchable_by_phone: false,
    allow_embedded_content: false,
    load_gifs_automatically: false,
    share_personalization_with_gif_provider: true,
    require_auth_before_purge: true,
    allow_friend_requests_from: 'everyone',
  };
  server.use(
    http.get('*/api/v1/users/me/privacy', () => HttpResponse.json({ privacy: confirmed })),
    http.patch('*/api/v1/users/me/privacy', async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      requests.push({
        body,
        authorization: request.headers.get('Authorization'),
        url: request.url,
      });
      if (patchResponse) return patchResponse(body);
      confirmed = { ...confirmed, ...body };
      return HttpResponse.json({ privacy: confirmed });
    })
  );
  await usePrivacyStore.getState().fetchPrivacy();
  expect(usePrivacyStore.getState().loaded).toBe(true);
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
});
afterEach(() => {
  usePrivacyStore.getState().clearPrivacy();
  for (const release of openResponses) release(new HttpResponse(null, { status: 503 }));
  openResponses.clear();
  vi.restoreAllMocks();
  vi.clearAllTimers();
  vi.useRealTimers();
  server.resetHandlers();
});

async function advance(ms = 350) {
  await vi.advanceTimersByTimeAsync(ms);
}

async function untilSettled(check: () => void) {
  vi.useRealTimers();
  await vi.waitFor(check);
}

function deferredResponse() {
  let release!: (response: Response) => void;
  const promise = new Promise<Response>((resolve) => {
    release = (response) => {
      openResponses.delete(release);
      resolve(response);
    };
    openResponses.add(release);
  });
  return { promise, release };
}

const disjointSelections = [
  {
    name: 'DM',
    initial: { dm_privacy_level: 0, allow_friend_requests_from: 'everyone' },
    field: 'dmPrivacyLevel',
    wireField: 'dm_privacy_level',
    initialValue: 0,
    selected: 3,
    otherField: 'allowFriendRequestsFrom',
    otherWireField: 'allow_friend_requests_from',
    otherSelected: 'nobody',
    schedule: () => usePrivacyStore.getState().scheduleDMPrivacy(3),
    scheduleOther: () => usePrivacyStore.getState().scheduleFriendRequestPrivacy('nobody'),
    pending: () => usePrivacyStore.getState().pendingDmPrivacyLevel,
    otherPending: () => usePrivacyStore.getState().pendingFriendRequestMode,
    directValue: 1,
    directUpdate: (context?: ApiRequestContext) =>
      usePrivacyStore.getState().updatePrivacy({ dmPrivacyLevel: 1 }, undefined, context),
    error: () => usePrivacyStore.getState().dmSaveError,
  },
  {
    name: 'friend-request',
    initial: { dm_privacy_level: 0, allow_friend_requests_from: 'nobody' },
    field: 'allowFriendRequestsFrom',
    wireField: 'allow_friend_requests_from',
    initialValue: 'nobody',
    selected: 'everyone',
    otherField: 'dmPrivacyLevel',
    otherWireField: 'dm_privacy_level',
    otherSelected: 3,
    schedule: () => usePrivacyStore.getState().scheduleFriendRequestPrivacy('everyone'),
    scheduleOther: () => usePrivacyStore.getState().scheduleDMPrivacy(3),
    pending: () => usePrivacyStore.getState().pendingFriendRequestMode,
    otherPending: () => usePrivacyStore.getState().pendingDmPrivacyLevel,
    directValue: 'mutual_servers',
    directUpdate: (context?: ApiRequestContext) =>
      usePrivacyStore
        .getState()
        .updatePrivacy({ allowFriendRequestsFrom: 'mutual_servers' }, undefined, context),
    error: () => usePrivacyStore.getState().friendRequestError,
  },
] as const;

describe.each(disjointSelections)('$name queue acceptance after a disjoint update', (selection) => {
  it.each([
    { commitOrder: 'after', responseOrder: 'last' },
    { commitOrder: 'before', responseOrder: 'last' },
    { commitOrder: 'after', responseOrder: 'first' },
    { commitOrder: 'before', responseOrder: 'first' },
  ] as const)(
    'keeps both confirmed values when its server commit occurs $commitOrder the disjoint commit and its response arrives $responseOrder',
    async ({ commitOrder, responseOrder }) => {
      confirmed = { ...confirmed, ...selection.initial };
      await usePrivacyStore.getState().fetchPrivacy();
      const first = deferredResponse();
      const second = deferredResponse();
      let firstBody: Record<string, unknown> = {};
      let firstEcho: Record<string, unknown> = {};
      let secondEcho: Record<string, unknown> = {};
      patchResponse = (body) => {
        if (selection.wireField in body) {
          firstBody = body;
          if (commitOrder === 'before') {
            confirmed = { ...confirmed, ...body };
            firstEcho = { ...confirmed };
          }
          return first.promise;
        }
        confirmed = { ...confirmed, ...body };
        secondEcho = { ...confirmed };
        return second.promise;
      };

      const { authGeneration: generation } = useAuthStore.getState();
      const origin = captureRuntimeServerSelection();
      selection.schedule();
      await advance();
      await untilSettled(() => expect(requests).toHaveLength(1));
      expect(requests[0]?.body).toEqual({ [selection.wireField]: selection.selected });
      expect(selection.pending()).toBe(selection.selected);

      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      selection.scheduleOther();
      await advance();
      await untilSettled(() =>
        expect(confirmed[selection.otherWireField]).toBe(selection.otherSelected)
      );
      if (commitOrder === 'after') {
        expect(confirmed[selection.wireField]).toBe(selection.initialValue);
        confirmed = { ...confirmed, ...firstBody };
        firstEcho = { ...confirmed };
      }
      if (responseOrder === 'last') {
        second.release(HttpResponse.json({ privacy: secondEcho }));
        await untilSettled(() => expect(selection.otherPending()).toBeNull());
        expect(usePrivacyStore.getState().settings[selection.otherField]).toBe(
          selection.otherSelected
        );
        expect(selection.pending()).toBe(selection.selected);
        first.release(HttpResponse.json({ privacy: firstEcho }));
        await untilSettled(() => expect(selection.pending()).toBeNull());
      } else {
        first.release(HttpResponse.json({ privacy: firstEcho }));
        await untilSettled(() => expect(selection.pending()).toBeNull());
        second.release(HttpResponse.json({ privacy: secondEcho }));
        await untilSettled(() => expect(selection.otherPending()).toBeNull());
      }

      // Both pending choices have disappeared, so PrivacySection displays the
      // confirmed values; compare those with the synthetic server's actual row.
      expect(confirmed[selection.wireField]).toBe(selection.selected);
      expect(confirmed[selection.otherWireField]).toBe(selection.otherSelected);
      expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.selected);
      expect(usePrivacyStore.getState().settings[selection.otherField]).toBe(
        selection.otherSelected
      );
      expect(selection.error()).toBeNull();
      expect(usePrivacyStore.getState().error).toBeNull();
      expect(useAuthStore.getState().authGeneration).toBe(generation);
      expect(useAuthStore.getState().accessToken).toBe(TOKEN);
      expect(captureRuntimeServerSelection()).toEqual(origin);
      expect(requests.map(({ body }) => body)).toEqual([
        { [selection.wireField]: selection.selected },
        { [selection.otherWireField]: selection.otherSelected },
      ]);
    }
  );

  it.each([
    {
      name: 'account',
      replace: () =>
        useAuthStore
          .getState()
          .beginAuthLifecycle('test-successor-token', 'test-successor-session'),
    },
    { name: 'server', replace: () => setRuntimeServerBase('http://localhost:9090') },
  ])(
    'rejects its old accepted field after a disjoint success and $name replacement',
    async ({ replace }) => {
      confirmed = { ...confirmed, ...selection.initial };
      await usePrivacyStore.getState().fetchPrivacy();
      const first = deferredResponse();
      let firstBody: Record<string, unknown> = {};
      patchResponse = (body) => {
        if (selection.wireField in body) {
          firstBody = body;
          return first.promise;
        }
        confirmed = { ...confirmed, ...body };
        return HttpResponse.json({ privacy: { ...confirmed } });
      };
      selection.schedule();
      await advance();
      await untilSettled(() => expect(requests).toHaveLength(1));
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      selection.scheduleOther();
      await advance();
      await untilSettled(() => expect(selection.otherPending()).toBeNull());
      expect(usePrivacyStore.getState().settings[selection.otherField]).toBe(
        selection.otherSelected
      );
      replace();
      confirmed = { ...confirmed, ...firstBody };
      first.release(HttpResponse.json({ privacy: { ...confirmed } }));
      await untilSettled(() => expect(selection.pending()).toBeNull());
      expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.initialValue);
      expect(usePrivacyStore.getState().settings[selection.otherField]).toBe(
        selection.otherSelected
      );
      expect(selection.error()).toBeNull();
      expect(usePrivacyStore.getState().error).toBeNull();
    }
  );

  it('a direct same-field successor keeps its confirmed value when the older queued echo arrives', async () => {
    confirmed = { ...confirmed, ...selection.initial };
    await usePrivacyStore.getState().fetchPrivacy();
    const first = deferredResponse();
    let firstEcho: Record<string, unknown> = {};
    patchResponse = (body) => {
      confirmed = { ...confirmed, ...body };
      if (body[selection.wireField] === selection.selected) {
        firstEcho = { ...confirmed };
        return first.promise;
      }
      return HttpResponse.json({ privacy: { ...confirmed } });
    };
    selection.schedule();
    await advance();
    await untilSettled(() => expect(requests).toHaveLength(1));
    await selection.directUpdate();
    expect(confirmed[selection.wireField]).toBe(selection.directValue);
    expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.directValue);
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    first.release(HttpResponse.json({ privacy: firstEcho }));
    await advance();
    await untilSettled(() => expect(selection.pending()).toBeNull());
    expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.directValue);
    expect(selection.error()).toBeNull();
    expect(usePrivacyStore.getState().error).toBeNull();
  });

  it('a direct same-field successor cancels the older choice before its debounce dispatches', async () => {
    selection.schedule();
    expect(selection.pending()).toBe(selection.selected);
    expect(requests).toEqual([]);
    await selection.directUpdate();
    await advance();
    await untilSettled(() => expect(selection.pending()).toBeNull());
    expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.directValue);
    expect(confirmed[selection.wireField]).toBe(selection.directValue);
    expect(requests.map(({ body }) => body)).toEqual([
      { [selection.wireField]: selection.directValue },
    ]);
  });

  it('an expired direct-write context cannot cancel a current queued choice', async () => {
    const oldContext = captureApiRequestContext();
    setRuntimeServerBase(BASE);
    selection.schedule();
    expect(selection.pending()).toBe(selection.selected);
    await expect(selection.directUpdate(oldContext)).rejects.toMatchObject({ name: 'AbortError' });
    expect(selection.pending()).toBe(selection.selected);
    expect(requests).toEqual([]);
    await advance();
    await untilSettled(() => expect(selection.pending()).toBeNull());
    expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.selected);
    expect(confirmed[selection.wireField]).toBe(selection.selected);
    expect(selection.error()).toBeNull();
    expect(usePrivacyStore.getState().error).toBeNull();
    expect(requests.map(({ body }) => body)).toEqual([
      { [selection.wireField]: selection.selected },
    ]);
  });

  it.each(['accepted', 'refused'] as const)(
    'a first-load queued %s response cannot certify placeholder values',
    async (outcome) => {
      usePrivacyStore.getState().clearPrivacy();
      confirmed = {
        ...confirmed,
        ...selection.initial,
        searchable_by_username: true,
        dm_friends_of_friends: false,
        load_gifs_automatically: false,
      };
      if (outcome === 'refused') {
        patchResponse = () =>
          HttpResponse.json({ error: 'First confirmation refused' }, { status: 403 });
      }
      expect(usePrivacyStore.getState().loaded).toBe(false);
      selection.schedule();
      await advance();
      await untilSettled(() => expect(selection.pending()).toBeNull());
      expect(usePrivacyStore.getState().loaded).toBe(outcome === 'accepted');
      if (outcome === 'accepted') {
        expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.selected);
        expect(usePrivacyStore.getState().settings[selection.otherField]).toBe(
          selection.initial[selection.otherWireField]
        );
        expect(usePrivacyStore.getState().settings.searchableByUsername).toBe(true);
        expect(usePrivacyStore.getState().settings.dmFriendsOfFriends).toBe(false);
        expect(usePrivacyStore.getState().settings.loadGifsAutomatically).toBe(false);
        expect(selection.error()).toBeNull();
      } else {
        expect(selection.error()).toBe('First confirmation refused');
        expect(usePrivacyStore.getState().settings.searchableByUsername).toBe(false);
      }
    }
  );
});

describe('privacyStore pending selection ownership', () => {
  it.each([
    { name: 'clearPrivacy', reset: () => usePrivacyStore.getState().clearPrivacy() },
    { name: 'gracefulReset', reset: () => gracefulReset() },
  ])('$name cancels both fields before the auth generation advances', async ({ reset }) => {
    const auth = useAuthStore.getState();
    const generation = auth.authGeneration;
    usePrivacyStore.getState().scheduleDMPrivacy(0);
    usePrivacyStore.getState().scheduleFriendRequestPrivacy('nobody');
    reset();
    expect(useAuthStore.getState().authGeneration).toBe(generation);
    expect(useAuthStore.getState().accessToken).toBe(TOKEN);
    expect(usePrivacyStore.getState().pendingDmPrivacyLevel).toBeNull();
    expect(usePrivacyStore.getState().pendingFriendRequestMode).toBeNull();
    await advance();

    // Same credentials remain usable. A fresh write is the positive control
    // for the HTTP observer; neither cancelled selection may accompany it.
    await usePrivacyStore.getState().fetchPrivacy();
    usePrivacyStore.getState().scheduleDMPrivacy(1);
    await advance();
    await untilSettled(() => expect(usePrivacyStore.getState().settings.dmPrivacyLevel).toBe(1));
    expect(requests.map(({ body }) => body)).toEqual([{ dm_privacy_level: 1 }]);
  });

  it.each([
    {
      name: 'replacement account',
      replace: () =>
        useAuthStore
          .getState()
          .beginAuthLifecycle('test-successor-token', 'test-successor-session'),
    },
    { name: 'replacement server', replace: () => setRuntimeServerBase('http://localhost:9090') },
    { name: 'a new selection of the same server', replace: () => setRuntimeServerBase(BASE) },
  ])('refuses to dispatch a queued selection for $name', async ({ replace }) => {
    usePrivacyStore.getState().scheduleDMPrivacy(0);
    usePrivacyStore.getState().scheduleFriendRequestPrivacy('nobody');
    replace();
    await advance();
    await untilSettled(() => {
      expect(usePrivacyStore.getState().pendingDmPrivacyLevel).toBeNull();
      expect(usePrivacyStore.getState().pendingFriendRequestMode).toBeNull();
    });
    expect(usePrivacyStore.getState().dmSaveError).toBeNull();
    expect(usePrivacyStore.getState().friendRequestError).toBeNull();
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    usePrivacyStore.getState().scheduleFriendRequestPrivacy('mutual_servers');
    await advance();
    await untilSettled(() =>
      expect(usePrivacyStore.getState().settings.allowFriendRequestsFrom).toBe('mutual_servers')
    );
    expect(requests.map(({ body }) => body)).toEqual([
      { allow_friend_requests_from: 'mutual_servers' },
    ]);
  });

  it('sends a queued selection with refreshed credentials in the same auth generation', async () => {
    const generation = useAuthStore.getState().authGeneration;
    usePrivacyStore.getState().scheduleDMPrivacy(0);
    expect(
      useAuthStore
        .getState()
        .rotateAuthCredentials(generation, 'test-rotated-token', 'test-rotated-session')
    ).toBe(true);
    await advance();
    await untilSettled(() => expect(usePrivacyStore.getState().settings.dmPrivacyLevel).toBe(0));
    expect(useAuthStore.getState().authGeneration).toBe(generation);
    expect(requests).toEqual([
      {
        body: { dm_privacy_level: 0 },
        authorization: 'Bearer test-rotated-token',
        url: `${BASE}/api/v1/users/me/privacy`,
      },
    ]);
  });

  it('a server replacement during asynchronous request admission discards the selection silently', async () => {
    let release!: (value: null) => void;
    const admission = new Promise<null>((resolve) => {
      release = resolve;
    });
    const bridge = globalThis.electron;
    expect(bridge).toBeDefined();
    if (!bridge) throw new Error('Test preload bridge missing');
    const original = Object.getOwnPropertyDescriptor(bridge, 'attestation');
    const getToken = vi.fn(() => admission);
    Object.defineProperty(bridge, 'attestation', { value: { getToken }, configurable: true });
    try {
      usePrivacyStore.getState().scheduleDMPrivacy(0);
      await advance();
      await untilSettled(() => expect(getToken).toHaveBeenCalled());
      expect(requests).toEqual([]);
      setRuntimeServerBase('http://localhost:9090');
      release(null);
      await untilSettled(() => expect(usePrivacyStore.getState().pendingDmPrivacyLevel).toBeNull());
      expect(usePrivacyStore.getState().dmSaveError).toBeNull();
      expect(usePrivacyStore.getState().error).toBeNull();
      expect(usePrivacyStore.getState().settings.dmPrivacyLevel).toBe(2);
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      usePrivacyStore.getState().scheduleDMPrivacy(1);
      await advance();
      await untilSettled(() => expect(usePrivacyStore.getState().settings.dmPrivacyLevel).toBe(1));
      expect(requests.map(({ body }) => body)).toEqual([{ dm_privacy_level: 1 }]);
    } finally {
      release(null);
      if (original) Object.defineProperty(bridge, 'attestation', original);
      else Reflect.deleteProperty(bridge, 'attestation');
    }
  });

  it.each(['accepted', 'refused'] as const)(
    'drops an in-flight %s response after clear and a successor selection',
    async (outcome) => {
      const first = deferredResponse();
      patchResponse = () => first.promise;
      const oldEcho = { ...confirmed, dm_privacy_level: 0, allow_friend_requests_from: 'nobody' };
      usePrivacyStore.getState().scheduleDMPrivacy(0);
      await advance();
      await untilSettled(() => expect(requests).toHaveLength(1));
      usePrivacyStore.getState().clearPrivacy();
      useAuthStore.getState().beginAuthLifecycle('test-successor-token', 'test-successor-session');
      patchResponse = null;
      await usePrivacyStore.getState().fetchPrivacy();
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      usePrivacyStore.getState().scheduleDMPrivacy(1);
      first.release(
        outcome === 'accepted'
          ? HttpResponse.json({ privacy: oldEcho })
          : HttpResponse.json({ error: 'Previous account refused' }, { status: 403 })
      );
      await advance();
      await untilSettled(() => expect(usePrivacyStore.getState().settings.dmPrivacyLevel).toBe(1));
      expect(usePrivacyStore.getState().settings.allowFriendRequestsFrom).toBe('everyone');
      expect(usePrivacyStore.getState().dmSaveError).toBeNull();
      expect(usePrivacyStore.getState().error).toBeNull();
      expect(requests.map(({ body }) => body)).toEqual([
        { dm_privacy_level: 0 },
        { dm_privacy_level: 1 },
      ]);
    }
  );

  it.each(['accepted', 'refused'] as const)(
    'drops an in-flight %s response after server replacement without clearing privacy',
    async (outcome) => {
      const first = deferredResponse();
      patchResponse = () => first.promise;
      usePrivacyStore.getState().scheduleFriendRequestPrivacy('nobody');
      await advance();
      await untilSettled(() => expect(requests).toHaveLength(1));
      setRuntimeServerBase('http://localhost:9090');
      first.release(
        outcome === 'accepted'
          ? HttpResponse.json({ privacy: { ...confirmed, allow_friend_requests_from: 'nobody' } })
          : HttpResponse.json({ error: 'Previous server refused' }, { status: 403 })
      );
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      await advance();
      await untilSettled(() =>
        expect(usePrivacyStore.getState().pendingFriendRequestMode).toBeNull()
      );
      expect(usePrivacyStore.getState().settings.allowFriendRequestsFrom).toBe('everyone');
      expect(usePrivacyStore.getState().friendRequestError).toBeNull();
      expect(usePrivacyStore.getState().error).toBeNull();
    }
  );
});

const replacementSelections = [
  {
    name: 'DM',
    field: 'dmPrivacyLevel',
    wireField: 'dm_privacy_level',
    initial: 2,
    first: 0,
    replacement: 1,
    scheduleFirst: () => usePrivacyStore.getState().scheduleDMPrivacy(0),
    scheduleReplacement: () => usePrivacyStore.getState().scheduleDMPrivacy(1),
    scheduleOther: () => usePrivacyStore.getState().scheduleFriendRequestPrivacy('nobody'),
    otherField: 'allowFriendRequestsFrom',
    otherValue: 'nobody',
    scheduleOtherAgain: () =>
      usePrivacyStore.getState().scheduleFriendRequestPrivacy('mutual_servers'),
    otherNextValue: 'mutual_servers',
    pending: () => usePrivacyStore.getState().pendingDmPrivacyLevel,
    error: () => usePrivacyStore.getState().dmSaveError,
    otherError: () => usePrivacyStore.getState().friendRequestError,
  },
  {
    name: 'friend-request',
    field: 'allowFriendRequestsFrom',
    wireField: 'allow_friend_requests_from',
    initial: 'everyone',
    first: 'nobody',
    replacement: 'mutual_servers',
    scheduleFirst: () => usePrivacyStore.getState().scheduleFriendRequestPrivacy('nobody'),
    scheduleReplacement: () =>
      usePrivacyStore.getState().scheduleFriendRequestPrivacy('mutual_servers'),
    scheduleOther: () => usePrivacyStore.getState().scheduleDMPrivacy(0),
    otherField: 'dmPrivacyLevel',
    otherValue: 0,
    scheduleOtherAgain: () => usePrivacyStore.getState().scheduleDMPrivacy(1),
    otherNextValue: 1,
    pending: () => usePrivacyStore.getState().pendingFriendRequestMode,
    error: () => usePrivacyStore.getState().friendRequestError,
    otherError: () => usePrivacyStore.getState().dmSaveError,
  },
] as const;

describe.each(replacementSelections)('$name selection supersession', (selection) => {
  it.each(['accepted', 'refused'] as const)(
    'its replacement during debounce discards the in-flight %s outcome',
    async (outcome) => {
      const first = deferredResponse();
      patchResponse = () => first.promise;
      selection.scheduleFirst();
      await advance();
      await untilSettled(() => expect(requests).toHaveLength(1));
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      selection.scheduleReplacement();
      first.release(
        outcome === 'accepted'
          ? HttpResponse.json({ privacy: { ...confirmed, [selection.wireField]: selection.first } })
          : HttpResponse.json({ error: 'Superseded choice refused' }, { status: 403 })
      );
      await advance(150);
      expect(selection.pending()).toBe(selection.replacement);
      expect(selection.error()).toBeNull();
      expect(usePrivacyStore.getState().error).toBeNull();
      expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.initial);
      patchResponse = null;
      await advance(200);
      await untilSettled(() => expect(selection.pending()).toBeNull());
      expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.replacement);
      expect(selection.error()).toBeNull();
      expect(requests.map(({ body }) => body)).toEqual([
        { [selection.wireField]: selection.first },
        { [selection.wireField]: selection.replacement },
      ]);
    }
  );

  it('preserves its refusal when the other field succeeds', async () => {
    const first = deferredResponse();
    patchResponse = (body) => {
      if (selection.wireField in body) return first.promise;
      confirmed = { ...confirmed, ...body };
      return HttpResponse.json({ privacy: { ...confirmed } });
    };
    selection.scheduleFirst();
    await advance();
    await untilSettled(() => expect(requests).toHaveLength(1));
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    selection.scheduleOther();
    await advance();
    await untilSettled(() =>
      expect(usePrivacyStore.getState().settings[selection.otherField]).toBe(selection.otherValue)
    );
    first.release(HttpResponse.json({ error: 'Current field refused' }, { status: 403 }));
    await untilSettled(() => expect(selection.error()).toBe('Current field refused'));
    expect(selection.pending()).toBeNull();
    expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.initial);
    expect(selection.otherError()).toBeNull();
    expect(usePrivacyStore.getState().settings[selection.otherField]).toBe(selection.otherValue);
    expect(usePrivacyStore.getState().error).toBe('Current field refused');

    // Preserve the original late-refusal ordering above, then exercise the
    // current failure owner's survival across a further disjoint success.
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    selection.scheduleOtherAgain();
    await advance();
    await untilSettled(() =>
      expect(usePrivacyStore.getState().settings[selection.otherField]).toBe(
        selection.otherNextValue
      )
    );
    expect(usePrivacyStore.getState().error).toBe('Current field refused');
    expect(selection.error()).toBe('Current field refused');
  });
});

describe('ordinary privacy field confirmations across a held GET', () => {
  it('an accepted search-visibility write survives the later GET snapshot captured before its commit', async () => {
    const writeResponse = deferredResponse();
    const readResponse = deferredResponse();
    let writeBody: Record<string, unknown> = {};
    let readEcho: Record<string, unknown> | null = null;
    patchResponse = (body) => {
      writeBody = body;
      return writeResponse.promise;
    };
    const write = usePrivacyStore.getState().updatePrivacy({ searchableByUsername: true });
    await untilSettled(() => expect(requests).toHaveLength(1));
    server.use(
      http.get('*/api/v1/users/me/privacy', () => {
        readEcho = { ...confirmed };
        return readResponse.promise;
      })
    );
    const read = usePrivacyStore.getState().fetchPrivacy();
    await untilSettled(() => expect(readEcho).toMatchObject({ searchable_by_username: false }));
    confirmed = { ...confirmed, ...writeBody };
    writeResponse.release(HttpResponse.json({ privacy: { ...confirmed } }));
    await write;
    expect(usePrivacyStore.getState().settings.searchableByUsername).toBe(true);

    readResponse.release(HttpResponse.json({ privacy: readEcho }));
    await read;
    expect(confirmed.searchable_by_username).toBe(true);
    expect(usePrivacyStore.getState().settings.searchableByUsername).toBe(true);
    expect(usePrivacyStore.getState().loaded).toBe(true);
    expect(usePrivacyStore.getState().isLoading).toBe(false);
    expect(usePrivacyStore.getState().error).toBeNull();
  });

  it('a refused search-visibility write permits a first-load GET to confirm server values', async () => {
    usePrivacyStore.getState().clearPrivacy();
    confirmed = { ...confirmed, searchable_by_username: true };
    const writeResponse = deferredResponse();
    const readResponse = deferredResponse();
    let readEcho: Record<string, unknown> | null = null;
    patchResponse = () => writeResponse.promise;
    expect(usePrivacyStore.getState().loaded).toBe(false);
    expect(usePrivacyStore.getState().settings.searchableByUsername).toBe(false);
    const write = usePrivacyStore
      .getState()
      .updatePrivacy({ searchableByUsername: false })
      .catch((error: unknown) => error);
    await untilSettled(() => expect(requests).toHaveLength(1));
    server.use(
      http.get('*/api/v1/users/me/privacy', () => {
        readEcho = { ...confirmed };
        return readResponse.promise;
      })
    );
    const read = usePrivacyStore.getState().fetchPrivacy();
    await untilSettled(() => expect(readEcho).toMatchObject({ searchable_by_username: true }));
    writeResponse.release(
      HttpResponse.json({ error: 'Search visibility refused' }, { status: 403 })
    );
    const refusal = await write;
    expect(refusal).toBeInstanceOf(Error);
    expect(usePrivacyStore.getState().error).toBe('Search visibility refused');
    expect(usePrivacyStore.getState().loaded).toBe(false);

    readResponse.release(HttpResponse.json({ privacy: readEcho }));
    await read;
    expect(usePrivacyStore.getState().loaded).toBe(true);
    expect(usePrivacyStore.getState().isLoading).toBe(false);
    expect(usePrivacyStore.getState().settings.searchableByUsername).toBe(true);
    expect(confirmed.searchable_by_username).toBe(true);
    expect(usePrivacyStore.getState().error).toBe('Search visibility refused');
  });
});

describe('privacy mutation admission during queue-retirement notifications', () => {
  it.each(['accepted', 'refused'] as const)(
    'a newer friend choice from the DM-retirement callback survives the outer %s response',
    async (outcome) => {
      const outerResponse = deferredResponse();
      patchResponse = (body) => {
        if ('dm_privacy_level' in body) {
          if (outcome === 'accepted') confirmed = { ...confirmed, ...body };
          return outerResponse.promise;
        }
        confirmed = { ...confirmed, ...body };
        return HttpResponse.json({ privacy: { ...confirmed } });
      };
      usePrivacyStore.getState().scheduleDMPrivacy(0);
      usePrivacyStore.getState().scheduleFriendRequestPrivacy('nobody');
      let installedSuccessor = false;
      let unsubscribe = () => {};
      unsubscribe = usePrivacyStore.subscribe((state, previous) => {
        if (
          installedSuccessor ||
          previous.pendingDmPrivacyLevel === null ||
          state.pendingDmPrivacyLevel !== null
        )
          return;
        installedSuccessor = true;
        unsubscribe();
        state.scheduleFriendRequestPrivacy('mutual_servers');
      });
      try {
        const outerWrite = usePrivacyStore
          .getState()
          .updatePrivacy({
            dmPrivacyLevel: 1,
            allowFriendRequestsFrom: 'nobody',
          })
          .then(
            () => ({ kind: 'accepted' as const }),
            (error: unknown) => ({ kind: 'refused' as const, error })
          );
        expect(installedSuccessor).toBe(true);
        expect(usePrivacyStore.getState().pendingDmPrivacyLevel).toBeNull();
        expect(usePrivacyStore.getState().pendingFriendRequestMode).toBe('mutual_servers');
        await vi.waitFor(() => expect(requests).toHaveLength(1), { interval: 0 });
        expect(requests[0]?.body).toEqual({
          dm_privacy_level: 1,
          allow_friend_requests_from: 'nobody',
        });
        outerResponse.release(
          outcome === 'accepted'
            ? HttpResponse.json({ privacy: { ...confirmed } })
            : HttpResponse.json({ error: 'Outer direct write refused' }, { status: 403 })
        );
        const result = await outerWrite;
        expect(result.kind).toBe(outcome);
        expect(usePrivacyStore.getState().settings.dmPrivacyLevel).toBe(
          outcome === 'accepted' ? 1 : 2
        );
        expect(usePrivacyStore.getState().settings.allowFriendRequestsFrom).toBe('everyone');
        expect(usePrivacyStore.getState().pendingFriendRequestMode).toBe('mutual_servers');
        expect(usePrivacyStore.getState().friendRequestError).toBeNull();

        // The callback-owned timer remains live. Neither original queued
        // choice may accompany the one request this successor dispatches.
        await advance();
        await vi.waitFor(
          () => expect(usePrivacyStore.getState().pendingFriendRequestMode).toBeNull(),
          { interval: 0 }
        );
        expect(usePrivacyStore.getState().settings.allowFriendRequestsFrom).toBe('mutual_servers');
        expect(confirmed.allow_friend_requests_from).toBe('mutual_servers');
        expect(usePrivacyStore.getState().friendRequestError).toBeNull();
        expect(requests.map(({ body }) => body)).toEqual([
          { dm_privacy_level: 1, allow_friend_requests_from: 'nobody' },
          { allow_friend_requests_from: 'mutual_servers' },
        ]);
      } finally {
        unsubscribe();
      }
    }
  );
});

describe.each(disjointSelections)('$name accepted queue vs ordinary snapshots', (selection) => {
  it.each([
    { ordinaryCommit: 'before', ordinaryResponse: 'first' },
    { ordinaryCommit: 'before', ordinaryResponse: 'last' },
    { ordinaryCommit: 'after', ordinaryResponse: 'first' },
    { ordinaryCommit: 'after', ordinaryResponse: 'last' },
  ] as const)(
    'confirms both writes when the earlier ordinary request commits $ordinaryCommit the queue and responds $ordinaryResponse',
    async ({ ordinaryCommit, ordinaryResponse }) => {
      confirmed = { ...confirmed, ...selection.initial };
      await usePrivacyStore.getState().fetchPrivacy();
      const directResponse = deferredResponse();
      const queuedResponse = deferredResponse();
      let directBody: Record<string, unknown> = {};
      let directEcho: Record<string, unknown> = {};
      let queuedEcho: Record<string, unknown> = {};
      patchResponse = (body) => {
        if ('searchable_by_username' in body) {
          directBody = body;
          if (ordinaryCommit === 'before') {
            confirmed = { ...confirmed, ...body };
            directEcho = { ...confirmed };
          }
          return directResponse.promise;
        }
        confirmed = { ...confirmed, ...body };
        queuedEcho = { ...confirmed };
        return queuedResponse.promise;
      };
      const generation = useAuthStore.getState().authGeneration;
      const origin = captureRuntimeServerSelection();
      expect(usePrivacyStore.getState().settings.searchableByUsername).toBe(false);
      const directWrite = usePrivacyStore.getState().updatePrivacy({ searchableByUsername: true });
      await untilSettled(() => expect(requests).toHaveLength(1));
      expect(confirmed.searchable_by_username).toBe(ordinaryCommit === 'before');
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      selection.schedule();
      await advance();
      await untilSettled(() => expect(confirmed[selection.wireField]).toBe(selection.selected));
      expect(queuedEcho[selection.wireField]).toBe(selection.selected);
      expect(queuedEcho.searchable_by_username).toBe(ordinaryCommit === 'before');
      if (ordinaryCommit === 'after') {
        // The first-issued ordinary request has not committed until now.
        // Its eventual row differs from the queued response's captured DTO.
        confirmed = { ...confirmed, ...directBody };
        directEcho = { ...confirmed };
      }
      if (ordinaryResponse === 'first') {
        directResponse.release(HttpResponse.json({ privacy: directEcho }));
        await directWrite;
        expect(usePrivacyStore.getState().settings.searchableByUsername).toBe(true);
        queuedResponse.release(HttpResponse.json({ privacy: queuedEcho }));
        await untilSettled(() => expect(selection.pending()).toBeNull());
      } else {
        queuedResponse.release(HttpResponse.json({ privacy: queuedEcho }));
        await untilSettled(() => expect(selection.pending()).toBeNull());
        expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.selected);
        directResponse.release(HttpResponse.json({ privacy: directEcho }));
        await directWrite;
      }

      expect(confirmed.searchable_by_username).toBe(true);
      expect(confirmed[selection.wireField]).toBe(selection.selected);
      expect(usePrivacyStore.getState().loaded).toBe(true);
      expect(usePrivacyStore.getState().error).toBeNull();
      expect(selection.error()).toBeNull();
      expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.selected);
      expect(useAuthStore.getState().authGeneration).toBe(generation);
      expect(useAuthStore.getState().accessToken).toBe(TOKEN);
      expect(captureRuntimeServerSelection()).toEqual(origin);
      expect(requests.map(({ body }) => body)).toEqual([
        { searchable_by_username: true },
        { [selection.wireField]: selection.selected },
      ]);
      expect(usePrivacyStore.getState().settings.searchableByUsername).toBe(true);
    }
  );

  it('a queue refusal allows the concurrent first-load GET to confirm the real tier', async () => {
    usePrivacyStore.getState().clearPrivacy();
    confirmed = { ...confirmed, ...selection.initial };
    const queuedResponse = deferredResponse();
    const readResponse = deferredResponse();
    let readEcho: Record<string, unknown> | null = null;
    patchResponse = () => queuedResponse.promise;
    expect(usePrivacyStore.getState().loaded).toBe(false);
    // Distinct placeholder and server values make a false revision bump
    // observable: it would certify the placeholder instead of this GET's row.
    expect(usePrivacyStore.getState().settings[selection.field]).not.toBe(selection.initialValue);
    selection.schedule();
    await advance();
    await untilSettled(() => expect(requests).toHaveLength(1));
    server.use(
      http.get('*/api/v1/users/me/privacy', () => {
        readEcho = { ...confirmed };
        return readResponse.promise;
      })
    );
    const read = usePrivacyStore.getState().fetchPrivacy();
    await untilSettled(() =>
      expect(readEcho).toMatchObject({ [selection.wireField]: selection.initialValue })
    );
    queuedResponse.release(HttpResponse.json({ error: 'Current tier refused' }, { status: 403 }));
    await untilSettled(() => expect(selection.error()).toBe('Current tier refused'));
    expect(selection.pending()).toBeNull();
    expect(usePrivacyStore.getState().loaded).toBe(false);

    readResponse.release(HttpResponse.json({ privacy: readEcho }));
    await read;
    expect(usePrivacyStore.getState().loaded).toBe(true);
    expect(usePrivacyStore.getState().isLoading).toBe(false);
    expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.initialValue);
    expect(selection.error()).toBe('Current tier refused');
    expect(confirmed[selection.wireField]).toBe(selection.initialValue);
  });

  it('retains the accepted tier after a delayed unrelated direct toggle snapshot', async () => {
    confirmed = { ...confirmed, ...selection.initial };
    await usePrivacyStore.getState().fetchPrivacy();
    const queuedResponse = deferredResponse();
    const directResponse = deferredResponse();
    let queuedBody: Record<string, unknown> = {};
    let directEcho: Record<string, unknown> = {};
    patchResponse = (body) => {
      if (selection.wireField in body) {
        queuedBody = body;
        return queuedResponse.promise;
      }
      confirmed = { ...confirmed, ...body };
      directEcho = { ...confirmed };
      return directResponse.promise;
    };
    const generation = useAuthStore.getState().authGeneration;
    const origin = captureRuntimeServerSelection();
    selection.schedule();
    await advance();
    await untilSettled(() => expect(requests).toHaveLength(1));
    const directWrite = usePrivacyStore.getState().updatePrivacy({ allowEmbeddedContent: true });
    await untilSettled(() => expect(confirmed.allow_embedded_content).toBe(true));
    expect(directEcho[selection.wireField]).toBe(selection.initialValue);
    confirmed = { ...confirmed, ...queuedBody };
    queuedResponse.release(HttpResponse.json({ privacy: { ...confirmed } }));
    await untilSettled(() => expect(selection.pending()).toBeNull());
    expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.selected);

    directResponse.release(HttpResponse.json({ privacy: directEcho }));
    await directWrite;
    expect(confirmed.allow_embedded_content).toBe(true);
    expect(confirmed[selection.wireField]).toBe(selection.selected);
    expect(usePrivacyStore.getState().settings.allowEmbeddedContent).toBe(true);
    expect(useAuthStore.getState().authGeneration).toBe(generation);
    expect(useAuthStore.getState().accessToken).toBe(TOKEN);
    expect(captureRuntimeServerSelection()).toEqual(origin);
    expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.selected);
    expect(selection.error()).toBeNull();
    expect(usePrivacyStore.getState().error).toBeNull();
    expect(requests.map(({ body }) => body)).toEqual([
      { [selection.wireField]: selection.selected },
      { allow_embedded_content: true },
    ]);
  });

  it('retains the accepted tier after a delayed GET issued before its server commit', async () => {
    confirmed = { ...confirmed, ...selection.initial };
    await usePrivacyStore.getState().fetchPrivacy();
    const queuedResponse = deferredResponse();
    const readResponse = deferredResponse();
    let queuedBody: Record<string, unknown> = {};
    let readEcho: Record<string, unknown> | null = null;
    patchResponse = (body) => {
      queuedBody = body;
      return queuedResponse.promise;
    };
    const generation = useAuthStore.getState().authGeneration;
    const origin = captureRuntimeServerSelection();
    selection.schedule();
    await advance();
    await untilSettled(() => expect(requests).toHaveLength(1));
    server.use(
      http.get('*/api/v1/users/me/privacy', () => {
        readEcho = { ...confirmed };
        return readResponse.promise;
      })
    );
    const read = usePrivacyStore.getState().fetchPrivacy();
    await untilSettled(() =>
      expect(readEcho).toMatchObject({ [selection.wireField]: selection.initialValue })
    );
    expect(usePrivacyStore.getState().isLoading).toBe(true);
    confirmed = { ...confirmed, ...queuedBody };
    queuedResponse.release(HttpResponse.json({ privacy: { ...confirmed } }));
    await untilSettled(() => expect(selection.pending()).toBeNull());
    expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.selected);

    readResponse.release(HttpResponse.json({ privacy: readEcho }));
    await read;
    expect(confirmed[selection.wireField]).toBe(selection.selected);
    expect(usePrivacyStore.getState().loaded).toBe(true);
    expect(usePrivacyStore.getState().isLoading).toBe(false);
    expect(useAuthStore.getState().authGeneration).toBe(generation);
    expect(useAuthStore.getState().accessToken).toBe(TOKEN);
    expect(captureRuntimeServerSelection()).toEqual(origin);
    expect(usePrivacyStore.getState().settings[selection.field]).toBe(selection.selected);
    expect(selection.error()).toBeNull();
    expect(usePrivacyStore.getState().error).toBeNull();
    expect(requests.map(({ body }) => body)).toEqual([
      { [selection.wireField]: selection.selected },
    ]);
  });
});
