/**
 * Bounds-safe cursor over image bytes (#2469).
 *
 * Every parser in this module reads through here rather than touching a DataView
 * directly. The input is an untrusted file the user dragged in, so a malformed
 * or truncated container must produce a thrown error at a known point rather
 * than an out-of-range read, an accidental `NaN`, or a walk loop that never
 * terminates.
 *
 * The iteration budget is the second half of that: a container whose structure
 * is a chain (JPEG segments, PNG chunks, BMFF boxes, IFD entries) can be crafted
 * so the chain is enormous or self-referential. Bounds checks alone do not stop
 * a loop that makes progress one byte at a time through a 25 MiB file.
 */

/** Thrown for any malformed, truncated, or over-long input. Callers fail closed. */
export class ImageParseError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'ImageParseError';
  }
}

export class ByteReader {
  private readonly view: DataView;
  private offset = 0;
  private steps = 0;

  /**
   * @param bytes  the buffer to read
   * @param budget maximum number of `step()` calls before the read is abandoned.
   *               Parsers call `step()` once per structural element.
   */
  constructor(
    private readonly bytes: Uint8Array,
    private readonly budget = 100_000
  ) {
    this.view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  }

  get position(): number {
    return this.offset;
  }

  get length(): number {
    return this.bytes.byteLength;
  }

  remaining(): number {
    return this.bytes.byteLength - this.offset;
  }

  eof(): boolean {
    return this.offset >= this.bytes.byteLength;
  }

  /**
   * Count one structural element. Parsers call this once per segment, chunk,
   * box, or directory entry so a crafted chain cannot spin indefinitely.
   */
  step(what: string): void {
    if (++this.steps > this.budget) {
      throw new ImageParseError(`too many ${what} elements (budget ${this.budget})`);
    }
  }

  seek(to: number): void {
    if (to < 0 || to > this.bytes.byteLength) {
      throw new ImageParseError(`seek out of range: ${to} of ${this.bytes.byteLength}`);
    }
    this.offset = to;
  }

  skip(n: number): void {
    this.seek(this.offset + n);
  }

  private require(n: number): number {
    const at = this.offset;
    if (n < 0 || at + n > this.bytes.byteLength) {
      throw new ImageParseError(`read of ${n} at ${at} exceeds ${this.bytes.byteLength} bytes`);
    }
    this.offset = at + n;
    return at;
  }

  u8(): number {
    return this.view.getUint8(this.require(1));
  }

  u16be(): number {
    return this.view.getUint16(this.require(2), false);
  }

  u16le(): number {
    return this.view.getUint16(this.require(2), true);
  }

  u32be(): number {
    return this.view.getUint32(this.require(4), false);
  }

  u32le(): number {
    return this.view.getUint32(this.require(4), true);
  }

  /**
   * 64-bit big-endian, used only by ISO-BMFF extended box sizes. Returned as a
   * number: a box larger than Number.MAX_SAFE_INTEGER cannot address bytes in a
   * buffer we already hold, so it is rejected rather than silently truncated.
   */
  u64be(): number {
    const hi = this.u32be();
    const lo = this.u32be();
    const value = hi * 0x100000000 + lo;
    if (!Number.isSafeInteger(value)) {
      throw new ImageParseError('64-bit size exceeds safe integer range');
    }
    return value;
  }

  /** A copy of `n` bytes. Copied rather than subarrayed so callers cannot alias the source. */
  bytesOf(n: number): Uint8Array {
    const at = this.require(n);
    return this.bytes.slice(at, at + n);
  }

  /** A view of `n` bytes without copying. For comparison only — never retained. */
  peek(n: number, at = this.offset): Uint8Array {
    if (at < 0 || at + n > this.bytes.byteLength) {
      throw new ImageParseError(`peek of ${n} at ${at} exceeds ${this.bytes.byteLength} bytes`);
    }
    return this.bytes.subarray(at, at + n);
  }

  /** ASCII of `n` bytes — container tags (`ftyp`, `EXIF`, `IHDR`), never user text. */
  ascii(n: number): string {
    const at = this.require(n);
    let out = '';
    for (let i = 0; i < n; i++) {
      out += String.fromCodePoint(this.bytes[at + i]);
    }
    return out;
  }
}

/** True when `haystack` starts with `needle` at `at`. */
export function startsWith(haystack: Uint8Array, needle: readonly number[], at = 0): boolean {
  if (at + needle.length > haystack.byteLength) {
    return false;
  }
  for (let i = 0; i < needle.length; i++) {
    if (haystack[at + i] !== needle[i]) {
      return false;
    }
  }
  return true;
}

/** ASCII bytes of a 4-character container tag, for comparison against `startsWith`. */
export function tag(text: string): number[] {
  return Array.from(text, (c) => c.codePointAt(0) ?? 0);
}

/** Concatenates parts into one buffer. Used by every parser to emit its result. */
export function concat(parts: readonly Uint8Array[]): ArrayBuffer {
  let total = 0;
  for (const p of parts) {
    total += p.byteLength;
  }
  const out = new Uint8Array(total);
  let at = 0;
  for (const p of parts) {
    out.set(p, at);
    at += p.byteLength;
  }
  return out.buffer;
}
