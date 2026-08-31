/**
 * ISO base media file format walker, and HEIC/HEIF metadata strip (#2469).
 *
 * This is written as a GENERAL box walker rather than a HEIC special case,
 * because HEIC/HEIF and MP4/MOV are the same container. PR 2 (video) reaches
 * `moov/udta/©xyz` — the QuickTime GPS tag — through a different box path in
 * this same code, not a second parser. That is the whole reason the format was
 * worth the extra work over a canvas round trip.
 *
 * The strip is deliberately OFFSET-STABLE. A HEIC's `iloc` box holds absolute
 * file offsets for every item's bytes, so removing or resizing any box would
 * require rewriting every offset that follows — a large, error-prone rewrite of
 * a container we do not otherwise need to understand. Instead:
 *
 *   1. zero the EXIF/XMP item's payload bytes in place, and
 *   2. set that item's `iloc` extent length to 0.
 *
 * Both edits are same-size, so no box changes length and no offset shifts. The
 * metadata bytes are unrecoverable, which is the privacy goal, and the failure
 * surface is a fraction of a full rebuild.
 */

import { ByteReader, ImageParseError } from './reader';

export interface Box {
  type: string;
  /** Offset of the box header. */
  start: number;
  /** Offset of the first payload byte. */
  payloadStart: number;
  /** Offset one past the last payload byte. */
  payloadEnd: number;
}

const CONTAINERS = new Set([
  'meta',
  'iinf',
  'iprp',
  'moov',
  'udta',
  'trak',
  'mdia',
  'minf',
  'stbl',
]);

/** Boxes that carry a 4-byte version+flags header before their contents. */
const FULL_BOXES = new Set(['meta', 'iinf', 'infe', 'iloc', 'iref', 'hdlr', 'pitm']);

/**
 * Walks the boxes directly inside [from, to), calling `visit` for each.
 * Does not recurse — callers descend deliberately, which keeps the traversal
 * explicit and bounded.
 */
export function walkBoxes(
  r: ByteReader,
  from: number,
  to: number,
  visit: (box: Box) => void
): void {
  let at = from;
  while (at + 8 <= to) {
    r.step('BMFF box');
    r.seek(at);
    let size = r.u32be();
    const type = r.ascii(4);
    let payloadStart = at + 8;

    if (size === 1) {
      size = r.u64be(); // extended 64-bit size
      payloadStart = at + 16;
    } else if (size === 0) {
      size = to - at; // extends to the end of the enclosing range
    }

    if (size < 8 || at + size > to) {
      throw new ImageParseError(`box ${type} at ${at} claims size ${size}, overruns ${to}`);
    }

    visit({ type, start: at, payloadStart, payloadEnd: at + size });
    at += size;
  }
}

/** Depth-first search for the first box matching `path`, e.g. ['meta', 'iloc']. */
export function findBox(
  r: ByteReader,
  path: readonly string[],
  from = 0,
  to = r.length
): Box | null {
  if (path.length === 0) {
    return null;
  }
  const [head, ...rest] = path;
  let found: Box | null = null;

  walkBoxes(r, from, to, (box) => {
    if (found || box.type !== head) {
      return;
    }
    if (rest.length === 0) {
      found = box;
      return;
    }
    // A FullBox has 4 bytes of version+flags before its children.
    const childrenFrom = FULL_BOXES.has(box.type) ? box.payloadStart + 4 : box.payloadStart;
    found = findBox(r, rest, childrenFrom, box.payloadEnd);
  });

  return found;
}

export function isContainer(type: string): boolean {
  return CONTAINERS.has(type);
}

interface MetadataItem {
  id: number;
  /** Offset of this item's extent-length field inside iloc. Always rewritable —
   *  an unrewritable width is rejected at parse time rather than carried here. */
  lengthFieldAt: number;
  lengthFieldSize: number;
  dataStart: number;
  dataLength: number;
}

/**
 * Reads `iinf` to find items whose type marks them as metadata, then resolves
 * each in `iloc` to a byte range and a rewritable length field.
 */
/** Reads `iinf` and returns the item IDs whose type marks them as metadata. */
function readMetadataItemIds(r: ByteReader, iinf: Box): Set<number> {
  const ids = new Set<number>();
  r.seek(iinf.payloadStart);
  const iinfVersion = r.u8();
  r.skip(3); // flags

  // The declared entry count is read for its bounds check and discarded — the
  // box walk below is authoritative about how many infe entries actually exist,
  // and trusting a declared count over the structure is how a crafted file gets
  // a parser to read past the end of a box.
  if (iinfVersion === 0) {
    r.u16be();
  } else {
    r.u32be();
  }

  walkBoxes(r, r.position, iinf.payloadEnd, (infe) => {
    if (infe.type !== 'infe') {
      return;
    }
    r.seek(infe.payloadStart);
    const version = r.u8();
    r.skip(3);
    if (version < 2) {
      return; // versions 0/1 predate item_type; not seen in HEIC
    }
    const itemId = version === 2 ? r.u16be() : r.u32be();
    r.skip(2); // protection index
    const itemType = r.ascii(4);
    if (itemType === 'Exif' || itemType === 'mime') {
      // 'mime' items are XMP in practice; both are metadata, neither is pixels.
      ids.add(itemId);
    }
  });

  return ids;
}

interface IlocHeader {
  version: number;
  offsetSize: number;
  lengthSize: number;
  baseOffsetSize: number;
  indexSize: number;
  itemCount: number;
}

function readIlocHeader(r: ByteReader, iloc: Box): IlocHeader {
  r.seek(iloc.payloadStart);
  const version = r.u8();
  r.skip(3); // flags
  const sizes = r.u8();
  const sizes2 = r.u8();
  return {
    version,
    offsetSize: sizes >> 4,
    lengthSize: sizes & 0x0f,
    baseOffsetSize: sizes2 >> 4,
    indexSize: version >= 1 ? sizes2 & 0x0f : 0,
    itemCount: version < 2 ? r.u16be() : r.u32be(),
  };
}

/** Reads one extent, returning it only when the item is metadata. */
function readOneExtent(
  r: ByteReader,
  h: IlocHeader,
  itemId: number,
  baseOffset: number,
  isMetadata: boolean,
  readSized: (n: number) => number
): MetadataItem | null {
  if (h.indexSize > 0) {
    readSized(h.indexSize);
  }
  const extentOffset = readSized(h.offsetSize);
  const lengthFieldAt = r.position;
  const extentLength = readSized(h.lengthSize);

  if (!isMetadata) {
    return null;
  }

  // An iloc with length_size = 0 stores no extent length, so the span of the
  // item's bytes is unknown to us. Zeroing a zero-length range removes nothing
  // while every downstream signal says the strip succeeded — the worst possible
  // outcome for a privacy control, and strictly worse than refusing. There is no
  // safe amount to zero without a length, so fail closed and let the caller
  // reject the upload.
  if (h.lengthSize !== 4 && h.lengthSize !== 8) {
    throw new ImageParseError(
      `HEIC iloc uses length_size=${h.lengthSize}; cannot determine the extent of metadata item ${itemId}`
    );
  }

  return {
    id: itemId,
    lengthFieldAt,
    lengthFieldSize: h.lengthSize,
    dataStart: baseOffset + extentOffset,
    dataLength: extentLength,
  };
}

/** Resolves the metadata items to byte ranges and rewritable length fields. */
function readIlocExtents(r: ByteReader, iloc: Box, metadataIds: Set<number>): MetadataItem[] {
  const h = readIlocHeader(r, iloc);
  const items: MetadataItem[] = [];

  const readSized = (n: number): number => {
    if (n === 0) return 0;
    if (n === 4) return r.u32be();
    if (n === 8) return r.u64be();
    throw new ImageParseError(`unsupported iloc field width ${n}`);
  };

  for (let i = 0; i < h.itemCount; i++) {
    r.step('iloc entry');
    const itemId = h.version < 2 ? r.u16be() : r.u32be();
    // 12 reserved bits then a 4-bit construction_method, present from version 1.
    // Version 0 has no such field and is file-offset by definition.
    let constructionMethod = 0;
    if (h.version >= 1) {
      constructionMethod = r.u16be() & 0x0f;
    }
    r.skip(2); // data reference index
    const baseOffset = readSized(h.baseOffsetSize);
    const extentCount = r.u16be();
    const isMetadata = metadataIds.has(itemId);

    // construction_method decides what the extent offset is RELATIVE TO:
    //   0  file offset      — absolute, which is what this code resolves
    //   1  idat offset      — relative to the idat box
    //   2  item offset      — relative to another item
    //
    // Treating 1 or 2 as absolute zeroes an unrelated byte range (potentially
    // corrupting pixel data) while the real EXIF survives — and then reports
    // stripped: true. That is the same "destroy the wrong bytes and claim
    // success" shape as an unknown length_size, so it gets the same answer.
    if (isMetadata && constructionMethod !== 0) {
      throw new ImageParseError(
        `HEIC metadata item ${itemId} uses construction_method=${constructionMethod}; cannot resolve an absolute extent`
      );
    }

    for (let e = 0; e < extentCount; e++) {
      r.step('iloc extent');
      const item = readOneExtent(r, h, itemId, baseOffset, isMetadata, readSized);
      if (item) {
        items.push(item);
      }
    }
  }

  return items;
}

/**
 * Reads `iinf` to find items whose type marks them as metadata, then resolves
 * each in `iloc` to a byte range and a rewritable length field.
 */
function findMetadataItems(r: ByteReader, meta: Box): MetadataItem[] {
  const metaChildren = meta.payloadStart + 4; // meta is a FullBox
  const iinf = findBox(r, ['iinf'], metaChildren, meta.payloadEnd);
  const iloc = findBox(r, ['iloc'], metaChildren, meta.payloadEnd);
  if (!iinf || !iloc) {
    return [];
  }

  const metadataIds = readMetadataItemIds(r, iinf);
  if (metadataIds.size === 0) {
    return [];
  }

  return readIlocExtents(r, iloc, metadataIds);
}

export function stripHeic(bytes: Uint8Array): { data: ArrayBuffer; stripped: boolean } {
  const out = bytes.slice(); // edit a copy in place; offsets stay valid
  const r = new ByteReader(out);

  const meta = findBox(r, ['meta']);
  if (!meta) {
    // No meta box means no item infrastructure and therefore no EXIF item.
    return { data: out.buffer, stripped: false };
  }

  const items = findMetadataItems(r, meta);
  if (items.length === 0) {
    return { data: out.buffer, stripped: false };
  }

  const view = new DataView(out.buffer);
  let stripped = false;

  for (const item of items) {
    const end = item.dataStart + item.dataLength;
    if (item.dataStart < 0 || end > out.byteLength) {
      throw new ImageParseError(
        `item ${item.id} extent ${item.dataStart}+${item.dataLength} overruns ${out.byteLength}`
      );
    }

    // 1. Destroy the bytes. This is the part that actually removes the GPS.
    out.fill(0, item.dataStart, end);

    // 2. Tell the container the item is empty, so a decoder does not try to
    //    parse a zeroed EXIF block. Same-size write, no offsets move.
    view.setUint32(item.lengthFieldAt, 0, false);
    if (item.lengthFieldSize === 8) {
      view.setUint32(item.lengthFieldAt + 4, 0, false);
    }

    // A zero-length extent would mean nothing was zeroed above, and reporting a
    // successful strip in that case is how a caller ends up trusting a file that
    // still carries GPS. Rejected at parse time; asserted here so a future edit
    // to the parse path cannot quietly reintroduce it.
    if (item.dataLength === 0) {
      throw new ImageParseError(`HEIC metadata item ${item.id} has a zero-length extent`);
    }
    stripped = true;
  }

  return { data: out.buffer, stripped };
}
