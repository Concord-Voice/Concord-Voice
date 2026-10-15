package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// lookupStored is the replay lookup (D-90, D-151, D-184): one key, (account_id,
// request_id), over all three tables (B1-D28). The device path reads
// device_events first; the published path reads it after an account_events
// miss, so an attached entry is reached through its published event. Equal
// intent_digest → the stored outcome (to be gated); different → 409.
func lookupStored(ctx context.Context, q queryer, account, requestID string, intentDigest []byte, device bool) (Outcome, bool, error) {
	if device {
		if o, hit, err := storedDevice(ctx, q, account, requestID, intentDigest); hit || err != nil {
			return o, hit, err
		}
	}
	// One statement, so the published event and its attached device row come
	// from one snapshot: a purge cannot land between them (review round 2).
	var dg, bundle, dev []byte
	err := q.QueryRowContext(ctx, `SELECT e.intent_digest, e.bundle, d.bundle FROM authority.account_events e
		LEFT JOIN authority.device_events d ON d.account_id = e.account_id AND d.request_id = e.request_id
		WHERE e.account_id = $1 AND e.request_id = $2`, account, requestID).Scan(&dg, &bundle, &dev)
	switch {
	case err == nil:
		if !bytes.Equal(dg, intentDigest) {
			return errorOutcome(409, ac.ErrorRequestConflict), true, nil
		}
		o, err := committedFromBundle(bundle, dev)
		return o, true, err
	case !errors.Is(err, sql.ErrNoRows):
		return Outcome{}, false, err
	}
	if !device { // a private event holds this request_id: its key is D or H(B), never an intent digest, so 409
		if o, hit, err := storedDevice(ctx, q, account, requestID, intentDigest); hit || err != nil {
			return o, hit, err
		}
	}
	var code string
	err = q.QueryRowContext(ctx, `SELECT intent_digest, code FROM authority.request_ledger
		WHERE account_id = $1 AND request_id = $2`, account, requestID).Scan(&dg, &code)
	switch {
	case err == nil:
		if !bytes.Equal(dg, intentDigest) {
			return errorOutcome(409, ac.ErrorRequestConflict), true, nil
		}
		return refusedOutcome(requestID, ac.Refusal(code)), true, nil
	case errors.Is(err, sql.ErrNoRows):
		return Outcome{}, false, nil
	}
	return Outcome{}, false, err
}

func refusedOutcome(requestID string, code ac.Refusal) Outcome {
	return Outcome{HTTP: 200, Kind: KindRefused, Code: string(code),
		Body: ac.Refused{RequestID: requestID, Code: string(code)}.Encode()}
}

// committedFromBundle rebuilds `committed` from the stored bundle and, when
// the op had a device part, the device_events bundle (nil when it had none).
// It runs no query: lookupStored reads both rows in one statement.
func committedFromBundle(bundle, dev []byte) (Outcome, error) {
	b, err := ac.DecodeBundle(bundle)
	if err != nil {
		return Outcome{}, err
	}
	c := ac.Committed{Result: b.Result, ResultSig: b.ResultSig}
	if dev != nil {
		dbun, err := ac.DecodeDeviceBundle(dev)
		if err != nil {
			return Outcome{}, err
		}
		c.DeviceResult, c.DeviceResultSig = dbun.DeviceResult, dbun.DeviceResultSig
	}
	return Outcome{HTTP: 200, Kind: KindCommitted, Body: c.Encode()}, nil
}

// storedDevice: a private op's stored outcome is `device-committed`. Its key
// is D, or H(B) for cancel_recovery_bind (§3.2.4 device path).
func storedDevice(ctx context.Context, q queryer, account, requestID string, intentDigest []byte) (Outcome, bool, error) {
	var dg, bundle []byte
	var op string
	err := q.QueryRowContext(ctx, `SELECT device_intent_digest, op, bundle FROM authority.device_events
		WHERE account_id = $1 AND request_id = $2`, account, requestID).Scan(&dg, &op, &bundle)
	if errors.Is(err, sql.ErrNoRows) {
		return Outcome{}, false, nil
	}
	if err != nil {
		return Outcome{}, false, err
	}
	if !bytes.Equal(dg, intentDigest) {
		return errorOutcome(409, ac.ErrorRequestConflict), true, nil
	}
	return Outcome{HTTP: 200, Kind: KindCommitted, Body: ac.DeviceCommitted{DeviceBundle: bundle}.Encode()}, true, nil
}
