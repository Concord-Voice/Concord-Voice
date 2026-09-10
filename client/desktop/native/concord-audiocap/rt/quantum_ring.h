// concord-audiocap / rt — single-producer single-consumer quantum ring.
//
// JSF++ BINDS HERE. See [internal]rules/native-audio.md for the enforceable list and
// ADR-0043 D2/D3 for why. The short version: this header runs on an OS audio
// callback thread, where an allocation is audible and an exception is unbounded
// latency.
//
// WHY SLOTS AND NOT BYTES.
// A byte-oriented ring has to split a write across the wrap point, which is where
// essentially every ring-buffer bug in the world lives. This one is a ring of
// FIXED-SIZE SLOTS: a producer claims one whole slot, a consumer releases one whole
// slot, and nothing is ever split. It costs a little tail padding on a short quantum
// and buys the removal of an entire defect class from the one component ADR-0043
// requires be fuzzed. That trade is the right way round for code reachable across a
// trust boundary.
//
// AV 215 (no pointer arithmetic): every access below is `storage_[index]`, and where
// a pointer is genuinely needed it is `&storage_[index]` — array subscripting then
// address-of, not a pointer being walked. That is the construct AV 215 names as the
// alternative to arithmetic, not an evasion of it.
//
// AV 206 (no allocation): storage is supplied by the caller and outlives this object.
// This class never allocates, at construction or after it.

#ifndef CONCORD_AUDIOCAP_RT_QUANTUM_RING_H_
#define CONCORD_AUDIOCAP_RT_QUANTUM_RING_H_

#include <atomic>
#include <cstddef>
#include <cstdint>
#include <cstring>

namespace concord {
namespace audiocap {
namespace rt {

// AV 209: no bare int/short/long/float/double.
using u8  = std::uint8_t;
using u32 = std::uint32_t;

// AV 208 forbids exceptions, so every failure crosses as an explicit value.
enum class RingResult : u8 {
  kOk      = 0,
  kFull    = 1,  // producer: no free slot. The caller drops; it must not block.
  kEmpty   = 2,  // consumer: nothing to read.
  kTooBig  = 3,  // payload exceeds slotBytes.
  kBadArg  = 4,  // null pointer, or a consumer buffer too small.
  kInvalid = 5   // the ring was constructed with unusable geometry.
};

class QuantumRing {
 public:
  // Bounded because lengths_ is a fixed array — AV 206 again. 64 slots at a 10 ms
  // quantum is 640 ms of buffer, far beyond the 8-quantum credit bound ADR-0043 D4c
  // sets for the MessagePort transport downstream.
  static constexpr u32 kMaxSlots = 64u;

  // The value overrun() sticks at. Spelled here rather than as UINT32_MAX at the
  // use site so the ceiling and the accessor cannot drift apart.
  static constexpr u32 kOverrunSaturated = 0xFFFFFFFFu;

  // slotCount must be a power of two in [2, kMaxSlots] so the wrap is a mask.
  // Anything else yields a ring whose valid() is false and whose every operation
  // returns kInvalid — it fails closed rather than half-working.
  // storageLen is REQUIRED, and the reason is worth stating rather than trusting.
  // Without it the constructor validated the GEOMETRY and never the BUFFER, so
  // valid() returned true for a ring whose caller under-allocated -- and push()
  // then wrote up to slotCount*slotBytes straight past the end. Nothing could
  // catch it: the test and the fuzzer both size via storageBytes(), so the fuzzer
  // structurally CANNOT reach the case. It would have first appeared when PR 5
  // wires the real producer, on the audio thread, in the one component ADR-0043
  // calls trust-boundary-reachable. valid() reading as "the ring checked itself"
  // is what made it a trap. Found by @security-reviewer on PR #3155.
  QuantumRing(u8* storage, std::size_t storageLen, u32 slotCount, u32 slotBytes) noexcept
      : storage_(storage),
        slotCount_(slotCount),
        slotBytes_(slotBytes),
        mask_(slotCount - 1u),
        valid_(false),
        writeIdx_(0u),
        readIdx_(0u),
        overrun_(0u),
        lengths_() {
    // AV 142: lengths_() above value-initializes the whole array before any use.
    // The last clause is a 32-bit BACKSTOP, and is deliberately NOT the fix for the
    // u32 wrap that slotOffset() documents -- that fix is the cast, and it makes
    // every geometry this class accepts correct on a 64-bit size_t. This clause
    // matters only where size_t is 32 bits, which is no platform the addon
    // currently builds for. It stays because storageBytes() is public: a caller on
    // some future 32-bit target could size a buffer from a product that wrapped,
    // and refusing the geometry is cheaper than discovering that as corruption.
    const bool offsetsRepresentable =
        (slotCount > 0u) && (static_cast<std::size_t>(slotBytes) <=
                             (SIZE_MAX / static_cast<std::size_t>(slotCount)));
    valid_ = (storage != nullptr) && (slotBytes > 0u) && (slotCount >= 2u) &&
             (slotCount <= kMaxSlots) && ((slotCount & (slotCount - 1u)) == 0u) &&
             offsetsRepresentable &&
             // Evaluated AFTER offsetsRepresentable deliberately: && short-circuits,
             // so storageBytes() is only called once its product is known not to
             // wrap. The other order computes the wrapped value to compare against.
             (storageLen >= storageBytes(slotCount, slotBytes));
  }

  // Approved deviation (C++11 `= delete`): a ring that owns a raw storage pointer
  // must not be copied, and saying so is stronger than a comment asking nicely.
  QuantumRing(const QuantumRing&)            = delete;
  QuantumRing& operator=(const QuantumRing&) = delete;

  bool valid()     const noexcept { return valid_; }
  u32  slotCount() const noexcept { return slotCount_; }
  u32  slotBytes() const noexcept { return slotBytes_; }

  // Producer side. Audio callback thread. Never blocks; a full ring is a DROP,
  // which ADR-0043 D4c establishes as the only back-pressure a real-time callback
  // may express.
  RingResult push(const u8* src, u32 bytes) noexcept {
    if (!valid_)              { return RingResult::kInvalid; }
    if (src == nullptr)       { return RingResult::kBadArg; }
    if (bytes > slotBytes_)   { return RingResult::kTooBig; }

    const u32 w = writeIdx_.load(std::memory_order_relaxed);
    const u32 r = readIdx_.load(std::memory_order_acquire);
    // Unsigned wraparound makes this correct across the u32 rollover; it is the
    // difference that matters, never the absolute values.
    if ((w - r) >= slotCount_) {
      // THE ONE DROP SITE (design section 4d). The transport downstream has no
      // second one: the consumer refusing to drain past its credit bound IS this
      // ring filling, because ringSlots == creditBound, so credit exhaustion and
      // ring overflow are the same event and are counted here exactly once.
      //
      // SATURATING, NOT WRAPPING. This counter is the drop WITNESS -- it is
      // stamped into every quantum header and surfaced in the UI -- and a wrapped
      // u32 reads as "no drops", which is the one answer it must never be able to
      // give. 2^32 drops is 497 days of a permanently full ring, so the ceiling is
      // unreachable in service and is seeded in the test rather than reached.
      //
      // A load/store pair rather than fetch_add or a CAS loop, and the reason is
      // the thread this runs on. push() is the PRODUCER side of an SPSC ring: the
      // producer is the only writer of this counter (the consumer only reads it),
      // so no other thread can interleave and a read-modify-write buys nothing. It
      // does cost something -- a CAS loop is lock-free but not wait-free, and this
      // executes inside an OS audio callback where an unbounded retry is a dropout.
      // A second producer would corrupt writeIdx_ long before it mattered here, so
      // the single-writer assumption is the class's, not this counter's.
      const u32 prior = overrun_.load(std::memory_order_relaxed);
      if (prior != kOverrunSaturated) {
        overrun_.store(prior + 1u, std::memory_order_relaxed);
      }
      return RingResult::kFull;
    }

    const u32         slot = w & mask_;
    const std::size_t base = slotOffset(slot, slotBytes_);
    if (bytes > 0u) {
      std::memcpy(&storage_[base], src, static_cast<std::size_t>(bytes));
    }
    lengths_[slot] = bytes;

    // Release: the payload and its length above must be visible to a consumer that
    // acquires this index. Without it the consumer can observe a published slot
    // whose bytes have not landed.
    writeIdx_.store(w + 1u, std::memory_order_release);
    return RingResult::kOk;
  }

  // Consumer side. Copies one whole slot out and frees it.
  RingResult pop(u8* dst, u32 dstCapacity, u32* outBytes) noexcept {
    if (!valid_)                            { return RingResult::kInvalid; }
    if (dst == nullptr || outBytes == nullptr) { return RingResult::kBadArg; }

    *outBytes = 0u;
    const u32 r = readIdx_.load(std::memory_order_relaxed);
    const u32 w = writeIdx_.load(std::memory_order_acquire);
    if (w == r) { return RingResult::kEmpty; }

    const u32 slot = r & mask_;
    const u32 len  = lengths_[slot];
    // A length that cannot fit is reported rather than truncated, and the slot is
    // NOT consumed — a silent short read is the failure shape this whole design is
    // built to avoid.
    if (len > dstCapacity) { return RingResult::kBadArg; }

    const std::size_t base = slotOffset(slot, slotBytes_);
    if (len > 0u) {
      std::memcpy(dst, &storage_[base], static_cast<std::size_t>(len));
    }
    *outBytes = len;

    readIdx_.store(r + 1u, std::memory_order_release);
    return RingResult::kOk;
  }

  // Pushes refused because the ring was full, saturating at kOverrunSaturated.
  // ADR-0043 D4c requires this be OBSERVABLE — a bound that fails as quietly as no
  // bound buys nothing.
  //
  // Named `overrun` and not `dropped` because it is the same number the wire
  // header calls `overrunTotal` and the store calls `screenAudio.overrun`. One
  // event, one counter, one name the whole way across: there is nothing to
  // reconcile between the native drop site and the transport, and a second name
  // would invite a second counter.
  u32 overrun() const noexcept { return overrun_.load(std::memory_order_relaxed); }

#ifdef CONCORD_AUDIOCAP_TEST_SEAM
  using DepthProbe = void (*)(void*);
  static inline DepthProbe depthProbe_ = nullptr;
  static inline void* depthProbeArg_ = nullptr;
  static void setDepthProbe(DepthProbe fn, void* arg) noexcept {
    depthProbe_ = fn;
    depthProbeArg_ = arg;
  }
#endif

  // APPROXIMATE UNDER CONCURRENCY, and deliberately so -- read the bound before
  // using it for anything but diagnostics.
  //
  // Two separate atomic loads cannot be one instant. readIdx_ IS LOADED FIRST, and
  // the order is the correctness argument: loading writeIdx_ first lets the producer
  // push and the consumer drain that same item between the two loads, so the
  // observed r exceeds the stale w and `w - r` UNDERFLOWS to roughly 2^32. Reading r
  // first means the later w is at or ahead of it, so the result is an OVER-estimate
  // instead. Found by CodeRabbit on PR #3155.
  //
  // The over-estimate is NOT bounded by slotCount, and an earlier version of the
  // test asserted that it was -- wrongly. It is
  // `true_depth + (pops between the two loads)`, so a fast consumer widens it. What
  // the order buys is that the answer is never WILD: an over-estimate stays in the
  // small-integer range while an underflow lands near 2^32, and those are trivially
  // distinguishable. Use this for a log line or a fault counter, never to decide
  // whether a slot is free -- push() and pop() read the indices themselves.
  u32 depth() const noexcept {
    const u32 r = readIdx_.load(std::memory_order_acquire);
#ifdef CONCORD_AUDIOCAP_TEST_SEAM
    // TEST-ONLY, and absent from every production translation unit: nothing under
    // binding.gyp defines this macro, so the released addon compiles the two loads
    // back to back exactly as before.
    //
    // It exists because the load order cannot be tested by RACING for it. Two
    // threads plus an observer make the required interleaving merely probable, and
    // Codex measured what that costs: an exact reversed-load-order mutant survived
    // 5 of 100 normal runs and 37 of 50 single-CPU runs. A mutation kill that
    // depends on the scheduler is not a kill. This seam lets one test force the
    // read index to advance BETWEEN the two loads, which is the whole property,
    // and turns a probabilistic check into a deterministic one. Found by Codex on
    // PR #3188.
    if (depthProbe_ != nullptr) { depthProbe_(depthProbeArg_); }
#endif
    const u32 w = writeIdx_.load(std::memory_order_acquire);
    return w - r;
  }

  // Bytes of caller-supplied storage a given geometry needs. Callers size their
  // buffer with this rather than recomputing the product and getting it wrong.
  static constexpr std::size_t storageBytes(u32 slotCount, u32 slotBytes) noexcept {
    return static_cast<std::size_t>(slotCount) * static_cast<std::size_t>(slotBytes);
  }

  // Byte offset of a slot. THE CAST IS THE FIX, not decoration: this product was
  // computed in u32 and wrapped. A four-slot ring with slotBytes 0x80000000 passed
  // valid(), and slot 2 computed base 0 -- aliasing slot 0 and corrupting it, while
  // storageBytes() above returned the correct 8 GiB because it already cast. The
  // class asked for the right allocation and then wrote outside its own slot.
  //
  // Public and static so the arithmetic is testable at geometries whose storage
  // nobody wants to allocate: exposing 8 GiB of real memory to a unit test to check
  // a multiplication is the wrong trade.
  static constexpr std::size_t slotOffset(u32 slot, u32 slotBytes) noexcept {
    return static_cast<std::size_t>(slot) * static_cast<std::size_t>(slotBytes);
  }

 private:
  // Test seam. The u32 rollover that push()/pop() rely on cannot be reached by
  // pushing 2^32 times, so the rollover test advances the counters directly rather
  // than asserting against a hand-copy of the arithmetic — which would test the
  // test's model of the class instead of the class. Nothing in production names
  // this type; it is declared only in the test translation unit.
  friend struct QuantumRingRolloverAccess;

  u8* const storage_;
  const u32 slotCount_;
  const u32 slotBytes_;
  const u32 mask_;
  bool      valid_;

  std::atomic<u32> writeIdx_;
  std::atomic<u32> readIdx_;
  std::atomic<u32> overrun_;

  u32 lengths_[kMaxSlots];
};

}  // namespace rt
}  // namespace audiocap
}  // namespace concord

#endif  // CONCORD_AUDIOCAP_RT_QUANTUM_RING_H_
