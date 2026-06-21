import { describe, it, expect, beforeEach } from 'vitest';
import { MediaMetrics, type MetricsSample } from '../src/lib/mediaMetrics';

function sample(over: Partial<MetricsSample> = {}): MetricsSample {
  const egressBytesByTransport = over.egressBytesByTransport ?? new Map<string, number>();
  return {
    publishers: over.publishers ?? { camera: 0, screen: 0 },
    activeByKind: over.activeByKind ?? { audio: 0, webcam: 0, screenshare: 0 },
    egressBytesByTransport,
    // By default a transport that reported bytes is "live". Override liveTransportIds
    // explicitly to model a transient getStats failure (live but no bytes this tick).
    liveTransportIds: over.liveTransportIds ?? new Set(egressBytesByTransport.keys()),
    perRoomVideoPublishers: over.perRoomVideoPublishers ?? [],
  };
}

describe('MediaMetrics', () => {
  let m: MediaMetrics;
  beforeEach(() => {
    m = new MediaMetrics();
  });

  it('accumulates participant-hours by media type across ticks', () => {
    m.ingest(sample({ activeByKind: { audio: 6, webcam: 3, screenshare: 1 } }), 30);
    m.ingest(sample({ activeByKind: { audio: 6, webcam: 3, screenshare: 1 } }), 30);
    const s = m.getSnapshot();
    expect(s.participantHoursByKind.audio).toBeCloseTo((6 * 60) / 3600); // 0.1h
    expect(s.participantHoursByKind.webcam).toBeCloseTo((3 * 60) / 3600);
    expect(s.participantHoursByKind.screenshare).toBeCloseTo((1 * 60) / 3600);
  });

  it('reports latest concurrency and the per-room peak', () => {
    m.ingest(sample({ publishers: { camera: 4, screen: 1 }, perRoomVideoPublishers: [3, 2] }), 30);
    m.ingest(sample({ publishers: { camera: 2, screen: 0 }, perRoomVideoPublishers: [5] }), 30);
    const s = m.getSnapshot();
    expect(s.concurrentVideoPublishers).toEqual({ camera: 2, screen: 0 });
    expect(s.peakConcurrentVideoPublishersPerRoom).toBe(5);
  });

  it('computes egress delta, cumulative bytes, bps and peak; ignores monotonic resets', () => {
    m.ingest(sample({ egressBytesByTransport: new Map([['t1', 1000]]) }), 10);
    expect(m.getSnapshot().egress.cumulativeBytes).toBe(1000);
    expect(m.getSnapshot().egress.currentBps).toBe(800); // 1000 bytes * 8 / 10s
    m.ingest(sample({ egressBytesByTransport: new Map([['t1', 3000]]) }), 10);
    expect(m.getSnapshot().egress.cumulativeBytes).toBe(3000); // +2000
    expect(m.getSnapshot().egress.peakBps).toBe(1600);
    // reset (transport recreated, counter < last): clamped to 0 delta
    m.ingest(sample({ egressBytesByTransport: new Map([['t1', 500]]) }), 10);
    expect(m.getSnapshot().egress.cumulativeBytes).toBe(3000);
    expect(m.getSnapshot().egress.currentBps).toBe(0);
  });

  it('prunes a transport only when it leaves room state (gone from liveTransportIds)', () => {
    m.ingest(sample({ egressBytesByTransport: new Map([['t1', 1000]]) }), 10);
    // t1 truly closed: absent from BOTH egress bytes and liveTransportIds
    m.ingest(sample({ egressBytesByTransport: new Map(), liveTransportIds: new Set() }), 10);
    // t1 reappears as a brand-new transport -> counts from 0 again, not from 1000
    m.ingest(sample({ egressBytesByTransport: new Map([['t1', 200]]) }), 10);
    expect(m.getSnapshot().egress.cumulativeBytes).toBe(1200); // 1000 + 200
  });

  it('does NOT double-count egress on a transient getStats failure (transport still live) (#1553 Gitar)', () => {
    // tick1: t1 reports 1000, live
    m.ingest(sample({ egressBytesByTransport: new Map([['t1', 1000]]) }), 10);
    expect(m.getSnapshot().egress.cumulativeBytes).toBe(1000);
    // tick2: getStats() threw for t1 — omitted from egress bytes BUT still live (not gone)
    m.ingest(sample({ egressBytesByTransport: new Map(), liveTransportIds: new Set(['t1']) }), 10);
    expect(m.getSnapshot().egress.cumulativeBytes).toBe(1000); // unchanged, NOT pruned
    expect(m.getSnapshot().egress.currentBps).toBe(0);
    // tick3: t1 recovers at 5000 — delta is 5000-1000=4000, NOT a re-count of the full 5000
    m.ingest(sample({ egressBytesByTransport: new Map([['t1', 5000]]) }), 10);
    expect(m.getSnapshot().egress.cumulativeBytes).toBe(5000); // 1000 + 4000, not 6000
  });

  it('reset zeroes everything', () => {
    m.ingest(sample({ activeByKind: { audio: 6, webcam: 0, screenshare: 0 } }), 30);
    m.ingest(sample({ egressBytesByTransport: new Map([['t1', 1000]]) }), 10);
    m.reset();
    const s = m.getSnapshot();
    expect(s.participantHoursByKind.audio).toBe(0);
    expect(s.egress.cumulativeBytes).toBe(0);
    expect(s.egress.peakBps).toBe(0);
    expect(s.peakConcurrentVideoPublishersPerRoom).toBe(0);
  });
});
