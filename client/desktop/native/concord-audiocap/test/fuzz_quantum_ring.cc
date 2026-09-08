// concord-audiocap / rt — libFuzzer target for QuantumRing.
//
// ADR-0043 § Verification: "The ring buffer is fuzzed. It is the one component
// reachable across a trust boundary." This is that fuzzer.
//
// It derives the ring GEOMETRY from the input as well as the operation sequence,
// so invalid geometries are exercised alongside valid ones -- the fail-closed path
// is as much a security property as the happy path, and a fuzzer that only ever
// builds a valid ring never tests it.
//
// Build (CI does this; it cannot run under a sandbox that blocks ASAN's shadow map):
//   clang++ -std=c++17 -g -O1 -fsanitize=fuzzer,address,undefined \
//       test/fuzz_quantum_ring.cc -o fuzz_quantum_ring

#include "../rt/quantum_ring.h"

// No <cassert>: this target signals with __builtin_trap() rather than assert(), so
// a failure survives -DNDEBUG. libFuzzer builds are optimized and an assert that
// compiles away is a fuzzer that finds nothing.
#include <cstddef>
#include <cstdint>
#include <deque>
#include <vector>

using concord::audiocap::rt::QuantumRing;
using concord::audiocap::rt::RingResult;
using concord::audiocap::rt::u32;
using concord::audiocap::rt::u8;

extern "C" int LLVMFuzzerTestOneInput(const uint8_t* data, size_t size) {
  if (size < 4) { return 0; }

  // Geometry from the input. Unconstrained on purpose: most draws are INVALID, and
  // an invalid ring must refuse every operation rather than half-work.
  const u32 slotCount = static_cast<u32>(data[0]);
  const u32 slotBytes = static_cast<u32>(data[1]) + 1u;
  // A dedicated control byte. `starve` used to be `slotBytes & 1`, which tied the
  // buffer-starvation decision to the SLOT WIDTH -- so every odd-width geometry
  // was always under-allocated and therefore always modeled invalid, and a valid
  // ring with an odd slotBytes was never exercised at all. Codex proved the gap by
  // making QuantumRing reject odd widths: the fuzzer and all 5,124 unit checks
  // stayed green. Drawing it from its own byte decouples the two.
  const u8  control   = data[2];
  data += 3;
  size -= 3;

  // Allocate for the geometry only when it is one the ring would accept; otherwise
  // hand it a real but deliberately undersized buffer, because a ring that reports
  // invalid must never touch its storage.
  const bool plausible = (slotCount >= 2u) && (slotCount <= QuantumRing::kMaxSlots) &&
                         ((slotCount & (slotCount - 1u)) == 0u);
  // One input bit decides whether a PLAUSIBLE geometry gets the buffer it needs or
  // one byte less. The short arm is the case this target structurally could not
  // reach before -- it always sized via storageBytes(), so a ring that trusted an
  // under-allocated buffer was invisible to it. See QuantumRing's constructor.
  const bool starve = (control & 1u) != 0u;
  const std::size_t need =
      plausible ? QuantumRing::storageBytes(slotCount, slotBytes) : std::size_t{1};
  const std::size_t give = (plausible && starve && need > 1u) ? need - 1u : need;
  std::vector<u8> storage(give, 0u);

  QuantumRing ring(storage.data(), storage.size(), slotCount, slotBytes);
  const bool shouldBeValid = plausible && (give >= need);
  if (ring.valid() != shouldBeValid) { __builtin_trap(); }

  std::vector<u8> payload(slotBytes + 8u, 0u);
  std::vector<u8> out(slotBytes + 8u, 0u);

  u32 pushed = 0u;
  u32 popped = 0u;

  // The expected-FIFO oracle. Without it this target checked only COUNTS and the
  // length ceiling, so a ring that returned the wrong slot, the wrong length, or
  // stale bytes from a previous round passed every check -- which is the one class
  // of defect a ring buffer actually has. Found by Codex on PR #3155.
  //
  // Bounded by construction: the ring refuses a push at depth == slotCount, so this
  // never holds more than slotCount entries of at most slotBytes bytes each, and
  // both are drawn from a single byte. No cap of its own is needed, and adding one
  // would silently make the oracle disagree with a correct ring.
  std::deque<std::vector<u8>> expected;

  for (size_t i = 0; i < size; ++i) {
    const u8 op = data[i];
    if ((op & 1u) == 0u) {
      // push, with a length drawn from the byte so oversized lengths occur
      // Two bytes, not one: `op >> 1` caps at 127, so kTooBig was unreachable for
      // any slotBytes >= 127 -- which is most of the geometries this target draws.
      const u32 raw = static_cast<u32>(op) |
                      (static_cast<u32>(i + 1u < size ? data[i + 1u] : 0u) << 8);
      const u32 len = raw % (slotBytes + 4u);
      // Stamp a monotonic counter into the first bytes. With `op + b` alone, two
      // pushes sharing an op byte were BYTE-IDENTICAL, so swapping them in the
      // queue was invisible to the oracle -- the one defect class the oracle
      // exists for.
      for (u32 b = 0u; b < len && b < payload.size(); ++b) {
        payload[b] = static_cast<u8>(op + b);
      }
      for (u32 b = 0u; b < 4u && b < len; ++b) {
        payload[b] = static_cast<u8>((pushed >> (8u * b)) & 0xFFu);
      }
      const RingResult r = ring.push(payload.data(), len);
      if (ring.valid()) {
        // The EXACT modeled result, not a two-sided predicate. `(r == kFull) !=
        // oracleFull` was satisfied whenever BOTH sides were false, so a valid
        // non-full push returning kBadArg, kInvalid or kTooBig passed -- and the
        // FIFO is not updated on those, so every later depth check stayed
        // consistent too. A conditional kBadArg mutation for 254-byte slots
        // survived this target AND all 5,124 unit checks. Found by Codex on
        // PR #3155.
        const bool oracleFull = expected.size() == static_cast<std::size_t>(slotCount);
        const RingResult want = (len > slotBytes) ? RingResult::kTooBig
                              : oracleFull        ? RingResult::kFull
                                                  : RingResult::kOk;
        if (r != want) { __builtin_trap(); }
      }
      if (r == RingResult::kOk) {
        ++pushed;
        expected.emplace_back(payload.begin(), payload.begin() + len);
      }
      if (!ring.valid() && r != RingResult::kInvalid) { __builtin_trap(); }
    } else {
      // A SENTINEL, not 0. The refusal check below asserts pop() zeroed *outBytes,
      // and initialising to 0 makes that assertion true before pop() is even
      // called -- it survived its own mutant (deleting `*outBytes = 0u` from pop)
      // on the first try. Same vacuity the unit test's `got = 7u` exists for.
      u32 got = 0xFFFFFFFFu;
      // Drawn from the input, not fixed at out.size(). While it was fixed, len
      // could never exceed it, so pop's `len > dstCapacity` arm -- refuse rather
      // than truncate, the property this component exists for -- was unreachable
      // by the fuzzer. Found by @code-reviewer on PR #3155.
      //
      // TWO bytes, for the same reason the push length above uses two. `op >> 1`
      // caps at 127, so for any slotBytes in 128..256 -- half the geometries this
      // target draws -- every queued payload longer than 127 could only ever meet
      // a capacity too small to hold it. Those pops returned kBadArg forever and
      // the payload stayed at the FIFO front, so successful copying and FIFO
      // integrity were never validated at all at those sizes: the refusal arm was
      // reachable and the success arm was not. Found by Codex on PR #3155.
      const u32 rawCapacity = static_cast<u32>(op) |
                              (static_cast<u32>(i + 1u < size ? data[i + 1u] : 0u) << 8);
      const u32 capacity = static_cast<u32>(rawCapacity % (out.size() + 1u));
      const RingResult r = ring.pop(out.data(), capacity, &got);
      if (ring.valid()) {
        // Exact model, for the same reason as push above: an empty ring is
        // exactly an empty oracle, and a payload that cannot fit the caller's
        // buffer is exactly kBadArg. Anything else is a regression, including
        // the refusal arms that a one-sided check cannot see.
        const RingResult wantPop =
            expected.empty()                                        ? RingResult::kEmpty
          : (static_cast<u32>(expected.front().size()) > capacity)  ? RingResult::kBadArg
                                                                    : RingResult::kOk;
        if (r != wantPop) { __builtin_trap(); }
        // pop() zeroes *outBytes on every valid-ring refusal, so a regression that
        // returns the right status and leaves a stale count would otherwise pass.
        if (r != RingResult::kOk && got != 0u) { __builtin_trap(); }
      }
      if (r == RingResult::kBadArg) {
        // A refusal must consume NOTHING: the oracle front is untouched and the
        // depth is unchanged, which is what separates refusing from truncating.
        if (expected.size() != static_cast<std::size_t>(pushed - popped)) {
          __builtin_trap();
        }
      }
      if (r == RingResult::kOk) {
        if (got > capacity) { __builtin_trap(); }
        ++popped;
        if (got > slotBytes) { __builtin_trap(); }
        // FIFO order, exact length, exact bytes. A pop with nothing queued is
        // itself a defect -- the ring reported data the oracle never wrote.
        if (expected.empty()) { __builtin_trap(); }
        const std::vector<u8>& want = expected.front();
        if (got != static_cast<u32>(want.size())) { __builtin_trap(); }
        for (u32 b = 0u; b < got; ++b) {
          if (out[b] != want[b]) { __builtin_trap(); }
        }
        expected.pop_front();
      }
      if (!ring.valid() && r != RingResult::kInvalid) { __builtin_trap(); }
    }

    // Invariants that must hold after EVERY operation.
    if (ring.valid()) {
      if (ring.depth() > slotCount)     { __builtin_trap(); }
      if (ring.depth() != pushed - popped) { __builtin_trap(); }
      // NOTE: `expected.size() == pushed - popped` and `popped <= pushed` were
      // asserted here and both are true BY CONSTRUCTION -- the deque gains one
      // entry per pushed++ and loses one per popped++, with no path between them
      // that diverges. Neither read the ring. Removed rather than kept as
      // reassurance; `ring.depth() != pushed - popped` below is the one that works.
    }
  }
  return 0;
}
