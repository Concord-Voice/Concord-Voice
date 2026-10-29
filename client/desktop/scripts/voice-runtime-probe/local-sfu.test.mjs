import assert from 'node:assert/strict';
import test from 'node:test';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { createLocalSfu } from './local-sfu.mjs';
import { createRequire } from 'node:module';

const directory = path.dirname(fileURLToPath(import.meta.url));
const media = process.env.MEDIA_PROBE_SERVER_DIR ?? path.resolve(directory, '../../../../services/media-plane');

test('loopback fixture uses a real worker and isolates owned resources', async (context) => {
  const sfu = await createLocalSfu(media);
  context.after(() => sfu.close());
  const caps = await sfu.rpc({ peer: 'a', action: 'capabilities' });
  assert(caps.codecs.some((codec) => codec.mimeType === 'audio/opus'));
  assert(caps.codecs.some((codec) => codec.mimeType === 'video/VP8'));
  const transport = await sfu.rpc({ peer: 'a', action: 'createTransport', direction: 'send' });
  assert(transport.iceCandidates.length > 0);
  assert(transport.iceCandidates.every((candidate) => candidate.address === '127.0.0.1' || candidate.ip === '127.0.0.1'));
  assert(transport.dtlsParameters.fingerprints.length > 0);
  await assert.rejects(sfu.rpc({ peer: 'b', action: 'connect', transportId: transport.id }), /Unknown fixture resource/);
  await assert.rejects(sfu.rpc({ peer: 'unknown', action: 'capabilities' }), /Unknown fixture peer/);
  await assert.rejects(sfu.rpc({ peer: 'a', action: 'unknown' }), /Unknown fixture action/);
  await assert.rejects(sfu.rpc({ peer: 'a', action: 'createTransport', direction: 'unknown' }), /Invalid direction/);
  // A rejected cross-peer action must not poison the worker's following request.
  const receiver = await sfu.rpc({ peer: 'b', action: 'createTransport', direction: 'audio' });
  assert.notEqual(receiver.id, transport.id);
});

test('router initialization rejection closes the already spawned real native worker', async () => {
  const require = createRequire(path.join(media, 'package.json'));
  const mediasoup = require('mediasoup');
  let capturedWorker;
  await assert.rejects(createLocalSfu(media, { workerFactory: async (options) => {
    capturedWorker = await mediasoup.createWorker(options);
    // Force only the setup failure. The subprocess and exit witness remain real.
    capturedWorker.createRouter = async () => { throw new Error('forced router initialization failure'); };
    return capturedWorker;
  } }), /forced router initialization failure/);
  assert.equal(capturedWorker.closed, true);
  assert.equal(capturedWorker.subprocessClosed, true);
});
