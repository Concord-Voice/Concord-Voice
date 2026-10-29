import { createRequire } from 'node:module';
import path from 'node:path';
import { once } from 'node:events';
import { readFile, realpath } from 'node:fs/promises';
import { createHash } from 'node:crypto';

/** A loopback-only fixture. No application auth or RoomManager assertion is made here. */
export async function createLocalSfu(mediaDirectory, { workerFactory } = {}) {
  const require = createRequire(path.join(mediaDirectory, 'package.json'));
  const mediasoup = require('mediasoup');
  const workerPath = await realpath(mediasoup.workerBin);
  const workerSha256 = createHash('sha256').update(await readFile(workerPath)).digest('hex');
  const expectedSha256 = process.env.MEDIA_PROBE_WORKER_SHA256;
  if (expectedSha256 !== undefined && (!/^[a-f0-9]{64}$/.test(expectedSha256) || expectedSha256 !== workerSha256)) {
    throw new Error('Native worker candidate SHA-256 mismatch');
  }
  const worker = await (workerFactory ?? mediasoup.createWorker)({ logLevel: 'error' });
  const closed = once(worker, 'subprocessclose');
  async function closeWorker() {
    let timer;
    try {
      worker.close();
      await Promise.race([closed, new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error('Native worker closure deadline')), 10_000);
      })]);
      if (!worker.subprocessClosed) throw new Error('Native worker exit unconfirmed');
    } finally { clearTimeout(timer); }
  }
  let router;
  try {
    router = await worker.createRouter({
    mediaCodecs: [
      { kind: 'audio', mimeType: 'audio/opus', clockRate: 48000, channels: 2 },
      { kind: 'video', mimeType: 'video/VP8', clockRate: 90000 },
    ],
    });
  } catch (error) {
    await closeWorker();
    throw error;
  }
  const peers = new Map(['a', 'b'].map((id) => [id, {
    transports: new Map(), producers: new Map(), consumers: new Map(),
  }]));
  const owned = (map, id) => {
    const value = typeof id === 'string' && map.get(id);
    if (!value || value.closed) throw new Error('Unknown fixture resource');
    return value;
  };
  const rpc = async (body) => {
    if (!body || typeof body !== 'object') throw new Error('Invalid fixture request');
    const peer = peers.get(body.peer);
    if (!peer) throw new Error('Unknown fixture peer');
    switch (body.action) {
      case 'capabilities': return router.rtpCapabilities;
      case 'createTransport': {
        if (peer.transports.size >= 3) throw new Error('Fixture transport limit');
        if (!['send', 'audio', 'video'].includes(body.direction)) throw new Error('Invalid direction');
        const transport = await router.createWebRtcTransport({
          listenInfos: [{ protocol: 'udp', ip: '127.0.0.1' }],
          initialAvailableOutgoingBitrate: 2_000_000,
        });
        await transport.setMaxIncomingBitrate(5_000_000);
        peer.transports.set(transport.id, transport);
        return {
          id: transport.id, iceParameters: transport.iceParameters,
          iceCandidates: transport.iceCandidates, dtlsParameters: transport.dtlsParameters,
        };
      }
      case 'connect': {
        await owned(peer.transports, body.transportId).connect({ dtlsParameters: body.dtlsParameters });
        return {};
      }
      case 'produce': {
        if (!['audio', 'video'].includes(body.kind) || peer.producers.size >= 2) {
          throw new Error('Invalid fixture producer');
        }
        const producer = await owned(peer.transports, body.transportId).produce({
          kind: body.kind, rtpParameters: body.rtpParameters,
        });
        peer.producers.set(producer.id, producer);
        return { id: producer.id };
      }
      case 'consume': {
        const other = peers.get(body.peer === 'a' ? 'b' : 'a');
        const producer = owned(other.producers, body.producerId);
        if (peer.consumers.size >= 2) throw new Error('Fixture consumer limit');
        const consumer = await owned(peer.transports, body.transportId).consume({
          producerId: producer.id, rtpCapabilities: body.rtpCapabilities, paused: true,
        });
        peer.consumers.set(consumer.id, consumer);
        if (consumer.type === 'simulcast') await consumer.setPreferredLayers({ spatialLayer: 2 });
        return {
          id: consumer.id, producerId: producer.id, kind: consumer.kind,
          rtpParameters: consumer.rtpParameters,
        };
      }
      case 'resume': await owned(peer.consumers, body.consumerId).resume(); return {};
      case 'closeConsumer': {
        const consumer = owned(peer.consumers, body.consumerId);
        peer.consumers.delete(consumer.id); consumer.close(); return {};
      }
      case 'closeProducer': {
        const producer = owned(peer.producers, body.producerId);
        peer.producers.delete(producer.id); producer.close(); return {};
      }
      default: throw new Error('Unknown fixture action');
    }
  };
  return {
    version: mediasoup.version,
    nativeWorker: {
      path: workerPath, sha256: workerSha256,
      selection: process.env.MEDIASOUP_WORKER_BIN ? 'MEDIASOUP_WORKER_BIN override' : 'package default',
      candidateDigestBinding: expectedSha256 ? 'MATCHED supplied candidate digest' : 'NOT VERIFIED',
    },
    rpc,
    close: closeWorker,
  };
}
