package store

import (
	"bytes"
	"context"
	"sort"

	"github.com/lib/pq"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

type keyBinding struct {
	Digest []byte
	Role   string
}

// bindingsOf lists every key a published op binds, in ascending digest order
// (D-192): the candidate root, the new dsk and dkx, the EK SPKI (role ek).
func bindingsOf(rq *request) []keyBinding {
	var out []keyBinding
	if len(rq.Intent.CandidateRoot) > 0 {
		out = append(out, keyBinding{ac.KeyDigest(rq.Intent.CandidateRoot), "root"})
	}
	if rq.DI != nil && len(rq.DI.NewDeviceDSK) > 0 {
		out = append(out, keyBinding{ac.KeyDigest(rq.DI.NewDeviceDSK), "dsk"}, keyBinding{ac.KeyDigest(rq.DI.NewDeviceDKX), "dkx"})
	}
	if rq.EKB != nil {
		out = append(out, keyBinding{ac.KeyDigest(rq.EKB.EKPublicKey), "ek"})
	}
	sortBindings(out)
	return out
}

func sortBindings(k []keyBinding) {
	sort.Slice(k, func(i, j int) bool { return bytes.Compare(k[i].Digest, k[j].Digest) < 0 })
}

// bindKeys is step 10: the pin check (E2-5), then one ordered multi-row INSERT
// under SAVEPOINT kb; any conflict rolls the savepoint back and records the
// lowest conflicting key's code.
func (s *Store) bindKeys(ctx context.Context, st *txState, keys []keyBinding, firstSeq uint64) (Outcome, bool) {
	for _, k := range keys {
		if k.Role == "ek" && len(st.h.Pin) > 0 && bytes.Equal(k.Digest, st.h.Pin) {
			return s.record(ctx, st, ac.RefusalEKReused), true
		}
	}
	if len(keys) == 0 {
		return Outcome{}, false
	}
	if _, err := st.tx.ExecContext(ctx, `SAVEPOINT kb`); err != nil {
		return s.storeError(), true
	}
	digests, roles := make([][]byte, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		digests[i], roles[i] = k.Digest, k.Role
	}
	rows, err := st.tx.QueryContext(ctx, `INSERT INTO authority.key_bindings (key_digest, account_id, role, first_seq)
		SELECT t.d, $2, t.r, $3 FROM unnest($1::bytea[], $4::text[]) WITH ORDINALITY AS t(d, r, o) ORDER BY t.o
		ON CONFLICT (key_digest) DO NOTHING RETURNING key_digest`,
		pq.Array(digests), st.h.AccountID, i64(firstSeq), pq.Array(roles))
	if err != nil {
		return s.storeError(), true
	}
	got := map[string]bool{}
	for rows.Next() {
		var d []byte
		if err := rows.Scan(&d); err != nil {
			_ = rows.Close()
			return s.storeError(), true
		}
		got[string(d)] = true
	}
	rerr := rows.Err()
	_ = rows.Close()
	if rerr != nil {
		return s.storeError(), true
	}
	if len(got) == len(keys) {
		return Outcome{}, false
	}
	if _, err := st.tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT kb`); err != nil {
		return s.storeError(), true
	}
	for _, k := range keys { // ascending: the first missing is the lowest conflicting key
		if !got[string(k.Digest)] {
			code, err := conflictCode(ctx, st, k)
			if err != nil {
				return s.storeError(), true
			}
			return s.record(ctx, st, code), true
		}
	}
	return s.storeError(), true
}

// conflictCode maps the new key's role to its refusal (§2.9 Key bindings).
func conflictCode(ctx context.Context, st *txState, k keyBinding) (ac.Refusal, error) {
	switch k.Role {
	case "dsk", "dkx":
		return ac.RefusalDeviceKeyBound, nil
	case "ek":
		return ac.RefusalEKReused, nil
	case "recovery":
		return ac.RefusalRecoveryKeyBound, nil
	}
	var owner string
	if err := st.tx.QueryRowContext(ctx, `SELECT account_id::text FROM authority.key_bindings WHERE key_digest = $1`, k.Digest).
		Scan(&owner); err != nil {
		return "", err
	}
	if owner == st.h.AccountID {
		return ac.RefusalRootReused, nil
	}
	return ac.RefusalRootBoundElsewhere, nil
}
