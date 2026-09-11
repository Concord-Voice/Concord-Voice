// concord-audiocap / rt — libFuzzer target for QuantumPump.
//
// Companion to fuzz_quantum_ring.cc: the ring is fuzzed because it is reachable
// across a trust boundary (ADR-0043 § Verification), and the pump sits directly
// upstream of it on the same audio-callback path -- every byte it emits is a
// ring push. This target draws BOTH the callback geometry (channel count,
// interleaved-vs-planar) and the per-callback frame-count sequence from the
// input, then asserts that every quantum the pump actually pushes is exactly
// kQuantumBytes with a well-formed header and a seq that never goes backwards.
//
// Build (CI does this; it cannot run under a sandbox that blocks ASAN's shadow map):
//   clang++ -std=c++17 -g -O1 -fsanitize=fuzzer,address,undefined \
//       test/fuzz_quantum_pump.cc -o fuzz_quantum_pump

#include "../rt/quantum_header.h"
#include "../rt/quantum_pump.h"
#include "../rt/quantum_ring.h"

// No <cassert>: this target signals with __builtin_trap() rather than assert(), so
// a failure survives -DNDEBUG the same way fuzz_quantum_ring.cc's does -- an
// assert() that compiles away in an optimized libFuzzer build finds nothing.
#include <cstddef>
#include <cstdint>
#include <vector>

using concord::audiocap::rt::QuantumPump;
using concord::audiocap::rt::QuantumRing;
using concord::audiocap::rt::RingResult;
using concord::audiocap::rt::u16;
using concord::audiocap::rt::u32;
using concord::audiocap::rt::u64;
using concord::audiocap::rt::u8;

namespace hdr = concord::audiocap::rt;

namespace {

// LOCAL little-endian readers, deliberately not the production decoder: a
// decoder that mirrors the encoder agrees with every mistake it makes, which is
// the one thing an oracle here may not do. Mirrors fuzz_quantum_ring.cc.
u16 readU16(const uint8_t* d) {
  return static_cast<u16>(static_cast<u16>(d[0]) |
                          static_cast<u16>(static_cast<u16>(d[1]) << 8));
}

u16 readHeaderU16(const std::vector<u8>& b, u32 off) {
  return static_cast<u16>(static_cast<u16>(b[off]) |
                          static_cast<u16>(static_cast<u16>(b[off + 1u]) << 8));
}

u32 readHeaderU32(const std::vector<u8>& b, u32 off) {
  u32 v = 0u;
  for (u32 i = 0u; i < 4u; ++i) { v |= static_cast<u32>(b[off + i]) << (8u * i); }
  return v;
}

// Stamps a 4-byte opaque "sample" at a byte offset: the low byte carries a
// fuzzer-drawn payload byte (so different inputs still exercise different
// content), and the high 24 bits carry the frame's GLOBAL index across the
// whole run. Both channels of a frame always receive the IDENTICAL value --
// this fuzzer never has a real L/R difference to preserve, which is what lets
// the oracle below assert L==R universally, in mono AND stereo, and catch a
// mono path that stops duplicating into the second channel.
void stampU32LE(std::vector<u8>& dst, std::size_t at, u32 value) {
  dst[at + 0u] = static_cast<u8>(value & 0xFFu);
  dst[at + 1u] = static_cast<u8>((value >> 8) & 0xFFu);
  dst[at + 2u] = static_cast<u8>((value >> 16) & 0xFFu);
  dst[at + 3u] = static_cast<u8>((value >> 24) & 0xFFu);
}

void noopSignal(void*) { /* the fuzzer reads emitted quanta straight off the ring;
                             it needs no availability edge */ }

// Bounds how many synthetic OS callbacks one input can drive. The regrouping
// defect this pump exists to catch lives in callback SIZE, not callback COUNT,
// so a modest cap keeps runs fast without narrowing what the size sequence can
// exercise. A dedicated cap, not derived from the input length, so a short
// input cannot silently exercise zero callbacks and a long one cannot silently
// blow the per-run time budget.
constexpr std::size_t kMaxCallbacks = 64u;

// Caps a single callback's frame count. 2001 straddles kFrameCount (480) at
// roughly 4x, so both a partial quantum and several whole quanta per callback
// occur, without letting a single u16 draw push the per-callback allocation
// into the hundreds of KB on every iteration.
constexpr u32 kFrameCountCeiling = 2001u;

}  // namespace

extern "C" int LLVMFuzzerTestOneInput(const uint8_t* data, size_t size) {
  if (size < 3u) { return 0; }

  // ONE dedicated control byte for both behavioural switches this target has
  // (channel count, interleaved-vs-planar). Not derived from any length or
  // geometry field -- see fuzz_quantum_ring.cc:135-136 for why that coupling
  // makes whole shapes structurally unreachable.
  const uint8_t control  = data[0];
  const bool stereo      = (control & 0x01u) != 0u;
  const bool interleaved = (control & 0x02u) != 0u;
  const u16  srcChannels = stereo ? 2u : 1u;
  data += 1;
  size -= 1;

  // The LEADING bytes are a sequence of per-callback frame counts, two bytes
  // each (a u16, so counts above 255 -- including values straddling
  // kFrameCount and multi-quantum-per-callback -- are reachable). Everything
  // after that sequence is sample payload.
  std::vector<u32> frameCounts;
  std::size_t cursor = 0u;
  while (cursor + 2u <= size && frameCounts.size() < kMaxCallbacks) {
    frameCounts.push_back(readU16(&data[cursor]) % kFrameCountCeiling);
    cursor += 2u;
  }
  const uint8_t* payload       = data + cursor;
  const std::size_t payloadLen = size - cursor;

  // Fixed production geometry: kRingSlots slots of exactly one quantum each.
  std::vector<u8> ringStorage(
      QuantumRing::storageBytes(hdr::kRingSlots, hdr::kQuantumBytes), 0u);
  QuantumRing ring(ringStorage.data(), ringStorage.size(), hdr::kRingSlots,
                    hdr::kQuantumBytes);
  if (!ring.valid()) { __builtin_trap(); }  // fixed geometry; must always hold

  QuantumPump pump(ring, &noopSignal, nullptr);
  pump.reset(0u);

  std::vector<u8> popped(hdr::kQuantumBytes, 0u);
  bool haveLastSeq = false;
  u32  lastSeq     = 0u;
  u64  timestampNs = 0u;
  std::size_t payloadCursor = 0u;
  // Increments by exactly one per frame SUBMITTED, in submission order, across
  // the whole run -- never reset per callback. Because frame_pack moves frames
  // in order and the pump's accumulator cursor advances one-for-one with what
  // it is handed, global frame index N is ALWAYS the (N mod kFrameCount)'th
  // frame of quantum (N / kFrameCount) -- independent of how the input carved
  // N frames across callbacks, and independent of any ring drop, since a drop
  // discards an already-numbered quantum rather than renumbering anything.
  // That is what lets the check below recompute the expected value from the
  // popped header's own seq field with no bookkeeping of its own.
  u32 globalFrameCounter = 0u;

  for (const u32 frameCount : frameCounts) {
    pump.noteCallback();

    // Buffers sized EXACTLY for this callback's frame count -- packFrames must
    // never be handed more room than it needs to read, which is what makes an
    // ASAN over-read here a real finding rather than a false one.
    const std::size_t bytesPerPlane =
        static_cast<std::size_t>(frameCount) * hdr::kBytesPerSample;
    const bool usesPlaneB = (srcChannels == 2u) && !interleaved;
    std::vector<u8> planeA(bytesPerPlane, 0u);
    std::vector<u8> planeB;
    if (usesPlaneB) { planeB.assign(bytesPerPlane, 0u); }

    const bool usesInterleavedPlane = (srcChannels == 2u) && interleaved;
    const std::size_t interleavedBytes =
        static_cast<std::size_t>(frameCount) *
        static_cast<std::size_t>(srcChannels) * hdr::kBytesPerSample;
    std::vector<u8> planeInterleaved;
    if (usesInterleavedPlane) { planeInterleaved.assign(interleavedBytes, 0u); }

    // Stamp every frame with (frame's global index << 8 | a payload byte),
    // identically into whichever plane(s) this layout uses for that frame --
    // the payload byte is what draws fuzzer-input diversity into the content
    // (distinct inputs still exercise distinct bytes), the index is what makes
    // the popped-quantum check below self-checking with no separate model.
    for (u32 f = 0u; f < frameCount; ++f) {
      const u8 noise = payloadLen > 0u ? payload[payloadCursor % payloadLen] : 0xABu;
      ++payloadCursor;
      const u32 sample = (globalFrameCounter << 8) | static_cast<u32>(noise);
      ++globalFrameCounter;

      if (srcChannels == 1u) {
        stampU32LE(planeA, static_cast<std::size_t>(f) * hdr::kBytesPerSample, sample);
      } else if (interleaved) {
        const std::size_t base =
            static_cast<std::size_t>(f) * 2u * hdr::kBytesPerSample;
        stampU32LE(planeInterleaved, base, sample);
        stampU32LE(planeInterleaved, base + hdr::kBytesPerSample, sample);
      } else {
        const std::size_t base = static_cast<std::size_t>(f) * hdr::kBytesPerSample;
        stampU32LE(planeA, base, sample);
        stampU32LE(planeB, base, sample);
      }
    }

    const u8* planes[2] = {nullptr, nullptr};
    if (srcChannels == 1u) {
      planes[0] = planeA.data();
    } else if (interleaved) {
      planes[0] = planeInterleaved.data();
    } else {
      planes[0] = planeA.data();
      planes[1] = planeB.data();
    }

    timestampNs += 10000000ull;  // 10 ms per callback, monotonic like a real clock
    const bool ok = pump.submit(planes, srcChannels, interleaved, frameCount,
                                 timestampNs);
    if (frameCount == 0u) {
      if (ok) { __builtin_trap(); }   // AV 115: a zero frame count must refuse
    } else {
      if (!ok) { __builtin_trap(); }  // every plane here is valid; must accept
    }

    // Drain everything this callback produced and check EVERY quantum, not
    // just the last -- one callback's frame count can complete several.
    for (;;) {
      u32 outBytes = 0u;
      const RingResult r =
          ring.pop(popped.data(), static_cast<u32>(popped.size()), &outBytes);
      if (r == RingResult::kEmpty) { break; }
      if (r != RingResult::kOk) { __builtin_trap(); }  // ring sized for its own producer
      if (outBytes != hdr::kQuantumBytes) { __builtin_trap(); }

      if (readHeaderU16(popped, hdr::kOffMagic) != hdr::kMagic) { __builtin_trap(); }
      if (popped[hdr::kOffVersion] != hdr::kHeaderVersion) { __builtin_trap(); }
      const u32 seq = readHeaderU32(popped, hdr::kOffSeq);
      if (haveLastSeq && seq <= lastSeq) { __builtin_trap(); }  // never backwards
      haveLastSeq = true;
      lastSeq     = seq;

      // Every one of the 480 output frames: L and R must be byte-identical
      // (catches a mono path that stops duplicating into the second channel),
      // and each must carry exactly the global frame index this quantum was
      // stamped with at submission time (catches a quantum emitted with the
      // accumulator one frame short of a full 480).
      const u64 expectedStart =
          static_cast<u64>(seq) * static_cast<u64>(hdr::kFrameCount);
      for (u32 i = 0u; i < static_cast<u32>(hdr::kFrameCount); ++i) {
        const u32 base = hdr::kHeaderBytes + i * 2u * hdr::kBytesPerSample;
        const u32 left  = readHeaderU32(popped, base);
        const u32 right = readHeaderU32(popped, base + hdr::kBytesPerSample);
        if (left != right) { __builtin_trap(); }
        const u64 expectedIndex = expectedStart + static_cast<u64>(i);
        if (static_cast<u64>(left >> 8) != expectedIndex) { __builtin_trap(); }
      }
    }
  }

  return 0;
}
