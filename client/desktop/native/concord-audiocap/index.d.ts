/**
 * concord-audiocap — per-process screen-share audio capture.
 * See [internal]0043-per-process-screen-share-audio-capture.md.
 */

export interface AudioCapCapability {
  /** 'win32' | 'darwin' | 'linux' | 'unsupported' */
  platform: string;
  /** Windows: the build number. macOS: kern.osproductversion, e.g. "14.4.1". */
  osVersion: string;
  /**
   * Whether per-process audio capture is available on THIS machine.
   *
   * A false value means the capability ladder's bottom rung applies: share video
   * only, and say so in the UI. It never means "fall back to a system mix" — a
   * window target must never obtain one (ADR-0043 D6, #2161).
   */
  perProcessAudio: boolean;
  /** Empty when supported; otherwise why not, in words fit to show a user. */
  reason: string;
}

export function capability(): AudioCapCapability;
