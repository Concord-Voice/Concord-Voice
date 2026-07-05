#!/usr/bin/env bash
#
# test-ci-shard-packages.sh — hermetic self-test for ci-shard-packages.sh.
# Stubs `go` (no module resolution / network needed) and asserts the sharding
# invariants that keep coverage complete:
#   1. union of all shards == full package set (nothing dropped)
#   2. shards are pairwise disjoint (nothing double-run)
#   3. assignment is deterministic across runs (so separate shard runners agree)
#   4. bad arguments fail loud (exit 2)
#   5. an empty `go list` fails loud rather than emitting an empty shard
#
# The invariant checks are factored into small helpers (shard_output /
# assert_partition / assert_exit2) so each unit stays simple; the run section
# at the bottom reads as a flat list of the five cases above.
#
# Run locally: bash services/control-plane/scripts/test-ci-shard-packages.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="${SCRIPT_DIR}/ci-shard-packages.sh"

FAILED=0
pass() { local msg="$1"; printf 'ok   - %s\n' "$msg"; }
fail() { local msg="$1"; printf 'FAIL - %s\n' "$msg" >&2; FAILED=1; }

MODULE="example.com/fake/mod"
# A representative spread incl. no-test packages absent from the real manifest.
FAKE_PKGS=(
  internal/auth internal/mfa internal/rbac internal/dm internal/users
  internal/voice internal/websocket internal/admin internal/api internal/age
  internal/servercapabilities pkg/config pkg/logger cmd/server cmd/migrate
  internal/nonexistent-new-package
)

STUB_DIR="$(mktemp -d)"
trap 'rm -rf "$STUB_DIR"' EXIT

# Write a `go` stub onto PATH. $1 selects the package-list behaviour:
#   "full"  -> emit FAKE_PKGS (module-prefixed)
#   "empty" -> emit nothing (simulate a mis-invoked `go list`)
write_go_stub() {
  local mode="$1"
  # The single-quoted printf formats below deliberately emit literal shell
  # (`${1:-}`, heredoc markers) into the generated stub, not expand it here.
  # shellcheck disable=SC2016
  {
    printf '#!/usr/bin/env bash\n'
    printf 'set -euo pipefail\n'
    printf 'if [[ "${1:-}" == "list" && "${2:-}" == "-m" ]]; then echo %q; exit 0; fi\n' "$MODULE"
    if [[ "$mode" == "full" ]]; then
      printf 'if [[ "${1:-}" == "list" ]]; then cat <<'\''PKGS'\''\n'
      for p in "${FAKE_PKGS[@]}"; do printf '%s/%s\n' "$MODULE" "$p"; done
      printf 'PKGS\nexit 0; fi\n'
    else
      printf 'if [[ "${1:-}" == "list" ]]; then exit 0; fi\n'
    fi
    printf 'echo "stub go: unhandled args: $*" >&2; exit 99\n'
  } > "${STUB_DIR}/go"
  chmod +x "${STUB_DIR}/go"
}

export PATH="${STUB_DIR}:${PATH}"

all_sorted() {
  local p
  for p in "${FAKE_PKGS[@]}"; do printf '%s/%s\n' "$MODULE" "$p"; done | LC_ALL=C sort
}

# Print one shard's package list, asserting it is deterministic across two runs
# (separate shard runners must compute the identical assignment).
shard_output() { # $1=shard-index  $2=shard-total
  local idx="$1" total="$2" a b
  a="$(bash "$TARGET" "$idx" "$total")"
  b="$(bash "$TARGET" "$idx" "$total")"
  if [[ "$a" != "$b" ]]; then
    fail "total=$total shard=$idx not deterministic"
  fi
  printf '%s' "$a"
}

# Assert that, for a given shard-total, the shards are pairwise disjoint and
# their union equals the full package set (invariants 1-2, with 3 via
# shard_output).
assert_partition() { # $1=shard-total
  local total="$1" idx pkg overlap=0 union
  union="$(mktemp)"
  local -A seen=()
  for ((idx = 1; idx <= total; idx++)); do
    while IFS= read -r pkg; do
      [[ -z "$pkg" ]] && continue
      if [[ -n "${seen[$pkg]:-}" ]]; then overlap=1; fi
      seen["$pkg"]=1
      printf '%s\n' "$pkg" >> "$union"
    done <<< "$(shard_output "$idx" "$total")"
  done
  if [[ "$overlap" -eq 0 ]]; then
    pass "total=$total shards are disjoint"
  else
    fail "total=$total shards overlap"
  fi
  if diff -q <(LC_ALL=C sort "$union") <(all_sorted) >/dev/null; then
    pass "total=$total union == full package set"
  else
    fail "total=$total union != full package set"
  fi
  rm -f "$union"
}

# Assert the target exits 2 (usage/environment error) for the given argv.
assert_exit2() { # $1=description  $2..=argv passed to the target
  local desc="$1"
  shift
  local rc=0
  bash "$TARGET" "$@" >/dev/null 2>&1 || rc=$?
  if [[ "$rc" -eq 2 ]]; then
    pass "$desc -> exit 2"
  else
    fail "$desc -> exit $rc (want 2)"
  fi
}

# ── Tests 1-3: complete, disjoint, deterministic for total=2 and total=3 ───
write_go_stub full
for total in 2 3; do
  assert_partition "$total"
done

# ── Test 4: bad args exit 2 ───────────────────────────────────────────────
assert_exit2 "bad args '0 2'" 0 2
assert_exit2 "bad args '3 2'" 3 2
assert_exit2 "bad args 'abc 2'" abc 2
assert_exit2 "bad args '1 0'" 1 0

# ── Test 5: empty `go list` fails loud ────────────────────────────────────
write_go_stub empty
assert_exit2 "empty 'go list'" 1 2

echo
if [[ "$FAILED" -eq 0 ]]; then
  echo "All ci-shard-packages tests passed."
else
  echo "ci-shard-packages tests FAILED." >&2
  exit 1
fi
