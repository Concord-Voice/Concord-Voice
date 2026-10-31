import { createHash, randomBytes, randomUUID } from 'node:crypto';
import {
  signHttpRequest, newRequestContext, verifyHttpResponse,
  type Binding, type Command, type Endpoint, type ExpectedResponse, type Target,
} from './voiceAuthProtocol.js';
import { logger } from './logger.js';
import { bearerAttestationHeaders, createServiceHopProofHeaders } from '../middleware/auth.js';
import type { VoiceEnforcementSession } from './voiceEnforcementSession.js';

const connectionBrand = Symbol();
export interface ConnectionHandle { readonly [connectionBrand]: true }
export interface VoiceAuthConnectionOptions {
  nodeBootId: string;
  secret: string;
  controlPlaneUrl: string;
  fetch?: typeof fetch;
  now?: () => number;
}
interface Completion {
  promise: Promise<void>;
  resolve: () => void;
  running?: Promise<void>;
  finished: boolean;
  failureLogged?: boolean;
  startedAt?: number;
}
interface ConnectionState {
  handle: ConnectionHandle;
  target: Target;
  userId: string;
  binding?: Binding;
  token: string;
  relayHeaders: () => Record<string, string>;
  body: Uint8Array;
  tokenDigest: string;
  bodyDigest: string;
  tokenExpiresAt: number;
  disconnect: () => void;
  state: 'provisional' | 'registered' | 'terminal' | 'released';
  registration?: Promise<void>;
  rooms: Map<string, RoomState>;
  terminal?: Completion;
}
interface RoomState {
  connection: ConnectionState;
  session: VoiceEnforcementSession;
  operations: Set<Promise<void>>;
  closeNative: () => Promise<void>;
  nativeClosed: boolean;
  attached: boolean;
  terminal?: Completion;
}
interface LegacyRoomState {
  session: VoiceEnforcementSession;
  release: () => Promise<void>;
  terminal: Completion;
  started: boolean;
}
const CAPACITY = 1000;
// Connection identities one account may hold, so a few accounts cannot fill CAPACITY
// with idle sockets. Twice the control plane's 8-per-user WebSocket admission, so
// reconnect overlap fits. ponytail: fixed cap; make it configurable if real multi-device use nears it.
const PER_USER_CONNECTIONS = 16;
// Three signed-release timeouts: an ordinary leave finishes well inside this.
const TERMINATION_STALL_MS = 15_000;
const bytes = (value: unknown): Uint8Array => Buffer.from(JSON.stringify(value), 'utf8');
const digest = (value: string | Uint8Array): string => createHash('sha256').update(value).digest('hex');
const sameTarget = (a: Target, b: Target): boolean =>
  a.connectionGeneration === b.connectionGeneration && a.nodeBootId === b.nodeBootId &&
  a.socketId === b.socketId && a.credentialEpoch === b.credentialEpoch;
const roomTuple = (session: VoiceEnforcementSession) => ({
  sessionGeneration: session.sessionGeneration, roomId: session.roomId, roomKind: session.roomKind,
});
function completion(): Completion {
  let resolve: () => void = () => undefined;
  const promise = new Promise<void>((done) => { resolve = done; });
  return { promise, resolve, finished: false };
}

/** Bound collection before the shared strict response decoder sees the bytes. */
export async function readVoiceAuthResponse(response: globalThis.Response) {
  const chunks: Uint8Array[] = [];
  let size = 0;
  const reader = response.body?.getReader();
  if (reader) {
    try {
      for (;;) {
        const part = await reader.read();
        if (part.done) break;
        size += part.value.byteLength;
        if (size > 4096) { void reader.cancel().catch(() => undefined); throw new Error('Voice response too large'); }
        chunks.push(part.value);
      }
    } finally { reader.releaseLock(); }
  }
  return { status: response.status,
    headers: [...response.headers].map(([name, value]) => ({ name, value })),
    body: Buffer.concat(chunks) };
}

/** One reservation ledger owns all connection AND room identities, including uncertainty. */
export class VoiceAuthConnections {
  private readonly handles = new WeakMap<ConnectionHandle, ConnectionState>();
  private readonly connections = new Map<string, ConnectionState>();
  private readonly rooms = new Map<string, RoomState>();
  private readonly roomReferences = new WeakMap<VoiceEnforcementSession, RoomState>();
  private readonly secret: Buffer;
  private readonly fetcher: typeof fetch;
  private readonly now: () => number;
  private retained = 0;
  private readonly userConnections = new Map<string, number>();
  private reservationHorizon: object = Object.freeze({});
  private readonly legacyRooms = new Map<string, LegacyRoomState>();
  private readonly legacyReferences = new WeakMap<VoiceEnforcementSession, LegacyRoomState>();
  private draining: Promise<void> | undefined;

  constructor(private readonly options: VoiceAuthConnectionOptions) {
    this.secret = Buffer.from(options.secret, 'utf8');
    this.fetcher = options.fetch ?? fetch;
    this.now = options.now ?? Date.now;
  }

  retainedIdentityCount(): number { return this.retained; }
  reservationGeneration(): object { return this.reservationHorizon; }

  /** Legacy drain shares capacity; its existing native+release adapter owns completion. */
  reserveLegacyRoom(session: VoiceEnforcementSession, release: () => Promise<void>): boolean {
    if (session.connectionGeneration || this.rooms.has(session.sessionGeneration) || this.ownsSession(session))
      throw new Error('Family room requires its private owner');
    const existing = this.legacyRooms.get(session.sessionGeneration) ?? this.legacyReferences.get(session);
    if (existing) {
      this.legacyRoom(session);
      return !existing.terminal.finished;
    }
    if (this.retained >= CAPACITY) throw new Error('Voice identity capacity exhausted');
    const room = { session: { ...session }, release, terminal: completion(), started: false };
    this.legacyRooms.set(session.sessionGeneration, room);
    this.legacyReferences.set(session, room);
    this.retained++;
    this.reservationHorizon = Object.freeze({});
    return true;
  }

  private legacyRoom(session: VoiceEnforcementSession): LegacyRoomState {
    const room = this.legacyRooms.get(session.sessionGeneration) ?? this.legacyReferences.get(session);
    if (!room || Object.entries(room.session).some(([key, value]) =>
      session[key as keyof VoiceEnforcementSession] !== value)) throw new Error('Unknown legacy room owner');
    return room;
  }

  releaseLegacyRoom(session: VoiceEnforcementSession): Promise<void> {
    const room = this.legacyRoom(session);
    room.started = true;
    this.start(room.terminal, async () => {
      await room.release();
      this.legacyRooms.delete(room.session.sessionGeneration);
      this.retained--;
    });
    return room.terminal.promise;
  }

  /**
   * Admission readiness: routine in-flight departures must not expire the boot
   * lease. Only a release that failed, or outlived its bounded budget, does.
   */
  hasStalledTermination(): boolean {
    const stalled = (task?: Completion): boolean => !!task && !task.finished &&
      (!!task.failureLogged || (task.startedAt !== undefined && this.now() - task.startedAt >= TERMINATION_STALL_MS));
    return [...this.legacyRooms.values()].some((room) => !room.started || stalled(room.terminal)) ||
      [...this.connections.values()].some((state) => stalled(state.terminal)) ||
      [...this.rooms.values()].some((room) => stalled(room.terminal));
  }

  hasPendingTermination(): boolean {
    return this.legacyRooms.size > 0 || [...this.connections.values()].some((state) => !!state.terminal) ||
      [...this.rooms.values()].some((room) => !!room.terminal);
  }

  private owned(handle: ConnectionHandle): ConnectionState {
    const state = this.handles.get(handle);
    if (!state) throw new Error('Unknown voice connection owner');
    return state;
  }

  snapshot(handle: ConnectionHandle) {
    const state = this.owned(handle);
    return {
      target: { ...state.target },
      binding: state.binding ? { ...state.binding, target: { ...state.target } } : undefined,
      state: state.state,
      tokenExpiresAt: state.tokenExpiresAt,
    };
  }

  reserve(input: {
    socketId: string; userId: string; credentialEpoch: string; token: string;
    tokenExpiresAt: number; disconnect: () => void; relayHeaders?: () => Record<string, string>;
  }): ConnectionHandle {
    if (this.retained >= CAPACITY) throw new Error('Voice identity capacity exhausted');
    const held = this.userConnections.get(input.userId) ?? 0;
    if (held >= PER_USER_CONNECTIONS) throw new Error('Voice identity capacity exhausted for user');
    if (!Number.isSafeInteger(input.tokenExpiresAt) || input.tokenExpiresAt <= this.now())
      throw new Error('Expired voice connection');
    const target: Target = {
      connectionGeneration: randomUUID(), nodeBootId: this.options.nodeBootId,
      socketId: input.socketId, credentialEpoch: input.credentialEpoch,
    };
    const body = bytes({ version: 1, ...target });
    // Validate the exact bytes before retaining or transmitting anything.
    const signed = signHttpRequest('connection_register', body, input.token, this.attempt(), this.secret);
    newRequestContext(signed.request, { userId: input.userId });
    const handle = Object.freeze({}) as ConnectionHandle;
    const state: ConnectionState = {
      handle, target, body, token: input.token, userId: input.userId,
      relayHeaders: input.relayHeaders ?? (() => ({})),
      tokenDigest: digest(input.token), bodyDigest: digest(body),
      tokenExpiresAt: input.tokenExpiresAt, disconnect: input.disconnect,
      state: 'provisional', rooms: new Map(),
    };
    this.retained++;
    this.userConnections.set(input.userId, held + 1);
    this.handles.set(handle, state);
    this.connections.set(target.connectionGeneration, state);
    this.reservationHorizon = Object.freeze({});
    return handle;
  }

  private attempt() {
    return { timestamp: Math.floor(this.now() / 1000).toString(), nonce: randomBytes(32).toString('hex') };
  }

  private async request(endpoint: Endpoint, body: Uint8Array, token: string, expected: ExpectedResponse,
    relayHeaders: () => Record<string, string> = () => ({})) {
    const { wire, request } = signHttpRequest(endpoint, body, token, this.attempt(), this.secret);
    const context = newRequestContext(request, expected);
    const headers: Record<string, string> = { 'Content-Type': 'application/json' };
    for (const header of wire.headers) headers[header.name] = header.value;
    // Only bearer hops reach RequireAttestation; the relay never rides a service-signed one.
    if (token) {
      Object.assign(headers, bearerAttestationHeaders(relayHeaders(), token),
        createServiceHopProofHeaders('POST', wire.path, token));
    }
    const response = await this.fetcher(`${this.options.controlPlaneUrl}${wire.path}`, {
      method: 'POST', headers, body: Buffer.from(wire.body), signal: AbortSignal.timeout(5_000),
    });
    return verifyHttpResponse(endpoint, await readVoiceAuthResponse(response),
      context, this.secret, BigInt(Math.floor(this.now() / 1000)));
  }

  register(handle: ConnectionHandle): Promise<void> {
    const state = this.owned(handle);
    if (state.state === 'terminal' || state.state === 'released')
      return Promise.reject(new Error('Terminal voice connection'));
    if (state.registration) return state.registration;
    state.registration = this.registerOwned(state);
    return state.registration;
  }

  private async registerOwned(state: ConnectionState): Promise<void> {
    const result = (await this.request('connection_register', state.body, state.token, {
      userId: state.userId,
    }, state.relayHeaders)).registration();
    if (!result) throw new Error('Missing voice binding');
    // The binding remains useful to terminal cleanup; it never grants revival.
    state.binding = { userId: result.userId, familyId: result.familyId, target: { ...result.target } };
    if (state.state !== 'provisional' || this.now() >= state.tokenExpiresAt)
      throw new Error('Terminal voice connection');
    state.state = 'registered';
  }

  assertProductive(handle: ConnectionHandle): void {
    const state = this.owned(handle);
    if (state.state !== 'registered' || this.now() >= state.tokenExpiresAt ||
        this.connections.get(state.target.connectionGeneration) !== state)
      throw new Error('Voice connection unavailable');
  }

  updateDeadline(handle: ConnectionHandle, deadline: number): void {
    this.assertProductive(handle);
    const state = this.owned(handle);
    if (!Number.isSafeInteger(deadline) || deadline <= state.tokenExpiresAt || deadline <= this.now())
      throw new Error('Invalid voice renewal deadline');
    state.tokenExpiresAt = deadline;
  }

  reserveRoom(handle: ConnectionHandle, session: VoiceEnforcementSession, closeNative: () => Promise<void>): void {
    this.assertProductive(handle);
    const state = this.owned(handle);
    if (this.retained >= CAPACITY) throw new Error('Voice identity capacity exhausted');
    if (this.rooms.has(session.sessionGeneration) || this.legacyRooms.has(session.sessionGeneration) ||
        session.userId !== state.userId || session.socketId !== state.target.socketId ||
        session.nodeBootId !== state.target.nodeBootId || session.credentialEpoch !== state.target.credentialEpoch ||
        (session.connectionGeneration !== undefined && session.connectionGeneration !== state.target.connectionGeneration))
      throw new Error('Invalid voice room owner');
    const binding = state.binding;
    if (!binding) throw new Error('Missing voice binding');
    const value = { schema: 'concord.auth-room-binding', version: 1, mode: 'attach',
      ...state.target, ...roomTuple(session) };
    const signed = signHttpRequest('room_register', bytes(value), state.token, this.attempt(), this.secret);
    newRequestContext(signed.request, { binding });
    const room: RoomState = { connection: state, session: { ...session }, operations: new Set(),
      closeNative, nativeClosed: false, attached: false };
    this.retained++;
    state.rooms.set(session.sessionGeneration, room);
    this.rooms.set(session.sessionGeneration, room);
    this.roomReferences.set(session, room);
    this.reservationHorizon = Object.freeze({});
  }

  private roomOwned(session: VoiceEnforcementSession): RoomState {
    const room = this.rooms.get(session.sessionGeneration) ?? this.roomReferences.get(session);
    if (!room ||
        (session.connectionGeneration !== undefined &&
          session.connectionGeneration !== room.connection.target.connectionGeneration) ||
        Object.entries(room.session).some(([key, value]) =>
          key !== 'connectionGeneration' && session[key as keyof VoiceEnforcementSession] !== value))
      throw new Error('Unknown voice room owner');
    return room;
  }

  /** Compatibility commands lack the new parent marker; ALL old exact fields must match. */
  ownsSession(session: VoiceEnforcementSession): boolean {
    try { this.roomOwned(session); return true; }
    catch { return false; }
  }

  assertRoomProductive(sessionGeneration: string): void {
    const room = this.rooms.get(sessionGeneration);
    if (!room || room.terminal) throw new Error('Terminal voice room');
    this.assertProductive(room.connection.handle);
  }

  trackRoomOperation<T>(session: VoiceEnforcementSession, effect: () => Promise<T>): Promise<T> {
    const room = this.roomOwned(session);
    this.assertRoomProductive(session.sessionGeneration);
    let done: () => void = () => undefined;
    const owned = new Promise<void>((resolve) => { done = resolve; });
    room.operations.add(owned);
    const run = async () => {
      try {
        const result = await effect();
        this.assertRoomProductive(session.sessionGeneration);
        return result;
      } finally { room.operations.delete(owned); done(); }
    };
    return run();
  }

  async attachRoom(handle: ConnectionHandle, session: VoiceEnforcementSession, token: string): Promise<void> {
    const room = this.roomOwned(session);
    if (room.connection !== this.owned(handle)) throw new Error('Voice room parent mismatch');
    await this.roomRequest(room, token, 'attach');
    this.assertRoomProductive(session.sessionGeneration);
    room.attached = true;
  }

  async renewRoom(handle: ConnectionHandle, session: VoiceEnforcementSession, token: string,
    isCurrent: () => boolean): Promise<void> {
    const room = this.roomOwned(session);
    if (room.connection !== this.owned(handle) || !room.attached || !isCurrent())
      throw new Error('No admitted voice session');
    this.assertRoomProductive(session.sessionGeneration);
    await this.roomRequest(room, token, 'renew');
    this.assertRoomProductive(session.sessionGeneration);
    if (!isCurrent()) throw new Error('Voice session changed during renewal');
  }

  private roomRequest(room: RoomState, token: string, mode: 'attach' | 'renew'): Promise<void> {
    const state = room.connection;
    const binding = state.binding;
    if (!binding) return Promise.reject(new Error('Missing voice binding'));
    return this.trackRoomOperation(room.session, async () => {
      const result = (await this.request('room_register', bytes({
        schema: 'concord.auth-room-binding', version: 1, mode,
        ...state.target, ...roomTuple(room.session),
      }), token, { binding }, state.relayHeaders)).roomRegistration();
      if (!result) throw new Error('Missing voice room binding');
    });
  }

  /** Compatibility adapters resolve an already reserved exact owner, never make one. */
  handleForSession(session: VoiceEnforcementSession): ConnectionHandle {
    return this.roomOwned(session).connection.handle;
  }

  releaseRoom(session: VoiceEnforcementSession): Promise<void> {
    const room = this.roomOwned(session);
    room.terminal ??= completion();
    this.start(room.terminal, () => this.closeRoom(room));
    return room.terminal.promise;
  }

  private async closeRoom(room: RoomState): Promise<void> {
    await Promise.all(room.operations);
    if (!room.nativeClosed) {
      await room.closeNative();
      room.nativeClosed = true;
    }
    const binding = room.connection.binding;
    if (!binding) throw new Error('Missing voice binding');
    await this.request('room_release', bytes({ schema: 'concord.auth-room-binding', version: 1,
      binding, room: roomTuple(room.session) }), '', { binding });
    this.rooms.delete(room.session.sessionGeneration);
    room.connection.rooms.delete(room.session.sessionGeneration);
    this.retained--;
  }

  terminate(handle: ConnectionHandle): Promise<void> {
    const state = this.owned(handle);
    if (!state.terminal) {
      state.state = 'terminal';
      state.terminal = completion();
      // Fence EVERY child before the first native/remote await or disconnect callback.
      for (const room of state.rooms.values()) room.terminal ??= completion();
      try { state.disconnect(); } catch { /* Cleanup still owns this generation. */ }
    }
    // Repeated exact commands retry failed child release attempts on their
    // existing tasks even while the connection task is waiting for those rooms.
    for (const room of state.rooms.values()) {
      if (room.terminal) this.start(room.terminal, () => this.closeRoom(room));
    }
    this.start(state.terminal, () => this.closeConnection(state));
    return state.terminal.promise;
  }

  private async closeConnection(state: ConnectionState): Promise<void> {
    if (state.registration) await state.registration.catch(() => undefined);
    const closing: Promise<unknown>[] = [];
    for (const room of state.rooms.values()) {
      if (!room.terminal) throw new Error('Unfenced voice room');
      this.start(room.terminal, () => this.closeRoom(room));
      closing.push(room.terminal.promise);
    }
    await Promise.all(closing);
    // Witness is private retained ownership: no child can be installed after
    // the fence, every owned create settled, every exact room has signed closure.
    if (state.rooms.size !== 0) throw new Error('Voice native ownership outstanding');
    if (state.binding) {
      await this.request('connection_release', bytes({ version: 1, ...state.binding }), '', { binding: state.binding });
    } else {
      await this.request('provisional_close', bytes({
        schema: 'concord.auth-connection-provisional-close', version: 1, target: state.target,
        registrationTokenDigest: state.tokenDigest, registrationBodyDigest: state.bodyDigest,
      }), '', {});
    }
    state.state = 'released';
    state.token = '';
    this.connections.delete(state.target.connectionGeneration);
    this.retained--;
    const held = (this.userConnections.get(state.userId) ?? 1) - 1;
    if (held > 0) this.userConnections.set(state.userId, held);
    else this.userConnections.delete(state.userId);
  }

  private start(task: Completion, effect: () => Promise<void>): void {
    if (task.running || task.finished) return;
    task.startedAt ??= this.now();
    task.running = Promise.resolve().then(effect).then(() => {
      task.finished = true;
      task.resolve();
    }, () => {
      // Unknown completion retains the SAME promise and identity for retry.
      // Log once per task: retries re-run this handler until success.
      if (!task.failureLogged) {
        task.failureLogged = true;
        logger.error('Voice terminal release failed; will retry');
      }
    }).finally(() => { task.running = undefined; });
  }

  retryPending(): Promise<void> {
    for (const room of this.legacyRooms.values()) {
      if (room.started) this.releaseLegacyRoom(room.session);
    }
    for (const room of this.rooms.values()) {
      if (room.terminal) this.start(room.terminal, () => this.closeRoom(room));
    }
    for (const state of this.connections.values()) {
      if (state.terminal) this.start(state.terminal, () => this.closeConnection(state));
    }
    return Promise.resolve();
  }

  terminateAll(): Promise<void> {
    if (this.draining) return this.draining;
    const tasks = [...this.connections.values()].map((state) => this.terminate(state.handle));
    for (const room of this.legacyRooms.values()) tasks.push(this.releaseLegacyRoom(room.session));
    this.draining = Promise.all(tasks).then(() => undefined).finally(() => { this.draining = undefined; });
    return this.draining;
  }

  fenceAndJoin(command: Command): Promise<void> | undefined {
    const value = command.auth() ?? command.epoch();
    if (!value) return undefined;
    const state = this.connections.get(value.target.connectionGeneration);
    if (!state || !sameTarget(state.target, value.target) || value.userId !== state.userId ||
        (state.binding !== undefined && state.binding.familyId !== value.familyId)) return undefined;
    // Fence conservatively without granting ACK authority from an untrusted
    // family assertion. A later retry may use a CP-verified binding.
    const task = this.terminate(state.handle);
    return state.binding ? task : undefined;
  }
}
