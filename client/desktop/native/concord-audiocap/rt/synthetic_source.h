// concord-audiocap / rt — the deterministic synthetic PCM source.
//
// COMPILE-GATED OUT OF EVERY RELEASE BUILD (design section 5, Q1, and ADR-0043's
// K1 ruling). A reachable test tone inside a signed binary can publish a wrong
// source into a call, which is the same class of harm as the system-mix fallback
// that constraint C9 forbids outright. The gate is the PRESENCE of
// CONCORD_AUDIOCAP_SYNTHETIC, set only by the CI/test gyp target and by the unit
// test's own translation unit; defining it to 0 does not turn it off, matching
// the CONCORD_AUDIOCAP_TEST_SEAM idiom in quantum_ring.h.
//
// JSF++ BINDS HERE.
//
// WHY A TABLE OF BIT PATTERNS AND NOT sinf().
// Two reasons, and the second is the one that matters.
//
//   1. AV 209 forbids bare float/double, and there is no fixed-width alias for
//      binary32 before C++23. Emitting the IEEE-754 bit pattern directly and
//      assembling it little-endian keeps the file inside the profile without a
//      deviation, and makes the byte order explicit rather than inherited from
//      the host.
//   2. Task 8 asserts EXACT SAMPLE CONTENT at the far end of the transport, three
//      processes away. A computed sine is a platform's libm rounding away from a
//      different last bit on a different runner, so that assertion would be
//      either flaky or forced down to a tolerance — and a tolerance cannot tell a
//      correctly-transported quantum from a subtly corrupted one, which is the
//      whole point of asserting content. A table is byte-identical everywhere.
//
// The waveform is 0.25 * sin(2*pi*k/47) at 48 kHz, so roughly 1021 Hz. The table
// length is 47 BECAUSE IT DOES NOT DIVIDE 480: a length that divided the frame
// count would start every quantum at phase 0, making the samples independent of
// seq, and a source that ignored seq entirely would then satisfy every
// determinism test written against it.

#ifndef CONCORD_AUDIOCAP_RT_SYNTHETIC_SOURCE_H_
#define CONCORD_AUDIOCAP_RT_SYNTHETIC_SOURCE_H_

#ifdef CONCORD_AUDIOCAP_SYNTHETIC

#include <cstddef>
#include <cstdint>

#include "quantum_header.h"

namespace concord {
namespace audiocap {
namespace rt {

// AV 207: the table is a private, immutable member of this class rather than an
// unencapsulated array at namespace scope.
class SyntheticSource {
 public:
  static constexpr u32 kTableLen = 47u;

  // A quarter period, rounded. The right channel leads the left by it, so the two
  // channels are never byte-identical and the interleave order is observable at
  // the far end — an encoder that swapped or duplicated channels would otherwise
  // pass every content assertion.
  static constexpr u32 kRightPhaseOffset = 12u;

  /// The table entry at `index`, wrapped. Exposed so a test can pin exact content
  /// without restating the table, which would test the test's copy of it.
  static u32 tableEntry(u32 index) noexcept { return kTable[index % kTableLen]; }

  /// The first table index of quantum `seq`.
  ///
  /// Computed as ((seq mod L) * (frames mod L)) mod L rather than (seq * frames)
  /// mod L: the direct product overflows u32 after about 25 hours of capture, and
  /// an overflow here is a phase discontinuity in the middle of a stream that is
  /// meant to be reproducible from seq alone. The largest intermediate this form
  /// can produce is 46 * 10.
  static u32 startIndex(u32 seq) noexcept {
    const u32 framesMod = static_cast<u32>(kFrameCount) % kTableLen;
    return ((seq % kTableLen) * framesMod) % kTableLen;
  }

  /// Writes kSampleBytes of interleaved little-endian binary32 for quantum `seq`,
  /// or writes NOTHING and returns false.
  ///
  /// A pure function of `seq`: no clock, no counter, no state of any kind. That is
  /// the property Task 8's content assertion rests on, and it is why this returns
  /// the same bytes for the same seq no matter what ran in between or in what
  /// order.
  static bool fillSamples(u32 seq, u8* dst, std::size_t dstLen) noexcept {
    if (dst == nullptr) { return false; }
    if (dstLen < static_cast<std::size_t>(kSampleBytes)) { return false; }

    u32 index = startIndex(seq);
    for (u32 frame = 0u; frame < static_cast<u32>(kFrameCount); ++frame) {
      // AV 201: the loop counter is not touched in the body; `index` is a
      // separate cursor advanced below.
      const u32 base = frame * static_cast<u32>(kChannels) * kBytesPerSample;
      writeU32LE(dst, base, kTable[index]);
      writeU32LE(dst, base + kBytesPerSample,
                 kTable[(index + kRightPhaseOffset) % kTableLen]);
      index = index + 1u;
      if (index == kTableLen) { index = 0u; }
    }
    return true;
  }

 private:
  // IEEE-754 binary32 bit patterns for 0.25 * sin(2*pi*k/47), k = 0..46.
  // Regenerate with:
  //   python3 -c "import math,struct;print([hex(struct.unpack('<I',struct.pack('<f',0.25*math.sin(2*math.pi*k/47)))[0]) for k in range(47)])"
  static constexpr u32 kTable[kTableLen] = {
      0x00000000u, 0x3D087C60u, 0x3D87449Fu, 0x3DC7E11Au,
      0x3E02763Cu, 0x3E1EA7EEu, 0x3E3804D4u, 0x3E4E1912u,
      0x3E607FCAu, 0x3E6EE4EDu, 0x3E7906B6u, 0x3E7EB6DEu,
      0x3E7FDB67u, 0x3E7C6F1Au, 0x3E748199u, 0x3E68371Cu,
      0x3E57C7CAu, 0x3E437EB6u, 0x3E2BB88Cu, 0x3E10E1E9u,
      0x3DE6EACCu, 0x3DA7F2E0u, 0x3D4BF769u, 0x3C88CA94u,
      0xBC88CA94u, 0xBD4BF769u, 0xBDA7F2E0u, 0xBDE6EACCu,
      0xBE10E1E9u, 0xBE2BB88Cu, 0xBE437EB6u, 0xBE57C7CAu,
      0xBE68371Cu, 0xBE748199u, 0xBE7C6F1Au, 0xBE7FDB67u,
      0xBE7EB6DEu, 0xBE7906B6u, 0xBE6EE4EDu, 0xBE607FCAu,
      0xBE4E1912u, 0xBE3804D4u, 0xBE1EA7EEu, 0xBE02763Cu,
      0xBDC7E11Au, 0xBD87449Fu, 0xBD087C60u,
  };
};

}  // namespace rt
}  // namespace audiocap
}  // namespace concord

#endif  // CONCORD_AUDIOCAP_SYNTHETIC

#endif  // CONCORD_AUDIOCAP_RT_SYNTHETIC_SOURCE_H_
