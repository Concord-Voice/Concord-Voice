// concord-audiocap / rt — the teardown sequence, as one routine.
//
// JSF++ BINDS HERE. See [internal]rules/native-audio.md.
//
// WHY THIS IS NOT SIMPLY THE BODY OF stopCapture().
// The five steps of design section 4.2 ARE this epic's privacy invariant: the OS
// artefact is destroyed BEFORE anything waits on bytes, and a threadsafe function
// a live callback may still call is ABANDONED rather than released. Written
// inline in napi/addon.cc that order is unassertable — that file includes
// node_api.h, which no test or fuzz target in this package can compile, so the
// one ordering the whole PR exists to fix would ship with no automated witness.
// Written here it is exercised by the same Linux ASAN/UBSAN/TSAN rows that cover
// the ring, against fakes that stand in for the backend and the handle.
//
// THE HOOKS ARE INJECTED for the reason QuantumPump takes its signal and
// CaptureBackend takes its HostServices: rt/ must never name N-API, uv, or a
// clock (AV 22/25). napi/ passes hooks that stop the real backend, sleep on uv,
// read uv's monotonic clock, release the real handle and read the real pump's
// ACTIVITY counter; the contract suite passes hooks that record.
//
// WHAT THIS ROUTINE DELIBERATELY DOES NOT DO. It never reopens the gate, never
// clears `poisoned`, and never releases the handle on the timeout branch. Each of
// those would be a use-after-release wearing a convenience: see design section
// 4.3 for why abandoning one handle in a process that is about to be killed is
// the cheap side of that trade.
//
// AV 206 (no allocation): six function pointers, one context pointer and a bool.

#ifndef CONCORD_AUDIOCAP_RT_TEARDOWN_H_
#define CONCORD_AUDIOCAP_RT_TEARDOWN_H_

#include <atomic>

#include "quantum_header.h"
#include "sink_gate.h"

namespace concord {
namespace audiocap {
namespace rt {

/// How long the producer must be OBSERVED not to move, in milliseconds.
///
/// THIS REPLACES AN UNBLOCKED SPIN that answered the ordinary stop without ever
/// sleeping, and the reason it is gone is that there is no longer such a thing as
/// a free answer. quiesced() proves "nobody was inside the gate at the instant I
/// sampled", which is NOT "no producer exists": a backend that breaks its stop()
/// post-condition spends microseconds per 10 ms inside the gate, so the FIRST
/// sample reads zero and a teardown that returned on it takes the clean arm over
/// a live producer. That is #3197 PoC-1, measured — session-1 audio delivered to
/// session 2's consumer through a gate re-armed over a producer nobody stopped.
///
/// So the wait stops sampling the gate and starts WATCHING THE PRODUCER. The
/// witness is a counter only the producer moves (napi/ passes the pump's
/// activityTotal), and the window is 50 ms because that is five quantum periods —
/// the same number, chosen for the same reason, as the "measured over 50 ms of
/// wall time" the post-condition suite in test/rt_contract_test.cc already uses
/// to decide that a stopped backend really stopped.
///
/// IT IS EVIDENCE, NOT PROOF, and that distinction decides what may be built on
/// it: a producer that is alive but idle for the whole window is indistinguishable
/// from one that is gone. So it may REFUSE (abandon the handle, latch `poisoned`)
/// and it may never ADMIT. Nothing re-arms on the strength of it — napi/addon.cc
/// arms its gate once per process and never replaces it.
///
/// WHAT IT STILL CANNOT SEE, stated here because the honest bound is part of the
/// contract: a producer that is PARKED for the whole window — descheduled, waiting
/// on a lock, or deliberately sitting it out — is indistinguishable from one that
/// is gone, and no finite window fixes that, because widening it only names a
/// longer nap. The seam therefore bounds the HARM rather than proving absence:
/// sink_gate.h is closed before this wait and is never reopened, so a producer
/// that resumes afterwards cannot reach the consumer or the released handle. What
/// remains open is the TAP'S EXISTENCE, which only the backend can end — see the
/// stop() post-condition in rt/capture_backend.h, where that obligation now
/// states itself, and the sleeps-then-resumes case in the contract suite, which
/// pins exactly this residual rather than leaving it described.
///
/// The cost is one settle window per stop, paid by a child the host is about to
/// kill. The previous cost was zero and the previous answer was wrong.
constexpr u32 kSettleObserveMs = 50u;

/// THE BUDGET MUST EXCEED THE WINDOW, asserted here because the two constants live
/// in different headers — the budget with the gate it bounds (rt/sink_gate.h), the
/// window with the wait that spends it — and nothing else makes them look at each
/// other. A budget edited to or below 50 ms turns EVERY ordinary stop into a full
/// budget spent, a refused release and a poisoned process, which reads downstream
/// as a backend that would not quiesce. That is one line away and no test names
/// both numbers, so the compiler is asked instead.
///
/// It does NOT forbid a short budget at the CALL: awaitSettled documents that a
/// budget below the window can only return false, and the expiry cases in the
/// contract suite pass exactly such a budget on purpose. What is pinned here is
/// the SHIPPED pair.
static_assert(kQuiesceBudgetMs > kSettleObserveMs,
              "the quiesce budget must exceed the settle window, or every ordinary "
              "stop spends the whole budget and poisons the process");

/// The value a saturating u32 witness sticks at. A witness sitting here has
/// stopped BEING a witness: it reads identical for a stopped producer and for one
/// calling into the sink as fast as a core will let it, so "it did not move" and
/// "it CANNOT move" would reach the same arm. Reaching it is not the 497 days an
/// honest 10 ms cadence implies — measured at 9.6 s of one thread submitting in a
/// loop, a cadence rt/capture_backend.h permits and places no limit on — so it
/// is treated as "cannot be shown", exactly as a null hook is.
constexpr u32 kWitnessSaturated = 0xFFFFFFFFu;

/// Nanoseconds per millisecond. Spelled once so the two conversions below cannot
/// drift apart.
constexpr u64 kNsPerMs = 1000000ull;

/// A bound on the observation loop that does not depend on the clock being sane.
/// Every exit below is a TIME comparison, so a nowNs hook that returned a constant
/// would spin forever; this turns that into the same refusal every other unusable
/// hook gets, and keeps the loop bounded by something the compiler can see.
///
/// It is NOT a second budget. A legitimate wait is ~250 iterations at the shipped
/// budget and one-millisecond yields, ~16 on a host whose millisecond is 15.6 ms,
/// and at most ~500 on one that grants less than it was asked for — two orders of
/// magnitude inside this, so it can never end a healthy wait early. The honest
/// cost of the case it does catch is one yield per iteration: a STUCK clock makes
/// this stop take ~65 s rather than hang, which is the right trade only because a
/// stuck clock is a programming error in the hook and not a runtime condition —
/// napi/ passes uv_hrtime, and a null hook is refused above before this is
/// reached.
constexpr u32 kMaxSettleIterations = 65536u;

enum class TeardownOutcome : u8 {
  kQuiesced = 0,   // the sink emptied AND the producer stopped: handle released
  kAbandoned       // one of the two could not be shown: handle abandoned, `poisoned` latched
};

/// The six things teardown must do that rt/ cannot name for itself. Every one is
/// required; a null hook is a programming error rather than a runtime condition,
/// and is skipped rather than dereferenced for the same reason QuantumPump tests
/// its signal pointer — a null here must not be a crash on the path that ends a
/// user's share.
///
/// FOUR OF THEM ARE EXCEPTIONS TO THAT SKIPPING RULE. Only `releaseSignal` and
/// `drain` may be skipped: a null there degrades one step and cannot turn a
/// refusal into an admission.
///
/// The other four fail closed. `producerActivity` is the only evidence the
/// release-or-abandon decision rests on; `sleepMs` turns 50 ms of observation into
/// 50 unsynchronised loads without it, which is the single sample #3197 PoC-1
/// defeated; `nowNs` is the unit both the window and the budget are measured in;
/// and `stopBackend` — added to this list on PR #3262 — is THE ONE HOOK THAT
/// DESTROYS THE OS ARTEFACT. Skipping it was the asymmetry that mattered most:
/// the routine would then close a gate, watch a producer nobody had asked to
/// stop, find it idle, release the handle and report kQuiesced over a LIVE TAP.
/// Every other refusal in this file exists to prevent exactly that outcome
/// reached by a different route, so the permissive reading was reserved for the
/// only step whose absence guarantees it.
///
/// All four are treated as "cannot be shown" rather than as "nothing to do" — see
/// awaitSettled and step 2 of run().
struct TeardownOps {
  void  (*stopBackend)(void* ctx) noexcept;    // step 2: the OS artefact dies here
  void  (*sleepMs)(void* ctx, u32 ms) noexcept;// step 3: one millisecond of the budget
  void  (*releaseSignal)(void* ctx) noexcept;  // step 4, QUIESCED ARM ONLY
  void  (*drain)(void* ctx) noexcept;          // step 5: the ring is emptied last
  /// A monotone counter that ONLY THE PRODUCER MOVES, read on the JS thread. Its
  /// absolute value is meaningless here; the only question asked of it is whether
  /// it is the same number it was when stop() returned.
  u32   (*producerActivity)(void* ctx) noexcept;
  /// A MONOTONE nanosecond clock, read on the JS thread — the same shape and the
  /// same reason as HostServices::nowNs, because rt/ may not name a clock (AV 25)
  /// and the window it is asked about is 50 ms of WALL TIME. Counting loop
  /// iterations instead was only ever correct where a one-millisecond yield takes
  /// one millisecond: on Windows, whose default timer resolution is ~15.6 ms and
  /// whose backend (#3196) inherits this contract, it stretched the window to
  /// ~780 ms inside a nominal 250 ms budget — so the window over-observed by 15x
  /// and the budget stopped being a ceiling at all. Only the DIFFERENCE of two
  /// readings is used; the epoch is the hook's own business.
  u64   (*nowNs)(void* ctx) noexcept;
  void*  ctx;
};

/// Bounded wait for the sink to empty AND the producer to stop moving. True means
/// the handle is safe to release; false means the caller MUST abandon it.
///
/// TWO CONDITIONS, AND THE SECOND IS THE ONE #3197 ADDED:
///
///   1. The gate reads zero. The gate is already closed when this is called, so
///      anything that could still reach the handle is already counted
///      (sink_gate.h's increment-before-test order), which is what makes an
///      unsynchronised acquire poll correct for handle hygiene.
///   2. The activity witness has not moved for kSettleObserveMs. A witness that
///      ADVANCES after stop() returned is a backend that broke its post-condition,
///      and it is reported at once rather than waited out — there is nothing to
///      wait for.
///
/// An occupied gate RESTARTS the observation rather than merely delaying it: the
/// window must be one unbroken stretch of "empty and still", because a producer
/// that alternates is exactly the one the single sample missed.
///
/// BOTH THE WINDOW AND THE BUDGET ARE WALL TIME, taken from `nowNs`. The loop is
/// bounded by iterations only as a backstop against a clock that does not move;
/// how many times it goes round is a property of the host's yield granularity and
/// is deliberately not a number this file knows.
///
/// A budget below kSettleObserveMs can only ever return false. That is deliberate
/// and it fails in the safe direction: the short-budget caller is a test of the
/// expiry arm, and abandoning a handle is the arm that costs one leaked handle in
/// a process that is about to be killed.
inline bool awaitSettled(const SinkGate& gate, const TeardownOps& ops,
                         u32 budgetMs, u32 baseline) noexcept {
  // FAIL CLOSED WITHOUT A WITNESS, A YIELD OR A CLOCK. Not "skip the check": the
  // check is the decision, and the answer to "can I show the producer stopped" is
  // no. See the exceptions paragraph on TeardownOps for why these three and not
  // the other three.
  if (ops.producerActivity == nullptr) { return false; }
  if (ops.sleepMs == nullptr) { return false; }
  if (ops.nowNs == nullptr) { return false; }

  // FAIL CLOSED ON A SATURATED WITNESS, tested at the baseline because that is
  // where the comparison gets its meaning: at the ceiling `activity == baseline`
  // is what a motionless producer and a maximally busy one BOTH look like.
  if (baseline == kWitnessSaturated) { return false; }

  const u64 startNs  = ops.nowNs(ops.ctx);
  const u64 budgetNs = static_cast<u64>(budgetMs) * kNsPerMs;
  const u64 settleNs = static_cast<u64>(kSettleObserveMs) * kNsPerMs;
  // The start of the current unbroken stretch of "empty and still".
  u64 stableSinceNs = startNs;

  for (u32 i = 0u; i < kMaxSettleIterations; ++i) {
    const u64 now = ops.nowNs(ops.ctx);
    // A CLOCK THAT WENT BACKWARDS cannot be subtracted safely — unsigned
    // arithmetic would turn the step back into an enormous elapsed time and
    // report a window that was never observed. The hook is documented monotone;
    // one that is not gets the same answer every other unusable hook gets.
    if (now < stableSinceNs) { return false; }
    if (ops.producerActivity(ops.ctx) != baseline) { return false; }
    if (!gate.quiesced()) {
      stableSinceNs = now;
    } else if ((now - stableSinceNs) >= settleNs) {
      return true;
    }
    // The budget is checked AFTER the settle test and BEFORE the yield, so a
    // window that completed on the last admissible sample is still admitted and
    // an expired budget never pays for one more sleep.
    if ((now - startNs) >= budgetNs) { return false; }
    ops.sleepMs(ops.ctx, 1u);
  }
  return false;
}

/// The teardown of design section 4.2, and the `poisoned` latch its timeout arm
/// sets. JS-THREAD-ONLY: every field here is written by the thread that called
/// stop(), and the producer never sees this object at all.
class Teardown {
 public:
  Teardown() noexcept : poisoned_(false) {}

  // Approved deviation (C++11 `= delete`): the latch is the identity of one
  // process's capture ability, and a copy would be a second answer to "may this
  // process capture again".
  Teardown(const Teardown&)            = delete;
  Teardown& operator=(const Teardown&) = delete;

  /// True once a teardown failed to quiesce. LATCHED FOR THE LIFE OF THE PROCESS:
  /// a later clean teardown does not clear it, because the handle abandoned by
  /// the earlier one is still reachable by whatever was inside the gate. A
  /// poisoned process is killed, never recovered (design section 4.3).
  bool poisoned() const noexcept { return poisoned_; }

  /// Steps 1-5, synchronously, in this order, with nothing between them.
  ///
  /// STEP 2 COMES BEFORE THE GATE WAIT and that is the load-bearing line of the
  /// whole file: the invariant this epic is about is that the TAP STOPS EXISTING
  /// when the share ends, not that bytes stop moving. Waiting first would leave a
  /// live tap for the length of the budget on every ordinary stop, and for
  /// unbounded time on a backend that never quiesces — which is precisely the
  /// case the budget exists for.
  TeardownOutcome run(std::atomic<bool>& running, SinkGate& gate,
                      const TeardownOps& ops, u32 budgetMs) noexcept {
    // 1. Every reader that gates on `running` fails closed from this instant.
    running.store(false, std::memory_order_release);

    // 2. The OS artefact dies here. A backend's stop() is a courtesy — the gate
    //    below is the enforcement — but it is the ONLY thing that destroys a tap,
    //    so a NULL HOOK IS A REFUSAL and not a skipped step: without it nothing
    //    in this process has asked the tap to end, and no amount of watching an
    //    idle producer may be allowed to report that it did. The short-circuit
    //    below is what carries that: the wait is not even entered, so this arm
    //    costs no budget and reads exactly like every other unusable hook.
    const bool backendStopped = (ops.stopBackend != nullptr);
    if (backendStopped) { ops.stopBackend(ops.ctx); }

    // 2b. THE BASELINE, sampled the instant stop() RETURNED, which is the instant
    //    the post-condition takes effect. Every later movement of this number is a
    //    call the backend promised would never happen.
    const u32 baseline =
        (ops.producerActivity != nullptr) ? ops.producerActivity(ops.ctx) : 0u;

    // 3. No new caller can reach the handle from here; the wait is about the
    //    callers already inside AND about whether the producer really stopped.
    gate.close();
    const bool quiesced =
        backendStopped && awaitSettled(gate, ops, budgetMs, baseline);

    // 4. Release ONLY when both were shown. Otherwise the handle is abandoned —
    //    not released, not nulled — because something still inside the gate may
    //    call it, and a released handle called from a foreign thread is the
    //    use-after-release the gate exists to prevent. A producer that outlived
    //    its own stop() reaches the same arm: the process is poisoned, and the
    //    host kills a poisoned child, which is the only thing left that can
    //    destroy a tap its backend refused to destroy.
    //
    // THE LATCH IS READ HERE AND NOT ONLY WRITTEN, which is what makes a SECOND
    // run safe. stop() is documented idempotent and napi/addon.cc now reaches
    // this routine from three places — stop(), the env cleanup hook, and the
    // unwind of a start that armed and then failed — so two runs in one process
    // is ordinary. Without this test the second one is the dangerous one: the
    // producer the first run could not account for is typically gone by then, so
    // the second sees a quiet gate and a still witness and RELEASES the handle
    // the first deliberately abandoned. That is the use-after-release the abandon
    // arm exists to prevent, reached by calling stop() twice.
    if (quiesced && !poisoned_) {
      if (ops.releaseSignal != nullptr) { ops.releaseSignal(ops.ctx); }
    } else {
      poisoned_ = true;
    }

    // 5. LAST, and unconditional. A share that ended must not leave 80 ms of
    //    captured audio in the ring for the next session to find, and that is as
    //    true on the abandoned path as on the clean one.
    if (ops.drain != nullptr) { ops.drain(ops.ctx); }

    // The OUTCOME describes what happened to the handle, not what the wait saw:
    // a run that quiesced into an already-poisoned process abandoned it too.
    return poisoned_ ? TeardownOutcome::kAbandoned : TeardownOutcome::kQuiesced;
  }

 private:
  bool poisoned_;
};

}  // namespace rt
}  // namespace audiocap
}  // namespace concord

#endif  // CONCORD_AUDIOCAP_RT_TEARDOWN_H_
