#!/usr/bin/env bash
# Dedicated security-event stream bootstrap; executable paths are pinned.
SECURITY_EVENT_LOG_ROOT="/var/log/concord-security"
SECURITY_EVENT_LOGROTATE_FILE="/etc/logrotate.d/concord-security"
SECURITY_EVENT_SYSTEMD_DIR="/etc/systemd/system"
SECURITY_EVENT_WRITER_GROUP="concord-security-writers"
SECURITY_EVENT_WRITER_GID=10001

security_group_inventory() {
  local group="$1" gid="$2" output status
  local name password record_gid members extra
  SECURITY_GROUP_NAMED_COUNT=0
  SECURITY_GROUP_NAMED_RECORD=
  SECURITY_GROUP_NAMED_GID=
  SECURITY_GROUP_GID_COUNT=0
  SECURITY_GROUP_GID_RECORD=

  if output="$(getent group)"; then
    :
  else
    status=$?
    err "group database lookup failed while enumerating groups"
    return "$status"
  fi
  [[ -z "$output" ]] || while IFS=: read -r name password record_gid members extra; do
    [[ -n "$name" && -n "$record_gid" && "$record_gid" =~ ^[0-9]+$ && -z "$extra" ]] || {
      err "malformed group database record"
      return 1
    }
    if [[ "$name" == "$group" ]]; then
      SECURITY_GROUP_NAMED_COUNT=$((SECURITY_GROUP_NAMED_COUNT + 1))
      SECURITY_GROUP_NAMED_RECORD="$name:$password:$record_gid:$members"
      SECURITY_GROUP_NAMED_GID="$record_gid"
    fi
    if [[ "$record_gid" == "$gid" ]]; then
      SECURITY_GROUP_GID_COUNT=$((SECURITY_GROUP_GID_COUNT + 1))
      SECURITY_GROUP_GID_RECORD="$name:$password:$record_gid:$members"
    fi
  done <<<"$output"
}

prepare_security_event_stream() {
  local stream="$1" gid="$2"
  python3 - "$stream" "$gid" <<'PY'
import os
import stat
import sys

path, gid = sys.argv[1], int(sys.argv[2])
parent, name = os.path.dirname(path), os.path.basename(path)
dirfd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
try:
    created = False
    try:
        fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_APPEND | os.O_NOFOLLOW | os.O_NONBLOCK, 0o620, dir_fd=dirfd)
        created = True
    except FileExistsError:
        fd = os.open(name, os.O_WRONLY | os.O_APPEND | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=dirfd)
    try:
        metadata = os.fstat(fd)
        if not stat.S_ISREG(metadata.st_mode):
            raise RuntimeError("security event stream is not a regular file")
        if created:
            os.fchown(fd, 0, gid)
            os.fchmod(fd, 0o620)
        elif (metadata.st_uid, metadata.st_gid, stat.S_IMODE(metadata.st_mode), metadata.st_nlink) != (0, gid, 0o620, 1):
            raise RuntimeError("existing security event stream has an untrusted owner, mode, or link count")
    finally:
        os.close(fd)
finally:
    os.close(dirfd)
PY
}

install_security_event_logging() {
  [[ $# -le 3 ]] || { err "public security-event logging takes no hook argument"; return 1; }
  # A public installation must not reuse an active or dangling managed-reader
  # boundary. Refuse before group lookup or any logging/rotation mutation.
  if [[ -e /var/lib/nightwatch/normalizer/commissioned || -L /var/lib/nightwatch/normalizer/commissioned ]]; then
    err "public security-event logging refuses managed normalizer state"
    return 1
  fi
  _install_security_event_logging "$@"
}

_install_security_event_logging() {
  [[ $# -le 4 ]] || { err "security-event logging internal arguments refused"; return 1; }
  # Optional positional roots exist only for the directly sourced hermetic
  # test. Private managed provisioning supplies its own fixed prerotate body;
  # the public command never supplies one or reads an environment selector.
  local root="${1:-$SECURITY_EVENT_LOG_ROOT}"
  local rotate_file="${2:-$SECURITY_EVENT_LOGROTATE_FILE}"
  local systemd_dir="${3:-$SECURITY_EVENT_SYSTEMD_DIR}"
  local prerotate_body="${4:-}"
  local group="$SECURITY_EVENT_WRITER_GROUP"
  local gid="$SECURITY_EVENT_WRITER_GID"
  local existing_group members service_dir stream

  command -v python3 >/dev/null 2>&1 || {
    err "python3 is required to prepare security event logging"
    return 1
  }
  if ! command -v logrotate >/dev/null 2>&1 || ! command -v systemd-analyze >/dev/null 2>&1; then
    err "logrotate and systemd-analyze are required to validate security event controls"
    return 1
  fi

  security_group_inventory "$group" "$gid" || return 1
  if [[ "$SECURITY_GROUP_NAMED_COUNT" -gt 1 || "$SECURITY_GROUP_GID_COUNT" -gt 1 ]]; then
    err "GID ${gid} must have exactly one group record named ${group}"
    return 1
  fi
  if [[ "$SECURITY_GROUP_NAMED_COUNT" == 1 ]]; then
    [[ "$SECURITY_GROUP_NAMED_GID" == "$gid" && "$SECURITY_GROUP_GID_COUNT" == 1 && "$SECURITY_GROUP_GID_RECORD" == "$SECURITY_GROUP_NAMED_RECORD" ]] || {
      err "${group} exists with GID ${SECURITY_GROUP_NAMED_GID}; expected ${gid}"
      return 1
    }
    existing_group="$SECURITY_GROUP_NAMED_RECORD"
  else
    if [[ "$SECURITY_GROUP_GID_COUNT" == 1 ]]; then
      err "GID ${gid} is already assigned to another group"
      return 1
    fi
    groupadd --system --gid "$gid" "$group"
    security_group_inventory "$group" "$gid" || return 1
    [[ "$SECURITY_GROUP_NAMED_COUNT" == 1 && "$SECURITY_GROUP_GID_COUNT" == 1 && "$SECURITY_GROUP_NAMED_GID" == "$gid" && "$SECURITY_GROUP_GID_RECORD" == "$SECURITY_GROUP_NAMED_RECORD" ]] || {
      err "group ${group} was not created at GID ${gid}"
      return 1
    }
    existing_group="$SECURITY_GROUP_NAMED_RECORD"
  fi

  members="${existing_group#*:*:*:}"
  [[ -z "$members" ]] || { err "${group} must not have supplementary members"; return 1; }
  local primary_users
  if ! primary_users="$(getent passwd | awk -F: -v gid="$gid" '$4 == gid {print $1}')"; then
    err "group database lookup failed while checking primary accounts"
    return 1
  fi
  [[ -z "$primary_users" ]] || { err "${group} must not be a primary group for any account"; return 1; }

  [[ ! -L "$root" ]] || { err "security log root must not be a symlink"; return 1; }
  [[ ! -e "$root" || -d "$root" ]] || { err "security log root must be a directory"; return 1; }
  install -d -o root -g "$group" -m 0750 "$root"

  for service_dir in control-plane media-plane; do
    service_dir="${root}/${service_dir}"
    [[ ! -L "$service_dir" ]] || { err "security log directory must not be a symlink: ${service_dir}"; return 1; }
    [[ ! -e "$service_dir" || -d "$service_dir" ]] || { err "security log directory must be a directory: ${service_dir}"; return 1; }
    install -d -o root -g "$group" -m 2710 "$service_dir"
    stream="${service_dir}/events.jsonl"
    [[ ! -L "$stream" ]] || { err "security event stream must not be a symlink: ${stream}"; return 1; }
    [[ ! -e "$stream" || -f "$stream" ]] || { err "security event stream must be a regular file: ${stream}"; return 1; }
    prepare_security_event_stream "$stream" "$gid" || {
      err "could not safely prepare security event stream: ${stream}"
      return 1
    }
  done

  _publish_security_event_controls "$root" "$rotate_file" "$systemd_dir" "$prerotate_body" "$group" || {
    err "could not publish security event logging controls"
    return 1
  }
  log "Security event logging prepared (${root}; GID ${gid})"
}

# Keep traps private to this operation when the installer sources this file.
# Stage and validate all three controls before replacing any live pathname.
# Each rename is atomic; the three pathnames are not one transaction.
_publish_security_event_controls() (
  local root="$1" rotate_file="$2" systemd_dir="$3" prerotate_body="$4" group="$5"
  local rotate_stage='' units_stage='' staged_rotate staged_service staged_timer
  trap '[[ -z "$rotate_stage" ]] || rm -rf -- "$rotate_stage"; [[ -z "$units_stage" ]] || rm -rf -- "$units_stage"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  install -d -o root -g root -m 0755 "$(dirname "$rotate_file")" || return 1
  install -d -o root -g root -m 0755 "$systemd_dir" || return 1
  rotate_stage=$(mktemp -d "$(dirname "$rotate_file")/.concord-security.XXXXXXXX") || return 1
  units_stage=$(mktemp -d "${systemd_dir}/.concord-security.XXXXXXXX") || return 1
  chown root:root "$rotate_stage" "$units_stage" || return 1
  chmod 0700 "$rotate_stage" "$units_stage" || return 1
  staged_rotate="${rotate_stage}/concord-security"
  staged_service="${units_stage}/concord-security-logrotate.service"
  staged_timer="${units_stage}/concord-security-logrotate.timer"

  cat > "$staged_rotate" <<EOF || return 1
${root}/control-plane/events.jsonl {
    size 19M
    rotate 5
    compress
    delaycompress
    missingok
    notifempty
    su root ${group}
    create 0620 root ${group}
${prerotate_body}
    postrotate
        [ ! -e ${root}/control-plane/events.jsonl.1 ] || { chown root:root ${root}/control-plane/events.jsonl.1 && chmod 0600 ${root}/control-plane/events.jsonl.1; } || exit 1
    endscript
}

${root}/media-plane/events.jsonl {
    size 19M
    rotate 5
    compress
    delaycompress
    missingok
    notifempty
    su root ${group}
    create 0620 root ${group}
${prerotate_body}
    postrotate
        [ ! -e ${root}/media-plane/events.jsonl.1 ] || { chown root:root ${root}/media-plane/events.jsonl.1 && chmod 0600 ${root}/media-plane/events.jsonl.1; } || exit 1
    endscript
}
EOF
  cat > "$staged_service" <<EOF || return 1
[Unit]
Description=Rotate Concord Voice security event logs

[Service]
Type=oneshot
User=root
ExecStart=/usr/sbin/logrotate ${rotate_file}
EOF
  cat > "$staged_timer" <<'EOF' || return 1
[Unit]
Description=Rotate Concord Voice security event logs every minute

[Timer]
Unit=concord-security-logrotate.service
OnCalendar=*-*-* *:*:00
AccuracySec=1s
Persistent=true

[Install]
WantedBy=timers.target
EOF
  chown root:root "$staged_rotate" "$staged_service" "$staged_timer" || return 1
  chmod 0644 "$staged_rotate" "$staged_service" "$staged_timer" || return 1
  if ! logrotate --debug "$staged_rotate" >/dev/null 2>&1 ||
     ! systemd-analyze verify --man=no "$staged_service" "$staged_timer" >/dev/null 2>&1; then
    err "security event control validation failed"
    return 1
  fi
  mv -fT -- "$staged_rotate" "$rotate_file" || return 1
  mv -fT -- "$staged_service" "${systemd_dir}/concord-security-logrotate.service" || return 1
  mv -fT -- "$staged_timer" "${systemd_dir}/concord-security-logrotate.timer" || return 1
  systemctl daemon-reload || return 1
  systemctl enable --now concord-security-logrotate.timer || return 1
)

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -euo pipefail
  log() { printf '%s\n' "$*"; }
  err() { printf '%s\n' "$*" >&2; }
  [[ "${1:-}" == security-event-logging ]] || {
    err "Usage: selfhost-security-event-logging.sh security-event-logging"
    exit 1
  }
  shift
  [[ $# -eq 0 ]] || { err "security-event-logging takes no arguments"; exit 1; }
  if [[ $EUID -ne 0 ]]; then
    err "security-event-logging must be run as root (use sudo)"
    exit 1
  fi
  install_security_event_logging "$@"
fi
