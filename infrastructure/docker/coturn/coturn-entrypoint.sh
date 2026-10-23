#!/bin/sh
# Fail-loud credential-source guard for coturn (#3931). Compose interpolation
# runs before file merging, so the base model can no longer reject an unset
# TURN_SECRET with `:?` without also rejecting the managed model that carries no
# static secret. The check therefore moves here and is an ALLOWLIST over the
# final argv, followed by a direct exec of turnserver:
#
#   legacy  --static-auth-secret=<non-empty>        (an empty value is refused)
#   managed --userdb=/var/lib/coturn-runtime/turndb (read-only RAM store)
#
# Exactly one source must be present. This wrapper replaces the image's
# docker-entrypoint.sh, whose `exec $(eval "echo $@")` re-expands and re-splits
# every argument and so defeated any token-level check made before it. Nothing
# here evaluates an argument; turnserver receives each one exactly as Compose
# produced it. Unknown options (getopt abbreviations such as --static-auth, the
# -b / --db aliases, a second config file) are refused rather than guessed at,
# and the one permitted config file must not carry a credential source of its
# own or turn authentication off. Refusal messages are fixed strings; no
# argument value is ever printed.
set -eu

conf=/etc/coturn/turnserver.conf
userdb_path=/var/lib/coturn-runtime/turndb

refuse() {
  echo "coturn_start_refused: $1" >&2
  exit 1
}

# True when $1 holds a character the retired eval path would have re-expanded.
has_meta() {
  case "$1" in
    *'$'* | *'`'* | *[[:space:]]*) return 0 ;;
  esac
  return 1
}

[ "${1:-}" = turnserver ] || refuse unexpected_argument
shift

static_secret=0
userdb=0
expect_conf=0
conf_count=0
for arg in "$@"; do
  if [ "$expect_conf" = 1 ]; then
    [ "$arg" = "$conf" ] || refuse unexpected_argument
    expect_conf=0
    conf_count=$((conf_count + 1))
    continue
  fi
  case "$arg" in
    -c) expect_conf=1 ;;
    # Secret and realm are operator free text. They are inert here: nothing
    # below evaluates them.
    --static-auth-secret=?*) static_secret=1 ;;
    --static-auth-secret=* | --static-auth-secret) refuse missing_static_auth_secret ;;
    --realm=*) ;;
    --userdb=*)
      [ "$arg" = "--userdb=$userdb_path" ] || refuse invalid_userdb_path
      userdb=1
      ;;
    --userdb) refuse invalid_userdb_path ;;
    --listening-ip=* | --external-ip=* | --cert=/etc/coturn/certs/cert.pem | --pkey=/etc/coturn/certs/key.pem)
      if has_meta "$arg"; then refuse unexpected_argument; fi
      ;;
    *) refuse unexpected_argument ;;
  esac
done
[ "$expect_conf" = 0 ] || refuse unexpected_argument

# True when a line of the config file matches the ERE in $1. Only grep's "no
# match" (1) is false; a read error (2) refuses instead of skipping the check.
conf_has() {
  rc=0
  grep -Eq "$1" "$conf" || rc=$?
  case "$rc" in
    0) return 0 ;;
    1) return 1 ;;
    *) refuse config_unreadable ;;
  esac
}

# The config file is the other place a credential source can hide.
if [ "$userdb" = 1 ] || [ "$static_secret" = 1 ]; then
  [ -r "$conf" ] || refuse config_unreadable
  # coturn reads the file with fgets(buf, 1024) and parses each 1023-byte piece
  # of a longer line as a line of its own, which no line-based check sees.
  rc=0
  LC_ALL=C awk 'length($0) > 1022 { bad = 1 } END { exit !bad }' "$conf" || rc=$?
  case "$rc" in
    0) refuse config_line_too_long ;;
    1) ;;
    *) refuse config_unreadable ;;
  esac
  # coturn also accepts `name value`, separated by a space or a tab.
  if conf_has '^[[:space:]]*(static-auth-secret|db|user|([a-z]+-)?userdb)([[:space:]=]|$)'; then
    refuse config_credential_source
  fi
  # --userdb turns on no authentication by itself: only a bare use-auth-secret
  # in this file does. lt-cred-mech, no-auth or a valued use-auth-secret line
  # can switch it off.
  if conf_has '^[[:space:]]*(lt-cred-mech|no-auth)([[:space:]=]|$)' \
    || conf_has '^[[:space:]]*use-auth-secret[[:space:]=]+[^[:space:]]'; then
    refuse config_auth_mode
  fi
  if [ "$userdb" = 1 ] && ! conf_has '^[[:space:]]*use-auth-secret[[:space:]]*$'; then
    refuse managed_auth_not_enabled
  fi
fi

if [ "$static_secret" = 1 ] && [ "$userdb" = 1 ]; then
  refuse mixed_credential_sources
fi
if [ "$static_secret" = 0 ] && [ "$userdb" = 0 ]; then
  refuse no_credential_source
fi
if [ "$userdb" = 1 ] && [ ! -r "$userdb_path" ]; then
  refuse managed_userdb_unavailable
fi
# Exactly one -c, so the file checked above is the only one coturn reads.
[ "$conf_count" = 1 ] || refuse unexpected_argument

exec turnserver "$@"
