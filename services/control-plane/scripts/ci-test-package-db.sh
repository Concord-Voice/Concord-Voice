#!/usr/bin/env bash
# go test -exec wrapper: give each package binary a clone of the migrated CI DB.
# This is CI-only; local tests keep their existing explicit DATABASE_URL contract.
set -euo pipefail

if [[ "${CI_PACKAGE_DB_ISOLATION:-}" != 1 || "$#" -lt 1 || ! -x "$1" ]]; then
  echo 'ci-test-package-db: expected a CI test binary and isolation opt-in' >&2
  exit 2
fi

# These integration packages observe the same fixed NATS voice.enforce.*
# subjects. Keep their observers sequential while unrelated binaries use the
# other Go test slot. This list follows live subscriptions in their *_test.go
# files; expanding per-package DBs does not isolate the shared NATS service.
case "$(basename -- "$1")" in
  channels.test|dm.test|members.test|rbac.test|servers.test|voice.test)
    exec 9>"${RUNNER_TEMP:-/tmp}/concord-voice-nats-tests.lock"
    flock 9
    ;;
  *) : ;;
esac

# The operations metrics reader uses a separately provisioned database and
# pg_hba rule. Those integration suites still share that one fixture, so keep
# only its consumers sequential instead of serializing the whole Go shard.
case "$(basename -- "$1")" in
  database.test|opsmetrics.test|server.test)
    exec 8>"${RUNNER_TEMP:-/tmp}/concord-opsmetrics-tests.lock"
    flock 8
    ;;
  *) : ;;
esac

digest="$(printf '%s' "$1" | sha256sum | cut -c1-16)"
database="concord_ci_${digest}_test"

# DATABASE_URL may contain a password; never echo it or pass it in psql args.
package_url="$(CI_PACKAGE_DATABASE="$database" python3 - <<'PY'
import os
from urllib.parse import urlsplit, urlunsplit

url = urlsplit(os.environ['DATABASE_URL'])
if url.scheme not in ('postgres', 'postgresql') or url.path != '/concord_test':
    raise SystemExit('ci-test-package-db: DATABASE_URL must select concord_test')
print(urlunsplit((url.scheme, url.netloc,
                  '/' + os.environ['CI_PACKAGE_DATABASE'], url.query, url.fragment)))
PY
)"

# A template prepared once by the caller avoids repeating the entire migration
# chain in every package. The test helper still checks schema version per binary.
psql --dbname=postgres --set=ON_ERROR_STOP=1 \
  --command="CREATE DATABASE \"$database\" TEMPLATE concord_test" >/dev/null
cleanup() {
  local status=$?
  trap - EXIT
  if ! psql --dbname=postgres --set=ON_ERROR_STOP=1 \
      --command="DROP DATABASE \"$database\" WITH (FORCE)" >/dev/null; then
    echo 'ci-test-package-db: could not remove package test database' >&2
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT

export DATABASE_URL="$package_url"
"$@"
