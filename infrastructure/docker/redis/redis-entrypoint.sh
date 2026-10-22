#!/bin/sh
set -eu

case "${REDIS_CREDENTIAL_MODE:-legacy}" in
  legacy)
    if [ -z "${REDISCLI_AUTH:-}" ]; then
      echo 'redis_start_refused: missing_legacy_password' >&2
      exit 1
    fi
    ;;
  managed)
    if [ "${REDISCLI_AUTH:-}" != "" ]; then
      echo 'redis_start_refused: legacy_auth_env_present' >&2
      exit 1
    fi
    if [ ! -r /run/redis-auth/users.acl ]; then
      echo 'redis_start_refused: managed_acl_unavailable' >&2
      exit 1
    fi
    for arg in "$@"; do
      if [ "$arg" = --requirepass ]; then
        echo 'redis_start_refused: managed_legacy_auth_argument' >&2
        exit 1
      fi
    done
    ;;
  *)
    echo 'redis_start_refused: invalid_credential_mode' >&2
    exit 1
    ;;
esac

exec /usr/local/bin/docker-entrypoint.sh "$@"
