/**
 * Split Electron desktop-capture sources for the share picker (R4).
 *
 * Electron's `DesktopCapturerSource` carries only `id`, `name`, `appIcon`,
 * `display_id` and `thumbnail` — there is no application name and no PID. A
 * window's `name` is its WINDOW TITLE, not its app.
 *
 * This module used to additionally group windows into guessed APPLICATIONS,
 * keyed on `appIcon` identity with a trailing `" - App"` title segment as a
 * fallback. That grouping is gone, and deliberately so.
 *
 * The picker now shows one flat grid of windows, so nothing consumes a group.
 * But the heuristic was also wrong in a way worth recording, because it reads
 * as sound: it took the LAST `" - "`-separated segment, on the reasonable
 * grounds that an app name is conventionally final. A Terminal titles itself
 * `"<project> - -zsh - 183x62"`, so the last segment is the window's
 * DIMENSIONS — which became a visible heading AND a grouping key, silently
 * collapsing unrelated projects' terminals into one phantom application.
 *
 * Do not reintroduce grouping by title. `appIcon` identity alone was sound and
 * could come back on its own if a future picker needs it; the title fallback is
 * what was unsafe. A test pins the absence of `apps` so this cannot return by
 * accident.
 *
 * The name still says "group" because splitting by kind — screens versus
 * windows — is the grouping that remains.
 *
 * Pure — no store, React or Electron imports — so it tests without a DOM.
 */

export interface DesktopSourceLike {
  id: string;
  name: string;
  thumbnail: string;
  /** Per-window icon, still rendered on the individual card. */
  appIcon: string | null;
}

export interface GroupedSources {
  /** `screen:` sources, in the order the platform reported them. */
  screens: DesktopSourceLike[];
  /** `window:` sources, flat, in the order the platform reported them. */
  windows: DesktopSourceLike[];
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

  return { screens, windows };
}
