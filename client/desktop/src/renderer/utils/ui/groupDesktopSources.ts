/**
 * Group Electron desktop-capture sources for the share picker (R4).
 *
 * Electron's `DesktopCapturerSource` carries only `id`, `name`, `appIcon`,
 * `display_id` and `thumbnail` — there is no application name and no PID. A window's
 * `name` is its WINDOW TITLE, not its app. So "group these windows by application" is
 * necessarily a heuristic, and this module is where that heuristic lives and is tested.
 *
 * Key preference, strongest first:
 *   1. `appIcon` data-URL identity — two windows of one app render the same icon bytes.
 *      Requires `fetchWindowIcons: true` at the getSources call site.
 *   2. The trailing `" - App"` / `" — App"` title segment, when the platform supplies one.
 *   3. The whole title, which yields a single-window group.
 *
 * Rung 3 is a graceful degradation, not a failure: the picker renders a single-window
 * group as a direct share target, so an ungroupable window stays perfectly usable.
 *
 * Pure — no store, React or Electron imports — so it tests without a DOM.
 */

export interface DesktopSourceLike {
  id: string;
  name: string;
  thumbnail: string;
  appIcon: string | null;
}

export interface AppGroup {
  /**
   * The identity this group was built on -- NOT `appName`, which is derived
   * separately and is not unique. Two groups legitimately share a display name:
   * one window reporting an appIcon keys on `icon:<data-url>` while a sibling
   * window of the same app reporting no icon keys on `app:Chrome`, and both
   * render "Chrome". Keying React off the name collides them.
   */
  groupKey: string;
  /** Best available display name for the owning application. */
  appName: string;
  /** First non-null appIcon among the group's windows, or null. */
  appIcon: string | null;
  windows: DesktopSourceLike[];
}

export interface GroupedSources {
  /** `screen:` sources, in the order the platform reported them. */
  screens: DesktopSourceLike[];
  /** `window:` sources grouped by owning application, sorted by name. */
  apps: AppGroup[];
  /** Every `window:` source, flat, for the Windows tab. */
  windows: DesktopSourceLike[];
}

/**
 * Trailing " - App" / " — App" / " – App" segment of a window title, if present.
 *
 * Requires whitespace on BOTH sides of the separator so a hyphenated title
 * ("well-known.txt") is not mistaken for a separator. Takes the LAST separator,
 * because the app name is conventionally the final segment.
 */
function trailingAppSegment(title: string): string | null {
  const match = /^.*\s[-–—]\s(.+)$/.exec(title);
  // `|| null` rather than `?? null`: a title ending in the separator yields an EMPTY
  // string, which is not a usable app name and must fall through like a missing match.
  return match?.[1]?.trim() || null;
}

function groupKeyFor(source: DesktopSourceLike): string {
  // Namespaced so an icon data-URL can never collide with a title string.
  if (source.appIcon) return `icon:${source.appIcon}`;
  const segment = trailingAppSegment(source.name);
  if (segment) return `app:${segment}`;
  return `title:${source.name}`;
}

function displayNameFor(source: DesktopSourceLike): string {
  return trailingAppSegment(source.name) ?? source.name;
}

export function groupDesktopSources(sources: DesktopSourceLike[]): GroupedSources {
  const screens: DesktopSourceLike[] = [];
  const windows: DesktopSourceLike[] = [];

  for (const source of sources) {
    if (source.id.startsWith('screen:')) screens.push(source);
    else if (source.id.startsWith('window:')) windows.push(source);
    // Any other id prefix is a shape we do not understand; dropping it is safer
    // than guessing which tab it belongs in.
  }

  const byKey = new Map<string, AppGroup>();
  for (const window of windows) {
    const key = groupKeyFor(window);
    const existing = byKey.get(key);
    if (existing) {
      existing.windows.push(window);
      existing.appIcon ??= window.appIcon;
    } else {
      byKey.set(key, {
        groupKey: key,
        appName: displayNameFor(window),
        appIcon: window.appIcon,
        windows: [window],
      });
    }
  }

  // Sorted so the Applications tab does not reshuffle between openings of the picker.
  const apps = [...byKey.values()].sort((a, b) =>
    a.appName.localeCompare(b.appName, undefined, { sensitivity: 'base' })
  );

  return { screens, apps, windows };
}
