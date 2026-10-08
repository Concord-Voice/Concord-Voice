#!/usr/bin/env bash
# Shared, source-safe read-only self-host coturn TLS admission and readiness.

_selfhost_tls_fail() {
  printf '%s\n' "$1" >&2
  return 1
}

_selfhost_tls_helper() {
  local source_file="${BASH_SOURCE[0]}" source_dir
  source_dir="$(cd -- "$(dirname -- "$source_file")" 2>/dev/null && pwd -P)" || return 1
  printf '%s/selfhost-tls.py' "$source_dir"
}

_selfhost_tls_context_ready() {
  local compose_args
  [[ "${SELFHOST_STORAGE_MODE_RESOLVED:-}" == bundled \
    || "${SELFHOST_STORAGE_MODE_RESOLVED:-}" == byo-s3 ]] || return 1
  [[ -n "${SELFHOST_STORAGE_PROJECT_ROOT:-}" \
    && -n "${SELFHOST_STORAGE_PROJECT_NAME:-}" ]] || return 1
  declare -p SELFHOST_STORAGE_COMPOSE_ARGS >/dev/null 2>&1 || return 1
  compose_args="${SELFHOST_STORAGE_COMPOSE_ARGS[*]-}"
  [[ -n "$compose_args" ]]
}

_selfhost_tls_run() {
  local operation="$1" budget="$2" helper
  shift 2
  _selfhost_tls_context_ready \
    || { _selfhost_tls_fail 'selfhost-tls: resolved storage authority required'; return 1; }
  if ! helper="$(_selfhost_tls_helper)" || [[ ! -f "$helper" ]]; then
    _selfhost_tls_fail 'selfhost-tls: observer prerequisite unavailable'
    return 1
  fi
  selfhost_storage_scrub_environment \
    || { _selfhost_tls_fail 'selfhost-tls: environment admission failed'; return 1; }
  python3 "$helper" "$operation" "$SELFHOST_STORAGE_PROJECT_ROOT" \
    "$SELFHOST_STORAGE_PROJECT_NAME" "$budget" "${SELFHOST_STORAGE_TRUSTED_IMAGE:-}" \
    -- "${SELFHOST_STORAGE_COMPOSE_ARGS[@]}"
}

selfhost_tls_admit() {
  [[ $# -eq 0 ]] || { _selfhost_tls_fail 'selfhost-tls: admission arguments refused'; return 1; }
  _selfhost_tls_run admit 5
}

selfhost_tls_ready() {
  local budget=15
  if [[ $# -gt 1 ]]; then
    _selfhost_tls_fail 'selfhost-tls: readiness arguments refused'
    return 1
  fi
  if [[ $# -eq 1 ]]; then
    budget="$1"
    [[ "$budget" =~ ^([0-9]+([.][0-9]*)?|[.][0-9]+)$ ]] \
      || { _selfhost_tls_fail 'selfhost-tls: caller deadline refused'; return 1; }
    [[ ! "$budget" =~ ^0*\.?0*$ ]] \
      || { _selfhost_tls_fail 'selfhost-tls: caller deadline expired'; return 1; }
  fi
  _selfhost_tls_run ready "$budget"
}
