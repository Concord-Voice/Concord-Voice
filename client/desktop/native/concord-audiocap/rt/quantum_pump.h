// concord-audiocap / rt — the quantum accumulator and the pump counters.
//
// JSF++ BINDS HERE. See [internal]rules/native-audio.md.
//
// WHAT THIS IS FOR.
// An OS audio callback does not deliver 480 frames. It delivers whatever the HAL
// chose — 1, 7, 512, 4096 — and it changes size while the capture is running.
// The seam downstream is fixed at exactly one 3872-byte quantum of 480 frames, so
// something has to regroup one into the other, and that something must run on the
// callback thread without allocating, blocking or throwing. This is it.
//
// WHY IT TAKES ITS RING AND ITS SIGNAL BY INJECTION.
// rt/ must never name N-API — the libFuzzer target cannot include node_api.h, and
// the whole reason ADR-0043 puts this code here rather than in napi/ is that it
// is reachable from a trust boundary and therefore must be fuzzable. The signal
// is a plain `void (*)(void*)`; napi/ passes one that calls
// napi_call_threadsafe_function, and a test passes one that counts.
//
// WHAT THE CALLER OWNS. The gate (rt/sink_gate.h) is NOT held here. The pump
// signals unconditionally and the injected signal function is what decides
// whether a call into JS is still allowed — that composition is what lets a
// post-stop submission reach the ring (where drainRing discards it) while being
// structurally unable to reach the threadsafe function.
//
// AV 206 (no allocation): the accumulator is a fixed member array and the ring's
// storage belongs to the caller. Nothing here allocates, at construction or after.
// AV 215 (no pointer arithmetic): every access is `array[index]`, and where a
// pointer is needed it is `&array[index]`.

#ifndef CONCORD_AUDIOCAP_RT_QUANTUM_PUMP_H_
#define CONCORD_AUDIOCAP_RT_QUANTUM_PUMP_H_

#include <atomic>
#include <cstddef>
#include <cstring>

#include "frame_pack.h"
#include "quantum_header.h"
#include "quantum_ring.h"

namespace concord {
namespace audiocap {
namespace rt {

/// Why a capture stopped producing. Mirrors AudioCapStatus.faultReason in
/// index.d.ts as a closed set; PR 1 declares the whole vocabulary even though
/// only a real backend can raise most of it, so the mapping on the JS side is
/// written once and asserted exhaustive.
enum class PumpFault : u8 {
  kNone = 0,
  kDeviceLost,
  kPermissionLost,
  kNoCallbacks,
  kFormatChanged
};

/// A snapshot, taken on the JS thread while the producer is still running. The
/// two COUNTERS are read with separate relaxed loads and are therefore NOT a
/// consistent instant — they are diagnostics, both are monotonic, and a torn
/// read of either is stale rather than wrong.
///
/// THE FAULT PAIR IS NOT LIKE THAT, because `faultReason` is not monotonic: it is
/// written once, before the flag that explains it. So it is READ IN THE OPPOSITE
/// ORDER TO THE WAY IT IS WRITTEN — `faulted` first with acquire, the reason
/// second — and that pairing is what closes `{faulted: true, faultReason:
/// "None"}`. Read flag-LAST, as this was until PR #3262, a reader can take the
/// reason from before the fault and then the flag from after it, and report that
/// something broke with nothing that broke it. The other order is harmless and
/// deliberately left open: `{faulted: false, faultReason: <something>}` is a
/// reader that arrived mid-write, and `faulted` is the load-bearing field, so it
/// reads as "no fault yet" rather than as a cause this pump never published.
struct PumpCounters {
  u32  callbackTotal;   // saturating; OS callbacks (or synthetic ticks)
  u32  quantaTotal;     // saturating; whole quanta actually PUSHED to the ring
  bool faulted;
  u8   faultReason;     // a PumpFault value, widened for the JS surface
};

class QuantumPump {
 public:
  /// Injected by napi/ so this header never names N-API. Called on the producer
  /// thread, once per quantum that was actually pushed.
  using SignalFn = void (*)(void*);

  /// Saturation ceiling, spelled as the ring's so the two cannot drift apart:
  /// the three counters a consumer sees (overrun, callbackTotal, quantaTotal)
  /// all stick at the same value, so "stuck" reads the same way in all three.
  static constexpr u32 kCounterSaturated = QuantumRing::kOverrunSaturated;

  QuantumPump(QuantumRing& ring, SignalFn signal, void* signalArg) noexcept
      : ring_(ring),
        signal_(signal),
        signalArg_(signalArg),
        partial_(),           // AV 142: the accumulator is zeroed before any use
        frameCursor_(0u),
        seq_(0u),
        quantumStartNs_(0u),
        callbackTotal_(0u),
        quantaTotal_(0u),
        activityTotal_(0u),
        faulted_(false),
        faultReason_(static_cast<u8>(PumpFault::kNone)) {}

  // Approved deviation (C++11 `= delete`): the pump holds a reference to a ring
  // it does not own, and a copy would give two pumps one seq counter.
  QuantumPump(const QuantumPump&)            = delete;
  QuantumPump& operator=(const QuantumPump&) = delete;

  /// Called on the JS thread at `start`, with no producer alive.
  ///
  /// Clears the ACCUMULATOR — the partial buffer, the frame cursor and seq — and
  /// nothing else. The counters are deliberately untouched, for the same reason
  /// the ring's overrun counter is never cleared: they are the post-mortem
  /// evidence for the life of the process, and the host forks one child per share
  /// (design section 5, Q3), so process lifetime is share lifetime. A fault
  /// latched before a reset stays latched.
  ///
  /// `nowNs` seeds the timestamp a quantum would carry if one were somehow
  /// completed before any callback supplied a clock reading; every real quantum
  /// takes its stamp from the callback that supplied its first frame.
  void reset(u64 nowNs) noexcept {
    std::memset(partial_, 0, static_cast<std::size_t>(kQuantumBytes));
    frameCursor_   = 0u;
    seq_           = 0u;
    quantumStartNs_ = nowNs;
  }

  /// One OS callback arrived. Separate from submit() because a callback that
  /// delivers nothing usable is still evidence the tap is alive, and "callbacks
  /// stopped" is the only starvation signal a native deadline can see.
  void noteCallback() noexcept {
    bumpSaturating(callbackTotal_);
    bumpSaturating(activityTotal_);
  }

  /// THE TEARDOWN WITNESS, and it is deliberately NOT callbackTotal.
  ///
  /// callbackTotal moves only when a backend VOLUNTEERS noteCallback(), which
  /// rt/capture_backend.h makes optional ("MAY call sink.submit()/noteCallback()")
  /// -- so a CONFORMING backend that submits and never notes leaves it still
  /// while its tap runs, and a teardown watching it takes the quiesced arm over a
  /// live producer. That is #3197 PoC-1 wearing a new name, and it is why this
  /// counter is bumped BY THE PUMP on every entry from the producer, including a
  /// submit() the pump then refuses: there is no conforming way to touch this
  /// sink without moving it.
  ///
  /// The two counters are not redundant. callbackTotal keeps its own meaning --
  /// "a callback that delivered nothing usable is still evidence the tap is
  /// alive", which is the only starvation signal a native deadline can see -- and
  /// this one answers the different question rt/teardown.h asks: did ANYTHING
  /// reach this sink after stop() returned.
  u32 activityTotal() const noexcept {
    return activityTotal_.load(std::memory_order_relaxed);
  }

  /// Accumulate one callback's frames, emitting every whole quantum they
  /// complete. Called on the audio thread; never blocks, never allocates.
  ///
  /// Returns false — having changed nothing — for a null plane array, a channel
  /// count outside 1..kChannels, or a zero frame count. AV 115: the result is
  /// tested by every caller, and a false here means the BACKEND passed something
  /// impossible, not that audio was lost.
  bool submit(const u8* const* srcPlanes, u16 srcChannels, bool interleaved,
              u32 frameCount, u64 timestampNs) noexcept {
    // BEFORE VALIDATION, and that order is the point: a submission this function
    // is about to REFUSE is still a producer that reached this sink, which is the
    // only thing the teardown witness is asked about. Counting it after the
    // guards would hand a backend a conforming way to be invisible.
    bumpSaturating(activityTotal_);
    if (srcPlanes == nullptr) { return false; }
    if (srcChannels == 0u || srcChannels > kChannels) { return false; }
    if (frameCount == 0u) { return false; }

    u32 consumed = 0u;
    while (consumed < frameCount) {
      if (frameCursor_ == 0u) {
        // THE STAMP IS THE FIRST FRAME'S, not the completing callback's. A
        // quantum held in the accumulator across two callbacks would otherwise
        // be stamped late by however long it waited, and the far end uses this
        // for AudioData.timestamp.
        quantumStartNs_ = timestampNs + framesToNs(consumed);
      }

      const u32 room = static_cast<u32>(kFrameCount) - frameCursor_;
      u32 take = frameCount - consumed;
      if (take > room) { take = room; }

      const std::size_t at =
          static_cast<std::size_t>(kHeaderBytes) +
          (static_cast<std::size_t>(frameCursor_) *
           static_cast<std::size_t>(kChannels) *
           static_cast<std::size_t>(kBytesPerSample));
      if (!packFrames(srcPlanes, srcChannels, interleaved, consumed, take,
                      &partial_[at])) {
        // A refusal here is a null plane the caller promised was there. Nothing
        // partial is published: the accumulator keeps whatever it already had
        // and this callback contributes nothing.
        return false;
      }

      frameCursor_ = frameCursor_ + take;
      consumed     = consumed + take;

      if (frameCursor_ == static_cast<u32>(kFrameCount)) {
        emit();
        frameCursor_ = 0u;
      }
    }
    return true;
  }

  /// Latch a fault. THE FIRST ONE WINS: a device loss that then surfaces as a
  /// format change must not overwrite the cause with its own consequence, and a
  /// post-mortem read wants the first thing that went wrong. kNone is a no-op, so
  /// this can never clear a latched fault.
  void fault(PumpFault reason) noexcept {
    if (reason == PumpFault::kNone) { return; }
    if (faulted_.load(std::memory_order_relaxed)) { return; }
    faultReason_.store(static_cast<u8>(reason), std::memory_order_relaxed);
    // Written LAST and RELEASED, so a reader that sees `faulted` has already been
    // able to see the reason that explains it. Relaxed on this store would make
    // that ordering a hope rather than a guarantee: the two are separate atomics
    // and nothing else orders them, so a relaxed pair is free to publish the flag
    // ahead of the reason on any machine that is not x86. counters() takes the
    // matching acquire.
    faulted_.store(true, std::memory_order_release);
  }

  /// Read from the JS thread, at any time, including after stop(). Two relaxed
  /// loads and one acquire; see the note on PumpCounters.
  PumpCounters counters() const noexcept {
    PumpCounters out = {0u, 0u, false, static_cast<u8>(PumpFault::kNone)};
    out.callbackTotal = callbackTotal_.load(std::memory_order_relaxed);
    out.quantaTotal   = quantaTotal_.load(std::memory_order_relaxed);
    // FLAG FIRST AND ACQUIRE, REASON SECOND. Paired with the release store in
    // fault(); swapping these two lines back reopens {true, "None"}.
    out.faulted     = faulted_.load(std::memory_order_acquire);
    out.faultReason = faultReason_.load(std::memory_order_relaxed);
    return out;
  }

 private:
  // The counter-saturation test seeds the ceiling rather than reaching it: at one
  // callback per 10 ms it is 497 days away. Declared here and DEFINED only in the
  // test translation unit, exactly as QuantumRing does it — a production
  // `setCounter()` would be a way to make the post-mortem evidence lie, and it
  // would exist in the shipped binary to serve a test.
  friend struct PumpCountersTestAccess;

  /// Nanoseconds spanned by `frames` at the pinned 48 kHz. Integer only: this
  /// file performs no floating-point arithmetic anywhere, which is what keeps
  /// AV 209 satisfied with no named deviation. 480 frames is exactly 10 ms, and
  /// the largest intermediate a plausible callback can produce (2^32 frames) is
  /// far inside u64.
  static u64 framesToNs(u32 frames) noexcept {
    return (static_cast<u64>(frames) * 1000000000ull) / static_cast<u64>(kSampleRate);
  }

  static void bumpSaturating(std::atomic<u32>& counter) noexcept {
    // A load/store pair rather than fetch_add, for the reason QuantumRing::push
    // gives: the producer is the only writer and the JS thread only reads, so a
    // read-modify-write buys nothing and costs an unbounded retry on a
    // real-time thread. SATURATING, because a wrapped counter reads as "nothing
    // ever happened", which is the one answer it must never be able to give.
    const u32 prior = counter.load(std::memory_order_relaxed);
    if (prior != kCounterSaturated) {
      counter.store(prior + 1u, std::memory_order_relaxed);
    }
  }

  /// Stamp the header over the accumulator's reserved 32 bytes and publish it.
  void emit() noexcept {
    const QuantumHeader header = {seq_, quantumStartNs_, ring_.overrun()};
    // AV 115: both results are tested. A refusal from the encoder means the
    // accumulator is the wrong size, which is a programming error rather than a
    // runtime condition, so nothing is pushed and nothing is signalled.
    if (encodeQuantumHeader(partial_, static_cast<std::size_t>(kQuantumBytes), header)) {
      if (ring_.push(partial_, kQuantumBytes) == RingResult::kOk) {
        bumpSaturating(quantaTotal_);
        if (signal_ != nullptr) { signal_(signalArg_); }
      }
      // kFull needs no branch. The ring counted the drop at the one drop site,
      // and a dropped quantum has nothing to announce.
    }
    // ADVANCED EVEN ON A DROP. The seq gap at the far end is the second,
    // independent drop observer, and it still moves if the counter ever lies.
    seq_ = seq_ + 1u;
  }

  QuantumRing&   ring_;
  const SignalFn signal_;
  void* const    signalArg_;

  // One whole quantum, header bytes reserved at the front so a completed quantum
  // is published as a single push with nothing to re-associate downstream.
  u8  partial_[kQuantumBytes];
  u32 frameCursor_;      // frames accumulated so far, 0..kFrameCount-1
  u32 seq_;              // wraps; a gap at the far end is a drop witness
  u64 quantumStartNs_;   // the clock reading of the frame at cursor 0

  std::atomic<u32> callbackTotal_;
  std::atomic<u32> quantaTotal_;
  // Never cleared by reset(), for the same reason the other two are not: it is
  // evidence for the life of the process, and the host forks one child per share.
  std::atomic<u32> activityTotal_;
  std::atomic<bool> faulted_;
  std::atomic<u8>  faultReason_;
};

}  // namespace rt
}  // namespace audiocap
}  // namespace concord

#endif  // CONCORD_AUDIOCAP_RT_QUANTUM_PUMP_H_
