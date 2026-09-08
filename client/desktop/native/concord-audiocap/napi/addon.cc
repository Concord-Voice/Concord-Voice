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

#include <cstddef>  // size_t, named directly in the macOS sysctl branch
#include <cstdio>   // std::snprintf, std::sscanf
#include <cstring>

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

}  // namespace

NAPI_MODULE_INIT() {
  // Throw before returning nullptr, mirroring NAPI_CALL. A null exports with NO
  // pending exception gives the loader a generic failure that names nothing --
  // the opposite of the report-rather-than-degrade posture the rest of this file
  // takes.
  napi_value fn = nullptr;
  if (napi_create_function(env, "capability", NAPI_AUTO_LENGTH, Capability_JS,
                           nullptr, &fn) != napi_ok) {
    napi_throw_error(env, nullptr, "concord-audiocap: could not create capability()");
    return nullptr;
  }
  if (napi_set_named_property(env, exports, "capability", fn) != napi_ok) {
    napi_throw_error(env, nullptr, "concord-audiocap: could not export capability()");
    return nullptr;
  }
  return exports;
}
