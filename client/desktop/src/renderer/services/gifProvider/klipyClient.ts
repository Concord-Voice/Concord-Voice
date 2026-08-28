/**
 * KLIPY low-level HTTP client.
 *
 * This is the ONLY file that knows about KLIPY's URL shapes, query params, and
 * response field names. All KLIPY API calls (search, trending, customer-id) are
 * routed through the Concord control-plane proxy at `/api/v1/klipy/...` — the
 * renderer never speaks directly to api.klipy.com, so the app key is never
 * embedded in the client bundle and API request metadata does not expose
 * per-user IPs.
 *
 * GIF media URLs are rewritten through the same proxy via `rewriteMediaUrl`
 * before being used as <img>/<video> src attributes, so KLIPY's CDN also never
 * sees per-user IPs. All KLIPY traffic is fully proxied.
 *
 * ## customer_id behaviour
 *
 * KLIPY requires a `customer_id` on trending/search requests. The value is an
 * opaque string we generate — KLIPY never sees PII.
 *
 * **Personalization ON** (this is the DEFAULT — `privacy_settings.
 * share_personalization_with_gif_provider` is `BOOLEAN NOT NULL DEFAULT TRUE`,
 * migration 000056, and `privacyStore` initialises it to `true`. This heading
 * previously read "default: off", which was wrong from the day it shipped):
 *   A stable UUID is generated server-side via `POST /api/v1/klipy/customer-id`,
 *   persisted in `localStorage`, and reused across sessions. This lets KLIPY
 *   personalise results and power the Recent tab. The user can manually rotate
 *   it at any time via `rotateCustomerId()`.
 *
 * **Personalization OFF:**
 *   A fresh ephemeral UUID is generated in-memory (never written to
 *   `localStorage`) and used for all requests until the app restarts or the ID
 *   auto-rotates (every 30 minutes). This satisfies KLIPY's required field
 *   while preventing any persistent cross-session tracking profile.
 *
 * NOTE: Response shapes are written against the documented KLIPY API contract
 * (https://docs.klipy.com) where available. Categories is CONFIRMED against
 * docs.klipy.com/gifs-api/gifs-categories-api (2026-08-27): `data.categories[]`
 * of `{ category, query, preview_url }`. The recent-items response body
 * remains UNOBSERVED — the indexed docs give no body for
 * `GET /gifs-recent-items-api-per-user` — so the defensive `data ?? result`
 * envelope handling in `unwrapList` (klipyProvider.ts) stays in place for
 * that endpoint and other still-unconfirmed shapes.
 */

import { apiFetch } from '../apiClient';
import {
  apiUrl,
  captureRuntimeServerSelection,
  runtimeServerSelectionIsCurrent,
  type RuntimeServerSelection,
} from '../runtimeServerBase';

const KLIPY_PROXY_BASE = '/api/v1/klipy';
const CUSTOMER_ID_STORAGE_KEY = 'concord:klipy-customer-id';

/** How long to cache a customer-id failure before retrying (personalization ON). */
const CUSTOMER_ID_FAILURE_TTL_MS = 60_000;

/** How often to rotate the ephemeral ID when personalization is OFF. */
const EPHEMERAL_ROTATION_INTERVAL_MS = 30 * 60 * 1_000; // 30 minutes

/** KLIPY/CDN host suffix patterns that are eligible for media proxying. */
const KLIPY_MEDIA_HOSTS = ['.klipy.com', '.klipy.io', 'klipy.com', 'klipy.io'];

/**
 * Rewrite a KLIPY CDN media URL through the control-plane media proxy so that
 * the client's IP address is never sent to KLIPY's CDN. Non-KLIPY URLs are
 * returned unchanged as a safe fallback.
 *
 * The control-plane endpoint validates the host against an allowlist before
 * fetching, so passing an arbitrary URL will result in a 400, not an SSRF.
 *
 * Returns an absolute URL (prefixed with the active runtime API base) so it resolves against the
 * API host when used as <img src> / <video src>, rather than the renderer
 * origin (which is `app://concord/` in bundled mode after #830 and would
 * dead-end at the asar protocol handler with ERR_UNEXPECTED).
 */
export function rewriteMediaUrl(url: string | undefined): string | undefined {
  if (!url) return url;
  try {
    const parsed = new URL(url);
    const host = parsed.hostname;
    const isKlipy = KLIPY_MEDIA_HOSTS.some(
      (suffix) => host === suffix || host.endsWith('.' + suffix.replace(/^\./, ''))
    );
    if (!isKlipy) return url;
    return apiUrl(`${KLIPY_PROXY_BASE}/media?url=${encodeURIComponent(url)}`);
  } catch {
    return url;
  }
}

/** Maximum accepted GIF slug length. Mirrors `MaxSlugLength` in
 *  `services/control-plane/internal/klipy/validate.go`. */
const MAX_GIF_SLUG_LENGTH = 100;

/** Nonempty run of unreserved path characters. Excludes `/`, `\`, `.`, `?`,
 *  `#`, whitespace, and every Unicode separator lookalike (U+2044, U+FF0F). */
const GIF_SLUG_PATTERN = /^[A-Za-z0-9-]+$/;

/**
 * Is this vendor-supplied slug safe to interpolate into a request path?
 *
 * KLIPY slugs reach us from an untrusted third party and are interpolated into
 * credentialed request paths. WHATWG URL normalisation collapses dot segments
 * BEFORE the request leaves the renderer, so a poisoned slug retargets a
 * different Concord route and the server-side guard never runs. That is why
 * this exists and why it is not merely defence-in-depth (#2580).
 *
 * DELIBERATELY STRICTER than `ValidateSlug` in
 * `services/control-plane/internal/klipy/validate.go`, which returns TRUE for
 * the empty string because it answers a different question ("is this optional
 * attached-slug field well-formed?"). Here the question is "may I interpolate
 * this into a path?" and the empty string answers no. Do not "fix" the
 * divergence — reconciling them reopens #2371 A2 behind a green test suite.
 */
/**
 * Classify a rejected share so operators can aggregate on the reason.
 *
 * Three classes, not two: 400 is a bug on our side, 429 is backpressure we
 * deliberately do not retry (the share is simply absent from Recent), and
 * everything else is vendor-side. A named function rather than a nested
 * ternary — typescript:S3358, and it reads better besides.
 */
function shareFailureClass(status: number): 'client-bug' | 'backpressure' | 'upstream' {
  if (status === 400) return 'client-bug';
  if (status === 429) return 'backpressure';
  return 'upstream';
}

export function isValidGifSlug(slug: string): boolean {
  if (typeof slug !== 'string' || slug.length === 0) return false;
  if (slug.length > MAX_GIF_SLUG_LENGTH) return false;
  return GIF_SLUG_PATTERN.test(slug);
}

/**
 * Is this customer id safe to interpolate into a request path?
 *
 * Same grammar as a slug, deliberately: the control-plane mints these as UUIDs
 * and already applies `ValidateSlug` to the `:customerID` route param, so any
 * legitimate value is already `[A-Za-z0-9-]{1,100}`. Aliased rather than
 * duplicated so the two can never drift.
 *
 * This exists because the stored id is NOT scoped per server. A hostile
 * self-hosted instance can mint one, it persists in localStorage, and it is
 * then reused against the official API with the official bearer token — so it
 * is an untrusted value reaching a credentialed path, exactly the class #2580
 * closed for slugs.
 */
export const isValidCustomerId = isValidGifSlug;

/** A single rendition variant in a KLIPY response (one format at one quality). */
export interface KlipyRendition {
  url?: string;
  width?: number;
  height?: number;
  size?: number;
}

/** A bundle of format variants at one quality tier (hd / md / sm). */
export interface KlipyQualityBundle {
  mp4?: KlipyRendition;
  gif?: KlipyRendition;
  webp?: KlipyRendition;
  jpg?: KlipyRendition;
}

/** Raw KLIPY GIF item shape. KLIPY's actual API nests format variants under a
 *  quality tier (hd / md / sm), e.g. `file.hd.gif.url`. We also keep the legacy
 *  flat shape (`file.gif.url`) as a fallback in case the contract changes. */
export interface KlipyGifItem {
  slug: string;
  id?: number | string;
  title?: string;
  url?: string;
  width?: number;
  height?: number;
  file?: KlipyQualityBundle & {
    hd?: KlipyQualityBundle;
    md?: KlipyQualityBundle;
    sm?: KlipyQualityBundle;
  };
  // Sometimes KLIPY returns format variants nested under a different key.
  // We handle both shapes defensively in klipyProvider.ts.
  preview?: KlipyQualityBundle;
  still?: { url?: string };
}

/**
 * KLIPY wraps every response in `{ result: boolean, data: <payload> }`. The
 * inner payload for list endpoints is itself `{ data: KlipyGifItem[], has_more
 * }`. We model both legacy flat shapes and the current nested envelope so the
 * parser stays defensive against future API tweaks.
 */
export interface KlipyListPayload {
  data?: KlipyGifItem[];
  result?: KlipyGifItem[];
  has_more?: boolean;
  meta?: { has_more?: boolean };
}

export interface KlipyListResponse {
  result?: boolean | KlipyGifItem[];
  data?: KlipyGifItem[] | KlipyListPayload;
  has_more?: boolean;
  meta?: { has_more?: boolean };
}

/** One category from GET /gifs/categories.
 *  Confirmed against docs.klipy.com/gifs-api/gifs-categories-api on 2026-08-27:
 *  the array lives at `data.categories`, the display name is `category` (not
 *  `name`), and `preview_url` is a BARE URL STRING (not a rendition object).
 *  The previous shape here was a guess and was wrong on all three counts. */
export interface KlipyCategoryItem {
  category: string;
  query: string;
  preview_url: string;
}

export interface KlipyCategoriesResponse {
  result?: boolean;
  data?: {
    locale?: string;
    categories?: KlipyCategoryItem[];
  };
}

/** Response shape from POST /api/v1/klipy/customer-id (control-plane endpoint). */
export interface KlipyCustomerIDResponse {
  customer_id?: string;
}

/** Thrown when personalization is ON but the stable customer id cannot be
 *  resolved. Distinct from "you have no recents": the picker must be able to
 *  tell an identity failure from a legitimate empty list, or an operator
 *  verifying the recents fix cannot tell whether it worked. */
export class KlipyIdentityError extends Error {
  constructor() {
    super('KLIPY: could not resolve a customer id for the recent list');
    this.name = 'KlipyIdentityError';
  }
}

class KlipyClient {
  private personalizationEnabled = false;

  // ── Personalization ON: stable UUID persisted to localStorage ──
  private cachedCustomerId: string | null = null;
  /** Epoch ms of the most recent customer-id fetch failure. */
  private customerIdFailureAt: number | null = null;
  /** Single in-flight customer-id promise so concurrent callers share one request. */
  private customerIdInFlight: Promise<string | null> | null = null;
  /** The server selection the in-flight request was started under. */
  private customerIdInFlightSelection: RuntimeServerSelection | null = null;

  // ── Personalization OFF: ephemeral UUID rotated every 30 minutes ──
  private ephemeralCustomerId: string | null = null;
  private ephemeralRotationTimer: ReturnType<typeof setTimeout> | null = null;

  setPersonalizationEnabled(enabled: boolean): void {
    const changed = this.personalizationEnabled !== enabled;
    this.personalizationEnabled = enabled;
    if (changed) {
      if (enabled) {
        // Switching ON: stop ephemeral rotation timer, clear ephemeral state.
        this._clearEphemeralTimer();
        this.ephemeralCustomerId = null;
      } else {
        // Switching OFF: clear persistent ID from memory (not localStorage — the
        // user may re-enable later and should get their previous stable ID back).
        // Generate a fresh ephemeral ID immediately.
        this.cachedCustomerId = null;
        this.customerIdInFlight = null;
        this.customerIdInFlightSelection = null;
        this.customerIdFailureAt = null;
        this._refreshEphemeral();
      }
    }
  }

  /** The current customer_id shown in settings UI (stable when on, ephemeral when off). */
  getCurrentCustomerId(): string | null {
    if (this.personalizationEnabled) {
      return this.cachedCustomerId ?? localStorage.getItem(CUSTOMER_ID_STORAGE_KEY);
    }
    return this.ephemeralCustomerId;
  }

  /**
   * Manually rotate the customer_id. When personalization is ON, generates a
   * new stable UUID via the control-plane and persists it. When OFF, generates
   * a new ephemeral UUID and resets the 30-minute auto-rotation timer.
   * Returns the new ID.
   */
  async rotateCustomerId(): Promise<string> {
    if (this.personalizationEnabled) {
      // Clear cached ID and force a fresh fetch from control-plane.
      this.cachedCustomerId = null;
      this.customerIdFailureAt = null;
      this.customerIdInFlight = null;
      this.customerIdInFlightSelection = null;
      localStorage.removeItem(CUSTOMER_ID_STORAGE_KEY);
      // Kick off the fetch immediately and return the new ID. The selection is
      // captured here for the same reason _getStableId captures one: the guard
      // inside _fetchStableId must be keyed to the server this rotation was
      // requested on, not to whatever is current when it resolves.
      const id = await this._fetchStableId(captureRuntimeServerSelection());
      return id ?? crypto.randomUUID();
    } else {
      return this._refreshEphemeral();
    }
  }

  /** Reset all internal state. Used by tests; not called from production code. */
  _resetForTesting(): void {
    this.personalizationEnabled = false;
    this.cachedCustomerId = null;
    this.customerIdFailureAt = null;
    this.customerIdInFlight = null;
    this.customerIdInFlightSelection = null;
    this._clearEphemeralTimer();
    this.ephemeralCustomerId = null;
  }

  /** Generate a fresh ephemeral ID, reset the 30-minute rotation timer, return the new ID. */
  private _refreshEphemeral(): string {
    this._clearEphemeralTimer();
    const id = crypto.randomUUID();
    this.ephemeralCustomerId = id;
    this.ephemeralRotationTimer = setTimeout(() => {
      if (!this.personalizationEnabled) {
        this._refreshEphemeral();
      }
    }, EPHEMERAL_ROTATION_INTERVAL_MS);
    return id;
  }

  private _clearEphemeralTimer(): void {
    if (this.ephemeralRotationTimer !== null) {
      clearTimeout(this.ephemeralRotationTimer);
      this.ephemeralRotationTimer = null;
    }
  }

  /**
   * All KLIPY requests go through the authenticated control-plane proxy.
   *
   * `authoritative: false` (#1957): a 401 from the third-party GIF proxy is NOT
   * proof the Concord session is dead, so it must never tear down auth. A GIF
   * that fails to load surfaces as a load error (getBySlug → null → embed error
   * placeholder), never a logout. Genuine session expiry is still caught by the
   * next real Concord API call, which is authoritative.
   */
  private async doFetch(path: string, init?: RequestInit): Promise<Response> {
    return apiFetch(`${KLIPY_PROXY_BASE}${path}`, init, { authoritative: false });
  }

  /** Append the customer_id query param. Always included (required by KLIPY).
   *  When personalization is off the value is the current ephemeral UUID. */
  private withCustomerID(params: URLSearchParams, customerId: string | null): URLSearchParams {
    if (customerId) {
      params.set('customer_id', customerId);
    }
    return params;
  }

  /**
   * Resolve the customer_id to use for this request.
   * - Personalization ON:  returns the stable persisted UUID (fetching it from
   *   the control-plane on first use).
   * - Personalization OFF: returns the current ephemeral UUID, generating one
   *   if not yet initialised.
   */
  async getCustomerID(): Promise<string | null> {
    if (this.personalizationEnabled) {
      return this._getStableId();
    }
    // Ephemeral path — no network call needed.
    if (!this.ephemeralCustomerId) {
      this._refreshEphemeral();
    }
    return this.ephemeralCustomerId;
  }

  /** Lazily fetch and cache the stable per-device customer_id from the control-plane. */
  private async _getStableId(): Promise<string | null> {
    if (this.cachedCustomerId) return this.cachedCustomerId;

    const stored = localStorage.getItem(CUSTOMER_ID_STORAGE_KEY);
    if (stored && !isValidCustomerId(stored)) {
      // Pre-existing poison self-heals rather than persisting forever: drop it
      // and fall through to mint a fresh id.
      console.warn('[gifProvider] customer-id: discarding an ill-formed stored id');
      localStorage.removeItem(CUSTOMER_ID_STORAGE_KEY);
    } else if (stored) {
      this.cachedCustomerId = stored;
      return stored;
    }

    // Short-circuit while a recent failure is still cached.
    if (
      this.customerIdFailureAt !== null &&
      Date.now() - this.customerIdFailureAt < CUSTOMER_ID_FAILURE_TTL_MS
    ) {
      return null;
    }

    // The handle is shared so concurrent callers make ONE request, but it is
    // only shareable WITHIN the selection it started under. Once the
    // stale-selection guard makes a request begun on the previous server
    // resolve null by design, a caller on the new server that joins it
    // inherits that null — recent() reports an identity error and shares go
    // unattributed against a server whose endpoint is perfectly healthy.
    if (
      this.customerIdInFlight &&
      this.customerIdInFlightSelection &&
      runtimeServerSelectionIsCurrent(this.customerIdInFlightSelection)
    ) {
      return this.customerIdInFlight;
    }

    const selection = captureRuntimeServerSelection();
    const pending = this._fetchStableId(selection).finally(() => {
      // Clear only OUR handle. A successor started under a newer selection
      // must not be torn down by its predecessor settling afterwards.
      if (this.customerIdInFlight === pending) {
        this.customerIdInFlight = null;
        this.customerIdInFlightSelection = null;
      }
    });
    this.customerIdInFlight = pending;
    this.customerIdInFlightSelection = selection;
    return pending;
  }

  private async _fetchStableId(selection: RuntimeServerSelection): Promise<string | null> {
    // frontend.md: async provider flows MUST fence every continuation on the
    // runtime-server selection. Comparing the URL alone is not ABA-safe, which
    // is why the selection carries an epoch. It is passed in rather than
    // captured here so the caller can key the shared in-flight handle on the
    // SAME selection this method fences against.
    // The FAILURE state is fenced for the same reason as the success state, and
    // it is the easier one to overlook: a 60-second backoff recorded against a
    // server the user has already left is inherited by the one they joined, so
    // recent() throws KlipyIdentityError there even though its endpoint is
    // healthy. Every failure write in this method goes through here.
    const markFailure = (): void => {
      if (runtimeServerSelectionIsCurrent(selection)) {
        this.customerIdFailureAt = Date.now();
      }
    };
    try {
      const res = await this.doFetch('/customer-id', { method: 'POST' });
      if (!res.ok) {
        markFailure();
        return null;
      }
      const data = (await res.json()) as KlipyCustomerIDResponse;
      const id = data.customer_id ?? null;
      if (id && !isValidCustomerId(id)) {
        // A server-supplied id that cannot be safely interpolated is treated as
        // a failure, not cached, and never written to localStorage.
        console.warn('[gifProvider] customer-id: server returned an ill-formed id');
        markFailure();
        return null;
      }
      if (id) {
        if (!runtimeServerSelectionIsCurrent(selection)) {
          // The user selected a different runtime server while this request was
          // in flight. Caching and persisting now would carry the value minted
          // by the previous server into the new one's session — a state check
          // made BEFORE an await says nothing about the world after it.
          return null;
        }
        this.cachedCustomerId = id;
        this.customerIdFailureAt = null;
        localStorage.setItem(CUSTOMER_ID_STORAGE_KEY, id);
      } else {
        markFailure();
      }
      return id;
    } catch {
      markFailure();
      return null;
    }
  }

  async trending(page: number, perPage: number, locale?: string): Promise<KlipyListResponse> {
    const customerId = await this.getCustomerID();
    const params = this.withCustomerID(
      new URLSearchParams({
        page: String(page),
        per_page: String(perPage),
      }),
      customerId
    );
    if (locale) params.set('locale', locale);
    params.set('format_filter', 'mp4,gif,webp');

    const res = await this.doFetch(`/gifs/trending?${params.toString()}`);
    if (!res.ok) throw new Error(`KLIPY trending failed: ${res.status}`);
    return (await res.json()) as KlipyListResponse;
  }

  async search(
    q: string,
    page: number,
    perPage: number,
    locale?: string
  ): Promise<KlipyListResponse> {
    const customerId = await this.getCustomerID();
    const params = this.withCustomerID(
      new URLSearchParams({
        q,
        page: String(page),
        per_page: String(perPage),
      }),
      customerId
    );
    if (locale) params.set('locale', locale);
    params.set('format_filter', 'mp4,gif,webp');

    const res = await this.doFetch(`/gifs/search?${params.toString()}`);
    if (!res.ok) throw new Error(`KLIPY search failed: ${res.status}`);
    return (await res.json()) as KlipyListResponse;
  }

  /**
   * Fetch the personalized recent-shares list. Throws `KlipyIdentityError`
   * when personalization is ON but the customer id cannot be resolved — a
   * fabricated `{ data: [], has_more: false }` success there would be
   * indistinguishable from "you have no recents" and would mask the A2 fix
   * during verification.
   */
  async recent(
    page: number,
    perPage: number,
    opts?: { force?: boolean }
  ): Promise<KlipyListResponse> {
    // Personalization OFF is a legitimate empty — the tab is hidden in that
    // state and there is no ledger to read.
    if (!this.personalizationEnabled) {
      return { data: [], has_more: false };
    }

    // An explicit user gesture is precisely the signal that should reset a
    // client-side backoff heuristic. `force` clears ONLY the customer-id
    // failure short-circuit; it never bypasses rate limiting.
    if (opts?.force) {
      this.customerIdFailureAt = null;
    }

    const customerId = await this.getCustomerID();
    // Re-read the flag AFTER the await, as the original implementation did.
    // Opting out while /customer-id is in flight must not send the stable
    // identifier to the vendor; this is a legitimate empty, not an error.
    if (!this.personalizationEnabled) {
      return { data: [], has_more: false };
    }
    if (!customerId) {
      throw new KlipyIdentityError();
    }

    const params = new URLSearchParams({
      page: String(page),
      per_page: String(perPage),
    });
    // encodeURIComponent as defence in depth: the value is validated above, and
    // this keeps a future edit that reintroduces an unvalidated id from
    // re-opening the path-interpolation class.
    const res = await this.doFetch(
      `/gifs/recent/${encodeURIComponent(customerId)}?${params.toString()}`
    );
    if (!res.ok) throw new Error(`KLIPY recent failed: ${res.status}`);
    return (await res.json()) as KlipyListResponse;
  }

  async categories(locale?: string): Promise<KlipyCategoriesResponse> {
    const params = new URLSearchParams();
    if (locale) params.set('locale', locale);
    const qs = params.toString();
    const path = qs ? `/gifs/categories?${qs}` : '/gifs/categories';
    const res = await this.doFetch(path);
    if (!res.ok) throw new Error(`KLIPY categories failed: ${res.status}`);
    return (await res.json()) as KlipyCategoriesResponse;
  }

  async getBySlug(slug: string): Promise<KlipyGifItem | null> {
    const customerId = await this.getCustomerID();
    const params = this.withCustomerID(new URLSearchParams({ slugs: slug }), customerId);
    params.set('format_filter', 'mp4,gif,webp');
    const res = await this.doFetch(`/gifs/items?${params.toString()}`);
    if (!res.ok) return null;
    const data = (await res.json()) as KlipyListResponse;
    let items: KlipyGifItem[] = [];
    if (data.data && !Array.isArray(data.data)) {
      items = data.data.data ?? data.data.result ?? [];
    } else if (Array.isArray(data.data)) {
      items = data.data;
    } else if (Array.isArray(data.result)) {
      items = data.result;
    }
    return items[0] ?? null;
  }

  /**
   * Fire KLIPY's share trigger. This is what POPULATES the per-customer recent
   * ledger read by `/gifs/recent/{customer_id}` — without `customer_id` here,
   * the Recent tab is structurally empty forever (#2371 A2).
   *
   * NEVER REJECTS. It is fire-and-forget from the composer and must not gate
   * the send flow (#2580). But "never rejects" is not "never reports": every
   * failure shape emits an observable signal, because a silent share failure
   * means a permanently empty Recent tab with no way to tell.
   *
   * No retry: KLIPY does not document share-trigger idempotency, so a retry
   * could double-count analytics. One share event per send.
   */
  async notifyShared(slug: string, ctx?: { q?: string }): Promise<void> {
    // Validate FIRST, before any await. An implementation that resolves the
    // customer id first issues one network request on a rejected slug and
    // fails the "zero requests" criterion (#2580 AC9).
    if (!isValidGifSlug(slug)) {
      console.warn('[gifProvider] share: refused an invalid slug');
      return;
    }

    try {
      // customer_id only under personalization ON. With it off the client uses
      // an ephemeral UUID rotating every 30 minutes; attaching it to share
      // would create a PERSISTENT upstream write keyed to an id we promise is
      // ephemeral, where today only transient reads carry it. The Recent tab
      // is hidden in that mode, so the ledger entry has no user-visible
      // purpose. Share still fires — just unattributed.
      // DEFENSIVE AND UNVERIFIED BY TEST — see the note in klipyClient.test.ts.
      // The recent() equivalent is falsified and covers the same class; this
      // one could not be staged, because disabling personalization clears the
      // in-flight handle and the continuation resolves null either way.
      //
      // Re-read the flag AFTER the await. A check made before it does not hold
      // after it: the user can disable personalization while /customer-id is
      // still in flight, and attaching the resolved id then would create a
      // PERSISTENT upstream ledger entry keyed to a user who has just opted
      // out. Share still fires — just unattributed.
      // Fence the whole continuation, not just the identifier. Rejecting a
      // stale id still leaves the SHARE itself — the slug, and the search term
      // in ctx.q — being POSTed to whichever server is current after the await.
      // A user who switches instances mid-share would hand the query they typed
      // on server A to server B.
      const selection = captureRuntimeServerSelection();
      let customerId: string | null = null;
      if (this.personalizationEnabled) {
        const resolved = await this.getCustomerID();
        if (this.personalizationEnabled) customerId = resolved;
      }
      if (!runtimeServerSelectionIsCurrent(selection)) return;

      const params = new URLSearchParams();
      if (customerId) params.set('customer_id', customerId);
      // `q` already reaches KLIPY via /gifs/search?q=, so forwarding it here
      // leaks nothing new. handlers.go's promise is about LOGGING, not
      // forwarding, and is unaffected.
      if (ctx?.q) params.set('q', ctx.q);

      const qs = params.toString();
      const path = qs ? `/gifs/share/${slug}?${qs}` : `/gifs/share/${slug}`;
      const res = await this.doFetch(path, { method: 'POST' });

      if (!res.ok) {
        // Never log the slug.
        console.warn('[gifProvider] share: upstream rejected the trigger', {
          status: res.status,
          failureClass: shareFailureClass(res.status),
        });
      }
    } catch {
      console.warn('[gifProvider] share: transport error reaching the share trigger');
    }
  }

  /** Report inappropriate content. User-initiated, so a rejected slug throws
   *  rather than failing silently — the caller needs to know it did nothing. */
  async report(slug: string): Promise<void> {
    // Pre-await: a rejected slug must issue ZERO requests, including zero
    // customer-id requests (#2580 AC9).
    if (!isValidGifSlug(slug)) {
      throw new Error('KLIPY report: refusing to send an invalid slug');
    }
    await this.doFetch(`/gifs/report/${slug}`, { method: 'POST' });
  }
}

// Singleton — the entire renderer talks to one KlipyClient instance.
export const klipyClient = new KlipyClient();
