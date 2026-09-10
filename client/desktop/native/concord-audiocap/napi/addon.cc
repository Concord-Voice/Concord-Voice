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
#include <cstdio>   // std::snprintf, std::sscanf
#include <cstring>

#include "../rt/quantum_header.h"
#include "../rt/quantum_ring.h"
#ifdef CONCORD_AUDIOCAP_SYNTHETIC
#  include "../rt/synthetic_source.h"
#endif

#if defined(_WIN32)
#  define WIN32_LEAN_AND_MEAN
#  define NOMINMAX
#  include <windows.h>
#elif defined(__APPLE__)
#  include <sys/sysctl.h>
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

#if defined(__APPLE__)
// CoreAudio process taps: AudioHardwareCreateProcessTap + CATapDescription arrived
// in macOS 14.2 and matured in 14.4. ADR-0043 D6 takes 14.4 as the floor.
constexpr int kMacOsTapFloorMajor = 14;
constexpr int kMacOsTapFloorMinor = 4;
#endif

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
    int major = 0;
    int minor = 0;
    // A patch component may or may not be present ("14.4" and "14.4.1" both occur).
    const int parsed = std::sscanf(cap.osVersion, "%d.%d", &major, &minor);
    if (parsed < 1) {
      cap.reason = "could not parse the macOS version";
    } else if (major > kMacOsTapFloorMajor ||
               (major == kMacOsTapFloorMajor && minor >= kMacOsTapFloorMinor)) {
      cap.perProcessAudio = true;
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
// The capture seam — start / drain / stop.
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
// ordering bug this seam can have: the producer thread is the only caller of
// napi_call_threadsafe_function, so stopCapture() sets `running` false, JOINS the
// thread, and only THEN releases the function. Releasing first would leave a live
// thread calling into a released handle. `signal` itself is written only before
// the thread exists and after it has been joined, so it is never read and written
// concurrently and needs no atomicity of its own.
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
  uv_thread_t              thread;
  std::atomic<bool>        running;
  bool                     threadJoinable;
  bool                     cleanupHookInstalled;
};

Capture g_capture = {nullptr, uv_thread_t(), {false}, false, false};

// Discard whatever is still queued. Called from start() and from stopCapture(),
// both of which run with no producer alive -- start() before the thread exists and
// stopCapture() after it has been joined -- which is what makes this safe on a
// ring that has no other synchronisation.
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

#ifdef CONCORD_AUDIOCAP_SYNTHETIC

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

// The synthetic producer. Compiled only into the CI/test target; a release binary
// has no producer at all and its start() returns NoBackend.
void synthesizeQuanta(void* /*arg*/) {
  rt::u8 quantum[rt::kQuantumBytes];
  rt::u32 seq = 0u;

  const std::uint64_t periodNs =
      static_cast<std::uint64_t>(rt::kQuantumMs) * 1000000ull;
  std::uint64_t deadline = uv_hrtime();

  while (g_capture.running.load(std::memory_order_acquire)) {
    const rt::QuantumHeader header = {seq, uv_hrtime(), g_ring.overrun()};
    // Both results are tested (AV 115). A refusal from either means the buffer is
    // the wrong size, which is a programming error here rather than a runtime
    // condition — so nothing is pushed and nothing is signalled.
    if (rt::encodeQuantumHeader(quantum, sizeof(quantum), header) &&
        rt::SyntheticSource::fillSamples(seq, &quantum[rt::kHeaderBytes],
                                         rt::kSampleBytes)) {
      if (g_ring.push(quantum, rt::kQuantumBytes) == RingResult::kOk) {
        // napi_queue_full is an EXPECTED status here, not a failure: it means a
        // signal is already pending and this one coalesces into it.
        (void)napi_call_threadsafe_function(g_capture.signal, nullptr,
                                            napi_tsfn_nonblocking);
      }
      // kFull needs no branch. The ring counted the drop, and a dropped quantum
      // has nothing to announce; the seq gap at the far end is its witness.
    }
    // Advanced even on a drop -- that gap is the second, independent drop
    // observer, and it still moves if the counter above ever lies.
    ++seq;

    deadline += periodNs;
    const std::uint64_t now = uv_hrtime();
    if (now >= deadline) {
      deadline = now;  // late: skip the missed slots, never burst to catch up
    } else {
      // Rounded UP, so a sub-millisecond remainder sleeps 1 ms instead of
      // spinning. The deadline accumulator absorbs the overshoot, so the cadence
      // stays right on average.
      const std::uint64_t remaining = deadline - now;
      uv_sleep(static_cast<unsigned int>((remaining + 999999ull) / 1000000ull));
    }
  }
}
#endif  // CONCORD_AUDIOCAP_SYNTHETIC

// Idempotent, and safe to call from the environment cleanup hook. See the
// ordering invariant in the header comment above.
void stopCapture() {
  g_capture.running.store(false, std::memory_order_release);
  if (g_capture.threadJoinable) {
    uv_thread_join(&g_capture.thread);
    g_capture.threadJoinable = false;
  }
  if (g_capture.signal != nullptr) {
    (void)napi_release_threadsafe_function(g_capture.signal, napi_tsfn_release);
    g_capture.signal = nullptr;
  }
  // AFTER the join, never before: the producer must be gone before anything else
  // touches the ring.
  drainRing();
}

// A capture left running at environment teardown would have a live thread calling
// into a dying env. The hook is what makes that unreachable even if the JS side
// never calls stop().
void captureEnvCleanup(void* /*arg*/) {
  g_capture.cleanupHookInstalled = false;  // node removes the hook as it runs it
  stopCapture();
}

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

  if (g_capture.running.load(std::memory_order_acquire)) {
    NAPI_CALL(env, makeStartResult(env, false, "AlreadyStarted", &result));
    return result;
  }

#ifndef CONCORD_AUDIOCAP_SYNTHETIC
  // THE RELEASE ANSWER. #3195 ships the transport, not a backend: Windows
  // ProcessLoopback is #3196 and the macOS Core Audio process tap is #3197. The
  // host resolves NoBackend to video-only with a reason — never to a system mix,
  // which is constraint C9 and #2161.
  NAPI_CALL(env, makeStartResult(env, false, "NoBackend", &result));
  return result;
#else
  drainRing();

  napi_value resourceName = nullptr;
  NAPI_CALL(env, napi_create_string_utf8(env, "concord-audiocap:quantum",
                                         NAPI_AUTO_LENGTH, &resourceName));
  // max_queue_size 1 and one acquiring thread (the producer). Both numbers are
  // load-bearing; see the section header.
  NAPI_CALL(env, napi_create_threadsafe_function(
                     env, args[1], nullptr, resourceName,
                     /*max_queue_size=*/1, /*initial_thread_count=*/1,
                     nullptr, nullptr, nullptr, callQuantumSignal,
                     &g_capture.signal));

  NAPI_CALL(env, napi_add_env_cleanup_hook(env, captureEnvCleanup, nullptr));
  g_capture.cleanupHookInstalled = true;

  g_capture.running.store(true, std::memory_order_release);
  if (uv_thread_create(&g_capture.thread, synthesizeQuanta, nullptr) != 0) {
    // Unwind in the reverse order, so nothing is left half-armed.
    g_capture.running.store(false, std::memory_order_release);
    (void)napi_remove_env_cleanup_hook(env, captureEnvCleanup, nullptr);
    g_capture.cleanupHookInstalled = false;
    (void)napi_release_threadsafe_function(g_capture.signal, napi_tsfn_release);
    g_capture.signal = nullptr;
    NAPI_CALL(env, makeStartResult(env, false, "ThreadStartFailed", &result));
    return result;
  }
  g_capture.threadJoinable = true;

  NAPI_CALL(env, makeStartResult(env, true, nullptr, &result));
  return result;
#endif
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
  return exports;
}
