#ifndef CONCORD_AUDIOCAP_RT_PLATFORM_MACOS_TAP_FLOOR_H_
#define CONCORD_AUDIOCAP_RT_PLATFORM_MACOS_TAP_FLOOR_H_

// THE ONE PLACE THE macOS TAP FLOOR IS WRITTEN DOWN.
//
// A PURE PREDICATE OVER AN INJECTED STRING that makes no OS call, and that is
// the whole point: the below-floor branch becomes provable by passing "14.3"
// rather than by owning a machine below 14.4, and no machine in the local or CI
// fleet is one. It compiles and runs everywhere, so the Linux ASAN/UBSAN/TSAN
// legs cover it -- which matters more than usual here, because
// client/desktop/native/** sits outside sonar.sources and no line of it can
// ever count toward the >=80% new-code gate.
//
// TWO FLOORS, NESTED, NOT PARALLEL. This is the PRODUCT floor: 14.4, ADR-0043
// D6. The tap symbols are API_AVAILABLE(macos(14.2)) and their @available guard
// is a SYMBOL floor which must sit strictly INSIDE a check against this one --
// 14.2 and 14.3 have the symbols and are deliberately refused the feature.
//
// WHY capability() CANNOT BE THE ENFORCEMENT POINT: it is a probe, not a grant.
// Nothing forces a caller to consult it before start(), so a floor enforced only
// there is a floor a caller walks around. platformBackend() returning nullptr is
// the enforcement, and Start_JS already resolves that to "NoBackend".

#include "../../quantum_header.h"

namespace concord {
namespace audiocap {
namespace rt {
namespace macos {

constexpr u32 kTapFloorMajor = 14u;
constexpr u32 kTapFloorMinor = 4u;

/// The ONE scan, with both of its answers.
///
/// `parsed` is whether the string could be read at all; `atOrAboveFloor` is what
/// it said. They are separate because they are separate QUESTIONS -- see the two
/// wrappers below -- but they come from one pass, because this file exists
/// precisely to stop a version comparison from having two implementations that
/// can drift. An earlier draft of the second question re-parsed, which would
/// have reintroduced the defect the file was created to remove.
struct FloorScan {
  bool parsed;
  bool atOrAboveFloor;
};

inline FloorScan scanProductVersion(const char* productVersion) noexcept {
  FloorScan out = {false, false};
  if (productVersion == nullptr) { return out; }

  u32  field[2]  = {0u, 0u};
  u32  which     = 0u;
  bool sawDigit  = false;

  for (u32 i = 0u; productVersion[i] != '\0'; ++i) {
    const char c = productVersion[i];
    if (c >= '0' && c <= '9') {
      // Saturates rather than overflowing on an absurd string. The comparison
      // below only cares about the relation to 14.4, so a clamped 100000 and a
      // true 10^12 answer it identically.
      if (field[which] < 100000u) {
        field[which] = (field[which] * 10u) + static_cast<u32>(c - '0');
      }
      sawDigit = true;
    } else if (c == '.') {
      // BELT AND BRACES, AND NOT TEST-COVERED -- deliberately recorded rather
      // than left to look verified. A dot with no digit before it is either
      // leading, which pins the major at 0 and is therefore already below 14,
      // or doubled after the major, where `which == 1` breaks out on the next
      // line. Mutation testing confirmed removing this line changes the verdict
      // for NO input: it is an equivalent mutant today. It stays because it
      // makes the refusal explicit and survives an edit to the break below,
      // which is what would make it load-bearing again.
      if (!sawDigit) { return out; }     // leading or doubled dot
      if (which == 1u) { break; }        // patch component; stop, never read it
      which    = 1u;
      sawDigit = false;
    } else {
      return out;                        // any other character is unparseable
    }
  }
  // A trailing dot ("14.") leaves sawDigit false for the minor field, which is
  // unparseable rather than "14.0".
  if (!sawDigit) { return out; }

  out.parsed = true;
  if (field[0] > kTapFloorMajor)      { out.atOrAboveFloor = true; }
  else if (field[0] < kTapFloorMajor) { out.atOrAboveFloor = false; }
  else                                { out.atOrAboveFloor = field[1] >= kTapFloorMinor; }
  return out;
}

/// True when `productVersion` -- kern.osproductversion, e.g. "14.4.1" -- is at
/// or above the floor.
///
/// ANYTHING UNPARSEABLE IS BELOW IT. A version string this cannot read is not
/// evidence of a version that qualifies, and the failure that matters here is
/// granting capture on a machine that cannot do it safely.
///
/// Numeric comparison, never lexical: "14.10" is above "14.4" and a string
/// compare says the opposite.
inline bool meetsTapFloor(const char* productVersion) noexcept {
  const FloorScan s = scanProductVersion(productVersion);
  return s.parsed && s.atOrAboveFloor;
}

/// Could the string be READ AT ALL, independent of what it said?
///
/// The CAPABILITY DECISION rightly collapses "below the floor" and "unreadable"
/// into one refusal -- fail-closed is the only safe direction there. The
/// DIAGNOSTIC must not: telling a user on macOS 26 that their OS is too old,
/// because Apple appended a suffix this parser does not know, is a false
/// statement dressed as a reason, and capability().reason is documented as
/// "why not, in words fit to show a user".
inline bool parsesAsVersion(const char* productVersion) noexcept {
  return scanProductVersion(productVersion).parsed;
}

}  // namespace macos
}  // namespace rt
}  // namespace audiocap
}  // namespace concord

#endif  // CONCORD_AUDIOCAP_RT_PLATFORM_MACOS_TAP_FLOOR_H_
