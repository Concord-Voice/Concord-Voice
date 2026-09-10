// concord-audiocap / rt — the 32-byte quantum header and the wire constants.
//
// JSF++ BINDS HERE. See [internal]rules/native-audio.md. Everything below is
// allocation-free, exception-free and total: the encoder either writes all 32
// bytes or writes none and says so.
//
// WHY THIS FILE EXISTS SEPARATELY FROM napi/.
// These bytes cross a trust boundary — child process to preload to renderer — and
// ADR-0043 D4b risk 4 is a native memory-safety fault reaching an E2EE client
// through exactly this path. Living in rt/ buys two things a napi/ helper could
// not: the JSF++ profile guard runs over it, and the libFuzzer target (which
// cannot include node_api.h) can reach it. The encoder is fuzzed for the same
// reason the ring is.
//
// THE CONSTANTS ARE A MIRROR, NOT A SOURCE. Their authority is
// client/desktop/src/shared/audiocapProtocol.ts, which every JS leg of the
// transport reads. This file must agree with it byte for byte; the child asserts
// that agreement at `start` by comparing the numbers it was handed against the
// ones compiled in here, and reports fault{stage:'start'} rather than adapting.
// Design section 4c owns the layout.
//
// AV 215 (no pointer arithmetic): every write below is `dst[index]`.
// AV 206 (no allocation): the caller owns the destination; nothing here allocates.

#ifndef CONCORD_AUDIOCAP_RT_QUANTUM_HEADER_H_
#define CONCORD_AUDIOCAP_RT_QUANTUM_HEADER_H_

#include <cstddef>
#include <cstdint>

namespace concord {
namespace audiocap {
namespace rt {

// AV 209: no bare int/short/long/float/double. u8 and u32 are also declared by
// quantum_ring.h; an identical alias redeclaration is legal and keeps either
// header usable on its own.
using u8  = std::uint8_t;
using u16 = std::uint16_t;
using u32 = std::uint32_t;
using u64 = std::uint64_t;

// --- Wire constants, mirroring src/shared/audiocapProtocol.ts -----------------

constexpr u16 kMagic         = 0xCA57u;
// The WIRE-FORMAT version in the second byte, distinct from AUDIOCAP_PROTOCOL,
// which versions the control channel. Both read 1 today and may move apart.
constexpr u8  kHeaderVersion = 1u;
constexpr u32 kHeaderBytes   = 32u;

// Pinned, not negotiated: the encoder is Opus with opusStereo, and the repo
// standardises on a 48 kHz AudioContext. Any resampling belongs upstream of this
// header, never in a branch taken off one of its fields.
constexpr u32 kSampleRate     = 48000u;
constexpr u16 kChannels       = 2u;
constexpr u16 kFrameCount     = 480u;
constexpr u32 kBytesPerSample = 4u;  // interleaved IEEE-754 binary32, little-endian

constexpr u32 kSampleBytes = static_cast<u32>(kFrameCount) *
                             static_cast<u32>(kChannels) * kBytesPerSample;
constexpr u32 kQuantumBytes = kHeaderBytes + kSampleBytes;

// One quantum is 480 frames at 48 kHz.
constexpr u32 kQuantumMs = 10u;

// EQUAL ON PURPOSE (design section 4d). The child drains only while
// outstanding < kCreditBound; when it stops draining, the ring fills and the
// producer drops the newest. Credit exhaustion and ring overflow are therefore
// the same event with a single drop site, counted once. Making these two numbers
// differ would create a second, uncounted drop site.
constexpr u32 kCreditBound = 8u;
constexpr u32 kRingSlots   = 8u;

static_assert(kQuantumBytes == 3872u,
              "QUANTUM_BYTES must equal audiocapProtocol.ts");
static_assert(kHeaderBytes == 32u,
              "HEADER_BYTES must equal audiocapProtocol.ts");
static_assert(kCreditBound == kRingSlots,
              "one drop site requires ringSlots == creditBound (design section 4d)");

// --- Field offsets, little-endian --------------------------------------------
//
// One table, read by the encoder and by every test's independent reader, so the
// two cannot drift apart by eye. Mirrors the OFFSET table in audiocapProtocol.ts.

constexpr u32 kOffMagic              = 0u;
constexpr u32 kOffVersion            = 2u;
constexpr u32 kOffFlags              = 3u;
constexpr u32 kOffSeq                = 4u;
constexpr u32 kOffSampleRate         = 8u;
constexpr u32 kOffChannels           = 12u;
constexpr u32 kOffFrameCount         = 14u;
constexpr u32 kOffCaptureTimestampNs = 16u;
constexpr u32 kOffOverrunTotal       = 24u;
constexpr u32 kOffReserved           = 28u;

static_assert(kOffReserved + 4u == kHeaderBytes, "the header ends where it says it does");

/// The three fields a caller may vary. Every other field is a constant of the
/// protocol and is written from this file, never from an argument — so there is
/// no value a producer can pass that changes what the consumer accepts.
struct QuantumHeader {
  u32 seq;                  // wraps; a gap at the far end is a drop witness
  u64 captureTimestampNs;   // producer clock, for AudioData.timestamp only
  u32 overrunTotal;         // saturating count of drops as of this quantum
};

// --- Little-endian writers ----------------------------------------------------
//
// Explicit byte assembly rather than a memcpy of the native representation: this
// is a wire format, and a big-endian or mixed-endian host must produce the same
// bytes. Nothing here reads the destination, so an uninitialised buffer is fine.

inline void writeU16LE(u8* dst, u32 off, u16 value) noexcept {
  dst[off]        = static_cast<u8>(value & 0xFFu);
  dst[off + 1u]   = static_cast<u8>((value >> 8) & 0xFFu);
}

inline void writeU32LE(u8* dst, u32 off, u32 value) noexcept {
  for (u32 i = 0u; i < 4u; ++i) {
    dst[off + i] = static_cast<u8>((value >> (8u * i)) & 0xFFu);
  }
}

inline void writeU64LE(u8* dst, u32 off, u64 value) noexcept {
  for (u32 i = 0u; i < 8u; ++i) {
    dst[off + i] = static_cast<u8>((value >> (8u * i)) & 0xFFu);
  }
}

/// Writes the 32-byte header at the front of `into`, or writes NOTHING.
///
/// Returns false for a null destination or one shorter than the header, and the
/// result MUST be tested (AV 115) — the fuzz target asserts that a refusal leaves
/// every byte untouched, because a half-written header is indistinguishable at the
/// far end from a truncated transfer, and the far end would then be deciding
/// accept-or-close on bytes nobody wrote.
///
/// It does not write the samples. The caller owns bytes [kHeaderBytes,
/// kQuantumBytes), which is what lets one slot carry a whole quantum and removes
/// any need to re-associate a header with its audio downstream.
inline bool encodeQuantumHeader(u8* into, std::size_t intoLen,
                                const QuantumHeader& header) noexcept {
  if (into == nullptr) { return false; }
  if (intoLen < static_cast<std::size_t>(kHeaderBytes)) { return false; }

  writeU16LE(into, kOffMagic, kMagic);
  into[kOffVersion] = kHeaderVersion;
  into[kOffFlags]   = 0u;
  writeU32LE(into, kOffSeq, header.seq);
  writeU32LE(into, kOffSampleRate, kSampleRate);
  writeU16LE(into, kOffChannels, kChannels);
  writeU16LE(into, kOffFrameCount, kFrameCount);
  writeU64LE(into, kOffCaptureTimestampNs, header.captureTimestampNs);
  writeU32LE(into, kOffOverrunTotal, header.overrunTotal);
  writeU32LE(into, kOffReserved, 0u);
  return true;
}

}  // namespace rt
}  // namespace audiocap
}  // namespace concord

#endif  // CONCORD_AUDIOCAP_RT_QUANTUM_HEADER_H_
