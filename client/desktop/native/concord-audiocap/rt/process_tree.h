// concord-audiocap / rt — which process objects belong to a window's app.
//
// JSF++ BINDS HERE. See [internal]rules/native-audio.md.
//
// THE POLICY FOR ADR-0043 D4b RISK 2, in the core rather than a backend, for
// the reason acceptsSourceFormat and buildTarget live here: rt/platform/ may call
// the OS and may not hold policy. A window's audio is usually rendered by a
// helper process (Discord Helper (Renderer), Chrome's audio service), so the
// owner PID alone taps a process that makes no sound (#3394 PR 2, evidence E1).
//
// EVERY ARM FAILS CLOSED. A pid is included only when its parent chain reaches
// the owner BEFORE it reaches the host's own subtree, launchd, a failed lookup,
// or the depth bound. Under-capture is the only failure direction.

#ifndef CONCORD_AUDIOCAP_RT_PROCESS_TREE_H_
#define CONCORD_AUDIOCAP_RT_PROCESS_TREE_H_

#include "quantum_header.h"

namespace concord {
namespace audiocap {
namespace rt {

/// The expanded set's own bound. Above it the share REFUSES; a subset could drop
/// the one helper that renders. kMaxTargetPids (8) bounds only the caller's list.
constexpr u32 kMaxTreeObjects    = 16u;
/// Also what terminates a parent-pointer cycle.
constexpr u32 kMaxAncestorDepth  = 32u;
/// The system-wide enumeration buffer. A longer list refuses; it never truncates.
constexpr u32 kMaxProcessObjects = 512u;

/// Injected parent lookup: false when the parent cannot be read (exited, EPERM).
using ParentOf = bool (*)(void* ctx, u32 pid, u32* outParent) noexcept;

enum class TreeVerdict : u8 { kExcluded = 0, kIncluded };

/// Does the owner's OWN ancestry reach launchd without meeting `excludeRoot`?
///
/// inTree() below stops the moment its climb reaches `root`, so by itself it
/// proves only that no hop BETWEEN `pid` and `root` is the host. When `root`
/// itself descends from `excludeRoot` -- an owner Concord spawned -- every pid
/// under it is inside the host's subtree as well, and that climb never sees
/// the host at all (red-team PoC, #3394 PR 2 Phase 4). This is the missing half.
/// Every arm that cannot PROVE "outside" answers false: a failed lookup, the
/// depth bound, a cycle.
inline bool rootOutsideHost(u32 root, u32 excludeRoot, ParentOf parentOf,
                            void* ctx) noexcept {
  if (parentOf == nullptr) { return false; }
  u32 current = root;
  for (u32 hop = 0u; hop <= kMaxAncestorDepth; ++hop) {
    if (current == excludeRoot) { return false; }
    if (current <= 1u)          { return true; }
    u32 parent = 0u;
    if (!parentOf(ctx, current, &parent)) { return false; }
    current = parent;
  }
  return false;
}

/// Is `pid` the owner `root` or a descendant of it, and outside the host's
/// subtree rooted at `excludeRoot`? `excludeRoot` is checked BEFORE `root` on
/// every hop, and reaching `root` additionally requires `root`'s own ancestry
/// to clear the host (rootOutsideHost), so a window of the host OR of anything
/// the host spawned captures nothing of the host's subtree.
inline TreeVerdict inTree(u32 pid, u32 root, u32 excludeRoot, ParentOf parentOf,
                          void* ctx) noexcept {
  if (parentOf == nullptr) { return TreeVerdict::kExcluded; }
  if (root <= 1u)          { return TreeVerdict::kExcluded; }  // launchd's tree is the whole system
  if (excludeRoot <= 1u)   { return TreeVerdict::kExcluded; }  // host unknown: capture nothing
  u32 current = pid;
  for (u32 hop = 0u; hop <= kMaxAncestorDepth; ++hop) {
    if (current <= 1u)          { return TreeVerdict::kExcluded; }
    if (current == excludeRoot) { return TreeVerdict::kExcluded; }
    if (current == root) {
      return rootOutsideHost(root, excludeRoot, parentOf, ctx) ? TreeVerdict::kIncluded
                                                               : TreeVerdict::kExcluded;
    }
    u32 parent = 0u;
    if (!parentOf(ctx, current, &parent)) { return TreeVerdict::kExcluded; }
    current = parent;
  }
  return TreeVerdict::kExcluded;
}

}  // namespace rt
}  // namespace audiocap
}  // namespace concord

#endif  // CONCORD_AUDIOCAP_RT_PROCESS_TREE_H_
