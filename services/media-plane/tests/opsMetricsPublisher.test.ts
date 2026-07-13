import { createHmac } from 'node:crypto';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import './mocks/logger.js';
import {
  OPS_METRICS_MEDIA_SUBJECT,
  OpsMetricsPublisher,
} from '../src/lib/opsMetricsPublisher.js';
import { canonicalEnvelopePayload } from '../src/lib/opsMetricsCatalog.js';
import { logger } from '../src/lib/logger.js';

const secret = '0123456789abcdef0123456789abcdef'; // pragma: allowlist secret
const observedAt = new Date('2026-07-12T20:00:00.000Z');

function createDependencies() {
  const sample = {
    publishers: { camera: 3, screen: 1 },
    activeByKind: { audio: 7, webcam: 3, screenshare: 1 },
    egressBytesByTransport: new Map<string, number>(),
    liveTransportIds: new Set<string>(),
    perRoomVideoPublishers: [4],
  };
  return {
    sample,
    natsService: {
      publish: vi.fn(() => true),
    },
    roomManager: {
      collectMetricsSample: vi.fn(async () => sample),
      getAggregateCounts: vi.fn(() => ({
        activeRooms: 2,
        audioParticipants: 7,
        webcamParticipants: 3,
        screenshareParticipants: 1,
      })),
    },
    mediaMetrics: {
      ingest: vi.fn(),
      getAggregateMetrics: vi.fn(() => ({
        cameraPublishersCurrent: 3,
        screenPublishersCurrent: 1,
        peakVideoPublishersPerRoom: 4,
        egressCurrentBps: 8_000,
        egressPeakBps: 12_000,
        egressCumulativeBytes: 50_000,
        participantHoursAudio: 12.5,
        participantHoursWebcam: 4.25,
        participantHoursScreenshare: 1.5,
      })),
    },
  };
}

function createPublisher(
  dependencies = createDependencies(),
  overrides: Partial<ConstructorParameters<typeof OpsMetricsPublisher>[0]> = {}
) {
  return {
    dependencies,
    publisher: new OpsMetricsPublisher({
      enabled: true,
      nodeId: 'cvn_aaaaaaaaaaaaaaaa',
      secret,
      intervalMs: 1_000,
      now: () => observedAt,
      ...dependencies,
      ...overrides,
    }),
  };
}

describe('OpsMetricsPublisher', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.clearAllMocks();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('builds a closed scalar-only snapshot from explicit aggregate fields', () => {
    const { publisher } = createPublisher();

    const snapshot = publisher.snapshot();

    expect(snapshot).toEqual({
      media_rooms_current: 2,
      media_participants_audio_current: 7,
      media_participants_webcam_current: 3,
      media_participants_screenshare_current: 1,
      media_camera_publishers_current: 3,
      media_screen_publishers_current: 1,
      media_peak_video_publishers_per_room: 4,
      media_egress_current_bps: 8_000,
      media_egress_peak_bps: 12_000,
      media_egress_cumulative_bytes: 50_000,
      media_participant_hours_audio: 12.5,
      media_participant_hours_webcam: 4.25,
      media_participant_hours_screenshare: 1.5,
    });
    expect(Object.values(snapshot).every((value) => typeof value === 'number')).toBe(true);
  });

  it('publishes a signed closed envelope to the fixed subject on each interval', async () => {
    const { publisher, dependencies } = createPublisher();

    publisher.start();
    await vi.advanceTimersByTimeAsync(999);
    expect(dependencies.natsService.publish).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    await vi.advanceTimersByTimeAsync(1_000);

    expect(dependencies.natsService.publish).toHaveBeenCalledTimes(2);
    expect(dependencies.natsService.publish.mock.calls.map((call) => call[0])).toEqual([
      OPS_METRICS_MEDIA_SUBJECT,
      OPS_METRICS_MEDIA_SUBJECT,
    ]);

    const firstEnvelope = dependencies.natsService.publish.mock.calls[0][1];
    const { signature, ...unsigned } = firstEnvelope;
    expect(Object.keys(firstEnvelope).sort()).toEqual([
      'metrics',
      'node_id',
      'observed_at',
      'sequence',
      'signature',
      'source',
      'version',
    ]);
    expect(firstEnvelope).toMatchObject({
      version: 1,
      source: 'media',
      node_id: 'cvn_aaaaaaaaaaaaaaaa',
      observed_at: observedAt.toISOString(),
      sequence: 1,
      metrics: publisher.snapshot(),
    });
    expect(signature).toBe(
      createHmac('sha256', secret)
        .update(canonicalEnvelopePayload(unsigned))
        .digest('hex')
    );
    expect(dependencies.natsService.publish.mock.calls[1][1]).toMatchObject({ sequence: 2 });
  });

  it('samples current media state immediately before publishing each snapshot', async () => {
    const { publisher, dependencies } = createPublisher();

    publisher.start();
    await vi.advanceTimersByTimeAsync(1_000);

    expect(dependencies.roomManager.collectMetricsSample).toHaveBeenCalledTimes(1);
    expect(dependencies.mediaMetrics.ingest).toHaveBeenCalledWith(dependencies.sample, 1);
    expect(dependencies.mediaMetrics.ingest.mock.invocationCallOrder[0]).toBeLessThan(
      dependencies.mediaMetrics.getAggregateMetrics.mock.invocationCallOrder[0]
    );
    expect(dependencies.mediaMetrics.getAggregateMetrics.mock.invocationCallOrder[0]).toBeLessThan(
      dependencies.natsService.publish.mock.invocationCallOrder[0]
    );
  });

  it('skips an overlapping interval while a publish is still in flight', async () => {
    let finishPublish: (() => void) | undefined;
    const dependencies = createDependencies();
    dependencies.natsService.publish.mockImplementation(
      () =>
        new Promise<boolean>((resolve) => {
          finishPublish = () => resolve(true);
        })
    );
    const { publisher } = createPublisher(dependencies);

    publisher.start();
    vi.advanceTimersByTime(1_000);
    await Promise.resolve();
    vi.advanceTimersByTime(1_000);

    expect(dependencies.natsService.publish).toHaveBeenCalledTimes(1);
    expect(logger.warn).toHaveBeenCalledWith('Operations metrics snapshot dropped', {
      reason: 'collection_in_progress',
    });

    finishPublish?.();
    await Promise.resolve();
    await Promise.resolve();
    await vi.advanceTimersByTimeAsync(1_000);
    expect(dependencies.natsService.publish).toHaveBeenCalledTimes(2);
  });

  it('stays dormant when disabled and does not require signing configuration', async () => {
    const dependencies = createDependencies();
    const { publisher } = createPublisher(dependencies, {
      enabled: false,
      nodeId: '',
      secret: '',
    });

    publisher.start();
    await vi.advanceTimersByTimeAsync(5_000);

    expect(dependencies.natsService.publish).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('rejects an enabled publisher with a missing signing secret', () => {
    expect(() => createPublisher(createDependencies(), { secret: '' })).toThrow(/secret/i);
  });

  it('drops publish failures without leaking the error or payload and continues', async () => {
    const dependencies = createDependencies();
    dependencies.natsService.publish.mockReturnValueOnce(false).mockReturnValueOnce(true);
    const { publisher } = createPublisher(dependencies);

    publisher.start();
    await vi.advanceTimersByTimeAsync(1_000);
    await vi.advanceTimersByTimeAsync(1_000);

    expect(dependencies.natsService.publish).toHaveBeenCalledTimes(2);
    expect(logger.warn).toHaveBeenCalledWith('Operations metrics snapshot dropped', {
      reason: 'publish_failed',
    });
    expect(JSON.stringify(vi.mocked(logger.warn).mock.calls)).not.toMatch(
      /private-room-id|signed-payload|signature/
    );
  });

  it('clears its interval on stop', async () => {
    const { publisher, dependencies } = createPublisher();
    publisher.start();
    publisher.start();
    await vi.advanceTimersByTimeAsync(1_000);

    await publisher.stop();
    await publisher.stop();
    await vi.advanceTimersByTimeAsync(5_000);

    expect(dependencies.natsService.publish).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('waits for an in-flight publication during stop', async () => {
    let finishPublish: (() => void) | undefined;
    const dependencies = createDependencies();
    dependencies.natsService.publish.mockImplementation(
      () =>
        new Promise<boolean>((resolve) => {
          finishPublish = () => resolve(true);
        })
    );
    const { publisher } = createPublisher(dependencies);

    publisher.start();
    vi.advanceTimersByTime(1_000);
    await Promise.resolve();

    let stopped = false;
    const stopping = publisher.stop().then(() => {
      stopped = true;
    });
    await Promise.resolve();
    expect(stopped).toBe(false);

    finishPublish?.();
    await stopping;
    expect(stopped).toBe(true);
  });
});
