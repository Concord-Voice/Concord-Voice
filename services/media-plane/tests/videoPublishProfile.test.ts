import { describe, expect, it } from 'vitest';
import {
  requireCompatibleVideoPublishProfile,
  VIDEO_BITRATE_PROFILE_VERSION,
  VideoClientUpdateRequiredError,
} from '../src/lib/videoPublishProfile.js';

describe('video publish rollout gate', () => {
  it.each(['camera', 'screen'])('accepts an upgraded %s publisher', (source) => {
    expect(() =>
      requireCompatibleVideoPublishProfile('video', {
        source,
        videoBitrateProfileVersion: VIDEO_BITRATE_PROFILE_VERSION,
      })
    ).not.toThrow();
  });

  it.each([undefined, {}, { videoBitrateProfileVersion: 0 }, { videoBitrateProfileVersion: 2 }])(
    'refuses a missing or unsupported video profile before publication: %s',
    (appData) => {
      expect(() => requireCompatibleVideoPublishProfile('video', appData)).toThrow(
        VideoClientUpdateRequiredError
      );
    }
  );

  it.each([undefined, {}, { source: 'mic' }, { source: 'screen-audio' }])(
    'keeps audio publishing available without a video profile: %s',
    (appData) => {
      expect(() => requireCompatibleVideoPublishProfile('audio', appData)).not.toThrow();
    }
  );
});
