import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('@/renderer/services/voice/micProcessor.worklet.js?url&no-inline', () => ({
  default: '/assets/micProcessor.worklet.js',
}));

import { createMicProcessor, gateForAudioSettings } from '@/renderer/services/voice/micProcessor';

type WorkletMessage = { data: unknown };
type MockPort = {
  onmessage: ((event: WorkletMessage) => void) | null;
  postMessage: ReturnType<typeof vi.fn>;
  close: ReturnType<typeof vi.fn>;
};

class MockNode {
  port: MockPort = {
    onmessage: null,
    postMessage: vi.fn(),
    close: vi.fn(),
  };
  addEventListener = vi.fn((event: string, listener: EventListener) => {
    if (event === 'processorerror') this.processorError = listener;
  });
  disconnect = vi.fn();
  processorError: EventListener | undefined;

  emit(data: unknown) {
    this.port.onmessage?.({ data });
  }

  fail() {
    this.processorError?.(new Event('processorerror'));
  }
}

const initial = { protectAgcPeaks: true, gate: { kind: 'off' as const } };
let addModule: ReturnType<typeof vi.fn>;
let context: AudioContext;
let constructed: MockNode[];
let constructorFailure: Error | undefined;

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

beforeEach(() => {
  constructed = [];
  constructorFailure = undefined;
  addModule = vi.fn().mockResolvedValue(undefined);
  context = { sampleRate: 48_000, audioWorklet: { addModule } } as unknown as AudioContext;
  vi.stubGlobal(
    'AudioWorkletNode',
    class extends MockNode {
      constructor(_context: AudioContext, _name: string, _options: unknown) {
        super();
        if (constructorFailure) throw constructorFailure;
        constructed.push(this);
      }
    }
  );
});

describe('createMicProcessor', () => {
  it.each([
    [
      { musicMode: false, autoGainControl: true, noiseGateMode: 'dynamic', noiseGateLevel: -45 },
      { kind: 'dynamic' },
    ],
    [
      {
        musicMode: false,
        autoGainControl: true,
        noiseGateMode: 'manualCalibrate',
        noiseGateLevel: -45,
      },
      { kind: 'dynamic' },
    ],
    [
      {
        musicMode: true,
        autoGainControl: true,
        noiseGateMode: 'manualCalibrate',
        noiseGateLevel: -45,
      },
      { kind: 'fixed', thresholdDbfs: -45 },
    ],
    [
      {
        musicMode: false,
        autoGainControl: false,
        noiseGateMode: 'manualCalibrate',
        noiseGateLevel: -60,
      },
      { kind: 'fixed', thresholdDbfs: -60 },
    ],
    [
      { musicMode: false, autoGainControl: true, noiseGateMode: 'off', noiseGateLevel: -45 },
      { kind: 'off' },
    ],
  ] as const)('resolves effective gate policy for %o', (settings, expected) => {
    expect(gateForAudioSettings(settings)).toEqual(expected);
  });

  it('accepts Dynamic as an initial and runtime gate command', async () => {
    const { setGate } = await createMicProcessor(
      context,
      { protectAgcPeaks: false, gate: { kind: 'dynamic' } },
      vi.fn(),
      vi.fn()
    );

    setGate({ kind: 'dynamic' });
    expect(constructed[0].port.postMessage).toHaveBeenCalledWith({
      type: 'setGate',
      gate: { kind: 'dynamic' },
    });
  });

  it('delivers a valid Dynamic status through the optional callback and preserves peak reports', async () => {
    const onWindow = vi.fn();
    const onGateStatus = vi.fn();
    await createMicProcessor(context, initial, onWindow, vi.fn(), onGateStatus);
    constructed[0].emit({
      type: 'dynamicGateStatus',
      state: 'adjusted',
      thresholdDbfs: -42,
    });
    constructed[0].emit({ peak: 0.02, frames: 960, valid: true, overloaded: false });

    expect(onGateStatus).toHaveBeenCalledWith({
      type: 'dynamicGateStatus',
      state: 'adjusted',
      thresholdDbfs: -42,
    });
    expect(onWindow).toHaveBeenCalledWith({
      peak: 0.02,
      frames: 960,
      valid: true,
      overloaded: false,
    });
  });

  it.each([
    { type: 'dynamicGateStatus', state: 'learning', thresholdDbfs: null },
    { type: 'dynamicGateStatus', state: 'uncertain', thresholdDbfs: null },
    { type: 'dynamicGateStatus', state: 'adjusted', thresholdDbfs: -80 },
    { type: 'dynamicGateStatus', state: 'adjusted', thresholdDbfs: -20 },
  ])('accepts supported Dynamic status: %o', async (status) => {
    const onGateStatus = vi.fn();
    await createMicProcessor(context, initial, vi.fn(), vi.fn(), onGateStatus);
    constructed[0].emit(status);
    expect(onGateStatus).toHaveBeenCalledWith(status);
  });

  it.each([
    { type: 'dynamicGateStatus', state: 'adjusted', thresholdDbfs: null },
    { type: 'dynamicGateStatus', state: 'learning', thresholdDbfs: -40 },
    { type: 'dynamicGateStatus', state: 'uncertain', thresholdDbfs: -40 },
    { type: 'dynamicGateStatus', state: 'adjusted', thresholdDbfs: -80.01 },
    { type: 'dynamicGateStatus', state: 'adjusted', thresholdDbfs: -19.99 },
    { type: 'dynamicGateStatus', state: 'adjusted', thresholdDbfs: Number.NaN },
    { type: 'dynamicGateStatus', state: 'guess', thresholdDbfs: null },
    { type: 'dynamicGateStatus', state: 'learning', thresholdDbfs: null, extra: true },
  ])('ignores malformed Dynamic status: %o', async (status) => {
    const onGateStatus = vi.fn();
    const onWindow = vi.fn();
    await createMicProcessor(context, initial, onWindow, vi.fn(), onGateStatus);
    constructed[0].emit(status);

    expect(onGateStatus).not.toHaveBeenCalled();
    expect(onWindow).not.toHaveBeenCalled();
  });

  it('waits for the module before constructing or returning a node', async () => {
    const moduleLoad = deferred<void>();
    addModule.mockReturnValue(moduleLoad.promise);
    let settled = false;
    const pending = Promise.resolve().then(() =>
      createMicProcessor(context, initial, vi.fn(), vi.fn())
    );
    void pending.then(
      () => {
        settled = true;
      },
      () => {
        settled = true;
      }
    );

    await Promise.resolve();
    expect(addModule).toHaveBeenCalledWith('/assets/micProcessor.worklet.js');
    expect(constructed).toHaveLength(0);
    expect(settled).toBe(false);

    moduleLoad.resolve();
    await expect(pending).resolves.toMatchObject({ node: expect.any(Object) });
    expect(constructed).toHaveLength(1);
  });

  it('rejects module-load failure and calls the processor error callback', async () => {
    addModule.mockRejectedValue(new Error('module load failed'));
    const onProcessorError = vi.fn();

    await expect(createMicProcessor(context, initial, vi.fn(), onProcessorError)).rejects.toThrow(
      'module load failed'
    );
    expect(onProcessorError).toHaveBeenCalledOnce();
    expect(constructed).toHaveLength(0);
  });

  it('rejects invalid initial settings before loading the module', async () => {
    await expect(
      createMicProcessor(
        context,
        { protectAgcPeaks: true, gate: { kind: 'fixed', thresholdDbfs: NaN } },
        vi.fn(),
        vi.fn()
      )
    ).rejects.toThrow('Invalid microphone processor configuration');
    expect(addModule).not.toHaveBeenCalled();
  });

  it('rejects constructor failure and reports it through the error callback', async () => {
    constructorFailure = new Error('node construction failed');
    const onProcessorError = vi.fn();

    await expect(createMicProcessor(context, initial, vi.fn(), onProcessorError)).rejects.toThrow(
      'node construction failed'
    );
    expect(onProcessorError).toHaveBeenCalledOnce();
  });

  it('treats processorerror as terminal and ignores later reports', async () => {
    const onWindow = vi.fn();
    const onProcessorError = vi.fn();
    const { node, close } = await createMicProcessor(context, initial, onWindow, onProcessorError);
    const mockNode = constructed[0];

    mockNode.fail();
    mockNode.emit({ peak: 0.2, frames: 960, valid: true, overloaded: false });

    expect(onProcessorError).toHaveBeenCalledOnce();
    expect(onWindow).not.toHaveBeenCalled();
    expect(mockNode.disconnect).toHaveBeenCalled();
    close();
  });

  it('accepts the exact 20 ms silence report without requiring a finite dBFS value', async () => {
    const onWindow = vi.fn();
    await createMicProcessor(context, initial, onWindow, vi.fn());
    constructed[0].emit({ peak: 0, frames: 960, valid: true, overloaded: false });

    expect(onWindow).toHaveBeenCalledWith({ peak: 0, frames: 960, valid: true, overloaded: false });
  });

  it('accepts an above-full-scale manual-level report with overload evidence', async () => {
    const onWindow = vi.fn();
    await createMicProcessor(context, { ...initial, protectAgcPeaks: false }, onWindow, vi.fn());
    constructed[0].emit({ peak: 1.5, frames: 960, valid: true, overloaded: true });

    expect(onWindow).toHaveBeenCalledWith({
      peak: 1.5,
      frames: 960,
      valid: true,
      overloaded: true,
    });
  });

  it('validates report length against the context sample rate', async () => {
    context = { sampleRate: 44_100, audioWorklet: { addModule } } as unknown as AudioContext;
    const onWindow = vi.fn();
    await createMicProcessor(context, initial, onWindow, vi.fn());
    constructed[0].emit({ peak: 0, frames: 882, valid: true, overloaded: false });

    expect(onWindow).toHaveBeenCalledWith({ peak: 0, frames: 882, valid: true, overloaded: false });
  });

  it.each([
    { peak: -1, frames: 960, valid: true, overloaded: false },
    { peak: 0.2, frames: 959, valid: true, overloaded: false },
    { peak: 0.2, frames: 960, valid: 1, overloaded: false },
    { peak: 0.2, frames: 960, valid: true, overloaded: false, pcm: new Float32Array(1) },
  ])('ignores malformed reports: %o', async (report) => {
    const onWindow = vi.fn();
    await createMicProcessor(context, initial, onWindow, vi.fn());
    constructed[0].emit(report);

    expect(onWindow).not.toHaveBeenCalled();
  });

  it('ignores late reports after close and makes close idempotent', async () => {
    const onWindow = vi.fn();
    const { node, close } = await createMicProcessor(context, initial, onWindow, vi.fn());
    const mockNode = constructed[0];
    close();
    close();
    mockNode.emit({ peak: 0, frames: 960, valid: true, overloaded: false });

    expect(onWindow).not.toHaveBeenCalled();
    expect(mockNode.port.postMessage).toHaveBeenCalledTimes(1);
    expect(mockNode.port.close).toHaveBeenCalledTimes(1);
    expect(mockNode.disconnect).toHaveBeenCalledTimes(1);
    expect(node).toBe(mockNode);
  });

  it('sends validated runtime settings and rejects invalid gate values', async () => {
    const { setGate, setWindowReporting } = await createMicProcessor(
      context,
      initial,
      vi.fn(),
      vi.fn()
    );
    setGate({ kind: 'fixed', thresholdDbfs: -40 });
    setWindowReporting(true);
    expect(constructed[0].port.postMessage).toHaveBeenNthCalledWith(1, {
      type: 'setGate',
      gate: { kind: 'fixed', thresholdDbfs: -40 },
    });
    expect(constructed[0].port.postMessage).toHaveBeenNthCalledWith(2, {
      type: 'setWindowReporting',
      enabled: true,
    });
    expect(() => setGate({ kind: 'fixed', thresholdDbfs: Infinity })).toThrow();
  });
});
