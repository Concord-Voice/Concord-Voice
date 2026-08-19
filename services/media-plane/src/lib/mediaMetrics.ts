export interface MetricsSample {
  publishers: { camera: number; screen: number };
  activeByKind: { audio: number; webcam: number; screenshare: number };
  /**
   * Per-recv-transport cumulative bytesSent (SFU->client egress), keyed by transport id.
   * ONLY contains transports whose getStats() succeeded this tick.
   */
  egressBytesByTransport: Map<string, number>;
  /**
   * ALL live recv-transport ids this tick, regardless of getStats() success. Used to prune
   * the accumulator's last-seen state ONLY for transports that genuinely left room state —
   * a transient getStats() failure must NOT prune (else the transport is re-counted from 0
   * on its next successful tick, double-counting egress).
   */
  liveTransportIds: Set<string>;
  /** Active video-publisher count per room (for the per-room peak). */
  perRoomVideoPublishers: number[];
}

export interface MetricsSnapshot {
  concurrentVideoPublishers: { camera: number; screen: number };
  peakConcurrentVideoPublishersPerRoom: number;
  participantHoursByKind: { audio: number; webcam: number; screenshare: number };
  egress: { cumulativeBytes: number; currentBps: number; peakBps: number };
  /**
   * Handshakes refused by the pre-upgrade admission gate (#2032). A bare count:
   * no address, no user, no room — the gate's onReject hook is given nothing
   * else to pass (`observability.md` #2, and /health is aggregate-only).
   * Deliberately NOT on AggregateMediaMetrics: that feeds the ops-metrics
   * publisher, whose metric-key catalog is closed (ADR-0030).
   */
  admissionRejected: number;
}

export interface AggregateMediaMetrics {
  readonly cameraPublishersCurrent: number;
  readonly screenPublishersCurrent: number;
  readonly peakVideoPublishersPerRoom: number;
  readonly egressCurrentBps: number;
  readonly egressPeakBps: number;
  readonly egressCumulativeBytes: number;
  readonly participantHoursAudio: number;
  readonly participantHoursWebcam: number;
  readonly participantHoursScreenshare: number;
}

/**
 * Pure accumulator for media-plane measurement counters (#1553). Fed by exactly one
 * active sampler: the room heartbeat when ops metrics is disabled, or the ops publisher
 * when enabled. No mediasoup/I/O — see
 * [internal]specs/2026-06-16-1553-media-plane-metrics-design.md.
 */
export class MediaMetrics {
  private participantSeconds = { audio: 0, webcam: 0, screenshare: 0 };
  private latestPublishers = { camera: 0, screen: 0 };
  private peakPerRoom = 0;
  private readonly lastSeenBytes = new Map<string, number>();
  private cumulativeBytes = 0;
  private currentBps = 0;
  private peakBps = 0;
  private admissionRejected = 0;

  /**
   * Count one admission-gate rejection (#2032). Not fed by `ingest` because it
   * is event-driven rather than sampled — the gate calls it from the pre-upgrade
   * path, where no room state exists yet.
   */
  incrementAdmissionRejected(): void {
    this.admissionRejected += 1;
  }

  ingest(sample: MetricsSample, tickSeconds: number): void {
    this.participantSeconds.audio += sample.activeByKind.audio * tickSeconds;
    this.participantSeconds.webcam += sample.activeByKind.webcam * tickSeconds;
    this.participantSeconds.screenshare += sample.activeByKind.screenshare * tickSeconds;

    this.latestPublishers = { ...sample.publishers };
    for (const n of sample.perRoomVideoPublishers) {
      if (n > this.peakPerRoom) this.peakPerRoom = n;
    }

    let deltaSum = 0;
    for (const [id, bytes] of sample.egressBytesByTransport) {
      const delta = Math.max(0, bytes - (this.lastSeenBytes.get(id) ?? 0));
      deltaSum += delta;
      this.lastSeenBytes.set(id, bytes);
    }
    // Prune ONLY transports that are gone from room state (absent from liveTransportIds).
    // A transport that is still live but whose getStats() failed this tick is retained, so
    // its last-seen byte count carries forward and the next successful tick computes a real
    // delta instead of re-counting from zero.
    for (const id of this.lastSeenBytes.keys()) {
      if (!sample.liveTransportIds.has(id)) this.lastSeenBytes.delete(id);
    }
    this.cumulativeBytes += deltaSum;
    this.currentBps = (deltaSum * 8) / tickSeconds;
    if (this.currentBps > this.peakBps) this.peakBps = this.currentBps;
  }

  getSnapshot(): MetricsSnapshot {
    return {
      concurrentVideoPublishers: { ...this.latestPublishers },
      peakConcurrentVideoPublishersPerRoom: this.peakPerRoom,
      participantHoursByKind: {
        audio: this.participantSeconds.audio / 3600,
        webcam: this.participantSeconds.webcam / 3600,
        screenshare: this.participantSeconds.screenshare / 3600,
      },
      egress: {
        cumulativeBytes: this.cumulativeBytes,
        currentBps: this.currentBps,
        peakBps: this.peakBps,
      },
      admissionRejected: this.admissionRejected,
    };
  }

  getAggregateMetrics(): AggregateMediaMetrics {
    const snapshot = this.getSnapshot();
    return {
      cameraPublishersCurrent: snapshot.concurrentVideoPublishers.camera,
      screenPublishersCurrent: snapshot.concurrentVideoPublishers.screen,
      peakVideoPublishersPerRoom: snapshot.peakConcurrentVideoPublishersPerRoom,
      egressCurrentBps: snapshot.egress.currentBps,
      egressPeakBps: snapshot.egress.peakBps,
      egressCumulativeBytes: snapshot.egress.cumulativeBytes,
      participantHoursAudio: snapshot.participantHoursByKind.audio,
      participantHoursWebcam: snapshot.participantHoursByKind.webcam,
      participantHoursScreenshare: snapshot.participantHoursByKind.screenshare,
    };
  }

  reset(): void {
    this.participantSeconds = { audio: 0, webcam: 0, screenshare: 0 };
    this.latestPublishers = { camera: 0, screen: 0 };
    this.peakPerRoom = 0;
    this.lastSeenBytes.clear();
    this.cumulativeBytes = 0;
    this.currentBps = 0;
    this.peakBps = 0;
    this.admissionRejected = 0;
  }
}
