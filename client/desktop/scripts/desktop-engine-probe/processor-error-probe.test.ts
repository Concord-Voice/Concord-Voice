// @vitest-environment node
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createRequire } from 'node:module';
import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';

const require = createRequire(import.meta.url);
const { probeNativeProcessorError } = require('./processor-error-probe.js');

// A contract model only: actual native events and sound require the engine run.
function fixture(
  options: {
    trusted?: boolean;
    correctTarget?: boolean;
    callbacks?: number;
    event?: boolean;
    closeFails?: boolean;
    disconnectFails?: boolean;
    renderCompletes?: boolean;
    compatibilityError?: boolean;
    compatibilityRetires?: boolean;
    compatibilityTrusted?: boolean;
    compatibilityCorrectTarget?: boolean;
    compatibilityType?: string;
    compatibilityEvents?: number;
    compatibilityFirst?: boolean;
    standardType?: string;
    standardEvents?: number;
  } = {}
) {
  type ProbeEvent = { isTrusted: boolean; type: string; target: unknown };
  const listeners = new Map<string, Set<(event: ProbeEvent) => void>>();
  const markerListeners = new Set<(event: { data: unknown }) => void>();
  const calls: string[] = [];
  const inputCalls: string[] = [];
  let closed = false;
  let failed = false;
  const node = {
    port: {
      postMessage() {},
      addEventListener(_name: string, callback: (event: { data: unknown }) => void) {
        markerListeners.add(callback);
      },
      removeEventListener(_name: string, callback: (event: { data: unknown }) => void) {
        markerListeners.delete(callback);
      },
      close() {
        expect(this).toBe(node.port);
        if (options.closeFails) throw new Error('Native close model failure');
        calls.push('port.close');
      },
    },
    connect() {},
    disconnect() {
      expect(this).toBe(node);
      if (options.disconnectFails) throw new Error('Native disconnect model failure');
      calls.push('disconnect');
    },
    addEventListener(name: string, callback: (event: ProbeEvent) => void) {
      if (!listeners.has(name)) listeners.set(name, new Set());
      listeners.get(name)?.add(callback);
    },
    removeEventListener(name: string, callback: (event: ProbeEvent) => void) {
      listeners.get(name)?.delete(callback);
    },
  };
  const close = () => {
    if (closed) return;
    closed = true;
    try {
      node.port.close();
    } catch {
      /* production teardown continues */
    }
    try {
      node.disconnect();
    } catch {
      /* production teardown is best effort */
    }
  };
  const createProcessor = async (
    _context: unknown,
    _initial: unknown,
    _onWindow: unknown,
    onError: () => void
  ) => {
    const fail = () => {
      if (closed || failed) return;
      failed = true;
      for (let n = 0; n < (options.callbacks ?? 1); n++) onError();
      close();
    };
    node.addEventListener('processorerror', fail);
    if (options.compatibilityRetires !== false) node.addEventListener('error', fail);
    return { node, close };
  };
  const input = {
    buffer: null as { getChannelData(channel: number): Float32Array } | null,
    connect(target: unknown) {
      expect(target).toBe(node);
      inputCalls.push('connect');
    },
    start() {
      expect(input.buffer?.getChannelData(0)[0]).toBe(1);
      inputCalls.push('start');
    },
    stop() {
      inputCalls.push('stop');
    },
    disconnect() {
      inputCalls.push('disconnect');
    },
  };
  const context = {
    destination: {},
    sampleRate: 48000,
    createBuffer(channels: number, frames: number, rate: number) {
      expect([channels, frames, rate]).toEqual([1, 4800, 48000]);
      const samples = new Float32Array(frames);
      return {
        getChannelData(channel: number) {
          expect(channel).toBe(0);
          return samples;
        },
      };
    },
    createBufferSource() {
      return input;
    },
    async startRendering() {
      inputCalls.push('render');
      for (const listener of markerListeners) {
        listener({ data: { type: 'engine-probe-constructor-entered' } });
        listener({ data: { type: 'engine-probe-process-entered' } });
      }
      const standard = () => {
        if (options.event === false) return;
        const event = {
          isTrusted: options.trusted !== false,
          type: options.standardType ?? 'processorerror',
          target: options.correctTarget !== false ? node : {},
        };
        for (let n = 0; n < (options.standardEvents ?? 1); n++)
          for (const listener of listeners.get('processorerror') ?? []) listener(event);
      };
      const compatibility = () => {
        if (!options.compatibilityError) return;
        const event = {
          isTrusted: options.compatibilityTrusted !== false,
          type: options.compatibilityType ?? 'error',
          target: options.compatibilityCorrectTarget !== false ? node : {},
        };
        for (let n = 0; n < (options.compatibilityEvents ?? 1); n++)
          for (const listener of listeners.get('error') ?? []) listener(event);
      };
      for (const deliver of options.compatibilityFirst
        ? [compatibility, standard]
        : [standard, compatibility])
        deliver();
      if (options.renderCompletes === false) await new Promise(() => {});
    },
  };
  return { node, calls, inputCalls, context, createProcessor };
}

afterEach(() => vi.useRealTimers());

describe('native failure evidence and production host retirement contract', () => {
  it('the labeled fault fixture constructs normally and throws from its actual process method', () => {
    let Processor: (new () => { process(): void }) | undefined;
    const markers: unknown[] = [];
    runInNewContext(readFileSync(new URL('./process-error.worklet.js', import.meta.url), 'utf8'), {
      AudioWorkletProcessor: class {
        port = { postMessage: (data: unknown) => markers.push(data) };
      },
      registerProcessor(name: string, value: typeof Processor) {
        expect(name).toBe('concord-mic-processor');
        Processor = value;
      },
    });
    if (!Processor) throw new Error('Fault processor was not registered');
    const processor = new Processor();
    expect(() => processor.process()).toThrow('Deliberate engine probe process failure');
    expect(markers).toEqual([
      { type: 'engine-probe-constructor-entered' },
      { type: 'engine-probe-process-entered' },
    ]);
  });

  it('requires the trusted node event, one callback and successful forwarded retirement', async () => {
    const f = fixture();
    const close = f.node.port.close;
    const disconnect = f.node.disconnect;
    await expect(probeNativeProcessorError(f.context, f.createProcessor)).resolves.toEqual({
      eventMode: 'processorerror',
      standardProcessorError: 'PASS',
      compatibilityError: 'NOT RUN',
      nativeEvents: 1,
      trustedEvents: 1,
      errorEvents: 0,
      trustedErrorEvents: 0,
      callbacks: 1,
      portCloses: 1,
      disconnects: 1,
    });
    expect(f.calls).toEqual(['port.close', 'disconnect']);
    expect(f.node.port.close).toBe(close);
    expect(f.node.disconnect).toBe(disconnect);
    expect(f.inputCalls).toEqual(['connect', 'start', 'render', 'stop', 'disconnect']);
  });

  it.each([
    { trusted: false },
    { correctTarget: false },
    { standardType: 'error' },
    { standardEvents: 2 },
    { callbacks: 0 },
    { callbacks: 2 },
    { closeFails: true },
    { disconnectFails: true },
  ])('refuses incomplete native evidence: %j', async (options) => {
    const f = fixture(options);
    await expect(probeNativeProcessorError(f.context, f.createProcessor)).rejects.toThrow(
      'not observed exactly once'
    );
  });

  it('does not count manual cleanup or silence as native failure evidence', async () => {
    vi.useFakeTimers();
    const f = fixture({ event: false });
    const result = expect(probeNativeProcessorError(f.context, f.createProcessor)).rejects.toThrow(
      '"renderCompleted":true,"nativeEvents":0'
    );
    await vi.advanceTimersByTimeAsync(2000);
    await result;
    expect(f.calls).toEqual(['port.close', 'disconnect']);
  });

  it('distinguishes missing render completion from an absent native event without accepting markers', async () => {
    vi.useFakeTimers();
    const f = fixture({ renderCompletes: false });
    const result = expect(probeNativeProcessorError(f.context, f.createProcessor)).rejects.toThrow(
      '"renderCompleted":false,"nativeEvents":1,"trustedEvents":1,"callbacks":1,"portCloses":1,"disconnects":1'
    );
    await vi.advanceTimersByTimeAsync(2000);
    await result;
    expect(f.calls).toEqual(['port.close', 'disconnect']);
    expect(f.inputCalls).toEqual(['connect', 'start', 'render', 'stop', 'disconnect']);
  });

  it.each([
    { event: false, compatibilityFirst: false, eventMode: 'error', standardCount: 0 },
    { event: true, compatibilityFirst: false, eventMode: 'both', standardCount: 1 },
    { event: true, compatibilityFirst: true, eventMode: 'both', standardCount: 1 },
  ])(
    'records verified compatibility delivery without relabeling the standard contract: %j',
    async (row) => {
      const f = fixture({ ...row, compatibilityError: true });
      await expect(probeNativeProcessorError(f.context, f.createProcessor)).resolves.toEqual({
        eventMode: row.eventMode,
        standardProcessorError: row.standardCount ? 'PASS' : 'BROKEN',
        compatibilityError: 'PASS',
        nativeEvents: row.standardCount,
        trustedEvents: row.standardCount,
        errorEvents: 1,
        trustedErrorEvents: 1,
        callbacks: 1,
        portCloses: 1,
        disconnects: 1,
      });
      expect(f.calls).toEqual(['port.close', 'disconnect']);
      expect(f.inputCalls).toEqual(['connect', 'start', 'render', 'stop', 'disconnect']);
    }
  );

  it.each([
    { compatibilityTrusted: false },
    { compatibilityCorrectTarget: false },
    { compatibilityType: 'processorerror' },
    { compatibilityEvents: 2 },
    { callbacks: 0 },
    { callbacks: 2 },
    { closeFails: true },
    { disconnectFails: true },
    { compatibilityRetires: false },
  ])(
    'refuses compatibility delivery without its own trusted target and actual retirement: %j',
    async (options) => {
      const f = fixture({ ...options, event: false, compatibilityError: true });
      await expect(probeNativeProcessorError(f.context, f.createProcessor)).rejects.toThrow(
        'not observed exactly once'
      );
    }
  );

  it.each([{ trusted: false }, { compatibilityTrusted: false }])(
    'refuses cross-kind trust confusion even with a valid event of the other name: %j',
    async (options) => {
      const f = fixture({ ...options, compatibilityError: true });
      await expect(probeNativeProcessorError(f.context, f.createProcessor)).rejects.toThrow(
        'not observed exactly once'
      );
    }
  );
});
