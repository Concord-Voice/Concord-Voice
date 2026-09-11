// THE ONLY FILE IN THIS ADDON THAT NAMES CORE AUDIO.
//
// Everything above it -- the refusal ordering, the format admission, the
// teardown barrier -- lives in tap_backend.h and is exercised by
// test/macos_tap_test.cc on the Linux ASAN/UBSAN/TSAN legs through a fake HAL.
// This file is deliberately thin: ten wrappers, a version read, and the
// singleton. If logic starts accumulating here it belongs in the header, where
// it can be tested.
//
// The call sequence below is not invented. It is the one the R9 spike ran on
// hardware (macOS 26.6.2), which returned noErr at every stage and produced
// correctly-shaped 48 kHz binary32 stereo buffers.

#import <CoreAudio/CoreAudio.h>
#import <CoreAudio/AudioHardwareTapping.h>
#import <CoreAudio/CATapDescription.h>
#import <Foundation/Foundation.h>

#include <atomic>
#include <mach/mach_time.h>
#include <sys/sysctl.h>

#include "tap_backend.h"

namespace concord {
namespace audiocap {
namespace rt {
namespace macos {
namespace {

// ---------------------------------------------------------------------------
// IOProc plumbing.
//
// One capture per process (the host forks one child per share and a second
// start() is refused with Poisoned), so a single static slot is the whole
// registry. It holds a FUNCTION POINTER and an opaque ctx -- never a pointer to
// the pump -- which is what keeps design section 9.5's listener prohibition
// structural: nothing here has a handle that reaches the sink.
// ---------------------------------------------------------------------------
/// ATOMIC, because these are written on the JS thread by halCreateIoProc /
/// halDestroyIoProc and read on Core Audio's real-time IO thread inside
/// ioProcThunk. Apple documents no guarantee that a callback has exited when
/// AudioDeviceDestroyIOProcID returns, so the plain-pointer version was a data
/// race and therefore UB -- in the ONE file of this addon that no sanitizer leg
/// can ever compile, since the Linux legs build the state machine against the
/// fake HAL and never see this translation unit.
///
/// WHAT THE RACE WAS AND WAS NOT, measured rather than assumed: no
/// use-after-free (the ctx is a function-local static with a trivial destructor
/// and the sink outlives the process) and no cross-session tear (one pair is
/// ever published, because a second start() is refused). The one reachable tear
/// was a valid fn paired with an already-cleared ctx, which ioProcEntry
/// null-checks. Fixed as undefined behaviour, not as a live defect.
///
/// The store order below is the publish/teardown mirror: ctx before fn on the
/// way up, fn before ctx on the way down, so a racing callback that observes a
/// non-null fn has already been able to observe the matching ctx.
std::atomic<HalApi::IoProcFn> g_ioProcFn{nullptr};
std::atomic<void*>            g_ioProcCtx{nullptr};

/// mach_absolute_time converted to nanoseconds -- the SAME domain uv_hrtime
/// reports, which is what HostServices::nowNs returns. QuantumPump's budget is
/// measured against that clock, so a device-clock value here would measure the
/// silence budget in the wrong unit.
///
/// Returns 0 ONLY when the timebase is unreadable, and callers must treat that
/// as "no usable stamp" rather than as time zero: a zero stamp makes
/// QuantumPump's `timestampNs >= captureStartNs_ + budget` permanently false,
/// which would switch the silence detector off silently -- the exact failure
/// class this backend exists to report.
u64 hostTimeToNs(u64 hostTime) noexcept {
  static mach_timebase_info_data_t tb = {0u, 0u};
  if (tb.denom == 0u) { (void)mach_timebase_info(&tb); }
  if (tb.denom == 0u) { return 0u; }
  return (hostTime * static_cast<u64>(tb.numer)) / static_cast<u64>(tb.denom);
}

/// A monotonic stamp for this callback, or 0 if the machine cannot give one.
u64 nowStampNs(const AudioTimeStamp* inputTime) noexcept {
  const bool hostValid =
      (inputTime != nullptr &&
       (inputTime->mFlags & kAudioTimeStampHostTimeValid) != 0);
  return hostValid ? hostTimeToNs(static_cast<u64>(inputTime->mHostTime))
                   : hostTimeToNs(static_cast<u64>(mach_absolute_time()));
}

OSStatus ioProcThunk(AudioObjectID, const AudioTimeStamp*,
                     const AudioBufferList* input, const AudioTimeStamp* inputTime,
                     AudioBufferList*, const AudioTimeStamp*, void*) noexcept {
  const HalApi::IoProcFn fn = g_ioProcFn.load(std::memory_order_acquire);
  if (fn == nullptr) { return noErr; }
  void* const ctx = g_ioProcCtx.load(std::memory_order_acquire);
  // STAMPED EVEN ON THE DEGENERATE ARMS. They used to pass 0, which meant a tap
  // delivering only empty callbacks could never latch the silence budget --
  // doubly blind, since submit()'s own guards also refuse before the latch is
  // reached. The starvation shape the two arms below exist to report was the
  // one shape the detector could not see.
  const u64 stamp = nowStampNs(inputTime);

  if (input == nullptr || input->mNumberBuffers == 0u) {
    // A callback that delivered nothing usable is still evidence the tap is
    // alive, and that distinction is exactly what callbackTotal exists for --
    // so it is reported with a zero frame count rather than dropped.
    fn(ctx, nullptr, 0u, true, 0u, stamp);
    return noErr;
  }

  // AV 215 CANDIDATE DEVIATION: walking mBuffers is pointer arithmetic over a
  // C flexible-array member. Only buffer 0 is read -- the tap is created by the
  // STEREO MIXDOWN initializer, so the OS delivers one interleaved buffer and
  // there is no second plane to gather. TapBackend::start() refuses a
  // non-interleaved admitted format, which is what makes that assumption
  // checked rather than merely stated.
  const AudioBuffer& buf = input->mBuffers[0];
  if (buf.mData == nullptr || buf.mDataByteSize == 0u || buf.mNumberChannels == 0u) {
    fn(ctx, nullptr, 0u, true, 0u, stamp);
    return noErr;
  }

  const u8* planes[1] = {static_cast<const u8*>(buf.mData)};
  const u32 frames =
      static_cast<u32>(buf.mDataByteSize) /
      (static_cast<u32>(buf.mNumberChannels) * static_cast<u32>(kBytesPerSample));

  fn(ctx, planes, static_cast<u16>(buf.mNumberChannels), true, frames, stamp);
  return noErr;
}

// ---------------------------------------------------------------------------
// The ten wrappers. Each is a direct translation and holds no policy.
// ---------------------------------------------------------------------------

u32 halTranslatePid(u32 pid, u32* outObject) noexcept {
  if (outObject == nullptr) { return static_cast<u32>(kAudio_ParamError); }
  *outObject = kObjectUnknown;
  AudioObjectPropertyAddress addr = {kAudioHardwarePropertyTranslatePIDToProcessObject,
                                     kAudioObjectPropertyScopeGlobal,
                                     kAudioObjectPropertyElementMain};
  AudioObjectID obj  = kAudioObjectUnknown;
  UInt32        size = sizeof(obj);
  pid_t         in   = static_cast<pid_t>(pid);
  const OSStatus s = AudioObjectGetPropertyData(kAudioObjectSystemObject, &addr,
                                                sizeof(in), &in, &size, &obj);
  // Written even on failure: the caller tests BOTH this status and the object,
  // because this call returns noErr while resolving nothing (design 9.0).
  *outObject = static_cast<u32>(obj);
  return static_cast<u32>(s);
}

/// ANNOTATED RATHER THAN GUARDED INTERNALLY. The requirement propagates to
/// realHal() and is discharged by the single @available in platformBackend(),
/// which is the one place that decides whether this backend exists at all.
/// Guarding here instead would put a runtime version test on every capture and
/// leave the decision in two places.
API_AVAILABLE(macos(14.2))
u32 halCreateTap(const u32* processObjects, u32 count, u32* outTap) noexcept {
  if (processObjects == nullptr || count == 0u || outTap == nullptr) {
    return static_cast<u32>(kAudio_ParamError);
  }
  *outTap = kObjectUnknown;

  @autoreleasepool {
    NSMutableArray<NSNumber*>* objects = [NSMutableArray arrayWithCapacity:count];
    for (u32 i = 0u; i < count; ++i) {
      [objects addObject:@(processObjects[i])];
    }
    // THE INCLUDE FORM, AND ONLY THE INCLUDE FORM. The sibling
    // initStereoGlobalTapButExcludeProcesses: with an empty array is a
    // whole-system tap -- #2161's shape -- and is constructed nowhere in this
    // addon. The stereo mixdown is also what lets rt/ claim nothing downmixes:
    // the OS does it.
    CATapDescription* desc =
        [[CATapDescription alloc] initStereoMixdownOfProcesses:objects];
    if (desc == nil) { return static_cast<u32>(kAudio_ParamError); }
    desc.name         = @"Concord Voice screen-share audio";
    desc.privateTap   = YES;             // invisible to other processes
    desc.muteBehavior = CATapUnmuted;    // the user keeps hearing their own audio

    AudioObjectID tap = kAudioObjectUnknown;
    const OSStatus s  = AudioHardwareCreateProcessTap(desc, &tap);
    if (s == noErr) { *outTap = static_cast<u32>(tap); }
    return static_cast<u32>(s);
  }
}

u32 halTapFormat(u32 tap, SourceFormat* outFormat) noexcept {
  if (outFormat == nullptr) { return static_cast<u32>(kAudio_ParamError); }
  AudioStreamBasicDescription asbd;
  std::memset(&asbd, 0, sizeof(asbd));
  AudioObjectPropertyAddress addr = {kAudioTapPropertyFormat,
                                     kAudioObjectPropertyScopeGlobal,
                                     kAudioObjectPropertyElementMain};
  UInt32 size = sizeof(asbd);
  const OSStatus s = AudioObjectGetPropertyData(static_cast<AudioObjectID>(tap),
                                                &addr, 0, nullptr, &size, &asbd);
  if (s != noErr) { return static_cast<u32>(s); }
  // THE OUTPUT IS TESTED, NOT JUST THE STATUS -- the convention halTranslatePid
  // already follows, for the same reason: AudioObjectGetPropertyData can return
  // noErr having written less than a full description. The memset above means a
  // short write would read as zeros and acceptsSourceFormat would refuse, so
  // this fails closed either way; testing it here names the failure instead of
  // letting it arrive disguised as an unsupported format.
  if (size != sizeof(asbd)) { return static_cast<u32>(kAudio_ParamError); }

  outFormat->sampleRate   = static_cast<u32>(asbd.mSampleRate);
  outFormat->channelCount = static_cast<u16>(asbd.mChannelsPerFrame);
  outFormat->interleaved  = (asbd.mFormatFlags & kAudioFormatFlagIsNonInterleaved) == 0u;
  outFormat->isFloat32    = ((asbd.mFormatFlags & kAudioFormatFlagIsFloat) != 0u) &&
                            (asbd.mBitsPerChannel == 32u);
  return static_cast<u32>(noErr);
}

u32 halCreateAggregate(u32 tap, u32* outAggregate) noexcept {
  if (outAggregate == nullptr) { return static_cast<u32>(kAudio_ParamError); }
  *outAggregate = kObjectUnknown;

  @autoreleasepool {
    CFStringRef tapUid = nullptr;
    AudioObjectPropertyAddress addr = {kAudioTapPropertyUID,
                                       kAudioObjectPropertyScopeGlobal,
                                       kAudioObjectPropertyElementMain};
    UInt32 size = sizeof(tapUid);
    const OSStatus us = AudioObjectGetPropertyData(static_cast<AudioObjectID>(tap),
                                                   &addr, 0, nullptr, &size, &tapUid);
    if (us != noErr || tapUid == nullptr) {
      return static_cast<u32>(us == noErr ? kAudio_ParamError : us);
    }
    NSString* uid = (__bridge_transfer NSString*)tapUid;

    // PRIVATE, and with NO sub-devices: this aggregate exists only to carry the
    // tap into an IOProc, never to play anything. tapautostart is NO because
    // that key DELAYS AudioDeviceStart until the first audio arrives, and a
    // capture that has started but not yet been spoken into is exactly the
    // state the silence detector needs to observe.
    NSDictionary* composition = @{
      @kAudioAggregateDeviceUIDKey           : [[NSUUID UUID] UUIDString],
      @kAudioAggregateDeviceNameKey          : @"Concord Voice capture",
      @kAudioAggregateDeviceIsPrivateKey     : @YES,
      @kAudioAggregateDeviceIsStackedKey     : @NO,
      @kAudioAggregateDeviceTapAutoStartKey  : @NO,
      @kAudioAggregateDeviceSubDeviceListKey : @[],
      @kAudioAggregateDeviceTapListKey       : @[ @{ @kAudioSubTapUIDKey : uid } ],
    };

    AudioObjectID agg = kAudioObjectUnknown;
    const OSStatus s =
        AudioHardwareCreateAggregateDevice((__bridge CFDictionaryRef)composition, &agg);
    if (s == noErr) { *outAggregate = static_cast<u32>(agg); }
    return static_cast<u32>(s);
  }
}

u32 halCreateIoProc(u32 aggregate, HalApi::IoProcFn fn, void* ctx,
                    void** outProcId) noexcept {
  if (fn == nullptr || outProcId == nullptr) { return static_cast<u32>(kAudio_ParamError); }
  *outProcId  = nullptr;
  // CTX FIRST, then fn: the thunk tests fn before it reads ctx, so publishing
  // in this order means a callback that observes a live fn can already observe
  // the ctx that belongs with it.
  g_ioProcCtx.store(ctx, std::memory_order_release);
  g_ioProcFn.store(fn, std::memory_order_release);

  AudioDeviceIOProcID procId = nullptr;
  const OSStatus s = AudioDeviceCreateIOProcID(static_cast<AudioObjectID>(aggregate),
                                               ioProcThunk, nullptr, &procId);
  if (s != noErr) {
    g_ioProcFn.store(nullptr, std::memory_order_release);
    g_ioProcCtx.store(nullptr, std::memory_order_release);
    return static_cast<u32>(s);
  }
  *outProcId = reinterpret_cast<void*>(procId);
  return static_cast<u32>(noErr);
}

u32 halStartDevice(u32 aggregate, void* procId) noexcept {
  return static_cast<u32>(AudioDeviceStart(static_cast<AudioObjectID>(aggregate),
                                           reinterpret_cast<AudioDeviceIOProcID>(procId)));
}

u32 halStopDevice(u32 aggregate, void* procId) noexcept {
  return static_cast<u32>(AudioDeviceStop(static_cast<AudioObjectID>(aggregate),
                                          reinterpret_cast<AudioDeviceIOProcID>(procId)));
}

u32 halDestroyIoProc(u32 aggregate, void* procId) noexcept {
  const OSStatus s =
      AudioDeviceDestroyIOProcID(static_cast<AudioObjectID>(aggregate),
                                 reinterpret_cast<AudioDeviceIOProcID>(procId));
  // Cleared AFTER the OS call, so a callback racing destruction still finds the
  // function pointer it was dispatched with rather than a half-cleared slot --
  // and cleared FN FIRST, the mirror of the publish order, so a racing thunk
  // that reads a null fn returns before it would have read the stale ctx. That
  // guarantee needed the slots to be atomic to hold at all; as plain pointers
  // the comment described an intent the code could not deliver.
  g_ioProcFn.store(nullptr, std::memory_order_release);
  g_ioProcCtx.store(nullptr, std::memory_order_release);
  return static_cast<u32>(s);
}

u32 halDestroyAggregate(u32 aggregate) noexcept {
  return static_cast<u32>(AudioHardwareDestroyAggregateDevice(
      static_cast<AudioObjectID>(aggregate)));
}

API_AVAILABLE(macos(14.2))
u32 halDestroyTap(u32 tap) noexcept {
  return static_cast<u32>(AudioHardwareDestroyProcessTap(static_cast<AudioObjectID>(tap)));
}

/// Carries the 14.2 requirement because it takes the addresses of two symbols
/// that have it -- taking an unavailable function's address is as unguarded as
/// calling it, which -Werror=unguarded-availability-new is right to reject.
API_AVAILABLE(macos(14.2))
const HalApi& realHal() noexcept {
  static const HalApi hal = {halTranslatePid,   halCreateTap,      halTapFormat,
                             halCreateAggregate, halCreateIoProc,  halStartDevice,
                             halStopDevice,      halDestroyIoProc, halDestroyAggregate,
                             halDestroyTap};
  return hal;
}

/// kern.osproductversion is the MARKETING version ("14.4.1", "26.6.2"), which is
/// what the floor is expressed in. kern.osrelease is the Darwin version and
/// would need a mapping table nobody would keep current.
const char* productVersion() noexcept {
  static char buf[64];
  static bool read = false;
  if (!read) {
    read = true;
    std::size_t len = sizeof(buf);
    if (sysctlbyname("kern.osproductversion", buf, &len, nullptr, 0) != 0) {
      buf[0] = '\0';
    }
    buf[sizeof(buf) - 1] = '\0';
  }
  return buf;
}

}  // namespace
}  // namespace macos

/// Above the 14.4 PRODUCT floor, the singleton; below it nullptr, which
/// Start_JS already resolves to "NoBackend".
///
/// THE TWO FLOORS ARE NESTED HERE, and that is the whole point of putting the
/// enforcement at this function: @available(macOS 14.2) protects the weak
/// SYMBOLS and sits strictly inside meetsTapFloor(14.4), which protects the
/// PRODUCT decision. Below the floor there is no backend object at all, so
/// there is no start() that must remember to check a version before touching a
/// weak symbol -- the class that cannot be reached cannot get it wrong.
CaptureBackend* platformBackend() noexcept {
  if (@available(macOS 14.2, *)) {
    // NO CLOCK, NO CAPTURE. hostTimeToNs returns 0 when mach_timebase_info
    // cannot be read, and every callback would then carry stamp 0 -- which is
    // before every budget, so QuantumPump's silence latch could never fire and
    // AudioData.timestamp would be meaningless. Substituting 0 satisfies the
    // type and breaks the contract QuantumPump states for timestampNs, and it
    // breaks it SILENTLY, switching off the one detector that can see a starved
    // tap. A machine that cannot report monotonic time is refused instead, the
    // same fail-closed direction as the version floor beside it.
    //
    // Unreachable in practice -- mach_timebase_info does not fail -- and cheap
    // enough that "unreachable" is not a reason to leave the silent arm in.
    if (macos::hostTimeToNs(1000000ull) == 0u) { return nullptr; }
    static macos::TapBackend backend(macos::realHal(), macos::productVersion());
    if (backend.availableForThisOs()) { return &backend; }
  }
  return nullptr;
}

}  // namespace rt
}  // namespace audiocap
}  // namespace concord
