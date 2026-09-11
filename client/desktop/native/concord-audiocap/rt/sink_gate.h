// concord-audiocap / rt — the teardown gate.
//
// JSF++ BINDS HERE. See [internal]rules/native-audio.md.
//
// WHY THIS EXISTS (design section 4.1, F3).
// The shipped teardown was "set running false, JOIN the producer thread, then
// release the threadsafe function". A callback-driven backend has no thread to
// join: a Core Audio IOProc or a WASAPI callback belongs to the OS, and there is
// no handle on which a join can wait. Releasing the threadsafe function while
// such a callback is mid-flight is a use-after-release in a process that loaded
// native code -- ADR-0043 D4b risk 4, materialized -- and the harm it names is
// the privacy defect this epic is about: a live tap outliving the share.
//
// So teardown stops being "join the thread", which a HAL callback structurally
// cannot satisfy, and becomes "close the gate and prove nobody is inside", which
// a joined thread satisfies trivially and a HAL callback satisfies observably.
//
// ONE ATOMIC, NOT A FLAG PLUS A COUNTER. Deliberately the repo's existing shape:
// presenceAuthzState packs epoch<<32 | openCount for the same reason -- a reader
// must be able to learn "a teardown was in flight WHEN I LOOKED", and two
// separate words cannot be read at one instant. Bit 31 is the closed flag and
// bits 0..30 are the in-flight count.
//
// AV 206 (no allocation): one atomic word, no storage of any kind.

#ifndef CONCORD_AUDIOCAP_RT_SINK_GATE_H_
#define CONCORD_AUDIOCAP_RT_SINK_GATE_H_

#include <atomic>

#include "quantum_header.h"

namespace concord {
namespace audiocap {
namespace rt {

/// Ceiling on the bounded wait for quiescence, in milliseconds.
///
/// 250 rather than seconds because THE OS ARTEFACT IS ALREADY GONE by the time
/// the wait begins: teardown destroys the tap and the IOProc before it closes the
/// gate, so this wait buys threadsafe-function handle hygiene only, and its
/// failure mode -- abandon the handle and latch `poisoned` -- is safe and
/// bounded. One quantum is 10 ms and an in-flight callback completes in
/// microseconds, so this is roughly 25 quantum periods of margin while keeping
/// stop() well inside any window in which the host may reap the child.
constexpr u32 kQuiesceBudgetMs = 250u;

class SinkGate {
 public:
  static constexpr u32 kClosedBit = 0x80000000u;
  static constexpr u32 kCountMask = 0x7FFFFFFFu;

  SinkGate() noexcept : state_(0u) {}

  // Approved deviation (C++11 `= delete`): a gate is the identity of one
  // teardown, and a copy would be a second gate nobody closes.
  SinkGate(const SinkGate&)            = delete;
  SinkGate& operator=(const SinkGate&) = delete;

  /// Producer thread. True means the caller is INSIDE and must call leave().
  ///
  /// THE INCREMENT HAPPENS BEFORE THE CLOSED-BIT TEST, and that order is the
  /// whole correctness argument: a racing enter is either observed by the closer
  /// (because it is already counted) or backs itself out (because it saw the
  /// bit). There is no window in which a caller is inside the sink and invisible
  /// to quiesced().
  ///
  /// The count occupies 31 bits and would need 2^31 simultaneous callers to
  /// carry into the closed bit. There is one producer thread per capture, so that
  /// is unreachable rather than guarded -- a guard would add a branch to the one
  /// path that runs inside an audio callback.
  bool tryEnter() noexcept {
    const u32 prev = state_.fetch_add(1u, std::memory_order_acq_rel);
#ifdef CONCORD_AUDIOCAP_TEST_SEAM
    // TEST-ONLY, and absent from every production translation unit: nothing under
    // binding.gyp defines this macro, so the released addon compiles the add and
    // the test back to back exactly as they read here.
    //
    // It exists because THIS ORDER CANNOT BE TESTED BY RACING FOR IT. Two threads
    // make the required interleaving merely probable: measured on this branch, an
    // exact test-before-count mutant survived 19 of 20 plain runs. A mutation kill
    // that depends on the scheduler is not a kill -- quantum_ring.h:230 records
    // the same lesson for depth()'s load order, and this is the same seam. It lets
    // one test close the gate at exactly the instant between the increment and the
    // closed-bit test, and assert that a closer looking right then SEES this
    // caller.
    if (enterProbe_ != nullptr) { enterProbe_(enterProbeArg_); }
#endif
    if ((prev & kClosedBit) != 0u) {
      state_.fetch_sub(1u, std::memory_order_release);
      return false;
    }
    return true;
  }

  /// Producer thread. RELEASE, paired with the acquire in quiesced(): everything
  /// this caller did inside the gate must be visible to the closer that observes
  /// a zero count. Without the pair, x86 TSO hides the bug on every machine a
  /// developer owns and TSAN finds it on the first run.
  void leave() noexcept { state_.fetch_sub(1u, std::memory_order_release); }

  /// JS thread, at teardown. Idempotent and PERMANENT -- there is no reopen, by
  /// design: a gate that could reopen would let a capture the user ended resume
  /// calling into JS. A poisoned process is killed, never recovered.
  void close() noexcept { state_.fetch_or(kClosedBit, std::memory_order_acq_rel); }

  /// True when nobody is inside. Says nothing about whether the gate is closed:
  /// an open, idle gate is quiesced too, and the caller that cares has just
  /// called close() itself.
  bool quiesced() const noexcept {
    return (state_.load(std::memory_order_acquire) & kCountMask) == 0u;
  }

  /// True once close() has been called. Diagnostics and assertions only -- the
  /// teardown path never needs to ask, because it is the thing that closed it.
  bool closed() const noexcept {
    return (state_.load(std::memory_order_acquire) & kClosedBit) != 0u;
  }

#ifdef CONCORD_AUDIOCAP_TEST_SEAM
  using EnterProbe = void (*)(void*);
  static inline EnterProbe enterProbe_ = nullptr;
  static inline void* enterProbeArg_ = nullptr;
  static void setEnterProbe(EnterProbe fn, void* arg) noexcept {
    enterProbe_ = fn;
    enterProbeArg_ = arg;
  }
#endif

 private:
  std::atomic<u32> state_;
};

}  // namespace rt
}  // namespace audiocap
}  // namespace concord

#endif  // CONCORD_AUDIOCAP_RT_SINK_GATE_H_
