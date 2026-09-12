#!/usr/bin/env bash
# Concord Voice — Unified Dev Lifecycle CLI
#
# Mirrors [internal]concord-ctl.sh verb grammar where possible.
# This file is laptop-only — NEVER copied to /opt/concord on production servers.
#
# Usage: ./scripts/concord-dev.sh <verb> [options]
# Run with no args or `help` for the verb list.
#
# Exit codes:
#   0  success
#   1  generic / runtime error
#   2  pre-flight failure (port-in-use, missing .env, docker not running)
#   3  service startup failure (control-plane or media-plane /health timeout)

set -euo pipefail

# ── Constants ──────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
readonly PROJECT_ROOT
# LOG_DIR is overridable via env var so tests can sandbox log file ops to a
# tmpdir without polluting the real $PROJECT_ROOT/logs/. Production callers
# get the default $PROJECT_ROOT/logs behavior unchanged.
: "${LOG_DIR:=$PROJECT_ROOT/logs}"
readonly LOG_DIR

readonly RED='\033[0;31m'
readonly GREEN='\033[0;32m'
readonly YELLOW='\033[1;33m'
readonly BLUE='\033[0;34m'
readonly NC='\033[0m'

# Compose layering: base + dev overlay. Mirrors prod's base + production.yml pattern.
COMPOSE_FILES=(
  "-f" "$PROJECT_ROOT/docker-compose.yml"
  "-f" "$PROJECT_ROOT/docker-compose.dev.yml"
)

# Global flags (set by parse_global_flags before verb dispatch).
VERBOSE=0
QUIET=0

# ── Verb dispatch ──────────────────────────────────────────────────────

usage() {
  cat <<'USAGE'
Concord Voice — Dev Lifecycle CLI

Usage: ./scripts/concord-dev.sh <verb> [options]

Verbs:
  up [--skip-client] [--no-rebuild] [--clients N] [--reset-db]
                      Start all infra + control-plane + media-plane + electron clients
  down [--keep-docker] [--force]
                      Stop natives and (optionally) docker containers
                      --force may SIGKILL matching Concord listeners in sibling worktrees
  status              Show containers, health, PIDs, log sizes
  logs [<service>] [-n N] [--no-follow] [--errors] [--clear]
                      Tail logs (services: all, control-plane, media-plane, vite, client [N], docker [svc])
  restart             Stop + start without rebuild
  rebuild             Stop + start with rebuild (go build + npm install)
  freshstart          Wipe all data (DB, Redis, client data) and start fresh
  validate-config     Validate base + dev compose overlay
  dev-code [email]    Print the latest dev-mode email verification code from the
                      control-plane log (empty SMTP_HOST = codes logged, not emailed)
  console | repl      Interactive REPL: a concord> prompt that runs any verb
                      above without re-invoking this script each time

Global options (apply to any verb):
  --verbose           Echo docker/build commands to stderr
  --quiet             Suppress non-error output

Run with no verb for this help.
USAGE
}

main() {
  parse_global_flags "$@"
  local verb="${POSITIONAL[0]:-help}"
  set -- "${POSITIONAL[@]:1}"  # shift past verb
  dispatch_verb "$verb" "$@"
}

# dispatch_verb() — route one verb + its args to the matching handler. Shared by
# main() (one-shot CLI) and verb_console() (interactive REPL) so the verb list
# lives in exactly ONE place. Returns the verb's exit status; an unknown verb
# prints usage and returns 1 (main propagates it as the process exit code; the
# REPL tolerates it and keeps the prompt alive).
dispatch_verb() {
  local verb="${1:-help}"
  shift || true

  case "$verb" in
    up)              verb_up "$@" ;;
    down|stop)       verb_down "$@" ;;
    status)          verb_status "$@" ;;
    logs)            verb_logs "$@" ;;
    restart)         verb_restart "$@" ;;
    rebuild)         verb_rebuild "$@" ;;
    freshstart)      verb_freshstart "$@" ;;
    validate-config) verb_validate_config "$@" ;;
    dev-code)        verb_dev_code "$@" ;;
    console|repl)    verb_console "$@" ;;
    help|-h|--help)  usage ;;
    *)
      echo "Unknown verb: $verb" >&2
      usage >&2
      return 1
      ;;
  esac
}

parse_global_flags() {
  POSITIONAL=()
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --verbose) VERBOSE=1; shift ;;
      --quiet)   QUIET=1; shift ;;
      *)         POSITIONAL+=("$1"); shift ;;
    esac
  done
}

# ── Helpers ────────────────────────────────────────────────────────────

# compose() — invoke docker compose with the standard file flags.
# Usage: compose ps
#        compose up -d postgres redis
#        compose logs control-plane
compose() {
  if [[ "$VERBOSE" -eq 1 ]]; then
    echo "+ docker compose ${COMPOSE_FILES[*]} $*" >&2
  fi
  docker compose "${COMPOSE_FILES[@]}" "$@"
}

# ── Compose-project split detection ──────────────────────────────────────────
# Containers created WITHOUT the dev overlay land under a project name derived
# from the directory (concord-voice-alpha) instead of the `name:` declared in
# docker-compose.dev.yml. `compose ps` filters by the declared project, so those
# containers are invisible to every verb here: `status` reports an empty stack
# while postgres and redis are up, `up` tries to create containers whose names
# are already taken, and `down` removes only what it can see and then reports
# success -- leaving the rest to collide on the next `up`.
#
# Seen 2026-08-19, when another session's DB tests failed against containers a
# `down` had reported removing and it read as a code regression, and again
# 2026-09-08, when a clean `status` nearly justified recreating a postgres that
# had been up 36 hours. Nothing is broken in either case; the tooling's VIEW of
# the stack is wrong, which is exactly why filtering it out silently is the
# defect. Name it instead.

# Read the project from the file rather than hardcoding it, so this can never
# disagree with the command it is warning about.
declared_compose_project() {
  # Quotes are stripped because a quoted value kept verbatim matches no docker
  # label, so EVERY container reads as stray under a banner whose remedy is
  # `docker rm -f`. (\047 is awk's single quote; embedding one literally inside
  # this single-quoted program would need shell gymnastics for no gain.)
  awk '/^name:[[:space:]]/ { v = $2; gsub(/["\047]/, "", v); print v; exit }' \
    "${1:-$PROJECT_ROOT/docker-compose.dev.yml}" 2>/dev/null
}

# Only containers THIS stack would create. Matching a `concordvoice-` prefix
# instead would sweep in other sessions' isolated stacks, which are correctly
# separate and none of our business.
expected_container_names() {
  # Same quote-stripping as above, but this one fails the OTHER way: a quoted
  # container_name: never enters `known`, so a genuine split goes unreported —
  # silent and total, where the name: case is merely loud.
  # An array, not "${@:-a b}" — that expands the default as ONE word, so awk gets
  # a single nonexistent filename, the caller sees an empty set and returns early.
  local files=("$@")
  [[ ${#files[@]} -gt 0 ]] || files=(
    "$PROJECT_ROOT/docker-compose.yml"
    "$PROJECT_ROOT/docker-compose.dev.yml"
  )
  awk '/^[[:space:]]*container_name:[[:space:]]/ { v = $2; gsub(/["\047]/, "", v); print v }' \
    "${files[@]}" 2>/dev/null | sort -u
}

# Advisory only: never fails a verb, and silent when there is no split.
warn_compose_project_split() {
  local declared expected rows stray
  declared=$(declared_compose_project) || return 0
  [[ -n "$declared" ]] || return 0
  # "Could not look" and "nothing to report" are the same silence to a reader, and
  # a silently-degraded view is the exact defect this check exists to name. Say so
  # under --verbose rather than adding noise to every run.
  if ! command -v docker >/dev/null 2>&1; then
    [[ "$VERBOSE" -eq 1 ]] && echo "  split check skipped: docker is not on PATH" >&2
    return 0
  fi
  expected=$(expected_container_names) || return 0
  [[ -n "$expected" ]] || return 0

  if ! rows=$(docker ps -a --format '{{.Names}}\t{{.Label "com.docker.compose.project"}}\t{{.Status}}' 2>/dev/null); then
    [[ "$VERBOSE" -eq 1 ]] && echo "  split check skipped: docker ps failed (daemon unreachable?)" >&2
    return 0
  fi
  [[ -n "$rows" ]] || return 0

  # awk -v cannot carry a literal newline, so pass the name list space-separated.
  # Container names cannot contain spaces, so the round-trip is lossless.
  stray=$(printf '%s\n' "$rows" | awk -F'\t' -v want="$declared" -v names="$(printf '%s' "$expected" | tr '\n' ' ')" '
    BEGIN { n = split(names, a, " "); for (i = 1; i <= n; i++) if (a[i] != "") known[a[i]] = 1 }
    ($1 in known) && $2 != want {
      printf "    %-30s project=%-24s %s\n", $1, ($2 == "" ? "(none)" : $2), $3
    }') || return 0
  [[ -n "$stray" ]] || return 0

  # stderr, matching env_drift_warn. Deliberately NOT suppressed under --quiet,
  # which is where this differs from that function: --quiet asks for less
  # narration, and a warning that the tool's VIEW of the stack is wrong is the
  # one line that must survive it. Hiding it is the defect, one level up.
  {
    echo ""
    echo "  ⚠ COMPOSE PROJECT SPLIT — these containers belong to this stack by name"
    echo "    but carry a different compose project, so compose cannot see them:"
    printf '%s\n' "$stray"
    echo ""
    echo "    Why it matters: status under-reports, 'up' may try to create containers"
    echo "    whose names are already taken, and 'down' can report success while"
    echo "    leaving these running -- so the next 'up' collides again."
    echo "    Usual cause: created without '-f docker-compose.dev.yml', so compose"
    echo "    derived the project from the directory instead of name: ${declared}."
    echo "    A container from the 'tools' or 'services' profile can also appear"
    echo "    here; this script never creates those, so check before removing one."
    echo "    Fix, when no other session is mid-test:"
    echo "      docker rm -f <the names above>   # named volumes survive; data is kept"
    echo "      ./scripts/concord-dev.sh up"
  } >&2
}

# lsof_query() — preserve lsof's three useful outcomes: match, no match, and
# execution/probe failure. Returns 0 with output, 1 for a clean no-match, and 2
# when port ownership could not be determined.
lsof_query() {
  local output rc
  if output=$(lsof -w "$@" 2>/dev/null); then
    [[ -n "$output" ]] || return 2
    printf '%s\n' "$output"
    return 0
  else
    rc=$?
  fi
  [[ $rc -eq 1 && -z "$output" ]] && return 1
  return 2
}

# port_in_use() — return 0 for a TCP listener, 1 when free, 2 on probe failure.
port_in_use() {
  lsof_query -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null
}

# udp_port_in_use() — return 0 for a local UDP endpoint, 1 when free, 2 on
# probe failure. Parsing the local side avoids treating outbound traffic to
# that remote port as a conflict.
udp_port_in_use() {
  local port="$1"
  local output rc field value local_endpoint remote_endpoint saw_endpoint=0
  # An `lsof -Fn` endpoint is ADDR:PORT. PORT is numeric for a bound socket,
  # and `*` when the socket is bound to no port at all — the `*:*` shape
  # stock macOS daemons (identitys, sharingd) report. Both are real socket
  # states, so neither is a probe failure: a wildcard-port endpoint simply
  # is not the port being asked about, so the scan skips it and continues.
  # Requiring a numeric port made EVERY probe on macOS return 2, so the
  # port-verification step at the end of verb_down always failed: `down
  # --force` finished the teardown and then still exited 1, and
  # verb_freshstart — which treats a non-zero verb_down as "processes could
  # not be stopped safely" — refused to proceed to its wipe. ADDR is
  # deliberately not inspected: `*` and bracketed IPv6 are both normal there,
  # and `*:3478` must still count as port 3478 in use.
  #
  # Keep the `=~` right-hand side UNQUOTED at both use sites below. Quoting
  # it turns the match into a literal-string comparison that never fires,
  # which reinstates this exact bug in a far subtler form.
  local endpoint_re=':([0-9]+|\*)$'
  if output=$(lsof_query -nP -iUDP -Fn); then
    :
  else
    rc=$?
    [[ $rc -eq 1 ]] && return 1
    return 2
  fi

  while IFS= read -r field; do
    case "$field" in
      p*)
        value="${field#p}"
        [[ "$value" =~ ^[0-9]+$ ]] && [[ "$value" -gt 1 ]] || return 2
        ;;
      f*)
        [[ -n "${field#f}" ]] || return 2
        ;;
      n*)
        value="${field#n}"
        local_endpoint="${value%%->*}"
        [[ "$local_endpoint" =~ $endpoint_re ]] || return 2
        if [[ "$value" == *"->"* ]]; then
          remote_endpoint="${value#*->}"
          [[ "$remote_endpoint" =~ $endpoint_re ]] || return 2
        fi
        saw_endpoint=1
        [[ "$local_endpoint" == *":$port" ]] && return 0
        ;;
      *) return 2 ;;
    esac
  done <<< "$output"
  [[ $saw_endpoint -eq 1 ]] || return 2
  return 1
}

# wait_for_service() — block until URL returns HTTP 200, or timeout.
# Usage: wait_for_service "http://localhost:8080/health" "Control Plane"
# Returns: 0 if ready, 1 if timeout.
wait_for_service() {
  local url="$1"
  local name="$2"
  local max_attempts="${3:-30}"
  local attempt=1

  if [[ "$QUIET" -eq 0 ]]; then
    echo -ne "${YELLOW}⏳ Waiting for $name...${NC}"
  fi

  while [[ $attempt -le $max_attempts ]]; do
    if curl -sf "$url" >/dev/null 2>&1; then
      [[ "$QUIET" -eq 0 ]] && echo -e "\r${GREEN}✓${NC} $name is ready                    "
      return 0
    fi
    [[ "$QUIET" -eq 0 ]] && echo -ne "\r${YELLOW}⏳ Waiting for $name... ($attempt/$max_attempts)${NC}"
    sleep 1
    # Use $((var + 1)) form, NOT ((var++)) — post-increment returns the OLD
    # value as its expression result, so when var == 0 the return is 0 which
    # `set -e` treats as failure and exits the script.
    attempt=$((attempt + 1))
  done

  [[ "$QUIET" -eq 0 ]] && echo -e "\r${RED}✗${NC} $name failed to start (timeout)      "
  return 1
}

# read_valid_pid() — read + validate the PID from a PID file. Returns 0
# and prints the PID on stdout when valid (numeric, > 1). Returns 1 and
# prints nothing when invalid. Defends against corrupt PID files that
# could cause `kill` to target every user process (kill -1) or `ps -p`
# to produce confusing output.
# Usage: pid=$(read_valid_pid logs/control-plane.pid) || handle-invalid
read_valid_pid() {
  local pid_file="$1"
  [[ -f "$pid_file" ]] || return 1
  local pid
  pid=$(sed -n '1p' "$pid_file" 2>/dev/null)
  if [[ "$pid" =~ ^[0-9]+$ ]] && [[ "$pid" -gt 1 ]]; then
    printf '%s' "$pid"
    return 0
  fi
  return 1
}

# process_start_identity() — print a locale-stable start identity. Returns 1
# only when the PID is absent and 2 when ps itself could not answer safely.
process_start_identity() {
  local pid="$1"
  local output rc
  if output=$(LC_ALL=C ps -p "$pid" -o lstart= 2>/dev/null); then
    [[ -n "$output" ]] || return 2
    printf '%s\n' "$output"
    return 0
  else
    rc=$?
  fi
  # With a validated numeric PID and fixed options, ps uses 1 for both an
  # absent PID and a PID above the platform maximum; both are definitively not
  # the tracked process. Command/probe failures use another status.
  [[ $rc -eq 1 ]] && return 1
  return 2
}

process_cwd_identity() {
  local pid="$1"
  local output rc cwd extra
  if output=$(lsof_query -a -p "$pid" -d cwd -Fn); then
    :
  else
    rc=$?
    [[ $rc -eq 1 ]] && return 1
    return 2
  fi
  cwd=$(printf '%s\n' "$output" | sed -n 's/^n//p' | sed -n '1p')
  extra=$(printf '%s\n' "$output" | sed -n 's/^n//p' | sed -n '2p')
  [[ -n "$cwd" && -z "$extra" ]] || return 2
  cwd=$(cd "$cwd" 2>/dev/null && pwd -P) || return 2
  printf '%s\n' "$cwd"
}

process_command_identity() {
  local pid="$1"
  local output rc
  if output=$(LC_ALL=C ps -ww -p "$pid" -o command= 2>/dev/null); then
    [[ -n "$output" ]] || return 2
    printf '%s\n' "$output"
    return 0
  else
    rc=$?
  fi
  [[ $rc -eq 1 ]] && return 1
  return 2
}

# process_identity_state() — 0 is the same process, 1 is definitively absent,
# and 2 is unverified/reused/unauthorized. Callers must never signal state 2.
process_identity_state() {
  local pid="$1"
  local expected_start="$2"
  local expected_cwd="$3"
  local current_start current_cwd rc

  if current_start=$(process_start_identity "$pid"); then
    :
  else
    rc=$?
    [[ $rc -eq 1 ]] && return 1
    return 2
  fi
  [[ "$current_start" == "$expected_start" ]] || return 2

  if current_cwd=$(process_cwd_identity "$pid"); then
    :
  else
    rc=$?
    [[ $rc -eq 1 ]] && return 1
    return 2
  fi
  [[ "$current_cwd" == "$expected_cwd" ]] || return 2
  return 0
}

write_pid_record_values() {
  local pid_file="$1"
  local pid="$2"
  local start="$3"
  local canonical_cwd="$4"
  local tmp_file
  tmp_file="$pid_file.tmp.$$"
  if ! (umask 077; printf '%s\n%s\n%s\n' "$pid" "$start" "$canonical_cwd" > "$tmp_file"); then
    rm -f "$tmp_file"
    return 1
  fi
  if ! mv -f "$tmp_file" "$pid_file"; then
    rm -f "$tmp_file"
    return 1
  fi
  return 0
}

wait_for_process_identity_exit() {
  local pid="$1"
  local expected_start="$2"
  local expected_cwd="$3"
  local count=0 rc
  while [[ $count -lt 10 ]]; do
    if process_identity_state "$pid" "$expected_start" "$expected_cwd"; then
      sleep 1
      count=$((count + 1))
      continue
    else
      rc=$?
    fi
    [[ $rc -eq 1 ]] && return 0
    return 2
  done
  return 1
}

cleanup_started_process() {
  local pid="$1"
  local expected_start="$2"
  local expected_cwd="$3"
  local rc
  if process_identity_state "$pid" "$expected_start" "$expected_cwd"; then
    :
  else
    rc=$?
    [[ $rc -eq 1 ]] && return 0
    return 1
  fi
  if ! kill -9 "$pid" 2>/dev/null; then
    if process_identity_state "$pid" "$expected_start" "$expected_cwd"; then
      return 1
    else
      rc=$?
    fi
    [[ $rc -eq 1 ]] && return 0
    return 1
  fi
  wait_for_process_identity_exit "$pid" "$expected_start" "$expected_cwd"
}

cleanup_started_job() {
  local pid="$1"
  local job_pid
  while IFS= read -r job_pid; do
    [[ "$job_pid" == "$pid" ]] || continue
    kill -9 "$pid" 2>/dev/null || return 1
    wait "$pid" 2>/dev/null || true
    return 0
  done < <(jobs -pr)
  wait "$pid" 2>/dev/null || true
  return 0
}

track_started_process() {
  local pid_file="$1"
  local pid="$2"
  local expected_cwd="$3"
  local name="$4"
  local start canonical_cwd
  start=$(process_start_identity "$pid") || {
    if ! cleanup_started_job "$pid"; then
      echo -e "${RED}✗${NC} Could not clean up unidentified $name process (PID: $pid)" >&2
    fi
    echo -e "${RED}✗${NC} Could not capture $name process identity" >&2
    return 1
  }
  canonical_cwd=$(cd "$expected_cwd" 2>/dev/null && pwd -P) || {
    if ! cleanup_started_job "$pid"; then
      echo -e "${RED}✗${NC} Could not clean up unidentified $name process (PID: $pid)" >&2
    fi
    echo -e "${RED}✗${NC} Could not canonicalize $name working directory" >&2
    return 1
  }
  if write_pid_record_values "$pid_file" "$pid" "$start" "$canonical_cwd"; then
    return 0
  fi
  if ! cleanup_started_process "$pid" "$start" "$canonical_cwd"; then
    echo -e "${RED}✗${NC} Could not safely clean up untracked $name process (PID: $pid)" >&2
  fi
  return 1
}

remove_pid_record() {
  local pid_file="$1"
  local name="$2"
  if rm -f "$pid_file"; then
    return 0
  fi
  echo -e "${RED}✗${NC} Could not remove $name PID file: $pid_file" >&2
  return 1
}

tracked_pid_identity_state() {
  local pid_file="$1"
  local pid="$2"
  local expected_start expected_cwd extra
  expected_start=$(sed -n '2p' "$pid_file" 2>/dev/null)
  expected_cwd=$(sed -n '3p' "$pid_file" 2>/dev/null)
  extra=$(sed -n '4p' "$pid_file" 2>/dev/null)
  [[ -n "$expected_start" && "$expected_cwd" == /* && -z "$extra" ]] || return 2
  process_identity_state "$pid" "$expected_start" "$expected_cwd"
}

pid_record_matches() {
  local pid_file="$1"
  local pid="$2"
  local expected_start="$3"
  local expected_cwd="$4"
  local stored_pid stored_start stored_cwd extra
  stored_pid=$(sed -n '1p' "$pid_file" 2>/dev/null)
  stored_start=$(sed -n '2p' "$pid_file" 2>/dev/null)
  stored_cwd=$(sed -n '3p' "$pid_file" 2>/dev/null)
  extra=$(sed -n '4p' "$pid_file" 2>/dev/null)
  [[ "$stored_pid" == "$pid" && "$stored_start" == "$expected_start" && \
    "$stored_cwd" == "$expected_cwd" && -z "$extra" ]]
}

finish_tracked_stop() {
  local pid_file="$1"
  local name="$2"
  local pid="$3"
  local expected_start="$4"
  local expected_cwd="$5"
  if ! pid_record_matches "$pid_file" "$pid" "$expected_start" "$expected_cwd"; then
    echo -e "${RED}✗${NC} $name PID record changed during shutdown — preserving it" >&2
    return 1
  fi
  remove_pid_record "$pid_file" "$name" || return 1
  [[ "$QUIET" -eq 0 ]] && echo -e "\r${GREEN}✓${NC} $name stopped                              "
  return 0
}

# stop_process() — kill PID from file, escalate TERM → KILL if needed.
# Usage: stop_process logs/control-plane.pid "Control Plane"
#        stop_process logs/control-plane.pid "Control Plane" --force
stop_process() {
  local pid_file="$1"
  local name="$2"
  local force_kill=0
  [[ "${3:-}" == "--force" ]] && force_kill=1

  if [[ ! -f "$pid_file" ]]; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}⚠${NC} $name PID file not found"
    return 0
  fi

  local pid
  if ! pid=$(read_valid_pid "$pid_file"); then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}⚠${NC} $name PID file contains invalid value — removing"
    remove_pid_record "$pid_file" "$name"
    return
  fi
  local expected_start expected_cwd extra identity_rc
  expected_start=$(sed -n '2p' "$pid_file" 2>/dev/null)
  expected_cwd=$(sed -n '3p' "$pid_file" 2>/dev/null)
  extra=$(sed -n '4p' "$pid_file" 2>/dev/null)
  if [[ -z "$expected_start" && -z "$expected_cwd" && -z "$extra" ]]; then
    if process_start_identity "$pid" >/dev/null; then
      echo -e "${RED}✗${NC} $name PID file is not identity-bound — refusing to signal PID $pid" >&2
      return 3
    else
      identity_rc=$?
      if [[ $identity_rc -eq 1 ]]; then
        [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}⚠${NC} $name was not running (stale PID file)"
        remove_pid_record "$pid_file" "$name"
        return
      fi
      echo -e "${RED}✗${NC} Could not verify $name PID $pid" >&2
      return 1
    fi
  fi

  if tracked_pid_identity_state "$pid_file" "$pid"; then
    :
  else
    identity_rc=$?
    if [[ $identity_rc -eq 1 ]]; then
      [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}⚠${NC} $name was not running (stale PID file)"
      remove_pid_record "$pid_file" "$name"
      return
    fi
    echo -e "${RED}✗${NC} $name PID identity could not be verified — refusing to signal PID $pid" >&2
    return 1
  fi

  [[ "$QUIET" -eq 0 ]] && echo -ne "${YELLOW}⏳ Stopping $name (PID: $pid)...${NC}"
  if [[ "$force_kill" -eq 1 ]]; then
    if ! kill -9 "$pid" 2>/dev/null; then
      if process_identity_state "$pid" "$expected_start" "$expected_cwd"; then
        :
      else
        identity_rc=$?
        [[ $identity_rc -eq 1 ]] && { finish_tracked_stop "$pid_file" "$name" "$pid" "$expected_start" "$expected_cwd"; return; }
      fi
      echo -e "${RED}✗${NC} Failed to stop $name (PID: $pid)" >&2
      return 1
    fi
  else
    if ! kill -TERM "$pid" 2>/dev/null; then
      if process_identity_state "$pid" "$expected_start" "$expected_cwd"; then
        :
      else
        identity_rc=$?
        [[ $identity_rc -eq 1 ]] && { finish_tracked_stop "$pid_file" "$name" "$pid" "$expected_start" "$expected_cwd"; return; }
      fi
      echo -e "${RED}✗${NC} Failed to stop $name (PID: $pid)" >&2
      return 1
    fi
    if wait_for_process_identity_exit "$pid" "$expected_start" "$expected_cwd"; then
      finish_tracked_stop "$pid_file" "$name" "$pid" "$expected_start" "$expected_cwd"
      return
    else
      identity_rc=$?
      if [[ $identity_rc -eq 2 ]]; then
        echo -e "${RED}✗${NC} $name identity changed during shutdown — refusing escalation" >&2
        return 1
      fi
    fi
    [[ "$QUIET" -eq 0 ]] && echo -e "\r${YELLOW}⚠${NC} $name didn't stop gracefully, force killing..."
    if process_identity_state "$pid" "$expected_start" "$expected_cwd"; then
      :
    else
      identity_rc=$?
      if [[ $identity_rc -eq 1 ]]; then
        finish_tracked_stop "$pid_file" "$name" "$pid" "$expected_start" "$expected_cwd"
        return
      fi
      echo -e "${RED}✗${NC} $name identity changed before escalation — refusing SIGKILL" >&2
      return 1
    fi
    if ! kill -9 "$pid" 2>/dev/null; then
      if process_identity_state "$pid" "$expected_start" "$expected_cwd"; then
        :
      else
        identity_rc=$?
        [[ $identity_rc -eq 1 ]] && { finish_tracked_stop "$pid_file" "$name" "$pid" "$expected_start" "$expected_cwd"; return; }
      fi
      echo -e "${RED}✗${NC} Failed to force stop $name (PID: $pid)" >&2
      return 1
    fi
  fi

  if ! wait_for_process_identity_exit "$pid" "$expected_start" "$expected_cwd"; then
    echo -e "${RED}✗${NC} Could not confirm $name stopped; preserving $pid_file" >&2
    return 1
  fi
  finish_tracked_stop "$pid_file" "$name" "$pid" "$expected_start" "$expected_cwd"
}

# Confirm that a root is registered by this repository, not merely pointed at
# its Git metadata by an arbitrary .git indirection.
canonical_git_common_dir() {
  local root="$1"
  local common
  common=$(env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR \
    git -C "$root" rev-parse --git-common-dir 2>/dev/null) || return 1
  if [[ "$common" != /* ]]; then
    common="$root/$common"
  fi
  common=$(cd "$common" 2>/dev/null && pwd -P) || return 1
  printf '%s\n' "$common"
}

registered_worktree_contains() {
  local expected_root="$1"
  local output line root="" saw_worktree=0 prunable=0 matched=0
  local trusted_common expected_common
  if output=$(env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR \
    git -C "$PROJECT_ROOT" worktree list --porcelain 2>&1); then
    :
  else
    return 2
  fi
  while IFS= read -r line; do
    case "$line" in
      worktree\ *)
        [[ "$root" == "$expected_root" && $prunable -eq 0 ]] && matched=1
        root="${line#worktree }"
        root=$(cd "$root" 2>/dev/null && pwd -P) || root=""
        saw_worktree=1
        prunable=0
        ;;
      prunable*)
        prunable=1
        ;;
      '')
        [[ "$root" == "$expected_root" && $prunable -eq 0 ]] && matched=1
        root=""
        prunable=0
        ;;
    esac
  done <<< "$output"
  [[ $saw_worktree -eq 1 ]] || return 2
  [[ "$root" == "$expected_root" && $prunable -eq 0 ]] && matched=1
  [[ $matched -eq 1 ]] || return 1

  trusted_common=$(canonical_git_common_dir "$PROJECT_ROOT") || return 2
  expected_common=$(canonical_git_common_dir "$expected_root") || return 2
  [[ "$trusted_common" == "$expected_common" ]]
}

listener_pid_matches() {
  local pid="$1"
  local port="$2"
  local output rc candidate
  if output=$(lsof_query -a -p "$pid" -iTCP:"$port" -sTCP:LISTEN -t); then
    :
  else
    rc=$?
    [[ $rc -eq 1 ]] && return 1
    return 2
  fi
  while IFS= read -r candidate; do
    [[ "$candidate" =~ ^[0-9]+$ ]] || return 2
    [[ "$candidate" == "$pid" ]] && return 0
  done <<< "$output"
  return 2
}

repo_listener_identity_state() {
  local pid="$1"
  local port="$2"
  local expected_cwd="$3"
  local expected_start="$4"
  local expected_root="$5"
  local expected_command="${6:-}"
  local current_command rc
  if process_identity_state "$pid" "$expected_start" "$expected_cwd"; then
    :
  else
    rc=$?
    [[ $rc -eq 1 ]] && return 1
    return 2
  fi
  registered_worktree_contains "$expected_root" || return 2
  if [[ -n "$expected_command" ]]; then
    if current_command=$(process_command_identity "$pid"); then
      [[ "$current_command" == "$expected_command" ]] || return 2
    else
      rc=$?
      [[ $rc -eq 1 ]] && return 1
      return 2
    fi
  fi
  if listener_pid_matches "$pid" "$port"; then
    return 0
  else
    rc=$?
  fi
  [[ $rc -eq 1 ]] && return 1
  return 2
}

repo_listener_absent() {
  local rc
  if repo_listener_identity_state "$@"; then
    return 1
  else
    rc=$?
  fi
  [[ $rc -eq 1 ]]
}

tcp_listener_pids() {
  local port="$1"
  local output rc pid
  if output=$(lsof_query -tiTCP:"$port" -sTCP:LISTEN); then
    :
  else
    rc=$?
    [[ $rc -eq 1 ]] && return 1
    return 2
  fi
  while IFS= read -r pid; do
    [[ "$pid" =~ ^[0-9]+$ ]] && [[ "$pid" -gt 1 ]] || return 2
    printf '%s\n' "$pid"
  done <<< "$output"
}

bind_legacy_direct_listener() {
  local pid_file="$1"
  local port="$2"
  local service_dir="$3"
  local owner_pid owner_start owner_cwd expected_cwd project_root
  local listener_pids listener_pid

  owner_pid=$(read_valid_pid "$pid_file") || return 1
  [[ -z "$(sed -n '2p' "$pid_file" 2>/dev/null)" && \
    -z "$(sed -n '3p' "$pid_file" 2>/dev/null)" && \
    -z "$(sed -n '4p' "$pid_file" 2>/dev/null)" ]] || return 1
  expected_cwd=$(cd "$PROJECT_ROOT/$service_dir" 2>/dev/null && pwd -P) || return 1
  project_root=$(cd "$PROJECT_ROOT" 2>/dev/null && pwd -P) || return 1
  owner_start=$(process_start_identity "$owner_pid") || return 1
  owner_cwd=$(process_cwd_identity "$owner_pid") || return 1
  [[ "$owner_cwd" == "$expected_cwd" ]] || return 1

  listener_pids=$(tcp_listener_pids "$port") || return 1
  while IFS= read -r listener_pid; do
    [[ "$listener_pid" == "$owner_pid" ]] || continue
    process_identity_state "$owner_pid" "$owner_start" "$owner_cwd" || return 1
    repo_listener_identity_state "$owner_pid" "$port" "$expected_cwd" \
      "$owner_start" "$project_root" || return 1
    [[ "$(sed -n '1p' "$pid_file" 2>/dev/null)" == "$owner_pid" && \
      -z "$(sed -n '2p' "$pid_file" 2>/dev/null)" && \
      -z "$(sed -n '3p' "$pid_file" 2>/dev/null)" && \
      -z "$(sed -n '4p' "$pid_file" 2>/dev/null)" ]] || return 1
    write_pid_record_values "$pid_file" "$owner_pid" "$owner_start" "$owner_cwd" || return 1
    return 0
  done <<< "$listener_pids"
  return 1
}

wait_for_repo_listener_exit() {
  local pid="$1"
  local port="$2"
  local expected_cwd="$3"
  local expected_start="$4"
  local expected_root="$5"
  local expected_command="${6:-}"
  local count=0 rc
  while [[ $count -lt 10 ]]; do
    if repo_listener_identity_state "$pid" "$port" "$expected_cwd" "$expected_start" "$expected_root" "$expected_command"; then
      sleep 1
      count=$((count + 1))
      continue
    else
      rc=$?
    fi
    [[ $rc -eq 1 ]] && return 0
    return 2
  done
  return 1
}

listener_command_matches_service() {
  local command="$1"
  local service_dir="$2"
  case "$service_dir" in
    services/control-plane) [[ "$command" == *"bin/control-plane"* ]] ;;
    services/media-plane) [[ "$command" == *"tsx"* && "$command" == *"src/index.ts"* ]] ;;
    client/desktop) [[ "$command" == *"vite"* && "$command" == *"--port 3001"* ]] ;;
    *) return 1 ;;
  esac
}

listener_cleanup_error() {
  local name="$1"
  local pid="$2"
  local port="$3"
  local reason="$4"
  echo -e "${RED}✗${NC} Could not verify $name listener PID $pid on port $port: $reason" >&2
}

# Recover a native service whose tracked watcher exited before its listening
# child. Plain down may TERM only this worktree's listener; --force may KILL a
# listener in a sibling worktree sharing this repository.
stop_repo_listener() {
  local port="$1"
  local name="$2"
  local service_dir="$3"
  local force_kill=0
  [[ "${4:-}" == "--force" ]] && force_kill=1

  local project_root listener_pids rc cleanup_failed=0
  project_root=$(cd "$PROJECT_ROOT" 2>/dev/null && pwd -P) || return 1
  if listener_pids=$(tcp_listener_pids "$port"); then
    :
  else
    rc=$?
    [[ $rc -eq 1 ]] && return 0
    return 1
  fi

  local pid cwd process_root process_start process_command=""
  while IFS= read -r pid; do
    if process_start=$(process_start_identity "$pid"); then
      :
    else
      rc=$?
      if [[ $rc -eq 2 ]]; then
        listener_cleanup_error "$name" "$pid" "$port" "start identity unavailable"
        cleanup_failed=1
      fi
      continue
    fi
    if cwd=$(process_cwd_identity "$pid"); then
      :
    else
      rc=$?
      [[ $rc -eq 1 ]] && continue
      listener_cleanup_error "$name" "$pid" "$port" "working directory unavailable"
      cleanup_failed=1
      continue
    fi
    if ! process_root=$(env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR \
      git -C "$cwd" rev-parse --show-toplevel 2>/dev/null); then
      listener_cleanup_error "$name" "$pid" "$port" "working directory is not in a Git worktree"
      cleanup_failed=1
      continue
    fi
    if ! process_root=$(cd "$process_root" 2>/dev/null && pwd -P); then
      listener_cleanup_error "$name" "$pid" "$port" "worktree root could not be canonicalized"
      cleanup_failed=1
      continue
    fi
    if [[ "$cwd" != "$process_root/$service_dir" ]]; then
      listener_cleanup_error "$name" "$pid" "$port" "working directory does not match $service_dir"
      cleanup_failed=1
      continue
    fi
    if [[ "$force_kill" -ne 1 && "$process_root" != "$project_root" ]]; then
      listener_cleanup_error "$name" "$pid" "$port" "listener belongs to another worktree"
      cleanup_failed=1
      continue
    fi
    process_command=""
    if [[ "$force_kill" -ne 1 ]]; then
      if process_command=$(process_command_identity "$pid"); then
        :
      else
        rc=$?
        [[ $rc -eq 1 ]] && continue
        listener_cleanup_error "$name" "$pid" "$port" "command identity unavailable"
        cleanup_failed=1
        continue
      fi
      if ! listener_command_matches_service "$process_command" "$service_dir"; then
        listener_cleanup_error "$name" "$pid" "$port" "command does not match the expected service"
        cleanup_failed=1
        continue
      fi
    fi
    if repo_listener_identity_state "$pid" "$port" "$cwd" "$process_start" "$process_root" "$process_command"; then
      :
    else
      rc=$?
      [[ $rc -eq 1 ]] && continue
      listener_cleanup_error "$name" "$pid" "$port" "identity changed during validation"
      cleanup_failed=1
      continue
    fi

    if [[ "$force_kill" -eq 1 ]]; then
      [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}⚠${NC} Force stopping orphaned $name listener (PID: $pid)"
      if ! kill -9 "$pid" 2>/dev/null; then
        if repo_listener_absent "$pid" "$port" "$cwd" "$process_start" "$process_root" "$process_command"; then
          continue
        fi
        listener_cleanup_error "$name" "$pid" "$port" "SIGKILL failed"
        cleanup_failed=1
        continue
      fi
    else
      [[ "$QUIET" -eq 0 ]] && echo -ne "${YELLOW}⏳ Stopping orphaned $name listener (PID: $pid)...${NC}"
      if ! kill -TERM "$pid" 2>/dev/null; then
        if repo_listener_absent "$pid" "$port" "$cwd" "$process_start" "$process_root" "$process_command"; then
          continue
        fi
        listener_cleanup_error "$name" "$pid" "$port" "SIGTERM failed"
        cleanup_failed=1
        continue
      fi
      if wait_for_repo_listener_exit "$pid" "$port" "$cwd" "$process_start" "$process_root" "$process_command"; then
        [[ "$QUIET" -eq 0 ]] && echo -e "\r${GREEN}✓${NC} $name listener stopped                         "
        continue
      else
        rc=$?
        if [[ $rc -eq 2 ]]; then
          listener_cleanup_error "$name" "$pid" "$port" "identity changed after SIGTERM"
          cleanup_failed=1
          continue
        fi
      fi
      [[ "$QUIET" -eq 0 ]] && echo -e "\r${YELLOW}⚠${NC} $name listener didn't stop gracefully, force killing..."
      if repo_listener_identity_state "$pid" "$port" "$cwd" "$process_start" "$process_root" "$process_command"; then
        :
      else
        rc=$?
        [[ $rc -eq 1 ]] && continue
        listener_cleanup_error "$name" "$pid" "$port" "identity changed before escalation"
        cleanup_failed=1
        continue
      fi
      if ! kill -9 "$pid" 2>/dev/null; then
        if repo_listener_absent "$pid" "$port" "$cwd" "$process_start" "$process_root" "$process_command"; then
          continue
        fi
        listener_cleanup_error "$name" "$pid" "$port" "SIGKILL failed"
        cleanup_failed=1
        continue
      fi
    fi

    if wait_for_repo_listener_exit "$pid" "$port" "$cwd" "$process_start" "$process_root" "$process_command"; then
      [[ "$QUIET" -eq 0 ]] && echo -e "\r${GREEN}✓${NC} $name listener stopped                         "
    else
      listener_cleanup_error "$name" "$pid" "$port" "process remained after shutdown"
      cleanup_failed=1
    fi
  done <<< "$listener_pids"
  [[ $cleanup_failed -eq 0 ]]
}

stop_native_service() {
  local pid_file="$1"
  local name="$2"
  local port="$3"
  local service_dir="$4"
  local force_flag="$5"
  local process_rc=0 listener_rc=0

  if stop_process "$pid_file" "$name" "$force_flag"; then
    :
  else
    process_rc=$?
  fi
  if [[ $process_rc -eq 3 && "$force_flag" == "--force" ]] && \
    bind_legacy_direct_listener "$pid_file" "$port" "$service_dir"; then
    if stop_process "$pid_file" "$name" "$force_flag"; then
      process_rc=0
    else
      process_rc=$?
    fi
  fi
  stop_repo_listener "$port" "$name" "$service_dir" "$force_flag" || listener_rc=$?

  # Listener cleanup may also finish a tracked watcher that exited through its
  # child. Reconcile once; stop_process revalidates identity and remains
  # fail-closed for live legacy, reused, or unverified PIDs.
  if [[ $process_rc -ne 0 && $listener_rc -eq 0 ]]; then
    if stop_process "$pid_file" "$name" "$force_flag"; then
      process_rc=0
    else
      process_rc=$?
    fi
  fi
  if [[ $process_rc -eq 3 ]]; then
    echo -e "${RED}✗${NC} $name has a live legacy watcher PID; verify and stop it manually" >&2
  fi
  [[ $process_rc -eq 0 && $listener_rc -eq 0 ]]
}

# pre_flight_checks() — verify environment before starting services.
# Exits with code 2 on failure (pre-flight class).
pre_flight_checks() {
  local errors=0

  # Check docker daemon
  if ! docker info >/dev/null 2>&1; then
    echo -e "${RED}✗${NC} Docker daemon not running. Start Docker Desktop." >&2
    errors=$((errors + 1))
  fi

  # Check ports
  for port in 8080 3000; do
    if port_in_use "$port"; then
      echo -e "${RED}✗${NC} Port $port already in use. Run 'concord-dev.sh down' first." >&2
      errors=$((errors + 1))
    else
      local probe_rc=$?
      if [[ $probe_rc -eq 2 ]]; then
        echo -e "${RED}✗${NC} Could not inspect port $port with lsof." >&2
        errors=$((errors + 1))
      fi
    fi
  done

  # Check .env or .env.example
  if [[ ! -f "$PROJECT_ROOT/.env" ]]; then
    if [[ ! -f "$PROJECT_ROOT/.env.example" ]]; then
      echo -e "${RED}✗${NC} No .env or .env.example found. Cannot bootstrap." >&2
      errors=$((errors + 1))
    fi
  fi

  if [[ $errors -gt 0 ]]; then
    exit 2
  fi
}

# bootstrap_env() — copy .env.example → .env if .env missing; otherwise warn
# (non-fatally) about keys an existing .env lacks vs .env.example.
bootstrap_env() {
  local env="$PROJECT_ROOT/.env" example="$PROJECT_ROOT/.env.example"
  if [[ ! -f "$env" && -f "$example" ]]; then
    cp "$example" "$env"
    chmod 600 "$env"
    [[ "$QUIET" -eq 0 ]] && echo -e "${GREEN}✓${NC} Created .env from .env.example"
    return 0
  fi
  # Existing .env: surface keys it's missing vs .env.example before they fail at `up`.
  env_drift_warn "$env" "$example"
}

# env_drift_warn() — print a non-fatal WARNING listing keys that .env.example
# defines but the given .env is missing. Compares KEY NAMES only — never reads
# or prints values (secret hygiene, per [internal]rules/observability.md). Catches
# the case where a months-old .env predates a newly-required compose var (the
# PUBLIC_IP papercut), which otherwise surfaces only as a cryptic compose
# interpolation error at `up`. Suppressed under --quiet.
# Usage: env_drift_warn <env-path> <example-path>
env_drift_warn() {
  local env="$1" example="$2"
  [[ "$QUIET" -eq 1 ]] && return 0
  [[ -f "$env" && -f "$example" ]] || return 0
  # Match ONLY plain column-0 `KEY=` lines — exactly what Docker Compose's
  # env-file parser reads (it does NOT honor an `export ` prefix or leading
  # whitespace). Keeping the check column-0-only keeps it aligned with the actual
  # consumer: an `export KEY=` / indented key in .env is unreadable by compose,
  # so it is correctly treated as ABSENT and still warns. Tolerating those forms
  # would make this checker more permissive than compose and mask the exact
  # interpolation failure it exists to catch. Values never match `^...=`, so no
  # secret value reaches the diff. (Gitar #1550 — do NOT loosen to accept
  # export/indented keys; that misaligns the check from compose.)
  local missing
  missing="$(comm -23 \
    <(grep -oE '^[A-Za-z_][A-Za-z0-9_]*=' "$example" | sed 's/=$//' | sort -u) \
    <(grep -oE '^[A-Za-z_][A-Za-z0-9_]*=' "$env"     | sed 's/=$//' | sort -u) 2>/dev/null || true)"
  [[ -n "$missing" ]] || return 0
  {
    echo -e "${YELLOW}⚠${NC}  Your .env is missing keys that .env.example defines:"
    # Intentional word split: print one key per line.
    # shellcheck disable=SC2086
    printf '     %s\n' $missing
    echo "    Add them from .env.example before 'up' — a required one (e.g. PUBLIC_IP)"
    echo "    otherwise fails with a cryptic compose interpolation error, not this hint."
  } >&2
  return 0
}

# ── Verbs ──────────────────────────────────────────────────────────────

verb_up() {
  local skip_client=0
  local no_rebuild=0
  local num_clients=1
  local reset_db=0

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --skip-client) skip_client=1; shift ;;
      --no-rebuild)  no_rebuild=1; shift ;;
      --clients)
        if [[ $# -lt 2 ]]; then
          echo "verb_up: --clients requires a number" >&2
          return 2
        fi
        if ! [[ "$2" =~ ^[0-9]+$ ]]; then
          echo "verb_up: --clients must be a positive integer, got '$2'" >&2
          return 2
        fi
        num_clients="$2"; shift 2 ;;
      --reset-db)    reset_db=1; shift ;;
      *)
        echo "verb_up: unknown arg '$1'" >&2
        return 2
        ;;
    esac
  done

  pre_flight_checks
  bootstrap_env
  mkdir -p "$LOG_DIR"

  _up_infra "$reset_db"
  _up_control_plane "$no_rebuild"
  _up_media_plane "$no_rebuild"
  if [[ "$skip_client" -eq 0 ]]; then
    _up_clients "$no_rebuild" "$num_clients"
  fi
  _up_summary "$skip_client" "$num_clients"
}

_up_infra() {
  local reset_db="$1"

  [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}📦 Starting infrastructure (5 containers via base + dev overlay)${NC}"

  if [[ "$reset_db" -eq 1 ]]; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}🗑  Resetting DB volumes (--reset-db)${NC}"
    compose down -v >/dev/null 2>&1 || true
    rm -rf "$PROJECT_ROOT/.dev-data" 2>/dev/null || true
  fi

  # A split project makes the next line create containers whose names are already
  # taken, which compose reports as a name conflict after it has begun creating
  # the rest. Say so BEFORE that happens rather than after it half-applies.
  warn_compose_project_split

  # Default compose profile = 5 infra services (postgres redis nats coturn minio).
  compose up -d >/dev/null

  # Wait up to 30s for all healthchecks to flip green.
  # Base compose has 4 healthcheck-bearing services (postgres/redis/nats/coturn);
  # MinIO has no healthcheck because the base image lacks curl/wget/mc (see
  # docker-compose.yml:128). MinIO readiness is verified by control-plane's own
  # 15s MinIO-client init timeout at startup. So we wait for >= 4 healthy here.
  local waited=0
  local healthy=0
  while [[ $waited -lt 30 ]]; do
    healthy=$(compose ps --format json 2>/dev/null | grep -c '"Health":"healthy"' || true)
    if [[ $healthy -ge 4 ]]; then
      [[ "$QUIET" -eq 0 ]] && echo -e "${GREEN}✓${NC} All infrastructure healthchecks green (4 services + minio)"
      return 0
    fi
    sleep 1
    waited=$((waited + 1))
  done

  echo -e "${RED}✗${NC} Infrastructure healthcheck timeout. Run '$0 status' to investigate." >&2
  exit 3
}

_up_control_plane() {
  local no_rebuild="$1"

  [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}🎛  Starting Control Plane (Go)${NC}"

  cd "$PROJECT_ROOT/services/control-plane"
  if [[ "$no_rebuild" -eq 0 || ! -f bin/control-plane ]]; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}🔧 Building control-plane binary${NC}"
    go mod download
    mkdir -p bin
    go build -o bin/control-plane ./cmd/server
  fi

  nohup ./bin/control-plane > "$LOG_DIR/control-plane.log" 2>&1 &
  local control_pid=$!
  if ! track_started_process "$LOG_DIR/control-plane.pid" "$control_pid" \
    "$PROJECT_ROOT/services/control-plane" "Control Plane"; then
    echo -e "${RED}✗${NC} Could not record Control Plane process identity" >&2
    return 1
  fi
  cd "$PROJECT_ROOT"

  if ! wait_for_service "http://localhost:8080/health" "Control Plane"; then
    echo -e "${RED}✗${NC} Control Plane startup failed. Last 20 lines of log:" >&2
    tail -20 "$LOG_DIR/control-plane.log" >&2
    exit 3
  fi
}

_up_media_plane() {
  local no_rebuild="$1"

  [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}🎙  Starting Media Plane (Node.js)${NC}"

  cd "$PROJECT_ROOT/services/media-plane"
  # Install if --no-rebuild was NOT passed, OR if node_modules is missing
  # (fresh-checkout case — CI smoke + first-time developer).
  if [[ "$no_rebuild" -eq 0 || ! -d node_modules ]]; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}🔧 npm install (media-plane, two-pass)${NC}"
    # Two-pass install defeats the mediasoup-postinstall ordering race
    # documented in `[internal]rules/media-plane.md` "Docker Build Context
    # Invariant" section.
    #
    # On a fresh checkout with no `node_modules/`, the standard `npm install`
    # runs postinstall scripts concurrently with dependency installation, so
    # mediasoup's postinstall (`node npm-scripts.mjs postinstall`) can fire
    # BEFORE npm has wired the `node-domexception` file:./stubs override
    # symlink. mediasoup's hook then imports something that pulls fetch-blob,
    # which imports node-domexception, and ERR_MODULE_NOT_FOUND aborts the
    # whole install. Locally-warm caches mask this because the symlink
    # persists across runs.
    #
    # Pass 1 (`--ignore-scripts`): resolve all deps + create override
    # symlinks, defer ALL lifecycle scripts.
    # Pass 2 (`npm rebuild`): re-run install hooks in a context where every
    # override target is already in place.
    #
    # Cost: ~3-5s extra on fresh `rm -rf node_modules`; zero cost on warm
    # cache (both passes short-circuit when deps + symlinks are current).
    npm install --ignore-scripts >/dev/null
    npm rebuild >/dev/null
  fi

  # Use npx tsx directly (not via npm run) so the PID file tracks tsx, not npm.
  nohup npx tsx watch src/index.ts > "$LOG_DIR/media-plane.log" 2>&1 &
  local media_pid=$!
  if ! track_started_process "$LOG_DIR/media-plane.pid" "$media_pid" \
    "$PROJECT_ROOT/services/media-plane" "Media Plane"; then
    echo -e "${RED}✗${NC} Could not record Media Plane process identity" >&2
    return 1
  fi
  cd "$PROJECT_ROOT"

  if ! wait_for_service "http://localhost:3000/health" "Media Plane"; then
    echo -e "${RED}✗${NC} Media Plane startup failed. Last 20 lines of log:" >&2
    tail -20 "$LOG_DIR/media-plane.log" >&2
    exit 3
  fi
}

_up_clients() {
  local no_rebuild="$1"
  local num_clients="$2"

  [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}🖥  Starting Vite dev server + ${num_clients} Electron client(s)${NC}"

  cd "$PROJECT_ROOT/client/desktop"
  # See _up_media_plane note — install when missing node_modules even with --no-rebuild.
  # A tree with no node_modules is where dependency RESOLUTION actually happens, so
  # that path is lock-exact (`npm ci`). An existing tree keeps `npm install` so the
  # normal dev loop does not pay for a full wipe-and-reinstall on every `up`.
  if [[ ! -d node_modules ]]; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}🔧 npm ci (client/desktop)${NC}"
    npm ci >/dev/null
  elif [[ "$no_rebuild" -eq 0 ]]; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}🔧 npm install (client/desktop)${NC}"
    npm install >/dev/null
  fi

  # The native addon is a BUILD ARTEFACT, not a checked-in binary, so a fresh
  # clone has no concord_audiocap.node at all and the audiocap host reports
  # `load-fault` -- which reads as a packaging defect and is not one. Gated
  # exactly like npm install above: skipped under --no-rebuild UNLESS the
  # artefact is actually missing, because "I asked you not to rebuild" cannot
  # sensibly mean "leave the client unable to start".
  if [[ "$no_rebuild" -eq 0 || ! -f native/concord-audiocap/build/Release/concord_audiocap.node ]]; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${YELLOW}🔧 npm run build:native (client/desktop)${NC}"
    npm run build:native >/dev/null
  fi

  # Always rebuild preload + main (Electron requires fresh JS).
  npm run build:preload >/dev/null
  npx tsc -p tsconfig.main.json >/dev/null

  nohup npx vite --port 3001 --strictPort > "$LOG_DIR/vite-renderer.log" 2>&1 &
  local vite_pid=$!
  if ! track_started_process "$LOG_DIR/vite-renderer.pid" "$vite_pid" \
    "$PROJECT_ROOT/client/desktop" "Vite Dev Server"; then
    echo -e "${RED}✗${NC} Could not record Vite process identity" >&2
    return 1
  fi

  if ! wait_for_service "http://localhost:3001" "Vite Dev Server"; then
    echo -e "${RED}✗${NC} Vite startup failed" >&2
    exit 3
  fi

  # Resolve the Electron binary once and launch it DIRECTLY (not via `npx`):
  # concurrent `npx electron` invocations contend on the shared npx cache and
  # silently fail to spawn clients 2..N (empty log, no window) — the cause of
  # "only 1 client launched". The resolved path also makes $! the real Electron
  # main PID, so the per-client .pid files are accurate and `down` can stop them.
  local electron_bin
  electron_bin="$(node -p "require('electron')" 2>/dev/null)"
  if [[ -z "$electron_bin" || ! -x "$electron_bin" ]]; then
    echo -e "${RED}✗${NC} Could not resolve Electron binary (node -p \"require('electron')\")" >&2
    exit 3
  fi

  # Synthetic camera/mic is ON BY DEFAULT for the dev harness. Every client gets a
  # fake A/V device (a deterministic moving test pattern + beep) so video E2EE is
  # testable with VISIBLE content end-to-end, all N clients can publish without
  # contending for the one real camera, and there is NO macOS Camera/Screen-
  # Recording TCC prompt (the unsigned dev Electron binary's TCC grants are
  # unreliable). This is a laptop-only test harness, so synthetic media is the
  # sane default; opt out to use the REAL camera/mic/screen with:
  #     CONCORD_DEV_FAKE_MEDIA=0 ./scripts/concord-dev.sh up ...
  local media_flags=()
  if [[ "${CONCORD_DEV_FAKE_MEDIA:-1}" == "1" ]]; then
    media_flags=(--use-fake-device-for-media-stream --use-fake-ui-for-media-stream)
    [[ "$QUIET" -eq 0 ]] && echo -e "  ${BLUE}🎥 synthetic camera/mic ON (default — set CONCORD_DEV_FAKE_MEDIA=0 for real devices)${NC}"
  fi

  for i in $(seq 1 "$num_clients"); do
    # Per-client isolation via --user-data-dir. This only works because
    # pinUserDataPath() (src/main/pinUserDataPath.ts) honors an explicit
    # --user-data-dir and skips the <appData>/ConcordVoice pin in that case.
    # (HOME isolation does NOT work on macOS: app.getPath('appData') resolves via
    # the Cocoa API to the real home and ignores $HOME, so all instances would
    # collide on one userData + one single-instance lock — only 1 client survives.)
    local user_data="$PROJECT_ROOT/.dev-data/client-$i"
    mkdir -p "$user_data"
    NODE_ENV=development nohup "$electron_bin" . --user-data-dir="$user_data" "${media_flags[@]}" \
      > "$LOG_DIR/desktop-client-$i.log" 2>&1 &
    local client_pid=$!
    if ! track_started_process "$LOG_DIR/desktop-client-$i.pid" "$client_pid" \
      "$PROJECT_ROOT/client/desktop" "Desktop Client $i"; then
      echo -e "${RED}✗${NC} Could not record Desktop Client $i process identity" >&2
      return 1
    fi
    [[ "$QUIET" -eq 0 ]] && echo -e "  ${GREEN}→${NC} Client $i: PID $client_pid (data: $user_data)"
    sleep 2
  done

  cd "$PROJECT_ROOT"
}

_up_summary() {
  local skip_client="$1"
  local num_clients="$2"

  [[ "$QUIET" -eq 1 ]] && return 0

  echo ""
  echo -e "${GREEN}✅ Concord Voice dev environment is up${NC}"
  echo ""
  echo -e "${BLUE}Services:${NC}"
  echo -e "  ${GREEN}✓${NC} PostgreSQL    → localhost:5432"
  echo -e "  ${GREEN}✓${NC} Redis         → localhost:6379"
  echo -e "  ${GREEN}✓${NC} NATS          → localhost:4222"
  echo -e "  ${GREEN}✓${NC} coturn        → localhost:3478 (STUN/TURN)"
  echo -e "  ${GREEN}✓${NC} MinIO         → localhost:9000 (S3) / localhost:9001 (console)"
  echo -e "  ${GREEN}✓${NC} Control Plane → http://localhost:8080"
  echo -e "  ${GREEN}✓${NC} Media Plane   → http://localhost:3000"
  if [[ "$skip_client" -eq 0 ]]; then
    echo -e "  ${GREEN}✓${NC} Vite Renderer → http://localhost:3001"
    echo -e "  ${GREEN}✓${NC} Desktop Clients × $num_clients"
  fi
  echo ""
  echo -e "${BLUE}Quick:${NC}"
  echo -e "  Status:   ${YELLOW}$0 status${NC}"
  echo -e "  Logs:     ${YELLOW}$0 logs control-plane${NC}"
  echo -e "  Stop:     ${YELLOW}$0 down${NC}"
}
verb_down() {
  local keep_docker=0
  local force_flag=""
  local docker_failed=0
  local native_failed=0
  local probe_failed=0

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --keep-docker) keep_docker=1; shift ;;
      --force)       force_flag="--force"; shift ;;
      *)
        echo "verb_down: unknown arg '$1'" >&2
        return 2
        ;;
    esac
  done

  mkdir -p "$LOG_DIR"

  # Warn BEFORE tearing down: `down` removes only what its own project can see,
  # so a split makes it report success while leaving these running. That is the
  # 2026-08-19 incident in this file's header comment.
  warn_compose_project_split

  if ! command -v lsof >/dev/null 2>&1; then
    echo -e "${RED}✗${NC} lsof is required to verify safe process cleanup" >&2
    probe_failed=1
  fi

  [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}🖥  Stopping desktop clients${NC}"
  for pid_file in "$LOG_DIR"/desktop-client-*.pid; do
    [[ -f "$pid_file" ]] || continue
    local num
    # Extract the index from the basename, not the full path. The path may
    # contain digits (e.g., worktree dir 'inspiring-mestorf-6dc965/'); the
    # old `grep -oE '[0-9]+' | head -1` would pick the first digit run anywhere
    # in the path. Strip everything up to and including 'desktop-client-' then
    # the '.pid' suffix to get the bare numeric index.
    local base="${pid_file##*/desktop-client-}"
    num="${base%.pid}"
    stop_process "$pid_file" "Desktop Client $num" "$force_flag" || native_failed=1
  done
  stop_native_service "$LOG_DIR/vite-renderer.pid" "Vite Dev Server" 3001 \
    "client/desktop" "$force_flag" || native_failed=1

  [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}🎙  Stopping media-plane${NC}"
  stop_native_service "$LOG_DIR/media-plane.pid" "Media Plane" 3000 \
    "services/media-plane" "$force_flag" || native_failed=1

  [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}🎛  Stopping control-plane${NC}"
  stop_native_service "$LOG_DIR/control-plane.pid" "Control Plane" 8080 \
    "services/control-plane" "$force_flag" || native_failed=1

  if [[ "$keep_docker" -eq 0 ]]; then
    [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}📦 Stopping infrastructure containers${NC}"
    local teardown_env_files=()
    [[ -f "$PROJECT_ROOT/.env.example" ]] && teardown_env_files+=(--env-file "$PROJECT_ROOT/.env.example")
    [[ -f "$PROJECT_ROOT/.env" ]] && teardown_env_files+=(--env-file "$PROJECT_ROOT/.env")
    if compose ${teardown_env_files[@]+"${teardown_env_files[@]}"} down >/dev/null 2>&1; then
      [[ "$QUIET" -eq 0 ]] && echo -e "${GREEN}✓${NC} Docker containers stopped"
    else
      echo -e "${RED}✗${NC} Docker containers could not be stopped" >&2
      docker_failed=1
    fi
  else
    [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}📦 Keeping Docker containers (--keep-docker)${NC}"
  fi

  # Verify ports are free.
  local ports_busy=0 tcp_rc udp_rc
  local ports=(8080 3000 3001)
  [[ "$keep_docker" -eq 0 ]] && ports+=(3478)
  for port in "${ports[@]}"; do
    if port_in_use "$port"; then
      echo -e "${RED}✗${NC} Port $port still in use" >&2
      ports_busy=1
      continue
    else
      tcp_rc=$?
    fi
    if [[ $tcp_rc -eq 2 ]]; then
      echo -e "${RED}✗${NC} Could not verify TCP port $port" >&2
      probe_failed=1
      continue
    fi
    if [[ "$port" == 3478 ]]; then
      if udp_port_in_use "$port"; then
        echo -e "${RED}✗${NC} Port $port still in use" >&2
        ports_busy=1
      else
        udp_rc=$?
        if [[ $udp_rc -eq 2 ]]; then
          echo -e "${RED}✗${NC} Could not verify UDP port $port" >&2
          probe_failed=1
        fi
      fi
    fi
  done

  if [[ $ports_busy -eq 1 || $docker_failed -eq 1 || $native_failed -eq 1 || $probe_failed -eq 1 ]]; then
    [[ "$QUIET" -eq 0 && $ports_busy -eq 1 && "$force_flag" != "--force" ]] && echo -e "${YELLOW}Try: $0 down --force${NC}"
    return 1
  else
    [[ "$QUIET" -eq 0 ]] && echo -e "${GREEN}✅ All services stopped${NC}"
    return 0
  fi
}
verb_status() {
  [[ $# -gt 0 ]] && { echo "verb_status: takes no args, got '$1'" >&2; return 2; }

  echo "=== Concord Voice Dev — Status ==="
  echo ""
  echo "--- Containers ---"
  compose ps 2>/dev/null || echo "(compose ps failed — Docker may not be running)"
  warn_compose_project_split
  echo ""
  echo "--- Native processes ---"
  for svc in control-plane media-plane vite-renderer; do
    local pid_file="$LOG_DIR/$svc.pid"
    local pid
    if pid=$(read_valid_pid "$pid_file"); then
      if tracked_pid_identity_state "$pid_file" "$pid"; then
        printf "  ✓ %-18s PID %s\n" "$svc" "$pid"
      else
        printf "  ✗ %-18s stale or unverified PID file\n" "$svc"
      fi
    elif [[ -f "$pid_file" ]]; then
      printf "  ✗ %-18s stale or invalid PID file\n" "$svc"
    else
      printf "  ✗ %-18s not running\n" "$svc"
    fi
  done
  for pid_file in "$LOG_DIR"/desktop-client-*.pid; do
    [[ -f "$pid_file" ]] || continue
    # Extract index from basename, not full path (worktree dirs have digits).
    local base="${pid_file##*/desktop-client-}"
    local num="${base%.pid}"
    local pid
    if pid=$(read_valid_pid "$pid_file") && tracked_pid_identity_state "$pid_file" "$pid"; then
      printf "  ✓ %-18s PID %s\n" "desktop-client-$num" "$pid"
    else
      printf "  ✗ %-18s stale or unverified PID file\n" "desktop-client-$num"
    fi
  done

  echo ""
  echo "--- Health checks ---"
  for svc in "Control Plane:8080" "Media Plane:3000"; do
    local name="${svc%%:*}"
    local port="${svc##*:}"
    if curl -sf "http://localhost:$port/health" >/dev/null 2>&1; then
      printf "  ✓ %s (localhost:%s)\n" "$name" "$port"
    else
      printf "  ✗ %s (port %s)\n" "$name" "$port"
    fi
  done

  echo ""
  echo "--- Log file sizes ---"
  if [[ -d "$LOG_DIR" ]]; then
    # Log names are script-owned; ls supplies the size ordering used here.
    # shellcheck disable=SC2012
    ls -lhS "$LOG_DIR" 2>/dev/null | awk 'NR>1 && /\.log$/ {printf "  %-30s %s\n", $9, $5}'
  fi
}
verb_logs() {
  local service="all"
  local lines=50
  local follow=1
  local errors_only=0
  local clear=0
  local -a positionals=()

  # First positional may be a service name OR a flag.
  if [[ $# -gt 0 && "${1:0:1}" != "-" ]]; then
    service="$1"; shift
  fi

  # Parse flags + collect remaining positionals (e.g., 'client 2' or 'docker postgres').
  # Flags can appear anywhere in the arg list — order-independent per usage docs.
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -n|--lines)
        if [[ $# -lt 2 ]]; then
          echo "verb_logs: -n/--lines requires a number" >&2
          return 2
        fi
        if ! [[ "$2" =~ ^[0-9]+$ ]]; then
          echo "verb_logs: -n/--lines must be a non-negative integer, got '$2'" >&2
          return 2
        fi
        lines="$2"; shift 2 ;;
      -f|--follow)  follow=1; shift ;;
      --no-follow)  follow=0; shift ;;
      --clear)      clear=1; shift ;;
      --errors)     errors_only=1; shift ;;
      *)
        if [[ "$service" == "docker" || "$service" == "client" ]]; then
          positionals+=("$1")
          shift
        else
          echo "verb_logs: unknown arg '$1'" >&2
          return 2
        fi
        ;;
    esac
  done

  # Docker logs delegate. Build args as an array so quoting survives.
  if [[ "$service" == "docker" ]]; then
    local docker_args=("--tail=$lines")
    [[ $follow -eq 1 ]] && docker_args+=("-f")
    local docker_svc="${positionals[0]:-}"
    if [[ -n "$docker_svc" ]]; then
      docker_args+=("$docker_svc")
    fi
    # `compose logs -f` blocks until SIGINT and returns 130. Caller may
    # interpret 130 as success; that's intentional for the follow path.
    compose logs "${docker_args[@]}"
    return $?
  fi

  # Map service name to log file FIRST so --clear can use the resolved path.
  # Otherwise vite→vite-renderer.log and client→desktop-client-N.log would
  # be silently bypassed and --clear would create useless empty files like
  # `vite.log` while the real log stayed full (Gitar finding, 2026-05-22).
  local log_file
  case "$service" in
    all)           log_file="" ;;
    control-plane) log_file="$LOG_DIR/control-plane.log" ;;
    media-plane)   log_file="$LOG_DIR/media-plane.log" ;;
    vite)          log_file="$LOG_DIR/vite-renderer.log" ;;
    client)
      local n="${positionals[0]:-1}"
      if ! [[ "$n" =~ ^[0-9]+$ ]]; then
        echo "verb_logs: client index must be numeric, got '$n'" >&2
        return 2
      fi
      log_file="$LOG_DIR/desktop-client-$n.log"
      ;;
    *)
      echo "verb_logs: unknown service '$service'" >&2
      echo "Services: all, control-plane, media-plane, vite, client [N], docker [<svc>]" >&2
      return 2
      ;;
  esac

  if [[ "$clear" -eq 1 ]]; then
    if [[ "$service" == "all" ]]; then
      for f in "$LOG_DIR"/*.log; do
        [[ -f "$f" ]] && : > "$f"
      done
    else
      : > "$log_file" 2>/dev/null || true
    fi
  fi

  local tail_args=("-n" "$lines")
  [[ $follow -eq 1 ]] && tail_args+=("-f")

  # Build the file list as an array so paths with spaces / metacharacters
  # survive — replaces a prior `eval $cmd` that was vulnerable to command
  # injection when $LOG_DIR contained shell metacharacters (PR review,
  # 2026-05-22).
  local -a log_files
  if [[ "$service" == "all" ]]; then
    log_files=("$LOG_DIR"/*.log)
    if [[ ${#log_files[@]} -eq 0 || ! -e "${log_files[0]}" ]]; then
      echo "verb_logs: no log files found in $LOG_DIR" >&2
      return 1
    fi
  else
    log_files=("$log_file")
  fi

  if [[ $errors_only -eq 1 ]]; then
    tail "${tail_args[@]}" "${log_files[@]}" 2>/dev/null | grep -E 'ERROR|error|FATAL|panic'
  else
    tail "${tail_args[@]}" "${log_files[@]}" 2>/dev/null
  fi
}
verb_restart() {
  [[ $# -gt 0 ]] && { echo "verb_restart: takes no args, got '$1'" >&2; return 2; }
  verb_down --keep-docker || return $?
  verb_up --no-rebuild
}

verb_rebuild() {
  [[ $# -gt 0 ]] && { echo "verb_rebuild: takes no args, got '$1'" >&2; return 2; }
  verb_down --keep-docker || return $?
  verb_up  # rebuild = default
}

verb_freshstart() {
  [[ $# -gt 0 ]] && { echo "verb_freshstart: takes no args, got '$1'" >&2; return 2; }

  [[ "$QUIET" -eq 0 ]] && echo -e "${RED}=== Fresh Start: this WIPES ALL DEV DATA ===${NC}"
  [[ "$QUIET" -eq 0 ]] && echo "Databases, Redis, client user data — all gone."
  if [[ -t 0 ]]; then  # only prompt if stdin is a TTY
    read -rp "Continue? (y/N) " confirm
    # Portable lowercase compare — `${var,,}` is bash 4+ and breaks macOS bash 3.2.
    if ! [[ "$confirm" =~ ^[Yy]$ ]]; then
      echo "Aborted."
      exit 0
    fi
  fi

  if ! verb_down --force; then
    echo -e "${RED}✗${NC} Fresh start aborted because existing processes could not be stopped safely." >&2
    return 1
  fi
  compose down -v >/dev/null 2>&1 || true
  # `compose down -v` only removes containers belonging to the current compose
  # project (`concordvoice-dev` per docker-compose.dev.yml `name:` directive).
  # `container_name:` in docker-compose.yml uses GLOBAL Docker names — not
  # project-prefixed — so orphans from a prior project (e.g., users migrating
  # from the pre-#1113 `dev-start.sh` which had no `name:` and defaulted to
  # `concord`) survive `compose down` and then conflict during `up` with
  # `Error response from daemon: Conflict. The container name "..." is already
  # in use`. Force-remove by literal name; missing containers are a no-op.
  docker rm -f \
    concordvoice-postgres \
    concordvoice-redis \
    concordvoice-nats \
    concordvoice-coturn \
    concordvoice-minio \
    >/dev/null 2>&1 || true
  rm -rf "$PROJECT_ROOT/.dev-data" 2>/dev/null || true
  verb_up
}
verb_validate_config() {
  if compose config --quiet 2>&1; then
    [[ "$QUIET" -eq 0 ]] && echo -e "  ${GREEN}✓${NC} docker compose config validates (base + dev overlay)"
    return 0
  else
    echo "" >&2
    echo -e "  ${RED}✗${NC} docker compose config failed — invalid YAML or missing env var." >&2
    echo "    See docker output above for the specific failure." >&2
    echo "    If a required env var is missing, run: cp .env.example .env" >&2
    return 1
  fi
}

# verb_dev_code() — print the most recent email verification code the
# control-plane logged in dev mode. When SMTP_HOST is empty the email service
# logs codes instead of sending them (services/control-plane/internal/email/
# service.go:92), so registration works locally without a real inbox; this
# surfaces the latest code without grepping the log by hand. An optional
# [email] arg filters to codes sent to that recipient. Read-only.
verb_dev_code() {
  local email_filter=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -h|--help)
        echo "Usage: $0 dev-code [email]"
        echo "  Print the most recent dev-mode email verification code from the"
        echo "  control-plane log. Pass an email to filter to that recipient."
        return 0
        ;;
      -*) echo "verb_dev_code: unknown option '$1'" >&2; return 2 ;;
      *)  email_filter="$1"; shift ;;
    esac
  done

  local log="$LOG_DIR/control-plane.log"
  if [[ ! -f "$log" ]]; then
    echo -e "${RED}✗${NC} No control-plane log at $log — is the stack up? Run: $0 up" >&2
    return 1
  fi

  # Dev-mode codes are logged as:
  #   ... msg="DEV MODE — email verification code" to=<email> code=<digits>
  # Match the ASCII-safe substrings ('DEV MODE', 'verification') to dodge
  # em-dash encoding hazards, and `|| true` so a no-match doesn't trip set -e.
  local matches
  matches="$(grep -F 'DEV MODE' "$log" 2>/dev/null | grep -F 'verification' || true)"
  if [[ -n "$email_filter" ]]; then
    # Exact whitespace-delimited field match: slog emits `to=<email>` as a single
    # token, so match the field exactly rather than a loose substring — a filter
    # of `a@b.com` must not also match `a@b.com.evil` (Gitar review, #1533). awk
    # exits 0 with no output on no-match, so no `|| true` is needed.
    matches="$(printf '%s\n' "$matches" | awk -v want="to=$email_filter" \
      '{ for (i = 1; i <= NF; i++) if ($i == want) { print; break } }')"
  fi

  local line
  line="$(printf '%s\n' "$matches" | grep -E 'code=' | tail -1 || true)"
  if [[ -z "$line" ]]; then
    local who=""
    [[ -n "$email_filter" ]] && who=" for '$email_filter'"
    echo -e "${YELLOW}⚠${NC} No dev-mode verification code found${who} in $log." >&2
    [[ -z "$email_filter" ]] && echo "    Register in the client first — the code is logged here in dev mode." >&2
    return 1
  fi

  local code to
  code="$(printf '%s\n' "$line" | sed -nE 's/.*code="?([0-9]+).*/\1/p' | tail -1)"
  to="$(printf '%s\n' "$line" | sed -nE 's/.*to="?([^"[:space:]]+).*/\1/p' | tail -1)"

  if [[ "$QUIET" -eq 0 ]]; then
    echo -e "${GREEN}✓${NC} Latest dev verification code: ${BLUE}${code}${NC}  (to: ${to})"
  else
    printf '%s\n' "$code"
  fi
}

# console_prompt() — the REPL prompt. Deliberately trivial (no per-prompt docker
# probe) so the prompt is instant.
console_prompt() {
  printf '%b' "${GREEN}concord${NC}> "
}

# console_help() — list the commands available inside the REPL.
console_help() {
  cat <<'HELP'
Commands (same as the CLI verbs):
  up | down | status | logs | restart | rebuild | freshstart
  validate-config | dev-code [email]
REPL builtins:
  help | ?      Show this list
  clear         Clear the screen
  exit | quit   Leave the console (Ctrl-D also works)
Verb flags work too, e.g.  logs control-plane -n 50
HELP
}

# console_repl() — the interactive dispatch loop. Reads one command per line and
# routes it through dispatch_verb until exit/quit/EOF. Each verb runs in a
# subshell with errexit disabled, so a failing command (or a `cd` inside a verb)
# returns cleanly to the prompt instead of ending the session. Factored out of
# verb_console() so tests can drive it with piped (non-TTY) input. The TTY check
# keeps readline (-e) + the prompt for humans while letting tests feed stdin.
console_repl() {
  local line args interactive=0
  [[ -t 0 ]] && interactive=1
  while true; do
    if [[ "$interactive" -eq 1 ]]; then
      IFS= read -e -r -p "$(console_prompt)" line || { echo; break; }
    else
      IFS= read -r line || break
    fi
    # Trim surrounding whitespace.
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    [[ -z "$line" ]] && continue
    [[ "$interactive" -eq 1 ]] && { history -s "$line" 2>/dev/null || true; }
    case "$line" in
      exit|quit) return 0 ;;
      # `|| true` so a builtin failing under set -e (e.g. `clear` with an unset
      # TERM) returns to the prompt instead of killing the session (Gitar #1534).
      help|'?')  console_help || true ;;
      clear)     command clear || true ;;
      *)
        read -ra args <<< "$line"
        # Subshell + `set +e`: isolate a verb's failure and any `cd` it does, so
        # the loop survives and the next command starts from a clean cwd.
        ( set +e; dispatch_verb "${args[@]}" ) || true
        ;;
    esac
  done
  return 0
}

# verb_console() — launch the interactive REPL. Refuses a non-interactive stdin
# (so it never blocks in CI/pipes with no input), prints a short banner, and
# installs a SIGINT trap so Ctrl-C aborts the running command and returns to the
# prompt instead of killing the session. NOTE: Ctrl-C at an idle prompt still
# exits the loop (v1 limitation) — use `exit`/Ctrl-D to leave deliberately.
verb_console() {
  if [[ ! -t 0 ]]; then
    echo -e "${RED}✗${NC} 'console' needs an interactive terminal (stdin is not a TTY)." >&2
    return 2
  fi
  [[ "$QUIET" -eq 0 ]] && echo -e "${BLUE}Concord dev console${NC} — type ${GREEN}help${NC} for commands, ${GREEN}exit${NC} to leave."
  # Ctrl-C during a running verb kills that verb's subshell (default INT there)
  # while this trap keeps the parent loop alive and reprints a clean line.
  trap 'echo' INT
  console_repl
  local rc=$?
  trap - INT
  return "$rc"
}

# ── Source-vs-execute guard ────────────────────────────────────────────
# Tests do `source concord-dev.sh` to call helpers without re-running main.

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
