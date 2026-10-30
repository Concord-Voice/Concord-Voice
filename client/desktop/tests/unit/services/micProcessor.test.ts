import { resetAllStores } from '../../helpers/store-helpers';
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

class MockNode extends EventTarget {
  port: MockPort = {
    onmessage: null,
    postMessage: vi.fn(),
    close: vi.fn(),
  };
  disconnect = vi.fn();

  emit(data: unknown) {
    this.port.onmessage?.({ data });
  }

  fail() {
    this.dispatchEvent(new Event('processorerror'));
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

  // Regression for #4147. Oracle: When an AudioWorkletNode emits an error event,
  // the microphone host invokes its existing error callback once and closes the
  // port and disconnects the node once, including when both event names occur.
  it.each([
    { order: 'error only', events: ['error'] },
    { order: 'processorerror only', events: ['processorerror'] },
    { order: 'error then processorerror', events: ['error', 'processorerror'] },
    { order: 'processorerror then error', events: ['processorerror', 'error'] },
  ])('terminates the microphone once for native event order $order', async ({ events }) => {
    const onWindow = vi.fn();
    const onProcessorError = vi.fn();
    const handle = await createMicProcessor(context, initial, onWindow, onProcessorError);
    expect(constructed).toHaveLength(1);
    const mockNode = constructed[0];
    expect(handle.node).toBe(mockNode);
    mockNode.emit({ peak: 0.2, frames: 960, valid: true, overloaded: false });
    expect(onWindow).toHaveBeenCalledOnce();
    onWindow.mockClear();
    expect(onProcessorError).not.toHaveBeenCalled();
    expect(mockNode.port.close).not.toHaveBeenCalled();
    expect(mockNode.disconnect).not.toHaveBeenCalled();

    for (const event of events) {
      mockNode.dispatchEvent(new Event(event));
      expect(
        {
          errorCallbackCalls: onProcessorError.mock.calls.length,
          portCloseCalls: mockNode.port.close.mock.calls.length,
          nodeDisconnectCalls: mockNode.disconnect.mock.calls.length,
        },
        `#4147: native ${event} must notify once, close the port once and disconnect once`
      ).toEqual({ errorCallbackCalls: 1, portCloseCalls: 1, nodeDisconnectCalls: 1 });
    }
    mockNode.emit({ peak: 0.2, frames: 960, valid: true, overloaded: false });
    expect(
      onWindow,
      '#4147: terminal worklet errors must fence later peak reports'
    ).not.toHaveBeenCalled();
    handle.close();
    expect(onProcessorError).toHaveBeenCalledOnce();
    expect(mockNode.port.close).toHaveBeenCalledOnce();
    expect(mockNode.disconnect).toHaveBeenCalledOnce();
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

describe('calibration host protocol', () => {
  const ack = (epoch = 1, phase = 'quiet') => ({ type: 'calibrationAck', epoch, phase });
  const window = (epoch = 1, phase = 'quiet') => ({
    type: 'calibrationWindow',
    epoch,
    phase,
    peak: 0.01,
    frames: 960,
    valid: true,
    overloaded: false,
  });

  it('delivers only acknowledged current-epoch observations separately from ordinary windows', async () => {
    const onEvent = vi.fn();
    const onWindow = vi.fn();
    const handle = await createMicProcessor(
      context,
      initial,
      onWindow,
      vi.fn(),
      undefined,
      onEvent
    );
    handle.setCalibrationPhase(1, 'quiet');
    expect(constructed[0].port.postMessage).toHaveBeenCalledWith({
      type: 'setCalibrationPhase',
      epoch: 1,
      phase: 'quiet',
    });
    constructed[0].emit(window());
    expect(onEvent).not.toHaveBeenCalled();
    constructed[0].emit(ack());
    constructed[0].emit(window());
    handle.setCalibrationPhase(2, 'speech');
    constructed[0].emit(window());
    constructed[0].emit(ack(2, 'speech'));
    constructed[0].emit(window());
    constructed[0].emit(window(2, 'speech'));
    expect(onEvent.mock.calls.map(([event]) => event)).toEqual([
      ack(),
      window(),
      ack(2, 'speech'),
      window(2, 'speech'),
    ]);
    expect(onWindow).not.toHaveBeenCalled();
  });

  it('delivers invalid and overload evidence instead of silently dropping it', async () => {
    const onEvent = vi.fn();
    const handle = await createMicProcessor(context, initial, vi.fn(), vi.fn(), undefined, onEvent);
    handle.setCalibrationPhase(1, 'quiet');
    constructed[0].emit(ack());
    const invalid = { ...window(), valid: false };
    const overload = { ...window(), peak: 1.5, overloaded: true };
    constructed[0].emit(invalid);
    constructed[0].emit(overload);
    expect(onEvent.mock.calls.map(([event]) => event)).toEqual([ack(), invalid, overload]);
  });

  it.each([
    { peak: NaN },
    { peak: -1 },
    { frames: 959 },
    { valid: 1 },
    { overloaded: null },
    { pcm: [0.1] },
    { epoch: 1.5 },
    { phase: 'idle' },
  ])('rejects malformed event %o', async (bad) => {
    const onEvent = vi.fn();
    const handle = await createMicProcessor(context, initial, vi.fn(), vi.fn(), undefined, onEvent);
    handle.setCalibrationPhase(1, 'quiet');
    constructed[0].emit(ack());
    onEvent.mockClear();
    constructed[0].emit({ ...window(), ...bad });
    expect(onEvent).not.toHaveBeenCalled();
  });

  it('rejects malformed acknowledgements and premature completion', async () => {
    const onEvent = vi.fn();
    const handle = await createMicProcessor(context, initial, vi.fn(), vi.fn(), undefined, onEvent);
    handle.setCalibrationPhase(1, 'quiet');
    constructed[0].emit({ ...ack(), extra: true });
    constructed[0].emit(ack(1, 'speech'));
    constructed[0].emit({ type: 'calibrationComplete', epoch: 1, phase: 'quiet', frames: 480_000 });
    expect(onEvent).not.toHaveBeenCalled();
    constructed[0].emit(ack());
    constructed[0].emit({ type: 'calibrationComplete', epoch: 1, phase: 'quiet', frames: 960 });
    expect(onEvent.mock.calls.map(([event]) => event)).toEqual([ack()]);
  });

  it.each([
    ['quiet', 500],
    ['speech', 1000],
  ] as const)('bounds %s callbacks even when the worklet over-reports', async (phase, limit) => {
    const onEvent = vi.fn();
    const handle = await createMicProcessor(context, initial, vi.fn(), vi.fn(), undefined, onEvent);
    handle.setCalibrationPhase(1, phase);
    constructed[0].emit(ack(1, phase));
    for (let index = 0; index < limit + 2; index++) constructed[0].emit(window(1, phase));
    const complete = { type: 'calibrationComplete', epoch: 1, phase, frames: limit * 960 };
    constructed[0].emit(complete);
    constructed[0].emit(complete);
    expect(onEvent.mock.calls.filter(([event]) => event.type === 'calibrationWindow')).toHaveLength(
      limit
    );
    expect(
      onEvent.mock.calls.filter(([event]) => event.type === 'calibrationComplete')
    ).toHaveLength(1);
  });

  it.each([44_100, 96_000])('validates calibration frames against %i Hz', async (rate) => {
    context = { sampleRate: rate, audioWorklet: { addModule } } as unknown as AudioContext;
    const onEvent = vi.fn();
    const handle = await createMicProcessor(context, initial, vi.fn(), vi.fn(), undefined, onEvent);
    handle.setCalibrationPhase(1, 'quiet');
    constructed[0].emit(ack());
    constructed[0].emit(window());
    const correct = { ...window(), frames: rate * 0.02 };
    constructed[0].emit(correct);
    expect(onEvent.mock.calls.map(([event]) => event)).toEqual([ack(), correct]);
  });

  it('rejects invalid commands without sending them and ignores stale epochs', async () => {
    const handle = await createMicProcessor(context, initial, vi.fn(), vi.fn(), undefined, vi.fn());
    for (const epoch of [-1, 1.5, NaN, Number.MAX_SAFE_INTEGER + 1]) {
      expect(() => handle.setCalibrationPhase(epoch, 'quiet')).toThrow();
    }
    expect(() => handle.setCalibrationPhase(1, 'other' as 'quiet')).toThrow();
    expect(constructed[0].port.postMessage).not.toHaveBeenCalled();
    handle.setCalibrationPhase(2, 'speech');
    handle.setCalibrationPhase(1, 'quiet');
    handle.setCalibrationPhase(2, 'quiet');
    expect(constructed[0].port.postMessage).toHaveBeenCalledTimes(1);
  });

  it.each(['close', 'error'])('fences queued callbacks and commands after %s', async (ending) => {
    const onEvent = vi.fn();
    const handle = await createMicProcessor(context, initial, vi.fn(), vi.fn(), undefined, onEvent);
    handle.setCalibrationPhase(1, 'quiet');
    constructed[0].emit(ack());
    onEvent.mockClear();
    if (ending === 'close') handle.close();
    else constructed[0].fail();
    const sends = constructed[0].port.postMessage.mock.calls.length;
    handle.setCalibrationPhase(2, 'speech');
    constructed[0].emit(window());
    constructed[0].emit(ack(2, 'speech'));
    expect(onEvent).not.toHaveBeenCalled();
    expect(constructed[0].port.postMessage).toHaveBeenCalledTimes(sends);
  });
});

beforeEach(() => resetAllStores());

describe('calibrated Auto runtime policy', () => {
  it.each([
    [false, false, -47, { kind: 'fixed', thresholdDbfs: -47 }],
    [true, true, -53, { kind: 'fixed', thresholdDbfs: -53 }],
    [false, true, -47, { kind: 'dynamic' }],
  ] as const)(
    'resolves Auto with Music Mode %s and saved AGC %s at threshold %s',
    (musicMode, autoGainControl, noiseGateLevel, expected) => {
      expect(
        gateForAudioSettings({
          musicMode,
          autoGainControl,
          noiseGateMode: 'autoCalibrate',
          noiseGateLevel,
        })
      ).toEqual(expected);
    }
  );

  it.each([
    [false, false],
    [false, true],
    [true, true],
  ])(
    'keeps explicit Off open with Music Mode %s and saved AGC %s',
    (musicMode, autoGainControl) => {
      expect(
        gateForAudioSettings({
          musicMode,
          autoGainControl,
          noiseGateMode: 'off',
          noiseGateLevel: -47,
        })
      ).toEqual({ kind: 'off' });
    }
  );
});
