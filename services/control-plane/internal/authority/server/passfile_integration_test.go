package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// createLoginRole makes a LOGIN role. Role DDL takes no parameters, so the
// server quotes it with format() and the statement it returns is run. The
// verifier is scram because PostgreSQL 16 defaults password_encryption to
// it; the test asserts that rather than setting it, since a SET on a pooled
// *sql.DB need not reach the connection the next Exec uses.
func createLoginRole(t *testing.T, super *sql.DB, role, secret string) {
	t.Helper()
	var stmt string
	if err := super.QueryRow(`SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', $1::text, $2::text)`, role, secret).Scan(&stmt); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(stmt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var drop string
		if err := super.QueryRow(`SELECT format('DROP ROLE IF EXISTS %I', $1::text)`, role).Scan(&drop); err == nil {
			_, _ = super.Exec(drop)
		}
	})
}

// passfileURL is the harness URL with role as its user, no password, and a
// passfile holding secret.
func passfileURL(t *testing.T, base *url.URL, role, secret string) string {
	t.Helper()
	port := base.Port()
	if port == "" {
		port = "5432"
	}
	line := strings.Join([]string{base.Hostname(), port, strings.TrimPrefix(base.Path, "/"), role, secret}, ":") + "\n"
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile("pgpass", []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	pf := filepath.Join(dir, "pgpass")
	u := *base
	u.User = url.User(role)
	q := base.Query()
	q.Set("passfile", pf)
	u.RawQuery = q.Encode()
	return u.String()
}

// currentUserOver connects with dsn and names the session's user. It is
// distinct from config_test.go's ping, which only makes an attempt.
func currentUserOver(dsn string) (string, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	var who string
	err = db.QueryRow(`SELECT current_user`).Scan(&who)
	return who, err
}

// D-173 (B2-D14): lib/pq authenticates by scram through a passfile, with no
// password in the URL. It runs over TCP, the only transport CI's PostgreSQL
// offers; the socket-only rule is the loader's (Task 6).
func TestPassfileAuthenticates(t *testing.T) {
	// lib/pq reads these from the environment, and PGPASSWORD would win over
	// the passfile: unset both for the test, restored at cleanup.
	for _, k := range []string{"PGPASSWORD", "PGPASSFILE"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	f := authoritydb.Setup(t)
	base, err := url.Parse(authoritydb.URL(t, authoritydb.EnvURL, authoritydb.DBName))
	if err != nil {
		t.Fatal(err)
	}
	role, secret := "authority_pf_"+randHex(t, 6), randHex(t, 24) // pragma: allowlist secret
	createLoginRole(t, f.Super, role, secret)
	var scram bool
	if err := f.Super.QueryRow(`SELECT rolpassword LIKE 'SCRAM-SHA-256$%' FROM pg_authid WHERE rolname = $1`, role).Scan(&scram); err != nil || !scram {
		t.Fatalf("the role's password is not a scram verifier: %v", err)
	}
	if who, err := currentUserOver(passfileURL(t, base, role, secret)); err != nil || who != role {
		t.Fatalf("passfile login: %q %v", who, err)
	}
	// Negative control: a wrong secret in the file is refused, so this
	// connection really checks a password and the passfile was what passed.
	if _, err := currentUserOver(passfileURL(t, base, role, randHex(t, 24))); err == nil {
		t.Fatal("a wrong passfile secret connected: the cluster does not check passwords over TCP (pg_hba trust?); the proof needs scram")
	}
}
