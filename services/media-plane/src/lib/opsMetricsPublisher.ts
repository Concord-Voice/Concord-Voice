import type { MediaMetrics as MediaMetricsAccumulator } from './mediaMetrics.js';
import type { RoomManager } from './roomManager.js';
import { logger } from './logger.js';
import {
  buildSignedEnvelope,
  type MediaMetricKey,
  type OpsMetricsEnvelope,
} from './opsMetricsCatalog.js';

export const OPS_METRICS_MEDIA_SUBJECT = 'ops.metrics.media.v1';

export type MediaOpsMetricsSnapshot = Readonly<Record<MediaMetricKey, number>>;

interface OpsMetricsNatsPublisher {
  publish(subject: string, data: Record<string, unknown>): boolean | Promise<boolean>;
}

export interface OpsMetricsPublisherOptions {
  readonly enabled: boolean;
  readonly nodeId: string;
  readonly secret: string;
  readonly intervalMs: number;
  readonly natsService: OpsMetricsNatsPublisher;
  readonly roomManager: Pick<RoomManager, 'collectMetricsSample' | 'getAggregateCounts'>;
  readonly mediaMetrics: Pick<MediaMetricsAccumulator, 'ingest' | 'getAggregateMetrics'>;
  readonly now?: () => Date;
}

export class OpsMetricsPublisher {
  private readonly now: () => Date;
  private timer: ReturnType<typeof setInterval> | undefined;
  private sequence = 0;
  private inFlight: Promise<void> | undefined;

  constructor(private readonly options: OpsMetricsPublisherOptions) {
    this.now = options.now ?? (() => new Date());
    if (options.enabled && Buffer.byteLength(options.secret, 'utf8') < 32) {
      throw new Error('Snapshot signing secret must be at least 32 bytes');
    }
  }

  snapshot(): MediaOpsMetricsSnapshot {
    const roomCounts = this.options.roomManager.getAggregateCounts();
    const mediaMetrics = this.options.mediaMetrics.getAggregateMetrics();
    return {
      media_rooms_current: roomCounts.activeRooms,
      media_participants_audio_current: roomCounts.audioParticipants,
      media_participants_webcam_current: roomCounts.webcamParticipants,
      media_participants_screenshare_current: roomCounts.screenshareParticipants,
      media_camera_publishers_current: mediaMetrics.cameraPublishersCurrent,
      media_screen_publishers_current: mediaMetrics.screenPublishersCurrent,
      media_peak_video_publishers_per_room: mediaMetrics.peakVideoPublishersPerRoom,
      media_egress_current_bps: mediaMetrics.egressCurrentBps,
      media_egress_peak_bps: mediaMetrics.egressPeakBps,
      media_egress_cumulative_bytes: mediaMetrics.egressCumulativeBytes,
      media_participant_hours_audio: mediaMetrics.participantHoursAudio,
      media_participant_hours_webcam: mediaMetrics.participantHoursWebcam,
      media_participant_hours_screenshare: mediaMetrics.participantHoursScreenshare,
    };
  }

  start(): void {
    if (!this.options.enabled || this.timer !== undefined) return;
    this.timer = setInterval(() => {
      this.tick();
    }, this.options.intervalMs);
  }

  async stop(): Promise<void> {
    if (this.timer !== undefined) {
      clearInterval(this.timer);
      this.timer = undefined;
    }
    await this.inFlight;
  }

  private tick(): void {
    if (this.inFlight !== undefined) {
      this.logDrop('collection_in_progress');
      return;
    }
    const operation = this.publishSnapshot();
    this.inFlight = operation;
    void operation.finally(() => {
      if (this.inFlight === operation) this.inFlight = undefined;
    });
  }

  private async publishSnapshot(): Promise<void> {
    try {
      const sample = await this.options.roomManager.collectMetricsSample();
      this.options.mediaMetrics.ingest(sample, this.options.intervalMs / 1_000);
    } catch {
      this.logDrop('collection_failed');
      return;
    }

    let envelope: OpsMetricsEnvelope;
    try {
      const sequence = this.sequence + 1;
      envelope = buildSignedEnvelope({
        nodeId: this.options.nodeId,
        observedAt: this.now(),
        sequence,
        secret: this.options.secret,
        metrics: this.snapshot(),
      });
      this.sequence = sequence;
    } catch {
      this.logDrop('snapshot_invalid');
      return;
    }

    try {
      const published = await this.options.natsService.publish(OPS_METRICS_MEDIA_SUBJECT, {
        ...envelope,
      });
      if (!published) this.logDrop('publish_failed');
    } catch {
      this.logDrop('publish_failed');
    }
  }

  private logDrop(
    reason: 'collection_in_progress' | 'collection_failed' | 'snapshot_invalid' | 'publish_failed'
  ): void {
    logger.warn('Operations metrics snapshot dropped', { reason });
  }
}
