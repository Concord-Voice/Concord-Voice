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
 * **Personalization ON (default: off):**
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
 * (https://docs.klipy.com). The defensive `data ?? result` envelope handling
 * accounts for both shapes the docs and demos show.
 */

import { apiFetch } from '../apiClient';
import { API_BASE } from '@/renderer/config';

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
 * Returns an absolute URL (prefixed with API_BASE) so it resolves against the
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
    return `${API_BASE}${KLIPY_PROXY_BASE}/media?url=${encodeURIComponent(url)}`;
  } catch {
    return url;
  }
}

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

export interface KlipyCategoryItem {
  name: string;
  preview?: KlipyGifItem;
}

export interface KlipyCategoriesPayload {
  data?: KlipyCategoryItem[];
  result?: KlipyCategoryItem[];
}

export interface KlipyCategoriesResponse {
  result?: boolean | KlipyCategoryItem[];
  data?: KlipyCategoryItem[] | KlipyCategoriesPayload;
}

/** Response shape from POST /api/v1/klipy/customer-id (control-plane endpoint). */
export interface KlipyCustomerIDResponse {
  customer_id?: string;
}

class KlipyClient {
  private personalizationEnabled = false;

  // ── Personalization ON: stable UUID persisted to localStorage ──
  private cachedCustomerId: string | null = null;
  /** Epoch ms of the most recent customer-id fetch failure. */
  private customerIdFailureAt: number | null = null;
  /** Single in-flight customer-id promise so concurrent callers share one request. */
  private customerIdInFlight: Promise<string | null> | null = null;

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
      localStorage.removeItem(CUSTOMER_ID_STORAGE_KEY);
      // Kick off the fetch immediately and return the new ID.
      const id = await this._fetchStableId();
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

  /** All KLIPY requests go through the authenticated control-plane proxy. */
  private async doFetch(path: string, init?: RequestInit): Promise<Response> {
    return apiFetch(`${KLIPY_PROXY_BASE}${path}`, init);
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
    if (stored) {
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

    if (this.customerIdInFlight) {
      return this.customerIdInFlight;
    }

    this.customerIdInFlight = this._fetchStableId();
    return this.customerIdInFlight;
  }

  private async _fetchStableId(): Promise<string | null> {
    try {
      const res = await this.doFetch('/customer-id', { method: 'POST' });
      if (!res.ok) {
        this.customerIdFailureAt = Date.now();
        return null;
      }
      const data = (await res.json()) as KlipyCustomerIDResponse;
      const id = data.customer_id ?? null;
      if (id) {
        this.cachedCustomerId = id;
        this.customerIdFailureAt = null;
        localStorage.setItem(CUSTOMER_ID_STORAGE_KEY, id);
      } else {
        this.customerIdFailureAt = Date.now();
      }
      return id;
    } catch {
      this.customerIdFailureAt = Date.now();
      return null;
    } finally {
      this.customerIdInFlight = null;
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

  async recent(page: number, perPage: number): Promise<KlipyListResponse> {
    const customerId = await this.getCustomerID();
    // Recent requires a stable customer_id — only available when personalization is on.
    if (!this.personalizationEnabled || !customerId) {
      return { data: [], has_more: false };
    }
    const params = new URLSearchParams({
      page: String(page),
      per_page: String(perPage),
    });
    const res = await this.doFetch(`/gifs/recent/${customerId}?${params.toString()}`);
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

  async notifyShared(slug: string): Promise<void> {
    try {
      await this.doFetch(`/gifs/share/${slug}`, { method: 'POST' });
    } catch {
      // Best-effort — never block on share-trigger failures
    }
  }

  async report(slug: string): Promise<void> {
    await this.doFetch(`/gifs/report/${slug}`, { method: 'POST' });
  }
}

// Singleton — the entire renderer talks to one KlipyClient instance.
export const klipyClient = new KlipyClient();
