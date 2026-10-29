import { Device } from 'mediasoup-client';
import { voiceService } from '/src/renderer/services/voice/voiceService.ts';
import { MediaEncryption, deriveFrameKey } from '/src/renderer/services/e2ee/mediaEncryption.ts';
import { importChannelKey } from '/src/renderer/utils/crypto/crypto.ts';
import { requireVoiceProbeContract } from './witness-contract.mjs';
import { createRemoteAudioWitness } from './audio-witness.mjs';
import { buildCameraEncodingPlan } from '/src/renderer/services/voice/cameraLayering.ts';
import { createAvSyncDriftDetector, mergeAvSyncStats } from '/src/renderer/services/voice/avSyncDriftDetector.ts';

// Fixture installation replaces only account/key/signaling setup. All RTC objects,
// production transforms, worker messages and sampler observations remain real.
const peer = new URL(location.href).searchParams.get('peer');
if (!['a', 'b'].includes(peer)) throw new Error('Invalid fixture peer');
const senderId = peer === 'a' ? '00000000-0000-4000-8000-000000000001' : '00000000-0000-4000-8000-000000000002';
const remoteId = peer === 'a' ? '00000000-0000-4000-8000-000000000002' : '00000000-0000-4000-8000-000000000001';
const rpc = async (action, params = {}) => {
  const response = await fetch('/__voice_runtime_probe/rpc', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    signal: AbortSignal.timeout(10_000),
    body: JSON.stringify({ peer, action, ...params }),
  });
  if (!response.ok) throw new Error(`Local SFU fixture failed: ${action}`);
  return response.json();
};
let device, send, audioRecv, videoRecv, worker, audioContext, oscillator, drawTimer;
let audioTrack, videoTrack, videoProducer, videoConsumer, independentTimer;
let videoElement, analyser, audioWitness;
let videoProgress;
let startedAt = 0;
let independentUsableCount = 0;
let independentDrift = false;
let injectedDetectorTripped = false;
let independentInFlight = false;
const refused = new Set();
const enteredFrames = new Map();
const consumers = new Map();
const producers = new Map();
const independent = createAvSyncDriftDetector();
const faultControl = createAvSyncDriftDetector();

async function makeTransport(direction) {
  const options = await rpc('createTransport', { direction });
  const transport = direction === 'send'
    ? device.createSendTransport({ ...options, iceServers: [] })
    : device.createRecvTransport({ ...options, iceServers: [] });
  transport.on('connect', ({ dtlsParameters }, callback, errback) => {
    rpc('connect', { transportId: transport.id, dtlsParameters }).then(() => callback(), errback);
  });
  if (direction === 'send') {
    transport.on('produce', ({ kind, rtpParameters }, callback, errback) => {
      rpc('produce', { transportId: transport.id, kind, rtpParameters }).then(callback, errback);
    });
  }
  voiceService.recordSelectedCandidatePair(transport, `probe-${direction}`);
  return transport;
}

async function setup(rawTestKey) {
  requireVoiceProbeContract(voiceService);
  if (typeof RTCRtpScriptTransform !== 'function') throw new Error('Script transforms unavailable');
  const csk = await importChannelKey(new Uint8Array(rawTestKey).buffer);
  const encryption = new MediaEncryption();
  await encryption.init(csk, senderId);
  encryption.setKeyVersion(1);
  voiceService.mediaEncryption = encryption;
  worker = voiceService.createE2EEWorker();
  voiceService.e2eeWorker = worker;
  worker.addEventListener('message', ({ data }) => {
    if (data.type === 'decryptStats') enteredFrames.set(data.probeId, data.entered);
  });
  worker.postMessage({ type: 'init', encryptKey: await deriveFrameKey(csk, senderId), currentKeyId: 0, keyVersion: 1 });
  worker.postMessage({ type: 'addDecryptKey', senderUserId: remoteId, keyVersion: 1, keyId: 0,
    key: await encryption.addDecryptKeyAtVersion(csk, remoteId, 1, 0) });
  device = new Device();
  await device.load({ routerRtpCapabilities: await rpc('capabilities') });
  send = await makeTransport('send');
  audioRecv = await makeTransport('audio');
  videoRecv = await makeTransport('video');
  voiceService.recvTransportAudio = audioRecv;
  voiceService.recvTransportVideo = videoRecv;

  audioContext = new AudioContext();
  await audioContext.resume();
  oscillator = audioContext.createOscillator();
  oscillator.frequency.value = peer === 'a' ? 440 : 660;
  const gain = audioContext.createGain(); gain.gain.value = 0.1;
  const destination = audioContext.createMediaStreamDestination();
  oscillator.connect(gain).connect(destination); oscillator.start();
  [audioTrack] = destination.stream.getAudioTracks();
  const canvas = document.createElement('canvas'); canvas.width = 640; canvas.height = 360;
  document.body.append(canvas);
  const paint = canvas.getContext('2d');
  let frame = 0;
  drawTimer = setInterval(() => {
    paint.fillStyle = frame++ % 2 ? '#e84040' : '#30c070'; paint.fillRect(0, 0, 640, 360);
    paint.fillStyle = '#ffffff'; paint.font = '40px sans-serif'; paint.fillText(String(frame), 40, 80);
  }, 1000 / 15);
  [videoTrack] = canvas.captureStream(15).getVideoTracks();
  return { handler: device.handlerName, nodeVisibleInRenderer: typeof globalThis.process !== 'undefined' };
}

async function publishVideo() {
  const codec = device.rtpCapabilities.codecs.find((entry) => entry.mimeType.toLowerCase() === 'video/vp8');
  if (!codec) throw new Error('VP8 unavailable');
  const plan = buildCameraEncodingPlan({ codec, maxBitrate: 1_200_000, scalabilityMode: 'L3T3',
    priority: { priority: 'medium', networkPriority: 'medium' }, eligibility: { svc: true, simulcast: true } });
  videoProducer = await voiceService.produceEncrypted(send, {
    track: videoTrack, codec, encodings: plan.encodings, stopTracks: false, appData: { source: 'camera' },
  });
  producers.set('video', videoProducer);
  return { id: videoProducer.id, kind: 'video' };
}

async function publish() {
  const audio = await voiceService.produceEncrypted(send, {
    track: audioTrack, stopTracks: false, encodings: [{ maxBitrate: 64_000 }],
    codecOptions: { opusDtx: false, opusPtime: 20 }, appData: { source: 'mic' },
  });
  producers.set('audio', audio);
  return [{ id: audio.id, kind: 'audio' }, await publishVideo()];
}

async function consume(descriptor) {
  const transport = descriptor.kind === 'audio' ? audioRecv : videoRecv;
  const result = await rpc('consume', { transportId: transport.id, producerId: descriptor.id,
    rtpCapabilities: device.rtpCapabilities });
  const consumer = await transport.consume({ ...result,
    ...voiceService.creationAttachConsumeOption({ ...result, producerUserId: remoteId }, remoteId) });
  consumers.set(consumer.id, consumer);
  voiceService.consumers.set(consumer.id, consumer);
  voiceService.applyDecryptTransform(consumer, remoteId);
  if (consumer.kind === 'audio') {
    audioWitness = createRemoteAudioWitness(audioContext, new MediaStream([consumer.track]), document);
    analyser = audioWitness.analyser;
  } else {
    videoConsumer = consumer;
    videoElement ??= document.createElement('video');
    videoElement.autoplay = true; videoElement.muted = true;
    document.body.append(videoElement);
    videoElement.srcObject = new MediaStream([consumer.track]);
    if (typeof videoElement.requestVideoFrameCallback !== 'function') throw new Error('Video presentation counter unavailable');
    const progress = { consumerId: consumer.id, framesDecoded: 0, presentedFrames: 0,
      lastDecodedAt: 0, lastPresentedAt: 0, callback: null };
    videoProgress = progress;
    const presented = (now, metadata) => {
      if (videoProgress !== progress) return;
      if (metadata.presentedFrames > progress.presentedFrames) {
        progress.presentedFrames = metadata.presentedFrames;
        progress.lastPresentedAt = now;
      }
      progress.callback = videoElement.requestVideoFrameCallback(presented);
    };
    progress.callback = videoElement.requestVideoFrameCallback(presented);
  }
  await rpc('resume', { consumerId: consumer.id });
  if (consumer.kind === 'audio') await audioWitness.start();
  if (consumer.kind === 'video') await videoElement.play();
}

async function independentTick() {
  if (independentInFlight) return;
  independentInFlight = true;
  try {
    const observedAt = performance.timeOrigin + performance.now();
    const [audio, video] = await Promise.all([audioRecv.getStats(), videoRecv.getStats()]);
    const entries = mergeAvSyncStats(audio.values(), video.values());
    const observation = independent.observe(entries, observedAt);
    if (observation.usable) { independentUsableCount++; independentDrift ||= observation.verdict === 'drift'; }
    else refused.add(observation.reason);
    // An explicitly separate detector positive control. RTC reports and the
    // production sampler are untouched; only this fork gets an imposed 2 ms/s drift.
    const fork = entries.map((entry) => entry.type === 'remote-outbound-rtp' && entry.id.startsWith('a:')
      ? { ...entry, remoteTimestamp: entry.remoteTimestamp + (performance.now() - startedAt) * 0.002 }
      : entry);
    const control = faultControl.observe(fork, observedAt);
    injectedDetectorTripped ||= control.usable && control.verdict === 'drift';
  } finally { independentInFlight = false; }
}

function arm() {
  startedAt = performance.now();
  voiceService.startAvSyncSampling();
  independentTimer = setInterval(() => { void independentTick(); }, 5000);
}

function updatePriority() {
  voiceService.liveUpdateVideoPriority(videoProducer, 'high');
}

async function witness() {
  for (const consumer of consumers.values()) worker.postMessage({ type: 'queryDecryptStats', probeId: consumer.id });
  const report = await videoConsumer.getStats();
  const audioConsumer = [...consumers.values()].find((consumer) => consumer.kind === 'audio');
  const audioReport = await audioConsumer.getStats();
  const sendReport = await send.getStats();
  let audioPacketsReceived, videoPacketsReceived, videoBytesReceived, outboundPacketsSent, outboundVideoFramesEncoded;
  audioReport.forEach((entry) => { if (entry.type === 'inbound-rtp') audioPacketsReceived = entry.packetsReceived; });
  report.forEach((entry) => {
    if (entry.type === 'inbound-rtp') { videoPacketsReceived = entry.packetsReceived; videoBytesReceived = entry.bytesReceived; }
  });
  sendReport.forEach((entry) => {
    if (entry.type !== 'outbound-rtp') return;
    if (typeof entry.packetsSent === 'number') outboundPacketsSent = (outboundPacketsSent ?? 0) + entry.packetsSent;
    if (typeof entry.framesEncoded === 'number') outboundVideoFramesEncoded = (outboundVideoFramesEncoded ?? 0) + entry.framesEncoded;
  });
  let framesDecoded = 0;
  report.forEach((entry) => { if (entry.type === 'inbound-rtp') framesDecoded += entry.framesDecoded ?? 0; });
  const now = performance.now();
  if (framesDecoded > videoProgress.framesDecoded) {
    videoProgress.framesDecoded = framesDecoded;
    videoProgress.lastDecodedAt = now;
  }
  const wave = new Float32Array(analyser.fftSize); analyser.getFloatTimeDomainData(wave);
  const audioRms = Math.sqrt(wave.reduce((sum, value) => sum + value * value, 0) / wave.length);
  const output = document.createElement('canvas'); output.width = 32; output.height = 32;
  const context = output.getContext('2d');
  if (videoElement.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA) context.drawImage(videoElement, 0, 0, 32, 32);
  const pixels = context.getImageData(0, 0, 32, 32).data;
  let colored = 0;
  for (let i = 0; i < pixels.length; i += 4) if (pixels[i] + pixels[i + 1] + pixels[i + 2] > 50) colored++;
  const encodings = videoProducer.rtpSender.getParameters().encodings;
  return {
    transportStates: { send: send.connectionState, audioReceive: audioRecv.connectionState, videoReceive: videoRecv.connectionState },
    iceGatheringStates: { send: send.iceGatheringState, audioReceive: audioRecv.iceGatheringState, videoReceive: videoRecv.iceGatheringState },
    audioContextState: audioContext.state, audioPacketsReceived, videoPacketsReceived, videoBytesReceived,
    outboundPacketsSent, outboundVideoFramesEncoded,
    measuredDurationMs: startedAt ? performance.now() - startedAt : 0,
    productionTimerArmed: voiceService.avSyncSamplingTimer !== null,
    productionObservationCount: voiceService.avSyncObservationCount,
    productionDrift: voiceService.avSyncDriftEmitted,
    independentUsableCount, independentDrift, independentRefusals: [...refused], injectedDetectorTripped,
    audioSignalPresent: audioRms > 0.005, decodedVideoPresent: framesDecoded > 5 && colored > 500,
    videoConsumerId: videoConsumer.id, framesDecoded,
    presentedFrames: videoProgress.presentedFrames,
    decodedProgressAgeMs: videoProgress.lastDecodedAt ? now - videoProgress.lastDecodedAt : Number.MAX_SAFE_INTEGER,
    presentedProgressAgeMs: videoProgress.lastPresentedAt ? now - videoProgress.lastPresentedAt : Number.MAX_SAFE_INTEGER,
    e2eeVideoEntered: enteredFrames.get(videoConsumer.id) ?? 0,
    e2eeEntered: [...consumers.keys()].map((id) => enteredFrames.get(id) ?? 0),
    consumerEvidence: [...consumers.values()].map((consumer) => ({ id: consumer.id, kind: consumer.kind,
      entered: enteredFrames.get(consumer.id) ?? 0 })),
    sourceTracksLive: audioTrack.readyState === 'live' && videoTrack.readyState === 'live',
    layeredPriorityApplied: encodings.length > 1 && encodings[0].priority === 'high' &&
      encodings.slice(1).every((entry) => (!entry.priority || entry.priority === 'low') &&
        (!entry.networkPriority || entry.networkPriority === 'low')),
    audioExtensionsNegotiated: producers.get('audio').rtpParameters.headerExtensions.length > 0,
    videoExtensionsNegotiated: videoProducer.rtpParameters.headerExtensions.length > 0,
  };
}

async function closeRemoteVideo() {
  videoElement.cancelVideoFrameCallback(videoProgress.callback);
  videoProgress = null;
  await rpc('closeConsumer', { consumerId: videoConsumer.id });
  consumers.delete(videoConsumer.id); voiceService.consumers.delete(videoConsumer.id);
  videoConsumer.close(); videoConsumer = null;
}

async function reproduceVideo() {
  await rpc('closeProducer', { producerId: videoProducer.id });
  videoProducer.close();
  if (videoTrack.readyState !== 'live') throw new Error('Producer close ended reusable capture track');
  return publishVideo();
}

async function cleanup() {
  if (videoProgress) videoElement.cancelVideoFrameCallback(videoProgress.callback);
  videoProgress = null;
  clearInterval(independentTimer); clearInterval(drawTimer);
  clearInterval(voiceService.avSyncSamplingTimer);
  voiceService.avSyncSamplingTimer = null;
  for (const consumer of consumers.values()) consumer.close();
  for (const producer of producers.values()) producer.close();
  for (const transport of [send, audioRecv, videoRecv]) transport?.close();
  audioTrack?.stop(); videoTrack?.stop(); oscillator?.stop();
  audioWitness?.close();
  await audioContext?.close();
  voiceService.terminateE2EEWorker(); voiceService.mediaEncryption?.destroy();
}

globalThis.mediaProbe = { setup, publish, consume, arm, updatePriority, witness, closeRemoteVideo, reproduceVideo, cleanup };
