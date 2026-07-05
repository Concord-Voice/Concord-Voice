#!/usr/bin/env bash
#
# ci-shard-packages.sh — deterministic, time-balanced package sharding for the
# control-plane `go test` suite (CI matrix). Prints the newline-separated set of
# Go import paths assigned to the requested shard.
#
# Usage:
#   ci-shard-packages.sh <shard-index> <shard-total>
#     shard-index : 1-based index of THIS shard (1..shard-total)
#     shard-total : number of shards in the matrix
#
# Balance strategy — greedy longest-processing-time (LPT): sort packages by
# expected run-time descending, then repeatedly place the next package on the
# currently-lightest shard. Per-package run-times come from the sibling manifest
# ci-shard-timings.txt (`<relative-package>  <seconds>`); packages absent from
# the manifest (newly added, or with no test files) get CI_SHARD_DEFAULT_WEIGHT
# so they are still assigned and roughly balanced.
#
# CORRECTNESS INVARIANT (why sharding cannot silently drop coverage): the
# authoritative package set is `go list ./...` evaluated at run time, and the
# LPT assignment is a pure, deterministic function of (sorted package list,
# manifest, shard-total). Every shard invocation therefore computes the SAME
# assignment and simply filters to its own index, so the union across shards is
# exactly `go list ./...` — complete, with no package duplicated or dropped.
# `go test <these packages>` thus covers the same set as the unsharded
# `go test ./...`, preserving the Go coverage denominator SonarCloud consumes.
# The manifest is advisory only: stale entries for removed packages are ignored
# and correctness never depends on it. The script fails loud if `go list`
# yields nothing rather than silently emitting an empty (zero-coverage) shard.
#
# Determinism note: the package list is piped through `sort` before assignment
# so the result is independent of any ordering differences in `go list` output
# across the separate shard runners — without this, a tie-break divergence
# between shard 1 and shard 2 could drop or double-run a package.
#
# Exits:
#   0 — printed this shard's package list
#   2 — usage / environment error (bad args, or `go list` failed / empty)

set -euo pipefail

SHARD_INDEX="${1:?usage: ci-shard-packages.sh <shard-index> <shard-total>}"
SHARD_TOTAL="${2:?usage: ci-shard-packages.sh <shard-index> <shard-total>}"

if ! [[ "$SHARD_INDEX" =~ ^[0-9]+$ ]] || ! [[ "$SHARD_TOTAL" =~ ^[0-9]+$ ]] \
   || [[ "$SHARD_INDEX" -lt 1 ]] || [[ "$SHARD_TOTAL" -lt 1 ]] \
   || [[ "$SHARD_INDEX" -gt "$SHARD_TOTAL" ]]; then
  echo "ERROR: invalid shard-index/shard-total: '${SHARD_INDEX}'/'${SHARD_TOTAL}'" >&2
  echo "       shard-index must be in 1..shard-total and both positive integers" >&2
  exit 2
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFEST="${SCRIPT_DIR}/ci-shard-timings.txt"
DEFAULT_WEIGHT="${CI_SHARD_DEFAULT_WEIGHT:-5}"

module="$(go list -m)"

mapfile -t PKGS < <(go list ./...)
if [[ "${#PKGS[@]}" -eq 0 ]]; then
  echo "ERROR: 'go list ./...' returned no packages (wrong working directory?)" >&2
  exit 2
fi

printf '%s\n' "${PKGS[@]}" | LC_ALL=C sort | awk \
  -v idx="$SHARD_INDEX" -v total="$SHARD_TOTAL" -v def="$DEFAULT_WEIGHT" \
  -v module="$module" -v manifest="$MANIFEST" '
  BEGIN {
    # Load the advisory timing manifest: "<relative-package>  <seconds>".
    while ((getline line < manifest) > 0) {
      if (line ~ /^[[:space:]]*#/ || line ~ /^[[:space:]]*$/) continue
      n = split(line, f, /[[:space:]]+/)
      if (n >= 2) weight[f[1]] = f[2] + 0
    }
    close(manifest)
  }
  {
    full = $0
    rel = full
    sub("^" module "/", "", rel)     # module-relative key for manifest lookup
    count++
    pkg[count] = full
    w[count] = (rel in weight) ? weight[rel] : def
  }
  END {
    # Selection sort package indices by weight descending (count is small).
    for (i = 1; i <= count; i++) order[i] = i
    for (i = 1; i <= count; i++) {
      mx = i
      for (j = i + 1; j <= count; j++)
        if (w[order[j]] > w[order[mx]]) mx = j
      tmp = order[i]; order[i] = order[mx]; order[mx] = tmp
    }
    # Greedy LPT: place each package on the currently-lightest bin.
    for (b = 1; b <= total; b++) load[b] = 0
    for (i = 1; i <= count; i++) {
      lb = 1
      for (b = 2; b <= total; b++) if (load[b] < load[lb]) lb = b
      bin[order[i]] = lb
      load[lb] += w[order[i]]
    }
    # Emit only this shard, in sorted order for stable, greppable logs.
    for (i = 1; i <= count; i++) if (bin[i] == idx) print pkg[i]
  }
'
