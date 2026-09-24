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
/// processObjectList's refusal when the system holds more HAL clients than the
/// caller's buffer. A four-char code in OSStatus style, so failStatus() reads
/// the same way as a real Core Audio refusal.
constexpr u32 kStatusListOverflow = 0x6F76666Cu;  // 'ovfl'

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

  // #3394 PR 2 -- the process-tree entries. APPENDED, never interleaved:
  // realHal() in tap_backend.mm initialises this table positionally.

  /// kAudioHardwarePropertyProcessObjectList: every client process currently
  /// connected to the HAL. A list longer than `capacity` REFUSES (non-kNoErr,
  /// *outCount = 0); it never truncates on its own account, because a truncated
  /// list could omit the helper that renders.
  ///
  /// ONE RESIDUAL THE WRAPPER CANNOT CLOSE: the size read and the data read are
  /// two HAL calls, so a list that grew past `capacity` between them comes back
  /// FULL -- exactly `capacity` entries, indistinguishable here from a list that
  /// really was that long. The caller owns that ambiguity and refuses a full
  /// buffer (TapBackend::resolveTree); this entry reports what it read.
  u32 (*processObjectList)(u32* outObjects, u32 capacity, u32* outCount) noexcept;
  /// kAudioProcessPropertyPID of one process object.
  u32 (*processObjectPid)(u32 object, u32* outPid) noexcept;
  /// proc_pidinfo(PROC_PIDTBSDINFO).pbi_ppid. false when unreadable.
  bool (*parentPid)(u32 pid, u32* outParent) noexcept;
  /// getppid() of this utility process, which is the Electron main process.
  u32 (*hostRootPid)() noexcept;
};

}  // namespace macos
}  // namespace rt
}  // namespace audiocap
}  // namespace concord

#endif  // CONCORD_AUDIOCAP_RT_PLATFORM_MACOS_HAL_API_H_
