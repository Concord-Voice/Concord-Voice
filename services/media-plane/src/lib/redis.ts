import { randomInt, randomUUID } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { hostname } from 'node:os';
import { createClient, type RedisClientOptions, type RedisClientType } from 'redis';
import { config } from '../config/index.js';
import { logger } from './logger.js';
import type { RoomEvent, RoomEventHandler } from './roomManager.js';
import type { EmitSecurityEvent } from './securityEvent.js';
import { RoomCreationRefusedError } from './roomManager.js';
import {
  acknowledgeMediaCredential,
  parseManagedCredential,
  PROVIDER_CREDENTIAL_FETCH_TIMEOUT_MS,
  readExactMediaCredential,
  type AppliedAck,
  type ExactSecretReader,
  type ManagedCredential,
  type SecretScope,
} from './managedRedis.js';
export { parseManagedCredential } from './managedRedis.js';

const ROOM_LIVE_TTL_SECONDS = 90;
const CREATE_GUARD_TIMEOUT_MS = 1_000;
// shortcut: 256 is an unmeasured fail-closed queue ceiling; replace with observed peak in-flight joins before enabling placement.
const REDIS_COMMAND_QUEUE_MAX_LENGTH = 256;
const MANAGED_CREDENTIAL_NAME = 'REDIS_MEDIA_PLANE_CREDENTIAL';
const MANAGED_STALE_MS = 120_000;
const MANAGED_POLL_MS = 30_000;
const MANAGED_AUTH_TIMEOUT_MS = 10_000;
const MANAGED_DRAIN_MS = 30_000;
const MANAGED_FAILURE_CODES = new Set([
  'managed_redis_provider_forbidden',
  'managed_redis_provider_rate_limited',
  'managed_redis_provider_unavailable',
  'managed_redis_provider_timeout',
  'managed_redis_provider_invalid',
  'managed_redis_credential_invalid',
  'managed_redis_generation_replay',
  'managed_redis_generation_changed',
  'managed_redis_name_mismatch',
  'managed_redis_fetch_timeout',
  'managed_redis_candidate_rejected',
  'managed_redis_ack_failed',
]);

interface ManagedRedisOptions {
  readExact?: ExactSecretReader;
  createManagedClient?: (options: RedisClientOptions) => RedisClientType;
  acknowledge?: (ack: AppliedAck) => Promise<void>;
  now?: () => Date;
}

function managedEndpoint(): { host: string; port: number } {
  const endpoint = config.redisEndpoint;
  if (
    !endpoint ||
    endpoint.includes('/') ||
    endpoint.includes('@') ||
    endpoint.includes('?') ||
    endpoint.includes('#')
  ) {
    throw new Error('managed_redis_endpoint_invalid');
  }
  try {
    const parsed = new URL(`redis://${endpoint}`);
    const port = Number(parsed.port);
    if (
      !parsed.hostname ||
      !Number.isInteger(port) ||
      port < 1 ||
      port > 65_535 ||
      (parsed.pathname !== '/' && parsed.pathname !== '') ||
      parsed.username ||
      parsed.password ||
      parsed.search ||
      parsed.hash
    ) {
      throw new Error('managed_redis_endpoint_invalid');
    }
    const host = parsed.hostname.startsWith('[') ? parsed.hostname.slice(1, -1) : parsed.hostname;
    return { host, port };
  } catch {
    throw new Error('managed_redis_endpoint_invalid');
  }
}

function sameValidityInstant(a: string, b: string): boolean {
  const subMillisecond = (value: string) =>
    (/\.(\d{1,9})/.exec(value)?.[1] ?? '').padEnd(9, '0').slice(3);
  return Date.parse(a) === Date.parse(b) && subMillisecond(a) === subMillisecond(b);
}

function sameManagedCredential(a: ManagedCredential, b: ManagedCredential): boolean {
  return (
    a.version === b.version &&
    a.rotationId === b.rotationId &&
    a.service === b.service &&
    a.generation === b.generation &&
    a.username === b.username &&
    a.password === b.password &&
    sameValidityInstant(a.validFrom, b.validFrom) &&
    sameValidityInstant(a.validUntil, b.validUntil) &&
    a.scope.projectId === b.scope.projectId &&
    a.scope.environment === b.scope.environment &&
    a.scope.path === b.scope.path &&
    a.scope.name === b.scope.name
  );
}
const REMOVE_OWNED_ROOM_LIVE_SCRIPT =
  "if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) else return 0 end";
const CREATE_ROOM_LIVE_WITH_FENCE_SCRIPT = `
local owner = redis.call('GET', KEYS[1])
local live = redis.call('GET', KEYS[2])
local draining = redis.call('GET', KEYS[3])
if draining then return -1 end
if owner and owner ~= ARGV[1] then return -2 end
if live and live ~= ARGV[2] and owner ~= ARGV[1] then return -2 end
redis.call('SET', KEYS[2], ARGV[2], 'EX', ARGV[3])
return 1
`;
const REFRESH_ROOM_LIVE_WITH_FENCE_SCRIPT = `
local owner = redis.call('GET', KEYS[1])
local live = redis.call('GET', KEYS[2])
if owner and owner ~= ARGV[1] then return 0 end
if live and live ~= ARGV[2] then return 0 end
redis.call('SET', KEYS[2], ARGV[2], 'EX', ARGV[3])
return 1
`;

export interface MediaInstanceIdentity {
  color: 'blue' | 'green';
  generation: string;
  instanceId: string;
}

export interface MediaHealthState {
  color: 'blue' | 'green' | null;
  generation: string | null;
  acceptingNewRooms: boolean;
  redisReachable: boolean;
  ownerRefusalsTotal: number;
}

// ---------------------------------------------------------------------------
// Redis room state — cross-instance awareness + control plane queries.
//
// Keys:
//   voice:room:{channelId}   → SET of userIds (TTL 120s, refreshed on join)
//   voice:user:{userId}      → HASH { channelId, joinedAt } (TTL 120s)
//
// The TTL acts as a safety net: if the media plane crashes without cleanup,
// stale entries expire within 2 minutes.
// ---------------------------------------------------------------------------

const ROOM_TTL = 120; // seconds
const USER_TTL = 120;

export function redisLogTarget(redisUrl: string): { endpoint: string } {
  try {
    const parsed = new URL(redisUrl);
    const port = parsed.port ? `:${parsed.port}` : '';
    return { endpoint: `${parsed.hostname}${port}` };
  } catch {
    return { endpoint: 'configured' };
  }
}

export class RedisService {
  private client: RedisClientType | null = null;
  private readonly managedOptions: ManagedRedisOptions;
  private readonly managedProviderAuth = { token: '', renewAt: 0, expiresAt: 0 };
  private managedCredential: ManagedCredential | null = null;
  private managedFetchedAt = 0;
  private managedFetchedMonotonic = 0;
  private managedAckPending = false;
  private managedPoll: NodeJS.Timeout | null = null;
  private managedRefresh: Promise<void> | null = null;
  private managedClosed = false;
  private readonly oldManagedClients = new Set<RedisClientType>();
  private emitSecurityEvent: EmitSecurityEvent | undefined;
  private securityDegraded = false;
  private instanceIdentity: MediaInstanceIdentity | undefined;
  private redisReachable = false;
  private acceptingNewRooms = false;
  private ownerRefusalsTotal = 0;
  private pendingOwnerReadCommands = 0;

  constructor(options: ManagedRedisOptions = {}) {
    this.managedOptions = options;
  }

  get appliedGeneration(): number | null {
    return this.managedCredential?.generation ?? null;
  }

  private now(): Date {
    return this.managedOptions.now?.() ?? new Date();
  }

  private currentClient(): RedisClientType | null {
    if (config.redisCredentialMode === 'managed' && !this.managedFresh()) {
      this.redisReachable = false;
      this.acceptingNewRooms = false;
      this.degraded();
      return null;
    }
    return this.client;
  }

  private managedFresh(): boolean {
    if (config.redisCredentialMode !== 'managed') return true;
    return this.credentialFresh(this.managedCredential, this.managedFetchedAt);
  }

  // A stale credential keeps serving until validUntil: a provider outage is an
  // alert, not an app outage, and Redis ACL revocation still cuts it off. The
  // age alone decides staleness, so the alert keeps firing after validUntil.
  private managedStale(): boolean {
    if (config.redisCredentialMode !== 'managed' || !this.managedCredential) return false;
    const age = this.managedOptions.now
      ? this.now().getTime() - this.managedFetchedAt
      : performance.now() - this.managedFetchedMonotonic;
    return age >= MANAGED_STALE_MS;
  }

  private credentialFresh(credential: ManagedCredential | null, fetchedAt: number): boolean {
    const until = credential && Date.parse(credential.validUntil);
    return !!until && fetchedAt > 0 && this.now().getTime() < until;
  }

  setSecurityEventEmitter(emit: EmitSecurityEvent | undefined): void {
    this.emitSecurityEvent = emit;
  }
  private security(
    outcome: 'degraded' | 'restored',
    reasonCode: 'dependency_unavailable' | 'dependency_recovered'
  ): void {
    try {
      this.emitSecurityEvent?.({
        eventType: 'dependency',
        outcome,
        severity: outcome === 'degraded' ? 'high' : 'informational',
        reasonCode,
      });
    } catch {
      // The observer is intentionally outside Redis availability semantics.
    }
  }
  private degraded(): void {
    if (this.securityDegraded) return;
    this.security('degraded', 'dependency_unavailable');
    this.securityDegraded = true;
  }
  private restored(): void {
    if (!this.securityDegraded) return;
    this.security('restored', 'dependency_recovered');
    this.securityDegraded = false;
  }
  private redisCommandSucceeded(): void {
    this.redisReachable = true;
    this.restored();
  }
  private redisCommandFailed(): void {
    this.redisReachable = false;
    this.degraded();
  }

  async connect(): Promise<void> {
    if (config.redisCredentialMode === 'managed') {
      const cold = this.managedCredential === null;
      try {
        await this.refreshManagedCredential();
      } catch {
        if (cold && this.client) {
          this.client.destroy();
          this.client = null;
          this.managedCredential = null;
          this.managedFetchedAt = 0;
          this.managedFetchedMonotonic = 0;
          this.managedAckPending = false;
          this.redisReachable = false;
        }
        throw new Error('managed_redis_cold_start_failed');
      }
      if (!this.managedPoll) {
        this.managedPoll = setInterval(() => {
          void this.refreshManagedCredential().catch((error: unknown) => {
            if (!this.managedFresh()) {
              this.redisReachable = false;
              this.acceptingNewRooms = false;
              this.degraded();
            }
            const code =
              error instanceof Error && MANAGED_FAILURE_CODES.has(error.message)
                ? error.message
                : 'managed_redis_refresh_failed';
            // A refused ACK leaves the applied generation serving; it is routine
            // during an in-place rotation until the running apps are registered.
            // Staleness below is the alert.
            if (code === 'managed_redis_ack_failed') {
              logger.warn('Managed Redis refresh failed', { code });
            } else {
              logger.error('Managed Redis refresh failed', { code });
            }
            if (this.managedStale()) {
              logger.error(
                'Managed Redis credential stale; serving the last validated generation',
                {
                  code: 'managed_redis_credential_stale',
                }
              );
            }
          });
        }, MANAGED_POLL_MS);
        this.managedPoll.unref();
      }
      return;
    }
    const client = createClient({
      url: config.redisUrl,
      ...(config.roomPlacementEnabled && {
        disableOfflineQueue: true,
        commandsQueueMaxLength: REDIS_COMMAND_QUEUE_MAX_LENGTH,
      }),
    });
    this.client = client;
    try {
      client.on('error', (err) => {
        if (this.client !== client) return;
        this.redisReachable = false;
        this.degraded();
        logger.error('Redis client error', { error: err });
      });

      client.on('reconnecting', () => {
        if (this.client !== client) return;
        this.redisReachable = false;
        this.degraded();
        logger.info('Redis reconnecting');
      });

      client.on('ready', () => {
        if (this.client !== client) return;
        this.redisReachable = true;
        this.restored();
      });

      await client.connect();
      if (this.client !== client) return;
      this.redisReachable = true;
      this.restored();
      logger.info('Connected to Redis', redisLogTarget(config.redisUrl));
    } catch (err) {
      this.degraded();
      logger.error('Failed to connect to Redis', { error: err });
      throw err;
    }
  }

  async refreshManagedCredential(): Promise<void> {
    if (config.redisCredentialMode !== 'managed') throw new Error('managed_redis_mode_required');
    if (this.managedRefresh) return this.managedRefresh;
    const attempt = this.refreshManagedCredentialOnce();
    this.managedRefresh = attempt;
    try {
      await attempt;
    } finally {
      if (this.managedRefresh === attempt) this.managedRefresh = null;
    }
  }

  private async fetchManagedCredential(
    scope: SecretScope
  ): Promise<{ credential: ManagedCredential; now: Date; fetchedMonotonic: number }> {
    if (Object.values(scope).some((value) => !value))
      throw new Error('managed_redis_scope_missing');
    const reader =
      this.managedOptions.readExact ??
      ((requested: SecretScope) =>
        readExactMediaCredential(
          requested,
          config.redisInfisicalUrl,
          config.redisInfisicalClientId,
          AbortSignal.timeout(PROVIDER_CREDENTIAL_FETCH_TIMEOUT_MS),
          this.managedProviderAuth
        ));
    const fetched = await this.managedDeadline(
      reader(scope),
      PROVIDER_CREDENTIAL_FETCH_TIMEOUT_MS,
      'managed_redis_fetch_timeout'
    );
    if (this.managedClosed) throw new Error('managed_redis_closed');
    if (fetched.returnedName !== undefined && fetched.returnedName !== scope.name) {
      throw new Error('managed_redis_name_mismatch');
    }
    const now = this.now();
    const fetchedMonotonic = performance.now();
    return {
      credential: parseManagedCredential(fetched.value, scope, now),
      now,
      fetchedMonotonic,
    };
  }

  private async reuseCurrentManagedCredential(
    candidate: ManagedCredential,
    now: Date,
    fetchedMonotonic: number
  ): Promise<boolean> {
    const previous = this.managedCredential;
    if (previous && candidate.generation < previous.generation) {
      throw new Error('managed_redis_generation_replay');
    }
    if (candidate.generation !== previous?.generation) return false;
    if (!sameManagedCredential(candidate, previous)) {
      throw new Error('managed_redis_generation_changed');
    }
    // A validated same-generation read resets staleness even while Redis is down.
    this.managedFetchedAt = now.getTime();
    this.managedFetchedMonotonic = fetchedMonotonic;
    if (!this.client?.isReady) return false;
    if (this.managedAckPending) await this.acknowledgeApplied();
    return true;
  }

  private async createAuthenticatedManagedClient(
    credential: ManagedCredential,
    endpoint: { host: string; port: number }
  ): Promise<RedisClientType> {
    const ca = config.redisTlsCaFile ? await readFile(config.redisTlsCaFile, 'utf8') : undefined;
    if (ca && !config.redisTls) throw new Error('managed_redis_tls_config_invalid');
    const reconnectStrategy = (retries: number): number | false => {
      if (this.client !== candidate || !this.managedFresh()) return false;
      return Math.min(2 ** retries * 50, 2_000) + randomInt(200);
    };
    const socket = config.redisTls
      ? {
          host: endpoint.host,
          port: endpoint.port,
          tls: true as const,
          reconnectStrategy,
          ...(ca && { ca }),
        }
      : { host: endpoint.host, port: endpoint.port, reconnectStrategy };
    const options: RedisClientOptions = {
      socket,
      username: credential.username,
      password: credential.password,
      disableOfflineQueue: true,
      commandsQueueMaxLength: REDIS_COMMAND_QUEUE_MAX_LENGTH,
    };
    const create = this.managedOptions.createManagedClient ?? createClient;
    const candidate = create(options);
    candidate.on('error', () => {
      if (this.client !== candidate) return;
      this.redisReachable = false;
      this.degraded();
    });
    candidate.on('reconnecting', () => {
      if (this.client !== candidate) return;
      if (!this.managedFresh()) {
        this.retireTimedOutClient(candidate);
        return;
      }
      this.redisReachable = false;
      this.degraded();
    });
    candidate.on('terminated', () => {
      if (this.client === candidate) this.retireTimedOutClient(candidate);
    });
    candidate.on('ready', () => {
      if (this.client !== candidate) return;
      if (!this.managedFresh()) {
        this.retireTimedOutClient(candidate);
        return;
      }
      this.redisReachable = true;
      this.restored();
    });
    try {
      const authDeadline = performance.now() + MANAGED_AUTH_TIMEOUT_MS;
      await this.managedDeadline(
        candidate.connect(),
        Math.max(0, authDeadline - performance.now()),
        'managed_redis_auth_timeout'
      );
      await this.managedDeadline(
        candidate.ping(),
        Math.max(0, authDeadline - performance.now()),
        'managed_redis_ping_timeout'
      );
      const username = await this.managedDeadline(
        candidate.aclWhoAmI(),
        Math.max(0, authDeadline - performance.now()),
        'managed_redis_identity_timeout'
      );
      if (username !== credential.username) throw new Error('managed_redis_candidate_rejected');
    } catch {
      candidate.destroy();
      throw new Error('managed_redis_candidate_rejected');
    }
    return candidate;
  }

  private async refreshManagedCredentialOnce(): Promise<void> {
    if (this.managedClosed) throw new Error('managed_redis_closed');
    const scope: SecretScope = {
      projectId: config.redisInfisicalProjectId,
      environment: config.redisInfisicalEnvironment,
      path: config.redisInfisicalPath,
      name: MANAGED_CREDENTIAL_NAME,
    };
    const endpoint = managedEndpoint();
    let fetched: { credential: ManagedCredential; now: Date; fetchedMonotonic: number };
    try {
      fetched = await this.fetchManagedCredential(scope);
    } catch (error) {
      await this.rebuildFromCachedCredential(endpoint);
      throw error;
    }
    const { credential, now, fetchedMonotonic } = fetched;
    if (await this.reuseCurrentManagedCredential(credential, now, fetchedMonotonic)) return;
    const candidate = await this.createAuthenticatedManagedClient(credential, endpoint);
    if (this.managedClosed) {
      candidate.destroy();
      throw new Error('managed_redis_closed');
    }
    if (!this.credentialFresh(credential, now.getTime())) {
      candidate.destroy();
      throw new Error('managed_redis_candidate_rejected');
    }
    const old = this.client;
    this.client = candidate;
    this.managedCredential = credential;
    this.managedFetchedAt = now.getTime();
    this.managedFetchedMonotonic = fetchedMonotonic;
    this.managedAckPending = true;
    this.redisReachable = true;
    this.restored();
    if (old) this.drainOldManagedClient(old);
    await this.acknowledgeApplied();
    logger.info('Managed Redis generation applied');
  }

  // Provider outage with no client (a Redis stall retired it): rebuild from the
  // last validated credential so serving continues until validUntil.
  private async rebuildFromCachedCredential(endpoint: {
    host: string;
    port: number;
  }): Promise<void> {
    const credential = this.managedCredential;
    if (this.client || !credential || !this.managedFresh()) return;
    let candidate: RedisClientType;
    try {
      candidate = await this.createAuthenticatedManagedClient(credential, endpoint);
    } catch {
      return;
    }
    if (
      this.managedClosed ||
      this.client ||
      this.managedCredential !== credential ||
      !this.managedFresh()
    ) {
      candidate.destroy();
      return;
    }
    this.client = candidate;
    this.redisReachable = true;
    this.restored();
    logger.warn('Managed Redis client rebuilt from the last validated generation', {
      code: 'managed_redis_cached_rebuild',
    });
  }

  private async managedDeadline<T>(promise: Promise<T>, ms: number, code: string): Promise<T> {
    let timer: NodeJS.Timeout | undefined;
    try {
      return await Promise.race([
        promise,
        new Promise<T>((_, reject) => {
          timer = setTimeout(() => reject(new Error(code)), ms);
          timer.unref();
        }),
      ]);
    } finally {
      if (timer) clearTimeout(timer);
    }
  }

  private async acknowledgeApplied(): Promise<void> {
    const credential = this.managedCredential;
    if (!credential) throw new Error('managed_redis_no_applied_generation');
    if (!this.managedFresh()) throw new Error('managed_redis_candidate_rejected');
    const acknowledge = this.managedOptions.acknowledge ?? acknowledgeMediaCredential;
    try {
      const client = this.client;
      if (!client) throw new Error('managed_redis_no_applied_generation');
      const username = await this.managedDeadline(
        client.aclWhoAmI(),
        MANAGED_AUTH_TIMEOUT_MS,
        'managed_redis_identity_timeout'
      );
      if (username !== credential.username) throw new Error('managed_redis_candidate_rejected');
      if (this.client !== client || this.managedCredential !== credential || !this.managedFresh())
        throw new Error('managed_redis_candidate_rejected');
      await acknowledge({
        version: 1,
        rotationId: credential.rotationId,
        service: 'media-plane',
        instanceId: hostname(),
        generation: credential.generation,
        username,
      });
      this.managedAckPending = false;
    } catch {
      throw new Error('managed_redis_ack_failed');
    }
  }

  private drainOldManagedClient(old: RedisClientType): void {
    this.oldManagedClients.add(old);
    const timeout = setTimeout(() => old.destroy(), MANAGED_DRAIN_MS);
    timeout.unref();
    void old
      .close()
      .catch(() => old.destroy())
      .finally(() => {
        clearTimeout(timeout);
        this.oldManagedClients.delete(old);
      });
  }

  /** Allocate a non-reused process generation before this colour serves joins. */
  async initializeMediaInstance(color: 'blue' | 'green'): Promise<MediaInstanceIdentity> {
    const client = this.currentClient();
    if (!client) throw new Error('Redis is not connected');
    const generationKey = 'media:generation';
    const seed = Math.floor(Date.now() / 1_000).toString();
    await this.withDeadline(client.set(generationKey, seed, { NX: true }));
    const freshClient = this.currentClient();
    if (!freshClient) throw new Error('Redis is not connected');
    const sequence = await this.withDeadline(freshClient.incr(generationKey));
    const generation = `${sequence}-${randomUUID()}`;
    const identity = {
      color,
      generation,
      instanceId: `${color}-${generation}`,
    };
    this.instanceIdentity = identity;
    this.acceptingNewRooms = await this.refreshAcceptingNewRooms();
    return identity;
  }

  /** Run D2's fail-closed, bounded owner/drain/occupancy read before gate creation. */
  async guardRoomCreation(roomId: string): Promise<void> {
    const identity = this.instanceIdentity;
    if (!identity) return; // Legacy single-colour mode; activation is opt-in.
    const client = this.currentClient();
    if (!client) return this.refuseOwner('room_owner_unavailable');

    let owner: string | null;
    let live: string | null;
    let draining: string | null;
    try {
      this.pendingOwnerReadCommands += 1;
      const command = client.mGet([
        `voice:room_owner:${roomId}`,
        `voice:room_live:${roomId}`,
        `media:colour_draining:${identity.color}`,
      ]);
      void command.then(
        () => {
          this.pendingOwnerReadCommands -= 1;
        },
        () => {
          this.pendingOwnerReadCommands -= 1;
        }
      );
      [owner, live, draining] = await this.withDeadline(command, () =>
        this.retireTimedOutClient(client)
      );
      this.redisReachable = true;
      this.acceptingNewRooms = draining === null;
    } catch {
      if (this.client === client) this.redisReachable = false;
      return this.refuseOwner('room_owner_unavailable');
    }

    if (draining !== null) return this.refuseOwner('room_draining');
    if (owner === identity.color) return;
    if (owner !== null) return this.refuseOwner('room_owner_mismatch');
    if (live !== null && live !== identity.instanceId) {
      return this.refuseOwner('room_owner_mismatch');
    }
  }

  pendingRoomOwnerReadCount(): number {
    return this.pendingOwnerReadCommands;
  }

  private refuseOwner(
    code: 'room_draining' | 'room_owner_mismatch' | 'room_owner_unavailable'
  ): never {
    this.ownerRefusalsTotal += 1;
    throw new RoomCreationRefusedError(code);
  }

  private retireTimedOutClient(client: RedisClientType): void {
    if (this.client !== client) return;
    this.client = null;
    this.redisReachable = false;
    this.acceptingNewRooms = false;
    this.degraded();
    client.destroy(); // Flushes already-written reads, which caller-side deadlines cannot cancel.
    void this.connect()
      .catch(() => {})
      // connect() throws during a provider outage even when it rebuilt the client.
      .then(() => (this.client ? this.refreshAcceptingNewRooms() : undefined))
      .catch(() => {});
  }

  private async withDeadline<T>(operation: Promise<T>, onTimeout?: () => void): Promise<T> {
    let timer: NodeJS.Timeout | undefined;
    try {
      return await Promise.race([
        operation,
        new Promise<T>((_, reject) => {
          timer = setTimeout(() => {
            onTimeout?.();
            reject(new Error('redis_operation_deadline'));
          }, CREATE_GUARD_TIMEOUT_MS);
          timer.unref();
        }),
      ]);
    } finally {
      if (timer) clearTimeout(timer);
    }
  }

  /** Refresh the drain projection; unreadable reads preserve the last good value. */
  async refreshAcceptingNewRooms(): Promise<boolean> {
    const identity = this.instanceIdentity;
    if (!identity) {
      this.acceptingNewRooms = true;
      if (config.redisCredentialMode === 'managed') {
        const client = this.currentClient();
        if (!client) return true;
        try {
          await this.withDeadline(client.ping(), () => this.retireTimedOutClient(client));
          this.redisReachable = true;
        } catch {
          this.redisReachable = false;
        }
      }
      return true;
    }
    const client = this.currentClient();
    if (!client) return this.acceptingNewRooms;
    try {
      const draining = await this.withDeadline(
        client.get(`media:colour_draining:${identity.color}`),
        () => this.retireTimedOutClient(client)
      );
      this.redisReachable = true;
      this.acceptingNewRooms = draining === null;
    } catch {
      this.redisReachable = false;
    }
    return this.acceptingNewRooms;
  }

  healthState(): MediaHealthState {
    const fresh = this.managedFresh();
    return {
      color: this.instanceIdentity?.color ?? null,
      generation: this.instanceIdentity?.generation ?? null,
      acceptingNewRooms: this.acceptingNewRooms && fresh,
      redisReachable: this.redisReachable && fresh,
      ownerRefusalsTotal: this.ownerRefusalsTotal,
    };
  }

  /** Refresh occupancy from the complete room map, including ring-only DMs. */
  async heartbeatRoomOccupancy(roomIds: readonly string[]): Promise<void> {
    const identity = this.instanceIdentity;
    const client = this.currentClient();
    if (!identity || !client || roomIds.length === 0) return;
    try {
      const transaction = client.multi();
      for (const roomId of roomIds) {
        transaction.eval(REFRESH_ROOM_LIVE_WITH_FENCE_SCRIPT, {
          keys: [`voice:room_owner:${roomId}`, `voice:room_live:${roomId}`],
          arguments: [identity.color, identity.instanceId, String(ROOM_LIVE_TTL_SECONDS)],
        });
      }
      if (this.currentClient() !== client) return;
      await this.withDeadline(transaction.exec(), () => this.retireTimedOutClient(client));
      this.redisCommandSucceeded();
    } catch (err) {
      this.redisCommandFailed();
      logger.error('Redis room occupancy heartbeat failed', {
        error: config.redisCredentialMode === 'managed' ? 'managed_redis_command_failed' : err,
      });
    }
  }

  private async markRoomLive(roomId: string, required = false): Promise<void> {
    const identity = this.instanceIdentity;
    const client = this.currentClient();
    if (!identity || !client) {
      if (required) this.refuseOwner('room_owner_unavailable');
      return;
    }
    if (required) {
      let result: unknown;
      try {
        result = await this.withDeadline(
          client.eval(CREATE_ROOM_LIVE_WITH_FENCE_SCRIPT, {
            keys: [
              `voice:room_owner:${roomId}`,
              `voice:room_live:${roomId}`,
              `media:colour_draining:${identity.color}`,
            ],
            arguments: [identity.color, identity.instanceId, String(ROOM_LIVE_TTL_SECONDS)],
          }),
          () => this.retireTimedOutClient(client)
        );
      } catch {
        this.redisCommandFailed();
        this.refuseOwner('room_owner_unavailable');
      }
      if (Number(result) === -1) this.refuseOwner('room_draining');
      if (Number(result) === -2) this.refuseOwner('room_owner_mismatch');
      if (Number(result) !== 1) this.refuseOwner('room_owner_unavailable');
      this.redisCommandSucceeded();
      return;
    }

    try {
      await this.withDeadline(
        client.eval(REFRESH_ROOM_LIVE_WITH_FENCE_SCRIPT, {
          keys: [`voice:room_owner:${roomId}`, `voice:room_live:${roomId}`],
          arguments: [identity.color, identity.instanceId, String(ROOM_LIVE_TTL_SECONDS)],
        }),
        () => this.retireTimedOutClient(client)
      );
      this.redisCommandSucceeded();
    } catch {
      this.redisCommandFailed();
    }
  }

  private async clearOwnedRoomLive(roomId: string): Promise<void> {
    const identity = this.instanceIdentity;
    const client = this.currentClient();
    if (!identity || !client) return;
    try {
      await this.withDeadline(
        client.eval(REMOVE_OWNED_ROOM_LIVE_SCRIPT, {
          keys: [`voice:room_live:${roomId}`],
          arguments: [identity.instanceId],
        })
      );
      this.redisCommandSucceeded();
    } catch {
      this.redisCommandFailed();
    }
  }

  /** Add a user to a voice room in Redis */
  async addParticipant(channelId: string, userId: string): Promise<void> {
    const client = this.currentClient();
    if (!client) {
      this.degraded();
      return;
    }

    try {
      const roomKey = `voice:room:${channelId}`;
      const userKey = `voice:user:${userId}`;

      await client
        .multi()
        .sAdd(roomKey, userId)
        .expire(roomKey, ROOM_TTL)
        .hSet(userKey, { channelId, joinedAt: new Date().toISOString() })
        .expire(userKey, USER_TTL)
        .exec();
      this.redisCommandSucceeded();
    } catch (err) {
      this.redisCommandFailed();
      logger.error('Redis addParticipant failed', {
        channelId,
        userId,
        error: config.redisCredentialMode === 'managed' ? 'managed_redis_command_failed' : err,
      });
    }
  }

  /** Remove a user from a voice room in Redis */
  async removeParticipant(channelId: string, userId: string): Promise<void> {
    const client = this.currentClient();
    if (!client) {
      this.degraded();
      return;
    }

    try {
      const roomKey = `voice:room:${channelId}`;
      const userKey = `voice:user:${userId}`;

      await client.multi().sRem(roomKey, userId).del(userKey).exec();
      this.redisCommandSucceeded();
    } catch (err) {
      this.redisCommandFailed();
      logger.error('Redis removeParticipant failed', {
        channelId,
        userId,
        error: config.redisCredentialMode === 'managed' ? 'managed_redis_command_failed' : err,
      });
    }
  }

  /** Remove all participants from a room (room empty) */
  async clearRoom(channelId: string): Promise<void> {
    const client = this.currentClient();
    if (!client) {
      this.degraded();
      return;
    }

    try {
      const roomKey = `voice:room:${channelId}`;

      // Get all users in room to clean up their keys
      const userIds = await client.sMembers(roomKey);
      const freshClient = this.currentClient();
      if (!freshClient) return;
      if (userIds.length > 0) {
        const userKeys = userIds.map((id) => `voice:user:${id}`);
        await freshClient.del([roomKey, ...userKeys]);
      } else {
        await freshClient.del(roomKey);
      }
      this.redisCommandSucceeded();
    } catch (err) {
      this.redisCommandFailed();
      logger.error('Redis clearRoom failed', {
        channelId,
        error: config.redisCredentialMode === 'managed' ? 'managed_redis_command_failed' : err,
      });
    }
  }

  /** Get all user IDs in a voice room */
  async getRoomParticipants(channelId: string): Promise<string[]> {
    const client = this.currentClient();
    if (!client) {
      this.degraded();
      return [];
    }

    try {
      const participants = await client.sMembers(`voice:room:${channelId}`);
      this.redisCommandSucceeded();
      return participants;
    } catch (err) {
      this.redisCommandFailed();
      logger.error('Redis getRoomParticipants failed', {
        channelId,
        error: config.redisCredentialMode === 'managed' ? 'managed_redis_command_failed' : err,
      });
      return [];
    }
  }

  /** Get which room a user is in */
  async getUserRoom(userId: string): Promise<string | null> {
    const client = this.currentClient();
    if (!client) {
      this.degraded();
      return null;
    }

    try {
      const room = (await client.hGet(`voice:user:${userId}`, 'channelId')) ?? null;
      this.redisCommandSucceeded();
      return room;
    } catch (err) {
      this.redisCommandFailed();
      logger.error('Redis getUserRoom failed', {
        userId,
        error: config.redisCredentialMode === 'managed' ? 'managed_redis_command_failed' : err,
      });
      return null;
    }
  }

  /**
   * Returns a RoomEventHandler that updates Redis state on room events.
   * Wire into RoomManager.onEvent().
   */
  createRoomEventHandler(): RoomEventHandler {
    return (event: RoomEvent) => {
      switch (event.type) {
        case 'room-created':
          return this.markRoomLive(event.roomId, config.roomPlacementEnabled);
        case 'user-joined':
          void this.markRoomLive(event.roomId);
          this.addParticipant(event.roomId, event.userId);
          break;
        case 'user-left':
          this.removeParticipant(event.roomId, event.userId);
          break;
        case 'room-empty':
          void this.clearOwnedRoomLive(event.roomId);
          this.clearRoom(event.roomId);
          break;
      }
      return undefined;
    };
  }

  async close(): Promise<void> {
    if (this.managedPoll) {
      clearInterval(this.managedPoll);
      this.managedPoll = null;
    }
    this.managedClosed = true;
    for (const old of this.oldManagedClients) old.destroy();
    this.oldManagedClients.clear();
    if (this.client) {
      if (config.redisCredentialMode === 'managed') {
        const client = this.client;
        await this.managedDeadline(client.close(), 5_000, 'managed_redis_close_timeout').catch(() =>
          client.destroy()
        );
      } else {
        await this.client.close();
      }
      logger.info('Redis connection closed');
    }
  }
}
