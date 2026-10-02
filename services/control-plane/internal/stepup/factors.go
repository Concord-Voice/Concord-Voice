package stepup

// The step-up factor read: which inline factors this account can verify, which
// one to offer first, and whether a backup code is still spendable. It backs
// GET /mfa/step-up and the advisory default_method on the login, refresh and
// SSO MFA challenges. It is presentation input only — every gate still reads
// P1 through InlineMFAMethods, LoadSubject or LockSubjectTx, none of which this
// file changes.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Factor names, as P1 and the wire spell them.
const (
	MethodTOTP     = "totp"
	MethodWebAuthn = "webauthn"
)

// totpStepSeconds is the TOTP period (mfa/totp.go totpPeriod). A step N was
// accepted somewhere in [N*30-30, N*30+60) because the verifier allows one
// period of skew each way, so recency read from a step is precise to about a
// minute (spec §3). Ordering two factors used within that band can invert; the
// cost is one link click, and a new column would add a writer to the
// replay-guard UPDATE for a presentation default.
const totpStepSeconds = 30

// inlineMFAFactorsSQL reads P1 plus the recency and backup-code facts the
// picker defaults on. It is anchored on the users row so that a deleted user
// is sql.ErrNoRows — a 401 — never an empty factor set (spec §2, SE-1).
//
// backup_code_available pairs the two parallel arrays with unnest, which pads
// the shorter with NULL, so it agrees with mfa.VerifyBackupCode on a damaged
// row: a hash with no used flag is not spendable (the verifier skips
// i >= len(used)).
const inlineMFAFactorsSQL = `
	SELECT ` + p1TOTPActive + `,
	       (SELECT last_used_step FROM user_mfa_totp WHERE user_id = $1 AND enabled AND confirmed),
	       ` + p1WebAuthnAny + `,
	       (SELECT MAX(COALESCE(last_used_at, created_at)) FROM user_mfa_webauthn WHERE user_id = $1),
	       EXISTS (SELECT 1
	               FROM user_mfa_totp t, unnest(t.backup_codes_hash, t.backup_codes_used) AS b(h, used)
	               WHERE t.user_id = $1 AND t.enabled AND t.confirmed
	                 AND b.h IS NOT NULL AND b.used = FALSE)
	FROM users u WHERE u.id = $1`

// Factors is one account's step-up factor state. The last-use times are
// unexported so they cannot be serialized by accident: they are recency-derived
// account posture and are never logged or sent (spec §2, Disclosure).
type Factors struct {
	// Methods is the P1 set: "totp" and/or "webauthn", never nil.
	Methods []string
	// BackupCodeAvailable is true when TOTP is active and at least one backup
	// code is unspent.
	BackupCodeAvailable bool

	totpLastUse     *time.Time
	webauthnLastUse *time.Time
}

// Default is the factor to offer first: the most recently used, then the
// strongest. "" when Methods is empty.
func (f Factors) Default() string {
	return chooseDefault(f.Methods, f.totpLastUse, f.webauthnLastUse)
}

// DefaultWithin is Default restricted to the factors a surface offers. A
// challenge that offers only recovery methods therefore gets "".
func (f Factors) DefaultWithin(allowed []string) string {
	within := make([]string, 0, len(f.Methods))
	for _, m := range f.Methods {
		for _, a := range allowed {
			if m == a {
				within = append(within, m)
				break
			}
		}
	}
	return chooseDefault(within, f.totpLastUse, f.webauthnLastUse)
}

// chooseDefault picks within methods (spec §3):
//  1. none → "";
//  2. one → it;
//  3. both timed → the later, an exact tie going to webauthn;
//  4. only one timed → that one;
//  5. neither timed → webauthn, the stronger factor.
//
// Backup codes are never a default and never record recency.
func chooseDefault(methods []string, totpAt, webauthnAt *time.Time) string {
	var hasTOTP, hasWebAuthn bool
	for _, m := range methods {
		switch m {
		case MethodTOTP:
			hasTOTP = true
		case MethodWebAuthn:
			hasWebAuthn = true
		}
	}
	switch {
	case !hasTOTP && !hasWebAuthn:
		return ""
	case !hasWebAuthn:
		return MethodTOTP
	case !hasTOTP:
		return MethodWebAuthn
	case totpAt != nil && webauthnAt != nil:
		if totpAt.After(*webauthnAt) {
			return MethodTOTP
		}
		return MethodWebAuthn
	case totpAt != nil:
		return MethodTOTP
	default:
		return MethodWebAuthn
	}
}

// InlineMFAFactors reads the account's factor state. A missing users row is
// subjectGone() (401); any other read failure is a 500 with Cause set — never
// an empty Factors standing in for "unknown".
func InlineMFAFactors(ctx context.Context, q RowQuerier, userID string) (Factors, *Error) {
	var totp, webauthn, backup bool
	var totpStep sql.NullInt64
	var webauthnAt sql.NullTime
	err := q.QueryRowContext(ctx, inlineMFAFactorsSQL, userID).
		Scan(&totp, &totpStep, &webauthn, &webauthnAt, &backup)
	if errors.Is(err, sql.ErrNoRows) {
		return Factors{}, subjectGone()
	}
	if err != nil {
		return Factors{}, verificationFailed(fmt.Errorf("read step-up factors: %w", err))
	}

	f := Factors{Methods: inlineMethods(totp, webauthn), BackupCodeAvailable: backup}
	if totp && totpStep.Valid {
		t := time.Unix(totpStep.Int64*totpStepSeconds, 0)
		f.totpLastUse = &t
	}
	if webauthn && webauthnAt.Valid {
		t := webauthnAt.Time
		f.webauthnLastUse = &t
	}
	return f, nil
}
