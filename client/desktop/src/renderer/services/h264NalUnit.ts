/**
 * Byte-structure helpers for untrusted H.264 Annex-B access units.
 * Malformed input returns `null`; no helper performs cryptography.
 */

const MAX_NAL_UNITS = 256;
const MAX_UE_LEADING_ZERO_BITS = 31;

export type H264NalKind = 'clear' | 'opaque' | 'slice';

export interface ParsedH264NalUnit {
  startCodeOffset: number;
  startCodeLength: 3 | 4;
  nalOffset: number;
  nalLength: number;
  nalType: number;
  kind: H264NalKind;
}

function startCodeLengthAt(data: Uint8Array, offset: number): 0 | 3 | 4 {
  if (
    offset + 3 < data.length &&
    data[offset] === 0x00 &&
    data[offset + 1] === 0x00 &&
    data[offset + 2] === 0x00 &&
    data[offset + 3] === 0x01
  ) {
    return 4;
  }
  if (
    offset + 2 < data.length &&
    data[offset] === 0x00 &&
    data[offset + 1] === 0x00 &&
    data[offset + 2] === 0x01
  ) {
    return 3;
  }
  return 0;
}

function classifyNalType(nalType: number): H264NalKind | null {
  switch (nalType) {
    case 1:
    case 5:
      return 'slice';
    case 6:
    case 12:
      return 'opaque';
    case 7:
    case 8:
    case 9:
    case 10:
    case 11:
    case 13:
    case 15:
      return 'clear';
    default:
      return null;
  }
}

interface ParsedH264NalSpan {
  unit: ParsedH264NalUnit;
  nextStartCodeOffset: number;
}

function findNextStartCodeOffset(data: Uint8Array, offset: number): number {
  let nextOffset = offset;
  while (nextOffset < data.length && startCodeLengthAt(data, nextOffset) === 0) {
    nextOffset++;
  }
  return nextOffset;
}

function parseH264NalAt(data: Uint8Array, startCodeOffset: number): ParsedH264NalSpan | null {
  const startCodeLength = startCodeLengthAt(data, startCodeOffset);
  if (startCodeLength === 0) return null;

  const nalOffset = startCodeOffset + startCodeLength;
  if (nalOffset >= data.length) return null;

  const nextStartCodeOffset = findNextStartCodeOffset(data, nalOffset);
  const nalLength = nextStartCodeOffset - nalOffset;
  if (nalLength === 0) return null;

  const header = data[nalOffset];
  if ((header & 0x80) !== 0) return null;

  const nalType = header & 0x1f;
  const kind = classifyNalType(nalType);
  if (kind === null) return null;

  return {
    unit: {
      startCodeOffset,
      startCodeLength,
      nalOffset,
      nalLength,
      nalType,
      kind,
    },
    nextStartCodeOffset,
  };
}

/** Parse a complete Annex-B access unit, preserving each original delimiter span. */
export function parseH264AnnexB(data: Uint8Array): ParsedH264NalUnit[] | null {
  if (data.length === 0 || startCodeLengthAt(data, 0) === 0) return null;

  const units: ParsedH264NalUnit[] = [];
  let startCodeOffset = 0;

  while (startCodeOffset < data.length) {
    if (units.length >= MAX_NAL_UNITS) return null;

    const parsed = parseH264NalAt(data, startCodeOffset);
    if (parsed === null) return null;

    units.push(parsed.unit);
    if (parsed.nextStartCodeOffset === data.length) break;
    startCodeOffset = parsed.nextStartCodeOffset;
  }

  return units.length === 0 ? null : units;
}

interface H264BitReaderState {
  rawOffset: number;
  rawBoundary: number;
  zeroRun: number;
  currentByte: number;
  bitsRemaining: number;
}

function loadH264RbspByte(payload: Uint8Array, state: H264BitReaderState): boolean {
  while (state.rawOffset < payload.length) {
    const byte = payload[state.rawOffset];
    state.rawOffset++;

    if (state.zeroRun === 2 && byte <= 0x02) return false;
    if (state.zeroRun === 2 && byte === 0x03) {
      if (state.rawOffset >= payload.length || payload[state.rawOffset] > 0x03) return false;
      state.zeroRun = 0;
      continue;
    }

    state.currentByte = byte;
    state.bitsRemaining = 8;
    state.rawBoundary = state.rawOffset;
    state.zeroRun = byte === 0x00 ? Math.min(2, state.zeroRun + 1) : 0;
    return true;
  }

  return false;
}

function readH264RbspBit(payload: Uint8Array, state: H264BitReaderState): 0 | 1 | null {
  if (state.bitsRemaining === 0 && !loadH264RbspByte(payload, state)) return null;

  state.bitsRemaining--;
  return ((state.currentByte >>> state.bitsRemaining) & 0x01) === 0 ? 0 : 1;
}

function skipH264UnsignedExpGolomb(payload: Uint8Array, state: H264BitReaderState): boolean {
  let leadingZeroBits = 0;
  while (true) {
    const bit = readH264RbspBit(payload, state);
    if (bit === null) return false;
    if (bit === 1) break;
    leadingZeroBits++;
    if (leadingZeroBits > MAX_UE_LEADING_ZERO_BITS) return false;
  }

  for (let suffixBit = 0; suffixBit < leadingZeroBits; suffixBit++) {
    if (readH264RbspBit(payload, state) === null) return false;
  }
  return true;
}

/**
 * Locate the raw EBSP byte boundary after first_mb_in_slice, slice_type, and
 * pic_parameter_set_id. `payload` starts after the one-byte NAL header, and the
 * returned length is relative to that payload. The last partly consumed byte is
 * included so callers can keep the complete parser-visible prefix clear.
 */
export function findH264SlicePrefixLength(payload: Uint8Array): number | null {
  if (payload.length === 0) return null;

  const state: H264BitReaderState = {
    rawOffset: 0,
    rawBoundary: 0,
    zeroRun: 0,
    currentByte: 0,
    bitsRemaining: 0,
  };

  for (let field = 0; field < 3; field++) {
    if (!skipH264UnsignedExpGolomb(payload, state)) return null;
  }

  return state.rawBoundary;
}

function validZeroRun(value: number): boolean {
  return Number.isInteger(value) && value >= 0 && value <= 2;
}

/**
 * Escape a terminal byte region, using the immediately preceding clear-byte zero run as context.
 * The region must extend through the containing access unit's end (or be separately framed): an
 * output trailing zero immediately before a preserved three-byte start code is delimiter-ambiguous.
 */
export function stuffH264Bytes(data: Uint8Array, precedingZeroRun = 0): Uint8Array | null {
  if (!validZeroRun(precedingZeroRun)) return null;

  let outputLength = data.length;
  let zeroRun = precedingZeroRun;
  for (const byte of data) {
    if (zeroRun === 2 && byte <= 0x03) {
      outputLength++;
      zeroRun = 0;
    }
    zeroRun = byte === 0x00 ? Math.min(2, zeroRun + 1) : 0;
  }

  const result = new Uint8Array(outputLength);
  let outputOffset = 0;
  zeroRun = precedingZeroRun;
  for (const byte of data) {
    if (zeroRun === 2 && byte <= 0x03) {
      result[outputOffset] = 0x03;
      outputOffset++;
      zeroRun = 0;
    }
    result[outputOffset] = byte;
    outputOffset++;
    zeroRun = byte === 0x00 ? Math.min(2, zeroRun + 1) : 0;
  }

  return result;
}

/** Reverse `stuffH264Bytes`, rejecting any byte sequence that it could not produce. */
export function unstuffH264Bytes(data: Uint8Array, precedingZeroRun = 0): Uint8Array | null {
  if (!validZeroRun(precedingZeroRun)) return null;

  const outputLength = getUnstuffedH264Length(data, precedingZeroRun);
  if (outputLength === null) return null;

  return copyUnstuffedH264Bytes(data, outputLength, precedingZeroRun);
}

function getUnstuffedH264Length(data: Uint8Array, precedingZeroRun: number): number | null {
  let outputLength = 0;
  let zeroRun = precedingZeroRun;
  for (let offset = 0; offset < data.length; offset++) {
    const byte = data[offset];
    if (zeroRun === 2) {
      if (byte <= 0x02) return null;
      if (byte === 0x03) {
        if (offset + 1 >= data.length || data[offset + 1] > 0x03) return null;
        zeroRun = 0;
        continue;
      }
    }
    outputLength++;
    zeroRun = byte === 0x00 ? Math.min(2, zeroRun + 1) : 0;
  }

  return outputLength;
}

function copyUnstuffedH264Bytes(
  data: Uint8Array,
  outputLength: number,
  precedingZeroRun: number
): Uint8Array {
  const result = new Uint8Array(outputLength);
  let outputOffset = 0;
  let zeroRun = precedingZeroRun;
  for (const byte of data) {
    if (zeroRun === 2 && byte === 0x03) {
      zeroRun = 0;
      continue;
    }
    result[outputOffset] = byte;
    outputOffset++;
    zeroRun = byte === 0x00 ? Math.min(2, zeroRun + 1) : 0;
  }

  return result;
}
