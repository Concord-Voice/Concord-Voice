#include "napi/window_owner.h"

#if defined(_WIN32)
#  define WIN32_LEAN_AND_MEAN
#  define NOMINMAX
#  include <windows.h>
#elif defined(__APPLE__)
#  include <CoreFoundation/CoreFoundation.h>
#  include <CoreGraphics/CoreGraphics.h>
#endif

namespace concord {

std::uint32_t ResolveWindowOwner(std::uint32_t handle) {
  if (handle == 0) { return 0; }

#if defined(_WIN32)
  // SIGN-EXTEND, do not zero-extend. Microsoft's documented Win64 rule for USER
  // and GDI handles is that a 32-bit handle value widens by sign extension. A
  // zero-extending cast turns an HWND whose 32-bit form has the top bit set into
  // 0x00000000_8xxxxxxx instead of 0xFFFFFFFF_8xxxxxxx, the lookup below refuses,
  // and that window can never carry app audio. Fail-closed, so never a wrong PID
  // -- but a PERMANENT silent refusal that no test on this branch can observe,
  // which is why the cast is spelled out rather than left to reinterpret_cast.
  HWND hwnd = reinterpret_cast<HWND>(
      static_cast<std::intptr_t>(static_cast<std::int32_t>(handle)));

  // ONE OS call, and deliberately no IsWindow pre-check in front of it.
  //
  // There used to be one, justified as protecting against handle recycling. It
  // did the OPPOSITE. GetWindowThreadProcessId already returns 0 -- the failure
  // value tested below -- for a handle no live window owns, so IsWindow refused
  // nothing the next line does not refuse. What it added was the GAP BETWEEN THE
  // TWO CALLS: a window destroyed after IsWindow said yes can have its HWND
  // recycled to a window owned by a DIFFERENT process before the second call
  // reads it, and the resolver then returns that other process's PID. A capture
  // aimed at an app the user never picked is exactly the widening ADR-0043 D4a
  // forbids, and the pre-check was the only thing that made it reachable.
  //
  // There is no atomic IsWindow+GetWindowThreadProcessId pair, so the fix is to
  // remove the window rather than narrow it: one call, no interval.
  DWORD pid = 0;
  if (GetWindowThreadProcessId(hwnd, &pid) == 0) { return 0; }
  return static_cast<std::uint32_t>(pid);

#elif defined(__APPLE__)
  CFArrayRef list = CGWindowListCopyWindowInfo(kCGWindowListOptionIncludingWindow,
                                               static_cast<CGWindowID>(handle));
  if (list == nullptr) { return 0; }

  std::uint32_t resolved = 0;
  // EXACTLY ONE entry, not "at least one". kCGWindowListOptionIncludingWindow
  // with a single id returns 0 or 1 elements; treating >1 as a match would be
  // accepting an answer the API does not document.
  //
  // MEASURED on macOS 26.6.2 (534 live windows): this option used ALONE is an
  // ON-SCREEN filter. 67 of 67 on-screen windows returned one element and 0 of
  // 467 off-screen ones did, so a MINIMISED window refuses here and its share
  // goes video-only with a reason. That is the fail-closed direction and is
  // deliberate -- kCGWindowListOptionAll is 0, so there is no flag that widens
  // this lookup without replacing it with a full-list scan.
  if (CFArrayGetCount(list) == 1) {
    // TYPE-CHECK THE CONTAINER TOO, not just the value inside it. The rule ten
    // lines below -- an unchecked CF cast is undefined behaviour in a process
    // that hosts an audio callback -- applies identically to the array element,
    // and CFDictionaryGetValue on a non-dictionary CFTypeRef is exactly that.
    // CoreGraphics documents this array as holding window-info dictionaries, so
    // this is defence in depth, but the asymmetry was the finding: the file
    // already argued for the check and then made one unguarded cast anyway.
    CFTypeRef raw = CFArrayGetValueAtIndex(list, 0);
    if (raw != nullptr && CFGetTypeID(raw) == CFDictionaryGetTypeID()) {
      CFDictionaryRef info = static_cast<CFDictionaryRef>(raw);

      // IDENTITY CHECK. `kCGWindowListOptionIncludingWindow` is documented for
      // use WITH an above/below companion option; used alone -- as here -- its
      // result is not specified to contain only the reference window. Measured
      // behaviour says it does (see below), and `== 1` already refuses a list
      // that is not a single element, but neither fact rules out a single
      // element that is some OTHER window. That would resolve a PID the user
      // never picked, which is the one outcome this resolver must never produce.
      // Four lines close it at the root and cost nothing when the API behaves.
      CFTypeRef number = CFDictionaryGetValue(info, kCGWindowNumber);
      std::int64_t windowId = 0;
      const bool identityProved =
          number != nullptr && CFGetTypeID(number) == CFNumberGetTypeID() &&
          CFNumberGetValue(static_cast<CFNumberRef>(number), kCFNumberSInt64Type, &windowId) &&
          windowId == static_cast<std::int64_t>(handle);

      CFTypeRef owner = CFDictionaryGetValue(info, kCGWindowOwnerPID);
      // TYPE-CHECK the CF value. CFDictionaryGetValue returns CFTypeRef and an
      // unchecked cast to CFNumberRef on a different type is undefined behaviour
      // in a process that hosts an audio callback.
      if (identityProved && owner != nullptr && CFGetTypeID(owner) == CFNumberGetTypeID()) {
        std::int32_t value = 0;
        if (CFNumberGetValue(static_cast<CFNumberRef>(owner), kCFNumberSInt32Type, &value) &&
            value > 0) {
          resolved = static_cast<std::uint32_t>(value);
        }
      }
    }
  }
  CFRelease(list);
  return resolved;

#else
  // Linux/PipeWire is out of ADR-0043's scope. Refuse rather than omit, so the
  // export exists on every platform and the refusal is the answer.
  (void)handle;
  return 0;
#endif
}

}  // namespace concord
