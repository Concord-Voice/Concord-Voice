/**
 * ByteReader bounds checks and iteration budget (#2469).
 *
 * These two mechanisms are what turn a malformed attachment into a thrown error
 * at a known point instead of an out-of-range read or a non-terminating walk.
 */

import { describe, it, expect } from 'vitest';
import {
  ByteReader,
  ImageParseError,
  concat,
  startsWith,
  tag,
} from '@/renderer/utils/imageMetadata/reader';
import { stripGif } from '@/renderer/utils/imageMetadata/gif';
import { buildGifWithSubBlockChain, cat, asc } from './fixtures';

const reader = (values: number[], budget?: number): ByteReader =>
  budget === undefined
    ? new ByteReader(new Uint8Array(values))
    : new ByteReader(new Uint8Array(values), budget);

describe('ByteReader reads', () => {
  it('reads unsigned integers in both byte orders', () => {
    const r = reader([0x01, 0x02, 0x03, 0x04, 0x05, 0x06]);
    expect(r.u16be()).toBe(0x0102);
    expect(r.u16le()).toBe(0x0403);
    expect(r.u8()).toBe(0x05);
    expect(r.u8()).toBe(0x06);
    expect(r.eof()).toBe(true);
  });

  it('reads 32-bit values in both byte orders', () => {
    expect(reader([0xde, 0xad, 0xbe, 0xef]).u32be()).toBe(0xdeadbeef);
    expect(reader([0xde, 0xad, 0xbe, 0xef]).u32le()).toBe(0xefbeadde);
  });

  it('tracks position and remaining', () => {
    const r = reader([1, 2, 3, 4]);
    expect(r.length).toBe(4);
    expect(r.remaining()).toBe(4);
    r.u16be();
    expect(r.position).toBe(2);
    expect(r.remaining()).toBe(2);
    expect(r.eof()).toBe(false);
  });

  it('copies bytes so callers cannot alias the source', () => {
    const source = new Uint8Array([1, 2, 3, 4]);
    const r = new ByteReader(source);
    const copy = r.bytesOf(4);
    copy[0] = 0xff;
    expect(source[0]).toBe(1);
  });

  it('reads ASCII container tags', () => {
    expect(reader(asc('ftypheic')).ascii(4)).toBe('ftyp');
  });
});

describe('ByteReader bounds violations', () => {
  it('throws when a read runs past the end', () => {
    const r = reader([1, 2, 3]);
    expect(() => r.u32be()).toThrow(ImageParseError);
  });

  it('throws when a read runs past the end after partial consumption', () => {
    const r = reader([1, 2, 3, 4]);
    r.u16be();
    expect(() => r.u32be()).toThrow(/exceeds 4 bytes/);
  });

  it('throws on a negative seek', () => {
    expect(() => reader([1, 2, 3]).seek(-1)).toThrow(/seek out of range/);
  });

  it('throws on a seek past the end', () => {
    expect(() => reader([1, 2, 3]).seek(4)).toThrow(/seek out of range/);
  });

  it('allows a seek to exactly the end', () => {
    const r = reader([1, 2, 3]);
    r.seek(3);
    expect(r.eof()).toBe(true);
  });

  it('throws when skip runs past the end', () => {
    const r = reader([1, 2, 3]);
    expect(() => r.skip(4)).toThrow(ImageParseError);
  });

  it('throws when skip runs before the start', () => {
    const r = reader([1, 2, 3]);
    expect(() => r.skip(-1)).toThrow(ImageParseError);
  });

  it('throws when peek runs past the end', () => {
    expect(() => reader([1, 2, 3]).peek(4)).toThrow(/peek of 4/);
  });

  it('throws when peek is given a negative offset', () => {
    expect(() => reader([1, 2, 3]).peek(1, -1)).toThrow(ImageParseError);
  });

  it('throws when ascii runs past the end', () => {
    expect(() => reader(asc('ftyp')).ascii(8)).toThrow(ImageParseError);
  });

  it('throws when a 64-bit size exceeds the safe integer range', () => {
    const r = reader([0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff]);
    expect(() => r.u64be()).toThrow(/safe integer range/);
  });

  it('reads a 64-bit size that is within the safe integer range', () => {
    expect(reader([0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00]).u64be()).toBe(0x1_0000_0000);
  });

  it('reads correctly through a non-zero byteOffset view', () => {
    const backing = new Uint8Array([0xaa, 0xbb, 0x01, 0x02]);
    const r = new ByteReader(backing.subarray(2));
    expect(r.length).toBe(2);
    expect(r.u16be()).toBe(0x0102);
  });
});

describe('ByteReader iteration budget', () => {
  it('permits exactly `budget` steps', () => {
    const r = reader([0], 3);
    r.step('thing');
    r.step('thing');
    r.step('thing');
    expect(() => r.step('thing')).toThrow(/too many thing elements \(budget 3\)/);
  });

  it('trips on a crafted sub-block chain longer than the default budget', () => {
    const crafted = buildGifWithSubBlockChain(100_001);

    expect(() => stripGif(crafted)).toThrow(ImageParseError);
    expect(() => stripGif(crafted)).toThrow(/budget 100000/);
  });

  it('does not trip on a chain well inside the budget', () => {
    const benign = buildGifWithSubBlockChain(100);

    const result = stripGif(benign);

    expect(result.stripped).toBe(true);
  });
});

describe('reader helpers', () => {
  it('startsWith matches a prefix at an offset', () => {
    const bytes = new Uint8Array(cat(asc('RIFF'), [0, 0, 0, 0], asc('WEBP')));
    expect(startsWith(bytes, tag('RIFF'))).toBe(true);
    expect(startsWith(bytes, tag('WEBP'), 8)).toBe(true);
    expect(startsWith(bytes, tag('WEBP'), 9)).toBe(false);
  });

  it('startsWith returns false rather than throwing past the end', () => {
    expect(startsWith(new Uint8Array([0x01]), [0x01, 0x02])).toBe(false);
  });

  it('tag converts a FourCC to bytes', () => {
    expect(tag('ftyp')).toEqual([0x66, 0x74, 0x79, 0x70]);
  });

  it('concat joins parts in order', () => {
    const joined = concat([new Uint8Array([1, 2]), new Uint8Array([]), new Uint8Array([3])]);
    expect(Array.from(new Uint8Array(joined))).toEqual([1, 2, 3]);
  });
});
