import { describe, it, expect, vi } from 'vitest';
import {
  AttachmentTooLargeError,
  parseContentLength,
  readBoundedBody,
} from '@/renderer/utils/boundedResponseBody';

const MAX = 1024;

/** A Response-shaped stub with a real ReadableStream body. */
function streamingResponse(chunkSizes: number[], headers: Record<string, string> = {}): Response {
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const n of chunkSizes) controller.enqueue(new Uint8Array(n));
      controller.close();
    },
  });
  return { body, headers: new Headers(headers) } as unknown as Response;
}

/** A Response-shaped stub with NO stream — the jsdom / polyfill shape. */
function bufferedResponse(bytes: number, headers: Record<string, string> = {}): Response {
  return {
    body: undefined,
    headers: new Headers(headers),
    arrayBuffer: vi.fn(() => Promise.resolve(new ArrayBuffer(bytes))),
  } as unknown as Response;
}

describe('parseContentLength', () => {
  // The VULN-001 root cause: Number(null) === 0, which compares below every
  // ceiling, so an absent header silently satisfied a "too large?" check.
  it('returns undefined for an ABSENT header — never 0', () => {
    expect(parseContentLength(null)).toBeUndefined();
    expect(parseContentLength(null)).not.toBe(0);
  });

  it('returns undefined for an empty or whitespace header', () => {
    expect(parseContentLength('')).toBeUndefined();
    expect(parseContentLength('   ')).toBeUndefined();
  });

  it('returns undefined for a malformed or negative header', () => {
    expect(parseContentLength('not-a-number')).toBeUndefined();
    expect(parseContentLength('-1')).toBeUndefined();
    expect(parseContentLength('Infinity')).toBeUndefined();
  });

  it('parses a real length, including a legitimate zero-byte body', () => {
    expect(parseContentLength('4096')).toBe(4096);
    expect(parseContentLength('0')).toBe(0);
  });
});

describe('readBoundedBody', () => {
  it('returns a body that fits, reassembled across chunks', async () => {
    const buf = await readBoundedBody(streamingResponse([100, 200, 300]), MAX);
    expect(buf.byteLength).toBe(600);
  });

  it('rejects early on an honest oversized Content-Length without streaming', async () => {
    const res = streamingResponse([10], { 'Content-Length': String(MAX + 1) });
    await expect(readBoundedBody(res, MAX)).rejects.toBeInstanceOf(AttachmentTooLargeError);
  });

  // THE EXPLOIT: chunked transfer encoding omits Content-Length entirely.
  it('refuses an oversized body when Content-Length is ABSENT', async () => {
    const res = streamingResponse([512, 512, 512], {}); // 1536 > 1024, header absent
    await expect(readBoundedBody(res, MAX)).rejects.toBeInstanceOf(AttachmentTooLargeError);
  });

  // gzip: Content-Length is the COMPRESSED length, the body inflates past it.
  it('refuses an oversized body when Content-Length understates it', async () => {
    const res = streamingResponse([512, 512, 512], { 'Content-Length': '10' });
    await expect(readBoundedBody(res, MAX)).rejects.toBeInstanceOf(AttachmentTooLargeError);
  });

  it('cancels the transfer instead of draining it, and drops what it buffered', async () => {
    const cancel = vi.fn(() => Promise.resolve());
    const read = vi
      .fn()
      .mockResolvedValueOnce({ done: false, value: new Uint8Array(900) })
      .mockResolvedValueOnce({ done: false, value: new Uint8Array(900) })
      .mockResolvedValue({ done: true, value: undefined });
    const res = {
      body: { getReader: () => ({ read, cancel }) },
      headers: new Headers(),
    } as unknown as Response;

    await expect(readBoundedBody(res, MAX)).rejects.toBeInstanceOf(AttachmentTooLargeError);
    expect(cancel).toHaveBeenCalledOnce();
    // Stopped at the breach — it did not keep pulling to the end of the body.
    expect(read).toHaveBeenCalledTimes(2);
  });

  it('reports the OBSERVED byte count, not the declared one', async () => {
    const res = streamingResponse([2000], { 'Content-Length': '10' });
    await expect(readBoundedBody(res, MAX)).rejects.toMatchObject({ byteLength: 2000 });
  });

  // Content-Length is NOT a size to allocate against: fetch decompresses
  // transparently, so the header is the compressed length while the body is the
  // decompressed bytes. These cases pin that the reader ignores it for sizing.
  it('assembles correctly when Content-Length matches the body', async () => {
    const res = streamingResponse([100, 100, 100], { 'Content-Length': '300' });
    const buf = await readBoundedBody(res, MAX);
    expect(buf.byteLength).toBe(300);
  });

  it('assembles correctly when the declared length UNDER-reports the body (gzip)', async () => {
    // The common case: Content-Length is the compressed size, body inflates.
    const res = streamingResponse([100, 100, 100], { 'Content-Length': '150' });
    const buf = await readBoundedBody(res, MAX);
    expect(buf.byteLength).toBe(300);
  });

  it('hands back only what arrived when the server sends fewer bytes than declared', async () => {
    const res = streamingResponse([100], { 'Content-Length': '500' });
    const buf = await readBoundedBody(res, MAX);
    expect(buf.byteLength).toBe(100);
  });

  // Review row 7: a cancelled stream knows only what it buffered, so the count
  // is a lower bound. Copy must not present it as the file's size.
  it('marks a cancelled-mid-stream refusal as truncated', async () => {
    const res = streamingResponse([600, 600], {}); // no declared length
    await expect(readBoundedBody(res, MAX)).rejects.toMatchObject({ truncated: true });
  });

  it('does NOT mark an honestly-declared oversize refusal as truncated', async () => {
    const res = streamingResponse([10], { 'Content-Length': String(MAX + 1) });
    await expect(readBoundedBody(res, MAX)).rejects.toMatchObject({
      truncated: false,
      byteLength: MAX + 1,
    });
  });

  it('accepts a body exactly at the cap', async () => {
    const buf = await readBoundedBody(streamingResponse([MAX]), MAX);
    expect(buf.byteLength).toBe(MAX);
  });

  describe('no-stream fallback (jsdom / polyfills)', () => {
    it('returns a body that fits', async () => {
      const buf = await readBoundedBody(bufferedResponse(600), MAX);
      expect(buf.byteLength).toBe(600);
    });

    it('still refuses an oversized body, so decrypt and Blob never see it', async () => {
      await expect(readBoundedBody(bufferedResponse(MAX + 1), MAX)).rejects.toBeInstanceOf(
        AttachmentTooLargeError
      );
    });

    it('rejects on an oversized Content-Length without calling arrayBuffer at all', async () => {
      const res = bufferedResponse(10, { 'Content-Length': String(MAX + 1) });
      await expect(readBoundedBody(res, MAX)).rejects.toBeInstanceOf(AttachmentTooLargeError);
      expect(res.arrayBuffer).not.toHaveBeenCalled();
    });
  });
});
