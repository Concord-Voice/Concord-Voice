#!/bin/sh
set -eu

if [ "$#" -gt 1 ]; then
  echo "usage: configure-ops-metrics-hba.sh [database_name]" >&2
  exit 1
fi
database_name="${1:-${POSTGRES_DB:-${POSTGRES_USER:-postgres}}}"
case "$database_name" in
  ''|*[!a-z0-9_]*)
    echo "operations metrics database must match [a-z0-9_]+" >&2
    exit 1
    ;;
esac

pgdata="${PGDATA:-/var/lib/postgresql/data}"
hba_file="$pgdata/pg_hba.conf"
rules_dir="${OPS_METRICS_HBA_DIR:-/etc/postgresql}"
rules_file="$rules_dir/concord-ops-metrics-hba.conf"

if [ ! -f "$hba_file" ]; then
  echo "PostgreSQL HBA file does not exist: $hba_file" >&2
  exit 1
fi

if [ "$(id -u)" = "0" ]; then
  mkdir -p "$rules_dir"
  chown postgres:postgres "$rules_dir"
  chmod 0750 "$rules_dir"
elif [ ! -d "$rules_dir" ] || [ ! -w "$rules_dir" ]; then
  echo "operations metrics HBA directory is not writable: $rules_dir" >&2
  exit 1
fi

role_hash="$(printf '%s' "$database_name" | md5sum)"
role_hash="${role_hash%% *}"
case "$role_hash" in
  ''|*[!0-9a-f]*)
    echo "failed to derive the operations metrics reader role" >&2
    exit 1
    ;;
esac
if [ "${#role_hash}" -ne 32 ]; then
  echo "failed to derive the operations metrics reader role" >&2
  exit 1
fi
reader_role="concord_ops_metrics_reader_$role_hash"

umask 077
rules_tmp="$(mktemp "${rules_file}.tmp.XXXXXX")"
hba_tmp="$(mktemp "${hba_file}.tmp.XXXXXX")"
cleanup() {
  rm -f "$rules_tmp" "$hba_tmp"
}
trap cleanup EXIT HUP INT TERM

cat >"$rules_tmp" <<EOF
# Managed by configure-ops-metrics-hba.sh. The first-match order is security-critical.
host  $database_name  $reader_role  0.0.0.0/0  scram-sha-256
host  $database_name  $reader_role  ::0/0       scram-sha-256
host  all             $reader_role  0.0.0.0/0  reject
host  all             $reader_role  ::0/0       reject
local all             $reader_role              reject
EOF

if [ "$(id -u)" = "0" ]; then
  chown postgres:postgres "$rules_tmp"
fi
chmod 0600 "$rules_tmp"
mv "$rules_tmp" "$rules_file"

{
  printf 'include %s\n' "$rules_file"
  awk -v path="$rules_file" '
    {
      normalized = $0
      sub(/^[[:space:]]+/, "", normalized)
      sub(/[[:space:]]+$/, "", normalized)
      if (normalized == "include " path ||
          normalized == "include \047" path "\047" ||
          normalized == "include \042" path "\042" ||
          normalized == "include_if_exists " path ||
          normalized == "include_if_exists \047" path "\047" ||
          normalized == "include_if_exists \042" path "\042") {
        next
      }
      print
    }
  ' "$hba_file"
} >"$hba_tmp"

if [ "$(id -u)" = "0" ]; then
  chown postgres:postgres "$hba_tmp"
fi
chmod 0600 "$hba_tmp"
mv "$hba_tmp" "$hba_file"

trap - EXIT HUP INT TERM
