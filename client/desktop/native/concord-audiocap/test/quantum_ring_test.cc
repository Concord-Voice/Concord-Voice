// concord-audiocap / rt — QuantumRing tests.
//
// A CHECK macro, no framework. This binary is built by CI under ASAN, UBSAN and
// TSAN (ADR-0043 § Verification: "a sanitizer failure is a merge blocker"), and a
// test framework would only add a dependency to a target whose whole job is to be
// run under a sanitizer.
//
// This file is test code and is NOT bound by the JSF++ profile — that profile
// governs rt/, where an allocation is audible. Asserting is what this file is for.

// Ask quantum_ring.h for its test seam BEFORE including it. Done here rather than
// in the build files so no workflow, gyp target or compile command has to know:
// a production TU simply never writes this macro.
#define CONCORD_AUDIOCAP_TEST_SEAM 1
#include "../rt/quantum_ring.h"

// The synthetic source is compile-gated OUT of every release translation unit
// (design section 5, Q1). Defining the macro HERE rather than on the command line
// is what lets the existing `rt-sanitizers` job -- which compiles this file with a
// fixed command and no -D of its own -- exercise it without a workflow change.
#define CONCORD_AUDIOCAP_SYNTHETIC 1
#include "../rt/quantum_header.h"
#include "../rt/synthetic_source.h"

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

  // Saturation is only observable near 2^32, and a ring cannot be driven there:
  // at one drop per 10 ms quantum it is 497 days of a permanently full ring. So
  // the counter is SEEDED, exactly as the index rollover above is seeded, rather
  // than reached. A production `force_overrun()` was the other option and is
  // deliberately not taken -- a setter on the counter is a way to make the drop
  // witness lie, and it would exist in the shipped binary to serve a test.
  static void seedOverrun(QuantumRing& ring, u32 value) noexcept {
    ring.overrun_.store(value, std::memory_order_relaxed);
  }
};

}  // namespace rt
}  // namespace audiocap
}  // namespace concord

using concord::audiocap::rt::encodeQuantumHeader;
using concord::audiocap::rt::QuantumHeader;
using concord::audiocap::rt::QuantumRing;
using concord::audiocap::rt::QuantumRingRolloverAccess;
using concord::audiocap::rt::RingResult;
using concord::audiocap::rt::SyntheticSource;
using concord::audiocap::rt::u16;
using concord::audiocap::rt::u32;
using concord::audiocap::rt::u64;
using concord::audiocap::rt::u8;

namespace hdr = concord::audiocap::rt;

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
  CHECK(ring.overrun() == 0u);
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
  CHECK(ring.overrun() == 1u);
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
  CHECK(ring.overrun() == 0u);  // kTooBig is a caller bug, not back-pressure

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
  CHECK(ring.overrun() == 0u);
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
  // Every push ATTEMPT, both loops. The conservation assertion below used to read
  // `accepted + dropped == kQuanta`, which was written when kQuanta was the only
  // source of pushes. The recovery loop added below breaks that the moment it runs
  // even once -- a dropped extra inflates ring.overrun() and an accepted one
  // inflates accepted, so the sum exceeds kQuanta and a CORRECT ring fails. The
  // commit that added the loop claimed otherwise; that claim was wrong. Found
  // independently by Gitar, CodeRabbit and Codex on PR #3188.
  std::atomic<u32>  attempted{0u};
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
      attempted.fetch_add(1u, std::memory_order_relaxed);
      if (ring.push(payload, kConcBytes) == RingResult::kOk) {
        accepted.fetch_add(1u, std::memory_order_relaxed);
      }
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
  // ...and prove the observer ran at all. Safe as a MANDATORY condition because the
  // observer takes one unconditional sample before publishing readiness, so this is
  // true by construction rather than by scheduling.
  //
  // There used to be a second, stricter gate here — a counter of samples taken while
  // the indices were moving, plus a bounded recovery loop the producer ran to force
  // one. Both are gone. That gate DEMANDED a scheduling event, so a correct ring
  // could abort CI simply by descheduling the observer at the wrong moment, and once
  // testDepthLoadOrderUnderForcedInterleaving started proving the load order
  // deterministically it bought nothing. Found by Codex on PR #3188.
  //
  // `CHECK(depthUnderflow == 0)` above stays, and the distinction is the whole point:
  // it is ONE-SIDED. It can only fire when an underflow was actually observed, so it
  // never fails on a correct ring — it is a free opportunistic catch, not a demand.
  CHECK(observerSamples.load() > 0u);
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
  CHECK(accepted.load() + ring.overrun() == attempted.load());
  // ...and the workload was at least the one this test was written around, so the
  // assertion above cannot be satisfied by a run that pushed almost nothing.
  CHECK(attempted.load() >= kQuanta);
}

}  // namespace

// The depth() load order, proven DETERMINISTICALLY and single-threaded.
//
// testConcurrentProducerConsumerNeverTears above is a liveness and tearing test.
// It can only ever make the underflow interleaving PROBABLE, and Codex measured
// the gap that leaves: an exact reversed-load-order mutant survived 5/100 normal
// runs and 37/50 single-CPU runs even with the read-index bracket. This test owns
// the ordering property instead, with no threads and no scheduler dependence.
//
// The mechanism: force the read index to advance PAST the already-loaded write
// index between depth()'s two loads.
//   correct order  — r is read first (0), the probe moves r to 2 and w to 2,
//                    then w is read (2): depth = 2 - 0 = 2. No underflow.
//   reversed order — w is read first (1), the probe moves r to 2, then r is read
//                    (2): depth = 1 - 2, which underflows to ~2^32.
static QuantumRing* g_probeRing = nullptr;
static bool         g_probeFired = false;

static void advanceReadIndexPastWrite(void*) {
  // One-shot: the pushes and pops below must not re-enter through depth().
  if (g_probeRing == nullptr || g_probeFired) { return; }
  g_probeFired = true;
  u8  scratch[8] = {0u};
  u32 got = 0u;
  (void)g_probeRing->pop(scratch, sizeof(scratch), &got);   // r: 0 -> 1
  (void)g_probeRing->push(scratch, 4u);                     // w: 1 -> 2
  (void)g_probeRing->pop(scratch, sizeof(scratch), &got);   // r: 1 -> 2
}

static void testDepthLoadOrderUnderForcedInterleaving() {
  std::vector<u8> storage(QuantumRing::storageBytes(4u, 8u), 0u);
  QuantumRing ring(storage.data(), storage.size(), 4u, 8u);
  CHECK(ring.valid());

  const u8 payload[4] = {1u, 2u, 3u, 4u};
  CHECK(ring.push(payload, 4u) == RingResult::kOk);         // w = 1, r = 0

  g_probeRing = &ring;
  g_probeFired = false;
  QuantumRing::setDepthProbe(advanceReadIndexPastWrite, nullptr);
  const u32 observed = ring.depth();
  QuantumRing::setDepthProbe(nullptr, nullptr);
  g_probeRing = nullptr;

  // Not vacuous: if the probe never ran, the interleaving never happened and this
  // test proves nothing about either load order.
  CHECK(g_probeFired);
  // The whole property. Reversed loads make this ~2^32.
  CHECK(observed < (1u << 31));
}


// ---------------------------------------------------------------------------
// #3195 — the credit-bound geometry, the saturating drop witness, the header
// encoder, and the compile-gated synthetic source.
//
// Everything below reads the wire bytes with a LOCAL little-endian reader rather
// than a decoder shipped beside the encoder. A reader that shares the encoder's
// mistake agrees with it perfectly, so the fixed fields are ALSO asserted byte by
// byte at their literal offsets -- that is the assertion an endianness or
// offset-shift mutant cannot survive.
// ---------------------------------------------------------------------------

u16 readU16(const u8* b, u32 off) {
  return static_cast<u16>(static_cast<u16>(b[off]) |
                          static_cast<u16>(static_cast<u16>(b[off + 1u]) << 8));
}

u32 readU32(const u8* b, u32 off) {
  u32 v = 0u;
  for (u32 i = 0u; i < 4u; ++i) { v |= static_cast<u32>(b[off + i]) << (8u * i); }
  return v;
}

u64 readU64(const u8* b, u32 off) {
  u64 v = 0u;
  for (u32 i = 0u; i < 8u; ++i) { v |= static_cast<u64>(b[off + i]) << (8u * i); }
  return v;
}

// One whole quantum exactly as the producer composes it: header(32) then 480x2
// interleaved f32. The ring carries the WHOLE quantum in one slot, so nothing
// downstream has to re-associate a header with its samples.
void composeQuantum(u32 seq, u64 timestampNs, u32 overrunTotal, u8* dst) {
  const QuantumHeader h = {seq, timestampNs, overrunTotal};
  CHECK(encodeQuantumHeader(dst, hdr::kQuantumBytes, h));
  CHECK(SyntheticSource::fillSamples(seq, &dst[hdr::kHeaderBytes], hdr::kSampleBytes));
}

// THE credit bound, at the real geometry. ringSlots == creditBound == 8, so the
// child refusing to drain past 8 outstanding IS the ring filling: credit
// exhaustion and ring overflow are one event with one counter (design section 4d).
void testCreditBoundRingDropsNewestAndCountsOverrun() {
  std::vector<u8> storage(QuantumRing::storageBytes(hdr::kRingSlots, hdr::kQuantumBytes), 0u);
  QuantumRing ring(storage.data(), storage.size(), hdr::kRingSlots, hdr::kQuantumBytes);
  CHECK(ring.valid());

  std::vector<u8> q(hdr::kQuantumBytes, 0u);
  for (u32 i = 0u; i < hdr::kRingSlots; ++i) {
    composeQuantum(i, 1000u + i, 0u, q.data());
    CHECK(ring.push(q.data(), hdr::kQuantumBytes) == RingResult::kOk);
  }
  CHECK(ring.depth() == hdr::kRingSlots);
  const u32 before = ring.overrun();
  CHECK(before == 0u);

  // The ninth quantum. Dropped, never blocked, and never at the cost of one
  // already published.
  composeQuantum(99u, 9999u, before, q.data());
  CHECK(ring.push(q.data(), hdr::kQuantumBytes) == RingResult::kFull);
  CHECK(ring.overrun() == before + 1u);
  CHECK(ring.depth() == hdr::kRingSlots);

  // Drop-newest, never drop-oldest: seq 0 is still at the front, seq 99 is
  // nowhere in the ring, and the survivors are in order.
  std::vector<u8> out(hdr::kQuantumBytes, 0u);
  u32 got = 0u;
  for (u32 i = 0u; i < hdr::kRingSlots; ++i) {
    CHECK(ring.pop(out.data(), hdr::kQuantumBytes, &got) == RingResult::kOk);
    CHECK(got == hdr::kQuantumBytes);
    CHECK(readU32(out.data(), hdr::kOffSeq) == i);
    CHECK(readU32(out.data(), hdr::kOffSeq) != 99u);
    // Byte-for-byte, not merely header-equal: a slot that returned the right
    // header with someone else's samples is the defect a header check misses.
    composeQuantum(i, 1000u + i, 0u, q.data());
    CHECK(std::memcmp(out.data(), q.data(), hdr::kQuantumBytes) == 0);
  }
  CHECK(ring.pop(out.data(), hdr::kQuantumBytes, &got) == RingResult::kEmpty);
  CHECK(ring.overrun() == 1u);
}

// Below the bound nothing is dropped, so a counter that increments on an ordinary
// push -- or on a pop -- cannot pass. Without this case, "overrun goes up when the
// ring is full" is satisfied by a counter that goes up all the time.
void testBelowTheCreditBoundNothingIsDropped() {
  std::vector<u8> storage(QuantumRing::storageBytes(hdr::kRingSlots, hdr::kQuantumBytes), 0u);
  QuantumRing ring(storage.data(), storage.size(), hdr::kRingSlots, hdr::kQuantumBytes);
  CHECK(ring.valid());

  std::vector<u8> q(hdr::kQuantumBytes, 0u);
  for (u32 i = 0u; i + 1u < hdr::kRingSlots; ++i) {
    composeQuantum(i, 1000u + i, 0u, q.data());
    CHECK(ring.push(q.data(), hdr::kQuantumBytes) == RingResult::kOk);
    CHECK(ring.overrun() == 0u);
  }
  CHECK(ring.depth() == hdr::kRingSlots - 1u);
  CHECK(ring.overrun() == 0u);

  // Draining and refilling around the bound, forever, still drops nothing: the
  // steady state of a consumer that keeps up.
  std::vector<u8> out(hdr::kQuantumBytes, 0u);
  u32 got = 0u;
  for (u32 i = 0u; i < 32u; ++i) {
    CHECK(ring.pop(out.data(), hdr::kQuantumBytes, &got) == RingResult::kOk);
    composeQuantum(100u + i, 2000u + i, 0u, q.data());
    CHECK(ring.push(q.data(), hdr::kQuantumBytes) == RingResult::kOk);
  }
  CHECK(ring.overrun() == 0u);
}

// A WRAPPED counter reads as "no drops", which is the one answer a drop witness
// must never be able to give. Saturation is the whole property.
void testOverrunSaturatesRatherThanWrapping() {
  std::vector<u8> storage = makeStorage();
  QuantumRing ring(storage.data(), storage.size(), kSlots, kBytes);
  CHECK(ring.valid());

  const u32 kMax = 0xFFFFFFFFu;
  QuantumRingRolloverAccess::seedOverrun(ring, kMax - 1u);

  const u8 payload[1] = {0x11u};
  for (u32 i = 0u; i < kSlots; ++i) { CHECK(ring.push(payload, 1u) == RingResult::kOk); }

  // One drop takes it to the ceiling...
  CHECK(ring.push(payload, 1u) == RingResult::kFull);
  CHECK(ring.overrun() == kMax);

  // ...and no number of further drops moves it. A wrapping counter reads 0 here,
  // then 1, 2, ...; an off-by-one ceiling sticks at kMax - 1 above.
  for (u32 i = 0u; i < 16u; ++i) {
    CHECK(ring.push(payload, 1u) == RingResult::kFull);
    CHECK(ring.overrun() == kMax);
    CHECK(ring.overrun() != 0u);
  }
  CHECK(ring.depth() == kSlots);
}

// The 32-byte header, field by field AND byte by byte (design section 4c).
void testQuantumHeaderEncoderWritesTheFixedLayout() {
  const u8 kFill = 0xA5u;
  std::vector<u8> buf(hdr::kQuantumBytes, kFill);
  const QuantumHeader h = {0x11223344u, 0x0102030405060708ull, 0xDEADBEEFu};
  CHECK(encodeQuantumHeader(buf.data(), buf.size(), h));

  CHECK(readU16(buf.data(), hdr::kOffMagic) == hdr::kMagic);
  CHECK(buf[hdr::kOffVersion] == hdr::kHeaderVersion);
  CHECK(buf[hdr::kOffFlags] == 0u);
  CHECK(readU32(buf.data(), hdr::kOffSeq) == 0x11223344u);
  CHECK(readU32(buf.data(), hdr::kOffSampleRate) == hdr::kSampleRate);
  CHECK(readU16(buf.data(), hdr::kOffChannels) == hdr::kChannels);
  CHECK(readU16(buf.data(), hdr::kOffFrameCount) == hdr::kFrameCount);
  CHECK(readU64(buf.data(), hdr::kOffCaptureTimestampNs) == 0x0102030405060708ull);
  CHECK(readU32(buf.data(), hdr::kOffOverrunTotal) == 0xDEADBEEFu);
  CHECK(readU32(buf.data(), hdr::kOffReserved) == 0u);

  // The literal bytes. This is the half a shared-mistake reader cannot give:
  // little-endian, at these offsets, in this order.
  CHECK(buf[0] == 0x57u);
  CHECK(buf[1] == 0xCAu);
  CHECK(buf[2] == 0x01u);
  CHECK(buf[3] == 0x00u);
  CHECK(buf[4] == 0x44u);
  CHECK(buf[7] == 0x11u);
  CHECK(buf[8] == 0x80u);   // 48000 = 0x0000BB80
  CHECK(buf[9] == 0xBBu);
  CHECK(buf[10] == 0x00u);
  CHECK(buf[11] == 0x00u);
  CHECK(buf[12] == 0x02u);
  CHECK(buf[13] == 0x00u);
  CHECK(buf[14] == 0xE0u);  // 480 = 0x01E0
  CHECK(buf[15] == 0x01u);
  CHECK(buf[16] == 0x08u);
  CHECK(buf[23] == 0x01u);
  CHECK(buf[24] == 0xEFu);
  CHECK(buf[27] == 0xDEu);
  CHECK(buf[28] == 0x00u);
  CHECK(buf[31] == 0x00u);

  // The encoder owns bytes 0..31 and NOTHING else. An encoder that ran off the
  // end of the header would corrupt the first frames of audio.
  for (u32 i = hdr::kHeaderBytes; i < hdr::kQuantumBytes; ++i) { CHECK(buf[i] == kFill); }

  // Fail closed, and write nothing while doing it (AV 115: the result is tested).
  const u8 kShortFill = 0x5Au;
  std::vector<u8> shortBuf(hdr::kHeaderBytes - 1u, kShortFill);
  CHECK(!encodeQuantumHeader(shortBuf.data(), shortBuf.size(), h));
  for (std::size_t i = 0u; i < shortBuf.size(); ++i) { CHECK(shortBuf[i] == kShortFill); }
  CHECK(!encodeQuantumHeader(nullptr, hdr::kQuantumBytes, h));

  // Exactly the header size is enough, so the bound is not merely conservative.
  std::vector<u8> exact(hdr::kHeaderBytes, 0u);
  CHECK(encodeQuantumHeader(exact.data(), exact.size(), h));
  CHECK(readU16(exact.data(), hdr::kOffMagic) == hdr::kMagic);
}

// The synthetic waveform is a pure function of `seq`. Task 8 asserts EXACT sample
// content per seq at the far end of the transport, which is only an assertion if
// nothing between here and there can vary the bytes.
void testSyntheticSourceIsDeterministicFromSeqAlone() {
  std::vector<u8> a(hdr::kSampleBytes, 0u);
  std::vector<u8> b(hdr::kSampleBytes, 0u);
  std::vector<u8> c(hdr::kSampleBytes, 0u);

  CHECK(SyntheticSource::fillSamples(7u, a.data(), a.size()));
  // A DIFFERENT seq in between. Filling seq 7 twice back to back also passes
  // against a source driven by a hidden counter that happens to repeat; this
  // ordering does not.
  CHECK(SyntheticSource::fillSamples(9u, c.data(), c.size()));
  CHECK(SyntheticSource::fillSamples(7u, b.data(), b.size()));
  CHECK(std::memcmp(a.data(), b.data(), a.size()) == 0);
  CHECK(std::memcmp(a.data(), c.data(), a.size()) != 0);

  // Consecutive quanta differ, so a source that ignores seq entirely -- the
  // easiest way to be "deterministic" -- cannot pass either.
  std::vector<u8> q0(hdr::kSampleBytes, 0u);
  std::vector<u8> q1(hdr::kSampleBytes, 0u);
  CHECK(SyntheticSource::fillSamples(0u, q0.data(), q0.size()));
  CHECK(SyntheticSource::fillSamples(1u, q1.data(), q1.size()));
  CHECK(std::memcmp(q0.data(), q1.data(), q0.size()) != 0);

  // PINNED CONTENT, which is what makes the far-end assertion mean something.
  // seq 0 starts the table at index 0; the right channel is a quarter period
  // ahead, so the two channels are distinguishable and the interleave order is
  // observable.
  CHECK(readU32(q0.data(), 0u) == SyntheticSource::tableEntry(0u));
  CHECK(readU32(q0.data(), 4u) == SyntheticSource::tableEntry(SyntheticSource::kRightPhaseOffset));
  CHECK(readU32(q0.data(), 8u) == SyntheticSource::tableEntry(1u));
  CHECK(readU32(q0.data(), 0u) != readU32(q0.data(), 4u));
  // seq 1 starts at (1 * 480) mod 47 = 10. A table length that DIVIDED 480 would
  // put every quantum at phase 0 and make the waveform seq-independent.
  CHECK(readU32(q1.data(), 0u) == SyntheticSource::tableEntry(10u));
  CHECK((hdr::kFrameCount % SyntheticSource::kTableLen) != 0u);

  // Every sample is a table entry -- no arithmetic on the values, so no platform
  // libm and no rounding difference can move a byte.
  for (u32 f = 0u; f < hdr::kFrameCount; ++f) {
    const u32 left  = readU32(q0.data(), f * 8u);
    const u32 right = readU32(q0.data(), (f * 8u) + 4u);
    CHECK(left  == SyntheticSource::tableEntry(f));
    CHECK(right == SyntheticSource::tableEntry(f + SyntheticSource::kRightPhaseOffset));
  }

  // Fail closed on a short buffer, writing nothing.
  const u8 kFill = 0x3Cu;
  std::vector<u8> shortBuf(hdr::kSampleBytes - 1u, kFill);
  CHECK(!SyntheticSource::fillSamples(0u, shortBuf.data(), shortBuf.size()));
  for (std::size_t i = 0u; i < shortBuf.size(); ++i) { CHECK(shortBuf[i] == kFill); }
  CHECK(!SyntheticSource::fillSamples(0u, nullptr, hdr::kSampleBytes));
}


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
  testDepthLoadOrderUnderForcedInterleaving();
  testCreditBoundRingDropsNewestAndCountsOverrun();
  testBelowTheCreditBoundNothingIsDropped();
  testOverrunSaturatesRatherThanWrapping();
  testQuantumHeaderEncoderWritesTheFixedLayout();
  testSyntheticSourceIsDeterministicFromSeqAlone();

  std::printf("quantum_ring_test: %d checks passed\n", g_checks);
  return 0;
}
