package server

import (
	"errors"
	"log/slog"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
)

// errNoCredentialsDir is a signing owner verb run without
// CREDENTIALS_DIRECTORY, which Load reads for an owner verb but never
// requires (B3-D1).
var errNoCredentialsDir = errors.New("CREDENTIALS_DIRECTORY is required for a verb that signs")

// OpenOwner turns an owner verb's loaded Config into the owner store (B3-D3).
// It makes the process non-dumpable first, as Open does. Only when needSigner
// (clear-held, B3-D1) does it read the RSK, and then the RSK alone: an owner
// verb serves nothing, so the TLS key is never read. It reads the anchors and
// the trust file under S3's rule, as the owner-CLI column requires, and hands
// both to the store: every verb reads the installed trust from the database,
// and revoke-key also matches against the file once it verifies whole under
// the anchors (B3-D10). Then it opens the authority_owner pool, which dials nothing
// until a verb uses it. closeFn releases the pool. A refusal names the
// variable, never its value (§3.6). Off Linux the RSK read refuses, so a
// signing verb is reason=config there (B2-D16).
func OpenOwner(cfg Config, needSigner bool, log *slog.Logger) (o *store.Owner, closeFn func(), err error) {
	if err := undumpable(); err != nil {
		return nil, nil, RefuseAt(StepDumpable, ReasonConfig, err)
	}
	sig, err := ownerSigner(cfg, needSigner)
	if err != nil {
		return nil, nil, RefuseAt(StepCredentials, ReasonConfig, err)
	}
	anchors, trust, err := readInputs(cfg)
	if err != nil {
		return nil, nil, RefuseAt(StepInputs, ReasonTrust, err)
	}
	db, err := openOwnerDB(cfg)
	if err != nil {
		return nil, nil, RefuseAt(StepDatabase, ReasonConfig, err)
	}
	o, err = store.OpenOwner(store.OwnerConfig{DB: db, Signer: sig, Anchors: anchors, TrustFile: trust, Logger: log})
	if err != nil {
		_ = db.Close()
		return nil, nil, RefuseAt(StepStore, ReasonTrust, err) // with the pool set, OpenOwner refuses only anchors that do not decode
	}
	return o, func() { _ = db.Close() }, nil
}

// openOwnerDB is OpenDB; a test swaps it for the fixture's owner pool, whose
// role is set by SET SESSION AUTHORIZATION rather than by a login URL that
// Load would accept, so the rest of OpenOwner still runs for real.
var openOwnerDB = OpenDB

// ownerSigner is the RSK as a signer when needSigner, and otherwise a nil
// interface, never a typed nil, so the store's nil check reads it.
func ownerSigner(cfg Config, needSigner bool) (signer.Signer, error) {
	if !needSigner {
		return nil, nil
	}
	if cfg.CredentialsDir == "" {
		return nil, errNoCredentialsDir
	}
	return loadSigner(cfg.CredentialsDir)
}

// ReadTrust reads AUTHORITY_TRUST_PATH under S3's rule, the read OpenOwner
// makes, for restore-begin, which installs the bytes it read (DoR:1744 step
// 4). A refusal names the variable, never the path, at StepInputs with
// ReasonTrust, as OpenOwner's own read refuses.
func ReadTrust(cfg Config) ([]byte, error) {
	b, err := readTrustFile("AUTHORITY_TRUST_PATH", cfg.TrustPath, trustMax)
	if err != nil {
		return nil, RefuseAt(StepInputs, ReasonTrust, err)
	}
	return b, nil
}
