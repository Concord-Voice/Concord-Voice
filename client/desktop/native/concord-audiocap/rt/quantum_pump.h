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
  /// Saturating; callbacks that admitted AT LEAST ONE non-zero sample.
  ///
  /// THE COUNTER STATE B FORCED. A callback deadline cannot see consent denial:
  /// measured, a denied Core Audio tap delivers ~94 correctly-shaped callbacks a
  /// second, every API returning noErr, every sample zero (design section 9.0).
  /// So "callbacks are arriving" and "audio is arriving" became different
  /// questions, and callbackTotal can only answer the first.
  u32  signalTotal;
  /// One-shot: callbacks arrived, signalTotal stayed 0, and the budget elapsed.
  ///
  /// ADVISORY ONLY. It never faults, never tears down, never mutes. A granted tap
  /// on a PAUSED app is byte-identical to a denied one -- also measured -- so
  /// nothing at this seam can tell them apart and no consumer may act as if it
  /// could. Cleared permanently by the first non-zero sample, so a share that
  /// goes quiet later is never accused.
  bool silentSinceStart;
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

  /// How long a capture may deliver callbacks with nothing audible in them
  /// before silentSinceStart latches. 10 s is 1000 quantum periods: long enough
  /// that ordinary lead-in silence -- a pre-roll, an unstarted video, a game
  /// sitting in a menu -- does not trip it, short enough that a denied share is
  /// reported while the user is still in the act of starting it.
  ///
  /// THE FALSE POSITIVE IT CANNOT AVOID, NAMED: a healthy consented share that is
  /// genuinely silent for its first 10 s. Nothing at this seam can distinguish
  /// that from denial, ever. Affordable for an advisory and for nothing else.
  static constexpr u64 kSilenceBudgetNs = 10000000000ull;

  QuantumPump(QuantumRing& ring, SignalFn signal, void* signalArg) noexcept
      : ring_(ring),
        signal_(signal),
        signalArg_(signalArg),
        partial_(),           // AV 142: the accumulator is zeroed before any use
        frameCursor_(0u),
        seq_(0u),
        quantumStartNs_(0u),
        captureStartNs_(0u),
        callbackTotal_(0u),
        quantaTotal_(0u),
        activityTotal_(0u),
        signalTotal_(0u),
        silentLatched_(false),
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
    // The budget in kSilenceBudgetNs is measured from HERE, not from the first
    // callback: a tap that is denied may still take a moment to deliver anything,
    // and starting the clock at the first delivery would move the deadline by
    // exactly the interval the detector is trying to observe.
    captureStartNs_ = nowNs;
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

    // THE BUDGET IS EVALUATED BEFORE THE GUARDS, for exactly the reason
    // activityTotal_ is counted before them. It used to sit at the end of the
    // success path, behind all three early returns -- so a tap delivering only
    // DEGENERATE callbacks could never latch, and that is not a hypothetical
    // shape: tap_backend.mm produces it at two sites, for a null or empty
    // AudioBufferList and for a null or zero-sized buffer 0. A tap that is alive
    // and starved is precisely what this latch exists to name, and it was the
    // one starvation shape the latch could not see.
    noteSilenceDeadline(timestampNs);

    if (srcPlanes == nullptr) { return false; }
    if (srcChannels == 0u || srcChannels > kChannels) { return false; }
    if (frameCount == 0u) { return false; }

    // Accumulated across the whole callback rather than bumped per quantum.
    // PumpCounters::signalTotal is documented as counting CALLBACKS that
    // admitted a non-zero sample, and one callback whose frames straddle a
    // quantum boundary runs this loop more than once -- so the per-quantum bump
    // could report more signal-bearing callbacks than callbackTotal saw.
    bool callbackHasSignal = false;
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

      // Scans ONLY the bytes just admitted, on a pass that already touched them:
      // 960 samples per 10 ms quantum, roughly 96k integer compares a second on
      // the audio thread, no allocation and no branch of consequence.
      // Scans from `at`, the offset just written, NOT from the head of the
      // accumulator: on a callback that completes a partially-filled quantum
      // the two differ, and scanning from the head would re-read already-scanned
      // silence while missing the slice this iteration admitted.
      if (anyNonZeroSample(&partial_[at],
                           take * static_cast<u32>(kChannels))) {
        callbackHasSignal = true;
      }

      frameCursor_ = frameCursor_ + take;
      consumed     = consumed + take;

      if (frameCursor_ == static_cast<u32>(kFrameCount)) {
        emit();
        frameCursor_ = 0u;
      }
    }

    if (callbackHasSignal) { bumpSaturating(signalTotal_); }
    return true;
  }

  /// NATIVE DEADLINE, NATIVE EVALUATION. #2992: a signal crossing a dispatcher
  /// inherits the dispatcher's lateness, so main must never compute this.
  ///
  /// BACKEND OBLIGATION: timestampNs must be in the SAME MONOTONIC DOMAIN as
  /// HostServices::nowNs, not a device clock, or the budget is measured in the
  /// wrong unit and the latch fires early or never. A backend that cannot read
  /// its clock passes 0, which is before every budget and therefore never
  /// latches -- the safe direction, since this latch's only failure mode that
  /// matters is claiming silence that is not there.
  ///
  /// EQUIVALENT MUTANT, recorded the way tap_floor.h records its own: removing
  /// the `signalTotal_ == 0` conjunct changes nothing observable, because
  /// signalTotal_ is monotonic and counters() already ANDs the latch with it.
  /// It is kept as the cheaper of the two tests and because the latch reads
  /// correctly on its own here, not because a test pins it.
  void noteSilenceDeadline(u64 timestampNs) noexcept {
    if (signalTotal_.load(std::memory_order_relaxed) == 0u &&
        timestampNs >= captureStartNs_ + kSilenceBudgetNs) {
      silentLatched_.store(true, std::memory_order_relaxed);
    }
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
    PumpCounters out = {0u, 0u, 0u, false, false, static_cast<u8>(PumpFault::kNone)};
    out.callbackTotal = callbackTotal_.load(std::memory_order_relaxed);
    out.quantaTotal   = quantaTotal_.load(std::memory_order_relaxed);
    out.signalTotal   = signalTotal_.load(std::memory_order_relaxed);
    // ANDed rather than reported raw. The latch has ONE writer (the producer)
    // and this is what lets a late first sample clear the reported value without
    // a second writer racing the first -- a store from this thread would be
    // exactly the extra writer the SPSC assumption forbids.
    out.silentSinceStart =
        (out.signalTotal == 0u) && silentLatched_.load(std::memory_order_relaxed);
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

  /// True if ANY 4-byte element is a non-zero binary32.
  ///
  /// AN INTEGER BIT-PATTERN TEST, NOT A FLOAT COMPARE. A binary32 zero is
  /// 0x00000000 or 0x80000000 and nothing else, so this decides "is there
  /// signal" while performing NO floating-point arithmetic -- which is what
  /// keeps rt/ inside AV 209 with no named deviation (design section 7.2).
  ///
  /// memcpy rather than a cast: reading through a u32* aimed at float storage is
  /// a strict-aliasing violation UBSAN is entitled to report, and the ASAN/UBSAN
  /// leg would find it on the first run.
  ///
  /// EXACT ZERO, NO THRESHOLD. The measurement behind this is nonZeroSamples == 0
  /// across every denied run, not "small". A threshold would need floating point
  /// and would be a tuning knob with no measurement behind it; a quiet-but-real
  /// source carrying dither is non-zero and correctly reads as signal.
  static bool anyNonZeroSample(const u8* bytes, u32 sampleCount) noexcept {
    for (u32 i = 0u; i < sampleCount; ++i) {
      u32 bits = 0u;
      std::memcpy(&bits, &bytes[static_cast<std::size_t>(i) * 4u], sizeof(bits));
      if (bits != 0x00000000u && bits != 0x80000000u) { return true; }
    }
    return false;
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
  u64 captureStartNs_;   // producer-thread only; seeded by reset()

  std::atomic<u32> callbackTotal_;
  std::atomic<u32> quantaTotal_;
  // Never cleared by reset(), for the same reason the other two are not: it is
  // evidence for the life of the process, and the host forks one child per share.
  std::atomic<u32> activityTotal_;
  std::atomic<u32> signalTotal_;
  // Latched by submit(); READ THROUGH counters(), which ANDs it with
  // signalTotal_ == 0 so a late first sample clears the reported value.
  std::atomic<bool> silentLatched_;
  std::atomic<bool> faulted_;
  std::atomic<u8>  faultReason_;
};

}  // namespace rt
}  // namespace audiocap
}  // namespace concord

#endif  // CONCORD_AUDIOCAP_RT_QUANTUM_PUMP_H_
