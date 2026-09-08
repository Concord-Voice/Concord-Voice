import { connect, JSONCodec } from 'nats';
import type { NatsConnection } from 'nats';
import { config } from '../config/index.js';
import { logger } from './logger.js';
import type { RoomEvent, RoomEventHandler } from './roomManager.js';
import type { EmitSecurityEvent } from './securityEvent.js';

// ---------------------------------------------------------------------------
// NATS client — publishes voice events for the control plane to consume.
//
// Subjects:
//   voice.joined          — user joined a voice channel
//   voice.left            — user left a voice channel
//   voice.room_empty      — last user left, room destroyed
//   voice.producer_added  — new producer (mic/camera/screen) started
//   voice.producer_removed — producer stopped
// ---------------------------------------------------------------------------

const jsonCodec = JSONCodec();

export class NatsService {
  private nc: NatsConnection | null = null;
  private connected = false;
  private readonly subscriptions: Array<{ unsubscribe(): void }> = [];
  private lastVoiceLifecycleMicroseconds = 0n;
  private emitSecurityEvent: EmitSecurityEvent | undefined;
  private securityDegraded = false;
  private readonly failedSubscriptions = new Set<string>();
  private closing = false;

  private observe(event: Parameters<EmitSecurityEvent>[0]): void {
    try {
      this.emitSecurityEvent?.(event);
    } catch {
      // An observer cannot terminate NATS subscription processing.
    }
  }

  setSecurityEventEmitter(emit: EmitSecurityEvent | undefined): void {
    this.emitSecurityEvent = emit;
  }

  private security(
    outcome: 'degraded' | 'restored',
    reasonCode: 'dependency_unavailable' | 'dependency_recovered'
  ): void {
    try {
      this.observe({
        eventType: 'dependency',
        outcome,
        severity: outcome === 'degraded' ? 'high' : 'informational',
        reasonCode,
      });
    } catch {
      // The observer is intentionally outside NATS availability semantics.
    }
  }

  private degraded(): void {
    if (this.securityDegraded) return;
    this.security('degraded', 'dependency_unavailable');
    this.securityDegraded = true;
  }

  private restored(): void {
    if (!this.connected || this.failedSubscriptions.size > 0 || !this.securityDegraded) return;
    this.security('restored', 'dependency_recovered');
    this.securityDegraded = false;
  }

  /**
   * Return a strictly increasing RFC3339 timestamp for lifecycle messages
   * published by this media-plane process. The control plane compares these
   * at microsecond precision, so advancing a logical microsecond avoids a
   * same-millisecond leave/join collapsing to the same lifecycle version.
   * Independent publisher processes remain unordered and are reconciled by
   * the control plane's lifecycle fencing and authoritative heartbeats.
   */
  nextVoiceLifecycleTimestamp(): string {
    const wallClockMicroseconds = BigInt(Date.now()) * 1000n;
    const nextMicroseconds =
      wallClockMicroseconds > this.lastVoiceLifecycleMicroseconds
        ? wallClockMicroseconds
        : this.lastVoiceLifecycleMicroseconds + 1n;
    this.lastVoiceLifecycleMicroseconds = nextMicroseconds;

    const milliseconds = nextMicroseconds / 1000n;
    const fraction = (nextMicroseconds % 1_000_000n).toString().padStart(6, '0');
    return new Date(Number(milliseconds)).toISOString().replace(/\.\d{3}Z$/, `.${fraction}Z`);
  }

  async connect(): Promise<void> {
    try {
      this.closing = false;
      this.nc = await connect({
        servers: config.natsUrl,
        name: 'media-plane',
        reconnect: true,
        maxReconnectAttempts: -1, // retry forever
        reconnectTimeWait: 2000,
      });
      this.connected = true;
      this.restored();

      logger.info('Connected to NATS', { server: config.natsUrl });

      // Monitor connection status. A completed or failed iterator cannot leave
      // the last connected state asserted: only connect/reconnect confirms it.
      const nc = this.nc;
      void (async () => {
        try {
          for await (const status of nc.status()) {
            if (status.type === 'disconnect' || status.type === 'reconnecting') {
              this.connected = false;
              this.degraded();
            } else if (status.type === 'reconnect') {
              this.connected = true;
              this.restored();
            }
            logger.info('NATS status', { type: status.type, data: status.data });
          }
          if (this.nc === nc && this.connected) {
            this.connected = false;
            this.degraded();
            logger.error('NATS status iterator ended');
          }
        } catch {
          if (this.nc === nc && this.connected) {
            this.connected = false;
            this.degraded();
          }
          logger.error('NATS status iterator failed', { error: 'nats_status_iterator_failed' });
        }
      })();
    } catch (err) {
      this.connected = false;
      this.degraded();
      logger.error('Failed to connect to NATS', { error: err, server: config.natsUrl });
      throw err;
    }
  }

  /** Publish a JSON message to a NATS subject */
  publish(subject: string, data: Record<string, unknown>): boolean {
    if (!this.nc || !this.connected || this.nc.isClosed()) {
      this.degraded();
      logger.warn('NATS not connected, dropping message', { subject });
      return false;
    }

    try {
      this.nc.publish(subject, jsonCodec.encode(data));
      this.restored();
      return true;
    } catch (err) {
      this.degraded();
      logger.error('Failed to publish NATS message', { subject, error: err });
      return false;
    }
  }

  /**
   * Returns a RoomEventHandler that publishes room events to NATS subjects.
   * Wire this into RoomManager.onEvent() to bridge media plane events to
   * the control plane via NATS.
   */
  createRoomEventHandler(): RoomEventHandler {
    return (event: RoomEvent) => {
      switch (event.type) {
        case 'user-joined':
          this.publish('voice.joined', {
            channelId: event.roomId,
            userId: event.userId,
            username: event.username,
            displayName: event.displayName,
            avatarUrl: event.avatarUrl,
            callId: event.callId,
            timestamp: this.nextVoiceLifecycleTimestamp(),
          });
          break;

        case 'user-left':
          this.publish('voice.left', {
            channelId: event.roomId,
            userId: event.userId,
            callId: event.callId,
            timestamp: this.nextVoiceLifecycleTimestamp(),
          });
          break;

        case 'room-empty':
          this.publish('voice.room_empty', {
            channelId: event.roomId,
            callId: event.callId,
            ringId: event.ringId,
            callerUserId: event.callerUserId,
            participantUserIds: event.participantUserIds,
            startedAt: event.startedAt,
            timestamp: this.nextVoiceLifecycleTimestamp(),
          });
          break;

        case 'producer-added':
          this.publish('voice.producer_added', {
            channelId: event.roomId,
            userId: event.userId,
            producerId: event.producerId,
            kind: event.kind,
            source: event.source,
            timestamp: new Date().toISOString(),
          });
          break;

        case 'producer-removed':
          this.publish('voice.producer_removed', {
            channelId: event.roomId,
            userId: event.userId,
            producerId: event.producerId,
            kind: event.kind,
            source: event.source,
            timestamp: new Date().toISOString(),
          });
          break;

        // active-speaker is handled locally via Socket.IO broadcast, not NATS
      }
    };
  }

  /** Subscribe to a NATS subject and call handler with decoded JSON payload */
  subscribe(
    subject: string,
    handler: (data: Record<string, unknown>) => void | Promise<void>
  ): void {
    if (!this.nc) {
      this.degraded();
      logger.warn('NATS not connected, cannot subscribe', { subject });
      return;
    }
    const nc = this.nc;
    const sub = nc.subscribe(subject);
    this.subscriptions.push(sub);
    if (this.failedSubscriptions.delete(subject)) this.restored();
    void (async () => {
      for await (const msg of sub) {
        let decoded: Record<string, unknown>;
        try {
          const value = jsonCodec.decode(msg.data);
          if (typeof value !== 'object' || value === null || Array.isArray(value)) {
            throw new TypeError('NATS message must decode to an object');
          }
          decoded = value as Record<string, unknown>;
        } catch {
          this.observe({
            eventType: 'media_integrity',
            outcome: 'denied',
            severity: 'medium',
            reasonCode: 'media_schema_rejected',
          });
          logger.warn('Rejected malformed NATS message', { subject });
          continue;
        }
        try {
          await handler(decoded);
        } catch (err) {
          logger.error('NATS message handler failed', { subject, error: err });
        }
      }
      if (!this.closing && this.nc === nc) {
        this.failedSubscriptions.add(subject);
        this.degraded();
        logger.error('NATS subscription ended', { subject });
      }
    })().catch((err) => {
      if (!this.closing && this.nc === nc) {
        this.failedSubscriptions.add(subject);
        this.degraded();
        logger.error('NATS subscription error', { subject, error: err });
      }
    });
  }

  async close(): Promise<void> {
    this.closing = true;
    this.connected = false;
    // Drain subscriptions before closing
    for (const sub of this.subscriptions) {
      try {
        sub.unsubscribe();
      } catch {
        /* already closed */
      }
    }
    this.subscriptions.length = 0;
    this.failedSubscriptions.clear();

    if (this.nc) {
      await this.nc.drain();
      logger.info('NATS connection closed');
      this.nc = null;
    }
  }
}
