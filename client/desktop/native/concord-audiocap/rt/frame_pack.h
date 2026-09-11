// concord-audiocap / rt — the channel and layout repacketizer.
//
// JSF++ BINDS HERE. See [internal]rules/native-audio.md. This function runs on an
// OS audio callback thread: it allocates nothing, it cannot fail in a way the
// caller may ignore (AV 115), and every access below is an array subscript.
//
// WHY THERE IS NO ARITHMETIC ON A SAMPLE, ANYWHERE IN THIS FILE.
// A sample is an OPAQUE 4-byte element here. It is moved with std::memcpy and
// never added to another one. That is not squeamishness about rounding: AV 209
// forbids a bare `float`, there is no fixed-width alias for binary32 before
// C++23, and a downmix is the only operation that would need one. Refusing to
// downmix keeps this file inside the profile with NO named deviation — which
// acceptance criterion A9 requires of PR 1 — and it is free, because the tap
// itself mixes: macOS delivers stereo via CATapDescription's stereo-mixdown
// form, and a source that is neither mono nor stereo is refused upstream with
// UnsupportedFormat rather than folded down here (design section 7.2).
//
// AV 215 (no pointer arithmetic): every read and write below is `x[index]`, and
// where a pointer is genuinely needed it is `&x[index]` — subscript then
// address-of, which is the construct AV 215 names as the alternative to walking
// a pointer. Same shape as quantum_ring.h.
//
// AV 206 (no allocation): source and destination are both owned by the caller.

#ifndef CONCORD_AUDIOCAP_RT_FRAME_PACK_H_
#define CONCORD_AUDIOCAP_RT_FRAME_PACK_H_

#include <cstddef>
#include <cstring>

#include "quantum_header.h"

namespace concord {
namespace audiocap {
namespace rt {

/// THE DESTINATION LAYOUT IS PARAMETERISED ON kChannels AND THE BODY IS NOT.
/// Three places below spell the number two by hand — mono is memcpy'd into
/// exactly two slots, planar stereo reads exactly plane 0 and plane 1, and the
/// interleaved arm moves one whole frame on the strength of source and
/// destination having the same channel count. Raising kChannels to 6 would leave
/// every one of them compiling and silently writing a two-channel frame into a
/// six-channel slot, which downstream reads as audio rather than as an error. So
/// the constant is pinned to the code that assumes it: a future surround layout
/// must come here and REWRITE this file, which is the correct cost, rather than
/// discovering it as a garbled quantum.
static_assert(kChannels == 2u,
              "frame_pack duplicates and interleaves exactly two channels; a wider "
              "kChannels needs this file rewritten, not recompiled");

/// Moves frames from an OS callback's layout into the seam's fixed layout:
/// interleaved stereo IEEE-754 binary32, little-endian, `kBytesPerSample` per
/// element. Writes exactly `frameCount * kChannels * kBytesPerSample` bytes at
/// `dst`, or writes NOTHING and returns false.
///
/// srcPlanes: interleaved -> ONE plane holding `srcChannels` samples per frame;
///            planar      -> one plane PER CHANNEL, one sample per frame each.
///            Mono has one plane in both layouts, so `interleaved` does not
///            change where a mono sample is read from.
/// srcFrameOffset: a count of whole FRAMES into each plane, never of samples. It
///            is what lets a caller feed one callback into the accumulator in
///            several pieces without copying the callback's buffer first.
///
/// Returns false — and writes nothing — on a null plane array, a null plane this
/// call would read, a null destination, `srcChannels` outside 1..kChannels, or a
/// zero frame count. AV 115: the result must be tested. A half-packed
/// destination is indistinguishable downstream from correctly packed audio, so
/// every refusal is decided before the first byte moves.
///
/// The caller owns bounds. This function cannot know how long a plane is, so it
/// reads exactly the frames it was told to and the caller must not tell it more
/// than the OS delivered.
inline bool packFrames(const u8* const* srcPlanes, u16 srcChannels,
                       bool interleaved, u32 srcFrameOffset, u32 frameCount,
                       u8* dst) noexcept {
  if (srcPlanes == nullptr || dst == nullptr) { return false; }
  if (srcChannels == 0u || srcChannels > kChannels) { return false; }
  if (frameCount == 0u) { return false; }

  // Both plane pointers are validated HERE, before the loop, and not inside it.
  // Forming `&plane[offset]` on a null plane in order to test the result against
  // nullptr is undefined behaviour, which is a check UBSAN reports rather than a
  // check that holds. Which planes are read depends on the layout: interleaved
  // stereo reads one plane, so a null second plane is not this call's business.
  const bool readsSecondPlane = (srcChannels > 1u) && !interleaved;
  if (srcPlanes[0] == nullptr) { return false; }
  if (readsSecondPlane && (srcPlanes[1] == nullptr)) { return false; }

  const std::size_t bytesPerSample = static_cast<std::size_t>(kBytesPerSample);
  const std::size_t bytesPerDstFrame =
      static_cast<std::size_t>(kChannels) * bytesPerSample;

  for (u32 f = 0u; f < frameCount; ++f) {
    // AV 201: `f` is not modified in the body; every cursor below is derived.
    const std::size_t srcFrame =
        static_cast<std::size_t>(srcFrameOffset) + static_cast<std::size_t>(f);
    const std::size_t dstAt = static_cast<std::size_t>(f) * bytesPerDstFrame;

    if (srcChannels == 1u) {
      // DUPLICATED into both channels, never averaged with anything. One plane
      // in either layout.
      const std::size_t at = srcFrame * bytesPerSample;
      std::memcpy(&dst[dstAt], &srcPlanes[0][at], bytesPerSample);
      std::memcpy(&dst[dstAt + bytesPerSample], &srcPlanes[0][at], bytesPerSample);
    } else if (interleaved) {
      // Already the seam's layout: one whole frame is one contiguous move.
      const std::size_t at = srcFrame * bytesPerDstFrame;
      std::memcpy(&dst[dstAt], &srcPlanes[0][at], bytesPerDstFrame);
    } else {
      const std::size_t at = srcFrame * bytesPerSample;
      std::memcpy(&dst[dstAt], &srcPlanes[0][at], bytesPerSample);
      std::memcpy(&dst[dstAt + bytesPerSample], &srcPlanes[1][at], bytesPerSample);
    }
  }
  return true;
}

}  // namespace rt
}  // namespace audiocap
}  // namespace concord

#endif  // CONCORD_AUDIOCAP_RT_FRAME_PACK_H_
