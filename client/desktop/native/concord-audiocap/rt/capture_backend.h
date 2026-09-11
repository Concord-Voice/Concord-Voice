// concord-audiocap / rt — the platform-neutral capture-backend contract.
//
// JSF++ BINDS HERE. See [internal]rules/native-audio.md.
//
// THIS IS THE SEAM #3196 (Windows) AND PR 2 (macOS Core Audio taps) BOTH
// INHERIT. It is deliberately declared in PR 1, with no platform code behind it,
// because the hardest constraint on it is not macOS: it is that a CALLBACK-DRIVEN
// backend cannot satisfy a teardown expressed as "join the producer thread".
//
// WHY THE TARGET IS A BOUNDED LIST AND NOT A PID.
// ADR-0043 risk 2 is that the window's PID is not the process rendering the
// audio. Windows expresses that with INCLUDE_TARGET_PROCESS_TREE on one PID;
// macOS cannot -- CATapDescription takes an explicit process-object list. A
// single-u32 field would force one of the two backends to reopen this contract,
// and not reopening it is what the rollout ordering exists to protect.
// kMaxTargetPids is a bound on a caller-supplied array length, not a claim about
// how many helper processes a browser has.
//
// WHY TEARDOWN IS A POST-CONDITION AND NOT A MECHANISM.
// A thread-driven backend satisfies it by joining. A callback-driven backend
// satisfies it by AudioDeviceStop + AudioDeviceDestroyIOProcID + destroying the
// tap. NEITHER IS TRUSTED: rt/sink_gate.h holds even if a backend lies, and the
// contract suite carries a fake that does exactly that.
//
// AV 206 (no allocation): every type here is a value type with fixed storage, and
// the base class is never heap-allocated -- see the protected destructor.

#ifndef CONCORD_AUDIOCAP_RT_CAPTURE_BACKEND_H_
#define CONCORD_AUDIOCAP_RT_CAPTURE_BACKEND_H_

#include "quantum_header.h"
#include "quantum_pump.h"

namespace concord {
namespace audiocap {
namespace rt {

constexpr u8 kMaxTargetPids = 8u;

/// WHAT A CAPTURE IS AIMED AT, and it exists because `pidCount == 0` could not
/// say. ADR-0043 D6 carries two adjacent rows that both land on an empty PID
/// list: a full-screen or monitor target legitimately captures the whole system
/// mix, and a WINDOW target whose process could not be resolved must NEVER
/// silently fall back to one (that is #2161's defect class — capture pointed
/// somewhere other than where the user said). Until PR #3262 only a comment
/// separated them, which meant the safe reading depended on every backend
/// author reading it the same way, and the contract suite's fakes proved the
/// fakes rather than the seam.
///
/// So the two requests are now DIFFERENT VALUES rather than the same one read
/// twice. A whole-system mix is reachable only by an affirmative
/// `scope = kSystemMix`, never by omission, never by a zeroed struct, and never
/// by a PID lookup that failed — every one of those is `kProcessList` with an
/// empty list, which a real backend refuses with kNoTarget.
///
/// kProcessList IS ZERO deliberately: `CaptureTarget{}` and any caller that
/// forgets the field both land on the arm that refuses, which is the only
/// default a target selector may have.
enum class CaptureScope : u8 {
  kProcessList = 0,   // capture exactly `pids[0..pidCount)`; an empty list is refused
  kSystemMix          // capture the whole system mix; `pids` is not read
};

struct CaptureTarget {              // value type, trivially copyable, no allocation
  u32  pids[kMaxTargetPids];
  u8   pidCount;                    // meaningful only when scope == kProcessList
  bool allowDescendants;
  /// Set by whoever knows WHAT THE USER PICKED, which is the host and never a
  /// backend: #3198 resolves a window target to a PID list (kProcessList) and a
  /// monitor / full-screen target to kSystemMix. Nothing in PR 1 ever sets
  /// kSystemMix, so the rung is declared and unreachable rather than absent and
  /// improvised later — see the note above on why it may not be encoded as an
  /// empty list.
  CaptureScope scope;
};

struct SourceFormat {               // what the OS says it will deliver
  u32  sampleRate;                  // MUST be 48000 or the pump refuses
  u16  channelCount;                // 1 or 2; anything else is refused
  bool interleaved;
  bool isFloat32;                   // false is refused in PR 1 and PR 2
};

enum class BackendStart : u8 {
  kOk = 0, kNoTarget, kUnsupportedOs, kUnsupportedFormat,
  kPermissionDenied, kDeviceError, kAlreadyRunning
};

/// Injected by napi/ so rt/ never names uv, <thread>, or a clock (AV 22/25).
///
/// `join` receives the SAME `arg` the backend handed to `spawn`: the host owns
/// the thread object and looks it up, because a backend under this profile has
/// nowhere to put an opaque handle it would have to allocate. `spawn` returning
/// false is a thread that did not start, which a backend reports as kDeviceError.
struct HostServices {
  bool (*spawn)(void (*entry)(void*), void* arg) noexcept;  // thread-driven backends only
  void (*join)(void* handle) noexcept;
  u64  (*nowNs)() noexcept;
};

/// The one place the admission rule for a source format is written down.
///
/// It lives here, and NOT in each backend, because rt/platform/ may call the OS
/// and may not hold policy (design section 2's boundary rule): two backends each
/// deciding what 48 kHz means is two chances to disagree on a rule that must fail
/// closed. Nothing is resampled and nothing is downmixed -- a source that is not
/// 48 kHz interleaved-or-planar binary32 with 1 or 2 channels is REFUSED, and the
/// caller reports kUnsupportedFormat.
inline bool acceptsSourceFormat(const SourceFormat& format) noexcept {
  if (format.sampleRate != kSampleRate) { return false; }
  if (!format.isFloat32) { return false; }
  if (format.channelCount == 0u || format.channelCount > kChannels) { return false; }
  return true;
}

/// The one place the admission rule for a caller-supplied PID LIST is written
/// down, and it lives here for the same reason acceptsSourceFormat does: napi/
/// owns what a JS value must BE to become a u32, rt/ owns what a CaptureTarget
/// will ACCEPT, and a policy split across two backends is two chances to
/// disagree on a rule that must fail closed.
///
/// A REFUSAL PUBLISHES NOTHING, exactly as encodeQuantumHeader's does. `out` is
/// cleared first and filled only after every element has been admitted, so a
/// caller that ignored the result (AV 115 says none may, but the struct must be
/// safe anyway) is left holding pidCount == 0 — "no target supplied", which a
/// real backend refuses with kNoTarget — and never a half-built list aimed at
/// some of the processes the caller asked for. A partially-accepted target is
/// the #2161 defect class in miniature: capture pointed somewhere other than
/// where the user said.
///
/// `count == 0` is REFUSED rather than treated as "no target". The two are
/// different requests: a caller that omitted the option entirely never reaches
/// here and gets the untouched no-target struct, while a caller that passed an
/// EMPTY list asked for something impossible and must be told so (BadOptions),
/// not silently downgraded to a start that fails later for a different reason.
///
/// PID 0 is refused on every platform this contract covers. It is the kernel /
/// swapper on macOS and the System Idle Process on Windows, it is the "any
/// process in my group" wildcard for POSIX signals, and it is what an
/// uninitialised or failed lookup leaves behind — none of which is a share
/// target.
///
/// A DUPLICATE PID IS REFUSED for a narrower reason: `pids` becomes a
/// CATapDescription process list on macOS, and Apple specifies no behaviour for
/// one that names the same process twice. A list this function admitted would be
/// handed to an OS call whose response to it is undefined — so the one place that
/// decides what a CaptureTarget will accept refuses it rather than each backend
/// discovering a platform's answer for itself. It is also evidence the caller's
/// resolution step went wrong, and #2161's lesson is that a target selector's
/// surprises are not to be smoothed over.
///
/// THIS FUNCTION NEVER PRODUCES kSystemMix. It is the admission rule for a PID
/// LIST, so everything it builds is kProcessList; a system mix carries no list to
/// validate and is set by the caller that knows the user picked a monitor.
inline bool buildTarget(const u32* pids, u32 count, bool allowDescendants,
                        CaptureTarget* out) noexcept {
  if (out == nullptr) { return false; }

  for (u8 slot = 0u; slot < kMaxTargetPids; ++slot) { out->pids[slot] = 0u; }
  out->pidCount         = 0u;
  out->allowDescendants = false;
  // CLEARED TO THE REFUSING ARM, not merely left alone: a caller that reused a
  // struct which already asked for a system mix must not keep that request
  // through a refusal it never saw.
  out->scope            = CaptureScope::kProcessList;

  if (pids == nullptr) { return false; }
  if (count == 0u) { return false; }
  if (count > static_cast<u32>(kMaxTargetPids)) { return false; }

  // Validated WHOLE before anything is written, which is what makes the refusal
  // arm publish nothing. The duplicate test is the inner loop: count is at most
  // kMaxTargetPids (8), so the comparison is at most 28 integer tests on a path
  // that runs once per capture, and a set would be an allocation (AV 206) to
  // avoid work nobody can measure. AV 201: neither counter is modified in a body.
  for (u32 i = 0u; i < count; ++i) {
    if (pids[i] == 0u) { return false; }
    for (u32 j = 0u; j < i; ++j) {
      if (pids[j] == pids[i]) { return false; }
    }
  }
  for (u32 i = 0u; i < count; ++i) {
    out->pids[i] = pids[i];
  }
  out->pidCount         = static_cast<u8>(count);
  out->allowDescendants = allowDescendants;
  out->scope            = CaptureScope::kProcessList;
  return true;
}

class CaptureBackend {
 public:
  /// WHAT THE TARGET OBLIGES, and it is ADR-0043 D6 stated where a backend author
  /// will read it:
  ///   scope == kProcessList with pidCount == 0  -> kNoTarget. ALWAYS. It is the
  ///     shape an omitted option, a zeroed struct and a FAILED PID LOOKUP all
  ///     take, and capturing the whole system for any of them is #2161.
  ///   scope == kProcessList with pidCount  > 0  -> capture exactly those
  ///     processes (plus descendants when asked), or refuse.
  ///   scope == kSystemMix                       -> the whole system mix, and
  ///     `pids` is not read. Only ever reached because a caller asked for it.
  ///
  /// On kOk the backend MAY call sink.submit()/noteCallback() from ANY thread
  /// until stop() returns. BOTH HALVES OF THAT "MAY" ARE REAL: noteCallback() is
  /// optional, which is why rt/teardown.h watches a counter the PUMP moves on
  /// every entry rather than one the backend volunteers — a witness a conforming
  /// backend can decline to move is not a witness.
  virtual BackendStart start(const CaptureTarget& target, QuantumPump& sink,
                             const HostServices& host) noexcept = 0;

  /// POST-CONDITION, and it is the whole contract:
  ///   when stop() RETURNS, no further call into `sink` will occur from any
  ///   thread, ever, and every OS artefact this backend created is destroyed.
  /// Idempotent. Must not block on anything the sink can hold.
  ///
  /// SO STOP() MUST NOT RETURN UNTIL THIS BACKEND'S OWN CALLBACKS ARE QUIESCED,
  /// and establishing that is the backend's job alone. A thread-driven backend
  /// joins. A callback-driven one must build the barrier itself — an in-callback
  /// flag the OS callback observes, plus an acknowledgement the stopping thread
  /// waits for — because the platform call that "stops" a callback is not
  /// documented to drain one already in flight.
  ///
  /// THE SEAM OBSERVES; IT DOES NOT SUBSTITUTE FOR THAT PROOF. rt/teardown.h
  /// watches the activity witness for kSettleObserveMs and reports a producer that
  /// MOVES inside that window, which catches a backend that simply kept running.
  /// It cannot catch one that is merely late, descheduled, or holding a lock for
  /// longer than the window, because ABSENCE IS NOT OBSERVABLE IN FINITE TIME:
  /// any window can be slept out, and widening it only names a longer nap. A
  /// backend that returns from stop() expecting its callbacks to drain shortly has
  /// already broken this contract, whether or not anything notices.
  ///
  /// WHAT BREAKING IT COSTS, stated so the trade is not rediscovered: the harm is
  /// BOUNDED but not undone. rt/sink_gate.h is closed before the wait and is never
  /// reopened, so a producer that resumes after stop() returned is refused at the
  /// gate — no bytes reach the consumer and the released handle is unreachable.
  /// What is NOT bounded is the tap's EXISTENCE, and that is the defect ADR-0043
  /// and #3197 are about: a live OS tap after the share ended is a privacy failure
  /// even when no byte moves. Nothing on this side of the seam can end it; only
  /// the backend can, which is why the obligation is written here.
  ///
  /// PR 2 (macOS Core Audio taps) OWES THIS EXPLICITLY. Apple documents no
  /// quiescence guarantee for a stopped device — the two teardown orderings the
  /// community uses disagree with each other and neither states a barrier — so
  /// returning from stop() on the strength of AudioDeviceStop's return alone would
  /// satisfy this contract by coincidence at best. The barrier is PR 2's to build
  /// and PR 2's to test; the contract suite here pins only what the seam itself
  /// can guarantee about a backend that does not.
  virtual void stop() noexcept = 0;

 protected:
  ~CaptureBackend() = default;      // never deleted through the base; never heap
};

/// nullptr on a platform with no backend compiled in -> start() = "NoBackend".
///
/// PR 1 HAS NO PLATFORM BACKEND ON ANY PLATFORM, which is what keeps a release
/// build's start() returning NoBackend and is asserted by the define-separation
/// step in native-audiocap.yml. #3196 and PR 2 each replace this body with a
/// platform-conditional one returning their own statically-stored singleton;
/// nothing here is ever heap-allocated (AV 206).
inline CaptureBackend* platformBackend() noexcept { return nullptr; }

}  // namespace rt
}  // namespace audiocap
}  // namespace concord

#endif  // CONCORD_AUDIOCAP_RT_CAPTURE_BACKEND_H_
