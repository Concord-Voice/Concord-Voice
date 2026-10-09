#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
task_tmp="$(mktemp -d)"
trap 'rm -rf "$task_tmp"' EXIT
mkdir -p "$task_tmp/bin"

cat > "$task_tmp/bin/psql" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PSQL_CALLS"
SH
cat > "$task_tmp/bin/fixture.test" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$DATABASE_URL" > "$TEST_URL_OUTPUT"
exit "${TEST_BINARY_EXIT:-0}"
SH
chmod +x "$task_tmp/bin/psql" "$task_tmp/bin/fixture.test"

export PATH="$task_tmp/bin:$PATH"
export CI_PACKAGE_DB_ISOLATION=1
fixture_password="$(python3 -c 'import secrets; print(secrets.token_hex(12))')"
export DATABASE_URL="postgres://ci:${fixture_password}@localhost:5432/concord_test?sslmode=disable"
export PSQL_CALLS="$task_tmp/psql-calls"
export TEST_URL_OUTPUT="$task_tmp/test-url"
export RUNNER_TEMP="$task_tmp"

bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/bin/fixture.test"
[[ "$(wc -l < "$PSQL_CALLS")" -eq 2 ]]
created="$(sed -n '1p' "$PSQL_CALLS")"
dropped="$(sed -n '2p' "$PSQL_CALLS")"
[[ "$created" =~ CREATE\ DATABASE\ \"(concord_ci_[a-f0-9]{16}_test)\"\ TEMPLATE\ concord_test ]]
database="${BASH_REMATCH[1]}"
[[ "$dropped" == *"DROP DATABASE \"$database\" WITH (FORCE)"* ]]
[[ "$(cat "$TEST_URL_OUTPUT")" == "postgres://ci:${fixture_password}@localhost:5432/$database?sslmode=disable" ]]

: > "$PSQL_CALLS"
if TEST_BINARY_EXIT=7 bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/bin/fixture.test"; then
  echo 'FAIL: test binary failure was hidden' >&2
  exit 1
else
  [[ "$?" -eq 7 ]]
fi
[[ "$(wc -l < "$PSQL_CALLS")" -eq 2 ]]

: > "$PSQL_CALLS"
invalid_database_url="postgres://ci:${fixture_password}@localhost:5432/concord?sslmode=disable"
if DATABASE_URL="$invalid_database_url" \
    bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/bin/fixture.test" 2>/dev/null; then
  echo 'FAIL: non-template DATABASE_URL was accepted' >&2
  exit 1
fi
[[ ! -s "$PSQL_CALLS" ]]

# Both packages observe fixed NATS subjects. A second binary entering while
# the first still holds the shared lock would fail the atomic mkdir.
cat > "$task_tmp/bin/voice.test" <<'SH'
#!/usr/bin/env bash
mkdir "$NATS_BUSY"
sleep 0.5
rmdir "$NATS_BUSY"
SH
cp "$task_tmp/bin/voice.test" "$task_tmp/bin/dm.test"
chmod +x "$task_tmp/bin/voice.test" "$task_tmp/bin/dm.test"
export NATS_BUSY="$task_tmp/nats-busy"
bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/bin/voice.test" &
voice_pid=$!
bash "$SCRIPT_DIR/ci-test-package-db.sh" "$task_tmp/bin/dm.test" &
dm_pid=$!
wait "$voice_pid"
wait "$dm_pid"

echo 'PASS: package DB wrapper clones, isolates, cleans up, preserves failures, and serializes shared NATS observers'
