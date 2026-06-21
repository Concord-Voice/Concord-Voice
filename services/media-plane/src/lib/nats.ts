import { connect, JSONCodec } from 'nats';
import type { NatsConnection } from 'nats';
import { config } from '../config/index.js';
import { logger } from './logger.js';
import type { RoomEvent, RoomEventHandler } from './roomManager.js';

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
  private readonly subscriptions: Array<{ unsubscribe(): void }> = [];

  async connect(): Promise<void> {
    try {
      this.nc = await connect({
        servers: config.natsUrl,
        name: 'media-plane',
        reconnect: true,
        maxReconnectAttempts: -1, // retry forever
        reconnectTimeWait: 2000,
      });

      logger.info('Connected to NATS', { server: config.natsUrl });

      // Monitor connection status
      (async () => {
        if (!this.nc) return;
        for await (const status of this.nc.status()) {
          logger.info('NATS status', { type: status.type, data: status.data });
        }
      })().catch(() => {});
    } catch (err) {
      logger.error('Failed to connect to NATS', { error: err, server: config.natsUrl });
      throw err;
    }
  }

  /** Publish a JSON message to a NATS subject */
  publish(subject: string, data: Record<string, unknown>): void {
    if (!this.nc) {
      logger.warn('NATS not connected, dropping message', { subject });
      return;
    }

    try {
      this.nc.publish(subject, jsonCodec.encode(data));
    } catch (err) {
      logger.error('Failed to publish NATS message', { subject, error: err });
    }
  }

  /**
   * Returns a RoomEventHandler that publishes room events to NATS subjects.
   * Wire this into RoomManager.onEvent() to bridge media plane events to
   * the control plane via NATS.
   */
  createRoomEventHandler(): RoomEventHandler {
    return (event: RoomEvent) => {
      const timestamp = new Date().toISOString();

      switch (event.type) {
        case 'user-joined':
          this.publish('voice.joined', {
            channelId: event.roomId,
            userId: event.userId,
            username: event.username,
            displayName: event.displayName,
            timestamp,
          });
          break;

        case 'user-left':
          this.publish('voice.left', {
            channelId: event.roomId,
            userId: event.userId,
            timestamp,
          });
          break;

        case 'room-empty':
          this.publish('voice.room_empty', {
            channelId: event.roomId,
            timestamp,
          });
          break;

        case 'producer-added':
          this.publish('voice.producer_added', {
            channelId: event.roomId,
            userId: event.userId,
            producerId: event.producerId,
            kind: event.kind,
            source: event.source,
            timestamp,
          });
          break;

        case 'producer-removed':
          this.publish('voice.producer_removed', {
            channelId: event.roomId,
            userId: event.userId,
            producerId: event.producerId,
            kind: event.kind,
            source: event.source,
            timestamp,
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
      logger.warn('NATS not connected, cannot subscribe', { subject });
      return;
    }
    const sub = this.nc.subscribe(subject);
    this.subscriptions.push(sub);
    (async () => {
      for await (const msg of sub) {
        try {
          const decoded = jsonCodec.decode(msg.data) as Record<string, unknown>;
          handler(decoded);
        } catch (err) {
          logger.error('Failed to handle NATS message', { subject, error: err });
        }
      }
    })().catch((err) => {
      logger.error('NATS subscription error', { subject, error: err });
    });
  }

  async close(): Promise<void> {
    // Drain subscriptions before closing
    for (const sub of this.subscriptions) {
      try {
        sub.unsubscribe();
      } catch {
        /* already closed */
      }
    }
    this.subscriptions.length = 0;

    if (this.nc) {
      await this.nc.drain();
      logger.info('NATS connection closed');
    }
  }
}
