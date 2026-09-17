// Handle -> owning PID. ONE OS call, no state, no allocation the caller owns.
//
// CALLED IN THE CAPTURE CHILD, NEVER IN MAIN (ADR-0043 D5). Main holds the
// handle; only the child ever learns a PID, and it never sends one back
// (invariant I-PID). Placing this in napi/ rather than rt/ is deliberate: it
// runs once per share, not per audio callback, so it is outside the JSF++
// profile and outside the real-time path.
#ifndef CONCORD_AUDIOCAP_NAPI_WINDOW_OWNER_H_
#define CONCORD_AUDIOCAP_NAPI_WINDOW_OWNER_H_

#include <cstdint>

namespace concord {

// Returns the PID owning `handle`, or 0 for EVERY refusal: handle 0, a handle no
// live window owns, a platform with no implementation, or an OS call that failed.
//
// 0 is safe as the sentinel because PID 0 is the idle/System process on Windows
// and the kernel task on macOS -- never a capture target on either, so it cannot
// collide with a real answer.
std::uint32_t ResolveWindowOwner(std::uint32_t handle);

}  // namespace concord

#endif  // CONCORD_AUDIOCAP_NAPI_WINDOW_OWNER_H_
