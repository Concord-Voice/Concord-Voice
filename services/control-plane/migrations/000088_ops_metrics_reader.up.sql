-- 000088_ops_metrics_reader.up.sql
-- Dedicated read-only PostgreSQL login boundary for the admin metrics API
-- (#1690). PostgreSQL roles are cluster-global, so the role name is scoped to
-- the current database and carries a migration-owned comment marker. A name
-- collision without that marker is rejected rather than adopted.

DO $migration$
DECLARE
    reader_role CONSTANT text := 'concord_ops_metrics_reader_' || md5(current_database());
    owner_marker_v1 CONSTANT text := 'concord-voice:ops-metrics-reader:v1:' || current_database();
    owner_marker_prefix CONSTANT text :=
        'concord-voice:ops-metrics-reader:v2:' || current_database() || ':public-temp=';
    owner_marker text;
    reader_oid oid;
    current_database_oid oid;
    public_schema_oid oid;
    public_had_temp boolean;
BEGIN
    SELECT oid INTO current_database_oid
    FROM pg_database
    WHERE datname = current_database();

    SELECT oid INTO public_schema_oid
    FROM pg_namespace
    WHERE nspname = 'public';

    IF current_database_oid IS NULL OR public_schema_oid IS NULL THEN
        RAISE EXCEPTION 'operations metrics reader database identity is invalid'
            USING ERRCODE = '55000';
    END IF;

    SELECT EXISTS (
        SELECT 1
        FROM pg_database AS database
        CROSS JOIN LATERAL aclexplode(
            COALESCE(database.datacl, acldefault('d', database.datdba))
        ) AS privilege
        WHERE database.oid = current_database_oid
          AND privilege.grantee = 0
          AND privilege.privilege_type = 'TEMPORARY'
    ) INTO public_had_temp;

    SELECT oid, shobj_description(oid, 'pg_authid')
    INTO reader_oid, owner_marker
    FROM pg_roles
    WHERE rolname = reader_role;

    IF reader_oid IS NULL THEN
        EXECUTE format(
            'CREATE ROLE %I NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE '
            'NOINHERIT NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 2',
            reader_role
        );
        SELECT oid INTO reader_oid
        FROM pg_roles
        WHERE rolname = reader_role;
        owner_marker := owner_marker_prefix || CASE WHEN public_had_temp THEN '1' ELSE '0' END;
        EXECUTE format('COMMENT ON ROLE %I IS %L', reader_role, owner_marker);
    ELSIF owner_marker = owner_marker_v1 THEN
        -- Adopt a prerelease v1-marked role encountered before migration 000088
        -- is recorded, preserving PUBLIC TEMP state for symmetric rollback.
        owner_marker := owner_marker_prefix || CASE WHEN public_had_temp THEN '1' ELSE '0' END;
        EXECUTE format('COMMENT ON ROLE %I IS %L', reader_role, owner_marker);
    ELSIF owner_marker = owner_marker_prefix || '0' THEN
        public_had_temp := false;
    ELSIF owner_marker = owner_marker_prefix || '1' THEN
        public_had_temp := true;
    ELSE
        RAISE EXCEPTION
            'refusing to adopt operations metrics reader role without the migration ownership marker'
            USING ERRCODE = '55000';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM pg_roles
        WHERE oid = reader_oid
          AND (
              rolsuper OR rolcreatedb OR rolcreaterole OR rolinherit
              OR rolreplication OR rolbypassrls OR rolconnlimit <> 2
          )
    ) THEN
        RAISE EXCEPTION 'operations metrics reader role attributes are invalid'
            USING ERRCODE = '55000';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM pg_roles
        WHERE oid = reader_oid
          AND rolconfig IS NOT NULL
          AND NOT (
              cardinality(rolconfig) = 4
              AND 'default_transaction_read_only=on' = ANY (rolconfig)
              AND 'statement_timeout=3s' = ANY (rolconfig)
              AND 'lock_timeout=1s' = ANY (rolconfig)
              AND 'idle_in_transaction_session_timeout=5s' = ANY (rolconfig)
          )
    ) THEN
        RAISE EXCEPTION 'operations metrics reader role settings are invalid'
            USING ERRCODE = '55000';
    END IF;

    -- NOINHERIT does not prevent SET ROLE. Any membership in either direction
    -- expands or delegates the boundary, so fail rather than silently mutate it.
    IF EXISTS (
        SELECT 1
        FROM pg_auth_members
        WHERE member = reader_oid OR roleid = reader_oid
    ) THEN
        RAISE EXCEPTION 'operations metrics reader role has unexpected role memberships'
            USING ERRCODE = '55000';
    END IF;

    -- Per-role REVOKE cannot override privileges inherited from PUBLIC.
    -- PostgreSQL grants TEMP to PUBLIC by default, so remove it for this
    -- database and record whether rollback must restore it.
    EXECUTE format(
        'REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC',
        current_database()
    );

    IF has_database_privilege(reader_role, current_database(), 'CREATE')
       OR has_database_privilege(reader_role, current_database(), 'TEMPORARY') THEN
        RAISE EXCEPTION 'operations metrics reader role has unsafe database privileges'
            USING ERRCODE = '55000';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM pg_namespace
        WHERE nspname NOT LIKE 'pg\_%' ESCAPE '\'
          AND nspname <> 'information_schema'
          AND has_schema_privilege(reader_role, oid, 'CREATE')
    ) THEN
        RAISE EXCEPTION 'operations metrics reader role has effective CREATE privilege'
            USING ERRCODE = '55000';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM pg_namespace
        WHERE nspname NOT LIKE 'pg\_%' ESCAPE '\'
          AND nspname NOT IN ('information_schema', 'public')
          AND has_schema_privilege(reader_role, oid, 'USAGE')
    ) THEN
        RAISE EXCEPTION 'operations metrics reader role has unexpected schema usage'
            USING ERRCODE = '55000';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM information_schema.table_privileges
        WHERE grantee = 'PUBLIC'
          AND table_schema NOT LIKE 'pg\_%' ESCAPE '\'
          AND table_schema <> 'information_schema'
          AND privilege_type IN (
              'SELECT', 'INSERT', 'UPDATE', 'DELETE', 'TRUNCATE', 'REFERENCES', 'TRIGGER'
          )
    ) THEN
        RAISE EXCEPTION 'operations metrics reader role inherits effective PUBLIC table privileges'
            USING ERRCODE = '55000';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM pg_attribute AS attribute
        JOIN pg_class AS relation ON relation.oid = attribute.attrelid
        JOIN pg_namespace AS schema ON schema.oid = relation.relnamespace
        CROSS JOIN LATERAL aclexplode(attribute.attacl) AS privilege
        WHERE attribute.attnum > 0
          AND NOT attribute.attisdropped
          AND schema.nspname NOT LIKE 'pg\_%' ESCAPE '\'
          AND schema.nspname <> 'information_schema'
          AND privilege.grantee IN (0, reader_oid)
    ) THEN
        RAISE EXCEPTION 'operations metrics reader role has unexpected direct or PUBLIC column privileges'
            USING ERRCODE = '55000';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM information_schema.table_privileges
        WHERE grantee = reader_role
          AND NOT (
              table_schema = 'public'
              AND table_name IN ('ops_metric_samples', 'ops_metric_rollups')
              AND privilege_type = 'SELECT'
          )
    ) THEN
        RAISE EXCEPTION 'operations metrics reader role has unexpected direct table privileges'
            USING ERRCODE = '55000';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM pg_class AS sequence
        JOIN pg_namespace AS schema ON schema.oid = sequence.relnamespace
        CROSS JOIN LATERAL aclexplode(sequence.relacl) AS privilege
        WHERE sequence.relkind = 'S'
          AND schema.nspname NOT LIKE 'pg\_%' ESCAPE '\'
          AND schema.nspname <> 'information_schema'
          AND privilege.grantee IN (0, reader_oid)
    ) THEN
        RAISE EXCEPTION 'operations metrics reader role has unexpected direct or PUBLIC sequence privileges'
            USING ERRCODE = '55000';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM pg_default_acl AS defaults
        CROSS JOIN LATERAL aclexplode(defaults.defaclacl) AS privilege
        WHERE privilege.grantee IN (0, reader_oid)
          AND defaults.defaclobjtype IN ('r', 'S', 'f', 'T', 'n')
    ) THEN
        RAISE EXCEPTION 'operations metrics reader role has unsafe default privileges'
            USING ERRCODE = '55000';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM pg_proc AS function
        JOIN pg_namespace AS schema ON schema.oid = function.pronamespace
        WHERE function.prosecdef
          AND schema.nspname NOT LIKE 'pg\_%' ESCAPE '\'
          AND schema.nspname <> 'information_schema'
          AND has_schema_privilege(reader_role, schema.oid, 'USAGE')
          AND has_function_privilege(reader_role, function.oid, 'EXECUTE')
    ) THEN
        RAISE EXCEPTION 'operations metrics reader role can execute a security definer function'
            USING ERRCODE = '55000';
    END IF;

    IF EXISTS (
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
        RAISE EXCEPTION 'operations metrics reader role has unexpected shared dependencies'
            USING ERRCODE = '55000';
    END IF;

    EXECUTE format(
        'ALTER ROLE %I NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE '
        'NOINHERIT NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 2 PASSWORD NULL',
        reader_role
    );
    EXECUTE format('ALTER ROLE %I RESET ALL', reader_role);
    EXECUTE format(
        'REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM %I',
        reader_role
    );
    EXECUTE format(
        'REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM %I',
        reader_role
    );
    EXECUTE format('REVOKE CREATE ON SCHEMA public FROM %I', reader_role);
    EXECUTE format('GRANT USAGE ON SCHEMA public TO %I', reader_role);
    EXECUTE format(
        'GRANT SELECT ON ops_metric_samples, ops_metric_rollups TO %I',
        reader_role
    );
    EXECUTE format('ALTER ROLE %I SET default_transaction_read_only = on', reader_role);
    EXECUTE format('ALTER ROLE %I SET statement_timeout = %L', reader_role, '3s');
    EXECUTE format('ALTER ROLE %I SET lock_timeout = %L', reader_role, '1s');
    EXECUTE format(
        'ALTER ROLE %I SET idle_in_transaction_session_timeout = %L',
        reader_role,
        '5s'
    );

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
        RAISE EXCEPTION 'operations metrics reader role dependency boundary is invalid'
            USING ERRCODE = '55000';
    END IF;
END
$migration$;
