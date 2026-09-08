// concord-audiocap / rt — QuantumRing tests.
//
// A CHECK macro, no framework. This binary is built by CI under ASAN, UBSAN and
// TSAN (ADR-0043 § Verification: "a sanitizer failure is a merge blocker"), and a
// test framework would only add a dependency to a target whose whole job is to be
// run under a sanitizer.
//
// This file is test code and is NOT bound by the JSF++ profile — that profile
// governs rt/, where an allocation is audible. Asserting is what this file is for.

#include "../rt/quantum_ring.h"

// EVERY include below is here for a symbol this file names directly, and <cstdlib>
// in particular is not optional: CHECK calls std::abort(), which libc++ on macOS
// supplies transitively and libstdc++ on the CI runners does not. A local macOS
// compile is therefore NOT sufficient verification for this file.
#include <cstdio>   // std::printf, std::fprintf, stderr
#include <cstdlib>  // std::abort
#include <cstring>  // std::memcmp
#include <atomic>
#include <thread>
#include <vector>

namespace concord {
namespace audiocap {
namespace rt {

// Grants the rollover test access to the private counters. Declared as a friend by
// quantum_ring.h; defined only here.
struct QuantumRingRolloverAccess {
  static void seed(QuantumRing& ring, u32 write, u32 read) noexcept {
    ring.writeIdx_.store(write, std::memory_order_relaxed);
    ring.readIdx_.store(read, std::memory_order_relaxed);
  }
};

}  // namespace rt
}  // namespace audiocap
}  // namespace concord

using concord::audiocap::rt::QuantumRing;
using concord::audiocap::rt::QuantumRingRolloverAccess;
using concord::audiocap::rt::RingResult;
using concord::audiocap::rt::u32;
using concord::audiocap::rt::u8;

namespace {

int g_checks = 0;
#define CHECK(cond)                                                            \
  do {                                                                         \
    ++g_checks;                                                                \
    if (!(cond)) {                                                             \
      std::fprintf(stderr, "FAIL %s:%d  %s\n", __FILE__, __LINE__, #cond);     \
      std::abort();                                                            \
    }                                                                          \
  } while (0)

constexpr u32 kSlots = 4u;
constexpr u32 kBytes = 16u;

std::vector<u8> makeStorage() {
  return std::vector<u8>(QuantumRing::storageBytes(kSlots, kBytes), 0u);
}

// Geometry that cannot work must produce a ring that refuses EVERYTHING, not one
// that half-works. Fail closed is the property; each row is a different way to be
// wrong.
void testInvalidGeometryFailsClosed() {
  std::vector<u8> storage = makeStorage();
  u8  out[kBytes] = {0};
  u32 got         = 0u;

  struct Case { u8* storage; u32 slots; u32 bytes; const char* why; };
  std::vector<u8> ok = makeStorage();
  const Case cases[] = {
      {nullptr,     kSlots, kBytes, "null storage"},
      {ok.data(),   3u,     kBytes, "slot count not a power of two"},
      {ok.data(),   1u,     kBytes, "fewer than two slots"},
      {ok.data(),   128u,   kBytes, "more slots than kMaxSlots"},
      {ok.data(),   kSlots, 0u,     "zero-byte slots"},
  };

  for (const Case& c : cases) {
    // ok.size(), never sizeof(c.storage) -- c.storage is a POINTER, so sizeof is 8
    // and every row would then be refused for a short buffer rather than for the
    // geometry it is actually testing. The rows would still pass, which is what
    // makes that mistake worth naming: a vacuous green in a fail-closed test.
    QuantumRing ring(c.storage, ok.size(), c.slots, c.bytes);
    CHECK(!ring.valid());
    CHECK(ring.push(storage.data(), 1u) == RingResult::kInvalid);
    CHECK(ring.pop(out, kBytes, &got) == RingResult::kInvalid);
  }

  // The buffer itself, which the geometry rows above cannot cover: a caller who
  // sizes correctly-shaped geometry against a SHORT buffer. Before storageLen
  // existed this ring reported valid() and push() wrote past the end. One byte
  // short is deliberate -- an off-by-one is the shape a real caller produces.
  {
    const std::size_t need = QuantumRing::storageBytes(kSlots, kBytes);
    std::vector<u8>   shortBuf(need - 1u, 0u);
    QuantumRing ring(shortBuf.data(), shortBuf.size(), kSlots, kBytes);
    CHECK(!ring.valid());
    CHECK(ring.push(storage.data(), 1u) == RingResult::kInvalid);
    CHECK(ring.pop(out, kBytes, &got) == RingResult::kInvalid);
    // ...and exactly enough is accepted, so the bound is not merely conservative.
    std::vector<u8> exactBuf(need, 0u);
    QuantumRing exact(exactBuf.data(), exactBuf.size(), kSlots, kBytes);
    CHECK(exact.valid());
  }
}

// Each null argument INDEPENDENTLY. Passing both at once cannot distinguish
// `dst == nullptr || outBytes == nullptr` from `&&`, and under `&&` the very next
// statement dereferences the null it just admitted.
void testEachNullArgumentIsRejectedOnItsOwn() {
  std::vector<u8> storage = makeStorage();
  QuantumRing ring(storage.data(), storage.size(), kSlots, kBytes);
  CHECK(ring.valid());

  u8  out[kBytes] = {0};
  u32 got         = 7u;
  CHECK(ring.pop(out, kBytes, nullptr) == RingResult::kBadArg);
  CHECK(ring.pop(nullptr, kBytes, &got) == RingResult::kBadArg);

  // On a VALID ring: the existing coverage only ever passed null to an INVALID
  // one, where !valid_ returns first and the null check is never reached. Deleting
  // push's null guard survived every test in this file.
  CHECK(ring.push(nullptr, 4u) == RingResult::kBadArg);
  CHECK(ring.depth() == 0u);
}

void testRoundTripPreservesBytesAndLength() {
  std::vector<u8> storage = makeStorage();
  QuantumRing ring(storage.data(), storage.size(), kSlots, kBytes);
  CHECK(ring.valid());
  CHECK(ring.depth() == 0u);

  const u8 payload[5] = {1u, 2u, 3u, 4u, 5u};
  CHECK(ring.push(payload, 5u) == RingResult::kOk);
  CHECK(ring.depth() == 1u);

  u8  out[kBytes] = {0};
  u32 got         = 0u;
  CHECK(ring.pop(out, kBytes, &got) == RingResult::kOk);
  CHECK(got == 5u);
  CHECK(std::memcmp(out, payload, 5u) == 0);
  CHECK(ring.depth() == 0u);
  CHECK(ring.dropped() == 0u);
}

void testEmptyPop() {
  std::vector<u8> storage = makeStorage();
  QuantumRing ring(storage.data(), storage.size(), kSlots, kBytes);
  u8  out[kBytes] = {0};
  u32 got         = 7u;  // must be zeroed even on the failure path
  CHECK(ring.pop(out, kBytes, &got) == RingResult::kEmpty);
  CHECK(got == 0u);
}

// The drop is the NEW quantum, never an already-published one. ADR-0043 D4c makes
// this forced rather than chosen for the MessagePort transport; here it is chosen,
// and the two must agree or the ring silently reorders audio.
void testFullRingDropsTheNewestAndCounts() {
  std::vector<u8> storage = makeStorage();
  QuantumRing ring(storage.data(), storage.size(), kSlots, kBytes);

  for (u32 i = 0u; i < kSlots; ++i) {
    const u8 payload[1] = {static_cast<u8>(0xA0u + i)};
    CHECK(ring.push(payload, 1u) == RingResult::kOk);
  }
  CHECK(ring.depth() == kSlots);

  const u8 rejected[1] = {0xFFu};
  CHECK(ring.push(rejected, 1u) == RingResult::kFull);
  CHECK(ring.dropped() == 1u);
  CHECK(ring.depth() == kSlots);

  // Every survivor is intact and in order, and 0xFF is nowhere in the ring.
  for (u32 i = 0u; i < kSlots; ++i) {
    u8  out[kBytes] = {0};
    u32 got         = 0u;
    CHECK(ring.pop(out, kBytes, &got) == RingResult::kOk);
    CHECK(got == 1u);
    CHECK(out[0] == static_cast<u8>(0xA0u + i));
  }
  CHECK(ring.pop(nullptr, 0u, nullptr) == RingResult::kBadArg);
}

// An oversized push must not consume a slot. If it did, a malformed quantum would
// cost a good one.
void testOversizedPushConsumesNothing() {
  std::vector<u8> storage = makeStorage();
  QuantumRing ring(storage.data(), storage.size(), kSlots, kBytes);

  const std::vector<u8> tooBig(kBytes + 1u, 0xEEu);
  CHECK(ring.push(tooBig.data(), kBytes + 1u) == RingResult::kTooBig);
  CHECK(ring.depth() == 0u);
  CHECK(ring.dropped() == 0u);  // kTooBig is a caller bug, not back-pressure

  const u8 exact[kBytes] = {0};
  CHECK(ring.push(exact, kBytes) == RingResult::kOk);
  CHECK(ring.depth() == 1u);
}

// A consumer buffer too small must REPORT, never truncate, and must leave the slot
// in place so a correctly sized retry still gets the audio. A silent short read is
// the exact failure shape ADR-0043 is written against.
void testShortConsumerBufferDoesNotTruncateOrConsume() {
  std::vector<u8> storage = makeStorage();
  QuantumRing ring(storage.data(), storage.size(), kSlots, kBytes);

  const u8 payload[8] = {9u, 8u, 7u, 6u, 5u, 4u, 3u, 2u};
  CHECK(ring.push(payload, 8u) == RingResult::kOk);

  u8  small[4] = {0};
  u32 got      = 7u;  // a sentinel, not 0 -- asserting 0 against a value the test
                      // itself set proves nothing about who wrote it.
  CHECK(ring.pop(small, 4u, &got) == RingResult::kBadArg);
  CHECK(got == 0u);
  CHECK(ring.depth() == 1u);  // still there

  // The correctly sized retry still gets the whole payload: refusing must leave the
  // slot intact, not merely return an error code.
  u8 big[kBytes] = {0};
  got = 7u;
  CHECK(ring.pop(big, kBytes, &got) == RingResult::kOk);
  CHECK(got == 8u);
  CHECK(std::memcmp(big, payload, 8u) == 0);

  // len == dstCapacity is the BOUNDARY, and it was only ever killed by accident --
  // by the concurrency test, which happens to pop with an exactly-sized buffer.
  // Pinned here, next to the refusal it borders on.
  CHECK(ring.push(payload, 8u) == RingResult::kOk);
  u8 exact[8] = {0};
  got = 7u;
  CHECK(ring.pop(exact, 8u, &got) == RingResult::kOk);
  CHECK(got == 8u);
  CHECK(ring.depth() == 0u);
}

// pop's COPY LENGTH. Every other successful pop in this file uses a dst exactly
// slotBytes wide, so `memcpy(dst, src, slotBytes_)` in place of `len` is invisible
// to all of them -- and to ASAN, because the write stays inside the caller's
// oversized buffer. It is a real overflow in CONFORMING use: pop's contract is
// only `dstCapacity >= len`, so a caller may legally size the buffer to the
// payload. Canary bytes make the over-copy observable.
// Found by @pr-review-toolkit:pr-test-analyzer on PR #3155.
void testPopCopiesOnlyTheStoredLength() {
  std::vector<u8> storage = makeStorage();
  QuantumRing ring(storage.data(), storage.size(), kSlots, kBytes);

  const u8 payload[5] = {1u, 2u, 3u, 4u, 5u};
  CHECK(ring.push(payload, 5u) == RingResult::kOk);

  // Exactly the payload, then a canary the ring must not touch.
  u8 dst[5 + 8];
  for (u32 i = 0u; i < sizeof(dst); ++i) { dst[i] = 0xCDu; }
  u32 got = 7u;
  CHECK(ring.pop(dst, 5u, &got) == RingResult::kOk);
  CHECK(got == 5u);
  for (u32 i = 0u; i < 5u; ++i) { CHECK(dst[i] == payload[i]); }
  for (u32 i = 5u; i < sizeof(dst); ++i) { CHECK(dst[i] == 0xCDu); }
  CHECK(ring.depth() == 0u);
}

// A geometry whose slot offsets cannot be represented must be REFUSED, not
// accepted and then indexed wrongly. Found by Codex on PR #3155 and reproduced
// before fixing: a four-slot ring with slotBytes 0x80000000 passed valid(), and
// `slot * slotBytes_` in u32 wrapped so slot 2 computed base 0 -- aliasing and
// corrupting slot 0. storageBytes() was correct throughout because it casts to
// size_t, so the class requested the right allocation and then wrote outside its
// own slot. Offsets are computed in size_t now. NOTE: on 64-bit the geometry is NOT refused -- offsetsRepresentable cannot be false for any u32 pair there, so the cast is the whole fix and that clause is a 32-bit backstop with no coverage on any target CI builds, because
// on a 32-bit size_t the wider arithmetic alone would not save it.
//
// No storage is ever touched here: an invalid ring refuses every operation, which
// is the property under test.
void testSlotOffsetsDoNotWrap() {
  // Found by Codex on PR #3155 and reproduced before fixing: slot offsets were
  // computed as `slot * slotBytes_` in u32. A four-slot ring with slotBytes
  // 0x80000000 passed valid(), and slot 2 computed base 0 -- aliasing slot 0 and
  // corrupting it -- while storageBytes() returned the correct 8 GiB because it
  // already cast to size_t. The class asked for the right allocation and then wrote
  // outside its own slot.
  //
  // Asserted on the ARITHMETIC rather than on a live ring: exposing 8 GiB of real
  // memory to a unit test to check a multiplication is the wrong trade, and the
  // multiplication is the whole defect.
  constexpr u32 kHuge = 0x80000000u;
  CHECK(QuantumRing::slotOffset(0u, kHuge) == 0u);
  CHECK(QuantumRing::slotOffset(1u, kHuge) == static_cast<std::size_t>(kHuge));
  // The one that used to wrap to 0.
  CHECK(QuantumRing::slotOffset(2u, kHuge) == static_cast<std::size_t>(kHuge) * 2u);
  CHECK(QuantumRing::slotOffset(2u, kHuge) != 0u);
  CHECK(QuantumRing::slotOffset(3u, kHuge) == static_cast<std::size_t>(kHuge) * 3u);

  // Every slot offset must be distinct — aliasing is the failure, not the size.
  CHECK(QuantumRing::slotOffset(0u, kHuge) != QuantumRing::slotOffset(2u, kHuge));
  CHECK(QuantumRing::slotOffset(1u, kHuge) != QuantumRing::slotOffset(3u, kHuge));

  // A CONSISTENCY PIN, not a discriminating assertion: storageBytes() and
  // slotOffset() are byte-identical implementations, so this cannot fail while
  // they stay that way. It exists to catch them DIVERGING.
  CHECK(QuantumRing::storageBytes(4u, kHuge) == QuantumRing::slotOffset(4u, kHuge));

  // The guard must not over-reject: a large but ordinary geometry stays usable.
  std::vector<u8> storage(QuantumRing::storageBytes(64u, 1024u), 0u);
  QuantumRing big(storage.data(), storage.size(), 64u, 1024u);
  CHECK(big.valid());
  const u8 payload[3] = {7u, 7u, 7u};
  CHECK(big.push(payload, 3u) == RingResult::kOk);
}

// Many laps, so the mask wrap is exercised rather than assumed.
void testWrapsAroundManyTimes() {
  std::vector<u8> storage = makeStorage();
  QuantumRing ring(storage.data(), storage.size(), kSlots, kBytes);

  for (u32 i = 0u; i < 1000u; ++i) {
    const u8 payload[2] = {static_cast<u8>(i & 0xFFu), static_cast<u8>((i >> 8) & 0xFFu)};
    CHECK(ring.push(payload, 2u) == RingResult::kOk);

    u8  out[kBytes] = {0};
    u32 got         = 0u;
    CHECK(ring.pop(out, kBytes, &got) == RingResult::kOk);
    CHECK(got == 2u);
    CHECK(out[0] == payload[0]);
    CHECK(out[1] == payload[1]);
  }
  CHECK(ring.dropped() == 0u);
}

// push()/pop() decide fullness with `w - r` on u32, which is correct ACROSS the
// rollover and would be wrong with a signed or absolute comparison. 2^32 pushes is
// not a test, so seed the counters at the boundary instead.
void testCountersSurviveU32Rollover() {
  std::vector<u8> storage = makeStorage();
  QuantumRing ring(storage.data(), storage.size(), kSlots, kBytes);

  const u32 nearMax = 0xFFFFFFFEu;
  QuantumRingRolloverAccess::seed(ring, nearMax, nearMax);
  CHECK(ring.depth() == 0u);

  // Straddle the rollover: these four pushes take writeIdx_ 0xFFFFFFFE -> 0x00000002.
  for (u32 i = 0u; i < kSlots; ++i) {
    const u8 payload[1] = {static_cast<u8>(0x50u + i)};
    CHECK(ring.push(payload, 1u) == RingResult::kOk);
  }
  CHECK(ring.depth() == kSlots);

  const u8 rejected[1] = {0xFFu};
  CHECK(ring.push(rejected, 1u) == RingResult::kFull);

  for (u32 i = 0u; i < kSlots; ++i) {
    u8  out[kBytes] = {0};
    u32 got         = 0u;
    CHECK(ring.pop(out, kBytes, &got) == RingResult::kOk);
    CHECK(out[0] == static_cast<u8>(0x50u + i));
  }
  CHECK(ring.depth() == 0u);
}

// The ONLY test here that can observe a memory-ordering defect.
//
// push() publishes the payload with a RELEASE store and pop() reads the index with
// an ACQUIRE load. Weaken either and a consumer can see a slot marked published
// whose bytes have not landed -- torn audio. NO SINGLE-THREADED TEST CAN CATCH
// THAT, because there is no second thread to observe the reordering; the
// falsification run confirms exactly that (relaxing the release store leaves every
// other test green).
//
// So this test exists to give ThreadSanitizer something to analyse. Under TSAN a
// missing release/acquire edge is reported as a data race on the payload bytes --
// deterministic, rather than hoping the hardware reorders during this particular
// run. Unsanitized it is still worth running: on a weakly-ordered CPU (arm64) a
// torn payload is observable, and the byte-pattern check below is what would see it.
//
// The payload is self-verifying: every byte of a quantum is derived from that
// quantum's sequence number, so a partially-published slot fails the check rather
// than merely looking odd.
void testConcurrentProducerConsumerNeverTears() {
  constexpr u32 kConcSlots = 8u;
  constexpr u32 kConcBytes = 64u;
  constexpr u32 kQuanta    = 200000u;

  std::vector<u8> storage(QuantumRing::storageBytes(kConcSlots, kConcBytes), 0u);
  QuantumRing ring(storage.data(), storage.size(), kConcSlots, kConcBytes);
  CHECK(ring.valid());

  std::atomic<bool> producerDone{false};
  std::atomic<u32>  accepted{0u};
  std::atomic<u32>  consumed{0u};
  std::atomic<u32>  tears{0u};
  std::atomic<u32>  outOfOrder{0u};
  std::atomic<u32>  depthUnderflow{0u};
  // The observer's own liveness. Its loop is `while (!producerDone)`, so if it is
  // scheduled late it samples ZERO times and the depthUnderflow check below passes
  // having observed nothing -- and that check is the only assertion that
  // discriminates depth()'s load order, the one mutation whose kill this suite
  // claims. The consumer already carries exactly this guard; the observer did not.
  // Found by @code-reviewer on PR #3155. TSAN's slowdown makes the relative
  // scheduling something not to rely on.
  std::atomic<u32>  observerSamples{0u};
  // A STARTUP BARRIER for the observer, because the liveness assertion above turns
  // a scheduling accident into a test failure. The observer thread is constructed
  // AFTER the producer, so on a loaded machine the producer can finish its whole
  // workload before the observer ever gets CPU -- the observer's `while
  // (!producerDone)` loop then exits without sampling once, and
  // `CHECK(observerSamples > 0)` aborts on a perfectly correct ring. The guard was
  // right to add and wrong to leave unsynchronised. Found by CodeRabbit on
  // PR #3155.
  std::atomic<bool> observerReady{false};
  // The pre-production sample below makes observerSamples >= 1 true by
  // construction -- which is exactly why it cannot be the liveness assertion. If
  // the observer is descheduled right after publishing readiness, the producer and
  // consumer can finish all 200,000 operations before it samples again, and
  // `observerSamples > 0` passes on the strength of a sample taken when the ring
  // was empty and no index could move. A reversed depth() load order then survives
  // by schedule. These two count only samples taken while the workload is in
  // flight, and the producer refuses to finish until one lands. Found by
  // CodeRabbit on PR #3155.
  std::atomic<bool> workPublished{false};
  std::atomic<u32>  workEraSamples{0u};

  auto fill = [](u8* dst, u32 seq) {
    dst[0] = static_cast<u8>(seq & 0xFFu);
    dst[1] = static_cast<u8>((seq >> 8) & 0xFFu);
    dst[2] = static_cast<u8>((seq >> 16) & 0xFFu);
    dst[3] = static_cast<u8>((seq >> 24) & 0xFFu);
    for (u32 i = 4u; i < kConcBytes; ++i) {
      dst[i] = static_cast<u8>((seq + i) & 0xFFu);
    }
  };

  std::thread producer([&] {
    // Do not start producing until the observer is actually sampling.
    while (!observerReady.load(std::memory_order_acquire)) {
      std::this_thread::yield();
    }
    u8 payload[kConcBytes];
    for (u32 seq = 0u; seq < kQuanta; ++seq) {
      fill(payload, seq);
      // A full ring is a DROP, never a block. ADR-0043 D4c: back-pressure inside a
      // real-time callback can only ever be expressed as a drop.
      if (ring.push(payload, kConcBytes) == RingResult::kOk) {
        accepted.fetch_add(1u, std::memory_order_relaxed);
        // Read-then-store so the release fence is paid once, not 200,000 times.
        if (!workPublished.load(std::memory_order_relaxed)) {
          workPublished.store(true, std::memory_order_release);
        }
      }
    }
    // Hold producerDone until the observer has taken at least one sample with work
    // in flight. Bounded rather than an open spin: an exhausted budget must surface
    // as the CHECK below failing with a diagnosis, never as a hung CI job.
    for (u32 spins = 0u;
         spins < 50000000u && workEraSamples.load(std::memory_order_acquire) == 0u;
         ++spins) {
      std::this_thread::yield();
    }
    producerDone.store(true, std::memory_order_release);
  });

  std::thread consumer([&] {
    u8   out[kConcBytes];
    u32  got      = 0u;
    bool haveLast = false;
    u32  last     = 0u;
    for (;;) {
      const RingResult r = ring.pop(out, kConcBytes, &got);
      if (r == RingResult::kEmpty) {
        if (producerDone.load(std::memory_order_acquire)) {
          // One last drain: the producer may have published between the empty read
          // and the flag read.
          if (ring.pop(out, kConcBytes, &got) != RingResult::kOk) { break; }
        } else {
          continue;
        }
      } else if (r != RingResult::kOk) {
        tears.fetch_add(1u, std::memory_order_relaxed);
        break;
      }

      if (got != kConcBytes) { tears.fetch_add(1u, std::memory_order_relaxed); continue; }

      const u32 seq = static_cast<u32>(out[0]) |
                      (static_cast<u32>(out[1]) << 8) |
                      (static_cast<u32>(out[2]) << 16) |
                      (static_cast<u32>(out[3]) << 24);
      // Every byte must belong to THIS sequence number. A slot published before its
      // bytes landed fails here.
      for (u32 i = 4u; i < kConcBytes; ++i) {
        if (out[i] != static_cast<u8>((seq + i) & 0xFFu)) {
          tears.fetch_add(1u, std::memory_order_relaxed);
          break;
        }
      }
      // Drops are expected and fine; going BACKWARDS is not.
      if (haveLast && seq <= last) { outOfOrder.fetch_add(1u, std::memory_order_relaxed); }
      last     = seq;
      haveLast = true;
      consumed.fetch_add(1u, std::memory_order_relaxed);


    }
  });

  // A THIRD, READ-ONLY OBSERVER. depth()'s load-order defect needs both indices to
  // move between its two loads, and neither the producer nor the consumer can
  // produce that from inside depth() -- whichever one calls it is the thread that
  // would have to advance an index mid-call. A consumer-side sample was tried first
  // and the mutation run proved it vacuous: reverting depth() to the wrong order
  // left the suite green.
  //
  // Loading writeIdx_ first lets the producer push (w: n -> n+1) and the consumer
  // drain that item (r: n -> n+1) between the two loads, so the observer holds a
  // stale w and a fresh r with r > w -- and `w - r` underflows to roughly 2^32.
  // Two atomic loads add no synchronisation the ring does not already have.
  std::thread observer([&] {
    // ONE UNCONDITIONAL SAMPLE, THEN signal ready. Storing ready first still
    // raced: preempted between the store and the loop, the producer and consumer
    // could finish all 200,000 operations and set producerDone before this thread
    // reached `while`, so it sampled zero times and the liveness CHECK failed on a
    // correct ring -- the exact flake the barrier was added to remove, just with a
    // narrower window. Taking the first sample before publishing readiness makes
    // observerSamples >= 1 true by construction rather than by scheduling.
    // Found by Codex on PR #3155.
    observerSamples.fetch_add(1u, std::memory_order_relaxed);
    if (ring.depth() > (1u << 31)) {
      depthUnderflow.fetch_add(1u, std::memory_order_relaxed);
    }
    observerReady.store(true, std::memory_order_release);
    while (!producerDone.load(std::memory_order_acquire)) {
      observerSamples.fetch_add(1u, std::memory_order_relaxed);
      if (workPublished.load(std::memory_order_acquire)) {
        workEraSamples.fetch_add(1u, std::memory_order_relaxed);
      }
      for (u32 i = 0u; i < 1000u; ++i) {
        // Threshold is 2^31, not kConcSlots. depth() is an OVER-estimate under
        // concurrency -- true depth plus however many pops landed between its two
        // loads -- so a value above the ring size is legal and a first version of
        // this assertion failed on correct code. What must never happen is the
        // UNDERFLOW the load order exists to prevent, and that lands near 2^32.
        if (ring.depth() > (1u << 31)) {
          depthUnderflow.fetch_add(1u, std::memory_order_relaxed);
        }
      }
    }
  });

  producer.join();
  consumer.join();
  observer.join();

  CHECK(tears.load() == 0u);
  CHECK(outOfOrder.load() == 0u);
  CHECK(depthUnderflow.load() == 0u);
  // ...and prove the line above actually observed something WHILE THE INDICES WERE
  // MOVING. observerSamples alone is satisfied by the pre-production sample, which
  // no load order can get wrong.
  CHECK(observerSamples.load() > 0u);
  CHECK(workEraSamples.load() > 0u);
  // The consumer must have seen real traffic, or this test asserted nothing at all.
  CHECK(consumed.load() > 0u);
  // EQUALITY, not <=. The consumer drains to empty after producerDone, so every
  // accepted quantum must come back out -- `<=` admitted silent audio loss: a pop
  // advancing readIdx_ by two whenever depth allows keeps tears at 0 (each payload
  // stays self-consistent), keeps outOfOrder at 0 (sequence numbers only jump
  // FORWARD), and keeps accepted + dropped == kQuanta. It survived every other
  // assertion here. Measured over four runs before the change, consumed and
  // accepted were equal every time (111789, 101229, 93466, 119954). Found by
  // @pr-review-toolkit:pr-test-analyzer on PR #3155.
  CHECK(consumed.load() == accepted.load());
  CHECK(accepted.load() + ring.dropped() == kQuanta);
}

}  // namespace

int main() {
  testInvalidGeometryFailsClosed();
  testRoundTripPreservesBytesAndLength();
  testEmptyPop();
  testFullRingDropsTheNewestAndCounts();
  testOversizedPushConsumesNothing();
  testShortConsumerBufferDoesNotTruncateOrConsume();
  testPopCopiesOnlyTheStoredLength();
  testEachNullArgumentIsRejectedOnItsOwn();
  testSlotOffsetsDoNotWrap();
  testWrapsAroundManyTimes();
  testCountersSurviveU32Rollover();
  testConcurrentProducerConsumerNeverTears();

  std::printf("quantum_ring_test: %d checks passed\n", g_checks);
  return 0;
}
