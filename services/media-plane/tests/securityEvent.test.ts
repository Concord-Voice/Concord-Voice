import {
  ftruncateSync,
  mkdtempSync,
  mkdirSync,
  readFileSync,
  renameSync,
  truncateSync,
  writeSync,
} from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SecurityEventWriter } from '../src/lib/securityEvent.js';

const uuid = '11111111-1111-4111-8111-111111111111';
let time = Date.parse('2026-08-31T12:00:00Z');
const event = {
  eventType: 'media_admission' as const,
  outcome: 'denied' as const,
  severity: 'medium' as const,
  reasonCode: 'origin_rejected' as const,
  routeTemplate: 'socket.join' as const,
  correlationRef: 'corr_0123456789abcdef',
};

function writer(path: string): SecurityEventWriter {
  return new SecurityEventWriter({ path, enabled: true, now: () => time, randomUUID: () => uuid });
}
function lines(path: string): Array<Record<string, unknown>> {
  return readFileSync(path, 'utf8')
    .trim()
    .split('\n')
    .filter(Boolean)
    .map((line) => JSON.parse(line));
}

afterEach(() => {
  vi.useRealTimers();
  time = Date.parse('2026-08-31T12:00:00Z');
});

describe('SecurityEventWriter', () => {
  it('stays disabled without retaining telemetry or a timer', () => {
    const sink = new SecurityEventWriter({ enabled: false });
    expect(sink.emit(event)).toBe(false);
    sink.flush();
    sink.close();
    sink.close();
    expect(sink.snapshot()).toMatchObject({ status: 'disabled', pendingUnique: 0 });
  });

  it('writes one closed media-plane event', () => {
    const dir = mkdtempSync(join(tmpdir(), 'nightwatch-'));
    const path = join(dir, 'events.jsonl');
    const sink = writer(path);
    expect(sink.emit(event)).toBe(true);
    sink.flush();
    expect(lines(path)).toEqual([
      {
        schema_version: 'security-event.v1',
        event_id: uuid,
        occurred_at: '2026-08-31T12:00:00.000Z',
        service: 'media-plane',
        event_type: 'media_admission',
        outcome: 'denied',
        severity: 'medium',
        reason_code: 'origin_rejected',
        route_template: 'socket.join',
        count: 1,
        correlation_ref: 'corr_0123456789abcdef',
      },
    ]);
    sink.close();
  });

  it('rejects unknown and prohibited input keys without writing them', () => {
    const path = join(mkdtempSync(join(tmpdir(), 'nightwatch-')), 'events.jsonl');
    const sink = writer(path);
    expect(sink.emit({ ...event, userId: 'forbidden' } as typeof event)).toBe(false);
    expect(sink.emit({ ...event, reasonCode: 'writer_budget_drop' } as typeof event)).toBe(false);
    expect(sink.snapshot().validationFailures).toBe(2);
  });

  it('coalesces repeats and records budget drops locally', () => {
    const path = join(mkdtempSync(join(tmpdir(), 'nightwatch-')), 'events.jsonl');
    const sink = writer(path);
    for (let index = 0; index < 3; index++) sink.emit(event);
    for (let index = 0; index < 99; index++)
      sink.emit({
        ...event,
        evidenceRef: `${index.toString(16).padStart(8, '0')}-0000-4000-8000-000000000000`,
      });
    expect(sink.emit({ ...event, evidenceRef: 'ffffffff-0000-4000-8000-000000000000' })).toBe(
      false
    );
    expect(sink.snapshot().pendingUnique).toBe(100);
    expect(sink.snapshot().dropped).toBe(1);
    sink.flush();
    const written = lines(path);
    expect(written).toHaveLength(100);
    expect(written.find((line) => line.reason_code === 'origin_rejected')?.count).toBe(3);
    expect(written.some((line) => line.event_type === 'pipeline_health')).toBe(false);
  });

  it('uses a timer without retaining an open event-loop handle', () => {
    vi.useFakeTimers();
    const path = join(mkdtempSync(join(tmpdir(), 'nightwatch-')), 'events.jsonl');
    const sink = writer(path);
    sink.emit(event);
    vi.advanceTimersByTime(1000);
    expect(lines(path)).toHaveLength(1);
    sink.close();
  });

  it('reopens after rename/create and refuses active files above 20 MiB', () => {
    const dir = mkdtempSync(join(tmpdir(), 'nightwatch-'));
    const path = join(dir, 'events.jsonl');
    const sink = writer(path);
    sink.emit(event);
    sink.flush();
    renameSync(path, `${path}.1`);
    sink.emit({ ...event, evidenceRef: 'aaaaaaaa-0000-4000-8000-000000000000' });
    sink.flush();
    expect(lines(path)).toHaveLength(1);
    truncateSync(path, 20 << 20);
    sink.emit({ ...event, evidenceRef: 'bbbbbbbb-0000-4000-8000-000000000000' });
    sink.flush();
    expect(sink.snapshot().sizeRefusals).toBe(1);
  });

  it('records a failed write and emits recovery only after the sink is writable', () => {
    const dir = mkdtempSync(join(tmpdir(), 'nightwatch-'));
    const path = join(dir, 'missing', 'events.jsonl');
    const sink = writer(path);
    sink.emit(event);
    sink.flush();
    expect(sink.snapshot().status).toBe('degraded');
    mkdirSync(join(dir, 'missing'));
    time += 1000;
    sink.emit({ ...event, evidenceRef: 'cccccccc-0000-4000-8000-000000000000' });
    sink.flush();
    expect(lines(path).map((line) => line.reason_code)).toEqual([
      'origin_rejected',
      'origin_rejected',
    ]);
    expect(sink.snapshot().status).toBe('healthy');
  });

  it('automatically retries retained records after a write failure', () => {
    vi.useFakeTimers();
    const dir = mkdtempSync(join(tmpdir(), 'nightwatch-'));
    const path = join(dir, 'missing', 'events.jsonl');
    const sink = writer(path);
    sink.emit(event);
    sink.flush();
    expect(sink.snapshot().writeFailures).toBe(1);

    mkdirSync(join(dir, 'missing'));
    vi.advanceTimersByTime(1_000);
    expect(lines(path)).toHaveLength(1);
    expect(sink.snapshot()).toMatchObject({ pendingUnique: 0, status: 'healthy' });
  });

  it('repairs a partial write on the same descriptor before retrying', () => {
    const path = join(mkdtempSync(join(tmpdir(), 'nightwatch-')), 'events.jsonl');
    const sink = writer(path);
    const internals = sink as unknown as {
      writeDescriptor(fd: number, payload: Buffer): number;
      truncateDescriptor(fd: number, size: number): void;
    };
    const writeDescriptor = internals.writeDescriptor.bind(sink);
    let writeCalls = 0;
    internals.writeDescriptor = (fd: number, payload: Buffer) => {
      writeCalls++;
      if (writeCalls === 1) {
        return writeSync(fd, payload.subarray(0, Math.floor(payload.length / 2)));
      }
      return writeDescriptor(fd, payload);
    };
    let truncateCalls = 0;
    let repairFd: number | undefined;
    internals.truncateDescriptor = (fd: number, size: number) => {
      truncateCalls++;
      if (truncateCalls <= 2) {
        if (repairFd !== undefined) expect(fd).toBe(repairFd);
        repairFd = fd;
        throw new Error('injected truncate failure');
      }
      expect(fd).toBe(repairFd);
      ftruncateSync(fd, size);
    };

    sink.emit(event);
    sink.flush();
    const torn = readFileSync(path, 'utf8');
    expect(torn).not.toBe('');
    expect(torn).not.toContain('\n');
    expect(sink.snapshot()).toMatchObject({ pendingUnique: 1, status: 'degraded' });

    sink.flush();
    expect(readFileSync(path, 'utf8')).toBe(torn);
    expect(writeCalls).toBe(1);
    expect(sink.snapshot()).toMatchObject({ pendingUnique: 1, status: 'degraded' });

    sink.flush();
    expect(lines(path)).toHaveLength(1);
    expect(writeCalls).toBe(2);
    expect(truncateCalls).toBe(3);
    expect(sink.snapshot()).toMatchObject({ pendingUnique: 0, status: 'healthy' });
    sink.close();
  });

  it('rolls back bytes written before the write operation throws', () => {
    const path = join(mkdtempSync(join(tmpdir(), 'nightwatch-')), 'events.jsonl');
    const sink = writer(path);
    const internals = sink as unknown as {
      writeDescriptor(fd: number, payload: Buffer): number;
    };
    const writeDescriptor = internals.writeDescriptor.bind(sink);
    let writeCalls = 0;
    internals.writeDescriptor = (fd: number, payload: Buffer) => {
      writeCalls++;
      if (writeCalls === 1) {
        writeSync(fd, payload.subarray(0, Math.floor(payload.length / 2)));
        throw new Error('injected write failure after partial append');
      }
      return writeDescriptor(fd, payload);
    };

    sink.emit(event);
    sink.flush();
    expect(readFileSync(path, 'utf8')).toBe('');
    expect(sink.snapshot()).toMatchObject({ pendingUnique: 1, status: 'degraded' });

    sink.flush();
    expect(lines(path)).toHaveLength(1);
    expect(writeCalls).toBe(2);
    expect(sink.snapshot()).toMatchObject({ pendingUnique: 0, status: 'healthy' });
    sink.close();
  });

  it('does not re-arm a retained retry after close', async () => {
    vi.useFakeTimers();
    const dir = mkdtempSync(join(tmpdir(), 'nightwatch-'));
    const path = join(dir, 'missing', 'events.jsonl');
    const sink = writer(path);
    sink.emit(event);
    sink.flush();
    expect(sink.snapshot().writeFailures).toBe(1);

    const close = sink.close();
    await vi.advanceTimersByTimeAsync(1_000);
    expect(await close).toBe(false);
    const failuresAfterClose = sink.snapshot().writeFailures;
    vi.advanceTimersByTime(10_000);
    expect(sink.snapshot().writeFailures).toBe(failuresAfterClose);
  });

  it('degrades on size refusal and restores after the file is replaced', () => {
    const dir = mkdtempSync(join(tmpdir(), 'nightwatch-'));
    const path = join(dir, 'events.jsonl');
    const sink = writer(path);
    sink.emit(event);
    sink.flush();
    truncateSync(path, 20 << 20);
    sink.emit({ ...event, evidenceRef: 'bbbbbbbb-0000-4000-8000-000000000000' });
    sink.flush();
    expect(sink.snapshot()).toMatchObject({ sizeRefusals: 1, status: 'degraded' });

    renameSync(path, `${path}.full`);
    sink.emit({ ...event, evidenceRef: 'cccccccc-0000-4000-8000-000000000000' });
    sink.flush();
    expect(sink.snapshot()).toMatchObject({ pendingUnique: 0, status: 'healthy' });
  });

  it('drops deterministic record failures once and continues with later records', () => {
    const dir = mkdtempSync(join(tmpdir(), 'nightwatch-'));
    const path = join(dir, 'events.jsonl');
    const ids = ['not-a-uuid', uuid];
    const sink = new SecurityEventWriter({
      path,
      enabled: true,
      now: () => time,
      randomUUID: () => ids.shift() ?? uuid,
    });
    sink.emit(event);
    sink.emit({ ...event, evidenceRef: 'dddddddd-0000-4000-8000-000000000000' });
    sink.flush();

    expect(lines(path)).toHaveLength(1);
    expect(sink.snapshot()).toMatchObject({
      dropped: 1,
      internalFailures: 1,
      pendingUnique: 0,
      status: 'healthy',
    });
  });

  it('drains retained records during bounded shutdown retry', async () => {
    vi.useFakeTimers();
    const dir = mkdtempSync(join(tmpdir(), 'nightwatch-'));
    const path = join(dir, 'missing', 'events.jsonl');
    const sink = writer(path);
    sink.emit(event);
    const firstClose = sink.close();
    mkdirSync(join(dir, 'missing'));
    await vi.advanceTimersByTimeAsync(1_000);
    expect(await firstClose).toBe(true);
    expect(sink.snapshot().pendingUnique).toBe(0);
    expect(lines(path).map((line) => line.reason_code)).toEqual(['origin_rejected']);
    expect(sink.snapshot()).toMatchObject({ pendingUnique: 0, status: 'healthy' });
  });

  it('retains a successful append for at-least-once retry when closing fails', async () => {
    vi.useFakeTimers();
    const dir = mkdtempSync(join(tmpdir(), 'nightwatch-'));
    const path = join(dir, 'events.jsonl');
    const sink = writer(path);
    sink.emit(event);
    const internals = sink as unknown as {
      closeDescriptor(fd: number): boolean;
      recordWriteFailure(): false;
    };
    const closeDescriptor = internals.closeDescriptor.bind(sink);
    const recordWriteFailure = internals.recordWriteFailure.bind(sink);
    internals.closeDescriptor = (fd: number) => {
      closeDescriptor(fd);
      return recordWriteFailure();
    };
    sink.flush();
    expect(sink.snapshot()).toMatchObject({
      status: 'degraded',
      pendingUnique: 1,
      writeFailures: 1,
    });
    expect(lines(path)).toHaveLength(1);
    expect(lines(path)[0]).toMatchObject({ reason_code: 'origin_rejected', count: 1 });
    const closing = sink.close();
    await vi.advanceTimersByTimeAsync(1_000);
    expect(await closing).toBe(false);
    expect(sink.snapshot()).toMatchObject({ status: 'degraded', pendingUnique: 1 });
  });

  it('does not throw when a clock or UUID source fails', () => {
    const path = join(mkdtempSync(join(tmpdir(), 'nightwatch-')), 'events.jsonl');
    const badClock = new SecurityEventWriter({
      path,
      enabled: true,
      now: () => {
        throw new Error('no clock');
      },
      randomUUID: () => uuid,
    });
    expect(() => badClock.emit(event)).not.toThrow();
    expect(badClock.snapshot()).toMatchObject({
      status: 'degraded',
      internalFailures: 1,
      validationFailures: 0,
    });
    const badCorrelationUUID = new SecurityEventWriter({
      path,
      enabled: true,
      now: () => time,
      randomUUID: () => {
        throw new Error('no uuid');
      },
    });
    expect(() => badCorrelationUUID.emit({ ...event, correlationRef: undefined })).not.toThrow();
    expect(badCorrelationUUID.snapshot()).toMatchObject({
      status: 'degraded',
      internalFailures: 1,
      pendingUnique: 0,
    });
    const badUUID = new SecurityEventWriter({
      path,
      enabled: true,
      now: () => time,
      randomUUID: () => {
        throw new Error('no uuid');
      },
    });
    badUUID.emit(event);
    expect(() => badUUID.flush()).not.toThrow();
    const invalidUUID = new SecurityEventWriter({
      path,
      enabled: true,
      now: () => time,
      randomUUID: () => 'not-a-uuid',
    });
    invalidUUID.emit(event);
    invalidUUID.flush();
    expect(invalidUUID.snapshot().internalFailures).toBe(1);
    expect(invalidUUID.emit({ ...event, correlationRef: 'short' })).toBe(false);
  });
});
