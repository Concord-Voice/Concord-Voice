-- Account-authority test databases (#3587 PR-B1, DoR §3.2.3). Idempotent.
-- Run as a cluster superuser against database `postgres`:
--   psql --dbname=postgres --set=ON_ERROR_STOP=1 --file=services/control-plane/scripts/authority-test-bootstrap.sql
-- NEVER copy this into infrastructure/docker/postgres/init/: that directory is
-- mounted into the production and self-host CP clusters, and authority roles
-- must never exist there (D-109). Production roles come from #3593's provisioner.
\set ON_ERROR_STOP on

DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'authority_owner') THEN
    CREATE ROLE authority_owner NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'authority_app') THEN
    CREATE ROLE authority_app NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'authority_backup') THEN
    CREATE ROLE authority_backup NOLOGIN;
  END IF;
END
$$;

SELECT 'CREATE DATABASE concord_authority_test OWNER authority_owner'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'concord_authority_test') \gexec
SELECT 'CREATE DATABASE concord_authority_dirty_test OWNER authority_owner'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'concord_authority_dirty_test') \gexec

-- B1-D2: the schema exists, and search_path names it, before the first
-- migration runs, so golang-migrate's schema_migrations lands in `authority`
-- on every run (it uses current_schema()).
\connect concord_authority_test
CREATE SCHEMA IF NOT EXISTS authority AUTHORIZATION authority_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
ALTER DATABASE concord_authority_test SET search_path = authority;

\connect concord_authority_dirty_test
CREATE SCHEMA IF NOT EXISTS authority AUTHORIZATION authority_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
ALTER DATABASE concord_authority_dirty_test SET search_path = authority;
