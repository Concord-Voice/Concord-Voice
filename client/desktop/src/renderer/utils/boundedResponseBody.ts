/**
 * Bounded reading of an attachment response body.
 *
 * Both attachment download paths must refuse an oversized body BEFORE it lands
 * in the renderer heap. Neither declared size is trustworthy on its own:
 *
 *  - `attachment.file_size` is server-supplied metadata riding on the message
 *    summary, so a hostile or compromised server simply understates it. Concord
 *    is self-hostable, so "the server is honest" is not an assumption this
 *    client gets to make.
 *  - `Content-Length` is ABSENT on every HTTP/1.1 `Transfer-Encoding: chunked`
 *    response, absent on HTTP/2 responses that omit it, and describes the
 *    COMPRESSED length under `Content-Encoding: gzip`.
 *
 * The second point hid a fail-open guard (#2157 review, VULN-001):
 * `Number(headers.get('Content-Length'))` is `Number(null)` → `0` when the
 * header is absent, `Number.isFinite(0)` is `true`, and `0 > max` is `false`,
 * so the check evaluated cleanly and waved the body through. An absent header
 * means UNKNOWN, never zero.
 *
 * So the real bound is measured off the bytes as they arrive.
 */

export class AttachmentTooLargeError extends Error {
  readonly byteLength: number;
  /**
   * True when `byteLength` is only what we had buffered when the cap tripped,
   * not the attachment's real size — which is the case whenever the transfer is
   * cancelled mid-stream because no honest declared size was available.
   *
   * Copy must not state a partial count as the file's size: "this file is
   * 256 MB" is then really just the guard ceiling read back to the user.
   */
  readonly truncated: boolean;

  constructor(byteLength: number, truncated = false) {
    super(`attachment too large to open (${byteLength} bytes${truncated ? ', truncated' : ''})`);
    this.name = 'AttachmentTooLargeError';
    this.byteLength = byteLength;
    this.truncated = truncated;
  }
}

/**
 * Parse a `Content-Length` header into a byte count, or `undefined` when the
 * transport did not state one. Absent, empty, and malformed all mean UNKNOWN —
 * they must never collapse to `0`, which compares below every ceiling.
 */
export function parseContentLength(raw: string | null): number | undefined {
  if (raw === null) return undefined;
  const trimmed = raw.trim();
  if (trimmed === '') return undefined;
  const n = Number(trimmed);
  if (!Number.isFinite(n) || n < 0) return undefined;
  return n;
}

/**
 * Read `response`'s body, refusing to buffer more than `maxBytes`.
 *
 * Streams where the platform gives us a stream, counting bytes as they arrive
 * and cancelling the transfer the moment the running total exceeds the cap — so
 * peak allocation is bounded by `maxBytes` plus one chunk regardless of what the
 * server actually sends, and regardless of what it claimed.
 *
 * Falls back to `arrayBuffer()` plus a post-check when no stream is available
 * (jsdom, some polyfills). That fallback still materialises the body once, so it
 * is strictly weaker — but it stops the decrypt and Blob copies that follow, and
 * it is the only option on that platform.
 *
 * @throws {AttachmentTooLargeError} with the observed (not declared) byte count.
 */
/** Join buffered chunks into one exact-size ArrayBuffer. */
function joinChunks(chunks: readonly Uint8Array[], total: number): ArrayBuffer {
  // A lone chunk that already owns its whole buffer needs no copy at all.
  if (chunks.length === 1) {
    const only = chunks[0];
    if (only.byteOffset === 0 && only.byteLength === only.buffer.byteLength) {
      return only.buffer as ArrayBuffer;
    }
  }
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return out.buffer;
}

/**
 * Drain a reader, refusing past `maxBytes` and cancelling rather than draining.
 *
 * NOTE ON MEMORY: between the last chunk and the join this holds up to ~2x the
 * body. An earlier revision preallocated from `Content-Length` to hold it once
 * — that is WRONG and was reverted: `fetch` decompresses transparently, so
 * `response.body` yields DECOMPRESSED bytes while `Content-Length` describes
 * the COMPRESSED size. Preallocating from it under-sizes every gzipped
 * response, and the branching needed to recover from that mis-sizing is what
 * made this function unreadable (cognitive complexity 26) — a poor trade in a
 * security primitive that has to be auditable. The transient is bounded by
 * `maxBytes` either way, and the chunked-format successor removes the need to
 * hold a whole body at all.
 */
async function drainBounded(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  maxBytes: number
): Promise<ArrayBuffer> {
  const chunks: Uint8Array[] = [];
  let total = 0;

  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    if (!value) continue;

    total += value.byteLength;
    if (total > maxBytes) {
      chunks.length = 0;
      await reader.cancel().catch(() => undefined);
      // Only what arrived before the cap tripped — a lower bound, not the size.
      throw new AttachmentTooLargeError(total, true);
    }
    chunks.push(value);
  }

  return joinChunks(chunks, total);
}

export async function readBoundedBody(response: Response, maxBytes: number): Promise<ArrayBuffer> {
  // Cheap early reject when the transport DOES state a length — saves streaming
  // a body we already know we will refuse. Never the only defence, and never a
  // size to allocate against (see drainBounded).
  const declared = parseContentLength(response.headers.get('Content-Length'));
  if (declared !== undefined && declared > maxBytes) {
    throw new AttachmentTooLargeError(declared);
  }

  const body = response.body;
  if (body) return drainBounded(body.getReader(), maxBytes);

  // No stream (jsdom, some polyfills). Materialises the body once, which is
  // strictly weaker — but it still stops the decrypt and Blob copies that
  // follow, and it is the only option on that platform.
  const buffer = await response.arrayBuffer();
  if (buffer.byteLength > maxBytes) {
    throw new AttachmentTooLargeError(buffer.byteLength);
  }
  return buffer;
}

/**
 * The single sentence both download paths use to refuse an oversized
 * attachment. Lived in two places (a component and a template literal) with two
 * test files asserting the wording, so editing one broke tests pointing at the
 * other.
 *
 * `truncated` means the count is only what arrived before the transfer was
 * cancelled, so the copy says "over" rather than stating it as the file's size.
 */
export function tooLargeMessage(
  bytes: number,
  truncated = false,
  format: (n: number) => string = String
): string {
  return truncated
    ? `This file is over ${format(bytes)} and is too large to open in this version of Concord.`
    : `This file is ${format(bytes)} and is too large to open in this version of Concord.`;
}
