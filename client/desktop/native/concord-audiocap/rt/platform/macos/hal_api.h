#ifndef CONCORD_AUDIOCAP_RT_PLATFORM_MACOS_HAL_API_H_
#define CONCORD_AUDIOCAP_RT_PLATFORM_MACOS_HAL_API_H_

// THE SEAM THAT MAKES A CORE AUDIO BACKEND TESTABLE ON LINUX.
//
// Not one Apple type appears here. Object ids are u32 (AudioObjectID is a u32),
// statuses are u32 (OSStatus is signed, but every caller asks only "is this
// kNoErr", and a negative value widened to u32 is still non-zero), and the
// IOProcID is an opaque void*. So this header -- and tap_backend.h above it,
// which holds the entire state machine -- compile everywhere.
//
// That is not a convenience. client/desktop/native/** sits outside
// sonar.sources, so NO line of the macOS backend can ever count toward the
// >=80% new-code gate; the fake-HAL cases running on the Linux ASAN/UBSAN/TSAN
// legs are the only coverage this code will ever have (design section 9.9, A5).
//
// tap_backend.mm supplies the real implementations and is the ONLY file in the
// addon that names Core Audio. test/macos_tap_test.cc supplies fakes with
// scripted failures.

#include "../../capture_backend.h"
#include "../../quantum_header.h"

namespace concord {
namespace audiocap {
namespace rt {
namespace macos {

constexpr u32 kNoErr         = 0u;   // OSStatus noErr
constexpr u32 kObjectUnknown = 0u;   // kAudioObjectUnknown

struct HalApi {
  /// Delivers ONE callback's frames.
  ///
  /// THE ONLY ROUTE AUDIO TAKES INTO THE PUMP, and that is structural rather
  /// than documentary: a HAL property listener is handed no pointer it could
  /// use to reach the sink, which is how design section 9.5's prohibition is
  /// enforced instead of merely written down.
  using IoProcFn = void (*)(void* ctx, const u8* const* planes, u16 channels,
                            bool interleaved, u32 frameCount,
                            u64 timestampNs) noexcept;

  /// kAudioHardwarePropertyTranslatePIDToProcessObject.
  ///
  /// RETURNS kNoErr WHILE YIELDING kObjectUnknown for a PID with no audio
  /// object -- MEASURED on macOS 26.6.2 (design section 9.0). Checking only the
  /// status is FAIL-OPEN: it hands kAudioObjectUnknown to CATapDescription as if
  /// it were a resolved target. Every caller tests both. This is ADR-0043 D4b
  /// risk 2's macOS mechanism.
  u32 (*translatePidToProcessObject)(u32 pid, u32* outObject) noexcept;

  /// AudioHardwareCreateProcessTap, via the INCLUDE form
  /// (initStereoMixdownOfProcesses:) and NOTHING ELSE.
  ///
  /// The sibling initStereoGlobalTapButExcludeProcesses: with an empty array is
  /// a whole-system tap -- #2161's shape, one selector away. No implementation
  /// of this table may construct it.
  u32 (*createProcessTap)(const u32* processObjects, u32 count, u32* outTap) noexcept;

  /// kAudioTapPropertyFormat, read and admitted BEFORE startDevice.
  ///
  /// The mixdown initializer measured 48 kHz on one machine, which is NOT an
  /// invariant: the device-and-stream initializer yields the device's native
  /// rate (192 kHz where this was measured), and turning a single measurement
  /// into an invariant is how R9 got its first wrong answer.
  u32 (*tapFormat)(u32 tap, SourceFormat* outFormat) noexcept;

  u32 (*createAggregate)(u32 tap, u32* outAggregate) noexcept;
  u32 (*createIoProc)(u32 aggregate, IoProcFn fn, void* ctx, void** outProcId) noexcept;
  u32 (*startDevice)(u32 aggregate, void* procId) noexcept;

  /// AudioDeviceStop. Documented to stop callbacks; NOT documented to drain one
  /// already in flight. That undocumented gap is the entire reason tap_backend.h
  /// builds a barrier around this call rather than trusting its return.
  u32 (*stopDevice)(u32 aggregate, void* procId) noexcept;

  u32 (*destroyIoProc)(u32 aggregate, void* procId) noexcept;
  u32 (*destroyAggregate)(u32 aggregate) noexcept;
  u32 (*destroyTap)(u32 tap) noexcept;
};

}  // namespace macos
}  // namespace rt
}  // namespace audiocap
}  // namespace concord

#endif  // CONCORD_AUDIOCAP_RT_PLATFORM_MACOS_HAL_API_H_
