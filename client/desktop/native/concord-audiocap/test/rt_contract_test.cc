// Ask sink_gate.h (and quantum_ring.h, which quantum_pump.h pulls in) for their
// test seams BEFORE including them. Done here rather than in the build files so
// no workflow, gyp target or compile command has to know: a production TU simply
// never writes this macro.
#define CONCORD_AUDIOCAP_TEST_SEAM 1

// concord-audiocap / rt — backend contract, gate, and repacketizer tests.
//
// A CHECK macro, no framework. This binary is built by CI under ASAN, UBSAN and
// TSAN, exactly as test/quantum_ring_test.cc is, and a test framework would only
// add a dependency to a target whose whole job is to be run under a sanitizer.
//
// This file is test code and is NOT bound by the JSF++ profile — that profile
// governs rt/, where an allocation is audible. Asserting is what this file is for.

#include "../rt/quantum_header.h"
#include "../rt/frame_pack.h"
#include "../rt/quantum_pump.h"
#include "../rt/capture_backend.h"
#include "../rt/sink_gate.h"
#include "../rt/teardown.h"

// EVERY include below is here for a symbol this file names directly, and <cstdlib>
// in particular is not optional: CHECK calls std::abort(), which libc++ on macOS
// supplies transitively and libstdc++ on the CI runners does not. A local macOS
// compile is therefore NOT sufficient verification for this file.
#include <cstdio>   // std::printf, std::fprintf, stderr
#include <cstdlib>  // std::abort
#include <cstring>  // std::memcpy, std::memcmp, std::memset
#include <atomic>
#include <chrono>
#include <mutex>
#include <thread>
#include <utility>
#include <vector>   // test-side quantum capture; rt/ itself allocates nothing

static int g_checks = 0;

#define CHECK(cond)                                                            \
  do {                                                                         \
    ++g_checks;                                                                \
    if (!(cond)) {                                                             \
      std::fprintf(stderr, "CHECK failed: %s (%s:%d)\n", #cond, __FILE__,      \
                   __LINE__);                                                  \
      std::abort();                                                            \
    }                                                                          \
  } while (0)

// Grants the counter-saturation test access to the private counters. Declared as
// a friend by quantum_pump.h; defined only here, exactly as QuantumRing's
// rollover accessor is. Saturation is 497 days of capture away, so the ceiling is
// SEEDED rather than reached.
namespace concord {
namespace audiocap {
namespace rt {
struct PumpCountersTestAccess {
  static void seed(QuantumPump& pump, u32 callbackTotal, u32 quantaTotal) noexcept {
    pump.callbackTotal_.store(callbackTotal, std::memory_order_relaxed);
    pump.quantaTotal_.store(quantaTotal, std::memory_order_relaxed);
  }

  /// The TEARDOWN witness, seeded separately because it is a separate question:
  /// the saturation test above is about what a JS consumer reads, and this one is
  /// about what the release-or-abandon decision rests on. Red team measured the
  /// ceiling at 9.6 s of a tight submit loop rather than the 497 days an honest
  /// 10 ms cadence implies, so seeding it is not even a long-horizon fiction.
  static void seedActivity(QuantumPump& pump, u32 activityTotal) noexcept {
    pump.activityTotal_.store(activityTotal, std::memory_order_relaxed);
  }
};
}  // namespace rt
}  // namespace audiocap
}  // namespace concord

namespace rt = concord::audiocap::rt;

// A DEADLINE, not a spin count, and the difference is what makes a failed
// precondition a FAILURE rather than a hang. `CHECK(spins < 2000000000ull)` is a
// bound no wall clock ever reaches: under the mutation the lock below exists to
// catch, the test sat in the spin instead of failing the assertion, and a lock
// that hangs under mutation cannot be told apart from one that passes. Found by
// @code-reviewer on PR #3262. Declared up here rather than beside its first
// user because two tasks now need it: the gate-concurrency case below and the
// teardown cases in Task 5.
template <typename Ready>
static bool waitUntil(Ready ready, rt::u32 timeoutMs) {
  const std::chrono::steady_clock::time_point deadline =
      std::chrono::steady_clock::now() + std::chrono::milliseconds(timeoutMs);
  while (!ready()) {
    if (std::chrono::steady_clock::now() >= deadline) { return false; }
    std::this_thread::yield();
  }
  return true;
}

// ---------------------------------------------------------------------------
// Task 1 — rt/frame_pack.h, the channel and layout repacketizer.
// ---------------------------------------------------------------------------

// A float32 sample is compared by its BYTES, never by value: the repacketizer is
// a byte shuffle and must not be tested through an arithmetic lens that would
// pass on a subtly-wrong-but-numerically-equal move.
static void writeSampleBytes(rt::u8* at, rt::u32 marker) {
  std::memcpy(at, &marker, sizeof(marker));
}
static rt::u32 readSampleBytes(const rt::u8* at) {
  rt::u32 out = 0u;
  std::memcpy(&out, at, sizeof(out));
  return out;
}

// The plan spelled these `0xL…`, `0xR…` and `0xM…` to make the intent readable;
// they are not valid hex. Left/right/mono are these three, and they are distinct
// in their top nibble so a channel that lands in the wrong slot is obvious in a
// hex dump rather than merely unequal.
static const rt::u32 kLeftMark = 0xC0000000u;
static const rt::u32 kRightMark = 0xD0000000u;
static const rt::u32 kMonoMark = 0xE0000000u;

static void test_packFrames_interleavedStereoIsAStraightCopy() {
  rt::u8 src[8u * 2u * 4u];
  for (rt::u32 i = 0u; i < 16u; ++i) { writeSampleBytes(&src[i * 4u], 0xA0000000u | i); }
  const rt::u8* planes[1] = {src};

  rt::u8 dst[8u * 2u * 4u];
  std::memset(dst, 0, sizeof(dst));
  CHECK(rt::packFrames(planes, 2u, true, 0u, 8u, dst));
  CHECK(std::memcmp(src, dst, sizeof(src)) == 0);
}

static void test_packFrames_planarStereoInterleaves() {
  rt::u8 left[4u * 4u];
  rt::u8 right[4u * 4u];
  for (rt::u32 f = 0u; f < 4u; ++f) {
    writeSampleBytes(&left[f * 4u],  kLeftMark | f);
    writeSampleBytes(&right[f * 4u], kRightMark | f);
  }
  const rt::u8* planes[2] = {left, right};

  rt::u8 dst[4u * 2u * 4u];
  std::memset(dst, 0, sizeof(dst));
  CHECK(rt::packFrames(planes, 2u, false, 0u, 4u, dst));
  for (rt::u32 f = 0u; f < 4u; ++f) {
    CHECK(readSampleBytes(&dst[(f * 2u + 0u) * 4u]) == (kLeftMark | f));
    CHECK(readSampleBytes(&dst[(f * 2u + 1u) * 4u]) == (kRightMark | f));
  }
}

static void test_packFrames_monoDuplicatesIntoBothChannels() {
  rt::u8 mono[3u * 4u];
  for (rt::u32 f = 0u; f < 3u; ++f) { writeSampleBytes(&mono[f * 4u], kMonoMark | f); }
  const rt::u8* planes[1] = {mono};

  rt::u8 dst[3u * 2u * 4u];
  std::memset(dst, 0, sizeof(dst));
  CHECK(rt::packFrames(planes, 1u, true, 0u, 3u, dst));
  for (rt::u32 f = 0u; f < 3u; ++f) {
    const rt::u32 v = kMonoMark | f;
    CHECK(readSampleBytes(&dst[(f * 2u + 0u) * 4u]) == v);
    CHECK(readSampleBytes(&dst[(f * 2u + 1u) * 4u]) == v);
  }

  // Mono has exactly one plane in BOTH layouts, so the interleaved flag must not
  // change where the samples come from. A branch that read srcPlanes[1] for
  // planar mono would read a plane the caller never supplied.
  rt::u8 planarDst[3u * 2u * 4u];
  std::memset(planarDst, 0, sizeof(planarDst));
  CHECK(rt::packFrames(planes, 1u, false, 0u, 3u, planarDst));
  CHECK(std::memcmp(dst, planarDst, sizeof(dst)) == 0);
}

static void test_packFrames_srcFrameOffsetSkipsWholeFrames() {
  rt::u8 src[6u * 2u * 4u];
  for (rt::u32 i = 0u; i < 12u; ++i) { writeSampleBytes(&src[i * 4u], 0xB0000000u | i); }
  const rt::u8* planes[1] = {src};

  rt::u8 dst[2u * 2u * 4u];
  std::memset(dst, 0, sizeof(dst));
  CHECK(rt::packFrames(planes, 2u, true, 4u, 2u, dst));   // start at frame 4
  CHECK(readSampleBytes(&dst[0]) == (0xB0000000u | 8u));  // frame 4 == sample 8
  CHECK(readSampleBytes(&dst[4]) == (0xB0000000u | 9u));
  CHECK(readSampleBytes(&dst[8]) == (0xB0000000u | 10u));
  CHECK(readSampleBytes(&dst[12]) == (0xB0000000u | 11u));

  // The offset counts FRAMES in each plane, not interleaved samples: a planar
  // reader that multiplied by kChannels would land one channel-pair too far in.
  rt::u8 leftPlane[6u * 4u];
  rt::u8 rightPlane[6u * 4u];
  for (rt::u32 f = 0u; f < 6u; ++f) {
    writeSampleBytes(&leftPlane[f * 4u],  kLeftMark | f);
    writeSampleBytes(&rightPlane[f * 4u], kRightMark | f);
  }
  const rt::u8* planarPlanes[2] = {leftPlane, rightPlane};
  rt::u8 planarDst[2u * 2u * 4u];
  std::memset(planarDst, 0, sizeof(planarDst));
  CHECK(rt::packFrames(planarPlanes, 2u, false, 4u, 2u, planarDst));
  CHECK(readSampleBytes(&planarDst[0]) == (kLeftMark | 4u));
  CHECK(readSampleBytes(&planarDst[4]) == (kRightMark | 4u));
  CHECK(readSampleBytes(&planarDst[8]) == (kLeftMark | 5u));
  CHECK(readSampleBytes(&planarDst[12]) == (kRightMark | 5u));
}

static void test_packFrames_refusesBadInput() {
  rt::u8 buf[8u];
  std::memset(buf, 0, sizeof(buf));
  const rt::u8* planes[1] = {buf};
  CHECK(!rt::packFrames(nullptr, 2u, true, 0u, 1u, buf));
  CHECK(!rt::packFrames(planes, 2u, true, 0u, 1u, nullptr));
  CHECK(!rt::packFrames(planes, 0u, true, 0u, 1u, buf));
  CHECK(!rt::packFrames(planes, 3u, true, 0u, 1u, buf));   // >2ch is refused, not downmixed
  CHECK(!rt::packFrames(planes, 2u, true, 0u, 0u, buf));   // a zero-frame callback

  // A NULL PLANE is refused before any address inside it is formed. The
  // reference implementation in the plan tested `&plane[0][off] == nullptr`
  // AFTER forming that address, which is undefined behaviour on a null plane and
  // is a check UBSAN would fire on rather than a check that holds.
  const rt::u8* nullFirst[2] = {nullptr, buf};
  CHECK(!rt::packFrames(nullFirst, 2u, false, 0u, 1u, buf));
  CHECK(!rt::packFrames(nullFirst, 1u, true, 0u, 1u, buf));
  const rt::u8* nullSecond[2] = {buf, nullptr};
  CHECK(!rt::packFrames(nullSecond, 2u, false, 0u, 1u, buf));
  // Interleaved stereo reads ONE plane, so a null second plane is not its
  // business and must not be refused on that ground.
  rt::u8 wide[1u * 2u * 4u];
  std::memset(wide, 0, sizeof(wide));
  CHECK(rt::packFrames(nullSecond, 2u, true, 0u, 1u, wide));

  // Every refusal above wrote nothing: a half-packed destination is
  // indistinguishable downstream from correctly packed silence.
  for (rt::u32 i = 0u; i < sizeof(buf); ++i) { CHECK(buf[i] == 0u); }
}

// ---------------------------------------------------------------------------
// Task 2 — rt/quantum_pump.h, the accumulator and the counters.
// ---------------------------------------------------------------------------

// Independent little-endian readers. The encoder assembles bytes by hand; a test
// that read them back with a memcpy of the native representation would agree with
// it on this host and on no other.
static rt::u16 readU16LE(const rt::u8* b, rt::u32 off) {
  return static_cast<rt::u16>(static_cast<rt::u16>(b[off]) |
                              static_cast<rt::u16>(static_cast<rt::u16>(b[off + 1u]) << 8));
}
static rt::u32 readU32LE(const rt::u8* b, rt::u32 off) {
  rt::u32 v = 0u;
  for (rt::u32 i = 0u; i < 4u; ++i) { v |= static_cast<rt::u32>(b[off + i]) << (8u * i); }
  return v;
}
static rt::u64 readU64LE(const rt::u8* b, rt::u32 off) {
  rt::u64 v = 0u;
  for (rt::u32 i = 0u; i < 8u; ++i) { v |= static_cast<rt::u64>(b[off + i]) << (8u * i); }
  return v;
}

// One frame's left sample carries `tag | frameIndex`; the right carries the same
// index under a different tag. Both channels are therefore distinguishable, so a
// pump that duplicated, swapped or dropped a channel cannot pass a content
// assertion, and the frame index makes ACCUMULATOR ORDER observable -- which is
// the whole point of the 479-then-1 case.
static const rt::u32 kLeftTag  = 0x11000000u;
static const rt::u32 kRightTag = 0x22000000u;

static void fillFrames(rt::u8* dst, rt::u32 firstFrame, rt::u32 frames) {
  for (rt::u32 f = 0u; f < frames; ++f) {
    writeSampleBytes(&dst[(f * 2u + 0u) * 4u], kLeftTag | (firstFrame + f));
    writeSampleBytes(&dst[(f * 2u + 1u) * 4u], kRightTag | (firstFrame + f));
  }
}

// A test double for the ring + signal pair, so the pump is exercised with no
// N-API in the picture -- which is the property that lets rt/ be fuzzed and
// sanitized on Linux at all.
struct PumpHarness {
  rt::u8 storage[rt::QuantumRing::storageBytes(rt::kRingSlots, rt::kQuantumBytes)];
  rt::QuantumRing ring;
  rt::u32 signalCount;
  rt::QuantumPump pump;

  PumpHarness()
      : storage(),
        ring(storage, sizeof(storage), rt::kRingSlots, rt::kQuantumBytes),
        signalCount(0u),
        pump(ring, &PumpHarness::onSignal, this) {
    CHECK(ring.valid());
  }
  static void onSignal(void* arg) {
    static_cast<PumpHarness*>(arg)->signalCount += 1u;
  }
};

static void drainAll(rt::QuantumRing& ring, std::vector<std::vector<rt::u8>>& out) {
  std::vector<rt::u8> scratch(rt::kQuantumBytes, 0u);
  rt::u32 got = 0u;
  while (ring.pop(scratch.data(), rt::kQuantumBytes, &got) == rt::RingResult::kOk) {
    // A short quantum would be a truncated one, and the far end cannot tell a
    // truncated transfer from a truncated capture.
    CHECK(got == rt::kQuantumBytes);
    out.push_back(std::vector<rt::u8>(scratch.begin(), scratch.begin() + got));
  }
}

// Every field the protocol pins is checked, not just seq: a pump that stamped
// its own channel count or sample rate would produce a quantum the consumer
// rejects, and that failure must surface here rather than three processes away.
static void checkWellFormedHeader(const std::vector<rt::u8>& q, rt::u32 expectSeq) {
  CHECK(q.size() == rt::kQuantumBytes);
  const rt::u8* b = q.data();
  CHECK(readU16LE(b, rt::kOffMagic) == rt::kMagic);
  CHECK(b[rt::kOffVersion] == rt::kHeaderVersion);
  CHECK(b[rt::kOffFlags] == 0u);
  CHECK(readU32LE(b, rt::kOffSeq) == expectSeq);
  CHECK(readU32LE(b, rt::kOffSampleRate) == rt::kSampleRate);
  CHECK(readU16LE(b, rt::kOffChannels) == rt::kChannels);
  CHECK(readU16LE(b, rt::kOffFrameCount) == rt::kFrameCount);
}

static rt::u32 sampleAt(const std::vector<rt::u8>& q, rt::u32 frame, rt::u32 channel) {
  return readU32LE(q.data(), rt::kHeaderBytes + ((frame * 2u) + channel) * 4u);
}

static void test_pump_emitsExactlyOneQuantumPer480Frames() {
  // 480 frames in ONE callback -> exactly one quantum.
  {
    PumpHarness h;
    h.pump.reset(1000u);
    std::vector<rt::u8> src(static_cast<std::size_t>(rt::kFrameCount) * 2u * 4u, 0u);
    fillFrames(src.data(), 0u, rt::kFrameCount);
    const rt::u8* planes[1] = {src.data()};

    h.pump.noteCallback();
    CHECK(h.pump.submit(planes, 2u, true, rt::kFrameCount, 5000u));

    std::vector<std::vector<rt::u8>> got;
    drainAll(h.ring, got);
    CHECK(got.size() == 1u);
    checkWellFormedHeader(got[0], 0u);
    CHECK(h.signalCount == 1u);
    CHECK(h.pump.counters().quantaTotal == 1u);
    CHECK(h.pump.counters().callbackTotal == 1u);
    // Content, so the samples are proven to land after the header and in order.
    for (rt::u32 f = 0u; f < rt::kFrameCount; ++f) {
      CHECK(sampleAt(got[0], f, 0u) == (kLeftTag | f));
      CHECK(sampleAt(got[0], f, 1u) == (kRightTag | f));
    }
  }

  // 479 then 1 -> still exactly one quantum, emitted on the SECOND callback.
  // This is the case a naive per-callback implementation gets wrong.
  {
    PumpHarness h;
    h.pump.reset(1000u);
    std::vector<rt::u8> first(479u * 2u * 4u, 0u);
    fillFrames(first.data(), 0u, 479u);
    const rt::u8* firstPlanes[1] = {first.data()};

    h.pump.noteCallback();
    CHECK(h.pump.submit(firstPlanes, 2u, true, 479u, 5000u));

    std::vector<std::vector<rt::u8>> afterFirst;
    drainAll(h.ring, afterFirst);
    CHECK(afterFirst.empty());          // NOTHING is emitted on a partial quantum
    CHECK(h.signalCount == 0u);
    CHECK(h.pump.counters().quantaTotal == 0u);

    rt::u8 second[1u * 2u * 4u];
    fillFrames(second, 479u, 1u);
    const rt::u8* secondPlanes[1] = {second};
    h.pump.noteCallback();
    CHECK(h.pump.submit(secondPlanes, 2u, true, 1u, 6000u));

    std::vector<std::vector<rt::u8>> got;
    drainAll(h.ring, got);
    CHECK(got.size() == 1u);
    checkWellFormedHeader(got[0], 0u);
    CHECK(h.signalCount == 1u);
    CHECK(h.pump.counters().callbackTotal == 2u);
    CHECK(h.pump.counters().quantaTotal == 1u);
    // THE ACCUMULATOR ASSERTION: frames 0..478 came from callback one and frame
    // 479 from callback two, in that order, with nothing overwritten between.
    for (rt::u32 f = 0u; f < rt::kFrameCount; ++f) {
      CHECK(sampleAt(got[0], f, 0u) == (kLeftTag | f));
      CHECK(sampleAt(got[0], f, 1u) == (kRightTag | f));
    }
  }
}

static void test_pump_regroupsAcrossCallbackSizes() {
  const rt::u32 kChunks[6] = {1u, 7u, 479u, 480u, 481u, 4096u};
  const rt::u32 kTotalFrames = 4800u;   // exactly 10 quanta

  for (rt::u32 c = 0u; c < 6u; ++c) {
    const rt::u32 chunk = kChunks[c];
    PumpHarness h;
    h.pump.reset(1000u);

    std::vector<rt::u8> src(static_cast<std::size_t>(chunk) * 2u * 4u, 0u);
    std::vector<std::vector<rt::u8>> got;

    rt::u32 fed = 0u;
    while (fed < kTotalFrames) {
      rt::u32 take = chunk;
      if ((kTotalFrames - fed) < take) { take = kTotalFrames - fed; }
      fillFrames(src.data(), fed, take);
      const rt::u8* planes[1] = {src.data()};
      h.pump.noteCallback();
      CHECK(h.pump.submit(planes, 2u, true, take, 1000000u + fed));
      fed += take;
      // Drained as we go, so the 8-slot ring cannot become the thing under test.
      drainAll(h.ring, got);
    }

    CHECK(fed == kTotalFrames);
    CHECK(got.size() == 10u);
    CHECK(h.pump.counters().quantaTotal == 10u);
    CHECK(h.signalCount == 10u);
    for (rt::u32 q = 0u; q < got.size(); ++q) {
      // seq runs 0..9 with NO gap: a gap is the independent drop witness, and
      // nothing was dropped here.
      checkWellFormedHeader(got[q], q);
      // Frame content is continuous ACROSS quanta as well as within one, which
      // is what a chunk size that does not divide 480 is here to break.
      for (rt::u32 f = 0u; f < rt::kFrameCount; ++f) {
        const rt::u32 absolute = (q * rt::kFrameCount) + f;
        CHECK(sampleAt(got[q], f, 0u) == (kLeftTag | absolute));
        CHECK(sampleAt(got[q], f, 1u) == (kRightTag | absolute));
      }
    }
  }
}

static void test_pump_headerCarriesFirstFrameTimestamp() {
  PumpHarness h;
  h.pump.reset(1000u);

  const rt::u64 kFirstTs  = 700000000ull;
  const rt::u64 kSecondTs = 900000000ull;

  std::vector<rt::u8> first(400u * 2u * 4u, 0u);
  fillFrames(first.data(), 0u, 400u);
  const rt::u8* firstPlanes[1] = {first.data()};
  h.pump.noteCallback();
  CHECK(h.pump.submit(firstPlanes, 2u, true, 400u, kFirstTs));

  std::vector<rt::u8> second(80u * 2u * 4u, 0u);
  fillFrames(second.data(), 400u, 80u);
  const rt::u8* secondPlanes[1] = {second.data()};
  h.pump.noteCallback();
  CHECK(h.pump.submit(secondPlanes, 2u, true, 80u, kSecondTs));

  std::vector<std::vector<rt::u8>> got;
  drainAll(h.ring, got);
  CHECK(got.size() == 1u);
  // The FIRST callback's clock, not the one that happened to complete the
  // quantum. Stamping the completing callback would make every timestamp late by
  // however long the accumulator held the partial.
  CHECK(readU64LE(got[0].data(), rt::kOffCaptureTimestampNs) == kFirstTs);
  CHECK(readU64LE(got[0].data(), rt::kOffCaptureTimestampNs) != kSecondTs);
}

static void test_pump_headerAdvancesTimestampWithinOneCallback() {
  // A callback larger than one quantum yields several quanta from ONE clock
  // reading. Stamping them all with that reading makes two quanta claim the same
  // instant, which downstream reads as a timestamp that stalled.
  PumpHarness h;
  h.pump.reset(1000u);

  const rt::u64 kTs = 500000000ull;
  const rt::u32 kFrames = 960u;                 // exactly two quanta
  std::vector<rt::u8> src(static_cast<std::size_t>(kFrames) * 2u * 4u, 0u);
  fillFrames(src.data(), 0u, kFrames);
  const rt::u8* planes[1] = {src.data()};
  h.pump.noteCallback();
  CHECK(h.pump.submit(planes, 2u, true, kFrames, kTs));

  std::vector<std::vector<rt::u8>> got;
  drainAll(h.ring, got);
  CHECK(got.size() == 2u);
  CHECK(readU64LE(got[0].data(), rt::kOffCaptureTimestampNs) == kTs);
  // 480 frames at 48 kHz is exactly 10 ms.
  CHECK(readU64LE(got[1].data(), rt::kOffCaptureTimestampNs) == kTs + 10000000ull);
}

static void test_pump_ringFullCountsDropAndAdvancesSeq() {
  PumpHarness h;
  h.pump.reset(1000u);

  std::vector<rt::u8> src(static_cast<std::size_t>(rt::kFrameCount) * 2u * 4u, 0u);
  const rt::u8* planes[1] = {src.data()};

  // kRingSlots quanta fill the ring; two more have nowhere to go. NOT drained.
  const rt::u32 kOver = 2u;
  for (rt::u32 q = 0u; q < rt::kRingSlots + kOver; ++q) {
    fillFrames(src.data(), q * rt::kFrameCount, rt::kFrameCount);
    h.pump.noteCallback();
    CHECK(h.pump.submit(planes, 2u, true, rt::kFrameCount, 1000000u + q));
  }

  // The ring counted the drop, at the ONE drop site.
  CHECK(h.ring.overrun() == kOver);
  // quantaTotal counts what was PUSHED, so it does not advance for a drop --
  // otherwise it and the ring's counter would disagree about the same event.
  CHECK(h.pump.counters().quantaTotal == rt::kRingSlots);
  CHECK(h.pump.counters().callbackTotal == rt::kRingSlots + kOver);
  // No signal is raised for a quantum that was never pushed.
  CHECK(h.signalCount == rt::kRingSlots);

  std::vector<std::vector<rt::u8>> got;
  drainAll(h.ring, got);
  CHECK(got.size() == rt::kRingSlots);
  for (rt::u32 q = 0u; q < got.size(); ++q) { checkWellFormedHeader(got[q], q); }

  // SEQ STILL ADVANCED over the two dropped quanta: the gap at the far end is
  // the drop witness that is independent of the counter, and it still moves if
  // the counter ever lies.
  fillFrames(src.data(), 0u, rt::kFrameCount);
  h.pump.noteCallback();
  CHECK(h.pump.submit(planes, 2u, true, rt::kFrameCount, 2000000u));
  std::vector<std::vector<rt::u8>> after;
  drainAll(h.ring, after);
  CHECK(after.size() == 1u);
  checkWellFormedHeader(after[0], rt::kRingSlots + kOver);   // 8, 9 dropped -> 10

  // overrunTotal is stamped from the ring, so a consumer reading only headers
  // still sees the drops that happened before this quantum.
  CHECK(readU32LE(after[0].data(), rt::kOffOverrunTotal) == kOver);
}

static void test_pump_countersSurviveReset() {
  PumpHarness h;
  h.pump.reset(1000u);

  std::vector<rt::u8> src(static_cast<std::size_t>(rt::kFrameCount) * 2u * 4u, 0u);
  fillFrames(src.data(), 0u, rt::kFrameCount);
  const rt::u8* planes[1] = {src.data()};
  for (rt::u32 q = 0u; q < 3u; ++q) {
    h.pump.noteCallback();
    CHECK(h.pump.submit(planes, 2u, true, rt::kFrameCount, 1000000u + q));
  }
  std::vector<std::vector<rt::u8>> got;
  drainAll(h.ring, got);
  CHECK(got.size() == 3u);

  rt::PumpCounters before = h.pump.counters();
  CHECK(before.callbackTotal == 3u);
  CHECK(before.quantaTotal == 3u);
  CHECK(!before.faulted);
  CHECK(before.faultReason == static_cast<rt::u8>(rt::PumpFault::kNone));

  // A FAULT does not zero the counters. A post-mortem status() read is the only
  // evidence a dead capture leaves, and a fault that reset it would erase the
  // one thing worth knowing -- whether callbacks were arriving at all.
  h.pump.fault(rt::PumpFault::kDeviceLost);
  rt::PumpCounters afterFault = h.pump.counters();
  CHECK(afterFault.callbackTotal == 3u);
  CHECK(afterFault.quantaTotal == 3u);
  CHECK(afterFault.faulted);
  CHECK(afterFault.faultReason == static_cast<rt::u8>(rt::PumpFault::kDeviceLost));

  // The FIRST fault wins. A cascade -- a device loss that then reads as a format
  // change -- must not overwrite the cause with its own consequence.
  h.pump.fault(rt::PumpFault::kFormatChanged);
  CHECK(h.pump.counters().faultReason == static_cast<rt::u8>(rt::PumpFault::kDeviceLost));

  // reset() clears the ACCUMULATOR and nothing else. Feeding a partial quantum,
  // resetting, and then feeding one frame must emit nothing: a reset that kept
  // 479 stale frames would splice audio from before the reset into the first
  // quantum after it.
  std::vector<rt::u8> partial(479u * 2u * 4u, 0u);
  fillFrames(partial.data(), 0u, 479u);
  const rt::u8* partialPlanes[1] = {partial.data()};
  h.pump.noteCallback();
  CHECK(h.pump.submit(partialPlanes, 2u, true, 479u, 3000000u));

  h.pump.reset(4000000u);
  rt::PumpCounters afterReset = h.pump.counters();
  CHECK(afterReset.callbackTotal == 4u);          // survives
  CHECK(afterReset.quantaTotal == 3u);            // survives
  CHECK(afterReset.faulted);                      // survives
  CHECK(afterReset.faultReason == static_cast<rt::u8>(rt::PumpFault::kDeviceLost));

  rt::u8 one[1u * 2u * 4u];
  fillFrames(one, 0u, 1u);
  const rt::u8* onePlanes[1] = {one};
  h.pump.noteCallback();
  CHECK(h.pump.submit(onePlanes, 2u, true, 1u, 5000000u));
  std::vector<std::vector<rt::u8>> afterResetQuanta;
  drainAll(h.ring, afterResetQuanta);
  CHECK(afterResetQuanta.empty());

  // And seq restarts from 0 with the accumulator, so a fresh capture's first
  // quantum is seq 0 rather than continuing a previous share's numbering.
  std::vector<rt::u8> rest(479u * 2u * 4u, 0u);
  fillFrames(rest.data(), 1u, 479u);
  const rt::u8* restPlanes[1] = {rest.data()};
  h.pump.noteCallback();
  CHECK(h.pump.submit(restPlanes, 2u, true, 479u, 6000000u));
  drainAll(h.ring, afterResetQuanta);
  CHECK(afterResetQuanta.size() == 1u);
  checkWellFormedHeader(afterResetQuanta[0], 0u);
}

static void test_pump_refusesBadSubmissions() {
  PumpHarness h;
  h.pump.reset(1000u);
  rt::u8 src[4u * 2u * 4u];
  std::memset(src, 0, sizeof(src));
  const rt::u8* planes[1] = {src};

  CHECK(!h.pump.submit(nullptr, 2u, true, 4u, 1u));
  CHECK(!h.pump.submit(planes, 0u, true, 4u, 1u));
  CHECK(!h.pump.submit(planes, 3u, true, 4u, 1u));   // >2ch refused, not downmixed
  CHECK(!h.pump.submit(planes, 2u, true, 0u, 1u));

  // A refused submission changes nothing: no quantum, no partial state, and the
  // accumulator still needs a full 480 frames.
  std::vector<std::vector<rt::u8>> got;
  drainAll(h.ring, got);
  CHECK(got.empty());
  CHECK(h.pump.counters().quantaTotal == 0u);

  std::vector<rt::u8> full(static_cast<std::size_t>(rt::kFrameCount) * 2u * 4u, 0u);
  fillFrames(full.data(), 0u, rt::kFrameCount);
  const rt::u8* fullPlanes[1] = {full.data()};
  h.pump.noteCallback();
  CHECK(h.pump.submit(fullPlanes, 2u, true, rt::kFrameCount, 2u));
  drainAll(h.ring, got);
  CHECK(got.size() == 1u);
  checkWellFormedHeader(got[0], 0u);
}

static void test_pump_faultNoneIsANoOp() {
  // fault() documents kNone as a no-op "so this can never clear a latched fault",
  // and nothing asserted either half of that. Two directions, and they fail
  // differently: a kNone that SET the flag invents a fault the pump never had --
  // and with the fault pair now published flag-last, an inventing kNone is exactly
  // the writer that produces {faulted: true, faultReason: "None"} -- while a kNone
  // that CLEARED one erases the post-mortem evidence of a real capture failure.
  PumpHarness h;
  h.pump.reset(1000u);

  // On a clean pump: it cannot set the flag, and it cannot move the reason.
  h.pump.fault(rt::PumpFault::kNone);
  CHECK(!h.pump.counters().faulted);
  CHECK(h.pump.counters().faultReason == static_cast<rt::u8>(rt::PumpFault::kNone));

  // On a latched pump: it cannot clear either field. The positive control is one
  // line up -- the latch really is set -- so this is not a case that would pass
  // against a pump that never faults at all.
  h.pump.fault(rt::PumpFault::kPermissionLost);
  CHECK(h.pump.counters().faulted);
  h.pump.fault(rt::PumpFault::kNone);
  CHECK(h.pump.counters().faulted);
  CHECK(h.pump.counters().faultReason ==
        static_cast<rt::u8>(rt::PumpFault::kPermissionLost));
}

static void test_pump_countersSaturateRatherThanWrapping() {
  // The counters are the post-mortem evidence, and a wrapped u32 reads as "no
  // callbacks at all" -- the one answer they must never be able to give. The
  // ceiling is 497 days of capture away, so it is SEEDED, exactly as the ring's
  // overrun ceiling is.
  PumpHarness h;
  h.pump.reset(1000u);
  rt::PumpCountersTestAccess::seed(h.pump, rt::QuantumRing::kOverrunSaturated - 1u,
                                   rt::QuantumRing::kOverrunSaturated - 1u);
  std::vector<rt::u8> src(static_cast<std::size_t>(rt::kFrameCount) * 2u * 4u, 0u);
  fillFrames(src.data(), 0u, rt::kFrameCount);
  const rt::u8* planes[1] = {src.data()};

  h.pump.noteCallback();
  CHECK(h.pump.submit(planes, 2u, true, rt::kFrameCount, 1u));
  CHECK(h.pump.counters().callbackTotal == rt::QuantumRing::kOverrunSaturated);
  CHECK(h.pump.counters().quantaTotal == rt::QuantumRing::kOverrunSaturated);

  fillFrames(src.data(), rt::kFrameCount, rt::kFrameCount);
  h.pump.noteCallback();
  CHECK(h.pump.submit(planes, 2u, true, rt::kFrameCount, 2u));
  CHECK(h.pump.counters().callbackTotal == rt::QuantumRing::kOverrunSaturated);
  CHECK(h.pump.counters().quantaTotal == rt::QuantumRing::kOverrunSaturated);
}

// ---------------------------------------------------------------------------
// Task 3 — rt/sink_gate.h, the teardown gate. F3's enforcement.
// ---------------------------------------------------------------------------

static void test_gate_enterSucceedsWhenOpen() {
  rt::SinkGate g;
  CHECK(g.quiesced());           // nobody inside a fresh gate
  CHECK(g.tryEnter());
  CHECK(!g.quiesced());          // one caller is inside
  g.leave();
  CHECK(g.quiesced());
}

static void test_gate_enterFailsAfterClose() {
  rt::SinkGate g;
  g.close();
  CHECK(!g.tryEnter());
  CHECK(g.quiesced());           // the failed enter backed itself out
  // Permanent. A gate that could reopen would let a capture the user ended
  // resume calling into JS, which is the whole harm this class exists to stop.
  CHECK(!g.tryEnter());
  CHECK(g.quiesced());
}

static void test_gate_closeDoesNotEvictAnInFlightCaller() {
  rt::SinkGate g;
  CHECK(g.tryEnter());
  g.close();
  CHECK(!g.quiesced());          // still inside -- the closer MUST wait
  // A second entrant is refused while the first is still in flight, and the
  // refusal must not decrement the first one's count.
  CHECK(!g.tryEnter());
  CHECK(!g.quiesced());
  g.leave();
  CHECK(g.quiesced());
}

static void test_gate_failedEnterPreservesTheClosedBit() {
  // tryEnter INCREMENTS BEFORE it tests the closed bit -- that ordering is what
  // makes a racing enter either counted by the closer or backed out by itself.
  // The cost of that order is that a refused enter transiently adds to the same
  // word that carries the flag, so the back-out must restore the word exactly.
  // A back-out that cleared the flag would reopen a closed gate.
  rt::SinkGate g;
  g.close();
  for (rt::u32 i = 0u; i < 1000u; ++i) {
    CHECK(!g.tryEnter());
  }
  CHECK(g.quiesced());
  CHECK(!g.tryEnter());

  // The same word survives a thousand refusals INTERLEAVED with a live caller.
  rt::SinkGate h;
  CHECK(h.tryEnter());
  h.close();
  for (rt::u32 i = 0u; i < 1000u; ++i) {
    CHECK(!h.tryEnter());
    CHECK(!h.quiesced());        // the in-flight caller is never lost
  }
  h.leave();
  CHECK(h.quiesced());
}

// The seam test. `closeOnFire` closes the gate at exactly the instant between
// tryEnter()'s increment and its closed-bit test.
struct GateProbeState {
  rt::SinkGate* gate;
  bool closeOnFire;
  bool quiescedDuringEnter;
  rt::u32 fired;
};

static void gateEnterProbe(void* arg) {
  GateProbeState* p = static_cast<GateProbeState*>(arg);
  p->fired += 1u;
  if (p->closeOnFire) { p->gate->close(); }
  // The caller is ALREADY COUNTED here. A closer looking at this instant must
  // therefore see a non-zero count.
  p->quiescedDuringEnter = p->gate->quiesced();
}

static void test_gate_entrantIsCountedBeforeItTestsTheClosedBit() {
  // Case 1: the gate is closed DURING the enter, in the window the ordering
  // exists to cover.
  {
    rt::SinkGate g;
    GateProbeState st = {&g, true, true, 0u};
    rt::SinkGate::setEnterProbe(&gateEnterProbe, &st);
    const bool entered = g.tryEnter();
    rt::SinkGate::setEnterProbe(nullptr, nullptr);

    CHECK(st.fired == 1u);
    // THE PROPERTY. A closer that looked at this instant saw the entrant. With
    // the test-before-count order it would have seen an empty gate and released
    // the threadsafe function out from under a caller about to be inside -- and
    // that is a use-after-release, not a missed quantum.
    CHECK(!st.quiescedDuringEnter);
    // AND IT IS ADMITTED, which is the correct half of the disjunction rather
    // than a leak. `prev` was read before the close, so this caller decided on
    // the state it actually saw -- and because it counted itself first, the
    // closer is now obliged to wait for it. The other half (a caller that reads
    // the bit and backs itself out) is test_gate_enterFailsAfterClose above.
    // A gate that EVICTED this caller instead would be the use-after-release
    // this class exists to prevent, dressed as tidiness.
    CHECK(entered);
    CHECK(g.closed());
    CHECK(!g.quiesced());          // the closer must wait
    g.leave();
    CHECK(g.quiesced());
  }

  // Case 2: nobody closes. The count is still taken first, so the same look
  // still sees the entrant -- the ordering is not conditional on a teardown.
  {
    rt::SinkGate g;
    GateProbeState st = {&g, false, true, 0u};
    rt::SinkGate::setEnterProbe(&gateEnterProbe, &st);
    const bool entered = g.tryEnter();
    rt::SinkGate::setEnterProbe(nullptr, nullptr);

    CHECK(st.fired == 1u);
    CHECK(!st.quiescedDuringEnter);
    CHECK(entered);
    CHECK(!g.quiesced());
    g.leave();
    CHECK(g.quiesced());
    CHECK(!g.closed());
  }
}

// THE REGRESSION-LOCK. Never delete: this encodes F3 together with the
// call-after-stop test in the backend suite below.
//
// The payload below is written NON-ATOMICALLY on purpose. It stands in for the
// threadsafe-function handle: the producer touches it while inside the gate, and
// the closer touches it only after quiesced() reads true. If leave() and the
// closer's read carried no happens-before, those two writes would be a data race
// -- and x86 TSO hides exactly that, which is why this test is worth nothing
// unless it is run under TSAN. Each producer owns its own slot, because the gate
// is a reference count and not mutual exclusion: two producers inside it at once
// is legal, and a shared slot would be a race the GATE is not claiming to stop.
static const rt::u32 kGateThreads = 4u;

struct GateWorker {
  rt::u32 enters;
  rt::u32 leaves;
  rt::u32 refusalsAfterFirst;
  bool    enteredAfterRefusal;
};

// REFUSALS, VISIBLE TO MAIN WHILE THE WORKERS ARE STILL RUNNING. The per-worker
// counters above are read only after the join, which is too late to DECIDE
// anything with -- and the decision main has to make is when to stop the
// workers. See the wait below.
static std::atomic<rt::u32> g_gateRefusals(0u);

static rt::u32 g_gatePayload[kGateThreads];

static void test_gate_concurrentEnterLeaveVersusClose() {
  rt::SinkGate gate;
  GateWorker workers[kGateThreads];
  std::atomic<rt::u32> totalEnters(0u);
  std::atomic<bool> stopHammering(false);

  for (rt::u32 i = 0u; i < kGateThreads; ++i) {
    workers[i].enters = 0u;
    workers[i].leaves = 0u;
    workers[i].refusalsAfterFirst = 0u;
    workers[i].enteredAfterRefusal = false;
    g_gatePayload[i] = 0u;
  }

  g_gateRefusals.store(0u, std::memory_order_relaxed);

  std::vector<std::thread> threads;
  for (rt::u32 i = 0u; i < kGateThreads; ++i) {
    threads.push_back(std::thread([&gate, &workers, &totalEnters, &stopHammering, i]() {
      bool sawClosed = false;
      while (!stopHammering.load(std::memory_order_relaxed)) {
        if (gate.tryEnter()) {
          if (sawClosed) {
            // THE GATE REOPENED. Recorded rather than aborted here, so the
            // assertion is made on the main thread with the others joined.
            workers[i].enteredAfterRefusal = true;
          }
          workers[i].enters += 1u;
          // Inside the gate: the one thing a producer does here in production is
          // the ring push and the call that follows it.
          g_gatePayload[i] = g_gatePayload[i] + 1u;
          totalEnters.fetch_add(1u, std::memory_order_relaxed);
          gate.leave();
          workers[i].leaves += 1u;
        } else {
          sawClosed = true;
          workers[i].refusalsAfterFirst += 1u;
          // Published AFTER the per-worker counter, so the sum read post-join is
          // never smaller than what main waited on.
          g_gateRefusals.fetch_add(1u, std::memory_order_relaxed);
          // Keep hammering after the first refusal: a gate that reopened under
          // contention would be caught by the branch above.
          if (workers[i].refusalsAfterFirst > 2000u) { break; }
        }
      }
    }));
  }

  // Do not close until the workers are demonstrably inside, or the test proves
  // only that closing an idle gate works.
  rt::u64 spins = 0u;
  while (totalEnters.load(std::memory_order_relaxed) < 1000u) {
    ++spins;
    CHECK(spins < 2000000000ull);
    std::this_thread::yield();
  }

  gate.close();

  // WAITED FOR, NOT HOPED FOR, and this is a FLAKE FIX rather than a new
  // assertion. The non-vacuity check at the end of this case -- that close()
  // refused somebody -- was a genuine race: every worker runs until
  // `stopHammering`, and main used to set that as soon as the gate read quiesced,
  // so a run in which all four workers were descheduled across close() and the
  // store woke them to `stopHammering == true` and exited with ZERO refusals.
  // Observed failing roughly 1 run in 10 under parallel build load. Nothing about
  // the gate was wrong in those runs; the test simply stopped its own workers
  // before the thing it wanted to observe could happen.
  //
  // This makes the refusal a PRECONDITION of proceeding rather than an outcome to
  // hope for. It terminates: after close() every tryEnter fails, the workers are
  // still running because `stopHammering` is set only below, and the one other way
  // out of a worker's loop (2000 refusals) implies the count this waits on.
  CHECK(waitUntil([] { return g_gateRefusals.load(std::memory_order_relaxed) > 0u; },
                  5000u));

  // The bounded wait napi/'s teardown performs, in miniature.
  rt::u64 waitSpins = 0u;
  while (!gate.quiesced()) {
    ++waitSpins;
    CHECK(waitSpins < 2000000000ull);
    std::this_thread::yield();
  }

  // THE UNSAFE WRITE, made safe only by the gate. In production this is
  // napi_release_threadsafe_function; here it is a plain store to the same
  // memory the producers were touching inside the gate.
  for (rt::u32 i = 0u; i < kGateThreads; ++i) {
    g_gatePayload[i] = 0xDEADBEEFu;
  }

  stopHammering.store(true, std::memory_order_relaxed);
  for (rt::u32 i = 0u; i < threads.size(); ++i) { threads[i].join(); }

  CHECK(gate.quiesced());
  rt::u32 observedEnters = 0u;
  for (rt::u32 i = 0u; i < kGateThreads; ++i) {
    // Every entrant left: an unbalanced pair would leave the gate permanently
    // un-quiesced and poison every future teardown.
    CHECK(workers[i].enters == workers[i].leaves);
    // The gate never reopened.
    CHECK(!workers[i].enteredAfterRefusal);
    observedEnters += workers[i].enters;
    // The producers stopped touching the payload before main wrote it.
    CHECK(g_gatePayload[i] == 0xDEADBEEFu);
  }
  CHECK(observedEnters >= 1000u);
  CHECK(observedEnters == totalEnters.load(std::memory_order_relaxed));
  // Not vacuous: close() actually refused somebody rather than landing after
  // every worker had already stopped. Deterministic now -- the wait above is what
  // makes it so -- and kept as an assertion because it is read post-join from the
  // PER-WORKER counters, which is the evidence the atomic only stands in for.
  rt::u32 totalRefusals = 0u;
  for (rt::u32 i = 0u; i < kGateThreads; ++i) { totalRefusals += workers[i].refusalsAfterFirst; }
  CHECK(totalRefusals > 0u);
  CHECK(totalRefusals >= g_gateRefusals.load(std::memory_order_relaxed));
}

// ---------------------------------------------------------------------------
// Task 4 — rt/capture_backend.h, the contract #3196 and PR 2 both inherit.
//
// TWO FAKES, ONE SUITE, and the second fake is the macOS shape proven on Linux.
// The point of this file is that a thread-driven backend and a callback-driven
// one satisfy the SAME post-condition by different mechanisms, and that neither
// is trusted: a third fake lies about it and the gate refuses it anyway.
// ---------------------------------------------------------------------------

// A QuantumPump whose signal goes through a SinkGate -- the composition napi/
// performs at Task 5. `tsfnCalls` stands in for napi_call_threadsafe_function:
// the one thing the producer does inside the gate, and the one thing that must
// be unreachable after teardown.
struct GatedSink {
  rt::u8 storage[rt::QuantumRing::storageBytes(rt::kRingSlots, rt::kQuantumBytes)];
  rt::QuantumRing ring;
  rt::SinkGate gate;
  std::atomic<rt::u32> tsfnCalls;
  std::atomic<rt::u32> refusedCalls;
  rt::QuantumPump pump;

  GatedSink()
      : storage(),
        ring(storage, sizeof(storage), rt::kRingSlots, rt::kQuantumBytes),
        gate(),
        tsfnCalls(0u),
        refusedCalls(0u),
        pump(ring, &GatedSink::signal, this) {
    CHECK(ring.valid());
  }

  static void signal(void* arg) {
    GatedSink* s = static_cast<GatedSink*>(arg);
    if (!s->gate.tryEnter()) {
      // THE REFUSAL. In production this is the branch that makes a post-stop
      // quantum unable to reach a released threadsafe function.
      s->refusedCalls.fetch_add(1u, std::memory_order_relaxed);
      return;
    }
    s->tsfnCalls.fetch_add(1u, std::memory_order_relaxed);
    s->gate.leave();
  }

  // Design section 4.2 step 3, in miniature: spin, then sleep, to a ceiling.
  // False is the timeout branch -- the one that makes Task 5 ABANDON the handle
  // rather than release it, and latch `poisoned`.
  bool closeAndWait(rt::u32 budgetMs) {
    gate.close();
    for (rt::u32 spin = 0u; spin < 128u; ++spin) {
      if (gate.quiesced()) { return true; }
      std::this_thread::yield();
    }
    for (rt::u32 ms = 0u; ms < budgetMs; ++ms) {
      if (gate.quiesced()) { return true; }
      std::this_thread::sleep_for(std::chrono::milliseconds(1));
    }
    return gate.quiesced();
  }
};

// Everything a sink can be observed to have done, as one value, so "nothing
// happened after stop()" is a comparison rather than four.
struct SinkObservation {
  rt::u32 callbackTotal;
  rt::u32 quantaTotal;
  rt::u32 tsfnCalls;
  rt::u32 refusedCalls;
};

static SinkObservation observe(const GatedSink& sink) {
  const rt::PumpCounters c = sink.pump.counters();
  SinkObservation out;
  out.callbackTotal = c.callbackTotal;
  out.quantaTotal   = c.quantaTotal;
  out.tsfnCalls     = sink.tsfnCalls.load(std::memory_order_relaxed);
  out.refusedCalls  = sink.refusedCalls.load(std::memory_order_relaxed);
  return out;
}

// The host services rt/ refuses to name for itself (AV 22/25: no <stdio.h>, no
// <time.h>, and nothing here may name uv or <thread>). `join` is keyed on the
// SAME `arg` that was handed to `spawn` -- see the note in capture_backend.h.
struct FakeHost {
  static inline std::mutex mutex;
  static inline std::vector<std::pair<void*, std::thread>> threads;

  static bool spawn(void (*entry)(void*), void* arg) noexcept {
    std::lock_guard<std::mutex> lock(mutex);
    threads.emplace_back(arg, std::thread(entry, arg));
    return true;
  }

  static void join(void* handle) noexcept {
    std::thread claimed;
    {
      std::lock_guard<std::mutex> lock(mutex);
      for (std::size_t i = 0u; i < threads.size(); ++i) {
        if (threads[i].first == handle) {
          claimed = std::move(threads[i].second);
          threads.erase(threads.begin() + static_cast<std::ptrdiff_t>(i));
          break;
        }
      }
    }
    if (claimed.joinable()) { claimed.join(); }
  }

  static rt::u64 nowNs() noexcept {
    return static_cast<rt::u64>(
        std::chrono::duration_cast<std::chrono::nanoseconds>(
            std::chrono::steady_clock::now().time_since_epoch()).count());
  }
};

static const rt::HostServices kFakeHost = {&FakeHost::spawn, &FakeHost::join,
                                           &FakeHost::nowNs};

static rt::CaptureTarget oneTarget(rt::u32 pid) {
  rt::CaptureTarget t = {};
  t.pids[0] = pid;
  t.pidCount = 1u;
  t.allowDescendants = false;
  return t;
}

// FAKE A -- THREAD-DRIVEN. Satisfies the post-condition by JOINING. This is the
// shape the shipped synthetic producer already has.
class ThreadDrivenFake final : public rt::CaptureBackend {
 public:
  ThreadDrivenFake()
      : sink_(nullptr), host_(nullptr), running_(false), stopFlag_(false),
        calloutMark_(0u), source_() {}

  /// Bumped at the START of every callout, once it has passed the stop-flag
  /// test. The suite waits for it to CHANGE and then tears down, which puts the
  /// teardown at the beginning of a callout window rather than merely somewhere
  /// inside one -- see the note there.
  const std::atomic<rt::u32>& calloutMark() const { return calloutMark_; }

  rt::BackendStart start(const rt::CaptureTarget& target, rt::QuantumPump& sink,
                         const rt::HostServices& host) noexcept override {
    if (running_) { return rt::BackendStart::kAlreadyRunning; }
    if (target.pidCount == 0u) { return rt::BackendStart::kNoTarget; }
    sink_ = &sink;
    host_ = &host;
    stopFlag_.store(false, std::memory_order_release);
    if (!host.spawn(&ThreadDrivenFake::entry, this)) {
      return rt::BackendStart::kDeviceError;
    }
    running_ = true;
    return rt::BackendStart::kOk;
  }

  void stop() noexcept override {
    if (!running_) { return; }             // idempotent
    stopFlag_.store(true, std::memory_order_release);
    host_->join(this);                     // the post-condition, by joining
    running_ = false;
  }

 private:
  static void entry(void* arg) { static_cast<ThreadDrivenFake*>(arg)->run(); }

  void run() {
    rt::u32 frame = 0u;
    while (!stopFlag_.load(std::memory_order_acquire)) {
      // A CALLOUT, ONCE ENTERED, RUNS TO COMPLETION -- the stop flag was tested
      // before this one began. The sleep models its duration so the suite can
      // tear down while it is genuinely in flight, which is the only interesting
      // moment to tear down at.
      calloutMark_.fetch_add(1u, std::memory_order_release);
      std::this_thread::sleep_for(std::chrono::milliseconds(2));
      fillFrames(source_, frame, rt::kFrameCount);
      const rt::u8* planes[1] = {source_};
      sink_->noteCallback();
      (void)sink_->submit(planes, 2u, true, rt::kFrameCount,
                          static_cast<rt::u64>(frame) * 20833ull);
      frame += rt::kFrameCount;
    }
  }

  rt::QuantumPump* sink_;
  const rt::HostServices* host_;
  bool running_;
  std::atomic<bool> stopFlag_;
  std::atomic<rt::u32> calloutMark_;
  rt::u8 source_[static_cast<std::size_t>(rt::kFrameCount) * 2u * 4u];
};

// FAKE B -- CALLBACK-DRIVEN. Submits from a DETACHED foreign thread and joins
// NOTHING. It satisfies the post-condition by setting its own stop flag and
// spinning until its callback is observably out, which is exactly what
// AudioDeviceStop + AudioDeviceDestroyIOProcID buys on macOS and what
// IAudioClient::Stop buys on Windows.
//
// THIS IS THE MACOS SHAPE, PROVEN ON LINUX. It is the reason the contract is a
// post-condition and not "join the thread".
class CallbackDrivenFake final : public rt::CaptureBackend {
 public:
  explicit CallbackDrivenFake(const rt::SourceFormat& format)
      : format_(format), sink_(nullptr), running_(false), stopFlag_(false),
        calloutDone_(true), calloutMark_(0u), source_() {}

  const std::atomic<rt::u32>& calloutMark() const { return calloutMark_; }

  rt::BackendStart start(const rt::CaptureTarget& target, rt::QuantumPump& sink,
                         const rt::HostServices& /*host*/) noexcept override {
    if (running_) { return rt::BackendStart::kAlreadyRunning; }
    if (target.pidCount == 0u) { return rt::BackendStart::kNoTarget; }
    // Nothing is resampled and nothing is downmixed: the rule lives in one place
    // so two backends cannot drift on it.
    if (!rt::acceptsSourceFormat(format_)) {
      return rt::BackendStart::kUnsupportedFormat;
    }
    sink_ = &sink;
    stopFlag_.store(false, std::memory_order_release);
    calloutDone_.store(false, std::memory_order_release);
    std::thread(&CallbackDrivenFake::entry, this).detach();   // the OS owns it
    running_ = true;
    return rt::BackendStart::kOk;
  }

  void stop() noexcept override {
    if (!running_) { return; }
    stopFlag_.store(true, std::memory_order_release);
    // NO JOIN EXISTS. The post-condition is met by waiting until the callout is
    // observably out, which is the only thing a HAL gives you.
    while (!calloutDone_.load(std::memory_order_acquire)) {
      std::this_thread::yield();
    }
    running_ = false;
  }

 private:
  static void entry(void* arg) { static_cast<CallbackDrivenFake*>(arg)->run(); }

  void run() {
    rt::u32 frame = 0u;
    while (!stopFlag_.load(std::memory_order_acquire)) {
      // A CALLOUT, ONCE ENTERED, RUNS TO COMPLETION. The stop flag was tested
      // before this one began, so a stop() that returned without waiting would be
      // racing a submission that is already committed.
      calloutMark_.fetch_add(1u, std::memory_order_release);
      std::this_thread::sleep_for(std::chrono::milliseconds(2));
      // A HAL callback does not deliver 480 frames; it delivers what it chose.
      const rt::u32 chunk = ((frame / rt::kFrameCount) % 2u == 0u) ? 271u : 480u;
      fillFrames(source_, frame, chunk);
      const rt::u8* planes[1] = {source_};
      sink_->noteCallback();
      (void)sink_->submit(planes, 2u, true, chunk,
                          static_cast<rt::u64>(frame) * 20833ull);
      frame += chunk;
    }
    // LAST TOUCH OF `this`. Everything after this store is stack teardown, so
    // the backend may be destroyed the instant stop() returns.
    calloutDone_.store(true, std::memory_order_release);
  }

  rt::SourceFormat format_;
  rt::QuantumPump* sink_;
  bool running_;
  std::atomic<bool> stopFlag_;
  std::atomic<bool> calloutDone_;
  std::atomic<rt::u32> calloutMark_;
  rt::u8 source_[static_cast<std::size_t>(rt::kFrameCount) * 2u * 4u];
};

// FAKE C -- A LIAR. It calls the sink once more AFTER stop() has returned, which
// is precisely what the post-condition forbids. Synchronous and thread-free on
// purpose: the assertion must be deterministic, not a race the test hopes to win.
class LyingFake final : public rt::CaptureBackend {
 public:
  LyingFake() : sink_(nullptr), running_(false), frame_(0u), source_() {}

  rt::BackendStart start(const rt::CaptureTarget& target, rt::QuantumPump& sink,
                         const rt::HostServices& /*host*/) noexcept override {
    if (target.pidCount == 0u) { return rt::BackendStart::kNoTarget; }
    sink_ = &sink;
    running_ = true;
    emitOne();
    emitOne();
    return rt::BackendStart::kOk;
  }

  void stop() noexcept override { running_ = false; }

  /// The lie. Called by the test AFTER stop() has returned and the gate is shut.
  void submitAfterStop() { emitOne(); }

 private:
  void emitOne() {
    fillFrames(source_, frame_, rt::kFrameCount);
    const rt::u8* planes[1] = {source_};
    sink_->noteCallback();
    (void)sink_->submit(planes, 2u, true, rt::kFrameCount,
                        static_cast<rt::u64>(frame_) * 20833ull);
    frame_ += rt::kFrameCount;
  }

  rt::QuantumPump* sink_;
  bool running_;
  rt::u32 frame_;
  rt::u8 source_[static_cast<std::size_t>(rt::kFrameCount) * 2u * 4u];
};

// FAKE D -- NEVER QUIESCES. It holds the gate open past the budget, which is the
// branch that makes the teardown abandon the threadsafe function instead of
// releasing it. PR 1 can see the wait expire; the abandon-and-poison half lives
// in napi/ and is Task 5's.
class NeverQuiescingFake final : public rt::CaptureBackend {
 public:
  explicit NeverQuiescingFake(rt::SinkGate& gate)
      : gate_(gate), inside_(false), release_(false), exited_(false) {}

  rt::BackendStart start(const rt::CaptureTarget& target, rt::QuantumPump& /*sink*/,
                         const rt::HostServices& /*host*/) noexcept override {
    if (target.pidCount == 0u) { return rt::BackendStart::kNoTarget; }
    release_.store(false, std::memory_order_release);
    exited_.store(false, std::memory_order_release);
    std::thread(&NeverQuiescingFake::entry, this).detach();
    return rt::BackendStart::kOk;
  }

  /// Returns having destroyed nothing and released nothing: the lie this fake
  /// tells is about its own callout, not about the gate.
  void stop() noexcept override {}

  bool inside() const { return inside_.load(std::memory_order_acquire); }
  void releaseAndWait() {
    release_.store(true, std::memory_order_release);
    while (!exited_.load(std::memory_order_acquire)) { std::this_thread::yield(); }
  }

 private:
  static void entry(void* arg) { static_cast<NeverQuiescingFake*>(arg)->run(); }

  void run() {
    if (gate_.tryEnter()) {
      inside_.store(true, std::memory_order_release);
      while (!release_.load(std::memory_order_acquire)) {
        std::this_thread::sleep_for(std::chrono::milliseconds(1));
      }
      gate_.leave();
    }
    exited_.store(true, std::memory_order_release);
  }

  rt::SinkGate& gate_;
  std::atomic<bool> inside_;
  std::atomic<bool> release_;
  std::atomic<bool> exited_;
};

// The identical assertion set, run against both shapes. A post-condition either
// holds for both mechanisms or it is not a contract.
static void runPostConditionSuite(rt::CaptureBackend& backend,
                                  const std::atomic<rt::u32>& calloutMark,
                                  const char* label) {
  GatedSink sink;
  sink.pump.reset(1000u);
  const rt::CaptureTarget target = oneTarget(4321u);

  CHECK(backend.start(target, sink.pump, kFakeHost) == rt::BackendStart::kOk);

  // Some quanta actually arrive -- otherwise "nothing happened after stop()"
  // would be satisfied by a backend that never started.
  rt::u64 spins = 0u;
  while (sink.pump.counters().quantaTotal < 4u) {
    ++spins;
    CHECK(spins < 2000000000ull);
    std::this_thread::yield();
  }
  CHECK(sink.tsfnCalls.load(std::memory_order_relaxed) >= 1u);

  // TEAR DOWN AT THE START OF A CALLOUT. Stopping an idle backend proves almost
  // nothing: the interesting instant is the one where the backend has already
  // committed to a submission it has not yet made, which is what a real HAL
  // teardown has to survive. Waiting for the mark to CHANGE lands the teardown at
  // the beginning of that window rather than anywhere inside it -- and the
  // difference is measurable, not decorative. Against a callback-driven fake that
  // skipped its own quiesce wait: no wait at all killed it 2 runs in 5, "somewhere
  // inside a callout" killed it 4 in 5, and this kills it 5 in 5.
  const rt::u32 markBefore = calloutMark.load(std::memory_order_acquire);
  spins = 0u;
  while (calloutMark.load(std::memory_order_acquire) == markBefore) {
    ++spins;
    CHECK(spins < 2000000000ull);
    std::this_thread::yield();
  }

  backend.stop();

  // THE POST-CONDITION. From the instant stop() returns, the sink is untouched.
  // Measured over 50 ms of wall time, which is five quantum periods -- a backend
  // still running would have moved every one of these four numbers.
  const SinkObservation atStop = observe(sink);
  std::this_thread::sleep_for(std::chrono::milliseconds(50));
  const SinkObservation later = observe(sink);
  CHECK(later.callbackTotal == atStop.callbackTotal);
  CHECK(later.quantaTotal == atStop.quantaTotal);
  CHECK(later.tsfnCalls == atStop.tsfnCalls);
  CHECK(later.refusedCalls == atStop.refusedCalls);

  // And the gate agrees: nobody is inside, so the handle is safe to release.
  CHECK(sink.closeAndWait(rt::kQuiesceBudgetMs));
  CHECK(sink.gate.quiesced());

  // stop() is idempotent -- the env-cleanup hook calls it after Stop_JS already has.
  backend.stop();
  const SinkObservation afterSecondStop = observe(sink);
  CHECK(afterSecondStop.callbackTotal == atStop.callbackTotal);
  CHECK(afterSecondStop.quantaTotal == atStop.quantaTotal);

  std::printf("  backend contract: %s satisfied the post-condition\n", label);
}

static void test_backendContract_bothShapesSatisfyThePostCondition() {
  ThreadDrivenFake threadDriven;
  runPostConditionSuite(threadDriven, threadDriven.calloutMark(),
                        "thread-driven (joins)");

  const rt::SourceFormat kGood = {rt::kSampleRate, 2u, true, true};
  CallbackDrivenFake callbackDriven(kGood);
  runPostConditionSuite(callbackDriven, callbackDriven.calloutMark(),
                        "callback-driven (joins nothing)");
}

static void test_backendContract_callAfterStopIsRefusedByTheGate() {
  // REGRESSION-LOCK -- never delete. This encodes F3: teardown is not "join the
  // thread", it is "close the gate and prove nobody is inside", and it holds
  // even against a backend that breaks its own post-condition.
  GatedSink sink;
  sink.pump.reset(1000u);
  LyingFake liar;

  CHECK(liar.start(oneTarget(99u), sink.pump, kFakeHost) == rt::BackendStart::kOk);
  const SinkObservation running = observe(sink);
  CHECK(running.quantaTotal == 2u);
  CHECK(running.tsfnCalls == 2u);
  CHECK(running.refusedCalls == 0u);

  liar.stop();
  CHECK(sink.closeAndWait(rt::kQuiesceBudgetMs));

  // The lie: one more submission after stop() returned and the gate is shut.
  liar.submitAfterStop();

  const SinkObservation afterLie = observe(sink);
  // THE THREADSAFE FUNCTION WAS NEVER TOUCHED. That is the whole assertion: in
  // production the handle may already be released, and this is what makes the
  // difference between a dropped quantum and a use-after-release.
  CHECK(afterLie.tsfnCalls == running.tsfnCalls);
  // The refusal is COUNTED, not silent -- a gate that refused invisibly would
  // make this defect undiagnosable in the field.
  CHECK(afterLie.refusedCalls == 1u);
  // The quantum did reach the ring, and that is correct and harmless: teardown
  // drains the ring after the gate closes, so a post-stop quantum is discarded
  // rather than delivered. Nothing downstream can see it.
  CHECK(afterLie.quantaTotal == running.quantaTotal + 1u);
  CHECK(sink.gate.quiesced());
  CHECK(sink.gate.closed());
}

static void test_backendContract_quiesceWaitExpiresOnABackendThatHoldsTheGate() {
  GatedSink sink;
  sink.pump.reset(1000u);
  NeverQuiescingFake stuck(sink.gate);

  CHECK(stuck.start(oneTarget(7u), sink.pump, kFakeHost) == rt::BackendStart::kOk);
  rt::u64 spins = 0u;
  while (!stuck.inside()) {
    ++spins;
    CHECK(spins < 2000000000ull);
    std::this_thread::yield();
  }

  stuck.stop();          // returns having proven nothing

  // A SHORT budget, because the point is the expiry and not the wall clock. The
  // shipped ceiling is asserted separately below.
  CHECK(!sink.closeAndWait(5u));
  CHECK(!sink.gate.quiesced());
  CHECK(sink.gate.closed());
  // This is the branch that makes Task 5 ABANDON the threadsafe function rather
  // than release it, and latch `poisoned`. Releasing here would be the
  // use-after-release the gate exists to prevent, so the timeout must NOT be
  // treated as "close enough".

  stuck.releaseAndWait();
  CHECK(sink.gate.quiesced());

  // The shipped budget, pinned so a future edit cannot quietly turn a bounded
  // wait into a long one that blocks the child's JS thread.
  CHECK(rt::kQuiesceBudgetMs == 250u);
}

static void test_backendContract_startFailsClosedWithNoTarget() {
  GatedSink sink;
  sink.pump.reset(1000u);
  rt::CaptureTarget none = {};
  none.pidCount = 0u;

  ThreadDrivenFake threadDriven;
  CHECK(threadDriven.start(none, sink.pump, kFakeHost) == rt::BackendStart::kNoTarget);

  const rt::SourceFormat kGood = {rt::kSampleRate, 2u, true, true};
  CallbackDrivenFake callbackDriven(kGood);
  CHECK(callbackDriven.start(none, sink.pump, kFakeHost) == rt::BackendStart::kNoTarget);

  // A refused start captured NOTHING. Until #3198 supplies a target, this is the
  // path every real backend takes, and it is what makes the feature dark.
  const SinkObservation after = observe(sink);
  CHECK(after.callbackTotal == 0u);
  CHECK(after.quantaTotal == 0u);
  CHECK(after.tsfnCalls == 0u);

  // stop() on a backend that never started is a no-op, not a crash: the env
  // cleanup hook calls it unconditionally.
  threadDriven.stop();
  callbackDriven.stop();
}

static void test_backendContract_unsupportedFormatIsRefusedNotResampled() {
  const rt::SourceFormat kFortyFourOne = {44100u, 2u, true, true};
  const rt::SourceFormat kNotFloat     = {rt::kSampleRate, 2u, true, false};
  const rt::SourceFormat kFiveOne      = {rt::kSampleRate, 6u, true, true};
  const rt::SourceFormat kZeroChannels = {rt::kSampleRate, 0u, true, true};
  const rt::SourceFormat kMono         = {rt::kSampleRate, 1u, true, true};
  const rt::SourceFormat kPlanarStereo = {rt::kSampleRate, 2u, false, true};

  CHECK(!rt::acceptsSourceFormat(kFortyFourOne));
  CHECK(!rt::acceptsSourceFormat(kNotFloat));
  CHECK(!rt::acceptsSourceFormat(kFiveOne));
  CHECK(!rt::acceptsSourceFormat(kZeroChannels));
  // Mono is accepted and DUPLICATED by the repacketizer; planar is accepted and
  // interleaved. Neither is a resample and neither is a downmix.
  CHECK(rt::acceptsSourceFormat(kMono));
  CHECK(rt::acceptsSourceFormat(kPlanarStereo));

  // And a backend handed a format it cannot take refuses to start at all rather
  // than starting and delivering something else.
  GatedSink sink;
  sink.pump.reset(1000u);
  CallbackDrivenFake wrongRate(kFortyFourOne);
  CHECK(wrongRate.start(oneTarget(11u), sink.pump, kFakeHost) ==
        rt::BackendStart::kUnsupportedFormat);
  CHECK(observe(sink).callbackTotal == 0u);
  wrongRate.stop();
}

// FAKE F -- A BACKEND THAT IMPLEMENTS ADR-0043 D6 AS WRITTEN. It exists to prove
// the SEAM carries the discriminator, which is the half a fake can prove: a
// backend can only obey a rule it can see, and before PR #3262 a whole-system mix
// and an unresolved window target arrived at this start() as the same struct.
class ScopeAwareFake final : public rt::CaptureBackend {
 public:
  ScopeAwareFake() noexcept : sawSystemMix_(false), sawPidCount_(0u) {}

  rt::BackendStart start(const rt::CaptureTarget& target, rt::QuantumPump& /*sink*/,
                         const rt::HostServices& /*host*/) noexcept override {
    sawSystemMix_ = (target.scope == rt::CaptureScope::kSystemMix);
    sawPidCount_  = target.pidCount;
    if (target.scope == rt::CaptureScope::kSystemMix) {
      // D6 row one: a monitor / full-screen target legitimately captures the whole
      // system mix. `pids` is not read.
      return rt::BackendStart::kOk;
    }
    // D6 row two: an empty process list is an omitted option or a PID lookup that
    // failed, and NEITHER may fall back to the whole system.
    if (target.pidCount == 0u) { return rt::BackendStart::kNoTarget; }
    return rt::BackendStart::kOk;
  }

  void stop() noexcept override {}

  bool    sawSystemMix() const { return sawSystemMix_; }
  rt::u8  sawPidCount() const { return sawPidCount_; }

 private:
  bool    sawSystemMix_;
  rt::u8  sawPidCount_;
};

static void test_backendContract_aSystemMixIsOnlyReachableByAffirmativeRequest() {
  // ADR-0043 D6 carries two adjacent rows that both landed on pidCount == 0: a
  // full-screen target legitimately gets the whole system mix, and a WINDOW
  // target whose process could not be resolved must never silently get one. With
  // no discriminator the safe reading of that pair was carried by a comment, and
  // the fakes in this file implemented it because their author had read the
  // comment -- which proves the fakes. Found by @security-reviewer on PR #3262.

  // 1. THE DEFAULT IS THE REFUSING ARM. A zeroed struct, a caller that forgot the
  //    field, and a lookup that wrote nothing all land here.
  rt::CaptureTarget zeroed = {};
  CHECK(zeroed.scope == rt::CaptureScope::kProcessList);
  CHECK(zeroed.pidCount == 0u);

  // 2. buildTarget NEVER PRODUCES kSystemMix -- not on the admitting arm...
  rt::u32 pids[2] = {501u, 502u};
  rt::CaptureTarget built;
  CHECK(rt::buildTarget(pids, 2u, false, &built));
  CHECK(built.scope == rt::CaptureScope::kProcessList);

  // ...and not by leaving a REUSED struct's earlier request in place. This is the
  // refusal-publishes-nothing property applied to the field that decides whether
  // the whole system is captured, and it is the one an empty list would otherwise
  // walk straight through.
  rt::CaptureTarget reused;
  CHECK(rt::buildTarget(pids, 2u, false, &reused));
  reused.scope = rt::CaptureScope::kSystemMix;
  CHECK(!rt::buildTarget(pids, 0u, false, &reused));
  CHECK(reused.scope == rt::CaptureScope::kProcessList);
  CHECK(reused.pidCount == 0u);

  // 3. AND THE SEAM CARRIES IT. Three requests, three answers, where a backend
  //    used to be able to see only two.
  GatedSink sink;
  sink.pump.reset(1000u);

  ScopeAwareFake unresolvedWindow;
  CHECK(unresolvedWindow.start(zeroed, sink.pump, kFakeHost) ==
        rt::BackendStart::kNoTarget);
  CHECK(!unresolvedWindow.sawSystemMix());
  unresolvedWindow.stop();

  ScopeAwareFake processList;
  CHECK(processList.start(built, sink.pump, kFakeHost) == rt::BackendStart::kOk);
  CHECK(!processList.sawSystemMix());
  CHECK(processList.sawPidCount() == 2u);
  processList.stop();

  rt::CaptureTarget monitor = {};
  monitor.scope = rt::CaptureScope::kSystemMix;
  ScopeAwareFake wholeSystem;
  CHECK(wholeSystem.start(monitor, sink.pump, kFakeHost) == rt::BackendStart::kOk);
  // Value passed, value obeyed: it got the mix BECAUSE it was asked for, with an
  // empty PID list that on the other arm is a refusal.
  CHECK(wholeSystem.sawSystemMix());
  CHECK(wholeSystem.sawPidCount() == 0u);
  wholeSystem.stop();
}

static void test_backendContract_platformBackendIsAbsentInPr1() {
  // PR 1 compiles no platform backend on ANY platform, which is what keeps a
  // release build's start() returning NoBackend -- the dark-ship gate the
  // define-separation step in native-audiocap.yml asserts. PR 2 and #3196 each
  // replace this body; until then a nullptr here IS the feature being off.
  CHECK(rt::platformBackend() == nullptr);
}

static void test_backendContract_targetIsCopiedByValueAndBounded() {
  // The target is copied at start and never retained: #3198 hands over a PID
  // list from JS, and a backend holding a reference to it would be reading a
  // buffer the JS thread owns.
  CHECK(rt::kMaxTargetPids == 8u);
  rt::CaptureTarget t = {};
  for (rt::u8 i = 0u; i < rt::kMaxTargetPids; ++i) {
    t.pids[i] = 1000u + i;
  }
  t.pidCount = rt::kMaxTargetPids;
  t.allowDescendants = true;

  const rt::CaptureTarget copy = t;
  t.pids[0] = 0u;
  t.pidCount = 0u;
  CHECK(copy.pids[0] == 1000u);
  CHECK(copy.pidCount == rt::kMaxTargetPids);
  CHECK(copy.allowDescendants);
  CHECK(copy.pids[rt::kMaxTargetPids - 1u] == 1007u);
}

// ---------------------------------------------------------------------------
// Task 6 — rt::buildTarget, the admission rule for a caller-supplied PID list.
//
// WHY THE POLICY IS TESTED HERE AND NOT AT THE N-API SEAM. napi/addon.cc
// includes node_api.h, which nothing in this package can compile, so a
// validation table written inline there would ship with no automated witness --
// the same reason rt/teardown.h exists. napi/ keeps only what a JS value must BE
// to become a u32 (a number, an exact integer, in range); everything about WHICH
// lists are admissible is below.
//
// This matters more than an ordinary parameter check. #3198 supplies the list,
// and a selector that is silently ignored, silently truncated, or silently
// half-applied points capture at processes the user did not name -- #2161's
// defect class, reached through a validation gap rather than through a fallback.
// ---------------------------------------------------------------------------

static void test_buildTarget_admitsOneThroughEightAndRefusesNine() {
  rt::u32 pids[9] = {101u, 102u, 103u, 104u, 105u, 106u, 107u, 108u, 109u};

  // Every admissible length, not just the endpoints: an off-by-one in the fill
  // loop would leave a middle length short and both endpoints correct.
  for (rt::u32 count = 1u; count <= rt::kMaxTargetPids; ++count) {
    rt::CaptureTarget t;
    CHECK(rt::buildTarget(pids, count, false, &t));
    CHECK(t.pidCount == static_cast<rt::u8>(count));
    for (rt::u32 i = 0u; i < count; ++i) {
      CHECK(t.pids[i] == pids[i]);
    }
    // Value passed, value obeyed: the slots BEYOND the count must stay zero, so a
    // shorter list cannot inherit a longer one's tail through a reused struct.
    for (rt::u32 i = count; i < rt::kMaxTargetPids; ++i) {
      CHECK(t.pids[i] == 0u);
    }
  }

  // THE BOUNDARY. Nine is refused, not truncated to eight -- truncation would
  // capture a subset of what the caller asked for and report success.
  rt::CaptureTarget nine;
  CHECK(!rt::buildTarget(pids, static_cast<rt::u32>(rt::kMaxTargetPids) + 1u, false, &nine));
  CHECK(nine.pidCount == 0u);
  CHECK(nine.pids[0] == 0u);
}

static void test_buildTarget_refusesEmptyListAndPidZero() {
  rt::u32 pids[3] = {201u, 0u, 203u};

  // An EMPTY list is a request that cannot be satisfied, and is refused so the
  // caller reports BadOptions. It is NOT the same as omitting the option, which
  // never reaches this function and leaves the no-target struct that a real
  // backend answers with kNoTarget.
  rt::CaptureTarget empty;
  CHECK(!rt::buildTarget(pids, 0u, false, &empty));
  CHECK(empty.pidCount == 0u);

  // PID 0 anywhere in the list refuses the WHOLE list.
  rt::CaptureTarget withZero;
  CHECK(!rt::buildTarget(pids, 3u, false, &withZero));
  CHECK(withZero.pidCount == 0u);

  // ...including in the last position, which a loop that stopped one short of
  // the count would miss.
  rt::u32 trailingZero[2] = {204u, 0u};
  rt::CaptureTarget trailing;
  CHECK(!rt::buildTarget(trailingZero, 2u, false, &trailing));
  CHECK(trailing.pidCount == 0u);

  // ...and in the first, which a loop that started one late would miss.
  rt::u32 leadingZero[2] = {0u, 205u};
  rt::CaptureTarget leading;
  CHECK(!rt::buildTarget(leadingZero, 2u, false, &leading));
  CHECK(leading.pidCount == 0u);
}

static void test_buildTarget_refusesDuplicatePids() {
  // A duplicate is refused for a narrower reason than PID 0: this list becomes a
  // CATapDescription process list on macOS and Apple specifies no behaviour for
  // one that names the same process twice, so admitting it hands an OS call an
  // input whose response is undefined. It is also evidence the caller's
  // resolution step went wrong. Found by @security-reviewer on PR #3262.
  rt::u32 adjacent[2] = {1234u, 1234u};
  rt::CaptureTarget t;
  CHECK(!rt::buildTarget(adjacent, 2u, false, &t));
  CHECK(t.pidCount == 0u);
  CHECK(t.pids[0] == 0u);      // and it published nothing, as every refusal does

  // NON-ADJACENT, which a comparison against only the previous element misses.
  rt::u32 apart[3] = {11u, 22u, 11u};
  rt::CaptureTarget spread;
  CHECK(!rt::buildTarget(apart, 3u, false, &spread));
  CHECK(spread.pidCount == 0u);

  // At the FULL length, where the inner loop runs longest.
  rt::u32 full[8] = {1u, 2u, 3u, 4u, 5u, 6u, 7u, 1u};
  rt::CaptureTarget eight;
  CHECK(!rt::buildTarget(full, 8u, false, &eight));
  CHECK(eight.pidCount == 0u);

  // THE POSITIVE CONTROL, without which this case passes against a buildTarget
  // that refuses every list: the same eight PIDs, distinct, are admitted whole.
  rt::u32 distinct[8] = {1u, 2u, 3u, 4u, 5u, 6u, 7u, 8u};
  rt::CaptureTarget ok;
  CHECK(rt::buildTarget(distinct, 8u, false, &ok));
  CHECK(ok.pidCount == 8u);
  CHECK(ok.pids[7] == 8u);
}

static void test_buildTarget_refusalPublishesNothing() {
  // A refusal must leave a struct that a backend reads as "no target supplied",
  // never one holding the admissible PREFIX of a rejected list. Seed the target
  // with a previously-good list first, so a refusal that merely fails to write is
  // distinguishable from one that clears.
  rt::u32 good[2] = {301u, 302u};
  rt::CaptureTarget t;
  CHECK(rt::buildTarget(good, 2u, true, &t));
  CHECK(t.pidCount == 2u);
  CHECK(t.allowDescendants);

  rt::u32 bad[3] = {303u, 304u, 0u};
  CHECK(!rt::buildTarget(bad, 3u, true, &t));
  CHECK(t.pidCount == 0u);
  CHECK(!t.allowDescendants);
  for (rt::u32 i = 0u; i < rt::kMaxTargetPids; ++i) {
    CHECK(t.pids[i] == 0u);
  }

  // A null array with a non-zero count, and a null destination. Neither may be
  // dereferenced and both must refuse.
  rt::CaptureTarget nullSrc;
  CHECK(!rt::buildTarget(nullptr, 2u, false, &nullSrc));
  CHECK(nullSrc.pidCount == 0u);
  CHECK(!rt::buildTarget(good, 2u, false, nullptr));
}

static void test_buildTarget_carriesAllowDescendantsBothWays() {
  // The flag is honoured natively by Windows and documented on macOS as already
  // expanded by the caller (design section 3.1). Either way it must arrive as it
  // was passed: a flag that is always false is a bounded-list capture wearing a
  // tree-capture name.
  rt::u32 pids[1] = {401u};
  rt::CaptureTarget on;
  CHECK(rt::buildTarget(pids, 1u, true, &on));
  CHECK(on.allowDescendants);

  rt::CaptureTarget off;
  CHECK(rt::buildTarget(pids, 1u, false, &off));
  CHECK(!off.allowDescendants);
}

// ---------------------------------------------------------------------------
// Task 5 — rt/teardown.h, the five-step sequence napi/addon.cc runs.
//
// WHY THE SEQUENCE IS NOT SIMPLY THE BODY OF stopCapture(). It is asserted here
// because it CANNOT be asserted there: napi/addon.cc includes node_api.h, which
// no test or fuzz target in this package can compile, so a teardown written
// inline would ship with its order — this epic's whole privacy invariant —
// checked by nothing. The four hooks below stand in for the parts that must name
// N-API and uv; everything about the ORDER and the BRANCH is the shipped routine.
// ---------------------------------------------------------------------------

// Every hook records that it ran, when it ran relative to the others, and what it
// could see of the world at that instant.
struct TeardownProbe {
  static constexpr rt::u32 kMarkStopBackend = 1u;
  static constexpr rt::u32 kMarkRelease     = 2u;
  static constexpr rt::u32 kMarkDrain       = 3u;
  static constexpr rt::u32 kMaxMarks        = 8u;

  rt::SinkGate      gate;
  // The gate the HOOKS observe. Its own by default; a test that runs the teardown
  // against a live sink points this at that sink's gate, so the two orderings
  // below are sampled on the gate the producer actually enters.
  rt::SinkGate*     watched;
  std::atomic<bool> running;

  rt::u32 stopBackendCalls;
  rt::u32 releaseCalls;
  rt::u32 drainCalls;
  rt::u32 sleepCalls;
  // THE CLOCK THE TEARDOWN MEASURES ITS WINDOW WITH, advanced by this probe's own
  // sleepMs and read by its own nowNs. It is virtual so the loop is DETERMINISTIC
  // -- a real clock would make `sleepCalls` a race against the scheduler -- and it
  // is advanced by `msPerSleep` rather than by the millisecond the teardown asked
  // for, which is the whole point: a host may grant far more than it was asked,
  // and a test that cannot express that cannot catch an iteration-counted window.
  rt::u64 clockNs;
  rt::u32 msPerSleep;
  // HOW MANY TIMES THE WITNESS WAS READ, which is the only thing that tells a
  // refusal taken BEFORE the wait apart from one the wait arrived at. Teardown
  // samples the baseline once; awaitSettled samples it once per iteration.
  rt::u32 activityReads;
  // Sleep number at which the virtual clock steps BACKWARDS instead of forwards;
  // 0 never. A monotone clock is what TeardownOps asks for, and this is how a hook
  // that does not deliver one is expressed in a test.
  rt::u32 rewindAtSleep;
  // Sampled INSIDE the stopBackend hook, which is the only vantage from which the
  // two orderings that matter can be seen at all.
  bool    runningWhenBackendStopped;
  bool    gateClosedWhenBackendStopped;
  // Sampled INSIDE the release hook: the handle may only be released after the
  // gate is shut, which is the ordering a hand-rolled unwind gets wrong.
  bool    gateClosedWhenReleased;
  // The liveness witness rt::Teardown reads. A live pump's activityTotal when
  // `witness` is set, and otherwise a CONSTANT ZERO -- there is deliberately no
  // second, probe-owned number to move. It used to have one and nothing ever
  // wrote it, which made every non-witness case read a field that only looked
  // adjustable; the cases that set no witness are about the clock, the budget and
  // the null hooks, and each of them needs a producer that is motionless, which is
  // exactly what a constant is. A case that needs a MOVING witness attaches a real
  // pump, because that is the thing the shipped hook reads.
  const rt::QuantumPump* witness;
  // Sampled INSIDE the wait, once per yield, on the gate the hooks watch. It is
  // the only vantage from which "the gate read zero WHILE a live producer was
  // running" can be observed at all -- after the run, the producer is gone and a
  // zero proves nothing.
  bool    sawQuiescedGateDuringWait;
  // Called by the stopBackend hook when set -- the backend a real teardown stops.
  rt::CaptureBackend* backend;
  rt::u32 marks[kMaxMarks];
  rt::u32 markCount;

  TeardownProbe()
      : gate(), watched(&gate), running(true), stopBackendCalls(0u), releaseCalls(0u),
        drainCalls(0u), sleepCalls(0u), clockNs(0u), msPerSleep(1u),
        activityReads(0u), rewindAtSleep(0u),
        runningWhenBackendStopped(true),
        gateClosedWhenBackendStopped(true), gateClosedWhenReleased(false),
        witness(nullptr), sawQuiescedGateDuringWait(false), backend(nullptr),
        marks(), markCount(0u) {}

  void mark(rt::u32 what) {
    if (markCount < kMaxMarks) {
      marks[markCount] = what;
      markCount += 1u;
    }
  }

  static void stopBackend(void* ctx) noexcept {
    TeardownProbe* const p = static_cast<TeardownProbe*>(ctx);
    p->stopBackendCalls += 1u;
    p->runningWhenBackendStopped = p->running.load(std::memory_order_acquire);
    p->gateClosedWhenBackendStopped = p->watched->closed();
    if (p->backend != nullptr) { p->backend->stop(); }
    p->mark(kMarkStopBackend);
  }

  /// The independent witness of #3197 PoC-1: a number only the producer moves.
  /// activityTotal and NOT callbackTotal, for the reason napi/addon.cc names --
  /// the latter is volunteered, and a witness a conforming backend may decline to
  /// move is not a witness.
  static rt::u32 producerActivity(void* ctx) noexcept {
    TeardownProbe* const p = static_cast<TeardownProbe*>(ctx);
    p->activityReads += 1u;
    if (p->witness != nullptr) { return p->witness->activityTotal(); }
    return 0u;
  }

  static void sleepMs(void* ctx, rt::u32 ms) noexcept {
    TeardownProbe* const p = static_cast<TeardownProbe*>(ctx);
    p->sleepCalls += 1u;
    // Sampled here because here is INSIDE the wait: this hook runs once per
    // iteration, with whatever producer exists still running.
    if (p->watched->quiesced()) { p->sawQuiescedGateDuringWait = true; }
    // The virtual clock advances by what this HOST grants, which at the default
    // msPerSleep == 1 is exactly what was asked for and tracks the real sleep
    // below. The real sleep stays because other cases in this task race a live
    // producer thread against the window and need wall time to actually pass.
    if ((p->rewindAtSleep != 0u) && (p->sleepCalls == p->rewindAtSleep)) {
      p->clockNs -= 10u * rt::kNsPerMs;
    } else {
      p->clockNs += static_cast<rt::u64>(p->msPerSleep) * rt::kNsPerMs;
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(ms));
  }

  static rt::u64 nowNs(void* ctx) noexcept {
    return static_cast<TeardownProbe*>(ctx)->clockNs;
  }

  static void releaseSignal(void* ctx) noexcept {
    TeardownProbe* const p = static_cast<TeardownProbe*>(ctx);
    p->releaseCalls += 1u;
    p->gateClosedWhenReleased = p->watched->closed();
    p->mark(kMarkRelease);
  }

  static void drain(void* ctx) noexcept {
    TeardownProbe* const p = static_cast<TeardownProbe*>(ctx);
    p->drainCalls += 1u;
    p->mark(kMarkDrain);
  }

  rt::TeardownOps ops() {
    const rt::TeardownOps out = {&TeardownProbe::stopBackend, &TeardownProbe::sleepMs,
                                 &TeardownProbe::releaseSignal, &TeardownProbe::drain,
                                 &TeardownProbe::producerActivity,
                                 &TeardownProbe::nowNs, this};
    return out;
  }
};

// Holds the gate for a bounded time and then leaves, which is what an in-flight
// callback does. The teardown's bounded wait is the ONLY thing that turns this
// into a released handle rather than an abandoned one.
class LateLeaver {
 public:
  LateLeaver(rt::SinkGate& gate, rt::u32 holdMs)
      : gate_(gate), holdMs_(holdMs), inside_(false), thread_() {}

  void begin() { thread_ = std::thread(&LateLeaver::run, this); }
  bool inside() const { return inside_.load(std::memory_order_acquire); }
  void finish() { if (thread_.joinable()) { thread_.join(); } }

 private:
  void run() {
    if (!gate_.tryEnter()) { return; }
    inside_.store(true, std::memory_order_release);
    std::this_thread::sleep_for(std::chrono::milliseconds(holdMs_));
    gate_.leave();
  }

  rt::SinkGate&     gate_;
  rt::u32           holdMs_;
  std::atomic<bool> inside_;
  std::thread       thread_;
};

static void test_teardown_quiescedRunReleasesTheHandleInTheRightOrder() {
  TeardownProbe p;
  rt::Teardown teardown;
  CHECK(!teardown.poisoned());
  CHECK(p.running.load(std::memory_order_acquire));

  const rt::TeardownOps ops = p.ops();
  CHECK(teardown.run(p.running, p.gate, ops, rt::kQuiesceBudgetMs) ==
        rt::TeardownOutcome::kQuiesced);

  // Step 1, and it happened BEFORE step 2: drain() and every other reader gated
  // on `running` must fail closed from the instant teardown begins, not from the
  // instant the backend finishes stopping.
  CHECK(!p.running.load(std::memory_order_acquire));
  CHECK(!p.runningWhenBackendStopped);

  // STEP 2 BEFORE STEP 3, and this is the assertion the whole file is for. The
  // privacy invariant is that the TAP STOPS EXISTING when the share ends; waiting
  // for the sink to quiesce first would leave a live tap for the length of the
  // budget on every ordinary stop, and unbounded on the one backend the budget
  // exists for.
  CHECK(p.stopBackendCalls == 1u);
  CHECK(!p.gateClosedWhenBackendStopped);
  CHECK(p.gate.closed());

  // THE ORDINARY PATH NOW PAYS ONE SETTLE WINDOW, and this assertion used to read
  // `sleepCalls == 0` on the argument that an idle gate quiesces on the first
  // poll. It does -- and #3197 PoC-1 is the case where it quiesces on the first
  // poll while the producer is still running, because a gate sample is an instant
  // and a producer is a process. The window is the evidence; the budget is still
  // the ceiling and is nowhere near reached.
  CHECK(p.sleepCalls == rt::kSettleObserveMs);
  CHECK(p.sleepCalls < rt::kQuiesceBudgetMs);

  CHECK(p.releaseCalls == 1u);
  CHECK(p.drainCalls == 1u);
  CHECK(!teardown.poisoned());

  // And the whole order, as one comparison: stop, release, drain.
  CHECK(p.markCount == 3u);
  CHECK(p.marks[0] == TeardownProbe::kMarkStopBackend);
  CHECK(p.marks[1] == TeardownProbe::kMarkRelease);
  CHECK(p.marks[2] == TeardownProbe::kMarkDrain);
}

static void test_teardown_timeoutAbandonsTheHandleAndPoisons() {
  // REGRESSION-LOCK -- never delete. Releasing a threadsafe function that a live
  // callback may still call is ADR-0043 D4b risk 4 materialized, and it is the
  // exact harm the gate exists to prevent.
  TeardownProbe p;
  // FAKE D takes a pump it never uses; the sink here exists only to satisfy the
  // backend contract's signature.
  GatedSink unused;
  unused.pump.reset(1000u);
  NeverQuiescingFake stuck(p.gate);
  CHECK(stuck.start(oneTarget(7u), unused.pump, kFakeHost) == rt::BackendStart::kOk);
  rt::u64 spins = 0u;
  while (!stuck.inside()) {
    ++spins;
    CHECK(spins < 2000000000ull);
    std::this_thread::yield();
  }

  rt::Teardown teardown;
  const rt::TeardownOps ops = p.ops();
  // A SHORT budget, because the point is the expiry and not the wall clock. The
  // shipped ceiling is pinned by the contract suite above.
  CHECK(teardown.run(p.running, p.gate, ops, 5u) == rt::TeardownOutcome::kAbandoned);

  // THE HANDLE WAS ABANDONED, NOT RELEASED. One leaked handle in a process the
  // host is about to kill, against a use-after-release in a process that loaded
  // native code and holds Screen Recording consent.
  CHECK(p.releaseCalls == 0u);
  CHECK(teardown.poisoned());
  // The whole budget was spent before the decision, not skipped.
  CHECK(p.sleepCalls == 5u);
  // Step 5 still runs on this arm: a share that ended must not leave captured
  // audio in the ring, and that is as true when the handle was abandoned.
  CHECK(p.drainCalls == 1u);
  CHECK(p.markCount == 2u);
  CHECK(p.marks[0] == TeardownProbe::kMarkStopBackend);
  CHECK(p.marks[1] == TeardownProbe::kMarkDrain);
  CHECK(p.gate.closed());
  CHECK(!p.gate.quiesced());

  stuck.releaseAndWait();
  CHECK(p.gate.quiesced());

  // THE LATCH IS FOR THE LIFE OF THE PROCESS, and PR #3262 made it DECIDE rather
  // than merely persist. A later teardown that quiesces cleanly does not clear it
  // -- and must not RELEASE on the strength of it either, which is the half that
  // was missing. stop() is documented idempotent and napi/addon.cc reaches this
  // routine from three places (stop(), the env cleanup hook, and the unwind of a
  // start that armed and then failed), so a second run in one process is ordinary;
  // by the time it happens the producer the first run could not account for is
  // typically gone, so it sees a quiet gate and a still witness and would have
  // released the handle the first one deliberately abandoned. That is the
  // use-after-release the abandon arm exists to prevent, reached by calling stop()
  // twice.
  TeardownProbe later;
  const rt::TeardownOps laterOps = later.ops();
  CHECK(teardown.run(later.running, later.gate, laterOps, rt::kQuiesceBudgetMs) ==
        rt::TeardownOutcome::kAbandoned);
  CHECK(later.releaseCalls == 0u);
  CHECK(teardown.poisoned());
  // Step 5 still runs, as it does on every arm.
  CHECK(later.drainCalls == 1u);

  // THE POSITIVE CONTROL, and it is what stops this from passing against a
  // teardown that refuses everything: the SAME probe shape under a FRESH latch
  // releases. So the refusal above is the latch talking and not the probe.
  TeardownProbe control;
  rt::Teardown unpoisoned;
  const rt::TeardownOps controlOps = control.ops();
  CHECK(unpoisoned.run(control.running, control.gate, controlOps,
                       rt::kQuiesceBudgetMs) == rt::TeardownOutcome::kQuiesced);
  CHECK(control.releaseCalls == 1u);
  CHECK(!unpoisoned.poisoned());
}

// FAKE E -- A LIAR THAT IS ALIVE. FAKE C tells the same lie synchronously, which
// is enough to prove the gate refuses a post-stop call; this one tells it on a
// thread, which is the only way to reproduce what #3197 PoC-1 measured: a producer
// that is inside the gate for microseconds per 10 ms, so every sample the teardown
// takes reads ZERO while the tap is very much alive.
//
// IT TAKES TWO BEHAVIOUR SWITCHES rather than being copied into three near-identical
// fakes, because the three cases differ by one line each and two producer loops
// that drift apart would quietly stop testing the same thing:
//   volunteersNoteCallback -- false is the CONFORMING backend that only submits
//     (rt/capture_backend.h says noteCallback() MAY be called), which the old
//     witness could not see at all.
//   parksOnStop -- the producer that sits out the whole window and comes back,
//     which nothing at this seam can refuse. See the sleeps-then-resumes case.
class LiveLiarFake final : public rt::CaptureBackend {
 public:
  LiveLiarFake(rt::QuantumPump& sink, bool volunteersNoteCallback, bool parksOnStop)
      : sink_(sink), volunteersNoteCallback_(volunteersNoteCallback),
        parksOnStop_(parksOnStop), stopFlag_(false), parkRequested_(false),
        parked_(false), iterations_(0u), thread_(), frame_(0u), source_() {}

  rt::BackendStart start(const rt::CaptureTarget& target, rt::QuantumPump& /*sink*/,
                         const rt::HostServices& /*host*/) noexcept override {
    if (target.pidCount == 0u) { return rt::BackendStart::kNoTarget; }
    thread_ = std::thread(&LiveLiarFake::run, this);
    return rt::BackendStart::kOk;
  }

  /// THE LIE, and it is the one rt/capture_backend.h says is never trusted:
  /// returns having destroyed nothing, with its producer still running.
  ///
  /// The parking variant tells a SUBTLER lie, and the one no window can catch: it
  /// really does park its producer before returning -- which is what an honest
  /// backend's barrier looks like -- and then comes back. Parking synchronously
  /// is also what makes that case deterministic rather than a race between the
  /// producer's next iteration and the teardown's first sample.
  void stop() noexcept override {
    if (!parksOnStop_) { return; }
    parkRequested_.store(true, std::memory_order_release);
    while (!parked_.load(std::memory_order_acquire)) { std::this_thread::yield(); }
  }

  /// Red-team escape hatch, not part of the contract: the producer comes back
  /// when the TEST says so rather than after a timer, so "it sat out the whole
  /// window" is a property of the run and not of the scheduler's mood.
  void resumeAfterStop() { parkRequested_.store(false, std::memory_order_release); }

  /// UNFORGEABLE LIVENESS, owned by the fake and read by the test only. Nothing in
  /// rt/ can see it, which is exactly why preconditions are gated on it: a
  /// precondition that waits on the witness under test is the same measurement as
  /// the assertion, and mutating the witness away silences both.
  rt::u32 iterations() const { return iterations_.load(std::memory_order_acquire); }

  /// Red-team escape hatch, not part of the contract: the test must not leave a
  /// thread running into the next case.
  void stopForReal() {
    stopFlag_.store(true, std::memory_order_release);
    if (thread_.joinable()) { thread_.join(); }
  }

 private:
  void run() {
    while (!stopFlag_.load(std::memory_order_acquire)) {
      if (parkRequested_.load(std::memory_order_acquire)) {
        parked_.store(true, std::memory_order_release);
        std::this_thread::sleep_for(std::chrono::milliseconds(1));
        continue;
      }
      parked_.store(false, std::memory_order_release);
      fillFrames(source_, frame_, rt::kFrameCount);
      const rt::u8* planes[1] = {source_};
      if (volunteersNoteCallback_) { sink_.noteCallback(); }
      (void)sink_.submit(planes, 2u, true, rt::kFrameCount,
                         static_cast<rt::u64>(frame_) * 20833ull);
      frame_ += rt::kFrameCount;
      iterations_.fetch_add(1u, std::memory_order_release);
      // One millisecond, not one quantum period: the test must observe the
      // witness move well inside the settle window, and a slower cadence would
      // make this a race the test hopes to win.
      std::this_thread::sleep_for(std::chrono::milliseconds(1));
    }
  }

  rt::QuantumPump&  sink_;
  const bool        volunteersNoteCallback_;
  const bool        parksOnStop_;
  std::atomic<bool> stopFlag_;
  std::atomic<bool> parkRequested_;
  std::atomic<bool> parked_;
  std::atomic<rt::u32> iterations_;
  std::thread       thread_;
  rt::u32           frame_;
  rt::u8            source_[static_cast<std::size_t>(rt::kFrameCount) * 2u * 4u];
};

// FAKE F -- ACQUIRES, THEN FAILS. The routine Core Audio and WASAPI shape: the
// IOProc is installed and a later step fails, so start() reports failure holding
// an OS artefact it created. rt/capture_backend.h states a post-condition for
// stop() and states NOTHING about what a failed start() may leave behind, which
// is why the failure path must run the same teardown as the success path.
class FailedStartFake final : public rt::CaptureBackend {
 public:
  FailedStartFake() : acquired_(false), stopCalls_(0u) {}

  rt::BackendStart start(const rt::CaptureTarget& target, rt::QuantumPump& /*sink*/,
                         const rt::HostServices& /*host*/) noexcept override {
    if (target.pidCount == 0u) { return rt::BackendStart::kNoTarget; }
    acquired_ = true;                            // the artefact exists from here
    return rt::BackendStart::kDeviceError;       // ...and the next step failed
  }

  void stop() noexcept override {
    acquired_ = false;
    stopCalls_ += 1u;
  }

  bool    acquired() const { return acquired_; }
  rt::u32 stopCalls() const { return stopCalls_; }

 private:
  bool    acquired_;
  rt::u32 stopCalls_;
};

static void test_teardown_aLiveProducerIsNotTakenForAQuiescedOne() {
  // REGRESSION-LOCK -- never delete. #3197 PoC-1, in the one place CI runs it
  // under all three sanitizers. A teardown whose only evidence is the gate count
  // takes the QUIESCED arm here -- the gate genuinely reads zero -- releases the
  // handle, never latches `poisoned`, and (before this fix) let the next start()
  // hand a fresh OPEN gate to this still-running producer. Session-1 audio reached
  // session 2's consumer.
  CHECK(rt::kSettleObserveMs == 50u);
  CHECK(rt::kSettleObserveMs < rt::kQuiesceBudgetMs);

  GatedSink sink;
  sink.pump.reset(1000u);
  LiveLiarFake liar(sink.pump, /*volunteersNoteCallback=*/true, /*parksOnStop=*/false);
  CHECK(liar.start(oneTarget(31u), sink.pump, kFakeHost) == rt::BackendStart::kOk);

  // NOT VACUOUS, and the precondition is gated on a counter THE FAKE OWNS under a
  // DEADLINE. Both halves were wrong before PR #3262: waiting on the pump's
  // callbackTotal made the precondition and the assertion one measurement, and
  // bounding that wait with 2e9 spins instead of a clock meant the mutation this
  // lock exists to catch -- deleting the fake's noteCallback() call -- left the
  // test spinning rather than failing. A lock that hangs under mutation cannot be
  // distinguished from one that passes. Found by @code-reviewer.
  CHECK(waitUntil([&liar] { return liar.iterations() >= 2u; }, 5000u));

  TeardownProbe p;
  p.watched = &sink.gate;
  p.witness = &sink.pump;
  p.backend = &liar;
  rt::Teardown teardown;
  const rt::TeardownOps ops = p.ops();
  const rt::u32 beforeRun = sink.pump.activityTotal();

  CHECK(teardown.run(p.running, sink.gate, ops, rt::kQuiesceBudgetMs) ==
        rt::TeardownOutcome::kAbandoned);

  // The backend WAS asked to stop, and it returned claiming success.
  CHECK(p.stopBackendCalls == 1u);
  // THE HANDLE WAS NOT RELEASED and the process is poisoned, which is what makes
  // the host kill this child -- the only thing left that can destroy a tap whose
  // backend refused to destroy it.
  CHECK(p.releaseCalls == 0u);
  CHECK(teardown.poisoned());
  // Step 5 still runs on this arm.
  CHECK(p.drainCalls == 1u);
  CHECK(sink.gate.closed());
  // The witness is what decided it: the producer kept going after stop() returned.
  CHECK(sink.pump.activityTotal() > beforeRun);

  // AND THE OLD EVIDENCE SAID YES WHILE THE PRODUCER WAS STILL RUNNING. This is
  // the counterfactual, and it is sampled from INSIDE the wait -- the assertion
  // that used to stand here ran after stopForReal() and therefore only proved
  // that a gate with no producer left reads zero, which is not the claim. Found
  // by @security-reviewer on PR #3262. A teardown that asked only "is anybody
  // inside" had every reason, right then, to release the handle and re-arm: the
  // liar is inside the gate for microseconds per iteration and outside it for the
  // rest. The gate was never the wrong mechanism; it was never a liveness test.
  CHECK(p.sawQuiescedGateDuringWait);

  liar.stopForReal();

  // And it still reads zero with the producer joined -- the same answer to the
  // same question, which is the point: the gate could not tell these two worlds
  // apart.
  CHECK(sink.gate.quiesced());
}

static void test_teardown_aSubmitOnlyProducerIsNotTakenForAQuiescedOne() {
  // REGRESSION-LOCK -- never delete. THE CASE THE LOCK ABOVE STRUCTURALLY COULD
  // NOT CATCH. rt/capture_backend.h says a backend MAY call noteCallback(), so a
  // fully CONFORMING backend that only ever calls submit() left the old witness
  // (the pump's callbackTotal) motionless for the whole settle window while its
  // tap ran -- and the teardown released the handle over it, exactly as #3197
  // PoC-1 did through the gate. No lie is required here: this fake breaks the
  // stop() post-condition and nothing else.
  GatedSink sink;
  sink.pump.reset(1000u);
  LiveLiarFake liar(sink.pump, /*volunteersNoteCallback=*/false, /*parksOnStop=*/false);
  CHECK(liar.start(oneTarget(31u), sink.pump, kFakeHost) == rt::BackendStart::kOk);
  CHECK(waitUntil([&liar] { return liar.iterations() >= 2u; }, 5000u));

  // THE VACUITY CONTROL, and it is the whole point of the case: the OLD witness is
  // still ZERO while this producer runs. If this ever reads non-zero the fake has
  // started volunteering and the case under test is no longer being run.
  CHECK(sink.pump.counters().callbackTotal == 0u);
  CHECK(sink.pump.activityTotal() > 0u);

  TeardownProbe p;
  p.watched = &sink.gate;
  p.witness = &sink.pump;
  p.backend = &liar;
  rt::Teardown teardown;
  const rt::TeardownOps ops = p.ops();
  const rt::u32 beforeRun = sink.pump.activityTotal();

  CHECK(teardown.run(p.running, sink.gate, ops, rt::kQuiesceBudgetMs) ==
        rt::TeardownOutcome::kAbandoned);
  CHECK(p.stopBackendCalls == 1u);
  CHECK(p.releaseCalls == 0u);
  CHECK(teardown.poisoned());
  CHECK(p.drainCalls == 1u);
  CHECK(sink.gate.closed());
  // What decided it moved; what USED to decide it never did.
  CHECK(sink.pump.activityTotal() > beforeRun);
  CHECK(sink.pump.counters().callbackTotal == 0u);

  liar.stopForReal();
  CHECK(sink.gate.quiesced());
}

static void test_teardown_aSaturatedWitnessCannotShowQuiescence() {
  // A witness at the ceiling has stopped being a witness: `activity == baseline`
  // reads identically for a producer that stopped and for one calling in as fast
  // as a core allows, and those are different answers that must not reach the same
  // arm. The ceiling is not far away either -- red team measured 9.6 s of one
  // thread submitting in a loop, against the 497 days an honest 10 ms cadence
  // implies, and rt/capture_backend.h places no limit on cadence.
  GatedSink sink;
  sink.pump.reset(1000u);
  rt::PumpCountersTestAccess::seedActivity(sink.pump, rt::kWitnessSaturated);
  CHECK(sink.pump.activityTotal() == rt::kWitnessSaturated);

  TeardownProbe p;
  p.watched = &sink.gate;
  p.witness = &sink.pump;
  rt::Teardown teardown;
  const rt::TeardownOps ops = p.ops();

  // NO PRODUCER AT ALL: an idle, empty gate, which the first case in this task
  // proves takes the QUIESCED arm. The only thing different here is that the
  // evidence is unreadable, so this is the refusal and not a second measurement.
  CHECK(teardown.run(p.running, sink.gate, ops, rt::kQuiesceBudgetMs) ==
        rt::TeardownOutcome::kAbandoned);
  CHECK(p.releaseCalls == 0u);
  CHECK(teardown.poisoned());
  CHECK(p.drainCalls == 1u);
  // REFUSED WITHOUT SPENDING THE BUDGET: the answer is known before the wait, and
  // a saturated witness cannot become readable by being watched for longer.
  CHECK(p.sleepCalls == 0u);
}

static void test_teardown_theFourHooksTheDecisionIsMadeOfFailClosedWhenNull() {
  // TeardownOps documents a null hook as "skipped rather than dereferenced", and
  // FOUR of them are exceptions to that rule because skipping them does not
  // degrade one step -- it removes the decision. producerActivity IS the evidence;
  // sleepMs is the only thing that makes the window WALL TIME rather than 50
  // unsynchronised loads (red team collapsed it to microseconds that way, which is
  // the single sample #3197 PoC-1 defeated wearing a new name); nowNs is the unit
  // both the window and the budget are measured in.
  //
  // AND stopBackend, added on PR #3262 and the one that mattered most. It was the
  // permissive member of the set until then -- skipped, with the run free to go on
  // and report kQuiesced -- which made the ONE HOOK THAT DESTROYS THE OS ARTEFACT
  // the only one whose absence was tolerated. A run that skips it has asked
  // nothing to stop, so an idle producer proves nothing and a released handle over
  // a live tap is the outcome every other refusal here exists to prevent. Found by
  // @code-reviewer on PR #3262.
  for (rt::u32 missing = 0u; missing < 5u; ++missing) {
    TeardownProbe p;
    rt::TeardownOps ops = p.ops();
    if (missing == 0u) { ops.producerActivity = nullptr; }
    if (missing == 1u) { ops.sleepMs = nullptr; }
    if (missing == 2u) { ops.nowNs = nullptr; }
    if (missing == 3u) { ops.stopBackend = nullptr; }
    // missing == 4 is THE POSITIVE CONTROL, in the same loop against the same
    // probe: nothing nulled, and it must take the other arm. Without it this case
    // would pass just as well against a teardown that refused everything.
    const bool expectQuiesced = (missing == 4u);

    rt::Teardown teardown;
    CHECK(teardown.run(p.running, p.gate, ops, rt::kQuiesceBudgetMs) ==
          (expectQuiesced ? rt::TeardownOutcome::kQuiesced
                          : rt::TeardownOutcome::kAbandoned));
    CHECK(teardown.poisoned() != expectQuiesced);
    CHECK(p.releaseCalls == (expectQuiesced ? 1u : 0u));
    // Step 5 runs on both arms; the budget is spent on neither refusal.
    CHECK(p.drainCalls == 1u);
    CHECK(p.sleepCalls == (expectQuiesced ? rt::kSettleObserveMs : 0u));

    // WHERE the refusal happened, and this is the assertion that makes the case
    // non-vacuous. `sleepCalls == 0` alone is satisfied just as well by a wait
    // that ran to its iteration ceiling without ever yielding -- which is exactly
    // what a teardown that SKIPPED a null sleepMs instead of refusing it does, and
    // that mutant survived this case until this line existed. The witness is read
    // once by step 2b's baseline and once per iteration of the wait, so exactly
    // one read means the wait was never entered.
    if (missing == 0u) {
      CHECK(p.activityReads == 0u);      // step 2b skips a null hook too
    } else if (expectQuiesced) {
      CHECK(p.activityReads > 1u);
    } else {
      CHECK(p.activityReads == 1u);
    }
    // The backend was asked to stop on every arm that HAS the hook, including the
    // three refusals: step 2 precedes the decision and is not conditional on it.
    CHECK(p.stopBackendCalls == ((missing == 3u) ? 0u : 1u));
  }
}

static void test_teardown_aBackwardsClockCannotShowQuiescence() {
  // The companion to measuring the window in wall time: the subtraction that
  // measures it is UNSIGNED, so a clock that steps back turns a 3 ms observation
  // into an elapsed time of about 585 years and admits on the next sample. The
  // hook is documented monotone; a hook that is not gets the answer every other
  // unusable hook gets, rather than the most permissive one arithmetic allows.
  TeardownProbe p;
  // The epoch is the hook's own business (TeardownOps says so), so it starts high
  // enough that stepping back is a smaller number and not an underflow -- the
  // defect under test is in awaitSettled's arithmetic, not in the probe's.
  p.clockNs = 1000u * rt::kNsPerMs;
  p.rewindAtSleep = 3u;
  rt::Teardown teardown;
  const rt::TeardownOps ops = p.ops();

  // An IDLE, EMPTY gate, which every other case proves takes the quiesced arm.
  CHECK(teardown.run(p.running, p.gate, ops, rt::kQuiesceBudgetMs) ==
        rt::TeardownOutcome::kAbandoned);
  CHECK(teardown.poisoned());
  CHECK(p.releaseCalls == 0u);
  CHECK(p.drainCalls == 1u);
  // REFUSED ON THE SAMPLE AFTER THE STEP BACK, not by running out of budget: three
  // yields happened, the fourth reading was in the past, and that ended it.
  CHECK(p.sleepCalls == 3u);
}

static void test_teardown_theWindowAndTheBudgetAreWallTimeNotLoopIterations() {
  // REGRESSION-LOCK -- never delete. The loop used to count one iteration as one
  // millisecond, which is true only where a one-millisecond yield takes one
  // millisecond. On Windows -- #3196's platform, which inherits this contract --
  // the default timer resolution is ~15.6 ms, so 50 iterations was ~780 ms of
  // observation inside a nominal 250 ms budget: the window over-observed by 15x
  // and the budget was not a ceiling at all. Found by @code-reviewer on PR #3262.
  //
  // The probe's clock advances by what the HOST grants per yield (msPerSleep),
  // not by what the teardown asked for, which is the only way to express that
  // host in a test at all.
  TeardownProbe coarse;
  coarse.msPerSleep = 16u;
  rt::Teardown teardown;
  const rt::TeardownOps ops = coarse.ops();
  CHECK(teardown.run(coarse.running, coarse.gate, ops, rt::kQuiesceBudgetMs) ==
        rt::TeardownOutcome::kQuiesced);
  CHECK(!teardown.poisoned());
  // FOUR yields, not fifty: the fourth lands the clock at 64 ms, the first sample
  // at or past the 50 ms window. An iteration-counted loop spent fifty of them.
  CHECK(coarse.sleepCalls == 4u);
  // ...and one yield per millisecond of window on a host that grants what it is
  // asked for, which is the SAME 50 ms measured with the same clock.
  TeardownProbe fine;
  rt::Teardown second;
  const rt::TeardownOps fineOps = fine.ops();
  CHECK(second.run(fine.running, fine.gate, fineOps, rt::kQuiesceBudgetMs) ==
        rt::TeardownOutcome::kQuiesced);
  CHECK(fine.sleepCalls == rt::kSettleObserveMs);

  // AND THE BUDGET IS WALL TIME TOO, against a gate that never empties. Sixteen
  // yields of 16 ms reaches the 250 ms ceiling; counting iterations spent 250 of
  // them, which on that host is four seconds -- sixteen times the ceiling this
  // constant exists to impose, on the path that ends a user's share.
  TeardownProbe stuckProbe;
  stuckProbe.msPerSleep = 16u;
  GatedSink unused;
  unused.pump.reset(1000u);
  NeverQuiescingFake stuck(stuckProbe.gate);
  CHECK(stuck.start(oneTarget(7u), unused.pump, kFakeHost) == rt::BackendStart::kOk);
  CHECK(waitUntil([&stuck] { return stuck.inside(); }, 5000u));

  rt::Teardown third;
  const rt::TeardownOps stuckOps = stuckProbe.ops();
  CHECK(third.run(stuckProbe.running, stuckProbe.gate, stuckOps,
                  rt::kQuiesceBudgetMs) == rt::TeardownOutcome::kAbandoned);
  CHECK(third.poisoned());
  CHECK(stuckProbe.releaseCalls == 0u);
  CHECK(stuckProbe.sleepCalls == 16u);
  stuck.releaseAndWait();
}

static void test_teardown_aProducerThatSitsOutTheWindowIsStillRefusedByTheGate() {
  // REGRESSION-LOCK -- never delete. THE RESIDUAL, PINNED RATHER THAN DESCRIBED.
  //
  // This producer parks for the whole window and comes back afterwards, and the
  // teardown CANNOT refuse it -- not because the window is too short, but because
  // absence is not observable in finite time and any finite window can be slept
  // out. Widening it only names a longer nap. So this case asserts the honest
  // outcome (kQuiesced) and then asserts the thing the seam DOES guarantee, which
  // is why releasing the handle over a sleeping producer is safe at all: the gate
  // was closed before the wait and is never reopened, so every post-teardown call
  // this producer makes is refused and none of them reaches the handle.
  //
  // The part that is NOT bounded here is the tap's EXISTENCE, and that obligation
  // lives where it can actually be discharged -- the stop() post-condition in
  // rt/capture_backend.h, which PR 2's macOS backend owes a real barrier for.
  GatedSink sink;
  sink.pump.reset(1000u);
  LiveLiarFake liar(sink.pump, /*volunteersNoteCallback=*/true, /*parksOnStop=*/true);
  CHECK(liar.start(oneTarget(31u), sink.pump, kFakeHost) == rt::BackendStart::kOk);
  CHECK(waitUntil([&liar] { return liar.iterations() >= 2u; }, 5000u));

  TeardownProbe p;
  p.watched = &sink.gate;
  p.witness = &sink.pump;
  p.backend = &liar;
  rt::Teardown teardown;
  const rt::TeardownOps ops = p.ops();

  // The admission. Its stop() really did park the producer before returning, so
  // the witness is still for the whole window and there is nothing to see.
  CHECK(teardown.run(p.running, sink.gate, ops, rt::kQuiesceBudgetMs) ==
        rt::TeardownOutcome::kQuiesced);
  CHECK(p.releaseCalls == 1u);
  CHECK(!teardown.poisoned());
  CHECK(sink.gate.closed());

  // Emptied so the resumed producer's quanta can be PUSHED, because the pump only
  // signals on a successful push -- a full ring would make the assertion below
  // pass for a reason that has nothing to do with the gate.
  std::vector<std::vector<rt::u8>> discarded;
  drainAll(sink.ring, discarded);

  const rt::u32 tsfnBefore    = sink.tsfnCalls.load(std::memory_order_relaxed);
  const rt::u32 refusedBefore = sink.refusedCalls.load(std::memory_order_relaxed);
  const rt::u32 itersBefore   = liar.iterations();

  liar.resumeAfterStop();
  CHECK(waitUntil([&liar, itersBefore] { return liar.iterations() > itersBefore + 2u; },
                  5000u));

  // NON-VACUOUS: it really did call in, and every one of those calls was refused
  // at the gate. tsfnCalls standing still IS the memory-safety half of #3197 --
  // in production that counter is napi_call_threadsafe_function on a handle this
  // teardown already released.
  CHECK(sink.refusedCalls.load(std::memory_order_relaxed) > refusedBefore);
  CHECK(sink.tsfnCalls.load(std::memory_order_relaxed) == tsfnBefore);

  liar.stopForReal();
}

static void test_teardown_runTearsDownABackendThatFailedItsStart() {
  // REGRESSION-LOCK -- never delete. #3197 PoC-2. Start_JS's failure path used to
  // unwind by hand -- null the backend pointer, drop `running`, remove the hook,
  // release the handle -- which is four steps of five in the wrong order, and the
  // missing one is the only one that DESTROYS anything. A backend that acquired an
  // artefact before failing was orphaned with its only handle discarded, and no
  // later stop() could reach it.
  //
  // WHAT THIS LOCKS AND WHAT IT CANNOT -- and the NAME is part of that, which is
  // why it changed on PR #3262. This case never touches Start_JS: it calls
  // Teardown::run directly, so it cannot witness that the failure path in
  // napi/addon.cc actually REACHES this routine, and a name reading "a failed
  // start is torn down by the same five steps" claimed exactly that. napi/addon.cc
  // includes node_api.h, which nothing in this package can compile, so the WIRING
  // is asserted by the out-of-repo PoC harness alone and by nothing here. What
  // lives here is the property that wiring must satisfy: run THIS routine against
  // a backend whose start() failed holding a resource, and its stop() is called,
  // exactly once, by the same five steps. Found by @code-reviewer on PR #3262.
  GatedSink unused;
  unused.pump.reset(1000u);
  FailedStartFake fake;
  CHECK(fake.start(oneTarget(17u), unused.pump, kFakeHost) ==
        rt::BackendStart::kDeviceError);
  // The artefact outlives the failure -- that is the whole premise.
  CHECK(fake.acquired());
  CHECK(fake.stopCalls() == 0u);

  TeardownProbe p;
  p.backend = &fake;
  p.witness = &unused.pump;
  rt::Teardown teardown;
  const rt::TeardownOps ops = p.ops();

  CHECK(teardown.run(p.running, p.gate, ops, rt::kQuiesceBudgetMs) ==
        rt::TeardownOutcome::kQuiesced);

  // THE ONE ASSERTION THIS FILE EXISTS FOR, on this path: stop() was called, and
  // once -- not zero times by a hand-rolled unwind, and not twice by an unwind
  // that ran alongside a teardown.
  CHECK(fake.stopCalls() == 1u);
  CHECK(!fake.acquired());

  // And in the right order: the gate was shut before the handle was released.
  // A hand-rolled unwind released the handle with the gate still WIDE OPEN, which
  // is the use-after-release the gate exists to prevent -- reached through the
  // failure path rather than through the stop path.
  CHECK(p.releaseCalls == 1u);
  CHECK(p.gateClosedWhenReleased);
  CHECK(!p.gateClosedWhenBackendStopped);   // step 2 still precedes step 3
  CHECK(p.drainCalls == 1u);
  CHECK(!p.running.load(std::memory_order_acquire));
  // A backend that acquired nothing and produced nothing leaves an unpoisoned
  // process: a failed start is not by itself a reason to refuse the handle.
  CHECK(!teardown.poisoned());
  CHECK(p.markCount == 3u);
  CHECK(p.marks[0] == TeardownProbe::kMarkStopBackend);
  CHECK(p.marks[1] == TeardownProbe::kMarkRelease);
  CHECK(p.marks[2] == TeardownProbe::kMarkDrain);
}

static void test_teardown_boundedWaitOutlastsALateLeaver() {
  // The POSITIVE half of the budget, and the one a "just abandon it immediately"
  // simplification breaks: a caller that was inside the gate at close() and left
  // 25 ms later -- two and a half quantum periods -- must end in a RELEASED
  // handle and an unpoisoned process. Without the bounded wait every ordinary
  // stop that raced a callback would poison the child.
  TeardownProbe p;
  LateLeaver holder(p.gate, 25u);
  holder.begin();
  rt::u64 spins = 0u;
  while (!holder.inside()) {
    ++spins;
    CHECK(spins < 2000000000ull);
    std::this_thread::yield();
  }

  rt::Teardown teardown;
  const rt::TeardownOps ops = p.ops();
  CHECK(teardown.run(p.running, p.gate, ops, rt::kQuiesceBudgetMs) ==
        rt::TeardownOutcome::kQuiesced);
  CHECK(p.releaseCalls == 1u);
  CHECK(!teardown.poisoned());
  // Not vacuous: the gate was genuinely still occupied when close() ran, so the
  // sleeping poll -- not the spin -- is what answered.
  CHECK(p.sleepCalls >= 1u);
  CHECK(p.sleepCalls < rt::kQuiesceBudgetMs);
  CHECK(p.gate.quiesced());

  holder.finish();
}

int main() {
  test_packFrames_interleavedStereoIsAStraightCopy();
  test_packFrames_planarStereoInterleaves();
  test_packFrames_monoDuplicatesIntoBothChannels();
  test_packFrames_srcFrameOffsetSkipsWholeFrames();
  test_packFrames_refusesBadInput();

  test_pump_emitsExactlyOneQuantumPer480Frames();
  test_pump_regroupsAcrossCallbackSizes();
  test_pump_headerCarriesFirstFrameTimestamp();
  test_pump_headerAdvancesTimestampWithinOneCallback();
  test_pump_ringFullCountsDropAndAdvancesSeq();
  test_pump_countersSurviveReset();
  test_pump_refusesBadSubmissions();
  test_pump_faultNoneIsANoOp();
  test_pump_countersSaturateRatherThanWrapping();

  test_gate_enterSucceedsWhenOpen();
  test_gate_enterFailsAfterClose();
  test_gate_closeDoesNotEvictAnInFlightCaller();
  test_gate_failedEnterPreservesTheClosedBit();
  test_gate_entrantIsCountedBeforeItTestsTheClosedBit();
  test_gate_concurrentEnterLeaveVersusClose();

  test_backendContract_platformBackendIsAbsentInPr1();
  test_backendContract_aSystemMixIsOnlyReachableByAffirmativeRequest();
  test_backendContract_targetIsCopiedByValueAndBounded();
  test_buildTarget_admitsOneThroughEightAndRefusesNine();
  test_buildTarget_refusesEmptyListAndPidZero();
  test_buildTarget_refusesDuplicatePids();
  test_buildTarget_refusalPublishesNothing();
  test_buildTarget_carriesAllowDescendantsBothWays();
  test_backendContract_unsupportedFormatIsRefusedNotResampled();
  test_backendContract_startFailsClosedWithNoTarget();
  test_backendContract_bothShapesSatisfyThePostCondition();
  test_backendContract_callAfterStopIsRefusedByTheGate();
  test_backendContract_quiesceWaitExpiresOnABackendThatHoldsTheGate();

  test_teardown_quiescedRunReleasesTheHandleInTheRightOrder();
  test_teardown_timeoutAbandonsTheHandleAndPoisons();
  test_teardown_aLiveProducerIsNotTakenForAQuiescedOne();
  test_teardown_aSubmitOnlyProducerIsNotTakenForAQuiescedOne();
  test_teardown_aSaturatedWitnessCannotShowQuiescence();
  test_teardown_theFourHooksTheDecisionIsMadeOfFailClosedWhenNull();
  test_teardown_aBackwardsClockCannotShowQuiescence();
  test_teardown_theWindowAndTheBudgetAreWallTimeNotLoopIterations();
  test_teardown_aProducerThatSitsOutTheWindowIsStillRefusedByTheGate();
  test_teardown_runTearsDownABackendThatFailedItsStart();
  test_teardown_boundedWaitOutlastsALateLeaver();

  std::printf("rt_contract_test: %d checks passed\n", g_checks);
  return 0;
}
