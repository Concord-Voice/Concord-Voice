import { describe, it, expect } from 'vitest';
import {
  encodeEnvelopeHeader,
  decodeEnvelopeHeader,
  buildChunkAad,
  totalChunksFor,
  expectedBlobLength,
  CHUNK_PLAINTEXT_BYTES,
  ENVELOPE_HEADER_BYTES,
  UnsupportedAttachmentFormatError,
  newEnvelopeHeader,
  buildUploadPart,
  bufferChunkSource,
  fileChunkSource,
  CHUNK_OVERHEAD_BYTES,
  type AttachmentEnvelopeHeader,
  classifyEnvelope,
  decryptAttachmentBlob,
  AttachmentIntegrityError,
  parseKeyVersionHeader,
  AttachmentKeyEpochError,
  type ChunkSource,
} from '@/renderer/utils/attachmentChunkedCrypto';
import { encryptFile } from '@/renderer/utils/attachmentCrypto';

const nonce = (fill: number) => new Uint8Array(16).fill(fill);

const header = (over: Partial<AttachmentEnvelopeHeader> = {}): AttachmentEnvelopeHeader => ({
  version: 2,
  flags: 0,
  chunkSize: CHUNK_PLAINTEXT_BYTES,
  totalChunks: 3,
  fileNonce: nonce(0xab),
  ...over,
});

describe('envelope version rollout', () => {
  it('constructs an explicitly selected v3 write while retaining v2 geometry', () => {
    expect(newEnvelopeHeader(1, 3).version).toBe(3);
    const v3FirstChunkCapacity = CHUNK_PLAINTEXT_BYTES - ENVELOPE_HEADER_BYTES;
    expect(newEnvelopeHeader(v3FirstChunkCapacity, 3).totalChunks).toBe(1);
    expect(newEnvelopeHeader(v3FirstChunkCapacity + 1, 3).totalChunks).toBe(2);

    const v2 = newEnvelopeHeader(CHUNK_PLAINTEXT_BYTES + 1, 2);
    expect(v2.version).toBe(2);
    expect(v2.totalChunks).toBe(2);
  });

  it('binds v3 to the CVA3 AAD domain', () => {
    const aad = buildChunkAad(header({ version: 3 }), 0);
    expect(Array.from(aad.slice(0, 4))).toEqual([0x43, 0x56, 0x41, 0x33]);
  });

  it('makes default v3 non-trailing parts uniformly sized', async () => {
    const key = await crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, false, [
      'encrypt',
      'decrypt',
    ]);
    const plaintext = new Uint8Array(CHUNK_PLAINTEXT_BYTES * 2 + 1);
    const h = newEnvelopeHeader(plaintext.byteLength, 3);
    expect(h.version).toBe(3);
    expect(h.totalChunks).toBe(3);
    const source = bufferChunkSource(plaintext);
    const first = await buildUploadPart(source, key, h, 0);
    const second = await buildUploadPart(source, key, h, 1);
    expect(first.byteLength).toBe(8_388_636);
    expect(second.byteLength).toBe(8_388_636);
  });

  it('agrees with the v3 arithmetic at the 512 MiB ceiling', () => {
    const plaintext = 512 * 1024 * 1024;
    expect(totalChunksFor(plaintext, 3)).toBe(65);
    expect(expectedBlobLength(plaintext, 3)).toBe(plaintext + 28 + 28 * 65);
  });
});

describe('envelope header', () => {
  it('round-trips every field', () => {
    const h = header();
    const out = decodeEnvelopeHeader(encodeEnvelopeHeader(h));
    expect(out.version).toBe(2);
    expect(out.flags).toBe(0);
    expect(out.chunkSize).toBe(CHUNK_PLAINTEXT_BYTES);
    expect(out.totalChunks).toBe(3);
    expect(Array.from(out.fileNonce)).toEqual(Array.from(h.fileNonce));
  });

  it('is exactly 28 bytes and starts with the CV magic', () => {
    const bytes = encodeEnvelopeHeader(header());
    expect(bytes.byteLength).toBe(ENVELOPE_HEADER_BYTES);
    expect(bytes[0]).toBe(0x43); // 'C'
    expect(bytes[1]).toBe(0x56); // 'V'
  });

  it('encodes chunkSize and totalChunks big-endian', () => {
    const bytes = encodeEnvelopeHeader(header({ totalChunks: 0x01020304 }));
    expect(Array.from(bytes.slice(8, 12))).toEqual([0x01, 0x02, 0x03, 0x04]);
  });

  it('rejects a wrong magic', () => {
    const bytes = encodeEnvelopeHeader(header());
    bytes[0] = 0x58;
    expect(() => decodeEnvelopeHeader(bytes)).toThrow(UnsupportedAttachmentFormatError);
  });

  it('rejects a non-zero flags byte rather than ignoring it', () => {
    const bytes = encodeEnvelopeHeader(header());
    bytes[3] = 0x01;
    expect(() => decodeEnvelopeHeader(bytes)).toThrow(UnsupportedAttachmentFormatError);
  });

  it('rejects a chunkSize outside the v2 allowlist', () => {
    const bytes = encodeEnvelopeHeader(header({ chunkSize: 1_048_576 }));
    expect(() => decodeEnvelopeHeader(bytes)).toThrow(UnsupportedAttachmentFormatError);
  });

  it('rejects a short buffer without reading past the end', () => {
    expect(() => decodeEnvelopeHeader(new Uint8Array(27))).toThrow(
      UnsupportedAttachmentFormatError
    );
  });

  it('rejects a fileNonce that is not 16 bytes', () => {
    expect(() => encodeEnvelopeHeader(header({ fileNonce: new Uint8Array(8) }))).toThrow(
      UnsupportedAttachmentFormatError
    );
  });

  it('decodes correctly from a non-zero byteOffset view', () => {
    // A blob read off a stream is usually a subarray, not a fresh buffer. A
    // DataView built without honouring byteOffset would silently read the
    // wrong bytes here rather than throwing.
    const padded = new Uint8Array(ENVELOPE_HEADER_BYTES + 9);
    padded.set(encodeEnvelopeHeader(header()), 9);
    const view = padded.subarray(9);
    expect(decodeEnvelopeHeader(view).totalChunks).toBe(3);
  });
});

describe('AAD', () => {
  it('is 34 bytes and starts with the CVA2 tag', () => {
    const aad = buildChunkAad(header(), 0);
    expect(aad.byteLength).toBe(34);
    expect(Array.from(aad.slice(0, 4))).toEqual([0x43, 0x56, 0x41, 0x32]);
  });

  it('LAYOUT LOCK: every field sits at its documented offset', () => {
    // A symmetric offset swap -- moving two fields consistently in encrypt and
    // decrypt -- survives every round-trip test, and would silently orphan every
    // attachment already stored under the old layout. Only a positional
    // assertion freezes a persisted wire format.
    const aad = buildChunkAad(
      header({
        version: 2,
        flags: 0,
        totalChunks: 7,
        chunkSize: 8_388_608,
        fileNonce: nonce(0xab),
      }),
      3
    );
    const view = new DataView(aad.buffer, aad.byteOffset, aad.byteLength);
    expect(Array.from(aad.slice(0, 4))).toEqual([0x43, 0x56, 0x41, 0x32]); // tag @0
    expect(aad[4]).toBe(2); // version @4
    expect(aad[5]).toBe(0); // flags @5
    expect(Array.from(aad.slice(6, 22))).toEqual(Array.from(nonce(0xab))); // fileNonce @6
    expect(view.getUint32(22, false)).toBe(3); // chunkIndex @22
    expect(view.getUint32(26, false)).toBe(7); // totalChunks @26
    expect(view.getUint32(30, false)).toBe(8_388_608); // chunkSize @30
  });

  it('differs for every field that must be bound', () => {
    const base = buildChunkAad(header(), 0);
    const key = (u: Uint8Array) => Array.from(u).join(',');
    expect(key(buildChunkAad(header(), 1))).not.toBe(key(base)); // chunkIndex
    expect(key(buildChunkAad(header({ totalChunks: 4 }), 0))).not.toBe(key(base)); // totalChunks
    expect(key(buildChunkAad(header({ fileNonce: nonce(0xcd) }), 0))).not.toBe(key(base)); // fileNonce
    expect(key(buildChunkAad(header({ version: 3 }), 0))).not.toBe(key(base)); // version
    // chunkSize had NO coverage: deleting its setUint32 passed all 42 tests,
    // because encrypt and decrypt both call this function so the mutation is
    // symmetric and invisible to every round-trip. The rules file calls the AAD
    // normative and says dropping chunkSize re-opens re-framing.
    expect(key(buildChunkAad(header({ chunkSize: 1_048_576 }), 0))).not.toBe(key(base)); // chunkSize
    // flags was outside the AAD entirely until this PR's review.
    expect(key(buildChunkAad(header({ flags: 1 }), 0))).not.toBe(key(base)); // flags
  });

  it('refuses a chunkIndex outside the declared range', () => {
    expect(() => buildChunkAad(header(), 3)).toThrow(UnsupportedAttachmentFormatError);
    expect(() => buildChunkAad(header(), -1)).toThrow(UnsupportedAttachmentFormatError);
  });
});

describe('size arithmetic', () => {
  it('computes chunk counts at the boundaries', () => {
    expect(totalChunksFor(1, 2)).toBe(1);
    expect(totalChunksFor(CHUNK_PLAINTEXT_BYTES, 2)).toBe(1);
    expect(totalChunksFor(CHUNK_PLAINTEXT_BYTES + 1, 2)).toBe(2);
    expect(totalChunksFor(268_435_456, 2)).toBe(32);
  });

  it('matches the normative length identity at the premium ceiling', () => {
    expect(expectedBlobLength(268_435_456, 2)).toBe(268_435_456 + 28 + 28 * 32);
  });

  it('rejects a zero-length plaintext', () => {
    expect(() => totalChunksFor(0, 2)).toThrow(UnsupportedAttachmentFormatError);
  });
});

// ── Task 2: bounded chunk encryption ────────────────────────────────────────

const aesKey = () =>
  crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);

/** Fast large-buffer fill. A per-element `Uint8Array.from` callback over tens of
 *  millions of elements dominates the runtime of every test that uses it. */
const filled = (n: number, seed = 7): Uint8Array => {
  const pattern = Uint8Array.from({ length: 256 }, (_, i) => (i * seed + seed) & 0xff);
  const out = new Uint8Array(n);
  for (let o = 0; o < n; o += 256) out.set(pattern.subarray(0, Math.min(256, n - o)), o);
  return out;
};

describe('buildUploadPart', () => {
  it('prepends the header to part 0 only, and the parts sum to the length identity', async () => {
    const key = await aesKey();
    const pt = filled(CHUNK_PLAINTEXT_BYTES + 100);
    const h = newEnvelopeHeader(pt.byteLength, 2);
    expect(h.totalChunks).toBe(2);

    const src = bufferChunkSource(pt);
    const p0 = await buildUploadPart(src, key, h, 0);
    const p1 = await buildUploadPart(src, key, h, 1);

    expect(p0.byteLength).toBe(
      ENVELOPE_HEADER_BYTES + CHUNK_OVERHEAD_BYTES + CHUNK_PLAINTEXT_BYTES
    );
    expect(p1.byteLength).toBe(CHUNK_OVERHEAD_BYTES + 100);
    expect(p0.byteLength + p1.byteLength).toBe(expectedBlobLength(pt.byteLength, 2));
    expect(decodeEnvelopeHeader(p0).totalChunks).toBe(2);
  });

  it('draws a fresh IV on every call for the same index', async () => {
    // A retry re-encrypts the same index. Deriving the IV from the index would
    // reuse an (key, IV) pair, which is the one failure GCM does not survive.
    const key = await aesKey();
    const pt = filled(1024);
    const h = newEnvelopeHeader(pt.byteLength, 2);
    h.version = 2;
    h.totalChunks = totalChunksFor(pt.byteLength, 2);
    const src = bufferChunkSource(pt);
    const a = await buildUploadPart(src, key, h, 0);
    const b = await buildUploadPart(src, key, h, 0);
    const iv = (p: Uint8Array) =>
      Array.from(p.slice(ENVELOPE_HEADER_BYTES, ENVELOPE_HEADER_BYTES + 12)).join(',');
    expect(iv(a)).not.toBe(iv(b));
  });

  it('produces IVs that are unique across chunks', async () => {
    const key = await aesKey();
    const pt = filled(CHUNK_PLAINTEXT_BYTES * 2 + 1);
    const h = newEnvelopeHeader(pt.byteLength, 2);
    const src = bufferChunkSource(pt);
    const ivs = new Set<string>();
    for (let i = 0; i < h.totalChunks; i++) {
      const part = await buildUploadPart(src, key, h, i);
      const off = i === 0 ? ENVELOPE_HEADER_BYTES : 0;
      ivs.add(Array.from(part.slice(off, off + 12)).join(','));
    }
    expect(ivs.size).toBe(h.totalChunks);
  });

  it('seals each chunk under its own AAD, so chunk 0 cannot open chunk 1', async () => {
    const key = await aesKey();
    const pt = filled(CHUNK_PLAINTEXT_BYTES + 10);
    const h = newEnvelopeHeader(pt.byteLength, 2);
    const p1 = await buildUploadPart(bufferChunkSource(pt), key, h, 1);
    const iv = p1.slice(0, 12);
    const body = p1.slice(12);

    const ok = await crypto.subtle.decrypt(
      { name: 'AES-GCM', iv, additionalData: buildChunkAad(h, 1) },
      key,
      body
    );
    expect(new Uint8Array(ok).byteLength).toBe(10);

    // This is what defeats reorder.
    await expect(
      crypto.subtle.decrypt({ name: 'AES-GCM', iv, additionalData: buildChunkAad(h, 0) }, key, body)
    ).rejects.toThrow();
  });

  it('gives every file a distinct fileNonce', () => {
    const a = newEnvelopeHeader(1024, 2).fileNonce;
    const b = newEnvelopeHeader(1024, 2).fileNonce;
    expect(Array.from(a)).not.toEqual(Array.from(b));
  });

  it('never reads more than one chunk of plaintext at a time', async () => {
    // The whole point of the format: bounded residency regardless of file size.
    const key = await aesKey();
    const pt = filled(CHUNK_PLAINTEXT_BYTES * 3);
    const h = newEnvelopeHeader(pt.byteLength, 2);
    let widest = 0;
    const spy: ChunkSource = {
      byteLength: pt.byteLength,
      async slice(s, e) {
        widest = Math.max(widest, e - s);
        return pt.slice(s, e);
      },
    };
    await buildUploadPart(spy, key, h, 2);
    expect(widest).toBeGreaterThan(0);
    expect(widest).toBeLessThanOrEqual(CHUNK_PLAINTEXT_BYTES);
  });

  it('refuses a chunk index outside the declared range before reading anything', async () => {
    const key = await aesKey();
    const pt = filled(1024);
    const h = newEnvelopeHeader(pt.byteLength, 2);
    let read = false;
    const src: ChunkSource = {
      byteLength: pt.byteLength,
      async slice(s, e) {
        read = true;
        return pt.slice(s, e);
      },
    };
    await expect(buildUploadPart(src, key, h, 1)).rejects.toThrow(UnsupportedAttachmentFormatError);
    expect(read).toBe(false); // guard runs BEFORE the allocating read
  });

  it('fileChunkSource reads lazily through File.slice, not File.arrayBuffer', async () => {
    const bytes = filled(4096);
    const file = new File([bytes], 'a.bin', { type: 'application/octet-stream' });
    Object.defineProperty(file, 'arrayBuffer', {
      value: () => {
        throw new Error('whole-file read on the chunked path');
      },
    });
    const src = fileChunkSource(file);
    expect(src.byteLength).toBe(4096);
    const slice = await src.slice(100, 200);
    expect(slice.byteLength).toBe(100);
    expect(Array.from(slice)).toEqual(Array.from(bytes.slice(100, 200)));
  });
});

// ── Task 3: streaming decrypt + deterministic legacy dispatch ───────────────

const concat = (...parts: Uint8Array[]): Uint8Array<ArrayBuffer> => {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.byteLength, 0));
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.byteLength;
  }
  return out;
};

/** Seal a whole plaintext as a chunked blob, the way the upload session assembles it. */
const sealEnvelope = async (key: CryptoKey, pt: Uint8Array, version: 2 | 3 = 2) => {
  const header = newEnvelopeHeader(pt.byteLength, version);
  const src = bufferChunkSource(pt);
  const parts: Uint8Array[] = [];
  for (let i = 0; i < header.totalChunks; i++)
    parts.push(await buildUploadPart(src, key, header, i));
  return { blob: concat(...parts), header };
};

const bytesOf = async (b: Blob) => new Uint8Array(await b.arrayBuffer());

describe('deterministic dispatch', () => {
  it('routes a v2 blob to v2 and a legacy blob to legacy', async () => {
    const key = await aesKey();
    const pt = filled(4096);
    const { blob } = await sealEnvelope(key, pt, 2);
    expect(classifyEnvelope(blob.subarray(0, ENVELOPE_HEADER_BYTES), blob.byteLength).kind).toBe(
      'v2'
    );

    const legacy = new Uint8Array(await encryptFile(pt.slice().buffer, key));
    expect(
      classifyEnvelope(legacy.subarray(0, ENVELOPE_HEADER_BYTES), legacy.byteLength).kind
    ).toBe('legacy');
  });

  it('routes to legacy when the chunk-count arithmetic does not hold', async () => {
    // A truncation large enough to change the implied chunk count is
    // arithmetically visible, so the blob stops being recognisable as ours.
    const key = await aesKey();
    const { blob } = await sealEnvelope(key, filled(CHUNK_PLAINTEXT_BYTES * 2));
    const gutted = blob.subarray(0, ENVELOPE_HEADER_BYTES + 1024);
    expect(
      classifyEnvelope(gutted.subarray(0, ENVELOPE_HEADER_BYTES), gutted.byteLength).kind
    ).toBe('legacy');
  }, 30000);

  it('a one-byte truncation is NOT arithmetically visible, so it fails closed on the tag', async () => {
    // ceil(4095 / 8 MiB) and ceil(4096 / 8 MiB) are both 1, so no size check can
    // see this. GCM is the backstop, and that is the correct division of labour:
    // arithmetic decides WHICH parser runs, the tag decides whether it is valid.
    const key = await aesKey();
    const { blob } = await sealEnvelope(key, filled(4096));
    const truncated = blob.subarray(0, blob.byteLength - 1);
    expect(
      classifyEnvelope(truncated.subarray(0, ENVELOPE_HEADER_BYTES), truncated.byteLength).kind
    ).toBe('v2');
    await expect(decryptAttachmentBlob(truncated, key, 'application/pdf')).rejects.toThrow(
      AttachmentIntegrityError
    );
  });

  it('round-trips a v2 blob across a chunk boundary', async () => {
    const key = await aesKey();
    const pt = filled(CHUNK_PLAINTEXT_BYTES + 12345);
    const { blob } = await sealEnvelope(key, pt, 2);
    const out = await bytesOf(await decryptAttachmentBlob(blob, key, 'application/pdf'));
    expect(out.byteLength).toBe(pt.byteLength);
    expect(Array.from(out.slice(0, 64))).toEqual(Array.from(pt.slice(0, 64)));
    expect(Array.from(out.slice(-64))).toEqual(Array.from(pt.slice(-64)));
  }, 20000);

  it('round-trips a v3 blob across the chunk-zero boundary and rejects a version rewrite', async () => {
    const key = await aesKey();
    const pt = filled(CHUNK_PLAINTEXT_BYTES + 1);
    const { blob } = await sealEnvelope(key, pt, 3);
    const out = await bytesOf(await decryptAttachmentBlob(blob, key, 'application/pdf'));
    expect(out.byteLength).toBe(pt.byteLength);
    expect(Array.from(out.slice(0, 64))).toEqual(Array.from(pt.slice(0, 64)));
    expect(Array.from(out.slice(-64))).toEqual(Array.from(pt.slice(-64)));

    const tampered = blob.slice();
    tampered[2] = 2;
    await expect(decryptAttachmentBlob(tampered, key, 'application/pdf')).rejects.toThrow(
      AttachmentIntegrityError
    );
  }, 20000);

  it('round-trips a single-chunk v2 blob', async () => {
    const key = await aesKey();
    const pt = filled(1000);
    const { blob } = await sealEnvelope(key, pt, 2);
    const out = await bytesOf(await decryptAttachmentBlob(blob, key, 'image/png'));
    expect(Array.from(out)).toEqual(Array.from(pt));
  });

  it('still decrypts a legacy blob, and carries the mime type onto the Blob', async () => {
    const key = await aesKey();
    const pt = filled(4096);
    const legacy = new Uint8Array(await encryptFile(pt.slice().buffer, key));
    const blob = await decryptAttachmentBlob(legacy, key, 'image/png');
    expect(blob.type).toBe('image/png');
    expect(Array.from(await bytesOf(blob))).toEqual(Array.from(pt));
  });
});

describe('the AAD negative matrix — one case per bound field', () => {
  it('rejects reordered chunks', async () => {
    const key = await aesKey();
    const { blob, header } = await sealEnvelope(key, filled(CHUNK_PLAINTEXT_BYTES * 2));
    const c0 = ENVELOPE_HEADER_BYTES;
    const c1 = c0 + 12 + header.chunkSize + 16;
    const swapped = concat(blob.subarray(0, c0), blob.subarray(c1), blob.subarray(c0, c1));
    await expect(decryptAttachmentBlob(swapped, key, 'application/pdf')).rejects.toThrow(
      AttachmentIntegrityError
    );
  }, 30000);

  it('rejects truncation even when totalChunks is rewritten to match', async () => {
    const key = await aesKey();
    const { blob, header } = await sealEnvelope(key, filled(CHUNK_PLAINTEXT_BYTES * 2));
    const keep = ENVELOPE_HEADER_BYTES + 12 + header.chunkSize + 16;
    const cut = blob.slice(0, keep);
    new DataView(cut.buffer).setUint32(8, 1, false); // totalChunks 2 -> 1
    // Structurally self-consistent now, so it dispatches to v2 -- and chunk 0's
    // tag fails, because it was sealed under totalChunks = 2.
    expect(classifyEnvelope(cut.subarray(0, ENVELOPE_HEADER_BYTES), cut.byteLength).kind).toBe(
      'v2'
    );
    await expect(decryptAttachmentBlob(cut, key, 'application/pdf')).rejects.toThrow(
      AttachmentIntegrityError
    );
  }, 30000);

  it('rejects a cross-file splice', async () => {
    const key = await aesKey();
    const a = await sealEnvelope(key, filled(CHUNK_PLAINTEXT_BYTES * 2, 3));
    const b = await sealEnvelope(key, filled(CHUNK_PLAINTEXT_BYTES * 2, 9));
    const c1 = ENVELOPE_HEADER_BYTES + 12 + a.header.chunkSize + 16;
    const spliced = concat(a.blob.subarray(0, c1), b.blob.subarray(c1));
    await expect(decryptAttachmentBlob(spliced, key, 'application/pdf')).rejects.toThrow(
      AttachmentIntegrityError
    );
  }, 30000);

  it('rejects a tampered version byte as an integrity failure', async () => {
    const key = await aesKey();
    const { blob } = await sealEnvelope(key, filled(4096));
    const t = blob.slice();
    t[2] = 3;
    await expect(decryptAttachmentBlob(t, key, 'application/pdf')).rejects.toThrow(
      AttachmentIntegrityError
    );
  });

  it('rejects a tampered flags byte', async () => {
    const key = await aesKey();
    const { blob } = await sealEnvelope(key, filled(4096));
    const t = blob.slice();
    t[3] = 0x80;
    await expect(decryptAttachmentBlob(t, key, 'application/pdf')).rejects.toThrow(
      UnsupportedAttachmentFormatError
    );
  });

  it('rejects a tampered fileNonce', async () => {
    const key = await aesKey();
    const { blob } = await sealEnvelope(key, filled(4096));
    const t = blob.slice();
    t[12] ^= 0xff;
    await expect(decryptAttachmentBlob(t, key, 'application/pdf')).rejects.toThrow(
      AttachmentIntegrityError
    );
  });

  it('rejects a tampered chunkSize as unsupported', async () => {
    const key = await aesKey();
    const { blob } = await sealEnvelope(key, filled(4096));
    const t = blob.slice();
    new DataView(t.buffer).setUint32(4, 1_048_576, false);
    await expect(decryptAttachmentBlob(t, key, 'application/pdf')).rejects.toThrow(
      UnsupportedAttachmentFormatError
    );
  });

  it('names the failing chunk without leaking plaintext', async () => {
    const key = await aesKey();
    const { blob } = await sealEnvelope(key, filled(4096));
    const t = blob.slice();
    t[t.byteLength - 1] ^= 0xff; // corrupt the tag
    await expect(decryptAttachmentBlob(t, key, 'application/pdf')).rejects.toThrow(
      /chunk 0 of 1 failed authentication/
    );
  });
});

describe('magic collision — the case the magic byte cannot save us from', () => {
  // Legacy blobs open with a random 12-byte IV, so about 1 in 65,536 of them
  // begins with the "CV" magic by chance. The magic check is no help there; the
  // chunk-count arithmetic is what keeps them on the legacy path. These two
  // cases pin that, deterministically rather than relying on a random IV.
  const craft = (totalChunks: number, chunkSize: number, totalLength: number) => {
    const head = new Uint8Array(ENVELOPE_HEADER_BYTES);
    head[0] = 0x43; // 'C'
    head[1] = 0x56; // 'V'
    head[2] = 2;
    head[3] = 0;
    const v = new DataView(head.buffer);
    v.setUint32(4, chunkSize, false);
    v.setUint32(8, totalChunks, false);
    return classifyEnvelope(head, totalLength);
  };

  it('routes a CV-prefixed blob to legacy when the arithmetic does not hold', () => {
    // A plausible legacy length, with header bytes that happen to spell CV.
    expect(craft(0x40302010, CHUNK_PLAINTEXT_BYTES, 4124).kind).toBe('legacy');
  });

  it('routes to v2 only when the arithmetic holds — the arithmetic is the discriminator', () => {
    const plaintext = 4096;
    const total = ENVELOPE_HEADER_BYTES + CHUNK_OVERHEAD_BYTES * 1 + plaintext;
    expect(craft(1, CHUNK_PLAINTEXT_BYTES, total).kind).toBe('v2');
    // Same bytes, one more declared chunk than the length can support.
    expect(craft(2, CHUNK_PLAINTEXT_BYTES, total).kind).toBe('legacy');
  });
});

const AES_GCM_IV_LENGTH = 12;

describe('decryptAttachmentBlob — bounded plaintext residency', () => {
  // NOTE ON WHAT IS AND IS NOT ASSERTED HERE. The residency improvement is
  // handing each authenticated chunk to the blob store as its own Blob so the JS
  // heap holds one chunk of plaintext rather than all of them. That is a
  // CHROMIUM property: jsdom's Blob keeps every part in the JS heap and never
  // spills, so this environment cannot demonstrate it -- an early draft of these
  // tests OOM'd the worker trying. What is asserted is the observable behaviour:
  // the early exit fires, it is opt-in, and it does not become an authentication
  // bypass. Chunk sizes are kept to the minimum that exercises the branch,
  // because every extra chunk is 8 MiB of real AES in a jsdom worker.

  it('stops decrypting once maxPlaintextBytes is exceeded', async () => {
    // The markdown overflow path renders at most a few hundred KiB of a file
    // that may be 256 MiB. Decrypting the rest produces bytes the caller is
    // about to discard.
    const key = await aesKey();
    const { blob } = await sealEnvelope(key, filled(CHUNK_PLAINTEXT_BYTES + 4096));

    const capped = await decryptAttachmentBlob(blob, key, 'text/markdown', 1);

    // One chunk decrypted, then the loop exits: a PREFIX, not the whole file.
    expect(capped.size).toBe(CHUNK_PLAINTEXT_BYTES);
    expect(capped.size).toBeLessThan(CHUNK_PLAINTEXT_BYTES + 4096);
  }, 30000);

  it('does not stop AT the cap — a prefix equal to the cap would read as complete', async () => {
    // `>` vs `>=` matters and was untested: the existing case caps at 1, where
    // both operators behave identically. Under `>=`, a document whose plaintext
    // crosses the cap exactly at a chunk boundary returns a prefix of EXACTLY
    // the cap -- and OverflowMarkdownAttachment gates on `blob.size > MAX`, so a
    // TRUNCATED document renders as complete. The library's `>` and the caller's
    // `>` are a matched pair and nothing tested the pairing.
    const key = await aesKey();
    const total = CHUNK_PLAINTEXT_BYTES + 4096;
    const { blob } = await sealEnvelope(key, filled(total));

    const capped = await decryptAttachmentBlob(blob, key, 'text/markdown', CHUNK_PLAINTEXT_BYTES);

    expect(capped.size).toBeGreaterThan(CHUNK_PLAINTEXT_BYTES);
  }, 30000);

  it('decrypts everything when no cap is supplied', async () => {
    // The cap must be opt-in: the download path has no render ceiling and must
    // still receive the complete file.
    //
    // MULTI-CHUNK ON PURPOSE. With a single-chunk file, "no cap" and "a cap of
    // zero" produce the identical Blob -- the loop returns the whole file either
    // way -- so a one-chunk payload cannot tell an absent cap from a wrongly
    // applied one. Falsification caught exactly that.
    const key = await aesKey();
    const total = CHUNK_PLAINTEXT_BYTES + 4096;
    const { blob } = await sealEnvelope(key, filled(total));

    const whole = await decryptAttachmentBlob(blob, key, 'application/octet-stream');

    expect(whole.size).toBe(total);
  }, 30000);

  it('still authenticates the chunks it does decrypt under a cap', async () => {
    // The early exit must not become an authentication bypass: a chunk BEFORE
    // the cap is still GCM-verified, so tampering fails even when the caller
    // only wants a prefix.
    const key = await aesKey();
    const { blob } = await sealEnvelope(key, filled(4096));
    blob[ENVELOPE_HEADER_BYTES + AES_GCM_IV_LENGTH + 5] ^= 0xff;

    await expect(decryptAttachmentBlob(blob, key, 'text/markdown', 1)).rejects.toThrow(
      AttachmentIntegrityError
    );
  });
});

describe('what a decrypt failure is ALLOWED to accuse the file of', () => {
  it('calls a damaged v2 blob an integrity failure, not a retryable load error', async () => {
    const key = await aesKey();
    // Two chunks, so a truncation can break the length arithmetic. A ONE-chunk
    // blob cannot: any truncation that leaves a body still rounds to 1 chunk,
    // so it stays v2 and already fails as an integrity error.
    const { blob } = await sealEnvelope(key, filled(CHUNK_PLAINTEXT_BYTES + 1));

    // The arithmetic IS the discriminator, so a damaged v2 blob does not fail
    // as v2 -- it falls through to the LEGACY path, where the legacy AAD
    // (empty) can never authenticate a v2 chunk. POSITIVE CONTROL for that
    // claim first: the assertion below proves nothing if this says 'v2'.
    const damaged = blob.subarray(0, blob.byteLength - CHUNK_PLAINTEXT_BYTES);
    expect(
      classifyEnvelope(damaged.subarray(0, ENVELOPE_HEADER_BYTES), damaged.byteLength).kind
    ).toBe('legacy');

    await expect(decryptAttachmentBlob(damaged, key, 'image/png')).rejects.toBeInstanceOf(
      AttachmentIntegrityError
    );
  });

  it('calls a wrong-key legacy blob an integrity failure too', async () => {
    // Terminal for the same reason: retrying re-fetches identical bytes.
    const legacy = new Uint8Array(await encryptFile(filled(64).slice().buffer, await aesKey()));
    await expect(decryptAttachmentBlob(legacy, await aesKey(), 'image/png')).rejects.toBeInstanceOf(
      AttachmentIntegrityError
    );
  });

  it('does NOT call a key fault tampering — it rethrows it untouched', async () => {
    // A CryptoKey without the `decrypt` usage rejects with InvalidAccessError,
    // which says nothing whatsoever about the ciphertext. Blaming the file for
    // it sends the user chasing a corrupt attachment over our own bug.
    const encryptOnly = await crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, true, [
      'encrypt',
    ]);
    const good = await aesKey();
    const { blob } = await sealEnvelope(good, filled(64));

    const err = await decryptAttachmentBlob(blob, encryptOnly, 'image/png').then(
      () => null,
      (e: unknown) => e
    );
    expect(err).not.toBeInstanceOf(AttachmentIntegrityError);
    expect((err as DOMException).name).toBe('InvalidAccessError');
  });

  it('keeps the underlying rejection as `cause` on the integrity error', async () => {
    const { blob } = await sealEnvelope(await aesKey(), filled(64));
    const err = await decryptAttachmentBlob(blob, await aesKey(), 'image/png').then(
      () => null,
      (e: unknown) => e
    );
    expect(err).toBeInstanceOf(AttachmentIntegrityError);
    // Asserted by name, not `instanceof DOMException`: jsdom's DOMException
    // global is a different realm from the one WebCrypto throws, so the
    // instanceof form is false here for a genuine OperationError. That is the
    // same trap isAuthenticationFailure has to avoid in production.
    expect((err as Error).cause).toMatchObject({ name: 'OperationError' });
  });
});

describe('parseKeyVersionHeader', () => {
  it('reads a decimal epoch', () => {
    expect(parseKeyVersionHeader('7')).toBe(7);
    expect(parseKeyVersionHeader('1')).toBe(1);
  });

  it('treats an ABSENT header as no epoch on record', () => {
    // The case this covers is a ROLLING DEPLOY -- an older control-plane
    // instance serving the download without the header. It is NOT "rows
    // predating the attestation": migration 000042's valid_media_context CHECK
    // makes key_version NOT NULL for media_tier = 2, the only tier this
    // download path selects, so no such row can exist.
    expect(parseKeyVersionHeader(null)).toBeNull();
  });

  it.each(['abc', '1.5', '', '1e3', ' 7', '7 ', '+7', '0x7', '7,8'])(
    'throws on the mangled epoch %j rather than falling back',
    (raw) => {
      // Falling back would decrypt with the wrong CSK and report the GCM
      // failure as tampering. `Number()` alone accepts "1e3" as 1000 and " 7"
      // as 7 -- forms strconv.Itoa never emits.
      expect(() => parseKeyVersionHeader(raw)).toThrow(AttachmentKeyEpochError);
    }
  );

  it.each(['0', '9007199254740993'])('throws on the out-of-range epoch %j', (raw) => {
    // There is no epoch 0 in the schema, and past 2^53 the parse stops being
    // exact -- an epoch that cannot be compared reliably must not select a key.
    expect(() => parseKeyVersionHeader(raw)).toThrow(AttachmentKeyEpochError);
  });
});
