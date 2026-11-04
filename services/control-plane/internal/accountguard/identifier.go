package accountguard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
)

// RowQuerier is the narrow DB surface identifier resolution needs: the history lookup
// reads several rows, so it is QueryContext and not QueryRowContext. Satisfied by
// *sql.DB, *sql.Tx and *sql.Conn.
type RowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Resolution is the outcome of mapping a sign-in identifier to an account. UserID is
// the zero uuid and Resolved is false when the identifier names no single account.
// SubjectHMAC is never the zero value on a successful call.
type Resolution struct {
	UserID      uuid.UUID
	Resolved    bool
	SubjectHMAC [32]byte
}

// The subject prefixes. They differ, so the digest of a resolved account can never equal
// the digest of an unresolved identifier, whatever that identifier is spelled like.
const (
	acctSubjectPrefix  = "acct"
	identSubjectPrefix = "ident"
)

// identifierByteLimit bounds the input a lookup may carry: an email address is at most 254
// bytes and a username is shorter, so a longer input names no account.
const identifierByteLimit = 320

// The three lookups. Each compares lower($1) against the column, so the one lowercasing
// function runs on both sides of the comparison, as the migration that stores the
// addresses folds them. Each reads at most two rows: a second row is all it takes to know
// the arm is ambiguous, and an unbounded read would make the work depend on the input.
// $1 is the identifier to look up (the trimmed input, or "" for one the database must not
// see: see identifierByteLimit); $2, in the history arm, is the start of the window.
//
// users.email is unique on its raw value, so differently-cased spellings can coexist and
// the email arm can match two accounts. The history arm groups by account, so one account
// named by several rows counts once; it resolves only a primary address, never a backup.
const (
	resolveByUsernameSQL = `SELECT id FROM users WHERE LOWER(username) = lower($1) LIMIT 2`
	resolveByEmailSQL    = `SELECT id FROM users WHERE LOWER(email) = lower($1) LIMIT 2`
	resolveByHistorySQL  = `SELECT user_id FROM user_credential_history
		WHERE kind = 'address' AND address = lower($1) AND was_primary AND retired_at > $2
		GROUP BY user_id LIMIT 2`
)

// ResolveIdentifier maps the identifier raw to the account it names, doing the same
// database work for every input: the username, email and retired-address lookups all
// run, in that order, whether or not an earlier one found an account, and an empty
// identifier runs them too. A caller that could tell a known identifier from an unknown
// one by how long this takes, or by how many queries it ran, would have an enumeration
// oracle.
//
// An input that is not valid UTF-8, holds a NUL byte, or is longer than identifierByteLimit
// is never sent to the driver, whose rejection of it echoes the input's bytes: the three
// lookups run with "" in its place, and the call is unresolved without an error. An empty
// identifier, whether trimmed to nothing or substituted so, never resolves, even when an
// account holds an empty username, email or address.
//
// An identifier resolves only when exactly one account matches on the arm that names
// it. The first arm that matches wins (username, then email, then an address the account
// retired as its primary within decide.HistoryRetention of now; the window is open at its
// start). An arm that matches more than one account makes the whole call unresolved: the
// later arms still run, but none may resolve, so an address that two current accounts
// hold is never handed to a former holder. A backup address resolves nothing.
//
// SubjectHMAC keys the account budgets: HMAC-SHA-256 under the identifier key over
// "acct" || the account's uuid bytes when resolved, and over "ident" || the lowercased,
// trimmed identifier when not: strings.ToLower of the trimmed input, even when the lookups
// ran with "", so each invalid UTF-8 byte becomes U+FFFD and malformed inputs share one
// subject. The unresolved form lowercases in Go, which can fold a non-ASCII letter differently
// from the database; that costs only the grouping of two spellings of one unknown
// identifier, never an account.
//
// The key ring is resolved before any query, so a ring that cannot derive its key wraps
// ErrKeyUnavailable and runs nothing. Any other failure is wrapped and returned with the
// zero Resolution, even when an earlier arm had found an account; no error carries the
// input, and a lookup's driver error is withheld (withheld: reachable through errors.Is and
// errors.As, never in the text). Nothing is logged: the identifier is the caller's secret.
func ResolveIdentifier(ctx context.Context, q RowQuerier, keys *KeyRing, raw string, now time.Time) (Resolution, error) {
	key, err := keys.Key(LabelIdentifier)
	if err != nil {
		return Resolution{}, fmt.Errorf("resolve identifier: %w", err)
	}

	trimmed := strings.TrimSpace(raw)
	lookup := trimmed
	if !utf8.ValidString(trimmed) || strings.IndexByte(trimmed, 0) >= 0 || len(trimmed) > identifierByteLimit {
		lookup = ""
	}
	windowStart := now.Add(-decide.HistoryRetention)

	// All three run before anything is decided, so no result can shorten the work.
	nameID, nameMatches, err := lookupAccounts(ctx, q, resolveByUsernameSQL, lookup)
	if err != nil {
		return Resolution{}, fmt.Errorf("resolve identifier: username lookup: %w", withhold(err))
	}
	emailID, emailMatches, err := lookupAccounts(ctx, q, resolveByEmailSQL, lookup)
	if err != nil {
		return Resolution{}, fmt.Errorf("resolve identifier: email lookup: %w", withhold(err))
	}
	historyID, historyMatches, err := lookupAccounts(ctx, q, resolveByHistorySQL, lookup, windowStart)
	if err != nil {
		return Resolution{}, fmt.Errorf("resolve identifier: address history lookup: %w", withhold(err))
	}

	if lookup != "" && nameMatches <= 1 && emailMatches <= 1 && historyMatches <= 1 {
		for _, arm := range []struct {
			id      uuid.UUID
			matches int
		}{{nameID, nameMatches}, {emailID, emailMatches}, {historyID, historyMatches}} {
			if arm.matches == 1 {
				return Resolution{
					UserID:      arm.id,
					Resolved:    true,
					SubjectHMAC: subjectDigest(key, acctSubjectPrefix, arm.id[:]),
				}, nil
			}
		}
	}

	return Resolution{
		SubjectHMAC: subjectDigest(key, identSubjectPrefix, []byte(strings.ToLower(trimmed))),
	}, nil
}

// lookupAccounts runs one lookup, which is written with LIMIT 2, and reads every row it
// returns. matches is the number of rows (0, 1 or 2) and id is the first row's account;
// id means nothing unless matches is 1.
func lookupAccounts(ctx context.Context, q RowQuerier, query string, args ...any) (id uuid.UUID, matches int, err error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return uuid.Nil, 0, err
	}
	defer func() {
		// A rows result that cannot be closed is not known to be fully read: refuse it.
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			id, matches, err = uuid.Nil, 0, fmt.Errorf("close rows: %w", closeErr)
		}
	}()

	for rows.Next() {
		var got uuid.UUID
		if err := rows.Scan(&got); err != nil {
			return uuid.Nil, 0, fmt.Errorf("scan: %w", err)
		}
		if matches == 0 {
			id = got
		}
		matches++
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, 0, fmt.Errorf("read rows: %w", err)
	}
	return id, matches, nil
}

// subjectDigest returns HMAC-SHA-256 under key over prefix || body.
func subjectDigest(key []byte, prefix string, body []byte) [32]byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(prefix)) // hash.Hash.Write never returns an error
	mac.Write(body)
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}
