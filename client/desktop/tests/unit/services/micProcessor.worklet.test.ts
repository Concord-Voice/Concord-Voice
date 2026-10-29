import { resetAllStores } from '../../helpers/store-helpers';
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

type TestProcessor = {
  process(inputs: Float32Array[][], outputs: Float32Array[][]): boolean;
  port: {
    onmessage: ((event: { data: unknown }) => void) | null;
    postMessage: ReturnType<typeof vi.fn>;
  };
};

type ProcessorOptions = {
  protectAgcPeaks: boolean;
  gate: { kind: 'off' } | { kind: 'fixed'; thresholdDbfs: number } | { kind: 'dynamic' };
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
  it('passes soft speech unchanged while Dynamic is learning', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    const softWord = Array(960).fill(0.002);
    expect(Math.max(...render(processor, [softWord])[0])).toBeCloseTo(0.002, 5);
  });

  it('learns a conservative threshold that closes on ambient and opens for speech', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    for (let window = 0; window < 120; window++) render(processor, [Array(960).fill(0.003)]);
    for (let window = 0; window < 120; window++) render(processor, [Array(960).fill(0.08)]);
    for (let window = 0; window < 65; window++) render(processor, [Array(960).fill(0.003)]);

    const statuses = processor.port.postMessage.mock.calls
      .map(([message]) => message)
      .filter((message) => message?.type === 'dynamicGateStatus');
    const adjusted = statuses.filter((message) => message.state === 'adjusted');
    expect(adjusted.length).toBeGreaterThan(0);
    expect(adjusted.every((message) => Number.isFinite(message.thresholdDbfs))).toBe(true);
    expect(adjusted.every((message) => message.thresholdDbfs <= -20)).toBe(true);
    expect(adjusted.every((message) => message.thresholdDbfs <= 20 * Math.log10(0.08) - 9)).toBe(
      true
    );

    let quietOutput: number[] = [];
    for (let window = 0; window < 12; window++) {
      quietOutput = render(processor, [Array(960).fill(0.003)])[0];
    }
    expect(Math.max(...quietOutput)).toBeLessThan(0.003 * 0.5);
    expect(Math.min(...render(processor, [Array(960).fill(0.08)])[0])).toBeCloseTo(0.08, 5);
  });

  it('keeps one isolated background utterance in permissive learning', () => {
    for (const [ambientWindows, elevatedWindows] of [
      [200, 40],
      [150, 50],
    ]) {
      const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
      for (let window = 0; window < ambientWindows; window++) {
        render(processor, [Array(960).fill(0.003)]);
      }
      for (let window = 0; window < elevatedWindows; window++) {
        render(processor, [Array(960).fill(0.08)]);
      }
      for (let window = 0; window < 100; window++) render(processor, [Array(960).fill(0.003)]);

      expect(processor.port.postMessage.mock.calls.map(([message]) => message)).not.toContainEqual(
        expect.objectContaining({ type: 'dynamicGateStatus', state: 'adjusted' })
      );
      expect(Math.max(...render(processor, [Array(960).fill(0.003)])[0])).toBeCloseTo(0.003, 5);
    }
  });

  it('learns from recurring short phrases below 20% of recent windows', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    for (let window = 0; window < 200; window++) render(processor, [Array(960).fill(0.003)]);
    for (let window = 0; window < 40; window++) render(processor, [Array(960).fill(0.08)]);
    for (let window = 0; window < 100; window++) render(processor, [Array(960).fill(0.003)]);
    for (let window = 0; window < 40; window++) render(processor, [Array(960).fill(0.08)]);
    for (let window = 0; window < 80; window++) render(processor, [Array(960).fill(0.003)]);

    expect(processor.port.postMessage.mock.calls.map(([message]) => message)).toContainEqual(
      expect.objectContaining({ type: 'dynamicGateStatus', state: 'adjusted' })
    );
    expect(Math.max(...render(processor, [Array(960).fill(0.08)])[0])).toBeCloseTo(0.08, 5);
  });

  // Regression for #3636: valid digital silence must not erase a completed short phrase.
  it.each([0.003, 0])('learns recurring phrases separated by a %s pause', (pausePeak) => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    const feed = (peak: number, windows: number) => {
      for (let window = 0; window < windows; window++) {
        render(processor, [Array(960).fill(peak)]);
      }
    };
    feed(0.003, 200);
    feed(0.08, 40);
    feed(pausePeak, 50);
    feed(0.003, 50);
    feed(0.08, 40);
    feed(0.003, 80);

    expect(
      processor.port.postMessage.mock.calls.map(([message]) => message),
      'Recurring separated phrases should establish confidence even when a pause is digital silence'
    ).toContainEqual(expect.objectContaining({ type: 'dynamicGateStatus', state: 'adjusted' }));
    expect(Math.max(...render(processor, [Array(960).fill(0.08)])[0])).toBeCloseTo(0.08, 5);
  });

  it.each([
    ['invalid samples', Number.NaN, 1],
    ['clipped samples', 1.1, 1],
    ['three seconds of silence', 0, 150],
  ] as const)('discards prior phrase evidence after %s', (_label, pausePeak, pauseWindows) => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    const feed = (peak: number, windows: number) => {
      for (let window = 0; window < windows; window++) {
        render(processor, [Array(960).fill(peak)]);
      }
    };
    feed(0.003, 200);
    feed(0.08, 40);
    feed(pausePeak, pauseWindows);
    feed(0.003, 50);
    feed(0.08, 40);
    feed(0.003, 80);

    expect(
      processor.port.postMessage.mock.calls.map(([message]) => message),
      'A single phrase after unusable or expired history must not establish suppression confidence'
    ).not.toContainEqual(expect.objectContaining({ type: 'dynamicGateStatus', state: 'adjusted' }));
    expect(Math.max(...render(processor, [Array(960).fill(0.002)])[0])).toBeCloseTo(0.002, 5);
  });

  it('does not mistake a brief elevated transient for a spoken phrase', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    for (let window = 0; window < 200; window++) render(processor, [Array(960).fill(0.003)]);
    for (let window = 0; window < 5; window++) render(processor, [Array(960).fill(0.08)]);
    for (let window = 0; window < 300; window++) render(processor, [Array(960).fill(0.003)]);

    expect(processor.port.postMessage.mock.calls.map(([message]) => message)).not.toContainEqual(
      expect.objectContaining({ type: 'dynamicGateStatus', state: 'adjusted' })
    );
  });

  it('learns an onset despite ordinary 2–3 dB level jitter', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    const block = (dbfs: number) => Array(960).fill(10 ** (dbfs / 20));
    for (let window = 0; window < 120; window++) {
      render(processor, [block(window % 2 === 0 ? -51 : -49)]);
    }
    for (let window = 0; window < 120; window++) {
      render(processor, [block(window % 2 === 0 ? -25 : -22)]);
    }
    for (let window = 0; window < 65; window++) {
      render(processor, [block(window % 2 === 0 ? -51 : -49)]);
    }

    expect(processor.port.postMessage.mock.calls.map(([message]) => message)).toContainEqual(
      expect.objectContaining({ type: 'dynamicGateStatus', state: 'adjusted' })
    );
  });

  it('retains a supported threshold long enough to reach the slow steady-state model cadence', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    const ambient = Array(960).fill(0.003);
    const voice = Array(960).fill(0.08);
    for (let window = 0; window < 120; window++) render(processor, [ambient]);
    for (let window = 0; window < 120; window++) render(processor, [voice]);
    for (let window = 0; window < 65; window++) render(processor, [ambient]);
    // Keep both recent level regimes without another long low-to-high onset.
    for (let window = 0; window < 1800; window++) {
      render(processor, [window % 2 === 0 ? ambient : voice]);
    }

    const statuses = processor.port.postMessage.mock.calls
      .map(([message]) => message)
      .filter((message) => message?.type === 'dynamicGateStatus');
    expect(statuses.at(-1)?.state).toBe('adjusted');
    expect(
      (processor as TestProcessor & { dynamicConfidentWindows: number }).dynamicConfidentWindows
    ).toBeGreaterThanOrEqual(1500);
    for (let window = 0; window < 10; window++) render(processor, [ambient]);
    const quietWord = render(processor, [Array(960).fill(0.002)])[0];
    expect(quietWord[0]).toBeGreaterThan(0.002 * 0.24);
  });

  // Regression for #3636: losing the lower regime also invalidates old separation evidence.
  it('releases a stale threshold within three seconds of sustained high-level input', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    const ambient = Array(960).fill(0.003);
    const voice = Array(960).fill(0.08);
    for (let window = 0; window < 120; window++) render(processor, [ambient]);
    for (let window = 0; window < 120; window++) render(processor, [voice]);
    for (let window = 0; window < 65; window++) render(processor, [ambient]);
    for (let window = 0; window < 1800; window++) {
      render(processor, [window % 2 === 0 ? ambient : voice]);
    }
    const latestStatus = () =>
      processor.port.postMessage.mock.calls
        .map(([message]) => message)
        .filter((message) => message?.type === 'dynamicGateStatus')
        .at(-1);
    // Positive control: the observation history really established a threshold.
    expect(latestStatus()?.state).toBe('adjusted');

    for (let window = 0; window < 150; window++) render(processor, [voice]);

    // Observe the whole 200 ms word: the old gate's short hold passes only its onset.
    for (let window = 0; window < 10; window++) {
      const softWord = render(processor, [Array(960).fill(0.002)])[0];
      expect(
        Math.min(...softWord),
        'A quiet word after three seconds without separation must pass unattenuated'
      ).toBeCloseTo(0.002, 5);
    }
    // Status is bounded to 1 Hz; audio release must not wait for its next report.
    for (let window = 0; window < 50; window++) render(processor, [voice]);
    expect(
      latestStatus()?.thresholdDbfs,
      'The next status report must reflect the expired separation evidence'
    ).toBeNull();
  });

  it.each([0.08, 0.003])(
    'expires learned regimes across %s input followed by digital silence',
    (precedingPeak) => {
      const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
      const feed = (peak: number, windows: number) => {
        for (let window = 0; window < windows; window++) {
          render(processor, [Array(960).fill(peak)]);
        }
      };
      feed(0.003, 120);
      feed(0.08, 120);
      feed(0.003, 65);
      const latestStatus = () =>
        processor.port.postMessage.mock.calls
          .map(([message]) => message)
          .filter((message) => message?.type === 'dynamicGateStatus')
          .at(-1);
      expect(latestStatus()?.state).toBe('adjusted');
      feed(precedingPeak, 100);
      feed(0, 100);
      for (let window = 0; window < 10; window++) {
        const word = render(processor, [Array(960).fill(0.002)])[0];
        expect(Math.min(...word)).toBeCloseTo(0.002, 5);
      }
      expect(latestStatus()?.thresholdDbfs).toBeNull();
    }
  );

  it('releases a stale threshold within three seconds of sustained low-level input', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    const ambient = Array(960).fill(0.003);
    const voice = Array(960).fill(0.08);
    for (let window = 0; window < 120; window++) render(processor, [ambient]);
    for (let window = 0; window < 120; window++) render(processor, [voice]);
    for (let window = 0; window < 65; window++) render(processor, [ambient]);
    for (let window = 0; window < 1800; window++) {
      render(processor, [window % 2 === 0 ? ambient : voice]);
    }

    let suppressed = false;
    let restoredAt: number | null = null;
    for (let window = 0; window < 200; window++) {
      const output = render(processor, [ambient])[0];
      if (Math.max(...output) < 0.003 * 0.5) suppressed = true;
      if (suppressed && Math.max(...output) >= 0.003 * 0.95) {
        restoredAt = window;
        break;
      }
    }
    expect(suppressed).toBe(true);
    expect(restoredAt).not.toBeNull();
    expect(restoredAt!).toBeLessThan(200);
  });

  it.each([
    ['silence only', () => Array(960).fill(0)],
    ['speech only', () => Array(960).fill(0.01)],
    ['overlapping ambient and voice', (index: number) => Array(960).fill(index % 2 ? 0.01 : 0.012)],
    ['background voice', (index: number) => Array(960).fill(index % 2 ? 0.01 : 0.02)],
  ])('keeps Dynamic permissive for %s', (_label, makeBlock) => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    const softSpeech = Array(960).fill(0.002);
    for (let index = 0; index < 240; index++) render(processor, [makeBlock(index)]);

    expect(Math.max(...render(processor, [softSpeech])[0])).toBeCloseTo(0.002, 5);
  });

  it('does not learn from a single sustained rise without a return to baseline', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    for (let window = 0; window < 120; window++) render(processor, [Array(960).fill(0.003)]);
    for (let window = 0; window < 120; window++) render(processor, [Array(960).fill(0.08)]);

    expect(processor.port.postMessage.mock.calls.map(([message]) => message)).not.toContainEqual(
      expect.objectContaining({ type: 'dynamicGateStatus', state: 'adjusted' })
    );
    expect(Math.max(...render(processor, [Array(960).fill(0.002)])[0])).toBeCloseTo(0.002, 5);
  });

  it('replaces a transient onset candidate when later speech returns to baseline', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    for (let window = 0; window < 120; window++) render(processor, [Array(960).fill(0.003)]);
    render(processor, [Array(960).fill(0.2)]);
    for (let window = 0; window < 10; window++) render(processor, [Array(960).fill(0.003)]);
    for (let window = 0; window < 120; window++) render(processor, [Array(960).fill(0.08)]);
    for (let window = 0; window < 65; window++) render(processor, [Array(960).fill(0.003)]);

    expect(processor.port.postMessage.mock.calls.map(([message]) => message)).toContainEqual(
      expect.objectContaining({ type: 'dynamicGateStatus', state: 'adjusted' })
    );
  });

  it('keeps one stable regime in Learning instead of claiming overlap', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    for (let window = 0; window < 200; window++) render(processor, [Array(960).fill(0.01)]);

    expect(processor.port.postMessage.mock.calls.map(([message]) => message)).toContainEqual({
      type: 'dynamicGateStatus',
      state: 'learning',
      thresholdDbfs: null,
    });
    expect(processor.port.postMessage.mock.calls.map(([message]) => message)).not.toContainEqual(
      expect.objectContaining({ type: 'dynamicGateStatus', state: 'uncertain' })
    );
  });

  it('reports Uncertain when distinct levels remain too close to separate safely', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    for (let window = 0; window < 200; window++) {
      render(processor, [Array(960).fill(window % 2 === 0 ? 0.01 : 0.02)]);
    }

    expect(processor.port.postMessage.mock.calls.map(([message]) => message)).toContainEqual({
      type: 'dynamicGateStatus',
      state: 'uncertain',
      thresholdDbfs: null,
    });
  });

  it('forgets a learned threshold after invalid input', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    for (let window = 0; window < 120; window++) render(processor, [Array(960).fill(0.003)]);
    for (let window = 0; window < 120; window++) render(processor, [Array(960).fill(0.08)]);
    for (let window = 0; window < 65; window++) render(processor, [Array(960).fill(0.003)]);
    expect(processor.port.postMessage.mock.calls.map(([message]) => message)).toContainEqual(
      expect.objectContaining({ type: 'dynamicGateStatus', state: 'adjusted' })
    );

    render(processor, [[Number.NaN, ...Array(959).fill(0.003)]]);
    expect(Math.max(...render(processor, [Array(960).fill(0.002)])[0])).toBeCloseTo(0.002, 5);
  });

  it('forgets a learned threshold when the live graph resets Dynamic', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    for (let window = 0; window < 120; window++) render(processor, [Array(960).fill(0.003)]);
    for (let window = 0; window < 120; window++) render(processor, [Array(960).fill(0.08)]);
    for (let window = 0; window < 65; window++) render(processor, [Array(960).fill(0.003)]);
    expect(processor.port.postMessage.mock.calls.map(([message]) => message)).toContainEqual(
      expect.objectContaining({ type: 'dynamicGateStatus', state: 'adjusted' })
    );

    processor.port.onmessage?.({ data: { type: 'setGate', gate: { kind: 'dynamic' } } });
    expect(Math.max(...render(processor, [Array(960).fill(0.002)])[0])).toBeCloseTo(0.002, 5);
  });

  it('keeps quiet speech onset and tail audible after a stronger syllable', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    for (let window = 0; window < 120; window++) render(processor, [Array(960).fill(0.003)]);
    for (let window = 0; window < 120; window++) render(processor, [Array(960).fill(0.08)]);
    for (let window = 0; window < 65; window++) render(processor, [Array(960).fill(0.003)]);
    for (let window = 0; window < 10; window++) render(processor, [Array(960).fill(0.003)]);

    const word = [...Array(160).fill(0.006), ...Array(640).fill(0.08), ...Array(160).fill(0.006)];
    const output = render(processor, [word])[0];
    expect(output[0]).toBeCloseTo(0.006, 5);
    expect(output[480]).toBeCloseTo(0.08, 5);
    expect(output[959]).toBeCloseTo(0.006, 5);
  });

  it('restarts Dynamic learning when the channel shape changes', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    for (let index = 0; index < 120; index++) render(processor, [Array(960).fill(0.003)]);
    for (let index = 0; index < 120; index++) render(processor, [Array(960).fill(0.08)]);
    for (let index = 0; index < 65; index++) render(processor, [Array(960).fill(0.003)]);
    render(processor, [Array(960).fill(0.003), Array(960).fill(0.003)]);

    expect(
      Math.max(...render(processor, [Array(960).fill(0.002), Array(960).fill(0.002)])[0])
    ).toBeCloseTo(0.002, 5);
  });

  it('abandons a Dynamic candidate after a fan-level shift and passes a knock onset', () => {
    const processor = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
    for (let index = 0; index < 120; index++) render(processor, [Array(960).fill(0.003)]);
    for (let index = 0; index < 120; index++) render(processor, [Array(960).fill(0.08)]);
    for (let index = 0; index < 65; index++) render(processor, [Array(960).fill(0.003)]);
    for (let index = 0; index < 240; index++) render(processor, [Array(960).fill(0.025)]);
    const knock = Array(960).fill(0);
    knock[0] = 0.7;

    expect(render(processor, [knock])[0][0]).toBeGreaterThan(0);
    expect(Math.max(...render(processor, [Array(960).fill(0.002)])[0])).toBeGreaterThanOrEqual(
      0.0005
    );
  });

  it('keeps Dynamic status bounded to one post per second and peak protection capped at -6 dBFS', () => {
    const processor = createProcessor({ protectAgcPeaks: true, gate: { kind: 'dynamic' } });
    const loud = Array(960).fill(0.9);
    for (let index = 0; index < 600; index++) render(processor, [loud]);

    const statuses = processor.port.postMessage.mock.calls
      .map(([message]) => message)
      .filter((message) => message?.type === 'dynamicGateStatus');
    expect(statuses.length).toBeLessThanOrEqual(13);
    expect(
      statuses.every((message) =>
        message.state === 'learning' || message.state === 'uncertain'
          ? message.thresholdDbfs === null
          : Number.isFinite(message.thresholdDbfs)
      )
    ).toBe(true);
    expect(Math.max(...render(processor, [loud])[0].map(Math.abs))).toBeLessThanOrEqual(
      10 ** (-6 / 20)
    );
  });

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

function calibrationMessages(processor: TestProcessor) {
  return processor.port.postMessage.mock.calls
    .map(([message]) => message)
    .filter((message) => message?.type?.startsWith('calibration'));
}
function phase(processor: TestProcessor, epoch: number, name: string) {
  expect(() =>
    processor.port.onmessage?.({ data: { type: 'setCalibrationPhase', epoch, phase: name } })
  ).not.toThrow();
}

describe('actual worklet calibration phases', () => {
  afterEach(() => vi.stubGlobal('sampleRate', 48_000));

  it('acknowledges before reports and excludes the old partial window', () => {
    const processor = createProcessor({ protectAgcPeaks: false });
    phase(processor, 1, 'quiet');
    render(processor, [Array(480).fill(0.3)]);
    phase(processor, 2, 'speech');
    render(processor, [Array(480).fill(0.01)]);
    expect(calibrationMessages(processor)).toEqual([
      { type: 'calibrationAck', epoch: 1, phase: 'quiet' },
      { type: 'calibrationAck', epoch: 2, phase: 'speech' },
    ]);
    render(processor, [Array(480).fill(0.01)]);
    expect(calibrationMessages(processor).at(-1)).toEqual({
      type: 'calibrationWindow',
      epoch: 2,
      phase: 'speech',
      peak: expect.closeTo(0.01, 6),
      frames: 960,
      valid: true,
      overloaded: false,
    });
  });

  it('observes input before fixed gate attenuation even without ordinary reporting', () => {
    const processor = createProcessor({
      protectAgcPeaks: false,
      gate: { kind: 'fixed', thresholdDbfs: -20 },
    });
    phase(processor, 1, 'quiet');
    expect(render(processor, [Array(960).fill(0.01)])[0].every((sample) => sample === 0)).toBe(
      true
    );
    expect(calibrationMessages(processor)).toContainEqual({
      type: 'calibrationWindow',
      epoch: 1,
      phase: 'quiet',
      peak: expect.closeTo(0.01, 6),
      frames: 960,
      valid: true,
      overloaded: false,
    });
  });

  it('reports post-protection peaks while preserving overload evidence', () => {
    const processor = createProcessor();
    phase(processor, 1, 'speech');
    render(processor, [Array(960).fill(2)]);
    const report = calibrationMessages(processor).find(
      (message) => message.type === 'calibrationWindow'
    );
    expect(report).toMatchObject({ valid: true, overloaded: true });
    expect(report.peak).toBeGreaterThan(0);
    expect(report.peak).toBeLessThanOrEqual(10 ** (-6 / 20));
  });

  it.each([44_100, 48_000, 96_000, 22_051])(
    'bounds complete quiet and speech windows at %i Hz',
    (rate) => {
      vi.stubGlobal('sampleRate', rate);
      const frames = Math.round(rate * 0.02);
      for (const [name, limit] of [
        ['quiet', Math.ceil((rate * 10) / frames)],
        ['speech', Math.floor((rate * 20) / frames)],
      ] as const) {
        const processor = createProcessor({ protectAgcPeaks: false });
        phase(processor, 1, name);
        for (let index = 0; index < limit + 3; index++)
          render(processor, [Array(frames).fill(0.01)]);
        const events = calibrationMessages(processor);
        expect(events.filter((event) => event.type === 'calibrationWindow')).toHaveLength(limit);
        expect(events.filter((event) => event.type === 'calibrationComplete')).toEqual([
          { type: 'calibrationComplete', epoch: 1, phase: name, frames: limit * frames },
        ]);
        phase(processor, 1, name);
        render(processor, [Array(frames).fill(0.01)]);
        expect(calibrationMessages(processor)).toEqual(events);
      }
    }
  );

  it('aggregates stereo peaks over actual unequal processing block sizes', () => {
    const processor = createProcessor({ protectAgcPeaks: false });
    phase(processor, 1, 'speech');
    for (const frames of [64, 128, 256, 512])
      render(processor, [Array(frames).fill(0.01), Array(frames).fill(-0.2)]);
    expect(calibrationMessages(processor).at(-1)).toMatchObject({
      type: 'calibrationWindow',
      peak: expect.closeTo(0.2, 6),
      frames: 960,
      valid: true,
    });
  });

  it.each(['missing', 'nonfinite'])('reports %s input as invalid evidence', (kind) => {
    const processor = createProcessor({ protectAgcPeaks: false });
    phase(processor, 1, 'quiet');
    if (kind === 'missing') processor.process([], [[new Float32Array(960)]]);
    else render(processor, [[NaN, ...Array(959).fill(0.01)]]);
    expect(calibrationMessages(processor)).toContainEqual(
      expect.objectContaining({
        type: 'calibrationWindow',
        epoch: 1,
        phase: 'quiet',
        valid: false,
      })
    );
  });

  it('does not let old or repeated epochs discard a current partial window', () => {
    const processor = createProcessor({ protectAgcPeaks: false });
    phase(processor, 2, 'speech');
    render(processor, [Array(480).fill(0.2)]);
    phase(processor, 1, 'quiet');
    phase(processor, 2, 'quiet');
    render(processor, [Array(480).fill(0.01)]);
    expect(calibrationMessages(processor)).toEqual([
      { type: 'calibrationAck', epoch: 2, phase: 'speech' },
      {
        type: 'calibrationWindow',
        epoch: 2,
        phase: 'speech',
        peak: expect.closeTo(0.2, 6),
        frames: 960,
        valid: true,
        overloaded: false,
      },
    ]);
  });

  it('idle discards partial evidence while ordinary windows continue unreset', () => {
    const processor = createProcessor({ protectAgcPeaks: false });
    processor.port.onmessage?.({ data: { type: 'setWindowReporting', enabled: true } });
    phase(processor, 1, 'quiet');
    render(processor, [Array(480).fill(0.2)]);
    phase(processor, 2, 'idle');
    render(processor, [Array(480).fill(0.01)]);
    expect(calibrationMessages(processor)).toEqual([
      { type: 'calibrationAck', epoch: 1, phase: 'quiet' },
      { type: 'calibrationAck', epoch: 2, phase: 'idle' },
    ]);
    expect(processor.port.postMessage).toHaveBeenCalledWith({
      peak: expect.closeTo(0.2, 6),
      frames: 960,
      valid: true,
      overloaded: false,
    });
  });

  it.each([
    { epoch: -1, phase: 'quiet' },
    { epoch: 1.5, phase: 'quiet' },
    { epoch: Number.MAX_SAFE_INTEGER + 1, phase: 'quiet' },
    { epoch: 1, phase: 'other' },
    { epoch: 1, phase: 'quiet', frames: 1 },
  ])('rejects malformed phase command %o without starting capture', (command) => {
    const processor = createProcessor({ protectAgcPeaks: false });
    try {
      processor.port.onmessage?.({ data: { type: 'setCalibrationPhase', ...command } });
    } catch {
      /* Rejection may be terminal in the real worklet. */
    }
    render(processor, [Array(960).fill(0.01)]);
    expect(calibrationMessages(processor)).toEqual([]);
  });
});

beforeEach(() => resetAllStores());

it('calibration boundaries preserve the actual Dynamic output and ordinary observation timeline', () => {
  const reference = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
  const observed = createProcessor({ protectAgcPeaks: false, gate: { kind: 'dynamic' } });
  for (const processor of [reference, observed]) {
    processor.port.onmessage?.({ data: { type: 'setWindowReporting', enabled: true } });
  }
  for (let index = 0; index < 340; index++) {
    if (index === 121) phase(observed, 1, 'quiet');
    if (index === 241) phase(observed, 2, 'speech');
    if (index === 300) phase(observed, 3, 'idle');
    const samples = Array(960).fill(index < 120 || index >= 240 ? 0.003 : 0.08);
    expect(render(observed, [samples])).toEqual(render(reference, [samples]));
  }
  const nonCalibration = (processor: TestProcessor) =>
    processor.port.postMessage.mock.calls
      .map(([event]) => event)
      .filter((event) => !event.type?.startsWith('calibration'));
  expect(nonCalibration(observed)).toEqual(nonCalibration(reference));
  expect(nonCalibration(reference)).toContainEqual(
    expect.objectContaining({ type: 'dynamicGateStatus', state: 'adjusted' })
  );
});
