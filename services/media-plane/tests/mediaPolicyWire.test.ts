import { describe, expect, it } from 'vitest';
import './mocks/logger.js';

import {
  MEDIA_POLICY_PAUSED_RESUME_ACK,
  mediaPolicyCooldownAck,
} from '../src/lib/mediaPolicyWire.js';
import { MediaPolicyCooldownError } from '../src/lib/roomManager.js';

describe('media-policy wire helpers (#2153)', () => {
  it('maps a cooldown refusal to the join/produce ack', () => {
    expect(mediaPolicyCooldownAck(new MediaPolicyCooldownError(412))).toEqual({
      error: 'Media policy cooldown',
      code: 'media_policy_cooldown',
      retryAfterSec: 412,
    });
  });

  it('pins the resume refusal ack and freezes it', () => {
    expect(MEDIA_POLICY_PAUSED_RESUME_ACK).toEqual({
      error: 'media_policy_paused',
      message: 'Paused by media policy',
    });
    expect(Object.isFrozen(MEDIA_POLICY_PAUSED_RESUME_ACK)).toBe(true);
  });
});
