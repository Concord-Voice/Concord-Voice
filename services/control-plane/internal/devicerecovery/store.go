package devicerecovery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/google/uuid"
)

// ErrNotFound conceals missing and unowned requests alike.
var ErrNotFound = &APIError{http.StatusNotFound, "Recovery request not found"}

// ErrConflict rejects stale, conflicting, and expired transitions.
var ErrConflict = &APIError{http.StatusConflict, "Recovery request state or transcript conflict"}

// ErrUnauthorized rejects invalid role authentication.
var ErrUnauthorized = &APIError{http.StatusUnauthorized, "Invalid or expired recovery token"}

// Row retains the exact database snapshot for all checked writes. The lock order
// is users FOR UPDATE, then requests FOR UPDATE, then database wall-clock read.
type Row struct {
	Context
	Offer
	Status    string
	UserID    string
	Payload   []byte
	Created   time.Time
	Expiry    time.Time
	Offered   sql.NullTime
	Responded sql.NullTime
	Completed sql.NullTime
}

type scanner interface{ Scan(...any) error }

func scan(s scanner) (Row, error) {
	var r Row
	var binding, nonce, key, jti, rkey, rnonce, digest []byte
	err := s.Scan(&r.RequestID, &r.UserID, &r.ProtocolVersion, &r.ServerOrigin, &binding, &r.Expiry,
		&nonce, &key, &jti, &r.Status, &rkey, &rnonce, &digest, &r.Payload, &r.Created, &r.Offered, &r.Responded, &r.Completed)
	if err != nil {
		return r, err
	}
	r.AccountBinding, r.RequesterNonce, r.RequesterPublicKey, r.RecoveryTokenJTIHash = Encode(binding), Encode(nonce), Encode(key), Encode(jti)
	r.ResponderPublicKey, r.ResponderNonce, r.TranscriptHash = Encode(rkey), Encode(rnonce), Encode(digest)
	r.ExpiresAt = r.Expiry.UnixMilli()
	if !CanonicalUUID(r.UserID) || !r.Expiry.After(r.Created) || r.Expiry.After(r.Created.Add(15*time.Minute)) {
		return r, ErrInvalid
	}
	if !validStoredOffer(r) {
		return r, ErrInvalid
	}
	if (r.Status == "complete") != r.Completed.Valid {
		return r, ErrInvalid
	}
	if r.Status == "approved" {
		if _, e := Envelope(Encode(r.Payload)); e != nil {
			return r, e
		}
	} else if r.Payload != nil {
		return r, ErrInvalid
	}

	if err := r.Validate(); err != nil || !Equal(r.AccountBinding, AccountBinding(r.UserID)) {
		return r, ErrInvalid
	}
	if r.Offered.Valid {
		_, h, err := Transcript(r.Context, r.Offer)
		if err != nil || !Equal(Encode(h), r.TranscriptHash) {
			return r, ErrInvalid
		}
	}
	return r, nil
}

func validStoredOffer(r Row) bool {
	offered := r.ResponderPublicKey != "" && r.ResponderNonce != "" && r.TranscriptHash != "" && r.Offered.Valid
	absent := r.ResponderPublicKey == "" && r.ResponderNonce == "" && r.TranscriptHash == "" && !r.Offered.Valid
	if !offered && !absent {
		return false
	}
	switch r.Status {
	case "pending":
		return absent
	case "offered", "approved", "complete":
		return offered
	case "rejected", "expired":
		return true
	default:
		return false
	}
}

// Wire restricts disclosure to the state and caller role.
func (r Row) Wire() any {
	if r.Status == "rejected" || r.Status == "expired" || r.Status == "complete" {
		return struct {
			RequestID       string `json:"request_id"`
			ProtocolVersion int    `json:"protocol_version"`
			Status          string `json:"status"`
			ExpiresAt       int64  `json:"expires_at"`
		}{r.RequestID, Version, r.Status, r.ExpiresAt}
	}
	if r.Status == "pending" {
		return struct {
			Context
			Status string `json:"status"`
		}{r.Context, r.Status}
	}
	if r.Status == "offered" {
		return struct {
			Context
			Offer
			Status string `json:"status"`
		}{r.Context, r.Offer, r.Status}
	}
	return struct {
		Context
		Offer
		Status           string `json:"status"`
		EncryptedPayload string `json:"encrypted_payload"`
	}{r.Context, r.Offer, r.Status, Encode(r.Payload)}
}
func checkRollback(rollbackErr error, resultErr *error) {
	if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
		*resultErr = fmt.Errorf("rollback recovery: %w", rollbackErr)
	}
}
func lockUser(ctx context.Context, tx *sql.Tx, userID string) error {
	if !CanonicalUUID(userID) {
		return ErrUnauthorized
	}
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("lock recovery owner: %w", err)
	}
	return nil
}
func lockRequest(ctx context.Context, tx *sql.Tx, userID, requestID, jtiHash string) (Row, error) {
	if !CanonicalUUID(requestID) {
		return Row{}, ErrNotFound
	}
	r, err := scan(tx.QueryRowContext(ctx, `SELECT id, user_id, protocol_version, server_origin,
 account_binding, expires_at, requester_nonce, ephemeral_public_key, recovery_token_jti_hash,
 status, responder_public_key, responder_nonce, transcript_hash, encrypted_payload,
 created_at, offered_at, responded_at, completed_at
 FROM recovery_requests WHERE id=$1 AND user_id=$2 AND protocol_version=2 FOR UPDATE`, requestID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, fmt.Errorf("read recovery snapshot: %w", err)
	}
	if jtiHash != "" && !Equal(r.RecoveryTokenJTIHash, jtiHash) {
		return r, ErrNotFound
	}
	return r, nil
}
func dbClock(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now)
	if err != nil {
		return now, fmt.Errorf("read recovery clock: %w", err)
	}
	return now, nil
}
func binary(s string) []byte {
	if s == "" {
		return nil
	}
	b, err := Decode(s, lenBase64(s))
	if err != nil {
		return nil
	}
	return b
}
func lenBase64(s string) int {
	n := len(s) / 4 * 3
	if len(s) > 0 && s[len(s)-1] == '=' {
		n--
	}
	if len(s) > 1 && s[len(s)-2] == '=' {
		n--
	}
	return n
}

// cas matches every immutable field, including the exact offer and creation
// timestamps. The wall-clock predicate is evaluated again by PostgreSQL.
func cas(ctx context.Context, tx *sql.Tx, before, after Row, expired bool) error {
	result, err := tx.ExecContext(ctx, `UPDATE recovery_requests SET status=$1,
 responder_public_key=$2, responder_nonce=$3, transcript_hash=$4, offered_at=$5,
 encrypted_payload=$6, responded_at=$7, completed_at=$8
 WHERE id=$9 AND user_id=$10 AND protocol_version=$11 AND status=$12
 AND server_origin=$13 AND account_binding=$14 AND expires_at=$15
 AND requester_nonce=$16 AND ephemeral_public_key=$17 AND recovery_token_jti_hash=$18
 AND created_at=$19 AND responder_public_key IS NOT DISTINCT FROM $20
 AND responder_nonce IS NOT DISTINCT FROM $21 AND transcript_hash IS NOT DISTINCT FROM $22
 AND offered_at IS NOT DISTINCT FROM $23
 AND CASE WHEN $24::boolean THEN expires_at <= clock_timestamp()
 ELSE expires_at > clock_timestamp() END`,
		after.Status, binary(after.ResponderPublicKey), binary(after.ResponderNonce), binary(after.TranscriptHash), after.Offered,
		after.Payload, after.Responded, after.Completed, before.RequestID, before.UserID, before.ProtocolVersion, before.Status,
		before.ServerOrigin, binary(before.AccountBinding), before.Expiry, binary(before.RequesterNonce), binary(before.RequesterPublicKey), binary(before.RecoveryTokenJTIHash),
		before.Created, binary(before.ResponderPublicKey), binary(before.ResponderNonce), binary(before.TranscriptHash), before.Offered, expired)
	if err != nil {
		return fmt.Errorf("write recovery transition: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read recovery transition result: %w", err)
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}
func expire(ctx context.Context, tx *sql.Tx, r Row, now time.Time) (Row, error) {
	if now.Before(r.Expiry) || (r.Status != "pending" && r.Status != "offered" && r.Status != "approved") {
		return r, nil
	}
	after := r
	after.Status, after.Payload = "expired", nil
	if err := cas(ctx, tx, r, after, true); err != nil {
		return r, err
	}
	return after, nil
}

// Create fixes the requester context and database-bounded lifetime.
func Create(ctx context.Context, db *sql.DB, userID, jti string, tokenExpiry time.Time, body CreateBody) (row Row, err error) {
	if !CanonicalUUID(userID) || jti == "" || tokenExpiry.IsZero() {
		return row, ErrUnauthorized
	}
	if !Equal(body.AccountBinding, AccountBinding(userID)) {
		return row, invalidBody()
	}
	if body.ProtocolVersion != Version {
		return row, &APIError{http.StatusBadRequest, UpdateMessage}
	}
	if !Origin(body.ServerOrigin) {
		return row, invalidBody()
	}
	if _, e := Decode(body.RequesterNonce, 32); e != nil {
		return row, invalidBody()
	}
	if _, e := PublicKey(body.RequesterPublicKey); e != nil {
		return row, invalidBody()
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return row, fmt.Errorf("begin recovery: %w", err)
	}
	defer func() { checkRollback(tx.Rollback(), &err) }()
	if err = lockUser(ctx, tx, userID); err != nil {
		return row, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM trusted_recovery_devices WHERE user_id=$1`, userID).Scan(&count); err != nil {
		return row, fmt.Errorf("read trusted devices: %w", err)
	}
	if count == 0 {
		return row, &APIError{http.StatusBadRequest, "No trusted devices configured"}
	}
	now, err := dbClock(ctx, tx)
	if err != nil {
		return row, err
	}
	// Millisecond creation/expiry guarantees an exact wire timestamp across languages.
	now = now.Truncate(time.Millisecond)
	expiry := now.Add(15 * time.Minute)
	if tokenExpiry.Before(expiry) {
		expiry = tokenExpiry.Truncate(time.Millisecond)
	}
	if !expiry.After(now) {
		return row, ErrUnauthorized
	}
	row = Row{Context: Context{uuid.NewString(), Version, body.ServerOrigin, body.AccountBinding, expiry.UnixMilli(), body.RequesterNonce, body.RequesterPublicKey, Encode(Hash(jti))}, UserID: userID, Status: "pending", Created: now, Expiry: expiry}
	result, err := tx.ExecContext(ctx, `INSERT INTO recovery_requests (id,user_id,protocol_version,server_origin,account_binding,requester_nonce,ephemeral_public_key,recovery_token_jti_hash,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, row.RequestID, userID, Version, row.ServerOrigin, binary(row.AccountBinding), binary(row.RequesterNonce), binary(row.RequesterPublicKey), binary(row.RecoveryTokenJTIHash), now, expiry)
	if err != nil {
		return row, fmt.Errorf("create recovery: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return row, fmt.Errorf("read recovery creation result: %w", err)
	}
	if inserted != 1 {
		return row, ErrConflict
	}
	if err = tx.Commit(); err != nil {
		return row, fmt.Errorf("commit recovery: %w", err)
	}
	return row, nil
}

// Poll scopes reads to the verified requester and expires ciphertext after locks.
func Poll(ctx context.Context, db *sql.DB, userID, jti, requestID string) (row Row, err error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return row, fmt.Errorf("begin recovery read: %w", err)
	}
	defer func() { checkRollback(tx.Rollback(), &err) }()
	if err = lockUser(ctx, tx, userID); err != nil {
		return row, err
	}
	row, err = lockRequest(ctx, tx, userID, requestID, Encode(Hash(jti)))
	if err != nil {
		return row, err
	}
	now, err := dbClock(ctx, tx)
	if err != nil {
		return row, err
	}
	row, err = expire(ctx, tx, row, now)
	if err != nil {
		return row, err
	}
	if err = tx.Commit(); err != nil {
		return row, fmt.Errorf("commit recovery read: %w", err)
	}
	return row, nil
}

// List returns only owned, unexpired pending/offered contexts.
func List(ctx context.Context, db *sql.DB, userID string) (requests []any, err error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin recovery list: %w", err)
	}
	defer func() { checkRollback(tx.Rollback(), &err) }()
	if err = lockUser(ctx, tx, userID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, user_id, protocol_version, server_origin,
 account_binding, expires_at, requester_nonce, ephemeral_public_key, recovery_token_jti_hash,
 status, responder_public_key, responder_nonce, transcript_hash, encrypted_payload,
 created_at, offered_at, responded_at, completed_at
 FROM recovery_requests WHERE user_id=$1 AND protocol_version=2
 AND status IN ('pending','offered') ORDER BY created_at DESC FOR UPDATE`, userID)
	if err != nil {
		return nil, fmt.Errorf("list recovery: %w", err)
	}
	defer func() {
		if e := rows.Close(); e != nil {
			err = errors.Join(err, fmt.Errorf("close recovery list: %w", e))
		}
	}()
	snapshots := []Row{}
	for rows.Next() {
		r, e := scan(rows)
		if e != nil {
			return nil, fmt.Errorf("scan recovery list: %w", e)
		}
		snapshots = append(snapshots, r)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recovery list: %w", err)
	}
	if err = rows.Close(); err != nil {
		return nil, fmt.Errorf("close recovery list: %w", err)
	}
	now, err := dbClock(ctx, tx)
	if err != nil {
		return nil, err
	}
	requests = []any{}
	for _, r := range snapshots {
		r, err = expire(ctx, tx, r, now)
		if err != nil {
			return nil, err
		}
		if r.Status != "expired" {
			requests = append(requests, r.Wire())
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit recovery list: %w", err)
	}
	return requests, nil
}

// Respond fences the access epoch and applies a checked immutable-snapshot transition.
func Respond(ctx context.Context, db *sql.DB, userID, epoch, requestID string, body RespondBody) (result ResponseResult, err error) {
	if body.ProtocolVersion != Version {
		return result, &APIError{http.StatusBadRequest, UpdateMessage}
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return result, fmt.Errorf("begin recovery response: %w", err)
	}
	defer func() { checkRollback(tx.Rollback(), &err) }()
	if err = lockUser(ctx, tx, userID); err != nil {
		return result, err
	}
	if err = credepoch.GuardTx(ctx, tx, userID, epoch); err != nil {
		if errors.Is(err, credepoch.ErrEpochMismatch) {
			return result, ErrUnauthorized
		}
		return result, fmt.Errorf("guard recovery response: %w", err)
	}
	row, err := lockRequest(ctx, tx, userID, requestID, "")
	if err != nil {
		return result, err
	}
	now, err := dbClock(ctx, tx)
	if err != nil {
		return result, err
	}
	current, err := expire(ctx, tx, row, now)
	if err != nil {
		return result, err
	}
	if current.Status == "expired" {
		if err = tx.Commit(); err != nil {
			return result, fmt.Errorf("commit recovery expiry: %w", err)
		}
		return result, ErrConflict
	}
	after, err := respondTransition(row, body, now)
	if err != nil {
		return result, err
	}
	if err = cas(ctx, tx, row, after, false); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, fmt.Errorf("commit recovery response: %w", err)
	}
	return ResponseResult{requestID, Version, after.Status}, nil
}
func respondTransition(row Row, body RespondBody, now time.Time) (Row, error) {
	after := row
	switch body.Action {
	case "offer":
		if row.Status != "pending" {
			return row, ErrConflict
		}
		after.Offer = Offer{body.ResponderPublicKey, body.ResponderNonce, body.TranscriptHash}
		_, h, err := Transcript(row.Context, after.Offer)
		if err != nil || !Equal(Encode(h), body.TranscriptHash) {
			return row, ErrConflict
		}
		after.TranscriptHash = Encode(h)
		after.Status = "offered"
		after.Offered = sql.NullTime{Time: now, Valid: true}
	case "approve":
		if row.Status != "offered" || !Equal(row.TranscriptHash, body.TranscriptHash) {
			return row, ErrConflict
		}
		payload, err := Envelope(body.EncryptedPayload)
		if err != nil {
			return row, invalidBody()
		}
		after.Payload = payload
		after.Status = "approved"
		after.Responded = sql.NullTime{Time: now, Valid: true}
	case "reject":
		if row.Status != "pending" && row.Status != "offered" {
			return row, ErrConflict
		}
		after.Status = "rejected"
		after.Payload = nil
		after.Responded = sql.NullTime{Time: now, Valid: true}
	default:
		return row, invalidBody()
	}
	return after, nil
}

// Complete acknowledges the approved transcript without consuming the reset token.
func Complete(ctx context.Context, db *sql.DB, userID, jti, requestID string, body CompleteBody) (result ResponseResult, err error) {
	if body.ProtocolVersion != Version {
		return result, &APIError{http.StatusBadRequest, UpdateMessage}
	}
	if _, e := Decode(body.TranscriptHash, 32); e != nil {
		return result, invalidBody()
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return result, fmt.Errorf("begin recovery completion: %w", err)
	}
	defer func() { checkRollback(tx.Rollback(), &err) }()
	if err = lockUser(ctx, tx, userID); err != nil {
		return result, err
	}
	row, err := lockRequest(ctx, tx, userID, requestID, Encode(Hash(jti)))
	if err != nil {
		return result, err
	}
	now, err := dbClock(ctx, tx)
	if err != nil {
		return result, err
	}
	current, err := expire(ctx, tx, row, now)
	if err != nil {
		return result, err
	}
	if current.Status == "expired" {
		if err = tx.Commit(); err != nil {
			return result, fmt.Errorf("commit recovery expiry: %w", err)
		}
		return result, ErrConflict
	}
	if row.Status != "approved" || !Equal(row.TranscriptHash, body.TranscriptHash) {
		return result, ErrConflict
	}
	after := row
	after.Status = "complete"
	after.Payload = nil
	after.Completed = sql.NullTime{Time: now, Valid: true}
	if err = cas(ctx, tx, row, after, false); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, fmt.Errorf("commit recovery completion: %w", err)
	}
	return ResponseResult{requestID, Version, "complete"}, nil
}
