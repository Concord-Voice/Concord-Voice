#!/bin/sh
# Fresh clusters install these rules from docker-entrypoint-initdb.d after
# initdb creates pg_hba.conf. Existing volumes need them reconciled before
# PostgreSQL starts so the reader login can never escape its target database.
if [ -s "${PGDATA:-/var/lib/postgresql/data}/PG_VERSION" ]; then
  /usr/local/bin/configure-ops-metrics-hba.sh
fi

exec docker-entrypoint.sh "$@"
