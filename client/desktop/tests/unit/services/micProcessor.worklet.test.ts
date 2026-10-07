import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

type TestProcessor = {
  process(inputs: Float32Array[][], outputs: Float32Array[][]): boolean;
  port: {
    onmessage: ((event: { data: unknown }) => void) | null;
    postMessage: ReturnType<typeof vi.fn>;
  };
};

type ProcessorOptions = {
  protectAgcPeaks: boolean;
  gate: { kind: 'off' } | { kind: 'fixed'; thresholdDbfs: number };
};

let Registered: new (options: { processorOptions: unknown }) => TestProcessor;

beforeAll(async () => {
  vi.stubGlobal('sampleRate', 48_000);
  vi.stubGlobal(
    'AudioWorkletProcessor',
    class {
      port = { onmessage: null, postMessage: vi.fn() };
    }
  );
  vi.stubGlobal('registerProcessor', (_name: string, ctor: typeof Registered) => {
    Registered = ctor;
  });
  await import('@/renderer/services/voice/micProcessor.worklet.js');
});

afterEach(() => vi.clearAllMocks());

function createProcessor(overrides: Partial<ProcessorOptions> = {}): TestProcessor {
  return new Registered({
    processorOptions: {
      protectAgcPeaks: true,
      gate: { kind: 'off' },
      ...overrides,
    },
  });
}

function render(processor: TestProcessor, channels: number[][]): number[][] {
  const inputs = channels.map((channel) => Float32Array.from(channel));
  const outputs = channels.map((channel) => new Float32Array(channel.length));
  processor.process([inputs], [outputs]);
  return outputs.map((channel) => [...channel]);
}

describe('microphone peak worklet', () => {
  it('emits an above-full-scale impulse while keeping every Float32 output sample below -6 dBFS', () => {
    const processor = createProcessor();
    const impulse = Array(256).fill(0);
    impulse[0] = 4;

    const output = [
      ...render(processor, [impulse])[0],
      ...render(processor, [Array(256).fill(0)])[0],
    ];

    expect(output.some((sample) => sample !== 0)).toBe(true);
    expect(Math.max(...output.map(Math.abs))).toBeLessThanOrEqual(10 ** (-6 / 20));
  });

  it('limits a negative above-full-scale impulse and emits it with negative polarity', () => {
    const processor = createProcessor();
    const impulse = Array(256).fill(0);
    impulse[0] = -4;
    const output = [
      ...render(processor, [impulse])[0],
      ...render(processor, [Array(256).fill(0)])[0],
    ];
    const emitted = output.find((sample) => sample !== 0);

    expect(emitted).toBeLessThan(0);
    expect(Math.max(...output.map(Math.abs))).toBeLessThanOrEqual(10 ** (-6 / 20));
  });

  it('keeps stereo channels linked under peak protection', () => {
    const processor = createProcessor();
    const left = Array(256).fill(0);
    const right = Array(256).fill(0);
    left[0] = 4;
    right[0] = 2;

    const first = render(processor, [left, right]);
    const drained = render(processor, [Array(256).fill(0), Array(256).fill(0)]);
    const leftSamples = [...first[0], ...drained[0]];
    const rightSamples = [...first[1], ...drained[1]];
    const peakIndex = leftSamples.findIndex((sample) => sample !== 0);

    expect(peakIndex).toBeGreaterThanOrEqual(0);
    expect(rightSamples[peakIndex] / leftSamples[peakIndex]).toBeCloseTo(0.5, 5);
  });

  it('passes nonzero manual input unchanged when protection is off', () => {
    const processor = createProcessor({ protectAgcPeaks: false });
    const input = Array(128).fill(0.25);
    expect(render(processor, [input])[0]).toEqual(input);
  });

  it('opens the fixed gate for samples above its threshold', () => {
    const processor = createProcessor({
      protectAgcPeaks: false,
      gate: { kind: 'fixed', thresholdDbfs: -40 },
    });
    const input = Array(128).fill(0.1);
    const output = render(processor, [input])[0];
    expect(Math.max(...output)).toBeGreaterThan(0);
  });

  it('keeps a fixed gate closed below its threshold', () => {
    const processor = createProcessor({
      protectAgcPeaks: false,
      gate: { kind: 'fixed', thresholdDbfs: -40 },
    });
    expect(render(processor, [Array(128).fill(0.001)])[0].every((sample) => sample === 0)).toBe(
      true
    );
  });

  it('releases an open fixed gate smoothly after its threshold changes', () => {
    const processor = createProcessor({
      protectAgcPeaks: false,
      gate: { kind: 'fixed', thresholdDbfs: -40 },
    });
    render(processor, [Array(128).fill(0.1)]);
    processor.port.onmessage?.({
      data: { type: 'setGate', gate: { kind: 'fixed', thresholdDbfs: -20 } },
    });

    const output = render(processor, [Array(128).fill(0.05)])[0];
    expect(output[0]).toBeGreaterThan(0);
    expect(output.at(-1)).toBeLessThan(output[0]);
  });

  it.each([64, 128, 256, 960])(
    'handles a %i-frame block and sustained loud input within the cap',
    (frames) => {
      const processor = createProcessor();
      const input = Array(frames).fill(0.9);
      const output = [
        ...render(processor, [input])[0],
        ...render(processor, [input])[0],
        ...render(processor, [input])[0],
        ...render(processor, [input])[0],
      ];
      expect(output.some((sample) => sample !== 0)).toBe(true);
      expect(Math.max(...output.map(Math.abs))).toBeLessThanOrEqual(10 ** (-6 / 20));
    }
  );

  it('emits silence for absent input', () => {
    const processor = createProcessor();
    const output = new Float32Array(128);
    processor.process([], [[output]]);
    expect([...output].every((sample) => sample === 0)).toBe(true);
  });

  it('emits silence for unequal channel lengths', () => {
    const processor = createProcessor();
    const left = new Float32Array(128).fill(0.5);
    const right = new Float32Array(64).fill(0.5);
    const outLeft = new Float32Array(128);
    const outRight = new Float32Array(64);
    processor.process([[left, right]], [[outLeft, outRight]]);
    expect([...outLeft, ...outRight].every((sample) => sample === 0)).toBe(true);
  });

  it('rejects invalid initial processor options', () => {
    expect(() => createProcessor({ gate: { kind: 'fixed', thresholdDbfs: Number.NaN } })).toThrow();
  });

  it('accepts runtime gate updates and terminates after close', () => {
    const processor = createProcessor({ protectAgcPeaks: false });
    processor.port.onmessage?.({
      data: { type: 'setGate', gate: { kind: 'fixed', thresholdDbfs: -40 } },
    });
    const quiet = render(processor, [Array(128).fill(0.001)])[0];
    expect(quiet.every((sample) => sample === 0)).toBe(true);

    processor.port.onmessage?.({ data: { type: 'close' } });
    expect(processor.process([], [[new Float32Array(128)]])).toBe(false);
  });

  it('rejects an invalid runtime gate update without disabling peak protection', () => {
    const processor = createProcessor();
    expect(() =>
      processor.port.onmessage?.({
        data: { type: 'setGate', gate: { kind: 'fixed', thresholdDbfs: Number.POSITIVE_INFINITY } },
      })
    ).toThrow();

    const impulse = Array(256).fill(0);
    impulse[0] = 4;
    const output = [
      ...render(processor, [impulse])[0],
      ...render(processor, [Array(256).fill(0)])[0],
    ];
    expect(Math.max(...output.map(Math.abs))).toBeLessThanOrEqual(10 ** (-6 / 20));
  });

  it('reports complete 20 ms windows across partial blocks only when enabled', () => {
    const processor = createProcessor();
    processor.port.onmessage?.({ data: { type: 'setWindowReporting', enabled: true } });
    render(processor, [Array(480).fill(0.1)]);
    expect(processor.port.postMessage).not.toHaveBeenCalled();
    render(processor, [Array(480).fill(0.1)]);
    const reports = processor.port.postMessage.mock.calls.map(([message]) => message);
    expect(reports).toContainEqual({
      peak: expect.any(Number),
      frames: 960,
      valid: true,
      overloaded: false,
    });
  });

  it('reports overload evidence for a peak at or above full scale', () => {
    const processor = createProcessor();
    processor.port.onmessage?.({ data: { type: 'setWindowReporting', enabled: true } });
    const input = Array(960).fill(0);
    input[0] = 2;
    const output = render(processor, [input])[0];
    const reports = processor.port.postMessage.mock.calls.map(([message]) => message);

    expect(Math.max(...output.map(Math.abs))).toBeLessThanOrEqual(10 ** (-6 / 20));
    expect(reports).toContainEqual({
      peak: expect.any(Number),
      frames: 960,
      valid: true,
      overloaded: true,
    });
  });

  it('reports the nonzero pre-gate peak while a fixed gate remains closed', () => {
    const processor = createProcessor({
      protectAgcPeaks: false,
      gate: { kind: 'fixed', thresholdDbfs: -20 },
    });
    processor.port.onmessage?.({ data: { type: 'setWindowReporting', enabled: true } });
    const output = render(processor, [Array(960).fill(0.01)])[0];
    const reports = processor.port.postMessage.mock.calls.map(([message]) => message);

    expect(output.every((sample) => sample === 0)).toBe(true);
    expect(reports).toContainEqual({
      peak: expect.closeTo(0.01, 5),
      frames: 960,
      valid: true,
      overloaded: false,
    });
  });

  it.each([Number.NaN, Number.POSITIVE_INFINITY])(
    'marks non-finite input %s invalid and emits no non-finite output samples',
    (badSample) => {
      const processor = createProcessor();
      processor.port.onmessage?.({ data: { type: 'setWindowReporting', enabled: true } });
      const input = Array(960).fill(0.1);
      input[0] = badSample;
      const output = render(processor, [input])[0];
      expect(output.every(Number.isFinite)).toBe(true);
      expect(processor.port.postMessage.mock.calls.map(([message]) => message)).toContainEqual(
        expect.objectContaining({ valid: false })
      );
    }
  );

  it('resets channel-shaped state when input changes from mono to stereo', () => {
    const processor = createProcessor();
    render(processor, [Array(256).fill(0.4)]);
    const first = render(processor, [Array(256).fill(0.4), Array(256).fill(0.2)]);
    const second = render(processor, [Array(256).fill(0.4), Array(256).fill(0.2)]);
    const output = [...first[0], ...first[1], ...second[0], ...second[1]];
    expect(output.every(Number.isFinite)).toBe(true);
    expect(Math.max(...output.map(Math.abs))).toBeLessThanOrEqual(10 ** (-6 / 20));
  });
});
