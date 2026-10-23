package server

import (
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
)

// ownerEnv is an owner verb's environment over real anchors and a trust file,
// with no CREDENTIALS_DIRECTORY.
func ownerEnv(t *testing.T) map[string]string {
	t.Helper()
	clearLibpqEnv(t)
	files := t.TempDir()
	for name, b := range map[string][]byte{"anchors": authoritytest.NewRealm(t).AnchorBytes, "trust": []byte("x")} {
		if err := os.WriteFile(filepath.Join(files, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]string{
		"AUTHORITY_TRUST_PATH":           filepath.Join(files, "trust"),
		"AUTHORITY_ANCHORS_PATH":         filepath.Join(files, "anchors"),
		"AUTHORITY_MIGRATE_DATABASE_URL": socketURL("authority_owner", ""),
	}
}

// ownerLoad is Load(VerbOwner) over env, which must pass.
func ownerLoad(t *testing.T, env map[string]string) Config {
	t.Helper()
	cfg, err := Load(VerbOwner, environOf(env))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// credDir is a 0700 directory holding a fresh P-384 key under each name, at
// mode.
func credDir(t *testing.T, mode os.FileMode, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- a credentials directory is 0700
		t.Fatal(err)
	}
	for _, name := range names {
		der, err := x509.MarshalPKCS8PrivateKey(newKey(t))
		if err != nil {
			t.Fatal(err)
		}
		writeCred(t, dir, name, der, mode)
	}
	return dir
}

// dumpableOK stubs the prctl, so a test leaves the test process dumpable.
func dumpableOK(t *testing.T) {
	t.Helper()
	hook := undumpable
	t.Cleanup(func() { undumpable = hook })
	undumpable = func() error { return nil }
}

// B3-D1: Load reads CREDENTIALS_DIRECTORY for an owner verb when it is set,
// and never requires it. Unread, clear-held could never find its RSK; judged
// only when read, a malformed value is still refused.
func TestOwnerLoadReadsTheCredentialsDirectoryWhenSet(t *testing.T) {
	env := ownerEnv(t)
	if cfg := ownerLoad(t, env); cfg.CredentialsDir != "" {
		t.Fatalf("unset: CredentialsDir %q, want empty", cfg.CredentialsDir)
	}
	dir := t.TempDir()
	env["CREDENTIALS_DIRECTORY"] = dir
	if cfg := ownerLoad(t, env); cfg.CredentialsDir != dir {
		t.Fatalf("set: CredentialsDir %q, want %q", cfg.CredentialsDir, dir)
	}
	env["CREDENTIALS_DIRECTORY"] = ""
	if cfg := ownerLoad(t, env); cfg.CredentialsDir != "" {
		t.Fatalf("set empty: CredentialsDir %q, want empty", cfg.CredentialsDir)
	}
	env["CREDENTIALS_DIRECTORY"] = "relative/creds"
	if _, err := Load(VerbOwner, environOf(env)); err == nil {
		t.Fatal("a relative CREDENTIALS_DIRECTORY loaded")
	}
}

// OpenOwner makes the process non-dumpable before it reads anything, whether
// or not the verb signs, and a failure to is a refusal, reason=config.
func TestOpenOwnerRefusesWhenTheProcessStaysDumpable(t *testing.T) {
	env := ownerEnv(t)
	env["CREDENTIALS_DIRECTORY"] = credDir(t, 0o400, credRSK)
	cfg := ownerLoad(t, env)
	sentinel := errors.New("prctl refused")
	hook := undumpable
	t.Cleanup(func() { undumpable = hook })
	undumpable = func() error { return sentinel }
	for _, needSigner := range []bool{false, true} {
		o, closeFn, err := OpenOwner(cfg, needSigner, quiet)
		if exitReason(err) != ReasonConfig || exitStep(err) != StepDumpable || !errors.Is(err, sentinel) || o != nil || closeFn != nil {
			t.Fatalf("needSigner=%v: %v, want reason config at step dumpable", needSigner, err)
		}
	}
	undumpable = func() error { return nil }
	if _, closeFn, err := OpenOwner(cfg, false, quiet); err != nil {
		t.Fatalf("control: %v", err)
	} else {
		closeFn()
	}
}

// B3-D1: only a signing verb reads a credential, and then the RSK alone. A
// verb that does not sign opens beside an RSK that would refuse, so it read
// none; a signing verb refuses that RSK; and a directory holding the RSK and
// no TLS key is enough, since an owner verb never reads the TLS key.
func TestOpenOwnerReadsTheRSKOnlyWhenItSigns(t *testing.T) {
	dumpableOK(t)
	env := ownerEnv(t)
	env["CREDENTIALS_DIRECTORY"] = credDir(t, 0o444, credRSK) // open to group and other: fileRule refuses it
	bad := ownerLoad(t, env)
	o, closeFn, err := OpenOwner(bad, false, quiet)
	if err != nil || o == nil {
		t.Fatalf("a verb that does not sign: %v, want it open without reading the RSK", err)
	}
	closeFn()
	if _, _, err := OpenOwner(bad, true, quiet); exitReason(err) != ReasonConfig || exitStep(err) != StepCredentials {
		t.Fatalf("a signing verb over a refused RSK: %v, want reason config at step credentials", err)
	}

	env["CREDENTIALS_DIRECTORY"] = credDir(t, 0o400, credRSK)
	o, closeFn, err = OpenOwner(ownerLoad(t, env), true, quiet)
	if runtime.GOOS != "linux" {
		if exitReason(err) != ReasonConfig || exitStep(err) != StepCredentials {
			t.Fatalf("a signing verb on %s: %v, want reason config at step credentials (B2-D16)", runtime.GOOS, err)
		}
		return
	}
	if err != nil || o == nil {
		t.Fatalf("a signing verb over the RSK alone: %v", err)
	}
	closeFn()
}

// A signing verb run without CREDENTIALS_DIRECTORY is refused by name, before
// any read.
func TestOpenOwnerSignerNeedsTheCredentialsDirectory(t *testing.T) {
	dumpableOK(t)
	_, _, err := OpenOwner(ownerLoad(t, ownerEnv(t)), true, quiet)
	if exitReason(err) != ReasonConfig || exitStep(err) != StepCredentials || !errors.Is(err, errNoCredentialsDir) {
		t.Fatalf("%v, want reason config at step credentials naming CREDENTIALS_DIRECTORY", err)
	}
}

// The inputs and the pool refuse as serve's do: missing anchors are trust at
// the inputs step, anchors that do not decode are trust at the store step,
// and a Config Load did not check never dials.
func TestOpenOwnerRefusesItsInputs(t *testing.T) {
	dumpableOK(t)
	env := ownerEnv(t)
	cfg := ownerLoad(t, env)

	missing := cfg
	missing.AnchorsPath = filepath.Join(filepath.Dir(cfg.AnchorsPath), "missing")
	if _, _, err := OpenOwner(missing, false, quiet); exitReason(err) != ReasonTrust || exitStep(err) != StepInputs {
		t.Errorf("missing anchors: %v, want reason trust at step inputs", err)
	}

	if err := os.WriteFile(cfg.AnchorsPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenOwner(cfg, false, quiet); exitReason(err) != ReasonTrust || exitStep(err) != StepStore {
		t.Errorf("undecodable anchors: %v, want reason trust at step store", err)
	}

	unchecked := Config{TrustPath: cfg.TrustPath, AnchorsPath: cfg.AnchorsPath}
	if _, _, err := OpenOwner(unchecked, false, quiet); exitReason(err) != ReasonConfig || exitStep(err) != StepDatabase {
		t.Errorf("an unchecked Config: %v, want reason config at step database", err)
	}
}
