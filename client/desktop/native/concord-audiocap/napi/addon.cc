// concord-audiocap / napi — the Node-API seam.
//
// JSF++ DOES NOT BIND HERE, by decision (ADR-0043 D2, and the named deviation in
// [internal]rules/native-audio.md). N-API must allocate to create JS values, so
// holding this layer to AV 206 would be a lie. The boundary is the point: rt/ keeps
// the properties that matter on an audio callback thread, and this file is where
// everything V8-shaped is confined.
//
// This uses the PLAIN C node_api.h, not node-addon-api. Two reasons, and the first
// is the stronger:
//
//   1. Zero new npm dependencies. ADR-0043 D1 rejects vendoring a third-party native
//      binary into an E2EE client on supply-chain grounds; adding one to build our
//      own would be the same argument pointed the other way.
//   2. It satisfies AV 208 at the seam by construction rather than by a compiler
//      flag — the C API has no exceptions to disable.

#include <node_api.h>
#include <uv.h>     // uv_thread_*, uv_sleep, uv_hrtime -- see the capture seam

#include <atomic>
#include <cstddef>  // size_t, named directly in the macOS sysctl branch
#include <cstdint>
#include <cstdio>   // std::snprintf
#include <cstring>

#include "../rt/capture_backend.h"
#include "../rt/quantum_header.h"
#include "../rt/quantum_pump.h"
#include "../rt/quantum_ring.h"
#include "../rt/sink_gate.h"
#include "../rt/teardown.h"
#ifdef CONCORD_AUDIOCAP_SYNTHETIC
#  include "../rt/synthetic_source.h"
#endif

#if defined(_WIN32)
#  define WIN32_LEAN_AND_MEAN
#  define NOMINMAX
#  include <windows.h>
#elif defined(__APPLE__)
#  include <sys/sysctl.h>
#  include "../rt/platform/macos/tap_floor.h"
#endif

namespace {

// ---------------------------------------------------------------------------
// Platform capability probe — the input to ADR-0043 D6's capability ladder.
//
// The ladder's bottom rung is "below the OS floor -> video-only, and the UI says
// so", which is #2161's invariant restated. That rung needs a truthful answer to
// "is per-process capture available here", and this is where it comes from.
// ---------------------------------------------------------------------------

// Documented floor for the Windows Application Loopback API: build 20348
// (Server 2022), which is ABOVE Windows 10 22H2 (19045). Sources disagree, some
// placing it at 19041.
//
// THIS CONSTANT IS PROVISIONAL AND IS THE ONE LINE SPIKE S2 CHANGES. It is written
// here, alone and named, precisely so that settling risk 3 on hardware is a
// one-line edit rather than a hunt. Do not scatter the number.
#if defined(_WIN32)
constexpr unsigned long kWindowsProcessLoopbackFloorBuild = 20348ul;
#endif

// THE macOS FLOOR IS NOT DECLARED HERE ANY MORE. It lives in
// rt/platform/macos/tap_floor.h as a pure predicate, because after #3197 PR 2
// there are TWO readers -- this probe, and rt::platformBackend(), which is the
// actual enforcement point -- and two copies of a version comparison are two
// chances to disagree about which machines may capture. Design section 9.3.

struct Capability {
  const char*   platform;
  char          osVersion[64];
  bool          perProcessAudio;
  const char*   reason;
};

#if defined(_WIN32)
// RtlGetVersion, not GetVersionEx. GetVersionEx reports 6.2 for an unmanifested
// binary on every OS since Windows 8, which would make this probe report a
// fabricated build number for the one question it exists to answer.
unsigned long windowsBuildNumber() {
  typedef LONG(WINAPI * RtlGetVersionFn)(PRTL_OSVERSIONINFOW);
  HMODULE nt = GetModuleHandleW(L"ntdll.dll");
  if (nt == nullptr) { return 0ul; }
  RtlGetVersionFn fn = reinterpret_cast<RtlGetVersionFn>(
      reinterpret_cast<void*>(GetProcAddress(nt, "RtlGetVersion")));
  if (fn == nullptr) { return 0ul; }
  RTL_OSVERSIONINFOW vi;
  ZeroMemory(&vi, sizeof(vi));
  vi.dwOSVersionInfoSize = sizeof(vi);
  if (fn(&vi) != 0) { return 0ul; }
  return vi.dwBuildNumber;
}
#endif

Capability probeCapability() {
  // `= {}` and not a bare declaration: Capability is POD and is returned BY VALUE,
  // so a bare declaration leaves osVersion[1..63] indeterminate even though
  // osVersion[0] is set below. Value-initialising the whole struct also means
  // every field has a defined value on a platform arm that forgets one.
  Capability cap    = {};
  cap.platform        = "unsupported";
  cap.osVersion[0]    = '\0';
  cap.perProcessAudio = false;
  cap.reason          = "";

#if defined(_WIN32)
  cap.platform = "win32";
  const unsigned long build = windowsBuildNumber();
  std::snprintf(cap.osVersion, sizeof(cap.osVersion), "%lu", build);
  if (build == 0ul) {
    cap.reason = "could not read the OS build number";
  } else if (build < kWindowsProcessLoopbackFloorBuild) {
    cap.perProcessAudio = false;
    cap.reason          = "Windows build is below the Application Loopback floor";
  } else {
    cap.perProcessAudio = true;
    cap.reason          = "";
  }

#elif defined(__APPLE__)
  cap.platform = "darwin";
  // kern.osproductversion is the marketing version ("14.4.1"), which is what the
  // 14.4 floor is expressed in. kern.osrelease is the Darwin version and would
  // need a mapping table nobody would keep current.
  size_t len = sizeof(cap.osVersion);
  if (sysctlbyname("kern.osproductversion", cap.osVersion, &len, nullptr, 0) != 0) {
    cap.osVersion[0] = '\0';
    cap.reason       = "could not read kern.osproductversion";
  } else {
    // sysctlbyname NUL-terminates a string node that fits and returns ENOMEM
    // otherwise, so this is a backstop rather than a fix. It is cheap and the
    // buffer is handed to napi_create_string_utf8 with NAPI_AUTO_LENGTH, which
    // walks to a NUL -- a future node or a partial write turns a defensive gap
    // into an over-read.
    cap.osVersion[sizeof(cap.osVersion) - 1] = '\0';
    // ONE predicate, shared with rt::platformBackend(). It handles the optional
    // patch component ("14.4" and "14.4.1" both occur) itself.
    //
    // A DELIBERATE BEHAVIOUR CHANGE from the std::sscanf("%d.%d") this replaces,
    // and it is a TIGHTENING: sscanf reads "14.4x" as 14.4 and reports CAPABLE,
    // while the predicate refuses anything it cannot fully parse. Neither form is
    // reachable today -- kern.osproductversion is plain numeric, measured
    // "26.6.2" -- but the two differ, and the predicate fails CLOSED where sscanf
    // failed open, which is the posture every other refusal in this addon takes.
    // The cost if Apple ever adds a suffix is a capable machine denied capture
    // rather than an unreadable one granted it.
    // Fully qualified: the `namespace rt =` alias is declared further down
    // (addon.cc:312), after this probe.
    //
    // ONE PREDICATE, TWO REASONS. meetsTapFloor answers the DECISION and
    // collapses "below the floor" with "unreadable", which is right -- both must
    // refuse. The DIAGNOSTIC must not collapse them: an unparseable string
    // reported as "below the floor" tells a user on macOS 26 that their OS is
    // too old, which may be flatly untrue, and index.d.ts documents `reason` as
    // words fit to show a user. The sibling sysctl-failure branch above already
    // keeps its own distinct reason; this one lost its when the sscanf form was
    // replaced by the predicate, and the two now match again.
    if (concord::audiocap::rt::macos::meetsTapFloor(cap.osVersion)) {
      cap.perProcessAudio = true;
    } else if (!concord::audiocap::rt::macos::parsesAsVersion(cap.osVersion)) {
      cap.reason = "could not read the macOS version";
    } else {
      cap.reason = "macOS is below the CoreAudio process-tap floor";
    }
  }

#elif defined(__linux__)
  cap.platform = "linux";
  // PipeWire per-node capture is the equivalent primitive and is explicitly out of
  // scope for ADR-0043. The ladder's bottom rung covers Linux as video-only.
  cap.reason = "per-process audio capture is not implemented on Linux";

#else
  // index.d.ts documents reason as "Empty when supported; otherwise why not, in
  // words fit to show a user" -- so the one platform class that is definitionally
  // unsupported must not be the one that returns an empty reason.
  cap.reason = "this platform is not supported";
#endif

  return cap;
}

// ---------------------------------------------------------------------------
// N-API plumbing
// ---------------------------------------------------------------------------

// Every N-API call returns a status. Dropping one is how a half-built object
// reaches JavaScript looking complete, so nothing here is unchecked.
#define NAPI_CALL(env, call)                                                   \
  do {                                                                         \
    if ((call) != napi_ok) {                                                   \
      napi_throw_error((env), nullptr, "concord-audiocap: N-API call failed"); \
      return nullptr;                                                          \
    }                                                                          \
  } while (0)

napi_status setStringProp(napi_env env, napi_value obj, const char* key,
                          const char* value) {
  napi_value v = nullptr;
  napi_status s = napi_create_string_utf8(env, value, NAPI_AUTO_LENGTH, &v);
  if (s != napi_ok) { return s; }
  return napi_set_named_property(env, obj, key, v);
}

napi_status setBoolProp(napi_env env, napi_value obj, const char* key, bool value) {
  napi_value v = nullptr;
  napi_status s = napi_get_boolean(env, value, &v);
  if (s != napi_ok) { return s; }
  return napi_set_named_property(env, obj, key, v);
}

// u32, never int32: the counters saturate at 0xFFFFFFFF and a signed create
// would deliver that as -1, which reads as "nothing ever happened" -- the one
// answer a saturating counter must never be able to give.
napi_status setUint32Prop(napi_env env, napi_value obj, const char* key,
                          std::uint32_t value) {
  napi_value v = nullptr;
  napi_status s = napi_create_uint32(env, value, &v);
  if (s != napi_ok) { return s; }
  return napi_set_named_property(env, obj, key, v);
}

napi_value Capability_JS(napi_env env, napi_callback_info /*info*/) {
  const Capability cap = probeCapability();

  napi_value result = nullptr;
  NAPI_CALL(env, napi_create_object(env, &result));
  NAPI_CALL(env, setStringProp(env, result, "platform", cap.platform));
  NAPI_CALL(env, setStringProp(env, result, "osVersion", cap.osVersion));
  NAPI_CALL(env, setBoolProp(env, result, "perProcessAudio", cap.perProcessAudio));
  NAPI_CALL(env, setStringProp(env, result, "reason", cap.reason));
  return result;
}

// Throw before returning false, mirroring NAPI_CALL. A null exports with NO
// pending exception gives the loader a generic failure that names nothing -- the
// opposite of the report-rather-than-degrade posture the rest of this file takes.
bool exportFunction(napi_env env, napi_value exports, const char* name,
                    napi_callback fn) {
  napi_value value = nullptr;
  if (napi_create_function(env, name, NAPI_AUTO_LENGTH, fn, nullptr, &value) != napi_ok) {
    napi_throw_error(env, nullptr, "concord-audiocap: could not create an export");
    return false;
  }
  if (napi_set_named_property(env, exports, name, value) != napi_ok) {
    napi_throw_error(env, nullptr, "concord-audiocap: could not set an export");
    return false;
  }
  return true;
}

// ---------------------------------------------------------------------------
// The capture seam — start / drain / stop / status.
//
// THE WHOLE FLOW-CONTROL DESIGN IN ONE PARAGRAPH (design section 4d). There is
// exactly ONE bound and exactly ONE drop site. The ring holds 8 whole quanta and
// the consumer is allowed 8 outstanding on the port, deliberately the same number:
// a consumer that stops draining IS a ring that fills, and a ring that fills makes
// the producer drop the NEWEST quantum and increment one saturating counter.
// Credit exhaustion and ring overflow are therefore the same event, counted once,
// with nothing to reconcile between them. Adding a second bound anywhere — a queue
// behind the signal, a free list, a catch-up batch — creates a second drop site
// that nothing counts.
//
// THE SIGNAL CARRIES NO DATA, and that is what keeps the property true. The
// threadsafe function is created with max_queue_size = 1 and is always called
// napi_tsfn_nonblocking, so it is a coalescing availability EDGE: a second signal
// arriving while one is pending is dropped, which is correct because the pending
// one will make the consumer drain everything available. Were the samples to ride
// the TSFN queue instead, that queue would be an unbounded buffer sitting behind
// the credit bound, and the bound would then measure the transport rather than the
// consumer.
//
// THE PRODUCER IS THE CLOCK. Nothing below asks the consumer for more, and a late
// producer skips the missed slots rather than emitting a burst. A batch-to-catch-up
// on the sender is precisely what turns drop-newest into reorder-newest at the far
// end.
//
// WHAT MAKES THE TEARDOWN SAFE, stated as an invariant because it is the one
// ordering bug this seam can have: NOTHING CALLS napi_call_threadsafe_function
// EXCEPT FROM INSIDE THE SINK GATE, and stopCapture() destroys the backend, then
// closes that gate and shows both that nobody is inside and that the producer has
// stopped moving, before it touches the handle.
//
// AND THERE IS EXACTLY ONE TEARDOWN. Start_JS's failure path does not unwind by
// hand — it calls stopCapture() like everything else, so a backend whose start()
// acquired something before failing gets its stop() called, its gate closed and
// its handle released or abandoned by the same five steps a successful capture
// gets. A second unwind that did four of the five was #3197 PoC-2: a producer
// orphaned by a failed start, unreachable by any later stop(), feeding the next
// capture.
//
// EVERY EXIT FROM THE ARMED WINDOW TAKES IT, which is the wider statement of the
// same rule and the one PR #3262 had to add. "The armed window" is everything
// after g_captureArmed goes true, and the ways out of it are not only a backend
// that refused: creating the threadsafe function, installing the cleanup hook and
// building the result object are all N-API calls that can return non-ok, and a
// bare NAPI_CALL on any of them threw past the teardown and left this process
// holding a capture it had no way to end. They go through NAPI_CALL_ARMED
// instead. A process that answers `Poisoned` forever while a tap it never stopped
// is still running is the same defect as PoC-2 with a different first cause.
//
// This block used to read "the producer thread is the only caller of
// napi_call_threadsafe_function, so stopCapture() sets `running` false, JOINS the
// thread, and only THEN releases the function". That was true of a thread this
// file created and is UNSATISFIABLE for a callback-driven backend: a Core Audio
// IOProc or a WASAPI callback belongs to the OS, and there is no handle on which
// a join can wait (design section 4.1, F3). The join did not disappear — it is
// now SyntheticBackend's own way of meeting its post-condition, one mechanism
// among several — but this file no longer relies on it. rt/sink_gate.h holds even
// against a backend that lies about having stopped.
//
// `signal` is written only by the JS thread: before any backend exists, and after
// the gate has read zero. So it is never read and written concurrently and needs
// no atomicity of its own. On the ONE path where the gate does not read zero the
// handle is ABANDONED rather than released and `signal` is deliberately left
// pointing at it, because whatever is still inside the gate is going to use it —
// see design section 4.3, and rt/teardown.h.
// ---------------------------------------------------------------------------

namespace rt = concord::audiocap::rt;
using concord::audiocap::rt::QuantumRing;
using concord::audiocap::rt::RingResult;

// One slot per quantum, header and samples together, so nothing downstream has to
// re-associate the two. Static storage: the ring never allocates, and neither does
// anything on the path a quantum takes from here to the caller's ArrayBuffer.
rt::u8 g_ringStorage[static_cast<std::size_t>(rt::kRingSlots) *
                     static_cast<std::size_t>(rt::kQuantumBytes)];
QuantumRing g_ring(g_ringStorage, sizeof(g_ringStorage), rt::kRingSlots, rt::kQuantumBytes);

struct Capture {
  napi_threadsafe_function signal;
#ifdef CONCORD_AUDIOCAP_SYNTHETIC
  // THE THREAD-DRIVEN HALF, and it exists only where something drives it. The
  // one backend in this repository that spawns a thread is SyntheticBackend,
  // which is compiled only into the CI/test target, so a SHIPPED binary carried
  // a thread handle, a joinable flag, and the two host hooks that use them with
  // no code able to reach any of it. #3196 (WASAPI) and PR 2 (Core Audio taps)
  // are both callback-driven and satisfy their post-condition with a barrier
  // rather than a join, so this is dead in every binary a user runs. A future
  // thread-driven backend un-gates it here and in kHostServices, together, in
  // the PR that introduces the backend.
  uv_thread_t              thread;
  bool                     threadJoinable;
#endif
  std::atomic<bool>        running;
  bool                     cleanupHookInstalled;
  // The backend this capture started, or nullptr. Written on the JS thread only:
  // set after start() succeeds, cleared by teardown step 2 the moment its stop()
  // returns, which is what makes a second teardown a no-op rather than a second
  // stop() on a backend that has already destroyed its artefacts.
  rt::CaptureBackend*      backend;
};

#ifdef CONCORD_AUDIOCAP_SYNTHETIC
Capture g_capture = {nullptr, uv_thread_t(), false, {false}, false, nullptr};
#else
Capture g_capture = {nullptr, {false}, false, nullptr};
#endif

// ONE GATE, ARMED ONCE, NEVER REPLACED.
//
// This was a std::optional re-armed at every start(), on the argument that a
// teardown which failed to drain latches `poisoned` and so makes a second start
// unreachable. #3197 PoC-1 measured that argument false. quiesced() answers
// "was anybody inside at the instant I sampled", not "does a producer exist": a
// backend that breaks its stop() post-condition spends microseconds per 10 ms
// inside the gate, the poll sampled zero on its first spin, teardown took the
// clean arm, `poisoned` never latched — and the next start() handed a still-live
// producer a fresh OPEN gate. Session-1 audio arrived at session 2's consumer.
//
// THE REPAIR IS NOT A BETTER SAMPLE. No evidence available at this seam can prove
// "no producer exists": the gate count proves nobody was inside at an instant, and
// rt/teardown.h's activity witness proves nobody produced during a window. Both
// are refusal evidence, and an admission rule built on refusal evidence is the
// same defect with a longer timer — PoC-1's producer needs one added sleep to sit
// out any finite window. So close() stays permanent, which is what it always said
// it was, and the gate that was closed is never replaced.
//
// NOTHING IS LOST BY THAT. The host forks one child per share and never respawns
// within one (design section 5, Q3), so "capture twice in one process" is not a
// capability production has. A second start() is refused EXPLICITLY, with
// `Poisoned` — see Start_JS. Refusing silently, or proceeding, was the original
// defect the re-arm was added to fix.
rt::SinkGate g_gate;

// JS-thread-only. True once this process armed its one capture — whether that
// capture then ran for an hour or failed to start at all. A failed start counts:
// the backend may have acquired something before it said no, which is exactly the
// producer PoC-2 orphaned.
bool g_captureArmed = false;

// JS-thread-only, and LATCHED: holds the answer to "did a teardown fail to
// quiesce", which is the only thing that can poison this process.
rt::Teardown g_teardown;

// Discard whatever is still queued. Called from start() and from stopCapture(),
// both of which run with no producer alive -- start() before any backend exists,
// and stopCapture() after that backend's stop() has RETURNED, which is the
// post-condition rt/capture_backend.h defines -- and that is what makes this safe
// on a ring that has no other synchronisation.
//
// On the one path where a backend breaks that post-condition, a late push can
// still land in the ring after this has run. It is unreachable rather than
// merely unwanted: `running` is already false, so Drain_JS refuses, the gate is
// shut, so nothing announces it, and the process is poisoned, so it can only be
// killed. That is the same "discarded rather than delivered" outcome, reached by
// three independent gates instead of by a join.
//
// AT STOP it is the thing that makes post-stop audio unreachable rather than
// merely un-asked-for: a share that ended must not be able to deliver another
// quantum, and leaving 80 ms of captured audio sitting in a ring for the next
// session to find is the permissive version of that. AT START it is the second
// half of the same property.
//
// The overrun counter is deliberately NOT cleared: it is cumulative for the life
// of the process, and the host forks a fresh child per share and never respawns
// within one (design section 5, Q3), so process lifetime is share lifetime.
void drainRing() {
  rt::u8 scratch[rt::kQuantumBytes];
  rt::u32 got = 0u;
  while (g_ring.pop(scratch, rt::kQuantumBytes, &got) == RingResult::kOk) {
    // Nothing to do with it. AV 115 is satisfied by the loop condition itself.
  }
}

// The TSFN's JS-side trampoline. `data` is ALWAYS nullptr — see the header above.
void callQuantumSignal(napi_env env, napi_value jsCallback, void* /*context*/,
                       void* /*data*/) {
  // A null env means the environment is tearing down and no JS may run. Node
  // still invokes this so a payload could be freed; there is no payload here.
  if (env == nullptr || jsCallback == nullptr) { return; }
  napi_value undefinedValue = nullptr;
  if (napi_get_undefined(env, &undefinedValue) != napi_ok) { return; }
  // The result is discarded on purpose: the consumer's own errors are its
  // business, and node surfaces a throwing callback as an uncaught exception.
  (void)napi_call_function(env, undefinedValue, jsCallback, 0, nullptr, nullptr);
}

// THE PRODUCER'S ONE CALL INTO JS, and the only thing that ever runs inside the
// gate. Injected into the pump as a plain `void (*)(void*)` so rt/ never names
// N-API — the composition, not the pump, is what makes a post-teardown quantum
// unable to reach a handle that may already be released.
//
// A refused enter is NOT an error and is deliberately silent here: the quantum is
// already in the ring, teardown empties the ring at step 5, and the far end sees
// the seq gap. Counting it belongs to the gate, which the contract suite asserts.
void signalQuantumAvailable(void* /*arg*/) noexcept {
  // An unarmed gate is OPEN and idle, and `signal` is null until start() creates
  // it, so a producer that somehow ran before any capture finds nothing to call.
  if (!g_gate.tryEnter()) { return; }
  if (g_capture.signal != nullptr) {
    // napi_queue_full is an EXPECTED status here, not a failure: it means a
    // signal is already pending and this one coalesces into it.
    (void)napi_call_threadsafe_function(g_capture.signal, nullptr,
                                        napi_tsfn_nonblocking);
  }
  g_gate.leave();
}

// The regrouper every backend submits into. One per process, reset at each
// start(): its counters are cumulative for the life of the process on purpose,
// because the host forks one child per share (design section 5, Q3).
rt::QuantumPump g_pump(g_ring, &signalQuantumAvailable, nullptr);

// ---------------------------------------------------------------------------
// The host services rt/ refuses to name for itself (AV 22/25).
//
// There is exactly ONE capture at a time, so the thread object is g_capture's and
// `join` looks it up rather than being handed a handle a backend would have to
// find somewhere to store — see the note on HostServices in rt/capture_backend.h.
// ---------------------------------------------------------------------------

#ifdef CONCORD_AUDIOCAP_SYNTHETIC
bool hostSpawn(void (*entry)(void*), void* arg) noexcept {
  if (entry == nullptr) { return false; }
  if (g_capture.threadJoinable) { return false; }   // one at a time, fail closed
  if (uv_thread_create(&g_capture.thread, entry, arg) != 0) { return false; }
  g_capture.threadJoinable = true;
  return true;
}

// The parameter is IGNORED BY CONTRACT, not by oversight: there is exactly one
// capture per process, so the thread object is g_capture's and this looks it up
// rather than trusting a handle a backend would have had to find somewhere to
// store (rt/capture_backend.h, HostServices). It is still declared, because a
// backend passes the same `arg` it gave spawn and the seam is what #3196 and
// PR 2 inherit.
void hostJoin(void* /*handle*/) noexcept {
  if (!g_capture.threadJoinable) { return; }        // idempotent
  (void)uv_thread_join(&g_capture.thread);
  g_capture.threadJoinable = false;
}
#endif  // CONCORD_AUDIOCAP_SYNTHETIC

std::uint64_t hostNowNs() noexcept { return uv_hrtime(); }

// The bounded-wait hook a callback-driven backend uses INSTEAD of a join.
// uv_sleep rather than std::this_thread::sleep_for: libuv is already linked
// through N-API, and this keeps <thread> out of a translation unit compiled
// with -fno-exceptions.
void hostSleepMs(rt::u32 ms) noexcept { uv_sleep(static_cast<unsigned int>(ms)); }

#ifdef CONCORD_AUDIOCAP_SYNTHETIC
const rt::HostServices kHostServices = {&hostSpawn, &hostJoin, &hostNowNs, &hostSleepMs};
#else
// NO SPAWN AND NO JOIN IN A SHIPPED BINARY. Null rather than absent, because the
// struct is the contract and it does not change shape between builds; a backend
// that wants a thread reads the null and returns kDeviceError, which is the
// fail-closed answer and the one SyntheticBackend already gives for it. There is
// no such backend on this path today -- platformBackend() is nullptr in PR 1 --
// and #3196 / PR 2 are both callback-driven.
//
// sleepMs IS SUPPLIED IN BOTH BUILDS, unlike spawn/join: it is what a
// callback-driven backend uses to prove its own quiescence, so a shipped binary
// is exactly where it is needed.
const rt::HostServices kHostServices = {nullptr, nullptr, &hostNowNs, &hostSleepMs};
#endif

#ifdef CONCORD_AUDIOCAP_SYNTHETIC

// THE SYNTHETIC BACKEND. Compiled only into the CI/test target; a release binary
// has no backend at all and its start() returns NoBackend.
//
// It is expressed against rt/capture_backend.h rather than as a bare thread
// function so that the ONE backend PR 1 can run is the same shape #3196 and PR 2
// implement — a contract with a single implementation that is not held to it is a
// contract nothing has tested. It is the thread-driven shape: it satisfies the
// post-condition by joining, which is a mechanism and not the contract.
//
// It IGNORES the target (design section 3.2): it is a generator, not a tap, so
// there is no process to point it at. A real backend handed pidCount == 0 returns
// kNoTarget, which is what keeps the feature dark until #3198 supplies a list.
class SyntheticBackend final : public rt::CaptureBackend {
 public:
  SyntheticBackend() noexcept
      : sink_(nullptr), host_(nullptr), running_(false), stopFlag_(false),
        samples_() {}

  rt::BackendStart start(const rt::CaptureTarget& /*target*/, rt::QuantumPump& sink,
                         const rt::HostServices& host) noexcept override {
    if (running_) { return rt::BackendStart::kAlreadyRunning; }
    if (host.spawn == nullptr || host.join == nullptr || host.nowNs == nullptr) {
      return rt::BackendStart::kDeviceError;
    }
    sink_ = &sink;
    host_ = &host;
    stopFlag_.store(false, std::memory_order_release);
    // A thread that did not start is kDeviceError, per rt/capture_backend.h.
    if (!host.spawn(&SyntheticBackend::entry, this)) {
      return rt::BackendStart::kDeviceError;
    }
    running_ = true;
    return rt::BackendStart::kOk;
  }

  // THE POST-CONDITION, by joining: when this returns, run() has exited and
  // nothing else in this process will touch the sink. Idempotent, because the
  // env-cleanup hook calls it after Stop_JS already has.
  void stop() noexcept override {
    if (!running_) { return; }
    stopFlag_.store(true, std::memory_order_release);
    host_->join(this);
    running_ = false;
  }

 private:
  static void entry(void* arg) { static_cast<SyntheticBackend*>(arg)->run(); }

  void run() {
    const std::uint64_t periodNs =
        static_cast<std::uint64_t>(rt::kQuantumMs) * 1000000ull;
    std::uint64_t deadline = host_->nowNs();
    rt::u32 seq = 0u;

    while (!stopFlag_.load(std::memory_order_acquire)) {
      // ONE WHOLE QUANTUM PER TICK, so the pump's seq and this source's seq
      // advance together and the far end's exact-content assertion still reads
      // the samples of the seq its header carries. The pump owns the header, the
      // ring push and the signal now; a backend produces frames and nothing else.
      if (rt::SyntheticSource::fillSamples(seq, samples_, sizeof(samples_))) {
        const rt::u8* planes[1] = {samples_};
        sink_->noteCallback();
        // DISCARDED, and this is the one place in the file where that is not a
        // dropped error: submit() returns false only for a null plane array, a
        // channel count outside 1..2, or a zero frame count, and all three are
        // compile-time constants two lines up. There is no runtime condition it
        // can report and nothing this loop could do differently if there were.
        (void)sink_->submit(planes, rt::kChannels, true, rt::kFrameCount,
                            host_->nowNs());
      }
      // Advanced even when the ring was full: that gap is the second, independent
      // drop observer, and it still moves if the counter ever lies.
      ++seq;

      deadline += periodNs;
      const std::uint64_t now = host_->nowNs();
      if (now >= deadline) {
        deadline = now;  // late: skip the missed slots, never burst to catch up
      } else {
        // Rounded UP, so a sub-millisecond remainder sleeps 1 ms instead of
        // spinning. The deadline accumulator absorbs the overshoot, so the
        // cadence stays right on average.
        const std::uint64_t remaining = deadline - now;
        uv_sleep(static_cast<unsigned int>((remaining + 999999ull) / 1000000ull));
      }
    }
  }

  rt::QuantumPump*        sink_;
  const rt::HostServices* host_;
  bool                    running_;
  std::atomic<bool>       stopFlag_;
  rt::u8                  samples_[rt::kSampleBytes];
};

SyntheticBackend g_syntheticBackend;

#endif  // CONCORD_AUDIOCAP_SYNTHETIC

// THE ONE PLACE A BACKEND IS CHOSEN, and there is exactly one candidate in each
// binary: the synthetic target has the generator above, and every other build has
// whatever rt/platformBackend() compiles in — which in PR 1 is nullptr on every
// platform, and is what keeps a release build's start() at NoBackend.
rt::CaptureBackend* selectBackend() noexcept {
#ifdef CONCORD_AUDIOCAP_SYNTHETIC
  return &g_syntheticBackend;
#else
  return rt::platformBackend();
#endif
}

// ---------------------------------------------------------------------------
// Teardown — design section 4.2's five steps. The ORDER lives in rt/teardown.h,
// where the contract suite can assert it; these four hooks are the parts of it
// that must name N-API and uv.
// ---------------------------------------------------------------------------

/// THE BACKEND'S EVIDENCE, SNAPSHOTTED BEFORE THE POINTER GOES AWAY.
///
/// teardownStopBackend nulls g_capture.backend once it has stopped it, so a
/// status() call after a share -- which is the ONLY time anyone asks these
/// questions -- would otherwise read the base-class defaults and report a clean
/// teardown no matter what happened. Copying three scalars here is also what
/// keeps status()'s documented promise literally true: it touches neither the
/// gate, the backend, nor the handle.
struct BackendEvidence {
  bool quiesceProved;
  rt::u32 destroyFailures;
  rt::u32 lastDeviceStatus;
};
BackendEvidence g_backendEvidence = {false, 0u, 0u};

// STEP 2. The OS artefact dies here, BEFORE the gate wait, because the invariant
// is about the tap existing and not about bytes moving.
void teardownStopBackend(void* /*ctx*/) noexcept {
  if (g_capture.backend == nullptr) { return; }
  g_capture.backend->stop();
  g_backendEvidence.quiesceProved    = g_capture.backend->quiesceProved();
  g_backendEvidence.destroyFailures  = g_capture.backend->destroyFailures();
  g_backendEvidence.lastDeviceStatus = g_capture.backend->lastDeviceStatus();
  g_capture.backend = nullptr;
}

void teardownSleepMs(void* /*ctx*/, rt::u32 ms) noexcept {
  uv_sleep(static_cast<unsigned int>(ms));
}

// STEP 3's UNIT. uv_hrtime() is the monotonic clock the capture deadline already
// runs on (hostNowNs above), and the settle window is measured with it for the
// reason it is not measured in loop iterations: uv_sleep(1) is one millisecond
// only where the host's timer says so, and on Windows -- whose backend inherits
// this contract at #3196 -- it is up to ~15.6 ms.
rt::u64 teardownNowNs(void* /*ctx*/) noexcept { return uv_hrtime(); }

// STEP 4, QUIESCED ARM ONLY. Never called on the timeout arm: releasing a handle
// a live callback may still call is the use-after-release the gate exists to
// prevent, so that arm abandons it and leaves `signal` pointing at it.
void teardownReleaseSignal(void* /*ctx*/) noexcept {
  if (g_capture.signal == nullptr) { return; }
  (void)napi_release_threadsafe_function(g_capture.signal, napi_tsfn_release);
  g_capture.signal = nullptr;
}

void teardownDrain(void* /*ctx*/) noexcept { drainRing(); }

// THE LIVENESS WITNESS, and it is an INDEPENDENT one: this number is moved by the
// PUMP on every entry from the producer, so a backend that returned from stop()
// and kept producing moves it and a backend that really stopped cannot. The gate
// count cannot answer that question — it reads zero between callouts of a producer
// that is very much alive, which is #3197 PoC-1.
//
// IT IS activityTotal AND NOT callbackTotal, which is the counter this hook read
// until PR #3262. callbackTotal moves only when a backend VOLUNTEERS
// noteCallback(), and rt/capture_backend.h makes that optional — so a conforming
// backend that only ever calls submit() left this witness motionless while its tap
// ran, and the teardown took the quiesced arm over it. callbackTotal keeps its own
// meaning as the starvation signal; it is simply not evidence of absence.
//
// It saturates rather than wrapping (rt/quantum_pump.h), and a saturated witness is
// REFUSED rather than read as stable — see rt::kWitnessSaturated, and note that
// 2^32 entries is a year of honest 10 ms quanta but under ten seconds of a loop
// the contract permits.
rt::u32 teardownProducerActivity(void* /*ctx*/) noexcept {
  return g_pump.activityTotal();
}

// THE CEILING IS A THREE-LINK CHAIN, AND ONLY THIS LINE HOLDS IT TOGETHER.
// rt/teardown.h's guard refuses a witness equal to rt::kWitnessSaturated, a plain
// literal; the value it actually receives is QuantumPump::kCounterSaturated, which
// is itself an alias of QuantumRing::kOverrunSaturated in a third header. Nothing
// in rt/ can connect them — teardown.h deliberately does not include quantum_pump.h,
// because it reaches the producer through a function pointer precisely so the two
// stay decoupled. This translation unit is the only place all three are visible, so
// it is the only place the equality can be checked.
//
// The failure mode if it ever drifts is silent and points the wrong way: the guard
// stops matching, a saturated witness reads as STABLE, and the teardown takes the
// quiesced arm over a producer that never stopped. That is the exact fail-open the
// guard was added to close, re-entered through a constant nobody thought was load
// bearing. The comment above already asserted this relationship in prose; prose does
// not fail a build. Raised by CodeRabbit on PR #3262.
static_assert(rt::QuantumPump::kCounterSaturated == rt::kWitnessSaturated,
              "the teardown witness ceiling must equal the pump counter ceiling "
              "(itself QuantumRing::kOverrunSaturated), or a saturated witness "
              "reads as stable and the teardown releases over a live producer");

const rt::TeardownOps kTeardownOps = {&teardownStopBackend, &teardownSleepMs,
                                      &teardownReleaseSignal, &teardownDrain,
                                      &teardownProducerActivity, &teardownNowNs,
                                      nullptr};

// Idempotent, and safe to call from the environment cleanup hook. See the
// ordering invariant in the header comment above, and rt/teardown.h for the
// sequence itself.
void stopCapture() {
  if (!g_captureArmed) {
    // Nothing was ever armed in this process: no backend, no handle, and a gate
    // nobody ever entered. The ring is still emptied, which is the post-condition
    // the full sequence ends with and costs nothing on a ring that was never
    // filled. This path leaves the process able to capture — an unarmed gate is
    // not a spent one.
    g_capture.running.store(false, std::memory_order_release);
    drainRing();
    return;
  }
  // The outcome is discarded HERE and read from the latch instead: `poisoned` is
  // what start() consults, and it survives every later teardown.
  (void)g_teardown.run(g_capture.running, g_gate, kTeardownOps,
                       rt::kQuiesceBudgetMs);
}

// A capture left running at environment teardown would have a live thread calling
// into a dying env. The hook is what makes that unreachable even if the JS side
// never calls stop().
void captureEnvCleanup(void* /*arg*/) {
  g_capture.cleanupHookInstalled = false;  // node removes the hook as it runs it
  stopCapture();
}

// THE UNWIND FOR A CAPTURE THAT WAS ARMED AND THEN COULD NOT BE FINISHED.
//
// Start_JS sets g_captureArmed and then makes four more N-API calls -- a string,
// the threadsafe function, the cleanup hook, and the result object -- any of
// which can return non-ok. Until PR #3262 every one of them was a bare NAPI_CALL,
// which throws and returns nullptr: no stop(), no gate close, no unwind at all.
// The process was left with `armed` true, `running` true on the one path that had
// already set it, a backend that may have installed an IOProc, and an answer of
// `Poisoned` to every later start() -- a live tap that nothing in the process
// could reach, which is #3197 PoC-2 arriving through the N-API failure path
// instead of through the backend's own.
//
// So those calls unwind the way a failed start() does: through stopCapture(), the
// single five-step teardown. The hook comes off FIRST for the reason Stop_JS
// takes it off first -- this call owns the teardown, and a hook left installed
// would run a second one against an env that is already unwinding.
//
// UNWIND BEFORE THROWING, not after: napi_throw_error leaves a pending exception
// and an N-API call made under one can fail for that reason alone, so the
// destructive work is finished while the env is still clean.
void unwindArmedCapture(napi_env env) {
  if (g_capture.cleanupHookInstalled) {
    (void)napi_remove_env_cleanup_hook(env, captureEnvCleanup, nullptr);
    g_capture.cleanupHookInstalled = false;
  }
  stopCapture();
}

// NAPI_CALL for the window in which this process holds a capture. Same throw and
// same return, with the teardown in front of it.
#define NAPI_CALL_ARMED(env, call)                                             \
  do {                                                                         \
    if ((call) != napi_ok) {                                                   \
      unwindArmedCapture(env);                                                 \
      napi_throw_error((env), nullptr, "concord-audiocap: N-API call failed"); \
      return nullptr;                                                          \
    }                                                                          \
  } while (0)

// `{ ok: true }` or `{ ok: false, reason }`. `reason` is a fixed string from this
// file, never anything derived from an argument.
napi_status makeStartResult(napi_env env, bool ok, const char* reason, napi_value* out) {
  napi_status s = napi_create_object(env, out);
  if (s != napi_ok) { return s; }
  s = setBoolProp(env, *out, "ok", ok);
  if (s != napi_ok || ok) { return s; }
  return setStringProp(env, *out, "reason", reason);
}

// Exact equality against the compiled constants, one field at a time.
//
// NOT a negotiation. The child hands over what it read from `start`, and a
// mismatch means the two ends of the transport disagree about the wire — which is
// a fault to report, never a geometry to adopt. Read as a double and compared
// exactly so a non-integer (10.5) is refused rather than truncated into a match.
napi_status optionsMatch(napi_env env, napi_value options, bool* matched) {
  *matched = false;

  napi_valuetype t = napi_undefined;
  napi_status s = napi_typeof(env, options, &t);
  if (s != napi_ok) { return s; }
  if (t != napi_object) { return napi_ok; }

  struct Expected { const char* key; std::uint32_t value; };
  const Expected expected[] = {
      {"quantumMs",  rt::kQuantumMs},
      {"sampleRate", rt::kSampleRate},
      {"channels",   static_cast<std::uint32_t>(rt::kChannels)},
      {"frameCount", static_cast<std::uint32_t>(rt::kFrameCount)},
      {"ringSlots",  rt::kRingSlots},
  };

  for (std::size_t i = 0; i < sizeof(expected) / sizeof(expected[0]); ++i) {
    napi_value v = nullptr;
    s = napi_get_named_property(env, options, expected[i].key, &v);
    if (s != napi_ok) { return s; }
    napi_valuetype vt = napi_undefined;
    s = napi_typeof(env, v, &vt);
    if (s != napi_ok) { return s; }
    if (vt != napi_number) { return napi_ok; }
    double actual = 0.0;
    s = napi_get_value_double(env, v, &actual);
    if (s != napi_ok) { return s; }
    if (actual != static_cast<double>(expected[i].value)) { return napi_ok; }
  }

  *matched = true;
  return napi_ok;
}

// BackendStart -> the closed AudioCapStartFailure vocabulary in index.d.ts. A
// fixed string from this file for every arm, never anything derived from an
// argument.
//
// kUnsupportedOs collapses to "NoBackend" on purpose: from the caller's side an
// OS below the capture floor and a binary with no backend compiled in are the
// same outcome — video-only with a reason — and the capability() probe is where
// the difference is already reported.
const char* startFailureReason(rt::BackendStart started) noexcept {
  switch (started) {
    case rt::BackendStart::kNoTarget:         return "NoTarget";
    case rt::BackendStart::kUnsupportedOs:    return "NoBackend";
    case rt::BackendStart::kUnsupportedFormat: return "UnsupportedFormat";
    case rt::BackendStart::kPermissionDenied: return "PermissionDenied";
    case rt::BackendStart::kDeviceError:      return "DeviceError";
    case rt::BackendStart::kAlreadyRunning:   return "AlreadyStarted";
    case rt::BackendStart::kOk:               break;
  }
  // Unreachable: kOk never reaches this function, and the enum is closed. It
  // fails to the most conservative member rather than to ok-shaped silence.
  return "DeviceError";
}

// PumpFault -> the closed AudioCapStatus.faultReason vocabulary in index.d.ts.
// A fixed string from this file for every arm.
const char* faultReasonName(rt::u8 reason) noexcept {
  switch (static_cast<rt::PumpFault>(reason)) {
    case rt::PumpFault::kNone:          return "None";
    case rt::PumpFault::kDeviceLost:    return "DeviceLost";
    case rt::PumpFault::kPermissionLost: return "PermissionLost";
    case rt::PumpFault::kNoCallbacks:   return "NoCallbacks";
    case rt::PumpFault::kFormatChanged: return "FormatChanged";
  }
  // Unreachable: the only writer is QuantumPump::fault(), which takes the enum.
  // `faulted` is the load-bearing field of the pair and is read separately, so a
  // reason that somehow fell outside the set reports as unexplained rather than
  // as a cause this file invented.
  return "None";
}

// A JS number -> a PID, with NO truncation anywhere on the path.
//
// The exact-integer test is the point. napi_get_value_uint32 and
// napi_get_value_int64 both TRUNCATE, so `4.5` would arrive as 4 and a capture
// would be aimed at a process the caller never named -- a target selector
// quietly reinterpreted, which is the same defect class as ignoring it. Read as
// a double and compared exactly, mirroring optionsMatch above.
bool pidFromNumber(double value, rt::u32* out) noexcept {
  // Written as POSITIVE tests: NaN compares false against everything, so it is
  // refused here rather than slipping through a negated range check.
  if (!(value >= 1.0)) { return false; }              // 0, -0, negatives, NaN
  if (!(value <= 4294967295.0)) { return false; }     // > u32, +inf
  const rt::u32 truncated = static_cast<rt::u32>(value);
  if (static_cast<double>(truncated) != value) { return false; }  // 4.5, 1e-3
  *out = truncated;
  return true;
}

// The OPTIONAL `targetPids` / `allowDescendants` half of the start options.
//
// WHY THIS IS PARSED IN PR 1, WHEN ONLY #3198 WILL SUPPLY IT. An option declared
// in index.d.ts that nothing reads is a silent-ignore trap: #3198 would hand over
// a PID list, this file would drop it, no error would be raised anywhere, and the
// capture would proceed against whatever the backend's own default is. That is a
// fail-open on a target selector -- #2161's defect class. Parsing it now means
// the option is honoured or reported, and never neither.
//
// PRECONDITION: `options` has already been proved to be an object by
// optionsMatch(). `*accepted == false` with napi_ok means BadOptions; a non-ok
// status means the N-API call itself failed and the caller throws.
//
// The POLICY -- which lists are admissible -- is rt::buildTarget, where a test
// binary can reach it (rt/capture_backend.h). What lives here is only what a JS
// VALUE must be to become a u32.
napi_status parseTarget(napi_env env, napi_value options, rt::CaptureTarget* target,
                        bool* accepted) {
  *accepted = false;
  *target = rt::CaptureTarget{};

  napi_value pidsValue = nullptr;
  napi_status s = napi_get_named_property(env, options, "targetPids", &pidsValue);
  if (s != napi_ok) { return s; }
  napi_valuetype pidsType = napi_undefined;
  s = napi_typeof(env, pidsValue, &pidsType);
  if (s != napi_ok) { return s; }

  napi_value descendantsValue = nullptr;
  s = napi_get_named_property(env, options, "allowDescendants", &descendantsValue);
  if (s != napi_ok) { return s; }
  napi_valuetype descendantsType = napi_undefined;
  s = napi_typeof(env, descendantsValue, &descendantsType);
  if (s != napi_ok) { return s; }

  bool allowDescendants = false;
  if (descendantsType == napi_boolean) {
    s = napi_get_value_bool(env, descendantsValue, &allowDescendants);
    if (s != napi_ok) { return s; }
  } else if (descendantsType != napi_undefined) {
    // NOT COERCED. A truthy string or a 1 is a caller that does not know what it
    // is asking for, and widening a capture to a process tree on a coercion is
    // the permissive reading of an ambiguous request.
    return napi_ok;
  }

  if (pidsType == napi_undefined) {
    // ABSENT, which is the PR-1 case and is NOT an error: pidCount stays 0, and a
    // real backend answers kNoTarget -> "NoTarget". That IS the feature being
    // dark until #3198 supplies a list.
    //
    // `allowDescendants` WITHOUT a list is refused rather than ignored, for the
    // reason this whole function exists: there is no list for it to expand, so
    // honouring it is impossible and accepting it silently would be the one
    // outcome that lies about what the addon did with the caller's request.
    if (descendantsType != napi_undefined) { return napi_ok; }
    *accepted = true;
    return napi_ok;
  }

  bool isArray = false;
  s = napi_is_array(env, pidsValue, &isArray);
  if (s != napi_ok) { return s; }
  if (!isArray) { return napi_ok; }

  std::uint32_t length = 0;
  s = napi_get_array_length(env, pidsValue, &length);
  if (s != napi_ok) { return s; }
  // The LOCAL BUFFER BOUND, checked before anything is written into it. It is
  // deliberately the same limit rt::buildTarget enforces as policy: this one
  // keeps the array below from overflowing, that one keeps an over-long list from
  // being truncated into a target the caller did not ask for. Both fail closed.
  if (length > static_cast<std::uint32_t>(rt::kMaxTargetPids)) { return napi_ok; }

  rt::u32 pids[rt::kMaxTargetPids] = {};
  for (std::uint32_t i = 0; i < length; ++i) {
    napi_value element = nullptr;
    s = napi_get_element(env, pidsValue, i, &element);
    if (s != napi_ok) { return s; }
    napi_valuetype elementType = napi_undefined;
    s = napi_typeof(env, element, &elementType);
    if (s != napi_ok) { return s; }
    if (elementType != napi_number) { return napi_ok; }
    double raw = 0.0;
    s = napi_get_value_double(env, element, &raw);
    if (s != napi_ok) { return s; }
    if (!pidFromNumber(raw, &pids[i])) { return napi_ok; }
  }

  // A length of 0 reaches here and buildTarget refuses it: an EMPTY list is a
  // request that cannot be satisfied, which is not the same as omitting the
  // option (handled above).
  *accepted = rt::buildTarget(pids, length, allowDescendants, target);
  return napi_ok;
}

// A PULL, and it carries no data path (design section 3.2). It does not widen
// drain(), adds no second threadsafe function, and cannot smuggle audio: every
// field below is a counter or a flag.
//
// SAFE AFTER stop(), which is the property that makes it a post-mortem rather
// than a monitor. It reads only atomics in static storage -- it never touches the
// gate, the backend pointer or the threadsafe function, all three of which
// teardown may have torn down or abandoned. ALLOCATION-FREE ON THE PRODUCER
// SIDE: the six reads are plain atomic loads, and the only allocation is the
// result object, which is created on the JS thread after every value has already
// been read.
napi_value Status_JS(napi_env env, napi_callback_info /*info*/) {
  const bool             running  = g_capture.running.load(std::memory_order_acquire);
  const rt::PumpCounters counters = g_pump.counters();
  const rt::u32          overrun  = g_ring.overrun();
  // THE ABANDON OUTCOME, MADE OBSERVABLE. stopCapture() discards Teardown::run's
  // return value and reads the latch instead, and until PR #3262 the latch was
  // readable only from inside this file -- so the one state the design says the
  // host must respond to by KILLING THE CHILD (design section 4.3) was invisible
  // to the host. #3198 owns the watchdog that consumes it; the signal exists now
  // so that watchdog is wiring rather than a new native surface.
  //
  // It is a plain bool read on the JS thread, which is the only thread that ever
  // writes it, so this stays the allocation-free post-mortem the doc above
  // describes: it touches neither the gate, the backend, nor the handle.
  const bool             poisoned = g_teardown.poisoned();

  napi_value result = nullptr;
  NAPI_CALL(env, napi_create_object(env, &result));
  NAPI_CALL(env, setBoolProp(env, result, "running", running));
  NAPI_CALL(env, setUint32Prop(env, result, "callbackTotal", counters.callbackTotal));
  NAPI_CALL(env, setUint32Prop(env, result, "quantaTotal", counters.quantaTotal));
  // THE PAIR R9 FORCED. callbackTotal alone cannot see consent denial: measured,
  // a denied Core Audio tap delivers ~94 correctly-shaped callbacks a second
  // with every API returning noErr and every sample zero. signalTotal answers
  // the different question, and silentSinceStart is the native evaluation of
  // "callbacks arriving, nothing audible, budget elapsed".
  //
  // ADVISORY. It is not a fault, it does not stop a capture, and it cannot
  // distinguish denial from a user sharing a paused app -- nothing at this seam
  // can. #3198 owns whatever consumes it.
  NAPI_CALL(env, setUint32Prop(env, result, "signalTotal", counters.signalTotal));
  NAPI_CALL(env, setBoolProp(env, result, "silentSinceStart", counters.silentSinceStart));
  NAPI_CALL(env, setUint32Prop(env, result, "overrunTotal", overrun));
  NAPI_CALL(env, setBoolProp(env, result, "faulted", counters.faulted));
  NAPI_CALL(env, setStringProp(env, result, "faultReason",
                               faultReasonName(counters.faultReason)));
  NAPI_CALL(env, setBoolProp(env, result, "poisoned", poisoned));

  // THE BACKEND'S OWN EVIDENCE ABOUT ITS TEARDOWN. Read from the backend rather
  // than the pump because these are facts about OS artefacts, which the pump
  // never sees. Defaults on the base class mean a backend that declines to
  // answer reads as "proved nothing, reported no failures" -- and a NULL
  // backend, which is every platform without one compiled in, reads the same.
  //
  // destroyTapFailures is the one that matters: the destroy status used to be
  // discarded and the handle cleared on the next line, so a live OS tap on
  // another application's audio could survive a share with nothing in the
  // process able to observe it. #3198's watchdog reads this alongside poisoned.
  // Read live while a capture is running, from the snapshot afterwards. The
  // snapshot is the load-bearing half: teardownStopBackend nulls the pointer,
  // and after a share is exactly when these are asked.
  const rt::CaptureBackend* backend = g_capture.backend;
  const bool    proved  = backend != nullptr ? backend->quiesceProved()
                                             : g_backendEvidence.quiesceProved;
  const rt::u32 failed  = backend != nullptr ? backend->destroyFailures()
                                             : g_backendEvidence.destroyFailures;
  const rt::u32 devStat = backend != nullptr ? backend->lastDeviceStatus()
                                             : g_backendEvidence.lastDeviceStatus;
  NAPI_CALL(env, setBoolProp(env, result, "quiesceProved", proved));
  NAPI_CALL(env, setUint32Prop(env, result, "destroyFailures", failed));
  NAPI_CALL(env, setUint32Prop(env, result, "lastDeviceStatus", devStat));
  return result;
}

napi_value Start_JS(napi_env env, napi_callback_info info) {
  size_t      argc    = 2;
  napi_value  args[2] = {nullptr, nullptr};
  NAPI_CALL(env, napi_get_cb_info(env, info, &argc, args, nullptr, nullptr));

  napi_value result = nullptr;

  if (argc < 2) {
    NAPI_CALL(env, makeStartResult(env, false, "BadArguments", &result));
    return result;
  }
  napi_valuetype callbackType = napi_undefined;
  NAPI_CALL(env, napi_typeof(env, args[1], &callbackType));
  if (callbackType != napi_function) {
    NAPI_CALL(env, makeStartResult(env, false, "BadArguments", &result));
    return result;
  }

  bool matched = false;
  NAPI_CALL(env, optionsMatch(env, args[0], &matched));
  if (!matched) {
    NAPI_CALL(env, makeStartResult(env, false, "BadOptions", &result));
    return result;
  }

  // Parsed with the geometry, BEFORE any state is touched: a malformed target
  // must leave the process exactly as it found it, with no ring drained, no gate
  // re-armed and no threadsafe function created.
  rt::CaptureTarget target        = {};
  bool              targetAccepted = false;
  NAPI_CALL(env, parseTarget(env, args[0], &target, &targetAccepted));
  if (!targetAccepted) {
    NAPI_CALL(env, makeStartResult(env, false, "BadOptions", &result));
    return result;
  }

  if (g_capture.running.load(std::memory_order_acquire)) {
    NAPI_CALL(env, makeStartResult(env, false, "AlreadyStarted", &result));
    return result;
  }

  // A PROCESS CAPTURES ONCE. Two conditions, one answer, because they are one
  // fact: this process's single gate is closed and will not be replaced.
  //
  //   `poisoned`      — a teardown could not show the sink empty and the producer
  //                     stopped, so it abandoned a threadsafe function that
  //                     something outside this file may still call.
  //   `captureArmed`  — a capture was armed here, cleanly or not. The gate it was
  //                     armed with is closed for the life of the process; see the
  //                     note on g_gate for why nothing may re-arm it.
  //
  // The host kills the child (design section 4.3); this is what makes that the
  // only outcome. It is a REFUSAL and not a silent no-op: a second start() that
  // appeared to succeed and delivered the previous capture's audio is the defect
  // the re-arm was introduced to fix, and it must not come back by omission.
  if (g_teardown.poisoned() || g_captureArmed) {
    NAPI_CALL(env, makeStartResult(env, false, "Poisoned", &result));
    return result;
  }

  // THE RELEASE ANSWER, and it is now a RUNTIME test rather than an #ifdef: PR 1
  // compiles no platform backend on any platform, so rt::platformBackend() is
  // nullptr and a release build still answers NoBackend. #3196 and PR 2 each fill
  // that in, and neither has to touch this arm. The host resolves NoBackend to
  // video-only with a reason — never to a system mix, which is constraint C9
  // and #2161.
  rt::CaptureBackend* const backend = selectBackend();
  if (backend == nullptr) {
    NAPI_CALL(env, makeStartResult(env, false, "NoBackend", &result));
    return result;
  }

  drainRing();
  // The accumulator and seq are cleared; the counters deliberately are not.
  g_pump.reset(uv_hrtime());
  // THE ONE CAPTURE THIS PROCESS GETS, spent here. Set BEFORE the backend can
  // produce anything and never cleared: from this line on, the gate below belongs
  // to this capture and the refusal above is what every later start() gets.
  g_captureArmed = true;

  // EVERY N-API CALL FROM HERE TO THE RETURN IS NAPI_CALL_ARMED. The capture is
  // spent, so a failure that merely threw would leave this process holding one it
  // never tore down -- see unwindArmedCapture.
  napi_value resourceName = nullptr;
  NAPI_CALL_ARMED(env, napi_create_string_utf8(env, "concord-audiocap:quantum",
                                               NAPI_AUTO_LENGTH, &resourceName));
  // max_queue_size 1 and one acquiring thread (the producer). Both numbers are
  // load-bearing; see the section header.
  NAPI_CALL_ARMED(env, napi_create_threadsafe_function(
                           env, args[1], nullptr, resourceName,
                           /*max_queue_size=*/1, /*initial_thread_count=*/1,
                           nullptr, nullptr, nullptr, callQuantumSignal,
                           &g_capture.signal));

  NAPI_CALL_ARMED(env, napi_add_env_cleanup_hook(env, captureEnvCleanup, nullptr));
  g_capture.cleanupHookInstalled = true;

  // Both are set BEFORE the backend can produce anything: a backend may call the
  // sink from any thread the instant start() is entered, and teardown must be
  // able to find it even if start() then fails.
  g_capture.running.store(true, std::memory_order_release);
  g_capture.backend = backend;

  // THE TARGET IS PARSED, NOT SUPPLIED, IN PR 1. #3198 owns picker->PID
  // resolution and is the first caller that will pass `targetPids`; until then
  // every real backend sees CaptureScope::kProcessList with pidCount == 0 and
  // fails closed with NoTarget, which IS the feature being dark.
  //
  // NOTHING HERE CAN ASK FOR A SYSTEM MIX. `scope` is only ever kProcessList on
  // this path -- parseTarget value-initialises the struct and buildTarget sets
  // the same arm -- so ADR-0043 D6's whole-system row is unreachable from JS in
  // this build rather than one omitted option away. When #3198 resolves a monitor
  // target it adds an explicit option here and sets kSystemMix; a window target
  // whose PID lookup failed must still arrive as an empty kProcessList list. The synthetic backend ignores the target entirely
  // -- it is a generator, not a tap, so there is no process to point it at.
  //
  // Copied BY VALUE into the backend (rt/capture_backend.h): a backend that
  // retained a reference would be reading a struct this frame owns.
  const rt::BackendStart started = backend->start(target, g_pump, kHostServices);
  if (started != rt::BackendStart::kOk) {
    // ONE TEARDOWN, NOT TWO. This used to unwind by hand: null the backend
    // pointer, drop `running`, remove the hook, release the handle. Four steps of
    // five, in the wrong order, and the missing one was the only one that destroys
    // anything — rt/capture_backend.h states a post-condition for stop() and states
    // NOTHING about what a FAILED start() may leave behind, so a backend that
    // installed an IOProc and then failed a later step (a routine Core Audio and
    // WASAPI shape) was left running with its only handle discarded. #3197 PoC-2:
    // the orphan outlived every later stop() and fed the next capture.
    //
    // So a failed start ends the same way a share does. stopCapture() calls the
    // backend's stop(), closes the gate, watches the producer, and releases or
    // abandons the handle on the evidence — a backend that acquired something gets
    // its stop() called, and one that acquired nothing pays an idempotent no-op.
    //
    // The cleanup hook is removed FIRST, for the reason Stop_JS removes it first:
    // this call owns the teardown, and a hook left installed would run a second one
    // against an env that is already unwinding.
    if (g_capture.cleanupHookInstalled) {
      (void)napi_remove_env_cleanup_hook(env, captureEnvCleanup, nullptr);
      g_capture.cleanupHookInstalled = false;
    }
    stopCapture();
    // PLAIN NAPI_CALL, and it is the one call in the armed window that is: the
    // teardown has already run on the line above, so there is nothing left for an
    // unwind to do and a second run would only spend another settle window.
    NAPI_CALL(env, makeStartResult(env, false, startFailureReason(started), &result));
    return result;
  }

  // ARMED, because this one runs after a SUCCESSFUL start(): a failure here is a
  // backend that is producing into a process whose caller will be told nothing.
  NAPI_CALL_ARMED(env, makeStartResult(env, true, nullptr, &result));
  return result;
}

// Fills the caller's ArrayBuffer in place and ALLOCATES NOTHING on the payload
// path: pop() memcpys one whole quantum straight into the buffer behind
// napi_get_arraybuffer_info. The only object created is the `{ ok }` the declared
// contract returns.
//
// A wrong-sized or detached buffer is refused rather than partially filled. A
// detached one reports length 0, so the size check covers it without a separate
// branch.
napi_value Drain_JS(napi_env env, napi_callback_info info) {
  size_t     argc    = 1;
  napi_value args[1] = {nullptr};
  NAPI_CALL(env, napi_get_cb_info(env, info, &argc, args, nullptr, nullptr));

  // FAIL CLOSED WHEN NOT CAPTURING. A drain after stop() would deliver audio for a
  // share that has ended -- the permissive outcome, and the one a caller is most
  // likely to reach by accident, since a threadsafe-function callback can still be
  // pending in the microtask queue when stop() runs. stopCapture() also empties the
  // ring, so this gate and that drain close the same hole from both sides.
  bool ok = false;
  if (argc >= 1 && g_capture.running.load(std::memory_order_acquire)) {
    bool isArrayBuffer = false;
    NAPI_CALL(env, napi_is_arraybuffer(env, args[0], &isArrayBuffer));
    if (isArrayBuffer) {
      void*  data = nullptr;
      size_t len  = 0;
      NAPI_CALL(env, napi_get_arraybuffer_info(env, args[0], &data, &len));
      if (data != nullptr && len == static_cast<size_t>(rt::kQuantumBytes)) {
        rt::u8* into = static_cast<rt::u8*>(data);
        rt::u32 got  = 0u;
        const RingResult r = g_ring.pop(into, rt::kQuantumBytes, &got);
        // The length check is defensive rather than reachable — the producer only
        // ever pushes whole quanta — but a short slot reported as a full one would
        // hand the far end stale bytes past the end of the audio.
        ok = (r == RingResult::kOk) && (got == rt::kQuantumBytes);
      }
    }
  }

  napi_value result = nullptr;
  NAPI_CALL(env, napi_create_object(env, &result));
  NAPI_CALL(env, setBoolProp(env, result, "ok", ok));
  return result;
}

napi_value Stop_JS(napi_env env, napi_callback_info /*info*/) {
  if (g_capture.cleanupHookInstalled) {
    (void)napi_remove_env_cleanup_hook(env, captureEnvCleanup, nullptr);
    g_capture.cleanupHookInstalled = false;
  }
  stopCapture();

  napi_value undefinedValue = nullptr;
  NAPI_CALL(env, napi_get_undefined(env, &undefinedValue));
  return undefinedValue;
}

}  // namespace

NAPI_MODULE_INIT() {
  if (!exportFunction(env, exports, "capability", Capability_JS)) { return nullptr; }
  if (!exportFunction(env, exports, "start", Start_JS)) { return nullptr; }
  if (!exportFunction(env, exports, "drain", Drain_JS)) { return nullptr; }
  if (!exportFunction(env, exports, "stop", Stop_JS)) { return nullptr; }
  if (!exportFunction(env, exports, "status", Status_JS)) { return nullptr; }
  return exports;
}
