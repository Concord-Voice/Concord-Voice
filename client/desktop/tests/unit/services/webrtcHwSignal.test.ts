import { describe, it, expect } from 'vitest';
import { extractWebrtcHwSignal } from '@/renderer/services/webrtcHwSignal';

/** Build a Map that satisfies the RTCStatsReport (ReadonlyMap) shape for the helper. */
const report = (entries: Record<string, unknown>[]): RTCStatsReport =>
  new Map(entries.map((e, i) => [String(e.id ?? i), e])) as unknown as RTCStatsReport;

describe('extractWebrtcHwSignal', () => {
  it('returns codec mime + powerEfficient=true for a hardware video encoder', () => {
    const stats = report([
      { type: 'codec', id: 'c1', mimeType: 'video/AV1' },
      { type: 'outbound-rtp', kind: 'video', codecId: 'c1', powerEfficientEncoder: true },
    ]);
    expect(extractWebrtcHwSignal(stats)).toEqual({ mime: 'video/av1', powerEfficient: true });
  });

  it('returns powerEfficient=false for a software WebRTC encoder (e.g. libaom AV1)', () => {
    const stats = report([
      { type: 'codec', id: 'c1', mimeType: 'video/AV1' },
      { type: 'outbound-rtp', kind: 'video', codecId: 'c1', powerEfficientEncoder: false },
    ]);
    expect(extractWebrtcHwSignal(stats)).toEqual({ mime: 'video/av1', powerEfficient: false });
  });

  it('accepts the legacy mediaType field as a fallback for kind', () => {
    const stats = report([
      { type: 'codec', id: 'c9', mimeType: 'video/H264' },
      { type: 'outbound-rtp', mediaType: 'video', codecId: 'c9', powerEfficientEncoder: true },
    ]);
    expect(extractWebrtcHwSignal(stats)).toEqual({ mime: 'video/h264', powerEfficient: true });
  });

  it('prefers an active simulcast layer over a later inactive one (multiple outbound-rtp)', () => {
    // Camera is simulcast: one outbound-rtp per rid; the active layer reports the HW
    // bit while a later inactive layer leaves it undefined — the active value must win.
    const stats = report([
      { type: 'codec', id: 'c1', mimeType: 'video/VP9' },
      { type: 'outbound-rtp', kind: 'video', codecId: 'c1', powerEfficientEncoder: true },
      { type: 'outbound-rtp', kind: 'video', codecId: 'c1' }, // trailing inactive layer
    ]);
    expect(extractWebrtcHwSignal(stats)).toEqual({ mime: 'video/vp9', powerEfficient: true });
  });

  it('returns null when powerEfficientEncoder is not yet populated (runtime-only field)', () => {
    const stats = report([
      { type: 'codec', id: 'c1', mimeType: 'video/VP9' },
      { type: 'outbound-rtp', kind: 'video', codecId: 'c1' },
    ]);
    expect(extractWebrtcHwSignal(stats)).toBeNull();
  });

  it('ignores audio outbound-rtp', () => {
    const stats = report([
      { type: 'codec', id: 'a1', mimeType: 'audio/opus' },
      { type: 'outbound-rtp', kind: 'audio', codecId: 'a1', powerEfficientEncoder: true },
    ]);
    expect(extractWebrtcHwSignal(stats)).toBeNull();
  });

  it('returns null when the codec entry for the outbound-rtp is missing', () => {
    const stats = report([
      { type: 'outbound-rtp', kind: 'video', codecId: 'missing', powerEfficientEncoder: true },
    ]);
    expect(extractWebrtcHwSignal(stats)).toBeNull();
  });
});
