/**
 * Vendor-agnostic GIF provider types.
 *
 * The picker, the chat embed, and the message composer all import from this
 * module — they should never reference vendor-specific types from
 * `klipyClient.ts` or `klipyProvider.ts`. The active provider is selected in
 * `services/gifProvider/index.ts` and can be swapped in one place if we ever
 * need to migrate vendors.
 *
 * This abstraction also enforces KLIPY's ToS Section 1 ("no mixing providers in
 * the same view") at the architectural level: the picker pulls from a single
 * `GifProvider` instance, so a multi-provider grid is impossible without first
 * adding new public surface area to this module.
 */

/** Vendor-agnostic GIF reference. The `slug` is opaque — only the active
 *  provider knows how to resolve it to renderable URLs. */
export interface GifRef {
  slug: string;
  /** Width and height hints, when the provider returns them. Used to size
   *  embeds before they load (prevents layout shift). */
  width?: number;
  height?: number;
}

/** A fully resolved GIF ready to render in a `<video>` or `<img>`.
 *  URLs are vendor-specific and MUST NOT be modified by callers. */
export interface GifResolved extends GifRef {
  /** The animated rendition URL. Prefer MP4 when the provider has one. */
  animatedUrl: string;
  /** Whether `animatedUrl` is a video (MP4/WEBM) or an image (GIF/WEBP).
   *  The renderer uses this to choose between `<video>` and `<img>`. */
  animatedKind: 'video' | 'image';
  /** Static still frame for reduce-motion mode and click-to-load placeholders. */
  stillUrl: string;
  /** Provider-specific metadata for analytics, attribution, etc. Opaque. */
  meta?: Record<string, unknown>;
}

export interface GifSearchResult {
  items: GifResolved[];
  hasMore: boolean;
  /** Cursor or page index to pass back to the next call. */
  nextOffset?: number;
}

export interface GifSearchOpts {
  q: string;
  offset: number;
  limit: number;
  locale?: string;
}

export interface GifTrendingOpts {
  offset: number;
  limit: number;
  locale?: string;
}

export interface GifRecentOpts {
  offset: number;
  limit: number;
}

export interface GifCategoryOpts {
  locale?: string;
}

export interface GifCategory {
  name: string;
  preview: GifResolved;
}

/** The contract every GIF vendor adapter must implement. */
export interface GifProvider {
  /** Display name for attribution: "KLIPY", "GIPHY", etc. */
  readonly name: string;
  /** Required search input placeholder text per the vendor's brand guidelines. */
  readonly searchPlaceholder: string;
  /** "Powered by X" attribution text. */
  readonly poweredByText: string;
  /** Optional vendor logo asset paths for the picker footer. Provide both
   *  variants when the brand kit ships separate light/dark marks so the
   *  attribution stays legible against either chrome theme. The picker reads
   *  the active theme from the settings store and chooses accordingly. */
  readonly logoAssetLight?: string;
  readonly logoAssetDark?: string;
  /** Optional independence disclaimer text required by the vendor's ToS. */
  readonly independenceDisclaimer?: string;
  /** Whether the provider supports a server-tracked "Recent" tab.
   *  When false, the picker hides the Recent tab entirely. */
  readonly supportsRecent: boolean;
  /** Whether the provider supports a categories browse view. */
  readonly supportsCategories: boolean;

  trending(opts: GifTrendingOpts): Promise<GifSearchResult>;
  search(opts: GifSearchOpts): Promise<GifSearchResult>;
  recent(opts: GifRecentOpts): Promise<GifSearchResult>;
  categories(opts: GifCategoryOpts): Promise<GifCategory[]>;
  /** Fetch a single GIF by slug. Used by the Saved tab when rendering bookmarks
   *  and by `GifEmbed` to render a GIF in a chat message. */
  getBySlug(slug: string): Promise<GifResolved>;
  /** Fire vendor-specific share/send tracking. May be a no-op for some providers. */
  notifyShared?(slug: string): Promise<void>;
  /** Report inappropriate content to the vendor. */
  report?(slug: string): Promise<void>;
  /** Set whether to send the personalization customer_id to the vendor. */
  setPersonalizationEnabled(enabled: boolean): void;
}
