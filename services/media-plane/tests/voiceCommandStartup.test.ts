import { beforeAll, describe, expect, it, vi } from 'vitest';
import { spawn, type ChildProcess } from 'node:child_process';
import { access, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createServer as httpServer, type IncomingMessage } from 'node:http';
import { createServer as tcpServer, type Socket as NetworkSocket } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { randomBytes } from 'node:crypto';
import jwt from 'jsonwebtoken';
import * as protocol from '../src/lib/voiceAuthProtocol.js';
import {
  natsServerAcquisitionTimeoutMs,
  resolveNatsServerBinary,
} from './helpers/natsServerBinary.js';
import {
  AckPolicy,
  DeliverPolicy,
  DiscardPolicy,
  JSONCodec,
  RetentionPolicy,
  StorageType,
  connect,
  nanos,
  type NatsConnection,
} from 'nats';

const serviceRoot = fileURLToPath(new URL('..', import.meta.url));
const codec = JSONCodec();
const fixtureSigner = randomBytes(32).toString('hex');
const generation = '11111111-1111-4111-8111-111111111111';
const roomId = '2222222a-2222-4222-8222-222222222222';
const userId = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const bucket = 'VOICE_COMMAND_OWNER_V1';
const backing = `KV_${bucket}`;
const stream = 'VOICE_COMMANDS_V1';
const metadata = { 'concord.voice-enforcement.generation': generation };
const description = `concord.voice-enforcement generation=${generation}`;
let brokerBinary: string;
const common = {
  description,
  metadata,
  storage: StorageType.File,
  retention: RetentionPolicy.Limits,
  discard: DiscardPolicy.New,
  num_replicas: 1,
  max_age: 0,
  max_consumers: -1,
  duplicate_window: nanos(120_000),
};
const ownerConfig = {
  ...common,
  name: backing,
  subjects: [`$KV.${bucket}.>`],
  max_bytes: -1,
  max_msgs: -1,
  max_msg_size: -1,
  max_msgs_per_subject: 1,
  allow_rollup_hdrs: true,
  deny_delete: true,
  allow_direct: true,
};

async function unusedPort() {
  const listener = tcpServer();
  await new Promise<void>((resolve, reject) => {
    listener.once('error', reject);
    listener.listen(0, '127.0.0.1', resolve);
  });
  const address = listener.address();
  if (!address || typeof address === 'string') throw new Error('isolated port missing');
  await new Promise<void>((resolve, reject) =>
    listener.close((error) => (error ? reject(error) : resolve()))
  );
  return address.port;
}

async function isolatedBroker(deniedSubject?: string) {
  const port = await unusedPort();
  const directory = await mkdtemp(join(tmpdir(), 'concord-startup-broker-'));
  const password = randomBytes(32).toString('hex');
  const configPath = join(directory, 'server.conf');
  async function writeConfig(subject?: string) {
    await writeFile(
      configPath,
      JSON.stringify({
        no_auth_user: 'fixture-application',
        authorization: {
          users: [
            { user: 'fixture-provisioner', password },
            {
              user: 'fixture-application',
              password,
              permissions: {
                publish: '>',
                subscribe: { allow: ['>'], deny: subject ? [subject] : [] },
              },
            },
          ],
        },
      })
    );
  }
  await writeConfig(deniedSubject);
  const arguments_ = [
    '-js',
    '-sd',
    directory,
    '-a',
    '127.0.0.1',
    '-p',
    String(port),
    '-c',
    configPath,
  ];
  const child = spawn(brokerBinary, arguments_, {
    stdio: ['ignore', 'ignore', 'pipe'],
  });
  let output = '';
  child.stderr.on('data', (data: Buffer) => {
    output = (output + data.toString()).slice(-16_384);
  });
  const exited = new Promise<void>((resolve, reject) => {
    child.once('error', reject);
    child.once('exit', () => resolve());
  });
  let connection: NatsConnection | undefined;
  try {
    await vi.waitFor(
      async () => {
        connection = await connect({
          servers: `nats://127.0.0.1:${port}`,
          reconnect: false,
          timeout: 100,
          user: 'fixture-provisioner',
          pass: password,
        });
      },
      { timeout: 5000, interval: 20 }
    );
    return {
      connection: connection!,
      url: `nats://127.0.0.1:${port}`,
      deniedSubscriptionObserved() {
        return (
          deniedSubject !== undefined &&
          output.includes('Subscription Violation') &&
          output.includes(`Subject "${deniedSubject}"`)
        );
      },
      async setDeniedSubject(subject?: string) {
        await writeConfig(subject);
        deniedSubject = subject;
        output = '';
        child.kill('SIGHUP');
      },
      async close() {
        await connection?.close();
        child.kill('SIGINT');
        await exited;
        await rm(directory, { recursive: true, force: true });
      },
    };
  } catch (error) {
    child.kill('SIGINT');
    await exited;
    await rm(directory, { recursive: true, force: true });
    throw error;
  }
}

async function provision(connection: NatsConnection) {
  const manager = await connection.jetstreamManager();
  const js = connection.jetstream();
  const commands = await manager.streams.add({
    ...common,
    name: stream,
    subjects: ['voice.commands.v1'],
    max_bytes: 67108864,
    max_msgs: 50000,
    max_msg_size: 4096,
    max_msgs_per_subject: -1,
  });
  const owner = await manager.streams.add(ownerConfig);
  const kv = await js.views.kv(bucket, { bindOnly: true, allow_direct: false });
  const birth = { streamCreatedAt: commands.created, ownerStoreCreatedAt: owner.created };
  const initial = await kv.create(
    'authoritative',
    codec.encode({
      version: 1,
      streamGeneration: generation,
      phase: 'initializing',
      nodeBootId: null,
      birth,
    })
  );
  await kv.update(
    'authoritative',
    codec.encode({
      version: 1,
      streamGeneration: generation,
      phase: 'released',
      nodeBootId: null,
      proof: { ...birth, initializingRevision: String(initial) },
    }),
    initial
  );
  return {
    manager,
    kv,
    async owner() {
      const entry = await kv.get('authoritative');
      if (!entry || entry.operation !== 'PUT') throw new Error('owner unavailable');
      return codec.decode(entry.value) as { phase: string; nodeBootId: string; proof: unknown };
    },
  };
}

// The actual Redis client must finish its startup handshake. This bounded RESP
// fixture implements only commands used by these tests; unknown commands fail.
// No persistence behavior is claimed from this network double.
async function redisFixture(placement = false) {
  const sockets = new Set<NetworkSocket>();
  const keys = new Map<string, string>();
  const server = tcpServer((socket) => {
    sockets.add(socket);
    socket.on('close', () => sockets.delete(socket));
    socket.on('error', () => socket.destroy());
    let pending = Buffer.alloc(0);
    let transaction = false;
    let queued = 0;
    socket.on('data', (data) => {
      pending = Buffer.concat([pending, Buffer.isBuffer(data) ? data : Buffer.from(data)]);
      while (pending.length) {
        const lineEnd = pending.indexOf('\r\n');
        if (lineEnd < 0) return;
        const count = Number(pending.subarray(1, lineEnd).toString());
        if (pending[0] !== 42 || !Number.isSafeInteger(count)) {
          socket.destroy();
          return;
        }
        let offset = lineEnd + 2;
        const arguments_: string[] = [];
        for (let i = 0; i < count; i++) {
          const end = pending.indexOf('\r\n', offset);
          if (end < 0) return;
          const length = Number(pending.subarray(offset + 1, end).toString());
          if (pending[offset] !== 36 || !Number.isSafeInteger(length) || length < 0) {
            socket.destroy();
            return;
          }
          offset = end + 2;
          if (pending.length < offset + length + 2) return;
          arguments_.push(pending.subarray(offset, offset + length).toString());
          offset += length + 2;
        }
        pending = pending.subarray(offset);
        const command = arguments_[0]?.toUpperCase();
        if (command === 'MULTI') {
          transaction = true;
          queued = 0;
          socket.write('+OK\r\n');
        } else if (command === 'EXEC') {
          transaction = false;
          socket.write(`*${queued}\r\n${':1\r\n'.repeat(queued)}`);
        } else if (transaction) {
          queued++;
          socket.write('+QUEUED\r\n');
        } else if (command === 'HELLO')
          socket.write(
            '%7\r\n+server\r\n+redis\r\n+version\r\n+7.2.0\r\n+proto\r\n:3\r\n+id\r\n:1\r\n+mode\r\n+standalone\r\n+role\r\n+master\r\n+modules\r\n*0\r\n'
          );
        else if (command === 'PING') socket.write('+PONG\r\n');
        else if (command === 'CLIENT' || command === 'QUIT') socket.write('+OK\r\n');
        else if (command === 'SMEMBERS') socket.write('*0\r\n');
        else if (placement && command === 'MGET') {
          const values = arguments_.slice(1).map((key) => keys.get(key));
          socket.write(
            `*${values.length}\r\n${values.map((value) => (value === undefined ? '_\r\n' : `$${Buffer.byteLength(value)}\r\n${value}\r\n`)).join('')}`
          );
        } else if (placement && command === 'EVAL') {
          const keyCount = Number(arguments_[2]);
          const keysForScript = arguments_.slice(3, 3 + keyCount);
          const scriptArgs = arguments_.slice(3 + keyCount);
          if (keysForScript[1]?.startsWith('voice:room_live:') && scriptArgs[1]) {
            keys.set(keysForScript[1], scriptArgs[1]);
          }
          socket.write(':1\r\n');
        } else if (placement && command === 'SET') {
          if (arguments_.includes('NX') && keys.has(arguments_[1])) socket.write('_\r\n');
          else {
            keys.set(arguments_[1], arguments_[2]);
            socket.write('+OK\r\n');
          }
        } else if (placement && command === 'INCR') {
          const value = Number(keys.get(arguments_[1]) ?? '0') + 1;
          keys.set(arguments_[1], String(value));
          socket.write(`:${value}\r\n`);
        } else if (placement && command === 'GET') {
          const value = keys.get(arguments_[1]);
          socket.write(
            value === undefined ? '_\r\n' : `$${Buffer.byteLength(value)}\r\n${value}\r\n`
          );
        } else if (['SADD', 'SREM', 'EXPIRE', 'HSET', 'DEL'].includes(command ?? ''))
          socket.write(':1\r\n');
        else socket.write('-ERR unsupported isolated fixture command\r\n');
      }
    });
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('Redis fixture port missing');
  return {
    url: `redis://127.0.0.1:${address.port}`,
    seed(key: string, value: string) {
      keys.set(key, value);
    },
    value(key: string) {
      return keys.get(key);
    },
    async close() {
      for (const socket of sockets) socket.destroy();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
}

const bootHealth = new Map<string, () => void>();
const bootReadiness = new Map<string, () => Promise<{ status: number; body: unknown }>>();

async function healthChallenge(connection: NatsConnection, boot: string) {
  const key = Buffer.from(fixtureSigner);
  const command = protocol.newHealthCommand(boot, randomBytes(32).toString('hex'), 'active', true,
    String(Math.floor(Date.now() / 1000)));
  const response = await connection.request(`voice.enforce.session.${boot}`,
    protocol.signCommand(command, key), { timeout: 2000 });
  const ack = protocol.verifyAck(response.data, command, key,
    BigInt(Math.floor(Date.now() / 1000))).health();
  expect(ack?.transportOK).toBe(true);
  return ack!.admissionGranted;
}

// Only the production bootstrap caller may renew authority. A NATS challenge
// alone must never turn a protocol-4 process ready.
async function signedHealth(connection: NatsConnection, boot: string) {
  await vi.waitFor(() => expect(bootHealth.has(boot)).toBe(true), { timeout: 3000 });
  bootHealth.get(boot)!();
  await healthChallenge(connection, boot);
  // Placement adds fields to /readyz; the caller asserts those separately.
  // The readiness witness is the actual process response after signature receipt.
  await vi.waitFor(async () =>
    expect(await bootReadiness.get(boot)!()).toMatchObject(readiness(boot, true)),
  { timeout: 15000, interval: 20 });
}

function readiness(boot: string, ready: boolean, transport = true) {
  return { status: ready ? 200 : 503, body: {
    ready, voiceEnforcementProtocol: 4, nodeBootId: boot,
    voiceEnforcementTransportOK: transport,
    voiceEnforcementAdmissionGranted: ready,
  } };
}

async function requestBody(request: IncomingMessage) {
  const chunks: Buffer[] = [];
  for await (const chunk of request) chunks.push(Buffer.from(chunk));
  return Buffer.concat(chunks).toString();
}

async function controlPlane(connection: NatsConnection) {
  let healthEnabled = false;
  let authorizeEnabled = false;
  let releaseEnabled = true;
  let authorizations = 0;
  let releaseAttempts = 0;
  let roomReleaseAttempts = 0;
  const boundaryErrors: string[] = [];
  let revision = 0;
  const registrations = new Map<string, protocol.RoomRegister>();
  const connections = new Map<string, protocol.Binding>();
  const endpoints: Record<string, protocol.Endpoint> = {
    '/api/v1/internal/voice/enforcement/health': 'health_bootstrap',
    '/api/v1/internal/voice/auth-connections': 'connection_register',
    '/api/v1/internal/voice/auth-connections/release': 'connection_release',
    '/api/v1/internal/voice/auth-connections/retire-boot': 'boot_retire',
    '/api/v1/voice/enforcement-sessions': 'room_register',
    '/api/v1/internal/voice/enforcement-sessions/release': 'room_release',
  };
  const server = httpServer(async (request, response) => {
    try {
      const path = request.url ?? '';
      const body = await requestBody(request);
      response.setHeader('Content-Type', 'application/json');
      const endpoint = endpoints[path];
      if (endpoint) {
        const key = Buffer.from(fixtureSigner);
        const now = String(Math.floor(Date.now() / 1000));
        const headers = Object.entries(request.headers).flatMap(([name, value]) =>
          Array.isArray(value) ? value.map(value => ({ name, value })) : value === undefined ? [] : [{ name, value }]);
        const decoded = protocol.decodeHttpRequest(endpoint, {
          method: request.method ?? '', path, headers, body: Buffer.from(body),
        });
        protocol.verifyHttpRequest(decoded, key, BigInt(now));
        let payload: protocol.ResponsePayload = null;
        const bootstrap = decoded.healthBootstrap();
        const registration = decoded.connectionRegister();
        const room = decoded.roomRegister();
        if (bootstrap) {
          bootHealth.set(bootstrap.nodeBootId, () => { healthEnabled = true; });
          if (!healthEnabled) { response.writeHead(503).end('{}'); return; }
          const granted = await healthChallenge(connection, bootstrap.nodeBootId);
          payload = { version: 1, voiceEnforcementProtocol: 4, nodeBootId: bootstrap.nodeBootId,
            activation: 'active', transportOK: true, admissionGranted: granted };
        } else if (registration) {
          const token = String(request.headers.authorization).replace(/^Bearer /, '');
          const claims = jwt.verify(token, fixtureSigner, { issuer: 'concordvoice-control-plane', algorithms: ['HS256'] });
          if (typeof claims === 'string' || claims.user_id !== userId) throw new Error('Invalid fixture bearer');
          const binding: protocol.Binding = { userId, familyId: generation, target: {
            connectionGeneration: registration.connectionGeneration, nodeBootId: registration.nodeBootId,
            socketId: registration.socketId, credentialEpoch: registration.credentialEpoch } };
          connections.set(registration.connectionGeneration, binding);
          payload = { version: 1, voiceEnforcementProtocol: 4, ...binding };
        } else if (room) {
          const binding = connections.get(room.connectionGeneration);
          if (!binding) throw new Error('Missing registered connection');
          registrations.set(room.sessionGeneration, room);
          payload = { schema: 'concord.auth-room-binding', version: 1, voiceEnforcementProtocol: 4,
            binding, room: { sessionGeneration: room.sessionGeneration, roomId: room.roomId, roomKind: room.roomKind } };
        } else if (endpoint === 'room_release' || endpoint === 'connection_release') {
          releaseAttempts++;
          if (endpoint === 'room_release') roomReleaseAttempts++;
          if (!releaseEnabled) { response.writeHead(503).end('{}'); return; }
          const released = decoded.roomRelease();
          if (released) registrations.delete(released.room.sessionGeneration);
          const releasedConnection = decoded.connectionRelease();
          if (releasedConnection) connections.delete(releasedConnection.target.connectionGeneration);
        }
        const signed = protocol.signHttpResponse(decoded, payload, now, key);
        for (const header of signed.headers) response.setHeader(header.name, header.value);
        response.writeHead(signed.status).end(Buffer.from(signed.body));
        return;
      }
      if (path.toLowerCase() === `/api/v1/channels/${roomId}/voice/join`) {
        authorizations++;
        if (!authorizeEnabled) {
          response.writeHead(403).end('{"error":"fixture authorization denied"}');
          return;
        }
        response.end(
          JSON.stringify({
            allowed: true,
            channel: {
              id: roomId,
              server_id: '33333333-3333-4333-8333-333333333333',
              name: 'fixture-room',
            },
            authorization_revision: String(++revision),
            permissions: '9223372036854775807',
            server_muted: false,
            server_deafened: false,
            room_owner_tier: 'free',
            username: 'fixture-user',
          })
        );
        return;
      }
      response.writeHead(404).end('{}');
    } catch (error) {
      boundaryErrors.push(`${request.url}: ${String(error)}`);
      if (!response.headersSent) response.writeHead(503).end('{}');
      else response.end();
    }
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('CP fixture port missing');
  return {
    url: `http://127.0.0.1:${address.port}`,
    registrations,
    connections,
    boundaryErrors,
    get roomReleaseAttempts() { return roomReleaseAttempts; },
    get authorizations() {
      return authorizations;
    },
    get releaseAttempts() {
      return releaseAttempts;
    },
    setHealth(value: boolean) {
      healthEnabled = value;
    },
    setAuthorize(value: boolean) {
      authorizeEnabled = value;
    },
    setRelease(value: boolean) {
      releaseEnabled = value;
    },
    async close() {
      server.closeAllConnections();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
}

async function alive(pid: number) {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

async function processState(pid: number): Promise<string> {
  const child = spawn('ps', ['-o', 'state=', '-p', String(pid)], {
    stdio: ['ignore', 'pipe', 'ignore'],
    timeout: 2000,
  });
  let output = '';
  child.stdout.on('data', (data: Buffer) => {
    output += data.toString();
  });
  return new Promise((resolve, reject) => {
    child.once('error', reject);
    child.once('close', (code) => {
      if (code !== 0) reject(new Error('Owned native worker state unavailable'));
      // BSD ps can append state flags; the first character is the process state.
      else resolve(output.trim().charAt(0));
    });
  });
}

async function application(
  natsUrl: string,
  redisUrl: string,
  controlPlaneUrl: string,
  placement = false
) {
  const directory = await mkdtemp(join(tmpdir(), 'concord-startup-app-'));
  const port = await unusedPort();
  const loader = join(serviceRoot, 'node_modules/tsx/dist/loader.mjs');
  await access(loader);
  const child: ChildProcess = spawn(
    process.execPath,
    ['--import', loader, join(serviceRoot, 'src/index.ts')],
    {
      cwd: directory,
      detached: true,
      env: {
        ...process.env,
        TSX_TSCONFIG_PATH: join(serviceRoot, 'tsconfig.json'),
        NODE_ENV: 'test',
        ENVIRONMENT: 'development',
        JWT_SECRET: fixtureSigner,
        PORT: String(port),
        NUM_WORKERS: '1',
        LOG_LEVEL: 'debug',
        NATS_URL: natsUrl,
        REDIS_URL: redisUrl,
        CONTROL_PLANE_URL: controlPlaneUrl,
        ANNOUNCED_IP: '127.0.0.1',
        OPS_METRICS_ENABLED: 'false',
        MEDIASOUP_ENABLE_TCP: 'false',
        ROOM_PLACEMENT_ENABLED: placement ? 'true' : 'false',
        MEDIA_INSTANCE_COLOR: placement ? 'blue' : '',
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    }
  );
  let output = '';
  const workerPids = new Set<number>();
  const capture = (data: Buffer) => {
    output = (output + data.toString()).slice(-32_768);
    for (const match of output.matchAll(/Mediasoup worker created[\s\S]{0,160}?"pid":\s*(\d+)/g))
      workerPids.add(Number(match[1]));
  };
  child.stdout?.on('data', capture);
  child.stderr?.on('data', capture);
  let result: { code: number | null; signal: NodeJS.Signals | null } | undefined;
  const exited = new Promise<typeof result>((resolve, reject) => {
    child.once('error', reject);
    child.once('exit', (code, signal) => {
      result = { code, signal };
      resolve(result);
    });
  });
  return {
    child,
    url: `http://127.0.0.1:${port}`,
    exited,
    get result() {
      return result;
    },
    get output() {
      return output;
    },
    async ready() {
      const response = await fetch(`http://127.0.0.1:${port}/readyz`, {
        signal: AbortSignal.timeout(1000),
      });
      return { status: response.status, body: await response.json() };
    },
    async listening() {
      await vi.waitFor(
        async () => {
          if (result) throw new Error(`owned app exited before listen: ${output}`);
          expect(
            (await fetch(`http://127.0.0.1:${port}/health`, { signal: AbortSignal.timeout(1000) }))
              .status
          ).toBe(200);
        },
        { timeout: 10_000, interval: 20 }
      );
      const ready = async () => {
        const response = await fetch(`http://127.0.0.1:${port}/readyz`, { signal: AbortSignal.timeout(1000) });
        return { status: response.status, body: await response.json() };
      };
      const state = await ready();
      bootReadiness.set(state.body.nodeBootId, ready);
    },
    async workers() {
      return [...workerPids];
    },
    async close() {
      if (child.pid) {
        try {
          process.kill(-child.pid, 'SIGCONT');
          process.kill(-child.pid, 'SIGKILL');
        } catch {
          /* Owned process group already stopped. */
        }
      }
      await exited;
      await rm(directory, { recursive: true, force: true });
    },
  };
}

async function pollingPeer(base: string, requestedRoomId = roomId) {
  const token = jwt.sign({ user_id: userId, cred_epoch: '' }, fixtureSigner, {
    issuer: 'concordvoice-control-plane',
    algorithm: 'HS256',
    expiresIn: '5m',
  });
  const open = await fetch(`${base}/socket.io/?EIO=4&transport=polling`, {
    signal: AbortSignal.timeout(2000),
  });
  const text = await open.text();
  expect(text[0]).toBe('0');
  const session = JSON.parse(text.slice(1)) as { sid: string };
  const endpoint = `${base}/socket.io/?EIO=4&transport=polling&sid=${encodeURIComponent(session.sid)}`;
  async function post(payload: string) {
    const response = await fetch(endpoint, {
      method: 'POST',
      headers: { 'Content-Type': 'text/plain;charset=UTF-8' },
      body: payload,
      signal: AbortSignal.timeout(2000),
    });
    expect(response.status).toBe(200);
  }
  async function poll() {
    const response = await fetch(endpoint, { signal: AbortSignal.timeout(3000) });
    expect(response.status).toBe(200);
    return (await response.text()).split('\x1e');
  }
  await post(`40${JSON.stringify({ token, username: 'fixture-user' })}`);
  const accepted = await poll();
  expect(accepted.some((packet) => packet.startsWith('40'))).toBe(true);
  let id = 0;
  return {
    async waitForServerDisconnect(timeoutMs: number) {
      const deadline = performance.now() + timeoutMs;
      while (performance.now() < deadline) {
        const remaining = Math.max(1, Math.ceil(deadline - performance.now()));
        const response = await fetch(endpoint, { signal: AbortSignal.timeout(remaining) });
        if (response.status === 400) {
          expect(await response.json()).toMatchObject({ code: 1 });
          return 'unknown_session';
        }
        expect(response.status).toBe(200);
        for (const packet of (await response.text()).split('\x1e')) {
          if (packet === '2') await post('3');
          if (packet === '1') return packet;
        }
      }
      throw new Error('Server did not close the owned polling session before its deadline');
    },
    async transportStatus() {
      return (await fetch(endpoint, { signal: AbortSignal.timeout(2000) })).status;
    },
    async join(joinRoomId = requestedRoomId) {
      const ackId = id++;
      await post(
        `42${ackId}${JSON.stringify(['join-room', { roomId: joinRoomId, mediaFrameCryptoVersion: 5 }])}`
      );
      let result: Record<string, unknown> | undefined;
      await vi.waitFor(
        async () => {
          for (const packet of await poll()) {
            if (packet.startsWith(`43${ackId}[`))
              result = JSON.parse(packet.slice(2 + String(ackId).length))[0];
          }
          expect(result).toBeDefined();
        },
        { timeout: 8000, interval: 10 }
      );
      return result!;
    },
  };
}

async function runtime(deniedSubject?: string, placement = false) {
  const broker = await isolatedBroker(deniedSubject);
  const redis = await redisFixture(placement);
  const cp = await controlPlane(broker.connection);
  const resources = await provision(broker.connection);
  let app: Awaited<ReturnType<typeof application>> | undefined;
  return {
    broker,
    redis,
    cp,
    resources,
    async start() {
      app = await application(broker.url, redis.url, cp.url, placement);
      return app;
    },
    async close() {
      await app?.close();
      await cp.close();
      await redis.close();
      await broker.close();
    },
  };
}

describe('actual index voice-command runtime boundaries', () => {
  beforeAll(async () => {
    brokerBinary = await resolveNatsServerBinary();
  }, natsServerAcquisitionTimeoutMs);

  it('fails first broker connection before binding HTTP', async () => {
    const redis = await redisFixture();
    const deadPort = await unusedPort();
    const app = await application(`nats://127.0.0.1:${deadPort}`, redis.url, 'http://127.0.0.1:1');
    try {
      await vi.waitFor(() => expect(app.result?.code, app.output).toBe(1), { timeout: 10_000 });
      expect(app.output).toContain('Fatal error during startup');
      expect(app.output).not.toContain('Media Plane server started');
      await expect(
        fetch(`${app.url}/health`, { signal: AbortSignal.timeout(500) })
      ).rejects.toThrow();
    } finally {
      await app.close();
      await redis.close();
    }
  }, 15_000);

  it('becomes ready on a fresh process restart after the initial broker connection failed', async () => {
    const f = await runtime();
    const deadPort = await unusedPort();
    const failed = await application(`nats://127.0.0.1:${deadPort}`, f.redis.url, f.cp.url);
    try {
      await vi.waitFor(() => expect(failed.result?.code, failed.output).toBe(1), {
        timeout: 10_000,
      });
      expect(failed.output).toContain('Failed to connect to NATS');
      expect(failed.output).not.toContain('Media Plane server started');
      expect((await f.resources.owner()).phase).toBe('released');

      const restarted = await f.start();
      await restarted.listening();
      const owner = await f.resources.owner();
      expect(owner.phase).toBe('held');
      await signedHealth(f.broker.connection, owner.nodeBootId);
      expect(await restarted.ready()).toEqual(readiness(owner.nodeBootId, true));
      // Legacy (placement off) still reports both conversion-gate counts.
      const health = await fetch(`${restarted.url}/health`).then((res) => res.json());
      expect(health).toMatchObject({ rooms: 0, creates_in_flight: 0 });
    } finally {
      await failed.close();
      await f.close();
    }
  }, 20_000);

  it('reports instance readiness and refuses promotion after a colour drain', async () => {
    const f = await runtime(undefined, true);
    try {
      const app = await f.start();
      await app.listening();
      const owner = await f.resources.owner();
      await signedHealth(f.broker.connection, owner.nodeBootId);
      expect(await app.ready()).toMatchObject({
        status: 200,
        body: {
          ready: true,
          voiceEnforcementProtocol: 4,
          nodeBootId: owner.nodeBootId,
          voiceEnforcementTransportOK: true,
          voiceEnforcementAdmissionGranted: true,
          colour: 'blue',
          generation: expect.stringMatching(
            /^\d+-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i
          ),
          accepting_new_rooms: true,
          rooms: 0,
          creates_in_flight: 0,
        },
      });
      f.redis.seed('media:colour_draining:blue', 'armed');
      expect(await app.ready()).toMatchObject({
        status: 503,
        body: {
          ready: false,
          colour: 'blue',
          accepting_new_rooms: false,
          rooms: 0,
          creates_in_flight: 0,
        },
      });
    } finally {
      await f.close();
    }
  }, 20_000);

  it('fails required durable initialization before listen with invalid consumer configuration', async () => {
    const f = await runtime();
    try {
      await f.resources.manager.consumers.add(stream, {
        name: 'voice-enforcement-v1',
        durable_name: 'voice-enforcement-v1',
        ack_policy: AckPolicy.Explicit,
        deliver_policy: DeliverPolicy.All,
        filter_subject: 'voice.commands.v1',
        max_ack_pending: 2,
        ack_wait: nanos(30_000),
        max_deliver: -1,
      });
      const app = await f.start();
      await vi.waitFor(() => expect(app.result?.code, app.output).toBe(1), { timeout: 10_000 });
      expect(app.output).toContain('Fatal error during startup');
      expect(app.output).not.toContain('Media Plane server started');
      expect((await f.resources.owner()).phase).toBe('held');
      await expect(
        fetch(`${app.url}/health`, { signal: AbortSignal.timeout(500) })
      ).rejects.toThrow();
    } finally {
      await f.close();
    }
  }, 15_000);

  it.each(['voice.enforce.permissions', 'voice.admission.activate'])(
    'fails before listen when broker refuses required subscription %s',
    async (deniedSubject) => {
      const f = await runtime(deniedSubject);
      try {
        const app = await f.start();
        await vi.waitFor(
          () => {
            expect(f.broker.deniedSubscriptionObserved()).toBe(true);
            expect(
              app.result !== undefined || app.output.includes('Media Plane server started')
            ).toBe(true);
          },
          { timeout: 10_000 }
        );
        expect(app.result?.code, app.output).toBe(1);
        expect(app.output).toContain('Fatal error during startup');
        expect(app.output).toContain('Connected to NATS');
        expect(f.broker.deniedSubscriptionObserved()).toBe(true);
        expect(app.output).not.toContain('Media Plane server started');
        expect((await f.resources.owner()).phase).toBe('released');
        await expect(
          fetch(`${app.url}/health`, { signal: AbortSignal.timeout(500) })
        ).rejects.toThrow();
      } finally {
        await f.close();
      }
    },
    15_000
  );

  it('gates live admission and restores the same authenticated peer after required subscription recovery', async () => {
    const f = await runtime();
    try {
      f.cp.setHealth(true);
      const app = await f.start();
      await app.listening();
      const owner = await f.resources.owner();
      await signedHealth(f.broker.connection, owner.nodeBootId);
      expect(await app.ready()).toEqual(readiness(owner.nodeBootId, true));
      const peer = await pollingPeer(app.url);
      expect((await peer.join()).error).toBeDefined();
      expect(f.cp.authorizations).toBe(1);

      await f.broker.setDeniedSubject('voice.enforce.permissions');
      await vi.waitFor(
        async () => {
          expect(f.broker.deniedSubscriptionObserved()).toBe(true);
          expect(await app.ready()).toEqual(readiness(owner.nodeBootId, false, false));
        },
        { timeout: 5000, interval: 20 }
      );
      const refused = await fetch(`${app.url}/socket.io/?EIO=4&transport=polling`, {
        signal: AbortSignal.timeout(2000),
      });
      expect(refused.status).toBe(403);
      expect(await peer.join()).toEqual({ error: 'Voice service is shutting down' });
      expect(f.cp.authorizations).toBe(1);

      await f.broker.setDeniedSubject(undefined);
      await vi.waitFor(
        async () => {
          await signedHealth(f.broker.connection, owner.nodeBootId);
          expect(await app.ready()).toEqual(readiness(owner.nodeBootId, true));
        },
        { timeout: 18000, interval: 100 }
      );
      expect(await f.resources.owner()).toEqual(owner);
      expect((await peer.join()).error).toBeDefined();
      expect(f.cp.authorizations).toBe(2);
    } finally {
      await f.close();
    }
  }, 25_000);

  it('disconnects admitted and idle sockets within the outage ceiling while native termination proof is pending', async () => {
    const f = await runtime();
    const pausedWorkers = new Set<number>();
    try {
      f.cp.setAuthorize(true);
      f.cp.setHealth(true);
      const app = await f.start();
      await app.listening();
      const owner = await f.resources.owner();
      await signedHealth(f.broker.connection, owner.nodeBootId);
      const admitted = await pollingPeer(app.url);
      expect((await admitted.join()).rtpCapabilities).toBeDefined();
      const idle = await pollingPeer(app.url);
      expect(f.cp.registrations.size).toBe(1);
      expect(f.cp.connections.size).toBe(2);
      const workers = await app.workers();
      expect(workers).toHaveLength(1);
      for (const pid of workers) {
        process.kill(pid, 'SIGSTOP');
        pausedWorkers.add(pid);
      }
      await vi.waitFor(async () => {
        for (const pid of pausedWorkers) expect(await processState(pid)).toBe('T');
      });
      const closures = Promise.all([
        admitted.waitForServerDisconnect(35_000),
        idle.waitForServerDisconnect(35_000),
      ]);
      const began = performance.now();
      await f.broker.setDeniedSubject('voice.enforce.permissions');
      await vi.waitFor(
        async () => {
          expect(f.broker.deniedSubscriptionObserved()).toBe(true);
          expect(await app.ready()).toEqual(readiness(owner.nodeBootId, false, false));
        },
        { timeout: 5000, interval: 20 }
      );
      expect(await closures).toHaveLength(2);
      expect(performance.now() - began).toBeLessThan(32_000);
      await vi.waitFor(async () => {
        expect(await admitted.transportStatus()).toBe(400);
        expect(await idle.transportStatus()).toBe(400);
      });
      const health = (await (await fetch(`${app.url}/health`)).json()) as {
        totalParticipants: number;
      };
      expect(health.totalParticipants).toBe(0);
      expect(await app.ready()).toEqual(readiness(owner.nodeBootId, false, false));
      expect(await f.resources.owner()).toEqual(owner);
      expect(f.cp.registrations.size).toBe(1);
      expect(f.cp.roomReleaseAttempts).toBe(0);
      expect(f.cp.connections.size).toBe(1);
      for (const pid of pausedWorkers) {
        expect(await alive(pid)).toBe(true);
        expect(await processState(pid)).toBe('T');
      }

      // Socket closure must precede native proof. Resume only after proving
      // the worker is still stopped and exact ownership/release evidence held.
      for (const pid of pausedWorkers) process.kill(pid, 'SIGCONT');
      pausedWorkers.clear();

      await f.broker.setDeniedSubject(undefined);
      await vi.waitFor(
        async () => {
          await signedHealth(f.broker.connection, owner.nodeBootId);
          expect(await app.ready()).toEqual(readiness(owner.nodeBootId, true));
        },
        { timeout: 18000, interval: 100 }
      );
      expect(await admitted.transportStatus()).toBe(400);
      expect(await idle.transportStatus()).toBe(400);
      const fresh = await pollingPeer(app.url);
      expect((await fresh.join()).rtpCapabilities).toBeDefined();
    } finally {
      for (const pid of pausedWorkers) {
        try {
          process.kill(pid, 'SIGCONT');
        } catch {
          /* Owned worker already exited during failed-test cleanup. */
        }
      }
      await f.close();
    }
  }, 48_000);

  it('keeps exact readyz bodies and refuses admission/revival after owner-key loss', async () => {
    const f = await runtime();
    try {
      const app = await f.start();
      await app.listening();
      const held = await f.resources.owner();
      expect(await app.ready()).toEqual(readiness(held.nodeBootId, false, false));
      expect(held.phase).toBe('held');
      await signedHealth(f.broker.connection, held.nodeBootId);
      await vi.waitFor(async () =>
        expect(await app.ready()).toEqual(readiness(held.nodeBootId, true))
      );
      const peer = await pollingPeer(app.url);
      const refused = await peer.join();
      expect(refused.error).toBeDefined();
      expect(f.cp.authorizations).toBe(1);
      await f.resources.kv.delete('authoritative');
      await vi.waitFor(
        async () => expect(await app.ready()).toEqual(readiness(held.nodeBootId, false)),
        { timeout: 3500, interval: 20 }
      );
      const afterLoss = await fetch(`${app.url}/socket.io/?EIO=4&transport=polling`, { signal: AbortSignal.timeout(2000) });
      expect(afterLoss.status).toBe(403);
      expect(await peer.waitForServerDisconnect(5000)).toBeDefined();
      expect(await peer.transportStatus()).toBe(400);
      expect(f.cp.authorizations).toBe(1);
      // A verified transport reply cannot revive lost ownership or productive authority.
      expect(await healthChallenge(f.broker.connection, held.nodeBootId)).toBe(false);
      expect(await app.ready()).toEqual(readiness(held.nodeBootId, false));
      expect((await f.resources.kv.get('authoritative'))?.operation).toBe('DEL');
    } finally {
      await f.close();
    }
  }, 20_000);

  it('retains held ownership for a paused predecessor with a live native worker beyond local expiry', async () => {
    const f = await runtime();
    let replacement: Awaited<ReturnType<typeof application>> | undefined;
    let app: Awaited<ReturnType<typeof application>> | undefined;
    try {
      app = await f.start();
      await app.listening();
      const before = await f.resources.owner();
      const workers = await app.workers();
      expect(workers).toHaveLength(1);
      expect(await alive(workers[0])).toBe(true);
      app.child.kill('SIGSTOP');
      await new Promise<void>((resolve) => setTimeout(resolve, 5100));
      expect(await alive(workers[0])).toBe(true);
      replacement = await application(f.broker.url, f.redis.url, f.cp.url);
      await vi.waitFor(() => expect(replacement!.result?.code).toBe(1), { timeout: 10_000 });
      expect(replacement.output).not.toContain('Media Plane server started');
      expect(await f.resources.owner()).toEqual(before);
    } finally {
      app?.child.kill('SIGCONT');
      await replacement?.close();
      await f.close();
    }
  }, 22_000);

  it('releases ownership only after the owned native worker physically exits during graceful shutdown', async () => {
    const f = await runtime();
    try {
      const app = await f.start();
      await app.listening();
      const workers = await app.workers();
      expect(workers).toHaveLength(1);
      expect(await alive(workers[0])).toBe(true);
      app.child.kill('SIGTERM');
      await vi.waitFor(async () => expect((await f.resources.owner()).phase).toBe('released'), {
        timeout: 10_000,
        interval: 5,
      });
      expect(await alive(workers[0])).toBe(false);
      await vi.waitFor(() => expect(app.result?.code).toBe(0), { timeout: 5000 });
    } finally {
      await f.close();
    }
  }, 20_000);

  it('rejects an authorized uppercase channel UUID before alternate room or Redis ownership state', async () => {
    const f = await runtime(undefined, true);
    try {
      f.cp.setAuthorize(true);
      f.cp.setHealth(true);
      const app = await f.start();
      await app.listening();
      const owner = await f.resources.owner();
      await signedHealth(f.broker.connection, owner.nodeBootId);
      const alternateRoomId = roomId.toUpperCase();
      expect(
        alternateRoomId,
        'positive control: uppercase spelling must differ from the canonical room ID'
      ).not.toBe(roomId);
      const authorization = await fetch(
        `${f.cp.url}/api/v1/channels/${alternateRoomId}/voice/join`,
        {
          method: 'POST',
          body: '{}',
        }
      );
      expect(
        authorization.status,
        'positive control: control plane accepts the uppercase UUID route'
      ).toBe(200);
      f.redis.seed(`voice:room_owner:${alternateRoomId}`, 'blue');
      const peer = await pollingPeer(app.url, alternateRoomId);
      const result = await peer.join();
      expect(
        f.redis.value(`voice:room_live:${alternateRoomId}`),
        'alternate spelling must not create Redis room occupancy'
      ).toBeUndefined();
      expect(
        result.error,
        'authorized alternate UUID spelling must be rejected before room creation'
      ).toMatch(/canonical|invalid room/i);
      const health = (await (await fetch(`${app.url}/health`)).json()) as {
        totalParticipants: number;
      };
      expect(health.totalParticipants).toBe(0);
    } finally {
      await f.close();
    }
  }, 20_000);

  it('retains held ownership when an actually admitted exact registry session cannot release', async () => {
    const f = await runtime();
    try {
      f.cp.setAuthorize(true);
      f.cp.setHealth(true);
      const app = await f.start();
      await app.listening();
      const before = await f.resources.owner();
      await signedHealth(f.broker.connection, before.nodeBootId);
      const peer = await pollingPeer(app.url);
      const joined = await peer.join();
      expect(joined.error).toBeUndefined();
      expect(joined.rtpCapabilities).toBeDefined();
      expect(f.cp.registrations.size).toBe(1);
      const health = (await (await fetch(`${app.url}/health`)).json()) as {
        totalParticipants: number;
      };
      expect(health.totalParticipants).toBe(1);
      f.cp.setRelease(false);
      app.child.kill('SIGTERM');
      await vi.waitFor(() => expect(f.cp.releaseAttempts, `${f.cp.boundaryErrors.join("; ")}\n${app.output}`).toBeGreaterThan(0), { timeout: 5000 });
      expect(await f.resources.owner()).toEqual(before);
      expect(await app.ready()).toEqual(readiness(before.nodeBootId, false));
      await vi.waitFor(() => expect(app.result?.code, app.output).toBe(1), {
        timeout: 38_000,
        interval: 50,
      });
      expect(f.cp.registrations.size).toBe(1);
      expect(await f.resources.owner()).toEqual(before);
    } finally {
      await f.close();
    }
  }, 48_000);
});
