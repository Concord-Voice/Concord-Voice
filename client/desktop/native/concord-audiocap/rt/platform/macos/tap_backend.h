#ifndef CONCORD_AUDIOCAP_RT_PLATFORM_MACOS_TAP_BACKEND_H_
#define CONCORD_AUDIOCAP_RT_PLATFORM_MACOS_TAP_BACKEND_H_

// The macOS capture backend's ENTIRE STATE MACHINE, header-only and free of any
// Apple header, so every branch of it runs on the Linux sanitizer legs through
// the fake HAL in test/macos_tap_test.cc. tap_backend.mm holds only the real
// Core Audio implementations of HalApi and the singleton platformBackend()
// returns.
//
// Why header-only rather than a .cc beside the .mm: a .cc would still be built
// by the mac-only gyp arm, and the point of the split is that the logic is
// reachable from a Linux compiler. This is the one place in rt/ where "header
// only" is a testability decision rather than a style one.

#include <atomic>

#include "../../capture_backend.h"
#include "../../quantum_pump.h"
#include "../../sink_gate.h"
#include "hal_api.h"
#include "tap_floor.h"

namespace concord {
namespace audiocap {
namespace rt {
namespace macos {

/// WHICH Core Audio call refused, so an operator reading status() learns more
/// than "a Core Audio call failed". Every kDeviceError arm in start() records
/// one of these plus the raw OSStatus: the closed BackendStart vocabulary stays
/// closed, and the diagnostic sits beside it rather than inside it.
enum class StartStage : u32 {
  kNone = 0u, kTranslatePid, kCreateTap, kTapFormat,
  kCreateAggregate, kCreateIoProc, kStartDevice
};

class TapBackend final : public CaptureBackend {
 public:
  /// `productVersion` is kern.osproductversion, INJECTED rather than read here,
  /// which is what makes the below-floor arm provable: no machine in the local
  /// or CI fleet runs below 14.4, so passing "14.3" is the only way to reach it.
  TapBackend(const HalApi& hal, const char* productVersion) noexcept
      : hal_(hal),
        aboveFloor_(meetsTapFloor(productVersion)),
        host_({nullptr, nullptr, nullptr, nullptr}),
        gate_(),
        pump_(nullptr),
        admittedChannels_(0u),
        tap_(kObjectUnknown),
        aggregate_(kObjectUnknown),
        procId_(nullptr),
        running_(false),
        armed_(false),
        spent_(false),
        quiesceProved_(false),
        destroyFailures_(0u),
        failStage_(StartStage::kNone),
        failStatus_(kNoErr) {}

  /// The PRODUCT floor (14.4, ADR-0043 D6). platformBackend() consults this and
  /// returns nullptr when it is false; the @available(macOS 14.2) SYMBOL guard
  /// in the .mm sits strictly inside it.
  bool availableForThisOs() const noexcept { return aboveFloor_; }

  BackendStart start(const CaptureTarget& target, QuantumPump& sink,
                     const HostServices& host) noexcept override {
    if (!aboveFloor_) { return BackendStart::kUnsupportedOs; }
    if (running_)     { return BackendStart::kAlreadyRunning; }
    // ONE CAPTURE PER INSTANCE, and this line is what makes that true HERE
    // rather than only in napi/addon.cc's g_captureArmed. gate_.close() is
    // permanent (rt/sink_gate.h), so a second start() would run the whole arm
    // sequence against the OS -- opening a real tap on the target process --
    // and then have every callback refused at the gate. kOk for a capture that
    // is structurally silent is worse than a refusal. Reported as
    // kAlreadyRunning because the seam owns the Poisoned vocabulary and this
    // backend has, in the only sense it can mean, already run.
    if (spent_)       { return BackendStart::kAlreadyRunning; }

    // REFUSED BEFORE ANY OS CALL, and the ordering is the assertion, not the
    // return value. An empty process list is the shape an omitted option, a
    // zeroed struct and a FAILED PID LOOKUP all take; it must never reach
    // CATapDescription. A refusal that has already created a tap is #2161 with
    // a cleanup step.
    //
    // kSystemMix is refused outright HERE rather than handled: buildTarget can
    // never emit it (rt/capture_backend.h:152-154), so reaching this backend
    // with it set means a caller assigned it by hand, and PR 2 has no
    // system-mix path at all -- the exclude-form initializer is not constructed
    // anywhere in this backend.
    //
    // pidCount is bounded for the SAME reason, and its absence was the gap that
    // proved the reasoning above is not rhetorical: target.pids is
    // kMaxTargetPids long and pidCount is a u8, so a hand-assembled struct with
    // pidCount > 8 wrote past `objects` below. buildTarget (capture_backend.h)
    // and parseTarget (napi/addon.cc) both bound it, which is exactly why this
    // backend must too -- it does not trust either of them with scope or with
    // an empty list, and a caller that set one field by hand can set this one.
    if (target.scope != CaptureScope::kProcessList) { return BackendStart::kNoTarget; }
    if (target.pidCount == 0u)                      { return BackendStart::kNoTarget; }
    if (target.pidCount > kMaxTargetPids)           { return BackendStart::kNoTarget; }

    // allowDescendants is READ BY NOTHING HERE, deliberately: CATapDescription
    // has no process-tree form, so macOS cannot honour it and the caller must
    // resolve the full PID list itself (#3198 owns that). Named rather than
    // silently ignored -- an option nothing reads is the silent-ignore trap
    // targetPids was fixed for in PR 1.

    // 1. PID -> process object. BOTH the status AND the object are tested:
    //    kAudioHardwarePropertyTranslatePIDToProcessObject returns kNoErr while
    //    yielding kObjectUnknown for a pid with no audio object -- measured --
    //    and checking only the status hands that straight to CATapDescription
    //    as though it were a resolved target.
    //
    //    The two arms are DIFFERENT IN KIND and no longer share a reason: a
    //    non-kNoErr status is the HAL refusing to answer, which is a device
    //    error; kObjectUnknown is the normal, explicable "this app has never
    //    played a sound". Collapsing them made a support ticket and a user's
    //    own choice of window indistinguishable at the seam.
    u32 objects[kMaxTargetPids];
    for (u8 i = 0u; i < target.pidCount; ++i) {
      u32 obj = kObjectUnknown;
      const u32 st = hal_.translatePidToProcessObject(target.pids[i], &obj);
      if (st != kNoErr) { return fail(StartStage::kTranslatePid, st); }
      if (obj == kObjectUnknown) { return BackendStart::kNoTarget; }
      objects[i] = obj;
    }

    // 2. The tap. INCLUDE form only -- see hal_api.h on the sibling selector.
    //
    //    NOTE WHAT IS **NOT** DONE ON THIS ARM: tap_ is not cleared. A HAL that
    //    published a handle and then reported failure would have that handle
    //    thrown away, leaving a live OS tap nothing can ever reclaim. hal_api.h
    //    states no "out-param untouched on failure" contract for these entries,
    //    so the state machine must not assume one -- teardown() destroys
    //    whatever is set, and destroyArtefacts()'s sentinel skips make that
    //    safe when nothing was.
    {
      const u32 st = hal_.createProcessTap(
          objects, static_cast<u32>(target.pidCount), &tap_);
      if (st != kNoErr) { teardown(); return fail(StartStage::kCreateTap, st); }
    }

    // 3. ADMIT THE FORMAT BEFORE ARMING ANYTHING. One machine measuring 48 kHz
    //    is not an invariant, and the whole capture graph can be built and
    //    started against a format rt/ would refuse.
    SourceFormat format = {0u, 0u, false, false};
    {
      const u32 st = hal_.tapFormat(tap_, &format);
      if (st != kNoErr) { teardown(); return fail(StartStage::kTapFormat, st); }
    }
    if (!acceptsSourceFormat(format)) {
      teardown();
      return BackendStart::kUnsupportedFormat;
    }
    // INTERLEAVED IS ADMITTED HERE, NOT IN acceptsSourceFormat. ioProcThunk
    // hardcodes `interleaved = true` because initStereoMixdownOfProcesses: is
    // the only tap constructor this backend uses -- so the assumption is this
    // BACKEND's, and it is refused where it is made. Adding the check to the
    // shared predicate instead would narrow it for every backend, killing
    // rt/frame_pack.h's working planar arm before #3196's WASAPI backend can
    // decide whether it needs it.
    if (!format.interleaved) {
      teardown();
      return BackendStart::kUnsupportedFormat;
    }
    // REMEMBERED, so the callback can tell "the format changed under us" from
    // "the backend passed nonsense". Without it a tap that started at 2ch and
    // began delivering 6ch degraded to total silence -- submit() refuses
    // srcChannels > kChannels and returns false -- with callbackTotal still
    // climbing, which is indistinguishable at the seam from capture-starved.
    admittedChannels_ = format.channelCount;

    {
      const u32 st = hal_.createAggregate(tap_, &aggregate_);
      if (st != kNoErr) {
        teardown();
        return fail(StartStage::kCreateAggregate, st);
      }
    }

    // Published BEFORE createIoProc, because the HAL may deliver a callback the
    // moment the IOProc exists. A null pump_ read from the callback would be a
    // dropped first quantum; the gate plus this ordering means the callback
    // either finds a live sink or is refused, never a half-built one.
    pump_.store(&sink, std::memory_order_release);
    host_ = host;

    {
      const u32 st = hal_.createIoProc(aggregate_, &TapBackend::ioProcEntry,
                                       this, &procId_);
      // armed_ is set on BOTH outcomes, and that is the point. If the HAL wrote
      // a procId and then reported failure, an IOProc exists and teardown owes
      // it the full barrier; if it wrote nothing, procId_ is null and every
      // step below skips it. Assuming failure means "nothing was created" is
      // what left this arm destroying an aggregate under a live callback.
      armed_ = true;
      if (st != kNoErr) { teardown(); return fail(StartStage::kCreateIoProc, st); }
    }
    {
      const u32 st = hal_.startDevice(aggregate_, procId_);
      if (st != kNoErr) { teardown(); return fail(StartStage::kStartDevice, st); }
    }

    running_ = true;
    return BackendStart::kOk;
  }

  /// POST-CONDITION, from rt/capture_backend.h: when this returns, no further
  /// call into the sink occurs from any thread, ever, and every OS artefact
  /// this backend created is destroyed. Idempotent.
  ///
  /// Apple documents NO quiescence guarantee after AudioDeviceStop returns, and
  /// HostServices::spawn/join are nullptr in a shipped binary -- so there is no
  /// thread to join and the barrier has to be built here. Four steps, in this
  /// order, and the order is the argument:
  void stop() noexcept override { teardown(); }

  /// Budget for step 3: FIVE QUANTUM PERIODS, the same number and the same
  /// reasoning as the seam's settle window.
  ///
  /// It is NOT kQuiesceBudgetMs and must not be confused with it. backend->stop()
  /// runs as teardown step 2, BEFORE the seam's own gate wait begins, so this
  /// 50 ms sits outside the 250 ms and adds to total stop() latency: worst case
  /// ~300 ms, still well inside the window in which main may reap the child.
  static constexpr u32 kBackendQuiesceMs = 50u;

  /// THE ONLY ROUTE AUDIO TAKES INTO THE PUMP. A HAL property listener is given
  /// no pointer that reaches here, which is how design section 9.5's
  /// prohibition is enforced structurally rather than by comment.
  static void ioProcEntry(void* ctx, const u8* const* planes, u16 channels,
                          bool interleaved, u32 frameCount,
                          u64 timestampNs) noexcept {
    TapBackend* self = static_cast<TapBackend*>(ctx);
    if (self == nullptr) { return; }
    // Entered before ANY pump touch. stop() closes this gate FIRST, which is
    // what makes destroying the OS artefacts safe under an in-flight callback.
    if (!self->gate_.tryEnter()) { return; }
    QuantumPump* pump = self->pump_.load(std::memory_order_acquire);
    if (pump != nullptr) {
      pump->noteCallback();
      // A degenerate callback carries channels == 0 by construction (see
      // tap_backend.mm) and is NOT a format change -- it is the starvation
      // shape, and submit() must still see it so the silence budget advances.
      if (frameCount != 0u && channels != self->admittedChannels_) {
        pump->fault(PumpFault::kFormatChanged);
      } else {
        (void)pump->submit(planes, channels, interleaved, frameCount, timestampNs);
      }
    }
    self->gate_.leave();
  }

 private:
  /// Records WHICH call refused and with what, then returns the closed-vocabulary
  /// answer. Exists so the five kDeviceError arms stop being indistinguishable:
  /// the .mm wrappers return real four-char OSStatus values ('!pri', 'nope',
  /// kAudioHardwareIllegalOperationError) and every one of them used to be
  /// dropped one layer up, leaving an operator with "a Core Audio call failed".
  BackendStart fail(StartStage stage, u32 status) noexcept {
    failStage_  = stage;
    failStatus_ = status;
    return BackendStart::kDeviceError;
  }

  /// THE BARRIER, and the ONLY path by which this backend releases anything.
  ///
  /// POST-CONDITION, from rt/capture_backend.h: when this returns, no further
  /// call into the sink occurs from any thread, ever, and every OS artefact
  /// this backend created is destroyed. Idempotent.
  ///
  /// Apple documents NO quiescence guarantee after AudioDeviceStop returns, and
  /// HostServices::spawn/join are nullptr in a shipped binary -- so there is no
  /// thread to join and the barrier has to be built here. Four steps, in this
  /// order, and the order is the argument.
  ///
  /// CALLED FROM stop() **AND FROM EVERY FAILED-START ARM**. It used to be
  /// stop()'s body, guarded by `if (!running_) return;` -- and running_ is set
  /// only after AudioDeviceStart succeeds, so the two arms that can leave a
  /// live IOProc behind (createIoProc and startDevice failing) could never
  /// reach it: they open-coded `pump_ = nullptr; destroyArtefacts();` with the
  /// gate still OPEN. `armed_` is the guard now, and it becomes true the moment
  /// an IOProc may exist.
  void teardown() noexcept {
    // Nothing was ever armed and nothing was created: a pre-OS-call refusal.
    // Leave spent_ alone -- the caller may legitimately retry with a corrected
    // target, and no gate has been closed.
    if (!armed_ && tap_ == kObjectUnknown && aggregate_ == kObjectUnknown &&
        procId_ == nullptr && !running_) {
      return;
    }
    running_ = false;
    // Any teardown closes the gate permanently, so this instance can never
    // carry another capture -- see start()'s spent_ refusal.
    spent_   = true;

    // 1. CLOSE THE GATE FIRST, before any HAL call. An IOProc entering after
    //    this returns immediately without touching the sink, which is what
    //    makes steps 2-4 safe under an in-flight callback. Permanent by design.
    //
    //    NOT REDUNDANT WITH the pump_ store below, though a functional test
    //    cannot tell them apart -- a mutation removing this close() survives
    //    every case in macos_tap_test.cc, because a late callback finds a null
    //    pump and does nothing either way. What the gate adds is the ORDERING
    //    GUARANTEE for the artefacts: steps 2-4 run knowing no NEW callback can
    //    enter. It is NOT what orders the pump_ access -- pump_ is std::atomic
    //    for that, because on the timeout arm a callback already inside the
    //    gate races this thread's store with no happens-before edge.
    //
    //    An earlier revision claimed "the CI thread-sanitizer leg is where that
    //    mutant dies". It does not: the only multithreaded case parks its
    //    producer and hands off through a release/acquire pair on g_ioInside,
    //    which orders the producer's pump_ read before this thread's write. TSAN
    //    has a happens-before chain and reports nothing. Removing close() is
    //    caught by test_stop_closesTheGateBeforeAnyHalCall, which asserts on the
    //    gate's own state rather than on an absence.
    gate_.close();

    // 2. Ask the OS to stop. Documented to stop callbacks; NOT documented to
    //    drain one already in flight -- which is the entire reason step 3 exists
    //    rather than this being the end of the function.
    if (aggregate_ != kObjectUnknown && procId_ != nullptr) {
      (void)hal_.stopDevice(aggregate_, procId_);
    }

    // 3. Bounded wait for the gate to read zero. FAILS CLOSED on a null nowNs
    //    or sleepMs: the wait cannot be performed, so take the timeout arm
    //    rather than assume quiescence -- the posture rt/teardown.h's hooks
    //    take. A pure spin is the rejected alternative: it burns the JS thread
    //    at the end of every share and, on a contended machine, can keep the
    //    audio thread from being scheduled to finish, delaying what it waits on.
    //
    //    THE OUTCOME IS NOW RECORDED. Three states used to run identical code
    //    and leave identical state -- waited-and-quiesced, waited-and-expired,
    //    and never-waited-at-all -- so "fails closed" named a timeout arm that
    //    did not exist, and no mutation of this block changed any verdict. The
    //    skipped wait and the expired wait both land on false, deliberately:
    //    both mean "this backend did not observe its callbacks stop".
    quiesceProved_ = false;
    if (host_.nowNs != nullptr && host_.sleepMs != nullptr) {
      const u64 deadline =
          host_.nowNs() + (static_cast<u64>(kBackendQuiesceMs) * 1000000ull);
      while (!gate_.quiesced() && host_.nowNs() < deadline) {
        host_.sleepMs(1u);
      }
      quiesceProved_ = gate_.quiesced();
    }

    // 4. DESTROY UNCONDITIONALLY, quiesced or not.
    //
    //    THE LOAD-BEARING DECISION, and it mirrors the seam's teardown one layer
    //    down: the privacy invariant is the tap's EXISTENCE, which is the one
    //    thing nothing above this backend can end. A teardown that skipped
    //    destruction because its wait expired leaves behind exactly the artefact
    //    #3197 is about -- a live OS tap after the share ended is a privacy
    //    failure even when no byte moves.
    //
    //    THE TRADE, STATED: destroying under a possibly-live callback can dangle
    //    APPLE's state, never ours. The IOProc touches this backend's gate, its
    //    atomic pump_ and the pump itself -- all of which outlive the process --
    //    plus the two atomic IOProc slots in tap_backend.mm, never the tap. If
    //    Apple does not honour it we get a crash in a child that holds no tokens
    //    and that the host already handles; a leaked live tap has neither
    //    property.
    destroyArtefacts();
    pump_.store(nullptr, std::memory_order_release);
  }

  /// Destroys whatever exists, in the mandated order, and clears the handles.
  /// IDEMPOTENT: kObjectUnknown / nullptr members are skipped, so a failed start
  /// that already unwound and a stop() that follows it do not double-destroy.
  ///
  /// EVERY STATUS IS COUNTED, AND NONE IS BRANCHED ON. The two are different
  /// decisions and this code used to conflate them. Not branching is right: a
  /// failed destroy must never skip the next destroy. But discarding the status
  /// meant AudioHardwareDestroyProcessTap could fail, the handle be cleared on
  /// the next line, and a live OS tap on another application's audio survive
  /// with nothing anywhere able to observe it -- the privacy invariant this
  /// epic exists for, failing silently. destroyFailures_ is that evidence; it
  /// costs one increment and no branch of consequence.
  void destroyArtefacts() noexcept {
    if (procId_ != nullptr && aggregate_ != kObjectUnknown) {
      if (hal_.destroyIoProc(aggregate_, procId_) != kNoErr) {
        bumpDestroyFailure();
      }
    }
    procId_ = nullptr;
    if (aggregate_ != kObjectUnknown) {
      if (hal_.destroyAggregate(aggregate_) != kNoErr) { bumpDestroyFailure(); }
    }
    aggregate_ = kObjectUnknown;
    if (tap_ != kObjectUnknown) {
      if (hal_.destroyTap(tap_) != kNoErr) { bumpDestroyFailure(); }
    }
    tap_ = kObjectUnknown;
    armed_ = false;
  }

  /// Saturating, so a pathological run cannot wrap the count back to "clean".
  void bumpDestroyFailure() noexcept {
    if (destroyFailures_ != 0xFFFFFFFFu) { destroyFailures_ += 1u; }
  }

 public:
  /// Read from the JS thread after stop(), for status(). None of these change
  /// a decision inside rt/ -- they are the evidence that the decisions taken
  /// were the ones intended, which is the thing this backend previously had no
  /// way to report. See design section 9.4.
  ///
  /// quiesceProved(): false for BOTH the expired wait and the skipped one. The
  /// two are the same fact -- "this backend did not observe its callbacks stop"
  /// -- and collapsing them is deliberate, because a caller that treated them
  /// differently would be claiming the skipped wait proved something.
  bool quiesceProved()   const noexcept override { return quiesceProved_; }
  /// Non-zero means an OS artefact this backend created may still exist. The
  /// destroy ran and the handle was released regardless; this is the only
  /// evidence anywhere that it did not take.
  u32  destroyFailures() const noexcept override { return destroyFailures_; }
  u32  lastDeviceStatus() const noexcept override { return failStatus_; }
  /// macOS-internal, not on the contract: WHICH call refused. The status above
  /// is the platform-opaque half a generic caller can carry; this half names a
  /// Core Audio entry point and is asserted directly by macos_tap_test.
  StartStage failStage() const noexcept { return failStage_; }
  u32  failStatus()      const noexcept { return failStatus_; }

 private:
  const HalApi&  hal_;
  const bool     aboveFloor_;
  HostServices   host_;
  SinkGate       gate_;
  /// ATOMIC, and the timeout path is why. The gate supplies the ordering on the
  /// quiesced path, but step 4 destroys UNCONDITIONALLY -- so on an expired wait
  /// this thread's store races the IOProc's load with no happens-before edge
  /// between them. std::atomic is a named deviation for rt/ already
  /// ([internal]rules/native-audio.md).
  std::atomic<QuantumPump*> pump_;
  /// What acceptsSourceFormat admitted, so a mid-capture change is named rather
  /// than degrading into silence. Written once in start() before the IOProc
  /// exists and only read after, so it needs no atomic.
  u16            admittedChannels_;
  u32            tap_;
  u32            aggregate_;
  void*          procId_;
  bool           running_;
  /// An IOProc EXISTS, so teardown's barrier is owed -- distinct from running_,
  /// which means AudioDeviceStart also succeeded. Every arm between those two
  /// facts used to unwind without the barrier; see teardown().
  bool           armed_;
  /// This instance has consumed its one capture. The gate closes permanently,
  /// so a second start() would open a real OS tap that can never deliver a
  /// frame -- see start().
  bool           spent_;
  bool           quiesceProved_;
  u32            destroyFailures_;
  StartStage     failStage_;
  u32            failStatus_;
};

}  // namespace macos
}  // namespace rt
}  // namespace audiocap
}  // namespace concord

#endif  // CONCORD_AUDIOCAP_RT_PLATFORM_MACOS_TAP_BACKEND_H_
