// concord-audiocap / rt/platform/macos — the tap backend's state machine.
//
// EVERY CASE HERE RUNS ON LINUX. That is the whole design of hal_api.h: it
// names no Apple type, so the refusal ordering, the format admission and the
// teardown barrier are exercised by the ASAN/UBSAN/TSAN legs against a fake
// HAL. It matters more than usual because client/desktop/native/** sits outside
// sonar.sources, so no line of this code can ever count toward the >=80%
// new-code gate -- these cases ARE the gate (design section 9.9, A5).
//
// This file is test code and is NOT bound by the JSF++ profile.

#include "../rt/platform/macos/tap_backend.h"

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <atomic>
#include <chrono>
#include <thread>

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

namespace rt    = concord::audiocap::rt;
namespace macos = concord::audiocap::rt::macos;

// ---------------------------------------------------------------------------
// A fake HAL that records the ORDER of the calls it received.
//
// The order is the assertion in most of these cases, not just the return value:
// "refused" and "refused after creating a tap" are different outcomes, and only
// one of them is safe.
//
// WHAT THE ORDER STRING CAN AND CANNOT SEE. It records that a call was MADE, in
// sequence -- which is enough to catch a skipped destroy, a swapped destroy
// order, or a stopDevice that never ran, and all three of those mutants die
// here. It cannot see whether the call WORKED: a destroy that is invoked and
// returns an error produces a byte-identical string. That gap is why
// destroyFailures() exists on the backend and is asserted separately below;
// without it a failed AudioHardwareDestroyProcessTap leaves a live OS tap while
// every assertion in this file stays green.
// ---------------------------------------------------------------------------
namespace {

char     g_order[256];
rt::u32  g_orderLen        = 0u;
rt::u32  g_translateStatus = macos::kNoErr;
rt::u32  g_translateObject = 7u;
rt::u32  g_tapRate         = 48000u;
rt::u32  g_tapInterleaved  = 1u;
rt::u32  g_createTapStatus = macos::kNoErr;
// The three arms that run AFTER a tap exists -- i.e. the only ones on which a
// leaked tap is reachable -- had no knob at all, so removing destroyArtefacts()
// from any of them survived the whole suite.
rt::u32  g_aggregateStatus = macos::kNoErr;
rt::u32  g_ioProcStatus    = macos::kNoErr;
rt::u32  g_startStatus     = macos::kNoErr;
// Destroy-side failures, for the liveness question the order string cannot ask.
rt::u32  g_destroyTapStatus       = macos::kNoErr;
rt::u32  g_destroyAggregateStatus = macos::kNoErr;
rt::u32  g_destroyIoProcStatus    = macos::kNoErr;

void note(char c) { if (g_orderLen < 255u) { g_order[g_orderLen++] = c; } }

const char* order() { g_order[g_orderLen] = '\0'; return g_order; }

void resetFake() {
  g_orderLen        = 0u;
  g_order[0]        = '\0';
  g_translateStatus = macos::kNoErr;
  g_translateObject = 7u;
  g_tapRate         = 48000u;
  g_tapInterleaved  = 1u;
  g_createTapStatus = macos::kNoErr;
  g_aggregateStatus = macos::kNoErr;
  g_ioProcStatus    = macos::kNoErr;
  g_startStatus     = macos::kNoErr;
  g_destroyTapStatus       = macos::kNoErr;
  g_destroyAggregateStatus = macos::kNoErr;
  g_destroyIoProcStatus    = macos::kNoErr;
}

macos::HalApi fakeHal() {
  macos::HalApi h;
  h.translatePidToProcessObject = [](rt::u32, rt::u32* out) noexcept -> rt::u32 {
    note('t'); *out = g_translateObject; return g_translateStatus; };
  h.createProcessTap = [](const rt::u32*, rt::u32, rt::u32* o) noexcept -> rt::u32 {
    note('T'); *o = 11u; return g_createTapStatus; };
  h.tapFormat = [](rt::u32, rt::SourceFormat* f) noexcept -> rt::u32 {
    note('f');
    f->sampleRate   = g_tapRate;
    f->channelCount = 2u;
    f->interleaved  = (g_tapInterleaved != 0u);
    f->isFloat32    = true;
    return macos::kNoErr; };
  // Every create writes its out-param BEFORE returning the scripted status --
  // deliberately, because that is the shape hal_api.h does not forbid and the
  // shape under which discarding a handle leaks an OS artefact.
  h.createAggregate = [](rt::u32, rt::u32* o) noexcept -> rt::u32 {
    note('A'); *o = 21u; return g_aggregateStatus; };
  h.createIoProc = [](rt::u32, macos::HalApi::IoProcFn, void*, void** o) noexcept -> rt::u32 {
    note('P'); *o = reinterpret_cast<void*>(1); return g_ioProcStatus; };
  h.startDevice      = [](rt::u32, void*) noexcept -> rt::u32 { note('s'); return g_startStatus; };
  h.stopDevice       = [](rt::u32, void*) noexcept -> rt::u32 { note('S'); return macos::kNoErr; };
  h.destroyIoProc    = [](rt::u32, void*) noexcept -> rt::u32 { note('p'); return g_destroyIoProcStatus; };
  h.destroyAggregate = [](rt::u32) noexcept -> rt::u32 { note('a'); return g_destroyAggregateStatus; };
  h.destroyTap       = [](rt::u32) noexcept -> rt::u32 { note('D'); return g_destroyTapStatus; };
  return h;
}

// The pump needs a real ring; rt/ allocates nothing, so the storage is ours.
struct Sink {
  rt::u8          storage[static_cast<std::size_t>(rt::kRingSlots) *
                          static_cast<std::size_t>(rt::kQuantumBytes)];
  rt::QuantumRing ring;
  rt::QuantumPump pump;
  Sink() : storage(),
           ring(storage, sizeof(storage), rt::kRingSlots, rt::kQuantumBytes),
           pump(ring, nullptr, nullptr) { CHECK(ring.valid()); }
};

/// ADVANCES, and that is load-bearing rather than cosmetic. A constant clock
/// makes stop()'s BOUNDED wait unbounded -- the deadline is never reached, so a
/// gate that is not quiesced spins forever. 1 ms per call reaches the 50 ms
/// budget in 50 iterations.
rt::u64 fakeNowNs() noexcept {
  static rt::u64 t = 0u;
  t += 1000000ull;
  return t;
}
void    fakeSleep(rt::u32) noexcept {}

rt::HostServices fakeHost() {
  rt::HostServices h = {nullptr, nullptr, &fakeNowNs, &fakeSleep};
  return h;
}

rt::CaptureTarget onePid(rt::u32 pid) {
  rt::CaptureTarget t;
  const rt::u32 pids[1] = {pid};
  CHECK(rt::buildTarget(pids, 1u, false, &t));
  return t;
}

}  // namespace

// ---------------------------------------------------------------------------

static void test_floor_backendExistsOnlyAtOrAboveTheProductFloor() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  // The SYMBOL floor is 14.2 and the PRODUCT floor is 14.4. A machine between
  // them has the Core Audio symbols and is still refused the feature.
  macos::TapBackend below(hal, "14.3");
  CHECK(below.availableForThisOs() == false);
  macos::TapBackend at(hal, "14.4");
  CHECK(at.availableForThisOs() == true);
  macos::TapBackend here(hal, "26.6.2");
  CHECK(here.availableForThisOs() == true);
}

static void test_start_belowFloorRefusesWithoutTouchingTheOs() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.3");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);

  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kUnsupportedOs);
  CHECK(g_orderLen == 0u);
}

static void test_start_emptyTargetIsRefusedBeforeAnyOsCall() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;

  // The shape an omitted option, a zeroed struct and a FAILED PID LOOKUP all
  // take. A refusal that has already created a tap is #2161 with a cleanup step.
  rt::CaptureTarget empty;
  std::memset(&empty, 0, sizeof(empty));
  empty.scope    = rt::CaptureScope::kProcessList;
  empty.pidCount = 0u;

  CHECK(backend.start(empty, sink.pump, fakeHost()) == rt::BackendStart::kNoTarget);
  CHECK(g_orderLen == 0u);
}

static void test_start_systemMixIsRefusedByThisBackend() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;

  // buildTarget can never produce kSystemMix, so reaching it means a caller set
  // it by hand. PR 2's backend has no system-mix path at all -- the exclude-form
  // initializer is #2161's shape and is not constructed anywhere.
  rt::CaptureTarget mix;
  std::memset(&mix, 0, sizeof(mix));
  mix.scope = rt::CaptureScope::kSystemMix;

  CHECK(backend.start(mix, sink.pump, fakeHost()) == rt::BackendStart::kNoTarget);
  CHECK(g_orderLen == 0u);
}

// ---------------------------------------------------------------------------
// start() -- the ORDER is the assertion, not just the return value. "refused"
// and "refused after creating a tap" are different outcomes and only one of
// them is safe.
//
// Legend: t translate  T createTap  f tapFormat  A createAggregate
//         P createIoProc  s startDevice  S stopDevice  p destroyIoProc
//         a destroyAggregate  D destroyTap
// ---------------------------------------------------------------------------

static void test_start_translateNoErrWithUnknownObjectIsNoTarget() {
  resetFake();
  g_translateStatus = macos::kNoErr;        // the OS says fine...
  g_translateObject = macos::kObjectUnknown; // ...and resolved nothing. MEASURED.
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);

  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kNoTarget);
  // Translated, then STOPPED. Checking only the status would have built a tap
  // aimed at kAudioObjectUnknown -- ADR-0043 D4b risk 2 in its macOS form.
  CHECK(std::strcmp(order(), "t") == 0);
}

// THE SIBLING OF THE TEST ABOVE, AND THE POINT IS THAT THEY DIFFER. The case
// above is the OS answering correctly that this process has no audio object --
// normal, explicable, the user picked a window that has never made a sound. This
// one is the HAL refusing to answer at all, which is a device error. They used
// to return the same value, so a support ticket and a user's own choice of
// window were indistinguishable at the seam.
static void test_start_translateHalErrorIsADeviceErrorNotNoTarget() {
  resetFake();
  g_translateStatus = 0xDEADu;
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);

  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kDeviceError);
  CHECK(std::strcmp(order(), "t") == 0);
  // The raw OSStatus survives to status() instead of being flattened away.
  CHECK(backend.failStage() == macos::StartStage::kTranslatePid);
  CHECK(backend.failStatus() == 0xDEADu);
}

static void test_start_unsupportedRateIsRefusedAndTheTapDestroyed() {
  resetFake();
  g_tapRate = 192000u;   // what the device-and-stream initializer yields; rt/ refuses it
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);

  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kUnsupportedFormat);
  // Refused BEFORE startDevice, and the tap it had already created is destroyed
  // rather than leaked. A live tap outliving a refused start is the privacy
  // defect this epic exists to close.
  CHECK(std::strcmp(order(), "tTfD") == 0);
}

static void test_start_tapCreateFailureIsADeviceError() {
  resetFake();
  g_createTapStatus = 0xBEEFu;
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);

  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kDeviceError);
  // THE ASSERTION THIS TEST USED TO MAKE WAS WRONG, AND THE FAKE PROVED IT.
  // It read `strcmp(order(), "tT")` under the comment "Nothing to destroy: the
  // tap was never created" -- but this fake's createProcessTap writes `*o = 11u`
  // UNCONDITIONALLY and only then returns the scripted error, so a handle really
  // had been published. The old assertion passed only because start() threw that
  // handle away, which is precisely the leak. hal_api.h states no "out-param
  // untouched on failure" contract for this entry, so the state machine must
  // destroy whatever is set rather than trust that nothing was.
  CHECK(std::strcmp(order(), "tTD") == 0);
  CHECK(backend.failStage() == macos::StartStage::kCreateTap);
  CHECK(backend.failStatus() == 0xBEEFu);
}

static void test_start_admitsAt48kAndArmsInOrder() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);

  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kOk);
  CHECK(std::strcmp(order(), "tTfAPs") == 0);
}

static void test_start_secondStartIsRefusedAsAlreadyRunning() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kOk);

  resetFake();
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kAlreadyRunning);
  CHECK(g_orderLen == 0u);   // and it touched nothing
}

static void test_ioProc_deliversThroughTheGateAndCountsSignal() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kOk);

  // Drive the entry point the HAL would call. One quantum of audible stereo.
  float frames[static_cast<std::size_t>(rt::kFrameCount) * 2u];
  for (std::size_t i = 0u; i < sizeof(frames) / sizeof(frames[0]); ++i) {
    frames[i] = 0.25f;
  }
  const rt::u8* planes[1] = {reinterpret_cast<const rt::u8*>(frames)};
  macos::TapBackend::ioProcEntry(&backend, planes, 2u, true, rt::kFrameCount, 0u);

  const rt::PumpCounters c = sink.pump.counters();
  CHECK(c.callbackTotal == 1u);
  CHECK(c.signalTotal   == 1u);   // audible, so the silence latch can never fire
  CHECK(c.silentSinceStart == false);
}

static void test_ioProc_aNullContextIsSurvivable() {
  // Defensive: the HAL hands back whatever ctx it was given, and a backend that
  // failed to arm must not turn a stray callback into a null deref.
  float frames[8] = {0.0f, 0.0f, 0.0f, 0.0f, 0.0f, 0.0f, 0.0f, 0.0f};
  const rt::u8* planes[1] = {reinterpret_cast<const rt::u8*>(frames)};
  macos::TapBackend::ioProcEntry(nullptr, planes, 2u, true, 4u, 0u);
  CHECK(true);   // reaching here is the assertion
}

static void test_start_systemMixWithPidsIsStillRefused() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;

  // THE CASE THE ZEROED-STRUCT TEST CANNOT SEE. With pidCount == 0 the empty
  // list refusal fires first, so removing the SCOPE check changes nothing there.
  // Here the list is non-empty, so only the scope check stands between a
  // hand-set kSystemMix and a tap -- and buildTarget can never produce this
  // shape, so reaching it means a caller assigned it deliberately.
  rt::CaptureTarget mix = onePid(4242u);
  mix.scope = rt::CaptureScope::kSystemMix;

  CHECK(backend.start(mix, sink.pump, fakeHost()) == rt::BackendStart::kNoTarget);
  CHECK(g_orderLen == 0u);
}

static void test_start_tapFormatReturningNoErrWithoutWritingIsRefused() {
  resetFake();
  macos::HalApi hal = fakeHal();
  // kNoErr AND WRITES NOTHING. This is the same shape R9 measured on
  // TranslatePIDToProcessObject -- an API reporting success while leaving the
  // out-parameter untouched -- and it is why `format` is zero-initialised
  // before the call rather than pre-filled with something plausible. A struct
  // seeded with 48 kHz would be ADMITTED here on a HAL that told us nothing.
  hal.tapFormat = [](rt::u32, rt::SourceFormat*) noexcept -> rt::u32 {
    note('f'); return macos::kNoErr; };
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);

  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kUnsupportedFormat);
  CHECK(std::strcmp(order(), "tTfD") == 0);
}

// ---------------------------------------------------------------------------
// stop() -- the barrier Apple documents no guarantee for.
//
// AudioDeviceStop is documented to stop callbacks and NOT documented to drain
// one already in flight. HostServices::spawn/join are nullptr in a shipped
// binary, so there is no thread to join. The artefact is plural with an
// ordering. These cases pin all three.
// ---------------------------------------------------------------------------

static void test_stop_stopsTheDeviceThenDestroysInOrder() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kOk);

  resetFake();
  backend.stop();
  CHECK(std::strcmp(order(), "SpaD") == 0);
}

static void test_stop_destroysEvenWhenTheQuiesceWaitExpires() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);

  // A clock that jumps past any deadline on its second read, so the wait
  // expires immediately.
  rt::HostServices host = {nullptr, nullptr,
                           []() noexcept -> rt::u64 {
                             static rt::u32 n = 0u;
                             return (n++ == 0u) ? 0u : 999000000000ull;
                           },
                           &fakeSleep};
  CHECK(backend.start(target, sink.pump, host) == rt::BackendStart::kOk);

  resetFake();
  backend.stop();
  // THE LOAD-BEARING ASSERTION. A stop() that skipped destruction because its
  // wait expired would leave behind exactly the artefact this epic is about:
  // the privacy invariant is the tap's EXISTENCE, and the seam provably cannot
  // end it. Destroying under a possibly-live callback can dangle Apple's state,
  // never ours.
  CHECK(std::strcmp(order(), "SpaD") == 0);
}

static void test_stop_nullSleepHookFailsClosedAndStillDestroys() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);
  // A host that omits the hook: the wait CANNOT be performed, so the backend
  // takes its timeout arm rather than assuming quiescence -- and destruction
  // still runs.
  rt::HostServices host = {nullptr, nullptr, &fakeNowNs, nullptr};
  CHECK(backend.start(target, sink.pump, host) == rt::BackendStart::kOk);

  resetFake();
  backend.stop();
  CHECK(std::strcmp(order(), "SpaD") == 0);
  // FAILS CLOSED, and this is the assertion that makes that phrase mean
  // something. The wait could not be performed at all, so the backend reports
  // that it did not observe its callbacks stop -- the same answer it gives for
  // an expired wait, deliberately, because the two facts are the same fact.
  // Without this the whole block was mutation-dead: all three stop() tests
  // asserted the identical order string and nothing read the outcome.
  CHECK(!backend.quiesceProved());
}

static void test_stop_isIdempotent() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kOk);

  backend.stop();
  resetFake();
  backend.stop();
  // A second stop() makes NO os call at all -- not a second AudioDeviceStop on
  // a destroyed aggregate, which is the shape that turns a clean teardown into
  // a crash in the child.
  CHECK(g_orderLen == 0u);
}

static void test_stop_closesTheGateSoALateCallbackReachesNothing() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kOk);

  float frames[static_cast<std::size_t>(rt::kFrameCount) * 2u];
  for (std::size_t i = 0u; i < sizeof(frames) / sizeof(frames[0]); ++i) { frames[i] = 0.5f; }
  const rt::u8* planes[1] = {reinterpret_cast<const rt::u8*>(frames)};

  macos::TapBackend::ioProcEntry(&backend, planes, 2u, true, rt::kFrameCount, 0u);
  const rt::u32 before = sink.pump.counters().callbackTotal;
  CHECK(before == 1u);

  backend.stop();

  // A producer that resumes after stop() returned is refused at the gate: no
  // bytes reach the consumer. This is the mutant that survived task 5 -- it was
  // untestable until stop() existed to close the gate.
  macos::TapBackend::ioProcEntry(&backend, planes, 2u, true, rt::kFrameCount, 0u);
  CHECK(sink.pump.counters().callbackTotal == before);
}

static void test_stop_beforeStartIsANoOp() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  backend.stop();
  CHECK(g_orderLen == 0u);
}

// ---------------------------------------------------------------------------
// THE ASSERTION THIS WHOLE TASK EXISTS FOR: destruction happens even when a
// callback is genuinely still INSIDE the sink when stop() is called.
//
// The earlier timeout case could not prove it -- with nothing in flight the
// gate reads quiesced, so a stop() that destroyed only on quiescence passed it.
// This one holds a real thread inside the gate across the whole of stop().
// ---------------------------------------------------------------------------
namespace {
std::atomic<bool> g_ioInside{false};
std::atomic<bool> g_ioRelease{false};

/// Runs on the IOProc thread, INSIDE the gate, when a whole quantum completes.
/// BOUNDED, like its sibling below. This one cannot hang on current code either
/// -- main always sets g_ioRelease -- but an unbounded spin in a test turns any
/// future regression into a silent CI timeout that produces no output at all,
/// and rt-sanitizers carries no timeout-minutes. A deadline converts a hang into
/// a named failure, which is the only difference that matters at 3am.
void blockingSignal(void*) noexcept {
  g_ioInside.store(true, std::memory_order_release);
  const std::chrono::steady_clock::time_point deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(10);
  while (!g_ioRelease.load(std::memory_order_acquire)) {
    if (std::chrono::steady_clock::now() >= deadline) { return; }
    std::this_thread::yield();
  }
}

struct BlockingSink {
  rt::u8          storage[static_cast<std::size_t>(rt::kRingSlots) *
                          static_cast<std::size_t>(rt::kQuantumBytes)];
  rt::QuantumRing ring;
  rt::QuantumPump pump;
  BlockingSink() : storage(),
                   ring(storage, sizeof(storage), rt::kRingSlots, rt::kQuantumBytes),
                   pump(ring, &blockingSignal, nullptr) { CHECK(ring.valid()); }
};
}  // namespace

static void test_stop_destroysWhileACallbackIsStillInsideTheGate() {
  resetFake();
  g_ioInside.store(false);
  g_ioRelease.store(false);

  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  BlockingSink sink;
  rt::CaptureTarget target = onePid(4242u);
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kOk);

  static float frames[static_cast<std::size_t>(rt::kFrameCount) * 2u];
  for (std::size_t i = 0u; i < sizeof(frames) / sizeof(frames[0]); ++i) { frames[i] = 0.5f; }

  std::thread producer([&backend]() {
    const rt::u8* planes[1] = {reinterpret_cast<const rt::u8*>(frames)};
    // Completes a whole quantum, so the pump emits and the blocking signal runs
    // while this thread is still inside the gate.
    macos::TapBackend::ioProcEntry(&backend, planes, 2u, true, rt::kFrameCount, 0u);
  });

  // Traced: emit() pushes onto a fresh 8-slot ring, which always returns kOk, so
  // signal_ always fires and this cannot hang today. The bound is for the change
  // that stops it firing -- without one the job burns its whole budget and
  // reports nothing, instead of failing here with a name.
  {
    const std::chrono::steady_clock::time_point deadline =
        std::chrono::steady_clock::now() + std::chrono::seconds(10);
    while (!g_ioInside.load(std::memory_order_acquire)) {
      CHECK(std::chrono::steady_clock::now() < deadline);
      std::this_thread::yield();
    }
  }

  resetFake();
  backend.stop();   // the gate CANNOT quiesce: the producer is parked inside it

  // Destroyed anyway, in order. A stop() that waited for quiescence before
  // destroying would leave a live OS tap behind -- the privacy defect this epic
  // is about -- and a stop() that hung here would never return at all.
  CHECK(std::strcmp(order(), "SpaD") == 0);
  // AND THE BACKEND KNOWS IT COULD NOT PROVE QUIESCENCE. This is the ONLY case
  // in the file where the gate is genuinely non-empty while both host hooks are
  // present, so it is the only one that can tell `quiesceProved_ =
  // gate_.quiesced()` apart from `quiesceProved_ = true`. The expired-clock test
  // cannot: its gate is already empty, so the wait exits on the first poll and
  // true is the correct answer there.
  CHECK(!backend.quiesceProved());

  g_ioRelease.store(true, std::memory_order_release);
  producer.join();
}

// ---------------------------------------------------------------------------
// The three arms that run AFTER a tap exists. These are the only paths on which
// a leaked OS tap is reachable, and until now the fake could not script any of
// them -- so removing destroyArtefacts() from all three survived the suite.
//
// The expected strings are derived from the POST-CONDITION, not from reading
// the implementation: every artefact that exists is destroyed, in the mandated
// order, and stopDevice runs first whenever an IOProc may exist.
// ---------------------------------------------------------------------------

static void test_start_aggregateFailureDestroysTheTapAndTheAggregate() {
  resetFake();
  g_aggregateStatus = 0xA11Eu;
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);

  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kDeviceError);
  // The fake published an aggregate before reporting failure, so BOTH artefacts
  // exist and both are destroyed. No stopDevice: there is no IOProc.
  CHECK(std::strcmp(order(), "tTfAaD") == 0);
  CHECK(backend.failStage() == macos::StartStage::kCreateAggregate);
  CHECK(backend.failStatus() == 0xA11Eu);
  CHECK(backend.destroyFailures() == 0u);
}

static void test_start_ioProcFailureStopsTheDeviceThenDestroysEverything() {
  resetFake();
  g_ioProcStatus = 0xB0B0u;
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);

  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kDeviceError);
  // An IOProc MAY exist here -- the fake published a procId before failing --
  // so this arm owes the full barrier, stopDevice included. It used to run a
  // bare destroyArtefacts() with the gate still open.
  CHECK(std::strcmp(order(), "tTfAPSpaD") == 0);
  CHECK(backend.failStage() == macos::StartStage::kCreateIoProc);
}

static void test_start_startDeviceFailureRunsTheFullBarrier() {
  resetFake();
  g_startStatus = 0xC0DEu;
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);

  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kDeviceError);
  CHECK(std::strcmp(order(), "tTfAPsSpaD") == 0);
  CHECK(backend.failStage() == macos::StartStage::kStartDevice);
  CHECK(backend.failStatus() == 0xC0DEu);
}

// ---------------------------------------------------------------------------
// The liveness question the order string structurally cannot ask.
// ---------------------------------------------------------------------------

static void test_stop_aFailedDestroyIsCountedRatherThanSwallowed() {
  resetFake();
  g_destroyTapStatus = 0xDEADu;
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kOk);

  resetFake();
  g_destroyTapStatus = 0xDEADu;   // resetFake() cleared it; this is the case
  backend.stop();

  // The call ORDER is byte-identical to a clean teardown -- which is exactly why
  // this assertion cannot be the order string. A live OS tap on another
  // application's audio survived, and destroyFailures() is the only evidence
  // anywhere in the process that it did.
  CHECK(std::strcmp(order(), "SpaD") == 0);
  CHECK(backend.destroyFailures() == 1u);
}

static void test_stop_aCleanTeardownCountsNoDestroyFailures() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kOk);
  backend.stop();
  // The negative control for the test above: without it, a destroyFailures()
  // that was always 1 would pass there and prove nothing.
  CHECK(backend.destroyFailures() == 0u);
  CHECK(backend.quiesceProved());
}

// ---------------------------------------------------------------------------
// Caller-supplied target bounds.
// ---------------------------------------------------------------------------

static void test_start_anOversizedPidListIsRefusedBeforeAnyOsCall() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;

  // pidCount is a u8 and target.pids is kMaxTargetPids long, so this is one
  // element past the end of BOTH that array and the u32 objects[] start()
  // writes into. Without the bound it was a stack write overflow; with it, a
  // refusal that touches no OS call at all.
  rt::CaptureTarget target = onePid(4242u);
  target.pidCount = static_cast<rt::u8>(rt::kMaxTargetPids + 1u);

  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kNoTarget);
  CHECK(std::strcmp(order(), "") == 0);

  // The boundary itself is admitted, so the guard is `>` and not `>=`.
  resetFake();
  macos::TapBackend atLimit(hal, "14.4");
  Sink sink2;
  rt::CaptureTarget full = onePid(4242u);
  full.pidCount = rt::kMaxTargetPids;
  for (rt::u8 i = 0u; i < rt::kMaxTargetPids; ++i) { full.pids[i] = 4242u + i; }
  CHECK(atLimit.start(full, sink2.pump, fakeHost()) == rt::BackendStart::kOk);
  atLimit.stop();
}

// ---------------------------------------------------------------------------
// Format admission: the flag the thunk assumes.
// ---------------------------------------------------------------------------

static void test_start_aNonInterleavedFormatIsRefusedAndTheTapDestroyed() {
  resetFake();
  g_tapInterleaved = 0u;   // kAudioFormatFlagIsNonInterleaved
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);

  // ioProcThunk hardcodes interleaved = true, so admitting a planar format
  // would silently duplicate the left channel and drop the right. Refused
  // where the assumption is made -- not in the shared acceptsSourceFormat,
  // which must stay honest for rt/frame_pack.h's planar arm.
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kUnsupportedFormat);
  CHECK(std::strcmp(order(), "tTfD") == 0);
}

// ---------------------------------------------------------------------------
// One capture per instance.
// ---------------------------------------------------------------------------

static void test_start_afterACleanStopIsRefusedRatherThanOpeningASilentTap() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kOk);
  backend.stop();

  // The gate is closed permanently, so a second start() would build the whole
  // graph against the OS -- a real tap on the target process -- and then have
  // every callback refused at the gate. kOk for a structurally silent capture
  // is worse than a refusal, and ZERO OS calls is the assertion.
  resetFake();
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kAlreadyRunning);
  CHECK(std::strcmp(order(), "") == 0);
}

static void test_start_afterAFailedStartIsAlsoRefused() {
  resetFake();
  g_startStatus = 0xC0DEu;
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kDeviceError);

  // A failed start ran teardown, which closed the gate -- so this instance is
  // spent for the same reason a cleanly stopped one is.
  resetFake();
  g_startStatus = macos::kNoErr;
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kAlreadyRunning);
  CHECK(std::strcmp(order(), "") == 0);
}

static void test_start_aPreOsRefusalLeavesTheInstanceUsable() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;

  // The counter-case to the two above, and the reason teardown() early-returns
  // rather than marking every refusal spent: nothing was created and no gate
  // was closed, so a caller that fixes its target may proceed.
  rt::CaptureTarget empty = onePid(4242u);
  empty.pidCount = 0u;
  CHECK(backend.start(empty, sink.pump, fakeHost()) == rt::BackendStart::kNoTarget);
  CHECK(std::strcmp(order(), "") == 0);

  rt::CaptureTarget good = onePid(4242u);
  CHECK(backend.start(good, sink.pump, fakeHost()) == rt::BackendStart::kOk);
  backend.stop();
}

// A mid-capture channel change is NAMED, not silently starved.
//
// submit() refuses srcChannels > kChannels and returns false, so a tap that
// started at 2ch and began delivering 6ch went totally silent while
// callbackTotal kept climbing -- indistinguishable at the seam from the
// capture-starved shape the silence latch reports. The fault says which.
static void test_ioProc_aChannelCountChangeFaultsRatherThanStarving() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kOk);
  CHECK(sink.pump.counters().faulted == false);

  static float frames[static_cast<std::size_t>(rt::kFrameCount) * 2u];
  const rt::u8* planes[1] = {reinterpret_cast<const rt::u8*>(frames)};
  // The fake admitted 2 channels; the OS now says 6.
  macos::TapBackend::ioProcEntry(&backend, planes, 6u, true, rt::kFrameCount, 0u);

  CHECK(sink.pump.counters().faulted == true);
  CHECK(sink.pump.counters().faultReason ==
        static_cast<rt::u8>(rt::PumpFault::kFormatChanged));
  backend.stop();
}

// THE COUNTER-CASE, and it is why the guard tests frameCount first. A degenerate
// callback carries channels == 0 BY CONSTRUCTION -- tap_backend.mm emits exactly
// that for an empty AudioBufferList -- and it is the starvation shape, not a
// format change. Faulting on it would convert the thing the silence detector
// exists to observe into a teardown.
static void test_ioProc_aDegenerateCallbackIsNotAFormatChange() {
  resetFake();
  const macos::HalApi hal = fakeHal();
  macos::TapBackend backend(hal, "14.4");
  Sink sink;
  rt::CaptureTarget target = onePid(4242u);
  CHECK(backend.start(target, sink.pump, fakeHost()) == rt::BackendStart::kOk);

  macos::TapBackend::ioProcEntry(&backend, nullptr, 0u, true, 0u, 1000u);

  CHECK(sink.pump.counters().faulted == false);
  CHECK(sink.pump.counters().callbackTotal == 1u);
  backend.stop();
}

int main() {
  test_floor_backendExistsOnlyAtOrAboveTheProductFloor();
  test_start_belowFloorRefusesWithoutTouchingTheOs();
  test_start_emptyTargetIsRefusedBeforeAnyOsCall();
  test_start_systemMixIsRefusedByThisBackend();
  test_start_anOversizedPidListIsRefusedBeforeAnyOsCall();

  test_start_translateNoErrWithUnknownObjectIsNoTarget();
  test_start_translateHalErrorIsADeviceErrorNotNoTarget();
  test_start_unsupportedRateIsRefusedAndTheTapDestroyed();
  test_start_tapCreateFailureIsADeviceError();
  test_start_admitsAt48kAndArmsInOrder();
  test_start_secondStartIsRefusedAsAlreadyRunning();
  test_ioProc_deliversThroughTheGateAndCountsSignal();
  test_ioProc_aNullContextIsSurvivable();
  test_ioProc_aChannelCountChangeFaultsRatherThanStarving();
  test_ioProc_aDegenerateCallbackIsNotAFormatChange();
  test_start_systemMixWithPidsIsStillRefused();
  test_start_tapFormatReturningNoErrWithoutWritingIsRefused();
  test_start_aNonInterleavedFormatIsRefusedAndTheTapDestroyed();

  test_start_aggregateFailureDestroysTheTapAndTheAggregate();
  test_start_ioProcFailureStopsTheDeviceThenDestroysEverything();
  test_start_startDeviceFailureRunsTheFullBarrier();

  test_start_afterACleanStopIsRefusedRatherThanOpeningASilentTap();
  test_start_afterAFailedStartIsAlsoRefused();
  test_start_aPreOsRefusalLeavesTheInstanceUsable();

  test_stop_stopsTheDeviceThenDestroysInOrder();
  test_stop_destroysEvenWhenTheQuiesceWaitExpires();
  test_stop_nullSleepHookFailsClosedAndStillDestroys();
  test_stop_isIdempotent();
  test_stop_closesTheGateSoALateCallbackReachesNothing();
  test_stop_beforeStartIsANoOp();
  test_stop_destroysWhileACallbackIsStillInsideTheGate();
  test_stop_aFailedDestroyIsCountedRatherThanSwallowed();
  test_stop_aCleanTeardownCountsNoDestroyFailures();

  std::printf("macos_tap_test: %d checks passed\n", g_checks);
  return 0;
}
