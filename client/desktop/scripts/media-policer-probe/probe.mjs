#!/usr/bin/env node
// #2153 MEDIA-POLICER OVER-RATE PROBE (plan Task 7; spec §9 T0).
//
// WHY IT EXISTS. The policer's unit tests prove the arithmetic on injected counters; they
// cannot prove that a REAL over-rate Chromium sender trips a REAL media plane inside
// S + interval, nor that stock senders stay under 0.8x their limit. This drives two live
// dev clients over CDP and reports a verdict line. It is deliberately NOT wired to CI:
// it needs a GUI session, a running dev stack and two signed-in accounts.
//
// Judge a run by its PROBE line (PASS / FAIL / INEFFECTIVE / FIT / MISFIT), never by
// the exit code alone.
//
// C8: a per-tick rate series leaks speech activity. `envelope --out` refuses any path
// inside the repository; peaks are printed, the series goes only where you point it.
import { execFileSync, spawn } from 'node:child_process';
import { createRequire } from 'node:module';
import { appendFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const DESKTOP = path.resolve(HERE, '../..');
const REPO_ROOT = path.resolve(DESKTOP, '../..');
const DEV_ORIGIN = 'http://localhost:3001'; // main.ts:1071 loads exactly this in dev

// Node 24 strips types from an erasable-syntax .ts import: the probe reads the SAME
// constants the media plane enforces, so a T8 constant change cannot leave it stale.
const policer = await import(path.join(REPO_ROOT, 'services/media-plane/src/lib/mediaPolicer.ts'));
const TRIP_DEADLINE_MS = policer.TRIP_BUDGET_S * 1000 + policer.POLICER_INTERVAL_MS + 5_000;

// ── argument parsing ───────────────────────────────────────────────────────
function parseArgs(argv) {
  const [command, ...rest] = argv;
  const opts = {};
  for (let i = 0; i < rest.length; i++) {
    const key = rest[i];
    if (!key.startsWith('--')) throw new Error(`unexpected argument ${key}`);
    const next = rest[i + 1];
    if (next === undefined || next.startsWith('--')) {
      opts[key.slice(2)] = true;
    } else {
      opts[key.slice(2)] = next;
      i++;
    }
  }
  return { command, opts };
}
const num = (value, fallback) => (value === undefined ? fallback : Number(value));

// ── launch ─────────────────────────────────────────────────────────────────
function launch(opts) {
  const clients = num(opts.clients, 2);
  const basePort = num(opts['base-port'], 9301);
  // The real binary, resolved exactly as scripts/concord-dev.sh does: node_modules/.bin/electron
  // is a Node wrapper, so a detached launch would report the wrapper's pid, not Electron's.
  const electronBin = createRequire(path.join(DESKTOP, 'package.json'))('electron');
  for (let i = 0; i < clients; i++) {
    const userData = path.join(os.tmpdir(), `concord-policer-probe-${i + 1}`);
    const port = basePort + i;
    const args = [
      '.',
      `--user-data-dir=${userData}`, // separate userData = separate single-instance lock
      `--remote-debugging-port=${port}`,
      '--allow-loopback-in-peer-connection', // dev media plane announces 127.0.0.1
      '--use-fake-device-for-media-stream',
      '--use-fake-ui-for-media-stream',
    ];
    if (opts['ipv4-vite']) args.push('--host-resolver-rules=MAP localhost 127.0.0.1');
    const child = spawn(electronBin, args, {
      cwd: DESKTOP,
      env: { ...process.env, NODE_ENV: 'development' },
      stdio: 'ignore',
      detached: true,
    });
    child.unref();
    console.log(`client ${i + 1}: pid ${child.pid}  CDP ${port}  userData ${userData}`);
  }
  console.log('Sign in on each client and join the SAME voice channel, then run `status`.');
}

// ── CDP ────────────────────────────────────────────────────────────────────
async function connect(port) {
  const targets = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
  const page = targets.find(
    (t) => t.type === 'page' && t.url.startsWith(DEV_ORIGIN) && !t.url.includes('#/pip/')
  );
  if (!page) throw new Error(`no ${DEV_ORIGIN} page on CDP port ${port}`);
  const ws = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => {
    ws.onopen = resolve;
    ws.onerror = () => reject(new Error(`CDP socket failed on port ${port}`));
  });
  let seq = 0;
  const pending = new Map();
  ws.onmessage = (message) => {
    const msg = JSON.parse(message.data);
    const waiter = pending.get(msg.id);
    if (!waiter) return;
    pending.delete(msg.id);
    if (msg.error) waiter.reject(new Error(msg.error.message));
    else waiter.resolve(msg.result);
  };
  const send = (method, params) =>
    new Promise((resolve, reject) => {
      const id = ++seq;
      pending.set(id, { resolve, reject });
      ws.send(JSON.stringify({ id, method, params }));
    });
  const evaluate = async (expression) => {
    const r = await send('Runtime.evaluate', {
      expression,
      awaitPromise: true,
      returnByValue: true,
    });
    if (r.exceptionDetails) {
      throw new Error(
        `page threw: ${r.exceptionDetails.exception?.description ?? r.exceptionDetails.text}`
      );
    }
    return r.result.value;
  };
  await evaluate(PRELUDE);
  return { evaluate, close: () => ws.close() };
}

// Resolves the LIVE singletons through the vite dev server. Two candidate roots because
// the vite root decides the URL; the first that yields both exports wins.
const PRELUDE = `(async () => {
  if (globalThis.__policerProbe) return true;
  for (const root of ['/src/renderer', '']) {
    try {
      const vsMod = await import(root + '/services/voice/voiceService.ts');
      const storeMod = await import(root + '/stores/voice/voiceStore.ts');
      if (vsMod.voiceService && storeMod.useVoiceStore) {
        globalThis.__policerProbe = { vs: vsMod.voiceService, store: storeMod.useVoiceStore };
        return true;
      }
    } catch { /* next root */ }
  }
  throw new Error('voiceService is not reachable through the dev server');
})()`;

// HMR trap (live-WebRTC harness note §4): an edited module yields a FRESH instance whose
// transports are null while the untouched store still says connected.
const STATUS = `(() => {
  const { vs, store } = globalThis.__policerProbe;
  const s = store.getState();
  const status = {
    connectionState: s.connectionState,
    activeChannelId: s.activeChannelId,
    sendTransport: !!vs.sendTransport,
    isMuted: s.isMuted,
    mediaPolicyPaused: s.mediaPolicyPaused,
    mediaPolicyInterrupt: s.mediaPolicyInterrupt,
    producers: [...vs.producers.entries()].map(([source, p]) => ({ source, id: p.id, paused: p.paused, closed: p.closed })),
    participants: Object.values(s.participants).map((p) => ({ userId: p.userId, isMuted: p.isMuted, isCameraPaused: p.isCameraPaused ?? false })),
  };
  if (status.connectionState === 'connected' && !status.sendTransport) {
    throw new Error('SECOND MODULE INSTANCE (vite HMR): restart vite AND the clients, then retry');
  }
  return status;
})()`;

// One cumulative sample per live producer: payload + header bytes, packets.
const SAMPLE = `(async () => {
  const { vs, store } = globalThis.__policerProbe;
  const out = { t: performance.now(), latchedMic: store.getState().mediaPolicyPaused.mic ?? null, producers: {} };
  for (const [source, p] of vs.producers) {
    if (p.closed) continue;
    let bytes = 0, packets = 0;
    (await p.getStats()).forEach((r) => {
      if (r.type === 'outbound-rtp') { bytes += r.bytesSent + (r.headerBytesSent ?? 0); packets += r.packetsSent; }
    });
    out.producers[source] = { bytes, packets };
  }
  return out;
})()`;

// Re-produces the mic the way a non-cooperative client would (CV-CAN-020). Raising the
// stock mic's encodings[0].maxBitrate alone was INEFFECTIVE (93.7 kbps peak, 2026-09-23)
// because the stock stats loop rewrites it every 5 s; see stopPacketLossMonitor below. The
// SDP maxaveragebitrate does NOT bind the encoder (measured: declared 96000, sent 145 kbps at
// maxBitrate 144000). mediasoup-client writes it from codecOptions (RemoteMediaSection.js)
// into the local ANSWER only, never into the rtpParameters enforceAudioTierGate reads, so this
// producer declares ptime 20 and no bitrate, the gate admits it, and only measurement can
// catch it. The stock mic source is a sparse fake-device beep; white noise is incompressible,
// so Opus spends the whole target regardless of VBR.
const tripExpression = (maxBitrate) => `(async () => {
  const { vs } = globalThis.__policerProbe;
  const mic = vs.producers.get('mic');
  if (!mic || mic.closed) throw new Error('no live mic producer');
  if (mic.paused) throw new Error('mic is paused — unmute it before tripping');
  await vs.closeProducer('mic'); // the real path: server close, latch retire, mic cleanup
  await globalThis.__policerProbe.noiseCtx?.close();

  const ctx = new AudioContext({ sampleRate: 48000 });
  await ctx.resume();
  const noise = ctx.createBuffer(2, ctx.sampleRate * 2, ctx.sampleRate);
  for (let ch = 0; ch < 2; ch++) {
    const data = noise.getChannelData(ch);
    for (let i = 0; i < data.length; i++) data[i] = Math.random() * 2 - 1;
  }
  const src = ctx.createBufferSource();
  src.buffer = noise;
  src.loop = true;
  const dest = ctx.createMediaStreamDestination();
  src.connect(dest);
  src.start();
  globalThis.__policerProbe.noiseCtx = ctx;

  const producer = await vs.produceEncrypted(vs.sendTransport, {
    track: dest.stream.getAudioTracks()[0],
    encodings: [{ maxBitrate: ${Number(maxBitrate)} }],
    codecOptions: {
      opusStereo: true,
      opusDtx: false,
      opusFec: true,
      opusMaxAverageBitrate: ${Number(maxBitrate)},
      opusMaxPlaybackRate: 48000,
      opusPtime: 20,
    },
    appData: { source: 'mic' },
  });
  vs.producers.set('mic', producer); // so the owner notice matches a producer this client owns
  // The stock stats loop rewrites the mic's maxBitrate to tier x FEC headroom every 5 s
  // (measured: a flat 96k x 1.28 = 123 kbps from the second tick). A non-cooperative
  // client does not run it.
  vs.stopPacketLossMonitor();
  return { producerId: producer.id, applied: ${Number(maxBitrate)}, audioContext: ctx.state };
})()`;

// ANY git working tree, not just this checkout: an agent worktree sits INSIDE the main
// checkout, so a REPO_ROOT prefix test would let a series land where the main tree commits it.
function insideGitWorkTree(dir) {
  try {
    const r = execFileSync('git', ['-C', dir, 'rev-parse', '--is-inside-work-tree'], {
      stdio: ['ignore', 'pipe', 'ignore'],
    });
    return r.toString().trim() === 'true';
  } catch {
    return false; // not a repo, or the directory does not exist (the write then fails loudly)
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ── commands ───────────────────────────────────────────────────────────────
async function status(opts) {
  const c = await connect(num(opts.port, 9301));
  console.log(JSON.stringify(await c.evaluate(STATUS), null, 2));
  c.close();
}

async function tripAudio(opts) {
  const port = num(opts.port, 9301);
  const caps = {
    audioCeilingBps: num(opts.ceiling, 96_000),
    minPtimeMs: num(opts['min-ptime'], 20),
    maxManualBitrateBps: num(opts['max-manual'], 5_000_000),
  };
  const limit = policer.deriveLimits(caps).audioBps;
  const c = await connect(port);
  await c.evaluate(STATUS); // aborts on the HMR trap before touching anything
  const applied = await c.evaluate(tripExpression(num(opts['max-bitrate'], 510_000)));
  console.log(
    `trip-audio re-produced mic ${applied.producerId} at maxaveragebitrate=${applied.applied} (noise AudioContext ${applied.audioContext}); limit ${limit} bps`
  );

  // The policer trips on SUSTAINED excess, not a peak: debt += bits - limit*dt, trip at
  // limit*TRIP_BUDGET_S (mediaPolicer.ts settleSlot). Replaying that bucket on the client's
  // own counters is what separates a policer defect from a sender that never earned a trip.
  const budget = limit * policer.TRIP_BUDGET_S;
  const started = Date.now();
  let prev = await c.evaluate(SAMPLE);
  const first = prev;
  let debt = 0;
  let filledAt = null;
  let latchedAt = null;
  for (;;) {
    await sleep(1_000);
    const cur = await c.evaluate(SAMPLE);
    const a = prev.producers.mic;
    const b = cur.producers.mic;
    if (a && b) {
      debt = Math.max(0, debt + (b.bytes - a.bytes) * 8 - (limit * (cur.t - prev.t)) / 1000);
      if (filledAt === null && debt >= budget) filledAt = Date.now() - started;
    }
    prev = cur;
    if (cur.latchedMic) {
      latchedAt = Date.now() - started;
      break;
    }
    const elapsed = Date.now() - started;
    const giveUpAt =
      filledAt === null ? TRIP_DEADLINE_MS : filledAt + policer.POLICER_INTERVAL_MS + 5_000;
    if (elapsed >= giveUpAt) break;
  }
  c.close();
  const a = first.producers.mic;
  const b = prev.producers.mic;
  const mean = a && b ? Math.round(((b.bytes - a.bytes) * 8) / ((prev.t - first.t) / 1000)) : 0;
  if (latchedAt !== null) {
    console.log(
      `PROBE PASS trip-audio: latched after ${latchedAt} ms (client bucket filled at ${filledAt ?? 'never'} ms; mean ${mean} bps vs limit ${limit})`
    );
    return 0;
  }
  if (filledAt === null) {
    console.log(
      `PROBE INEFFECTIVE trip-audio: mean ${mean} bps vs limit ${limit}; the sustained excess never filled the ${policer.TRIP_BUDGET_S}-s bucket, so no trip was owed (see README "Method")`
    );
    return 2;
  }
  console.log(
    `PROBE FAIL trip-audio: client bucket filled at ${filledAt} ms (mean ${mean} bps vs limit ${limit}) yet no latch within ${policer.POLICER_INTERVAL_MS + 5_000} ms of it`
  );
  return 1;
}

async function envelope(opts) {
  const port = num(opts.port, 9301);
  const seconds = num(opts.seconds, 600);
  const label = String(opts.label ?? 'unlabelled');
  const out = opts.out ? path.resolve(String(opts.out)) : null;
  if (out && insideGitWorkTree(path.dirname(out))) {
    throw new Error('refusing --out inside the repository: a rate series is never committed (C8)');
  }
  const caps = {
    audioCeilingBps: num(opts.ceiling, 96_000),
    minPtimeMs: num(opts['min-ptime'], 20),
    maxManualBitrateBps: num(opts['max-manual'], 5_000_000),
  };
  const limits = policer.deriveLimits(caps);
  const c = await connect(port);
  await c.evaluate(STATUS);
  if (opts['camera-on'])
    await c.evaluate(
      `(async () => { const { vs, store } = globalThis.__policerProbe; if (!store.getState().isVideoOn) await vs.toggleVideo(); return true; })()`
    );

  const samples = [await c.evaluate(SAMPLE)];
  for (let i = 0; i < seconds; i++) {
    await sleep(1_000);
    const s = await c.evaluate(SAMPLE);
    samples.push(s);
    if (out) appendFileSync(out, `${JSON.stringify({ label, ...s })}\n`);
  }
  c.close();

  // Peak rate over any 10-s window, per source, and the aggregate across all of them.
  const WINDOW = 10;
  const peaks = {};
  let peakAggregate = 0;
  for (let i = WINDOW; i < samples.length; i++) {
    const a = samples[i - WINDOW];
    const b = samples[i];
    const dt = (b.t - a.t) / 1000;
    let aggregate = 0;
    for (const [source, cur] of Object.entries(b.producers)) {
      const old = a.producers[source];
      if (!old) continue;
      const rate = ((cur.bytes - old.bytes) * 8) / dt;
      const pps = (cur.packets - old.packets) / dt;
      aggregate += rate;
      const p = (peaks[source] ??= { bps: 0, pps: 0 });
      p.bps = Math.max(p.bps, rate);
      p.pps = Math.max(p.pps, pps);
    }
    peakAggregate = Math.max(peakAggregate, aggregate);
  }

  // Fit criterion (spec §9): every stock peak <= 0.8 x its limit.
  const rows = [];
  for (const source of ['mic', 'screen-audio']) {
    if (!peaks[source]) continue;
    rows.push({ check: `${source} bytes`, peak: peaks[source].bps, limit: limits.audioBps });
    rows.push({ check: `${source} pps`, peak: peaks[source].pps, limit: limits.audioPps });
  }
  rows.push({ check: 'aggregate', peak: peakAggregate, limit: limits.aggregateBps });
  let fit = true;
  for (const r of rows) {
    const ratio = r.peak / r.limit;
    if (ratio > 0.8) fit = false;
    console.log(
      `${label}  ${r.check.padEnd(20)} peak ${Math.round(r.peak)}  limit ${r.limit}  ratio ${ratio.toFixed(3)}`
    );
  }
  console.log(
    `PROBE ${fit ? 'FIT' : 'MISFIT'} envelope ${label} (${seconds}s, client-side outbound incl. RTP headers)`
  );
  return fit ? 0 : 3;
}

// ── main ───────────────────────────────────────────────────────────────────
const { command, opts } = parseArgs(process.argv.slice(2));
const commands = { launch, status, 'trip-audio': tripAudio, envelope };
if (!commands[command]) {
  console.error(
    'usage: probe.mjs <launch|status|trip-audio|envelope> [--port N] …  (see README.md)'
  );
  process.exit(64);
}
try {
  const code = await commands[command](opts);
  process.exit(typeof code === 'number' ? code : 0);
} catch (err) {
  console.error(`PROBE ERROR ${command}: ${err instanceof Error ? err.message : String(err)}`);
  process.exit(1);
}
