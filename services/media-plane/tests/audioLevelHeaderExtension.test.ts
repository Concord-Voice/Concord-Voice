import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import * as mediasoup from 'mediasoup';
import type { Worker, Router } from 'mediasoup/types';
import { config } from '@/config/index.js';

// This test intentionally uses the REAL mediasoup module (no vi.mock). The rest
// of the media-plane suite mocks mediasoup with headerExtensions: [], so only a
// real worker+router can prove that Concord's router actually advertises the
// RFC 6464 audio-level header extension. Gates audio last-N (#1544 / S5b). #1543.
const SSRC_AUDIO_LEVEL_URI = 'urn:ietf:params:rtp-hdrext:ssrc-audio-level';

describe('RFC 6464 audio-level header extension negotiation (#1543)', () => {
  // Layer 1 — pure, no subprocess: the installed mediasoup supports the extension.
  it('mediasoup library supports ssrc-audio-level for audio', () => {
    const caps = mediasoup.getSupportedRtpCapabilities();
    const ext = caps.headerExtensions?.find(
      (h) => h.uri === SSRC_AUDIO_LEVEL_URI && h.kind === 'audio'
    );
    expect(ext, 'mediasoup must support ssrc-audio-level for audio').toBeDefined();
  });

  // Layer 2 — decisive regression lock: CONCORD'S router advertises it.
  describe("Concord's router advertises ssrc-audio-level", () => {
    let worker: Worker;
    let router: Router;

    beforeAll(async () => {
      worker = await mediasoup.createWorker({
        logLevel: config.mediasoup.worker.logLevel,
        logTags: config.mediasoup.worker.logTags,
      });
      router = await worker.createRouter({
        mediaCodecs: config.mediasoup.router.mediaCodecs,
      });
    });

    afterAll(() => {
      // Reap the worker subprocess so the runner doesn't hang.
      worker?.close();
    });

    it('router.rtpCapabilities advertises a recv-capable audio-level extension', () => {
      const ext = router.rtpCapabilities.headerExtensions?.find(
        (h) => h.uri === SSRC_AUDIO_LEVEL_URI && h.kind === 'audio'
      );
      expect(
        ext,
        "Concord's mediasoup router must advertise ssrc-audio-level for audio " +
          '(required by AudioLevelObserver under E2EE — gates audio last-N #1544)'
      ).toBeDefined();
      // The SFU must be able to RECEIVE the header from producers.
      expect(['sendrecv', 'recvonly']).toContain(ext?.direction);
    });
  });
});
