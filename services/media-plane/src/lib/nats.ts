import {
  AckPolicy,
  DeliverPolicy,
  DiscardPolicy,
  JSONCodec,
  RetentionPolicy,
  StorageType,
  StoreCompression,
  connect,
  nanos,
} from 'nats';
import type {
  Consumer,
  ConsumerInfo,
  JetStreamManager,
  JsMsg,
  KV,
  NatsConnection,
  StreamInfo,
  Subscription,
} from 'nats';
import { isCanonicalEnforcementUUID } from './enforcementCommand.js';
import { decodeVoiceCommand, type VoiceCommand } from './voiceCommand.js';
import {
  VoiceCommandOwner,
  decodeVoiceCommandOwnerRecord,
  type VoiceCommandOwnerRecord,
  type VoiceCommandOwnerStore,
} from './voiceCommandOwner.js';
import type { VoiceCommandDispatcher } from './voiceCommandDispatcher.js';
import { config } from '../config/index.js';
import { logger } from './logger.js';
import type { RoomEvent, RoomEventHandler } from './roomManager.js';
import type { EmitSecurityEvent } from './securityEvent.js';
import { RequiredNatsSubscriptions } from './requiredNatsSubscriptions.js';

// ---------------------------------------------------------------------------
// NATS client — publishes voice events for the control plane to consume.
//
// Subjects:
//   voice.joined          — user joined a voice channel
//   voice.left            — user left a voice channel
//   voice.room_empty      — last user left, room destroyed
//   voice.producer_added  — new producer (mic/camera/screen) started
//   voice.producer_removed — producer stopped
//   voice.tier_violation  — the audio tier gate refused a mic (#1308)
// ---------------------------------------------------------------------------

const jsonCodec = JSONCodec();

const voiceCommandStream = 'VOICE_COMMANDS_V1';
const voiceCommandSubject = 'voice.commands.v1';
const voiceCommandDurable = 'voice-enforcement-v1';
const voiceCommandOwnerBucket = 'VOICE_COMMAND_OWNER_V1';
const voiceCommandOwnerStream = `KV_${voiceCommandOwnerBucket}`;
const voiceCommandOwnerKey = 'authoritative';
const voiceCommandGenerationMetadata = 'concord.voice-enforcement.generation';

function commandUnavailable(): Error {
  return new Error('Voice command transport unavailable');
}

function sameBirth(
  first: { streamCreatedAt: string; ownerStoreCreatedAt: string },
  second: { streamCreatedAt: string; ownerStoreCreatedAt: string }
): boolean {
  return (
    first.streamCreatedAt === second.streamCreatedAt &&
    first.ownerStoreCreatedAt === second.ownerStoreCreatedAt
  );
}

function validateCommandStream(info: StreamInfo, generation: string, ownerStore = false): void {
  // Pinned JS types predate these broker fields; StreamInfo preserves raw JSON.
  const c: StreamInfo['config'] & {
    allow_msg_ttl?: unknown;
    subject_delete_marker_ttl?: unknown;
    template_owner?: unknown;
    allow_msg_counter?: unknown;
    allow_atomic?: unknown;
    allow_msg_schedules?: unknown;
    allow_batched?: unknown;
    persist_mode?: unknown;
  } = info.config;
  const description = `concord.voice-enforcement generation=${generation}`;
  if (
    !isCanonicalEnforcementUUID(generation) ||
    !info.created ||
    c.name !== (ownerStore ? voiceCommandOwnerStream : voiceCommandStream) ||
    c.description !== description ||
    c.metadata?.[voiceCommandGenerationMetadata] !== generation ||
    c.subjects?.length !== 1 ||
    c.subjects[0] !== (ownerStore ? `$KV.${voiceCommandOwnerBucket}.>` : voiceCommandSubject) ||
    c.storage !== StorageType.File ||
    c.retention !== RetentionPolicy.Limits ||
    c.discard !== DiscardPolicy.New ||
    c.num_replicas !== 1 ||
    c.max_age !== 0 ||
    c.max_bytes !== (ownerStore ? -1 : 67108864) ||
    c.max_msgs !== (ownerStore ? -1 : 50000) ||
    c.max_msg_size !== (ownerStore ? -1 : 4096) ||
    c.max_msgs_per_subject !== (ownerStore ? 1 : -1) ||
    c.max_consumers !== -1 ||
    c.duplicate_window !== nanos(120_000) ||
    !!c.allow_rollup_hdrs !== ownerStore ||
    !!c.deny_delete !== ownerStore ||
    !!c.deny_purge ||
    c.no_ack ||
    c.sealed ||
    c.mirror ||
    c.sources?.length ||
    c.republish ||
    c.subject_transform ||
    c.discard_new_per_subject ||
    (c.allow_direct ?? false) !== ownerStore ||
    (c.mirror_direct ?? false) !== false ||
    (c.allow_msg_ttl ?? false) !== false ||
    (c.subject_delete_marker_ttl ?? 0) !== 0 ||
    (c.consumer_limits?.inactive_threshold ?? 0) !== 0 ||
    (c.consumer_limits?.max_ack_pending ?? 0) !== 0 ||
    (c.compression ?? StoreCompression.None) !== StoreCompression.None ||
    c.placement != null ||
    (c.template_owner ?? '') !== '' ||
    (c.first_seq !== undefined && c.first_seq !== 0) ||
    (c.allow_msg_counter !== undefined && c.allow_msg_counter !== false) ||
    (c.allow_atomic !== undefined && c.allow_atomic !== false) ||
    (c.allow_msg_schedules !== undefined && c.allow_msg_schedules !== false) ||
    (c.allow_batched !== undefined && c.allow_batched !== false) ||
    (c.persist_mode !== undefined && c.persist_mode !== 'default')
  )
    throw commandUnavailable();
}

function validateCommandConsumer(info: ConsumerInfo): void {
  const c = info.config;
  const pauseUntil = c.pause_until;
  const pauseUntilMs =
    typeof pauseUntil === 'string' &&
    /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/.test(pauseUntil)
      ? Date.parse(pauseUntil)
      : Number.NaN;
  if (
    (info.paused !== undefined && info.paused !== false) ||
    (pauseUntil !== undefined &&
      (typeof pauseUntil !== 'string' ||
        !Number.isFinite(pauseUntilMs) ||
        pauseUntilMs > Date.now())) ||
    info.stream_name !== voiceCommandStream ||
    info.name !== voiceCommandDurable ||
    c.durable_name !== voiceCommandDurable ||
    c.ack_policy !== AckPolicy.Explicit ||
    c.deliver_policy !== DeliverPolicy.All ||
    c.ack_wait !== nanos(30_000) ||
    c.max_ack_pending !== 1 ||
    c.max_deliver !== -1 ||
    c.filter_subject !== voiceCommandSubject ||
    c.filter_subjects?.length ||
    c.deliver_subject ||
    c.deliver_group ||
    c.inactive_threshold ||
    c.backoff?.length ||
    c.headers_only ||
    c.flow_control ||
    c.idle_heartbeat ||
    c.rate_limit_bps ||
    (c.num_replicas !== undefined && c.num_replicas !== 0 && c.num_replicas !== 1) ||
    c.mem_storage
  )
    throw commandUnavailable();
}

function consumerMissing(error: unknown): boolean {
  if (typeof error !== 'object' || error === null) return false;
  const value = error as { code?: string; api_error?: { err_code?: number } };
  return value.code === '404' && value.api_error?.err_code === 10014;
}

/** Read-only binding: a missing stream, bucket or key is never recreated here. */
class NatsVoiceCommandOwnerStore implements VoiceCommandOwnerStore {
  constructor(
    private readonly manager: JetStreamManager,
    private readonly kv: KV,
    private readonly generation: string,
    private readonly connected: () => boolean,
    private readonly stopRenewal: () => Promise<void>
  ) {}

  stopRenewalBeforeRelease(): Promise<void> {
    return this.stopRenewal();
  }

  private async birth(): Promise<{ streamCreatedAt: string; ownerStoreCreatedAt: string }> {
    if (!this.connected()) throw commandUnavailable();
    const [stream, bucket] = await Promise.all([
      this.manager.streams.info(voiceCommandStream),
      this.manager.streams.info(voiceCommandOwnerStream),
    ]);
    validateCommandStream(stream, this.generation);
    validateCommandStream(bucket, this.generation, true);
    if (!this.connected()) throw commandUnavailable();
    // StreamInfo.created is the broker's exact RFC3339Nano string. Passing it
    // through Date would round an authority fingerprint to milliseconds.
    return { streamCreatedAt: stream.created, ownerStoreCreatedAt: bucket.created };
  }

  async read(): Promise<{ value: VoiceCommandOwnerRecord; revision: number } | null> {
    const before = await this.birth();
    const entry = await this.kv.get(voiceCommandOwnerKey);
    const after = await this.birth();
    if (
      !sameBirth(before, after) ||
      entry?.operation !== 'PUT' ||
      !Number.isSafeInteger(entry.revision) ||
      entry.revision <= 0
    )
      throw commandUnavailable();
    const value = decodeVoiceCommandOwnerRecord(jsonCodec.decode(entry.value), this.generation);
    const proof = value.phase === 'initializing' ? value.birth : value.proof;
    if (!proof || !sameBirth(proof, after)) throw commandUnavailable();
    return { value, revision: entry.revision };
  }

  async compareAndSet(revision: number, value: VoiceCommandOwnerRecord): Promise<number> {
    if (!Number.isSafeInteger(revision) || revision <= 0) throw commandUnavailable();
    const validated = decodeVoiceCommandOwnerRecord(value, this.generation);
    if (validated.phase === 'initializing' || !validated.proof) throw commandUnavailable();
    const current = await this.read();
    if (
      current?.revision !== revision ||
      !current.value.proof ||
      !sameBirth(current.value.proof, validated.proof) ||
      current.value.proof.initializingRevision !== validated.proof.initializingRevision
    ) {
      throw commandUnavailable();
    }
    const before = await this.birth();
    if (!sameBirth(before, validated.proof)) throw commandUnavailable();
    const next = await this.kv.update(voiceCommandOwnerKey, jsonCodec.encode(validated), revision);
    const after = await this.birth();
    if (
      !sameBirth(before, after) ||
      !sameBirth(after, validated.proof) ||
      !Number.isSafeInteger(next) ||
      next <= revision
    )
      throw commandUnavailable();
    return next;
  }
}

interface VoiceCommandRuntime {
  owner: VoiceCommandOwner;
  dispatcher: VoiceCommandDispatcher;
  quarantine: (reason: string) => Promise<void>;
  manager: JetStreamManager;
  consumer: Consumer;
  consumerCreatedAt: string;
  ready: boolean;
  running: boolean;
  stopped: boolean;
  epoch: number;
  loop?: Promise<void>;
  inFlight?: Promise<void>;
  quarantinePromise?: Promise<void>;
  renewal?: Promise<void>;
  renewalTimer?: ReturnType<typeof setInterval>;
}

export class NatsService {
  private mediaInstanceIdentity:
    { color: 'blue' | 'green'; generation: string; instanceId: string } | undefined;
  private voiceCommands: VoiceCommandRuntime | undefined;

  private async voiceCommandBinding(generation?: string): Promise<{
    manager: JetStreamManager;
    store: VoiceCommandOwnerStore;
    generation: string;
  }> {
    if (!this.nc || !this.isConnected()) throw commandUnavailable();
    const manager = await this.nc.jetstreamManager({ timeout: 2000 });
    const stream = await manager.streams.info(voiceCommandStream);
    const actualGeneration = stream.config.metadata?.[voiceCommandGenerationMetadata];
    if (!actualGeneration || (generation && generation !== actualGeneration))
      throw commandUnavailable();
    validateCommandStream(stream, actualGeneration);
    // bindOnly calls Bucket.bind in pinned nats 2.29.3. It makes no create or
    // update request. Avoid the direct view: reads must use the stream leader.
    const kv = await this.nc.jetstream({ timeout: 2000 }).views.kv(voiceCommandOwnerBucket, {
      bindOnly: true,
      allow_direct: false,
      timeout: 2000,
    });
    const store = new NatsVoiceCommandOwnerStore(
      manager,
      kv,
      actualGeneration,
      () => this.isConnected(),
      () => this.stopVoiceCommandRenewal()
    );
    await store.read();
    return { manager, store, generation: actualGeneration };
  }

  async getVoiceCommandTransport(): Promise<{
    streamGeneration: string;
    store: VoiceCommandOwnerStore;
  }> {
    const binding = await this.voiceCommandBinding();
    return { streamGeneration: binding.generation, store: binding.store };
  }

  async confirmVoiceCommandContinuity(): Promise<void> {
    const runtime = this.voiceCommands;
    if (!runtime) throw commandUnavailable();
    // A clean stop retains a read-only owner/consumer proof for signed registry
    // release retries; it never restores command readiness or media authority.
    const stopped = runtime.stopped;
    if (!stopped && !this.voiceCommandReady()) throw commandUnavailable();
    const epoch = runtime.epoch;
    const ownerEpoch = runtime.owner.authorityEpoch();
    await this.verifyVoiceCommandContinuity(runtime);
    if (
      this.voiceCommands !== runtime ||
      runtime.epoch !== epoch ||
      runtime.owner.authorityEpoch() !== ownerEpoch ||
      runtime.stopped !== stopped ||
      (stopped ? runtime.ready || runtime.running : !this.voiceCommandReady())
    )
      throw commandUnavailable();
  }

  async createVoiceCommandOwner(options: {
    nodeBootId: string;
    onInvalidated: () => void;
  }): Promise<VoiceCommandOwner> {
    const binding = await this.voiceCommandBinding();
    return new VoiceCommandOwner({
      store: binding.store,
      streamGeneration: binding.generation,
      nodeBootId: options.nodeBootId,
      now: () => performance.now(),
      onInvalidated: options.onInvalidated,
    });
  }

  voiceCommandReady(): boolean {
    const runtime = this.voiceCommands;
    return (
      !!runtime && runtime.ready && runtime.running && this.isConnected() && runtime.owner.valid()
    );
  }

  requireVoiceCommandAuthority(): void {
    if (!this.voiceCommandReady()) throw commandUnavailable();
  }

  private async commandConsumer(manager: JetStreamManager): Promise<Consumer> {
    if (!this.nc || !this.isConnected()) throw commandUnavailable();
    let info: ConsumerInfo;
    try {
      info = await manager.consumers.info(voiceCommandStream, voiceCommandDurable);
    } catch (error) {
      if (!consumerMissing(error)) throw commandUnavailable();
      info = await manager.consumers.add(voiceCommandStream, {
        name: voiceCommandDurable,
        durable_name: voiceCommandDurable,
        ack_policy: AckPolicy.Explicit,
        deliver_policy: DeliverPolicy.All,
        ack_wait: nanos(30_000),
        max_ack_pending: 1,
        max_deliver: -1,
        filter_subject: voiceCommandSubject,
      });
    }
    validateCommandConsumer(info);
    return this.nc
      .jetstream({ timeout: 2000 })
      .consumers.get(voiceCommandStream, voiceCommandDurable);
  }

  async startVoiceCommandConsumer(options: {
    owner: VoiceCommandOwner;
    dispatcher: VoiceCommandDispatcher;
    quarantine: (reason: string) => Promise<void>;
  }): Promise<void> {
    if (this.voiceCommands) throw commandUnavailable();
    const binding = await this.voiceCommandBinding(options.owner.streamGeneration);
    await options.owner.acquire();
    const consumer = await this.commandConsumer(binding.manager);
    const info = await consumer.info();
    validateCommandConsumer(info);
    if (!info.created) throw commandUnavailable();
    const runtime: VoiceCommandRuntime = {
      ...options,
      manager: binding.manager,
      consumer,
      consumerCreatedAt: info.created,
      ready: true,
      running: true,
      stopped: false,
      epoch: 0,
    };
    this.voiceCommands = runtime;
    try {
      await this.verifyVoiceCommandContinuity(runtime);
      this.runVoiceCommandConsumer(runtime);
    } catch {
      await this.invalidateVoiceCommands('startup_unavailable');
      throw commandUnavailable();
    }
  }

  private async verifyVoiceCommandContinuity(runtime: VoiceCommandRuntime): Promise<void> {
    if (!this.isConnected() || !runtime.owner.valid()) throw commandUnavailable();
    const epoch = runtime.epoch;
    const ownerEpoch = runtime.owner.authorityEpoch();
    const [, info] = await Promise.all([
      runtime.owner.confirmContinuity(),
      runtime.consumer.info(),
    ]);
    validateCommandConsumer(info);
    if (
      info.created !== runtime.consumerCreatedAt ||
      runtime.epoch !== epoch ||
      runtime.owner.authorityEpoch() !== ownerEpoch ||
      !this.isConnected() ||
      !runtime.owner.valid()
    ) {
      throw commandUnavailable();
    }
  }

  private runVoiceCommandConsumer(runtime: VoiceCommandRuntime): void {
    const epoch = runtime.epoch;
    if (!runtime.renewalTimer) {
      runtime.renewalTimer = setInterval(() => {
        if (runtime.renewal) return;
        runtime.renewal = (async () => {
          await this.verifyVoiceCommandContinuity(runtime);
          await runtime.owner.renew();
        })()
          .catch(async () => {
            await this.invalidateVoiceCommands('continuity_unavailable');
          })
          .finally(() => {
            runtime.renewal = undefined;
          });
      }, 2000);
      runtime.renewalTimer.unref();
    }
    runtime.loop = this.pullVoiceCommands(runtime, epoch).catch(async () => {
      await this.invalidateVoiceCommands('consumer_unavailable');
    });
  }

  private async pullVoiceCommands(runtime: VoiceCommandRuntime, epoch: number): Promise<void> {
    while (runtime.running && runtime.epoch === epoch) {
      await this.verifyVoiceCommandContinuity(runtime);
      if (!runtime.running || runtime.epoch !== epoch) return;
      const message = await runtime.consumer.next({ expires: 2000 });
      if (!runtime.running || runtime.epoch !== epoch) return;
      if (!message) continue;
      const operation = this.processVoiceCommand(runtime, message, epoch);
      runtime.inFlight = operation;
      try {
        await operation;
      } finally {
        if (runtime.inFlight === operation) runtime.inFlight = undefined;
      }
    }
  }

  private async processVoiceCommand(
    runtime: VoiceCommandRuntime,
    message: JsMsg,
    epoch: number
  ): Promise<void> {
    let command: VoiceCommand;
    try {
      if (message.subject !== voiceCommandSubject) throw commandUnavailable();
      command = decodeVoiceCommand(message.data, runtime.owner.streamGeneration);
    } catch {
      this.observe({
        eventType: 'media_integrity',
        outcome: 'denied',
        severity: 'medium',
        reasonCode: 'media_schema_rejected',
      });
      runtime.ready = false;
      runtime.running = false;
      try {
        await runtime.quarantine('schema_rejected');
        await this.verifyVoiceCommandContinuity(runtime);
        if (runtime.epoch === epoch) message.term();
      } catch {
        await this.invalidateVoiceCommands('schema_teardown_unavailable');
      }
      return;
    }
    const timeout = setTimeout(() => {
      void this.invalidateVoiceCommands('handler_deadline');
    }, 10_000);
    const progress = setTimeout(() => {
      if (runtime.running && runtime.epoch === epoch && this.voiceCommandReady()) {
        void this.verifyVoiceCommandContinuity(runtime)
          .then(() => {
            if (runtime.running && runtime.epoch === epoch && this.voiceCommandReady())
              message.working();
          })
          .catch(() => this.invalidateVoiceCommands('continuity_unavailable'));
      }
    }, 5000);
    try {
      // A deadline invalidates authority but never detaches this promise. The
      // serial loop and shutdown retain it until its underlying effect settles.
      await runtime.dispatcher.applyDurable(command);
      if (!runtime.running || runtime.epoch !== epoch) {
        // A requested graceful stop withholds ack while the shutdown owner
        // remains valid for registry-release health/retry work.
        if (!runtime.owner.valid()) await this.invalidateVoiceCommands('authority_unavailable');
        return;
      }
      if (!this.voiceCommandReady()) {
        await this.invalidateVoiceCommands('authority_unavailable');
        return;
      }
      try {
        await this.verifyVoiceCommandContinuity(runtime);
      } catch {
        await this.invalidateVoiceCommands('continuity_unavailable');
        return;
      }
      if (!runtime.running || runtime.epoch !== epoch || !this.voiceCommandReady()) return;
      // Pinned JsMsg.ackAck marks didAck before awaiting the reply. An uncertain
      // result is retried on a fresh broker redelivery, never this same JsMsg.
      await message.ackAck({ timeout: 2000 });
    } catch {
      if (!runtime.owner.valid()) await this.invalidateVoiceCommands('authority_unavailable');
      // Effect/ack failures leave the retained delivery obligation unacknowledged.
    } finally {
      clearTimeout(progress);
      clearTimeout(timeout);
    }
  }

  private invalidateVoiceCommands(reason: string): Promise<void> {
    const runtime = this.voiceCommands;
    if (!runtime) return Promise.resolve();
    runtime.ready = false;
    runtime.running = false;
    runtime.epoch++;
    this.clearVoiceCommandRenewalTimer(runtime);
    runtime.owner.invalidate();
    this.degraded();
    if (!runtime.quarantinePromise) {
      runtime.quarantinePromise = runtime.quarantine(reason);
      void runtime.quarantinePromise.catch(() => {
        logger.error('Voice command quarantine unavailable', {
          error: 'voice_command_quarantine_failed',
        });
      });
    }
    return runtime.quarantinePromise;
  }

  async recoverVoiceCommandConsumerAfterTeardown(): Promise<void> {
    const runtime = this.voiceCommands;
    if (!runtime || runtime.inFlight || !this.isConnected()) throw commandUnavailable();
    if (runtime.quarantinePromise) {
      await runtime.quarantinePromise.catch(() => runtime.quarantine('recovery_teardown'));
    }
    const binding = await this.voiceCommandBinding(runtime.owner.streamGeneration);
    if (runtime.owner.valid()) await runtime.owner.renew();
    else await runtime.owner.recoverAfterTeardown();
    runtime.manager = binding.manager;
    runtime.consumer = await this.commandConsumer(binding.manager);
    const info = await runtime.consumer.info();
    validateCommandConsumer(info);
    if (!info.created) throw commandUnavailable();
    runtime.consumerCreatedAt = info.created;
    await this.verifyVoiceCommandContinuity(runtime);
    runtime.quarantinePromise = undefined;
    runtime.ready = true;
    runtime.running = true;
    runtime.stopped = false;
    this.runVoiceCommandConsumer(runtime);
    this.restored();
  }

  async stopVoiceCommandConsumer(): Promise<void> {
    const runtime = this.voiceCommands;
    if (!runtime) return;
    runtime.ready = false;
    runtime.running = false;
    runtime.epoch++;
    await runtime.loop;
    await runtime.inFlight;
    await runtime.renewal;
    runtime.stopped = true;
    // Ownership release belongs to the caller after terminal media AND registry
    // release completion. Neither this method nor close deletes the durable.
  }

  private clearVoiceCommandRenewalTimer(runtime: VoiceCommandRuntime): void {
    if (runtime.renewalTimer) clearInterval(runtime.renewalTimer);
    runtime.renewalTimer = undefined;
  }

  async stopVoiceCommandRenewal(): Promise<void> {
    const runtime = this.voiceCommands;
    if (!runtime) return;
    this.clearVoiceCommandRenewalTimer(runtime);
    await runtime.renewal;
  }

  private nc: NatsConnection | null = null;
  private connected = false;
  private readonly subscriptions = new Set<Subscription>();
  private lastVoiceLifecycleMicroseconds = 0n;
  private emitSecurityEvent: EmitSecurityEvent | undefined;
  private securityDegraded = false;
  private readonly failedSubscriptions = new Set<string>();
  private readonly requestSubscriptions = new Map<string, Subscription>();
  private closing = false;
  private connectionEpoch = 0;
  private cancelConnect?: () => void;
  private requiredSubscriptions = this.newRequiredSubscriptions();
  private hasRequiredSubscriptions = false;

  private newRequiredSubscriptions(): RequiredNatsSubscriptions {
    const subscriptions = new RequiredNatsSubscriptions(
      () => this.isConnected(),
      () => {
        if (!this.nc || !this.isConnected()) throw commandUnavailable();
        return this.nc.flush();
      }
    );
    subscriptions.observe((ready) => {
      if (this.closing || !this.hasRequiredSubscriptions) return;
      if (ready) this.restored();
      else this.degraded();
    });
    return subscriptions;
  }

  confirmRequiredSubscriptions(): Promise<void> {
    return this.requiredSubscriptions.confirm();
  }

  requiredSubscriptionsReady(): boolean {
    return this.requiredSubscriptions.ready();
  }

  observeRequiredSubscriptions(observer: (ready: boolean) => void): () => void {
    return this.requiredSubscriptions.observe(observer);
  }

  stopRequiredSubscriptionRecovery(): void {
    this.requiredSubscriptions.stop();
  }

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

  isConnected(): boolean {
    return this.connected && this.nc !== null && !this.nc.isClosed();
  }

  isRequestSubscriptionHealthy(subject: string): boolean {
    const sub = this.requestSubscriptions.get(subject);
    return this.isConnected() && !!sub && !sub.isClosed();
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
    if (
      !this.connected ||
      this.failedSubscriptions.size > 0 ||
      (this.hasRequiredSubscriptions && !this.requiredSubscriptionsReady()) ||
      !this.securityDegraded ||
      (this.voiceCommands && !this.voiceCommandReady())
    )
      return;
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
    const epoch = ++this.connectionEpoch;
    this.closing = false;
    try {
      const pending = connect({
        servers: config.natsUrl,
        name: 'media-plane',
        reconnect: true,
        maxReconnectAttempts: -1,
        reconnectTimeWait: 2000,
      });
      // Cancellation settles startup even if the underlying first dial is still
      // pending; a late connection is immediately closed without being adopted.
      void pending.then(
        (nc) => {
          if (this.closing || epoch !== this.connectionEpoch)
            void nc.close().catch(() => undefined);
        },
        () => undefined
      );
      const nc = await new Promise<NatsConnection>((resolve, reject) => {
        this.cancelConnect = () => reject(commandUnavailable());
        pending.then(resolve, reject);
      });
      if (this.closing || epoch !== this.connectionEpoch) throw commandUnavailable();
      this.cancelConnect = undefined;
      this.nc = nc;
      this.connected = true;
      this.restored();
      logger.info('Connected to NATS');

      void (async () => {
        try {
          for await (const status of nc.status()) {
            if (this.nc !== nc || this.closing) return;
            if (status.type === 'disconnect' || status.type === 'reconnecting') {
              this.connectionUnavailable();
            } else if (status.type === 'reconnect' && !this.connected) {
              this.connected = true;
              this.requiredSubscriptions.connectionChanged();
              this.restored();
            }
            logger.info('NATS status', { type: status.type });
          }
          if (this.nc === nc && this.connected) {
            this.connectionUnavailable();
            logger.error('NATS status iterator ended');
          }
        } catch {
          if (this.nc === nc && this.connected) this.connectionUnavailable();
          logger.error('NATS status iterator failed', { error: 'nats_status_iterator_failed' });
        }
      })();
    } catch (err) {
      if (epoch === this.connectionEpoch) {
        this.cancelConnect = undefined;
        this.connectionUnavailable();
      }
      logger.error('Failed to connect to NATS', { error: 'nats_connect_failed' });
      throw err;
    }
  }

  private connectionUnavailable(): void {
    this.connected = false;
    this.requiredSubscriptions.connectionChanged();
    void this.invalidateVoiceCommands('connection_unavailable').catch(() => undefined);
    this.degraded();
  }

  /** Publish a JSON message to a NATS subject */
  publish(subject: string, data: Record<string, unknown>): boolean {
    if (!this.nc || !this.connected || this.nc.isClosed()) {
      this.degraded();
      logger.warn('NATS not connected, dropping message');
      return false;
    }

    try {
      this.nc.publish(subject, jsonCodec.encode(data));
      this.restored();
      return true;
    } catch {
      this.degraded();
      logger.error('Failed to publish NATS message', { error: 'nats_publish_failed' });
      return false;
    }
  }

  setMediaInstanceIdentity(identity: {
    color: 'blue' | 'green';
    generation: string;
    instanceId: string;
  }): void {
    this.mediaInstanceIdentity = identity;
  }

  mediaLifecycleFields(): Record<string, string> {
    const identity = this.mediaInstanceIdentity;
    return identity ? { colour: identity.color, instance_id: identity.instanceId } : {};
  }

  publishRoomHeartbeat(payload: {
    channelId: string;
    userIds: string[];
    callId?: string;
    ringId?: string;
    callerUserId?: string;
    timestamp: string;
  }): boolean {
    return this.publish('voice.heartbeat', {
      ...this.mediaLifecycleFields(),
      ...payload,
      participants_authoritative: payload.userIds.length > 0,
    });
  }

  /**
   * Returns a RoomEventHandler that publishes room events to NATS subjects.
   * Wire this into RoomManager.onEvent() to bridge media plane events to
   * the control plane via NATS.
   */
  createRoomEventHandler(): RoomEventHandler {
    return (event: RoomEvent) => {
      switch (event.type) {
        case 'room-created':
          break;
        case 'user-joined':
          this.publish('voice.joined', {
            ...this.mediaLifecycleFields(),
            channelId: event.roomId,
            userId: event.userId,
            username: event.username,
            displayName: event.displayName,
            avatarUrl: event.avatarUrl,
            callId: event.callId,
            admissionId: event.admissionId,
            socketId: event.socketId,
            timestamp: this.nextVoiceLifecycleTimestamp(),
          });
          break;

        case 'user-left':
          this.publish('voice.left', {
            ...this.mediaLifecycleFields(),
            channelId: event.roomId,
            userId: event.userId,
            callId: event.callId,
            timestamp: this.nextVoiceLifecycleTimestamp(),
          });
          break;

        case 'room-empty':
          this.publish('voice.room_empty', {
            ...this.mediaLifecycleFields(),
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

        case 'tier-violation':
          // #1308 misbehaviour telemetry. The user id and the folded, closed
          // reason ONLY -- never the room, the rtpParameters or any media
          // detail. The control plane bounds this payload's length before it
          // parses it, so nothing may be added here without raising that bound.
          this.publish('voice.tier_violation', {
            userId: event.userId,
            reason: event.reason,
            timestamp: new Date().toISOString(),
          });
          break;

        // active-speaker is handled locally via Socket.IO broadcast, not NATS
      }
    };
  }

  /** Subscribe to a NATS subject and call handler with decoded JSON payload. */
  subscribe(
    subject: string,
    handler: (data: Record<string, unknown>) => void | Promise<void>,
    options: { required?: boolean } = {}
  ): void {
    if (options.required && (this.closing || !this.isConnected())) throw commandUnavailable();
    if (!this.nc) {
      this.degraded();
      logger.warn('NATS not connected, cannot subscribe');
      return;
    }
    const install = (): Subscription => {
      const nc = this.nc;
      if (!nc) throw commandUnavailable();
      const sub = nc.subscribe(subject);
      this.subscriptions.add(sub);
      if (this.failedSubscriptions.delete(subject)) this.restored();
      const ended = () => {
        this.subscriptions.delete(sub);
        if (this.closing || this.nc !== nc) return;
        if (options.required) {
          if (!this.requiredSubscriptions.ended(subject, sub)) return;
        } else this.failedSubscriptions.add(subject);
        this.degraded();
        logger.error('NATS subscription ended', { error: 'nats_subscription_ended' });
      };
      void (async () => {
        for await (const msg of sub) {
          if (options.required && sub.isClosed()) break;
          let decoded: Record<string, unknown>;
          try {
            const value = jsonCodec.decode(msg.data);
            if (typeof value !== 'object' || value === null || Array.isArray(value))
              throw new TypeError('NATS message must decode to an object');
            decoded = value as Record<string, unknown>;
          } catch {
            this.observe({
              eventType: 'media_integrity',
              outcome: 'denied',
              severity: 'medium',
              reasonCode: 'media_schema_rejected',
            });
            logger.warn('Rejected malformed NATS message');
            continue;
          }
          try {
            await handler(decoded);
          } catch {
            logger.error('NATS message handler failed', { error: 'nats_handler_failed' });
          }
        }
        ended();
      })().catch(() => {
        ended();
        logger.error('NATS subscription error', { error: 'nats_subscription_failed' });
      });
      if (options.required) void sub.closed.then(ended, ended);
      return sub;
    };
    if (options.required) {
      this.hasRequiredSubscriptions = true;
      this.requiredSubscriptions.register(subject, install);
    } else install();
  }

  /** Reply only after a handler confirms its terminal effect; failure retains the obligation. */
  subscribeRequest(
    subject: string,
    handler: (
      data: Record<string, unknown>
    ) => Record<string, unknown> | undefined | Promise<Record<string, unknown> | undefined>,
    options: { required?: boolean } = {}
  ): boolean {
    if (options.required && (this.closing || !this.isConnected())) throw commandUnavailable();
    if (!this.nc) {
      this.degraded();
      logger.warn('NATS not connected, cannot subscribe');
      return false;
    }
    const install = (): Subscription => {
      const nc = this.nc;
      if (!nc) throw commandUnavailable();
      const sub = nc.subscribe(subject);
      this.subscriptions.add(sub);
      this.requestSubscriptions.set(subject, sub);
      const ended = () => {
        this.subscriptions.delete(sub);
        if (this.requestSubscriptions.get(subject) === sub)
          this.requestSubscriptions.delete(subject);
        if (this.closing || this.nc !== nc) return;
        if (options.required && !this.requiredSubscriptions.ended(subject, sub)) return;
        this.degraded();
        logger.error('NATS request subscription ended', {
          error: 'nats_request_subscription_ended',
        });
      };
      void (async () => {
        for await (const msg of sub) {
          if (options.required && sub.isClosed()) break;
          try {
            if (!msg.reply) continue;
            const decoded = jsonCodec.decode(msg.data) as Record<string, unknown>;
            const response = await handler(decoded);
            if (response !== undefined && (!options.required || !sub.isClosed()))
              msg.respond(jsonCodec.encode(response));
          } catch {
            logger.error('Failed to handle NATS request', { error: 'nats_request_handler_failed' });
          }
        }
        ended();
      })().catch(() => {
        ended();
        logger.error('NATS request subscription error', {
          error: 'nats_request_subscription_failed',
        });
      });
      if (options.required) void sub.closed.then(ended, ended);
      return sub;
    };
    if (options.required) {
      this.hasRequiredSubscriptions = true;
      this.requiredSubscriptions.register(subject, install);
    } else install();
    return true;
  }

  async close(): Promise<void> {
    this.closing = true;
    this.connectionEpoch++;
    this.cancelConnect?.();
    this.cancelConnect = undefined;
    this.connected = false;
    this.requiredSubscriptions.close();
    await this.stopVoiceCommandRenewal();
    await this.stopVoiceCommandConsumer();
    // Drain subscriptions before closing
    for (const sub of this.subscriptions) {
      try {
        sub.unsubscribe();
      } catch {
        /* already closed */
      }
    }
    this.subscriptions.clear();
    this.failedSubscriptions.clear();
    this.requestSubscriptions.clear();

    if (this.nc) {
      await this.nc.drain();
      logger.info('NATS connection closed');
      this.nc = null;
    }
    this.requiredSubscriptions = this.newRequiredSubscriptions();
    this.hasRequiredSubscriptions = false;
  }
}
