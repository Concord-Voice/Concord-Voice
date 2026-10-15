package authoritydb

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestCheckEnvNeverReadsDatabaseURL(t *testing.T) {
	_, skip, err := CheckEnv(envOf(map[string]string{"DATABASE_URL": "postgres://x@h/concord_authority_test"}), EnvURL, DBName)
	if err != nil || !skip {
		t.Fatalf("CheckEnv with only DATABASE_URL set = (skip %v, err %v), want a local skip", skip, err)
	}
}

func TestCheckEnvFailsUnderCIInsteadOfSkipping(t *testing.T) {
	_, skip, err := CheckEnv(envOf(map[string]string{"CI": "true"}), EnvURL, DBName)
	if err == nil || skip {
		t.Fatalf("CheckEnv under CI=true with no URL = (skip %v, err %v), want an error", skip, err)
	}
}

func TestCheckEnvRefusesAnyOtherDatabaseName(t *testing.T) {
	for _, name := range []string{"concord", "concord_test", "concord_authority_test_x", "postgres"} {
		dsn := "postgres://u:secretpw@localhost:5432/" + name + "?sslmode=disable" // pragma: allowlist secret
		_, _, err := CheckEnv(envOf(map[string]string{EnvURL: dsn}), EnvURL, DBName)
		if err == nil {
			t.Fatalf("database %q was accepted", name)
		}
		if strings.Contains(err.Error(), "secretpw") {
			t.Fatalf("error echoes the URL's password: %v", err)
		}
	}
}

func TestCheckEnvAcceptsTheExactName(t *testing.T) {
	dsn := "postgres://u@localhost:5432/concord_authority_test?sslmode=disable"
	got, skip, err := CheckEnv(envOf(map[string]string{EnvURL: dsn}), EnvURL, DBName)
	if err != nil || skip || got != dsn {
		t.Fatalf("CheckEnv = (%q, %v, %v)", got, skip, err)
	}
}

func TestOpenAsSwitchesEveryPooledConnection(t *testing.T) {
	dsn := URL(t, EnvURL, DBName)
	for _, role := range []string{RoleApp, RoleOwner} {
		db := OpenAs(t, dsn, role)
		db.SetMaxOpenConns(3)
		conns := make([]string, 0, 3)
		ctx := context.Background()
		held := make([]func() error, 0, 3)
		for i := 0; i < 3; i++ { // three distinct pooled connections
			c, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, c.Close)
			var who string
			if err := c.QueryRowContext(ctx, "SELECT current_user").Scan(&who); err != nil {
				t.Fatal(err)
			}
			conns = append(conns, who)
		}
		for _, closeFn := range held {
			_ = closeFn()
		}
		for _, who := range conns {
			if who != role {
				t.Fatalf("connection runs as %q, want %q", who, role)
			}
		}
	}
}

// It reads as the owner: authority_app has no USAGE on the schema until 000001
// grants it, and PostgreSQL leaves a schema without USAGE out of the effective
// search path, so current_schema() is NULL for authority_app on a database no
// migration has touched. search_path is a database setting, the same for every role.
func TestBootstrapSetSearchPathAndSchemaOwner(t *testing.T) {
	db := OpenAs(t, URL(t, EnvURL, DBName), RoleOwner)
	var schema, owner string
	if err := db.QueryRow(`SELECT current_schema(), pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = current_schema()`).
		Scan(&schema, &owner); err != nil {
		t.Fatal(err)
	}
	if schema != "authority" || owner != RoleOwner {
		t.Fatalf("current_schema %q owned by %q, want authority owned by authority_owner (B1-D2)", schema, owner)
	}
}

// lib/pq v1.12.3 lets a query key override the URL path (connector.go:1146–1156:
// the path and every query key become sorted dbname= pairs, the last one wins),
// and service= can name a database too. CheckEnv checks only the path, so OpenAs
// asks the server before Setup migrates or truncates anything (review round 4).
func TestCheckDatabaseRefusesAQueryOverride(t *testing.T) {
	dsn := URL(t, EnvURL, DBName)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	over := dsn + sep + "dbname=postgres"
	db, err := sql.Open("postgres", over)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var got string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&got); err != nil || got != "postgres" {
		t.Fatalf("control: the override reached %q (%v), want postgres; without it this test proves nothing", got, err)
	}
	if err := checkDatabase(db, over); err == nil {
		t.Fatal("checkDatabase accepted a connection to postgres for a URL whose path names concord_authority_test")
	}
	if err := checkDatabase(OpenAs(t, dsn, RoleSuper), dsn); err != nil {
		t.Fatalf("checkDatabase refused the plain URL: %v", err)
	}
}
