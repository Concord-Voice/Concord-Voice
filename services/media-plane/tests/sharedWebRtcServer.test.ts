import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { EventEmitter } from 'node:events';
import { readFileSync } from 'node:fs';
import { Script } from 'node:vm';
import ts from 'typescript';
import * as protocol from '../src/lib/voiceAuthProtocol.js';
import { VoiceEnforcementLease, VoiceEnforcementHealth } from '../src/lib/voiceEnforcementLease.js';
import type { Router } from 'mediasoup/types';
import { createMockTransport, createMockWorker, createMockProducer, createRtpParameters } from './mocks/mediasoup.js';
import './mocks/logger.js';

const { createWorker, config } = vi.hoisted(() => ({
  createWorker: vi.fn(),
  config: {
    freeVideoPublisherCap: 8,
    freeScreenProducerCap: 1,
    freeAudioLastN: 8,
    audioLastNHoldMs: 2500,
    audioLevelObserver: { maxEntries: 1, threshold: -60, interval: 300 },
    mediasoup: {
      numWorkers: 3,
      worker: { logLevel: 'warn', logTags: [], rtcMinPort: 45000, rtcMaxPort: 46000 },
      router: { mediaCodecs: [{ kind: 'audio', mimeType: 'audio/opus', clockRate: 48000, channels: 2 }] },
      webRtcServer: {
        enabled: true,
        ports: [42000, 42001, 42002],
        listenIp: '0.0.0.0',
        announcedAddress: 'media.example.test',
      },
      webRtcTransport: {
        listenIps: [{ ip: '0.0.0.0', announcedIp: 'media.example.test' }],
        enableUdp: true,
        enableTcp: false,
        preferUdp: true,
        initialAvailableOutgoingBitrate: 1_000_000,
        maxIncomingBitrate: 50_000_000,
      },
    },
  },
}));
vi.mock('mediasoup', () => ({ createWorker }));
vi.mock('@/config/index.js', () => ({ config }));

import { MediasoupService } from '../src/lib/mediasoup.js';
import { RoomManager, SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION } from '../src/lib/roomManager.js';

function listener(id: string) {
  const observer = new EventEmitter();
  const events = new EventEmitter();
  const server = {
    id,
    closed: false,
    observer,
    on: events.on.bind(events),
    close: vi.fn(() => {
      server.closed = true;
      observer.emit('close');
    }),
  };
  return server;
}

function worker(index: number) {
  const server = listener(`listener-${index}`);
  const native = createMockWorker({ pid: 6100 + index });
  const originalClose = native.close.getMockImplementation();
  const result = Object.assign(native, {
    server,
    createWebRtcServer: vi.fn(async () => server),
    completeSubprocessClose: () => {
      native.subprocessClosed = true;
      native._emit('subprocessclose');
    },
  });
  result.close.mockImplementation(() => {
    server.close();
    originalClose?.();
  });
  return result;
}

function observe<T>(promise: Promise<T>) {
  const result = { status: 'pending' as 'pending' | 'fulfilled' | 'rejected', promise };
  void promise.then(
    () => { result.status = 'fulfilled'; },
    () => { result.status = 'rejected'; }
  );
  return result;
}

async function turn() {
  await new Promise<void>((resolve) => setImmediate(resolve));
}

describe('shared worker WebRTC listeners', () => {
  let service: MediasoupService;
  let workers: ReturnType<typeof worker>[];

  beforeEach(() => {
    createWorker.mockReset();
    config.mediasoup.webRtcServer.enabled = true;
    config.mediasoup.webRtcTransport.enableTcp = false;
    workers = [worker(0), worker(1), worker(2)];
    workers.forEach((current) => createWorker.mockResolvedValueOnce(current));
    service = new MediasoupService();
    service.setRoomConsumerCounts(() => new Map());
  });
  afterEach(() => { vi.useRealTimers(); });

  it.each([false, true])('binds exactly one listener per worker with TCP=%s', async (tcp) => {
    config.mediasoup.webRtcTransport.enableTcp = tcp;
    await service.init();
    expect(createWorker).toHaveBeenCalledTimes(3);
    for (const [index, current] of workers.entries()) {
      const udp = {
        protocol: 'udp', ip: '0.0.0.0', announcedAddress: 'media.example.test', port: 42000 + index,
      };
      expect(current.createWebRtcServer).toHaveBeenCalledExactlyOnceWith({
        listenInfos: tcp ? [udp, { ...udp, protocol: 'tcp' }] : [udp],
      });
    }
  });

  it('becomes ready only after every configured listener has bound', async () => {
    let release!: (server: ReturnType<typeof listener>) => void;
    workers[2].createWebRtcServer.mockImplementationOnce(() => new Promise((resolve) => { release = resolve; }));
    expect(service.isReady()).toBe(false);
    const initializing = service.init();
    await turn();
    expect(workers[2].createWebRtcServer).toHaveBeenCalledOnce();
    expect(service.isReady()).toBe(false);
    release(workers[2].server);
    await initializing;
    expect(service.isReady()).toBe(true);
  });

  it.each([false, true])('uses the assigned worker listener at the actual transport boundary with TCP=%s', async (tcp) => {
    config.mediasoup.webRtcTransport.enableTcp = tcp;
    const manager = new RoomManager(service);
    service.setRoomConsumerCounts(() => manager.getRoomConsumerCounts());
    await service.init();
    // Four sequential rooms cover all workers and reuse worker zero. A lookup
    // that always returns the first server fails on rooms one and two.
    for (let index = 0; index < 4; index++) {
      const roomId = `room-${index}`;
      await manager.joinRoom(roomId, 'user', `socket-${index}`, { username: 'test-user' }, undefined, {
        mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
      });
      const router = manager.getRoom(roomId)!.router;
      const assigned = workers[index % 3];
      for (const direction of ['send', 'recv'] as const) {
        await manager.createTransport(roomId, 'user', direction);
      }
      expect(router.createWebRtcTransport).toHaveBeenCalledTimes(2);
      for (const call of vi.mocked(router.createWebRtcTransport).mock.calls) {
        expect(call[0]).toEqual({
          webRtcServer: assigned.server,
          enableUdp: true,
          enableTcp: tcp,
          preferUdp: true,
          initialAvailableOutgoingBitrate: 1_000_000,
        });
        expect(call[0]).not.toHaveProperty('listenIps');
        expect(call[0]).not.toHaveProperty('listenInfos');
      }
    }
  });

  it('keeps the legacy transport options and creates no shared listeners when disabled', async () => {
    config.mediasoup.webRtcServer.enabled = false;
    const manager = new RoomManager(service);
    service.setRoomConsumerCounts(() => manager.getRoomConsumerCounts());
    await service.init();
    await manager.joinRoom('legacy-room', 'user', 'socket', { username: 'test-user' }, undefined, {
      mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
    });
    const router = manager.getRoom('legacy-room')!.router;
    await manager.createTransport('legacy-room', 'user', 'send');
    expect(router.createWebRtcTransport).toHaveBeenCalledExactlyOnceWith({
      listenIps: config.mediasoup.webRtcTransport.listenIps,
      enableUdp: true,
      enableTcp: false,
      preferUdp: true,
      initialAvailableOutgoingBitrate: 1_000_000,
    });
    expect(workers.every((current) => current.createWebRtcServer.mock.calls.length === 0)).toBe(true);
    expect(service.getWebRtcServer(router)).toBeUndefined();
    expect(service.isReady()).toBe(true);
  });

  it('retains exact router ownership after cache removal and refuses a foreign router', async () => {
    await service.init();
    const first = await service.getOrCreateRouter('room-a');
    const second = await service.getOrCreateRouter('room-b');
    service.removeRouter('room-a');
    expect(service.getWebRtcServer(first)).toBe(workers[0].server);
    expect(service.getWebRtcServer(second)).toBe(workers[1].server);
    expect(() => service.getWebRtcServer({ id: first.id } as Router)).toThrow();
  });

  it.each([0, 1, 2])('cleans up every created worker after listener %s fails to bind', async (index) => {
    const failure = new Error('test listener bind failed');
    workers[index].createWebRtcServer.mockRejectedValueOnce(failure);
    await expect(service.init()).rejects.toBe(failure);
    expect(createWorker).toHaveBeenCalledTimes(index + 1);
    expect(workers.slice(0, index + 1).every((current) => current.close.mock.calls.length === 1)).toBe(true);
    expect(workers.slice(index + 1).every((current) => current.close.mock.calls.length === 0)).toBe(true);
    expect(service.isReady()).toBe(false);
    expect(service.getWorkerCount()).toBe(0);
  });

  it('cleans up earlier listeners if a later worker cannot start', async () => {
    const failure = new Error('test worker spawn failed');
    createWorker.mockReset().mockResolvedValueOnce(workers[0]).mockRejectedValueOnce(failure);
    await expect(service.init()).rejects.toBe(failure);
    expect(workers[0].close).toHaveBeenCalledOnce();
    expect(service.isReady()).toBe(false);
  });

  it('awaits actual subprocess closure during partial startup cleanup', async () => {
    const failure = new Error('test listener bind failed');
    workers[1].createWebRtcServer.mockRejectedValueOnce(failure);
    workers[1].close.mockImplementation(() => { workers[1].closed = true; });
    const initializing = observe(service.init());
    await turn();
    expect(workers[1].close).toHaveBeenCalledOnce();
    expect(initializing.status).toBe('pending');
    workers[1].completeSubprocessClose();
    await expect(initializing.promise).rejects.toBe(failure);
    expect(initializing.status).toBe('rejected');
  });

  it('refuses startup when an already bound listener closes while another worker is starting', async () => {
    let release!: (server: ReturnType<typeof listener>) => void;
    workers[2].createWebRtcServer.mockImplementationOnce(() => new Promise((resolve) => { release = resolve; }));
    const initializing = observe(service.init());
    await turn();
    expect(workers[2].createWebRtcServer).toHaveBeenCalledOnce();
    workers[0].server.close();
    release(workers[2].server);
    await expect(initializing.promise).rejects.toThrow();
    expect(workers.every((current) => current.close.mock.calls.length === 1)).toBe(true);
    expect(service.isReady()).toBe(false);
  });

  it('closes a router that finishes creation after its required listener is lost', async () => {
    await service.init();
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const createRouter = workers[0].createRouter.getMockImplementation()!;
    let capturedRouter: Awaited<ReturnType<typeof createRouter>> | undefined;
    workers[0].createRouter.mockImplementationOnce(async () => {
      capturedRouter = await createRouter();
      await gate;
      return capturedRouter;
    });
    const creating = observe(service.getOrCreateRouter('racing-room'));
    await turn();
    expect(capturedRouter).toBeDefined();
    workers[0].server.close();
    release();
    await expect(creating.promise).rejects.toThrow();
    expect(capturedRouter?.close).toHaveBeenCalledOnce();
    expect(service.isReady()).toBe(false);
    expect(createWorker).toHaveBeenCalledTimes(3);
  });

  it('bounds failed-startup native cleanup at 35 seconds without publishing an unbound worker', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'performance'] });
    workers[0].createWebRtcServer.mockRejectedValueOnce(new Error('test listener bind failed'));
    workers[0].close.mockImplementation(() => { workers[0].closed = true; });
    const initializing = observe(service.init());
    await turn();
    expect(workers[0].close).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(34_999);
    expect(initializing.status).toBe('pending');
    await vi.advanceTimersByTimeAsync(1);
    await expect(initializing.promise).rejects.toThrow();
    expect(service.isReady()).toBe(false);
    expect(service.getWorkerCount()).toBe(0);
    expect(service.getWorkers()).toEqual([]);
    workers[0].completeSubprocessClose();
    await expect(service.close(performance.now() + 1000)).resolves.toBeUndefined();
  });

  it('latches listener loss unhealthy and refuses further allocation without replacement', async () => {
    const manager = new RoomManager(service);
    service.setRoomConsumerCounts(() => manager.getRoomConsumerCounts());
    await service.init();
    await manager.joinRoom('room', 'user', 'socket', { username: 'test-user' }, undefined, {
      mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
    });
    const router = manager.getRoom('room')!.router;
    expect(service.isReady()).toBe(true);
    workers[0].server.close();
    expect(service.isReady()).toBe(false);
    expect(() => service.getWebRtcServer(router)).toThrow();
    await expect(manager.createTransport('room', 'user', 'recv')).rejects.toThrow();
    await expect(service.getOrCreateRouter('new-room')).rejects.toThrow();
    expect(router.createWebRtcTransport).not.toHaveBeenCalled();
    expect(createWorker).toHaveBeenCalledTimes(3);
    expect(workers.every((current) => current.createWebRtcServer.mock.calls.length === 1)).toBe(true);
  });

  it('does not treat listener closure as native worker shutdown proof', async () => {
    await service.init();
    workers.forEach((current) => current.close.mockImplementation(() => { current.closed = true; }));
    const closing = observe(service.close(performance.now() + 1000));
    await turn();
    expect(service.isReady()).toBe(false);
    expect(workers.every((current) => current.server.close.mock.calls.length === 1)).toBe(true);
    expect(closing.status).toBe('pending');
    workers[0].completeSubprocessClose();
    workers[1].completeSubprocessClose();
    await turn();
    expect(closing.status).toBe('pending');
    workers[2].completeSubprocessClose();
    await closing.promise;
    expect(service.getWorkerCount()).toBe(0);
  });

  it.each((['send', 'recv'] as const).flatMap((direction) =>
    ['native-create', 'bitrate'].flatMap((boundary) =>
      ['listener', 'transport'].map((fault) => ({ direction, boundary, fault }))
    )
  ))('rejects $direction creation after $fault loss during the $boundary await', async ({ direction, boundary, fault }) => {
    const manager = new RoomManager(service);
    service.setRoomConsumerCounts(() => manager.getRoomConsumerCounts());
    await service.init();
    await manager.joinRoom('await-room', 'user', 'socket', { username: 'test-user' }, undefined, {
      mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
    });
    const router = manager.getRoom('await-room')!.router;
    const participant = manager.getParticipant('await-room', 'user')!;
    const transport = createMockTransport();
    transport.close.mockImplementation(() => { transport.closed = true; });
    // Native forwarding survives the JS closed flag until an authoritative
    // dump proves absence. This makes a dropped termination record observable.
    const nativeIds = new Set([transport.id]);
    Object.assign(router, {
      dump: vi.fn(async () => ({ id: router.id, transportIds: [...nativeIds] })),
    });
    let reached!: () => void;
    const pending = new Promise<void>((resolve) => { reached = resolve; });
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    vi.mocked(router.createWebRtcTransport).mockImplementationOnce(async () => {
      if (boundary === 'native-create') { reached(); await gate; }
      return transport as never;
    });
    if (boundary === 'bitrate') {
      transport.setMaxIncomingBitrate.mockImplementationOnce(async () => { reached(); await gate; });
    }
    const creating = observe(manager.createTransport('await-room', 'user', direction));
    await pending;
    // Isolate the two guards: a stale pending reply may still carry an open
    // handle after listener loss; a transport may close with healthy listeners.
    // Combining both in every fixture would let either missing guard pass.
    if (fault === 'listener') workers[0].server.close();
    else transport.closed = true;
    release();
    await expect(creating.promise).rejects.toThrow();
    expect(service.isReady()).toBe(fault !== 'listener');
    expect(transport.closed).toBe(true);
    expect(manager.getParticipant('await-room', 'user')).toBe(participant);
    expect(participant.sendTransport).toBeNull();
    expect(participant.recvTransports.size).toBe(0);
    expect(participant.pendingRecvTransports).toBe(0);
    await expect(manager.awaitNativeTermination('await-room', 'user', 'socket')).rejects.toThrow();
    nativeIds.clear();
    await expect(manager.awaitNativeTermination('await-room', 'user', 'socket')).resolves.toBeUndefined();
  });

  it('retains native absence proof after an orphan producer returns during legacy unbound leave', async () => {
    const manager = new RoomManager(service);
    service.setRoomConsumerCounts(() => manager.getRoomConsumerCounts());
    await service.init();
    for (const user of ['leaver', 'keeper']) {
      await manager.joinRoom('orphan-room', user, `${user}-socket`, { username: 'native-fixture' }, undefined,
        { mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION });
    }
    const router = manager.getRoom('orphan-room')!.router;
    const transport = createMockTransport();
    transport.close.mockImplementation(() => { transport.closed = true; });
    const nativeIds = new Set([transport.id]);
    Object.assign(router, { dump: vi.fn(async () => ({ id: router.id, transportIds: [...nativeIds] })) });
    vi.mocked(router.createWebRtcTransport).mockResolvedValueOnce(transport as never);
    await manager.createTransport('orphan-room', 'leaver', 'send');
    const observer = new EventEmitter();
    const orphan = createMockProducer({ observer });
    const closed = vi.fn();
    observer.on('close', closed);
    orphan.close.mockImplementation(() => { orphan.closed = true; observer.emit('close'); });
    let returnProducer!: (value: typeof orphan) => void;
    transport.produce.mockImplementationOnce(() => new Promise(resolve => { returnProducer = resolve; }));
    const producing = manager.produce('orphan-room', 'leaver', transport.id, 'audio', createRtpParameters() as never, 'mic');
    const refusal = expect(producing).rejects.toThrow('Participant left during producer creation');
    await turn();
    const leaving = observe(manager.leaveRoom('orphan-room', 'leaver'));
    const absenceRefusal = expect(leaving.promise).rejects.toThrow('Native media termination unavailable');
    await turn();
    expect(leaving.status).toBe('pending');
    expect(transport.closed).toBe(true);
    returnProducer(orphan);
    await refusal;
    await absenceRefusal;
    expect(orphan.closed).toBe(true);
    expect(closed).toHaveBeenCalledOnce();
    expect(manager.getParticipant('orphan-room', 'keeper')).toBeDefined();
    expect(workers[0].dump).toHaveBeenCalled();
    expect(router.dump).toHaveBeenCalled();
    nativeIds.clear();
    await expect(manager.awaitNativeTermination('orphan-room', 'leaver', 'leaver-socket')).resolves.toBeUndefined();
    await manager.leaveRoom('orphan-room', 'keeper');
  });

  it('still closes every worker when a listener close request throws', async () => {
    await service.init();
    workers[0].server.close.mockImplementationOnce(() => { throw new Error('test listener close failed'); });
    await expect(service.close(performance.now() + 1000)).resolves.toBeUndefined();
    expect(workers.every((current) => current.close.mock.calls.length === 1)).toBe(true);
    expect(workers.every((current) => current.subprocessClosed)).toBe(true);
    expect(service.getWorkerCount()).toBe(0);
  });

  it('continues after a worker close request throws and still requires its subprocess proof', async () => {
    await service.init();
    workers[0].close.mockImplementationOnce(() => { throw new Error('test worker close failed'); });
    const closing = observe(service.close(performance.now() + 1000));
    await turn();
    expect(workers.every((current) => current.close.mock.calls.length === 1)).toBe(true);
    expect(workers[0].subprocessClosed).toBe(false);
    expect(closing.status).toBe('pending');
    workers[0].completeSubprocessClose();
    await closing.promise;
    expect(service.getWorkerCount()).toBe(0);
  });

  it('preserves the mandatory worker-death exit in shared mode', async () => {
    await service.init();
    workers[1]._emit('died');
    expect(process.exit).toHaveBeenCalledWith(1);
  });
});

// Execute the actual route and availability consumer without starting main.
// AST selection avoids assertions about a particular implementation spelling.
describe('shared listener readiness wiring', () => {
  it('consumes listener readiness and protocol-4 health in readyz and admission availability', async () => {
    createWorker.mockReset();
    const workers = [worker(0), worker(1), worker(2)];
    workers.forEach((current) => createWorker.mockResolvedValueOnce(current));
    const service = new MediasoupService();
    service.setRoomConsumerCounts(() => new Map());
    await service.init();
    const source = readFileSync(new URL('../src/index.ts', import.meta.url), 'utf8');
    const ast = ts.createSourceFile('index.ts', source, ts.ScriptTarget.Latest, true);
    const selected: string[] = [];
    const visit = (node: ts.Node) => {
      if (ts.isFunctionDeclaration(node) && node.name?.text === 'enforcementAvailable') selected.push(node.getText(ast));
      if (ts.isExpressionStatement(node) && ts.isCallExpression(node.expression)) {
        const call = node.expression;
        if (ts.isPropertyAccessExpression(call.expression) && call.expression.expression.getText(ast) === 'app' &&
            call.expression.name.text === 'get' && ts.isStringLiteral(call.arguments[0]) && call.arguments[0].text === '/readyz') {
          selected.push(node.getText(ast));
        }
      }
      ts.forEachChild(node, visit);
    };
    visit(ast);
    expect(selected).toHaveLength(2);
    const lease = new VoiceEnforcementLease(() => 0);
    lease.renew();
    const health = new VoiceEnforcementHealth({
      lease, now: () => 0, nodeBootId: '11111111-1111-4111-8111-111111111111',
      localReady: () => service.isReady(), transportReady: () => true,
      poll: async () => {
        const key = Buffer.from(['readiness', 'fixture', 'only'].join('-'));
        const timestamp = String(Math.floor(Date.now() / 1000));
        const nonce = 'b'.repeat(64);
        const body = Buffer.from(JSON.stringify({ version: 1, nodeBootId: '11111111-1111-4111-8111-111111111111', nonce }));
        const { request } = protocol.signHttpRequest('health_bootstrap', body, '', { timestamp, nonce }, key);
        protocol.verifyHttpRequest(request, key, BigInt(timestamp));
        const response = protocol.signHttpResponse(request, { version: 1, voiceEnforcementProtocol: 4,
          nodeBootId: '11111111-1111-4111-8111-111111111111', activation: 'active', transportOK: true, admissionGranted: true }, timestamp, key);
        const result = protocol.verifyHttpResponse('health_bootstrap', response,
          protocol.newRequestContext(request, {}), key, BigInt(timestamp)).healthBootstrap();
        if (!result) throw new Error('Missing verified health result');
        return result;
      },
      recover: async () => {}, onTransport: () => {},
    });
    await health.poll();
    expect(health.status()).toMatchObject({ voiceEnforcementTransportOK: true, voiceEnforcementAdmissionGranted: true });
    let route: ((_request: object, response: object) => Promise<void>) | undefined;
    const compiled = ts.transpileModule(selected.join('\n'), {
      compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None },
    }).outputText;
    const available = new Script(`${compiled}\nenforcementAvailable;`).runInNewContext({
      app: { get: (_path: string, handler: typeof route) => { route = handler; } },
      config: { roomPlacementEnabled: false, redisCredentialMode: 'legacy' },
      redisService: { refreshAcceptingNewRooms: async () => true, healthState: () => ({ redisReachable: true }) },
      roomManager: { getRoomIds: () => [], getCreatesInFlightCount: () => 0 },
      mediasoupService: service, shuttingDown: false,
      voiceEnforcementReleaseQueue: { saturated: () => false },
      enforcementAvailability: { isReady: () => true },
      voiceEnforcementLease: lease, voiceEnforcementHealth: health,
      voiceEnforcementExpiryFence: { ready: () => true }, voiceEnforcementSubscriptionReady: true,
      voiceEnforcementSubject: 'test-subject',
      natsService: { requiredSubscriptionsReady: () => true, voiceCommandReady: () => true,
        isRequestSubscriptionHealthy: () => true },
    }) as () => boolean;
    const readReady = async () => {
      if (!route) throw new Error('Actual readyz route was not registered');
      let status = 200;
      let body: unknown;
      const response = { status(code: number) { status = code; return response; }, json(value: unknown) { body = value; } };
      await route({}, response);
      return { status, body };
    };
    try {
      expect(available()).toBe(true);
      expect(await readReady()).toEqual({ status: 200, body: { ready: true, ...health.status() } });
      expect(health.status().voiceEnforcementProtocol).toBe(4);
      workers[0].server.close();
      expect(service.isReady()).toBe(false);
      expect(available()).toBe(false);
      expect(health.status()).toMatchObject({ voiceEnforcementTransportOK: true, voiceEnforcementAdmissionGranted: false });
      expect(await readReady()).toEqual({ status: 503, body: { ready: false, ...health.status() } });
    } finally {
      await service.close(performance.now() + 1000);
    }
  });
});
