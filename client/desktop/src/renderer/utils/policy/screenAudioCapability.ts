/**
 * Can a given capture target carry audio on this platform?
 *
 * ONE authority, deliberately. The picker and the capture path both need this answer,
 * and when they each computed it inline they drifted: the picker tested only the
 * `screen:` prefix and so offered an enabled, default-on Stream Audio control on Linux,
 * where this capture path has no loopback at all and silently falls back to video.
 *
 * Two independent reasons a target cannot carry audio:
 *
 *   1. It is a window or application. Electron's desktop audio capture is a
 *      whole-system loopback that ignores `chromeMediaSourceId`, so a window share
 *      asking for audio sends every application's sound to the channel (#2161).
 *      Per-application audio needs a native addon — see ADR-0043.
 *   2. The platform has no loopback. Linux is the known case.
 *
 * An UNKNOWN platform resolves to allowed rather than refused, and that is not a
 * fail-open: it is the dev/web path, which reaches `getDisplayMedia` — OS-mediated
 * consent for one user-chosen surface — not the whole-desktop loopback #2161 is about.
 * The capture path still gates on the prefix regardless of what this returns.
 */
export function canCarryScreenAudio(
  sourceId: string | null | undefined,
  platform: string | null | undefined
): boolean {
  if (!sourceId) return false;
  // Prefix allowlist, not a window denylist: an id shape we do not recognise must not
  // fall through to "audio is fine". Fail closed on the unknown.
  if (!sourceId.startsWith('screen:')) return false;
  if (platform === 'linux') return false;
  return true;
}
