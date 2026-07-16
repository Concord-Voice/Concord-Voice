// @vitest-environment node
import { describe, expect, it } from 'vitest';
import {
  DecoderBudgetSampler,
  selectInboundVideoDecoderReport,
  type DecoderBudgetResult,
} from '@/renderer/services/decoderBudgetSampler';

interface Cursor {
  totalDecodeTime: number;
  framesDecoded: number;
  timestamp: number;
}

function observe(
  sampler: DecoderBudgetSampler,
  consumerId: string,
  cursor: Cursor | null,
  options: { paused?: boolean; observedAtMs?: number; reportId?: string } = {}
): DecoderBudgetResult {
  return sampler.observe({
    consumerId,
    reportId: options.reportId ?? 'inbound-1',
    paused: options.paused ?? false,
    observedAtMs: options.observedAtMs ?? cursor?.timestamp ?? 0,
    report: cursor,
  });
}

function advance(
  sampler: DecoderBudgetSampler,
  consumerId: string,
  cursor: Cursor,
  decodeMsPerFrame: number,
  frames = 1,
  elapsedMs = 1_000
): DecoderBudgetResult {
  cursor.totalDecodeTime += (decodeMsPerFrame * frames) / 1_000;
  cursor.framesDecoded += frames;
  cursor.timestamp += elapsedMs;
  return observe(sampler, consumerId, cursor);
}

function expectUsable(result: DecoderBudgetResult) {
  expect(result.usable).toBe(true);
  if (!result.usable) throw new Error(`expected usable decoder sample, got ${result.reason}`);
  return result;
}

describe('DecoderBudgetSampler', () => {
  it('returns an explicit unknown result for the first sample while establishing a baseline', () => {
    const sampler = new DecoderBudgetSampler();

    expect(
      observe(sampler, 'camera-1', { totalDecodeTime: 4, framesDecoded: 200, timestamp: 1_000 })
    ).toEqual({ usable: false, reason: 'first-sample' });
  });

  it('derives decode time and FPS from interval deltas instead of lifetime averages', () => {
    const sampler = new DecoderBudgetSampler();
    const cursor = { totalDecodeTime: 100, framesDecoded: 10_000, timestamp: 1_000 };
    observe(sampler, 'camera-1', cursor);

    const result = expectUsable(advance(sampler, 'camera-1', cursor, 4, 10, 1_000));

    expect(result.p95DecodeMs).toBeCloseTo(4);
    expect(result.currentFps).toBeCloseTo(10);
    expect(result.rho).toBeCloseTo(0.04);
  });

  it('keeps decoder histories isolated by consumer', () => {
    const sampler = new DecoderBudgetSampler();
    const camera = { totalDecodeTime: 0, framesDecoded: 0, timestamp: 0 };
    const screen = { totalDecodeTime: 0, framesDecoded: 0, timestamp: 0 };
    observe(sampler, 'camera', camera);
    observe(sampler, 'screen', screen);

    const cameraResult = expectUsable(advance(sampler, 'camera', camera, 50, 10));
    const screenResult = expectUsable(advance(sampler, 'screen', screen, 2, 10));

    expect(cameraResult.p95DecodeMs).toBeCloseTo(50);
    expect(screenResult.p95DecodeMs).toBeCloseTo(2);
  });

  it('uses the nearest-rank p95 over usable interval decode averages', () => {
    const sampler = new DecoderBudgetSampler();
    const cursor = { totalDecodeTime: 10, framesDecoded: 1_000, timestamp: 0 };
    observe(sampler, 'camera-1', cursor);

    let result: DecoderBudgetResult = { usable: false, reason: 'first-sample' };
    for (let decodeMs = 1; decodeMs <= 20; decodeMs++) {
      result = advance(sampler, 'camera-1', cursor, decodeMs);
    }

    expect(expectUsable(result).p95DecodeMs).toBeCloseTo(19);
  });

  it('bounds history to the newest twenty usable intervals', () => {
    const sampler = new DecoderBudgetSampler();
    const cursor = { totalDecodeTime: 0, framesDecoded: 0, timestamp: 0 };
    observe(sampler, 'camera-1', cursor);

    advance(sampler, 'camera-1', cursor, 100);
    advance(sampler, 'camera-1', cursor, 100);
    let result: DecoderBudgetResult = { usable: false, reason: 'first-sample' };
    for (let index = 0; index < 18; index++) {
      result = advance(sampler, 'camera-1', cursor, 1);
    }
    expect(expectUsable(result).p95DecodeMs).toBeCloseTo(100);

    result = advance(sampler, 'camera-1', cursor, 1);
    expect(expectUsable(result).p95DecodeMs).toBeCloseTo(1);
  });

  it.each([
    {
      name: 'framesDecoded',
      reset: { totalDecodeTime: 0.11, framesDecoded: 0, timestamp: 2_000 },
    },
    {
      name: 'totalDecodeTime',
      reset: { totalDecodeTime: 0.05, framesDecoded: 2, timestamp: 2_000 },
    },
  ])('starts a fresh history segment when $name moves backwards', ({ reset }) => {
    const sampler = new DecoderBudgetSampler();
    observe(sampler, 'camera-1', { totalDecodeTime: 0, framesDecoded: 0, timestamp: 0 });
    expectUsable(
      observe(sampler, 'camera-1', {
        totalDecodeTime: 0.1,
        framesDecoded: 1,
        timestamp: 1_000,
      })
    );

    expect(observe(sampler, 'camera-1', reset)).toEqual({
      usable: false,
      reason: 'counter-reset',
    });

    const next = {
      totalDecodeTime: reset.totalDecodeTime + 0.01,
      framesDecoded: reset.framesDecoded + 5,
      timestamp: 3_000,
    };
    const result = expectUsable(observe(sampler, 'camera-1', next));
    expect(result.p95DecodeMs).toBeCloseTo(2);
    expect(result.currentFps).toBeCloseTo(5);
  });

  it('returns out-of-order and rebaselines when the stats timestamp does not advance', () => {
    const sampler = new DecoderBudgetSampler();
    observe(sampler, 'camera-1', { totalDecodeTime: 1, framesDecoded: 10, timestamp: 2_000 });

    expect(
      observe(sampler, 'camera-1', {
        totalDecodeTime: 1.1,
        framesDecoded: 20,
        timestamp: 1_000,
      })
    ).toEqual({ usable: false, reason: 'out-of-order' });

    const result = expectUsable(
      observe(sampler, 'camera-1', {
        totalDecodeTime: 1.11,
        framesDecoded: 25,
        timestamp: 2_000,
      })
    );
    expect(result.p95DecodeMs).toBeCloseTo(2);
    expect(result.currentFps).toBeCloseTo(5);
  });

  it('starts a fresh history segment when the selected inbound stream identity changes', () => {
    const sampler = new DecoderBudgetSampler();
    observe(sampler, 'camera-1', { totalDecodeTime: 0, framesDecoded: 0, timestamp: 0 });
    expectUsable(
      observe(sampler, 'camera-1', {
        totalDecodeTime: 0.1,
        framesDecoded: 1,
        timestamp: 1_000,
      })
    );

    expect(
      observe(
        sampler,
        'camera-1',
        { totalDecodeTime: 8, framesDecoded: 400, timestamp: 2_000 },
        { reportId: 'inbound-2' }
      )
    ).toEqual({ usable: false, reason: 'stream-changed' });

    const next = expectUsable(
      observe(
        sampler,
        'camera-1',
        { totalDecodeTime: 8.01, framesDecoded: 405, timestamp: 3_000 },
        { reportId: 'inbound-2' }
      )
    );
    expect(next.p95DecodeMs).toBeCloseTo(2);
    expect(next.currentFps).toBeCloseTo(5);
  });

  it('returns paused and requires a fresh baseline after decoding resumes', () => {
    const sampler = new DecoderBudgetSampler();
    observe(sampler, 'camera-1', { totalDecodeTime: 0, framesDecoded: 0, timestamp: 0 });
    expectUsable(
      observe(sampler, 'camera-1', {
        totalDecodeTime: 0.1,
        framesDecoded: 1,
        timestamp: 1_000,
      })
    );

    expect(
      observe(
        sampler,
        'camera-1',
        { totalDecodeTime: 0.11, framesDecoded: 6, timestamp: 2_000 },
        { paused: true }
      )
    ).toEqual({ usable: false, reason: 'paused' });
    expect(
      observe(sampler, 'camera-1', {
        totalDecodeTime: 0.12,
        framesDecoded: 11,
        timestamp: 3_000,
      })
    ).toEqual({ usable: false, reason: 'first-sample' });
    const resumed = expectUsable(
      observe(sampler, 'camera-1', {
        totalDecodeTime: 0.13,
        framesDecoded: 16,
        timestamp: 4_000,
      })
    );
    expect(resumed.p95DecodeMs).toBeCloseTo(2);
  });

  it.each([
    { name: 'absent report', report: null },
    {
      name: 'missing counter',
      report: { totalDecodeTime: 1, framesDecoded: undefined, timestamp: 1_000 },
    },
    {
      name: 'non-finite value',
      report: { totalDecodeTime: Number.NaN, framesDecoded: 1, timestamp: 1_000 },
    },
  ])('returns missing-stats for an $name and discards the old baseline', ({ report }) => {
    const sampler = new DecoderBudgetSampler();
    observe(sampler, 'camera-1', { totalDecodeTime: 0, framesDecoded: 0, timestamp: 0 });

    expect(observe(sampler, 'camera-1', report)).toEqual({
      usable: false,
      reason: 'missing-stats',
    });
    expect(
      observe(sampler, 'camera-1', {
        totalDecodeTime: 0.02,
        framesDecoded: 10,
        timestamp: 2_000,
      })
    ).toEqual({ usable: false, reason: 'first-sample' });
  });

  it('returns stale-stats and discards the old baseline when the report is over ten seconds old', () => {
    const sampler = new DecoderBudgetSampler();
    observe(sampler, 'camera-1', { totalDecodeTime: 0, framesDecoded: 0, timestamp: 0 });

    expect(
      observe(
        sampler,
        'camera-1',
        { totalDecodeTime: 0.01, framesDecoded: 5, timestamp: 1_000 },
        { observedAtMs: 11_001 }
      )
    ).toEqual({ usable: false, reason: 'stale-stats' });
    expect(
      observe(sampler, 'camera-1', {
        totalDecodeTime: 0.02,
        framesDecoded: 10,
        timestamp: 12_000,
      })
    ).toEqual({ usable: false, reason: 'first-sample' });
  });

  it('accepts a report exactly ten seconds old', () => {
    const sampler = new DecoderBudgetSampler();

    expect(
      observe(
        sampler,
        'camera-1',
        { totalDecodeTime: 0, framesDecoded: 0, timestamp: 1_000 },
        { observedAtMs: 11_000 }
      )
    ).toEqual({ usable: false, reason: 'first-sample' });
    expectUsable(
      observe(
        sampler,
        'camera-1',
        { totalDecodeTime: 0.01, framesDecoded: 5, timestamp: 2_000 },
        { observedAtMs: 12_000 }
      )
    );
  });

  it('returns future-stats separately from an out-of-order report', () => {
    const sampler = new DecoderBudgetSampler();
    observe(sampler, 'camera-1', { totalDecodeTime: 0, framesDecoded: 0, timestamp: 1_000 });

    expect(
      observe(
        sampler,
        'camera-1',
        { totalDecodeTime: 0.01, framesDecoded: 5, timestamp: 3_000 },
        { observedAtMs: 2_000 }
      )
    ).toEqual({ usable: false, reason: 'future-stats' });
    expect(
      observe(sampler, 'camera-1', {
        totalDecodeTime: 0.01,
        framesDecoded: 5,
        timestamp: 3_000,
      })
    ).toEqual({ usable: false, reason: 'first-sample' });
  });

  it('returns no-progress without adding history and rebaselines the next interval', () => {
    const sampler = new DecoderBudgetSampler();
    observe(sampler, 'camera-1', { totalDecodeTime: 0, framesDecoded: 0, timestamp: 0 });

    expect(
      observe(sampler, 'camera-1', {
        totalDecodeTime: 0,
        framesDecoded: 0,
        timestamp: 1_000,
      })
    ).toEqual({ usable: false, reason: 'no-progress' });

    const result = expectUsable(
      observe(sampler, 'camera-1', {
        totalDecodeTime: 0.02,
        framesDecoded: 10,
        timestamp: 2_000,
      })
    );
    expect(result.p95DecodeMs).toBeCloseTo(2);
    expect(result.currentFps).toBeCloseTo(10);
  });

  it.each([
    {
      name: 'FPS overflow',
      next: { totalDecodeTime: 0.001, framesDecoded: 1, timestamp: Number.MIN_VALUE },
    },
    {
      name: 'decode-time overflow',
      next: { totalDecodeTime: Number.MAX_VALUE, framesDecoded: 1, timestamp: 1_000 },
    },
    {
      name: 'rho overflow',
      next: { totalDecodeTime: 1e305, framesDecoded: 1, timestamp: 1 },
    },
  ])('returns unknown when finite counters produce a non-finite $name', ({ next }) => {
    const sampler = new DecoderBudgetSampler();
    observe(sampler, 'camera-1', { totalDecodeTime: 0, framesDecoded: 0, timestamp: 0 });

    expect(observe(sampler, 'camera-1', next)).toEqual({
      usable: false,
      reason: 'invalid-derived-metrics',
    });
  });

  it('does not append missing, stale, or no-progress observations to history', () => {
    const sampler = new DecoderBudgetSampler();
    const cursor = { totalDecodeTime: 0, framesDecoded: 0, timestamp: 0 };
    observe(sampler, 'camera-1', cursor);
    advance(sampler, 'camera-1', cursor, 100);

    observe(sampler, 'camera-1', null);
    cursor.timestamp += 1_000;
    observe(sampler, 'camera-1', cursor);
    observe(sampler, 'camera-1', cursor, { observedAtMs: cursor.timestamp + 10_001 });
    cursor.timestamp += 1_000;
    observe(sampler, 'camera-1', cursor);
    cursor.timestamp += 1_000;
    observe(sampler, 'camera-1', cursor);

    let result: DecoderBudgetResult = { usable: false, reason: 'first-sample' };
    for (let index = 0; index < 18; index++) {
      result = advance(sampler, 'camera-1', cursor, 1);
    }
    expect(expectUsable(result).p95DecodeMs).toBeCloseTo(100);
  });

  it('deletes one consumer without disturbing another consumer history', () => {
    const sampler = new DecoderBudgetSampler();
    const camera = { totalDecodeTime: 0, framesDecoded: 0, timestamp: 0 };
    const screen = { totalDecodeTime: 0, framesDecoded: 0, timestamp: 0 };
    observe(sampler, 'camera', camera);
    observe(sampler, 'screen', screen);
    advance(sampler, 'camera', camera, 100);
    advance(sampler, 'screen', screen, 50);

    sampler.deleteConsumer('camera');

    expect(advance(sampler, 'camera', camera, 1)).toEqual({
      usable: false,
      reason: 'first-sample',
    });
    expect(expectUsable(advance(sampler, 'screen', screen, 1)).p95DecodeMs).toBeCloseTo(50);
  });

  it('clears every consumer baseline and history', () => {
    const sampler = new DecoderBudgetSampler();
    const camera = { totalDecodeTime: 0, framesDecoded: 0, timestamp: 0 };
    const screen = { totalDecodeTime: 0, framesDecoded: 0, timestamp: 0 };
    observe(sampler, 'camera', camera);
    observe(sampler, 'screen', screen);
    advance(sampler, 'camera', camera, 100);
    advance(sampler, 'screen', screen, 50);

    sampler.clear();

    expect(advance(sampler, 'camera', camera, 1)).toEqual({
      usable: false,
      reason: 'first-sample',
    });
    expect(advance(sampler, 'screen', screen, 1)).toEqual({
      usable: false,
      reason: 'first-sample',
    });
  });
});

describe('selectInboundVideoDecoderReport', () => {
  function inbound(
    id: string,
    options: {
      ssrc: number;
      framesPerSecond?: number;
      timestamp?: number;
      active?: boolean;
      kind?: string;
    }
  ) {
    return {
      id,
      type: 'inbound-rtp',
      kind: options.kind ?? 'video',
      totalDecodeTime: 0,
      framesDecoded: 0,
      ...options,
    };
  }

  it('filters inactive and non-video reports', () => {
    const selected = selectInboundVideoDecoderReport(
      [
        ['inactive', inbound('inactive', { ssrc: 1, active: false, framesPerSecond: 60 })],
        ['audio', inbound('audio', { ssrc: 2, kind: 'audio', framesPerSecond: 60 })],
      ],
      new Set([1, 2])
    );

    expect(selected).toBeNull();
  });

  it('prefers a negotiated encoding SSRC over every weaker activity signal', () => {
    const selected = selectInboundVideoDecoderReport(
      [
        ['new-fast', inbound('new-fast', { ssrc: 1, framesPerSecond: 60, timestamp: 5_000 })],
        ['negotiated', inbound('negotiated', { ssrc: 2, framesPerSecond: 1, timestamp: 1 })],
      ],
      new Set([2])
    );

    expect(selected?.reportId).toBe('negotiated|2');
  });

  it('uses positive reported FPS only as selection evidence, then newest timestamp', () => {
    const positiveFps = selectInboundVideoDecoderReport(
      [
        ['new-zero', inbound('new-zero', { ssrc: 1, framesPerSecond: 0, timestamp: 5_000 })],
        ['old-moving', inbound('old-moving', { ssrc: 2, framesPerSecond: 30, timestamp: 1 })],
      ],
      new Set()
    );
    expect(positiveFps?.reportId).toBe('old-moving|2');

    const newest = selectInboundVideoDecoderReport(
      [
        ['old', inbound('old', { ssrc: 1, framesPerSecond: 30, timestamp: 1 })],
        ['new', inbound('new', { ssrc: 2, framesPerSecond: 30, timestamp: 2 })],
      ],
      new Set()
    );
    expect(newest?.reportId).toBe('new|2');
  });

  it('breaks equal-evidence ties by stable report id rather than iteration order', () => {
    const selected = selectInboundVideoDecoderReport(
      [
        ['z-report', inbound('z-report', { ssrc: 2, framesPerSecond: 30, timestamp: 1 })],
        ['a-report', inbound('a-report', { ssrc: 1, framesPerSecond: 30, timestamp: 1 })],
      ],
      new Set()
    );

    expect(selected?.reportId).toBe('a-report|1');
  });
});
