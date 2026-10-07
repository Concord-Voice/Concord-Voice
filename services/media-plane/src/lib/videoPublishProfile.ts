/**
 * A rollout compatibility marker for desktop video encodings whose combined
 * simulcast bitrate is capped. It is client-declared, so it is not an
 * entitlement or an enforcement signal; the media policer still measures the
 * bytes that actually arrive.
 */
export const VIDEO_BITRATE_PROFILE_VERSION = 1 as const;

export class VideoClientUpdateRequiredError extends Error {
  readonly code = 'video_client_update_required';

  constructor() {
    super('Update Concord Voice to share your camera or screen.');
    this.name = 'VideoClientUpdateRequiredError';
  }
}

/** Refuse older video publishers before the SFU creates a producer. */
export function requireCompatibleVideoPublishProfile(kind: unknown, appData: unknown): void {
  if (kind !== 'video') return;
  if (
    typeof appData !== 'object' ||
    appData === null ||
    !('videoBitrateProfileVersion' in appData) ||
    appData.videoBitrateProfileVersion !== VIDEO_BITRATE_PROFILE_VERSION
  ) {
    throw new VideoClientUpdateRequiredError();
  }
}
