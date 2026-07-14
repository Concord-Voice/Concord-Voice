-- 000088_ops_metrics_reader.down.sql
-- Remove only the database-scoped role proven to belong to this migration.
-- Shared dependencies are restricted to the three ACL grants owned by this
-- database before DROP ROLE is attempted.

DO $migration$
DECLARE
    reader_role CONSTANT text := 'concord_ops_metrics_reader_' || md5(current_database());
    owner_marker_prefix CONSTANT text :=
        'concord-voice:ops-metrics-reader:v2:' || current_database() || ':public-temp=';
    owner_marker text;
    reader_oid oid;
    current_database_oid oid;
    public_schema_oid oid;
    restore_public_temp boolean;
BEGIN
    SELECT oid INTO current_database_oid
    FROM pg_database
    WHERE datname = current_database();

    SELECT oid INTO public_schema_oid
    FROM pg_namespace
    WHERE nspname = 'public';

    SELECT oid, shobj_description(oid, 'pg_authid')
    INTO reader_oid, owner_marker
    FROM pg_roles
    WHERE rolname = reader_role;

    IF reader_oid IS NULL THEN
        RETURN;
    END IF;
    IF owner_marker = owner_marker_prefix || '0' THEN
        restore_public_temp := false;
    ELSIF owner_marker = owner_marker_prefix || '1' THEN
        restore_public_temp := true;
    ELSE
        RAISE EXCEPTION
            'refusing to drop operations metrics reader role without the migration ownership marker'
            USING ERRCODE = '55000';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM pg_auth_members
        WHERE member = reader_oid OR roleid = reader_oid
    ) THEN
        RAISE EXCEPTION 'refusing to drop operations metrics reader role with role memberships'
            USING ERRCODE = '55000';
    END IF;

    IF (
        SELECT count(*)
        FROM pg_shdepend
        WHERE refclassid = 'pg_authid'::regclass
          AND refobjid = reader_oid
    ) <> 3 OR EXISTS (
        SELECT 1
        FROM pg_shdepend AS dependency
        WHERE dependency.refclassid = 'pg_authid'::regclass
          AND dependency.refobjid = reader_oid
          AND NOT (
              dependency.dbid = current_database_oid
              AND dependency.deptype = 'a'
              AND dependency.objsubid = 0
              AND (
                  (
                      dependency.classid = 'pg_namespace'::regclass
                      AND dependency.objid = public_schema_oid
                  )
                  OR (
                      dependency.classid = 'pg_class'::regclass
                      AND dependency.objid IN (
                          'public.ops_metric_samples'::regclass,
                          'public.ops_metric_rollups'::regclass
                      )
                  )
              )
          )
    ) THEN
        RAISE EXCEPTION 'refusing to drop operations metrics reader role with shared dependencies'
            USING ERRCODE = '55000';
    END IF;

    EXECUTE format('ALTER ROLE %I NOLOGIN PASSWORD NULL', reader_role);
    EXECUTE format('ALTER ROLE %I RESET ALL', reader_role);
    EXECUTE format(
        'REVOKE SELECT ON ops_metric_samples, ops_metric_rollups FROM %I',
        reader_role
    );
    EXECUTE format('REVOKE USAGE ON SCHEMA public FROM %I', reader_role);
    EXECUTE format(
        'REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM %I',
        reader_role
    );
    EXECUTE format(
        'REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM %I',
        reader_role
    );
    EXECUTE format('DROP ROLE %I', reader_role);

    IF restore_public_temp THEN
        EXECUTE format(
            'GRANT TEMPORARY ON DATABASE %I TO PUBLIC',
            current_database()
        );
    END IF;
END
$migration$;
