package devicerecovery

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The shared fixture otherwise falls back to the developer's concord database.
// Accept explicitly configured scratch/CI databases without fixing host, port,
// user or test database name to this implementation session's environment.
func explicitDatabaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return false
	}
	database := strings.TrimPrefix(u.Path, "/")
	if database == "" || strings.EqualFold(database, "concord") {
		return false
	}
	// Refuse query overrides that could redirect the parsed path's database.
	query := u.Query()
	return !query.Has("dbname") && !query.Has("database")
}

func TestExplicitDatabaseURLGuard(t *testing.T) {
	for _, raw := range []string{
		"postgres://test@localhost:5432/concord_test?sslmode=disable",
		"postgres://recovery_test@127.0.0.1:55432/recovery_test?sslmode=disable",
		"postgresql://runner@db:6432/disposable_suite?sslmode=disable",
	} {
		require.True(t, explicitDatabaseURL(raw), raw)
	}
	for _, raw := range []string{
		"", "postgres://runner@localhost:5432/", "postgres://runner@localhost:5432/concord",
		"postgres://runner@db:5432/Concord", "postgres://runner@db:5432/%63oncord",
		"postgres://runner@db:5432/concord_test?dbname=concord", "postgres://runner@db:5432/concord_test?database=concord",
		"https://db/concord_test", "postgres:/concord_test",
	} {
		require.False(t, explicitDatabaseURL(raw), raw)
	}
}

func setupDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	require.True(t, explicitDatabaseURL(os.Getenv("DATABASE_URL")), "DATABASE_URL must explicitly select an isolated database; the default developer concord database is refused")
	db, _ := dbtest.SetupTestDB(t)
	id := dbtest.CreateUser(t, db).String()
	_, err := db.Exec(`INSERT INTO trusted_recovery_devices(user_id,device_name,machine_id) VALUES($1,'synthetic device',$2)`, id, uuid.NewString())
	require.NoError(t, err)
	return db, id
}
func newRequest(t *testing.T, db *sql.DB, id string) Row {
	t.Helper()
	b := createBody(t)
	b.AccountBinding = AccountBinding(id)
	r, err := Create(context.Background(), db, id, "public-jti", time.Now().Add(time.Hour), b)
	require.NoError(t, err)
	return r
}
func offerBody(t *testing.T, r Row) RespondBody {
	t.Helper()
	v := fixture(t)
	o := v.Offer
	_, h, err := Transcript(r.Context, o)
	require.NoError(t, err)
	return RespondBody{Action: "offer", ProtocolVersion: Version, ResponderPublicKey: o.ResponderPublicKey, ResponderNonce: o.ResponderNonce, TranscriptHash: Encode(h)}
}
func approveBody(t *testing.T, o RespondBody) RespondBody {
	t.Helper()
	return RespondBody{Action: "approve", ProtocolVersion: Version, TranscriptHash: o.TranscriptHash, EncryptedPayload: fixture(t).Envelope}
}
func TestDatabaseHappyPathOwnershipEpochAndCompletion(t *testing.T) {
	db, id := setupDB(t)
	ctx := context.Background()
	r := newRequest(t, db, id)
	o := offerBody(t, r)
	other := dbtest.CreateUser(t, db).String()
	_, err := Poll(ctx, db, other, "public-jti", r.RequestID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = Poll(ctx, db, id, "other-jti", r.RequestID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = Poll(ctx, db, id, "public-jti", "NOT-A-UUID")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = Respond(ctx, db, other, "", r.RequestID, o)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = Respond(ctx, db, id, "", r.RequestID, approveBody(t, o))
	require.ErrorIs(t, err, ErrConflict)
	bad := o
	bad.TranscriptHash = Encode(Hash("wrong"))
	_, err = Respond(ctx, db, id, "", r.RequestID, bad)
	require.ErrorIs(t, err, ErrConflict)
	_, err = Respond(ctx, db, id, "", r.RequestID, o)
	require.NoError(t, err)
	_, err = Respond(ctx, db, id, "", r.RequestID, o)
	require.ErrorIs(t, err, ErrConflict)
	polled, err := Poll(ctx, db, id, "public-jti", r.RequestID)
	require.NoError(t, err)
	require.Equal(t, "offered", polled.Status)
	require.Equal(t, o.TranscriptHash, polled.TranscriptHash)
	listed, err := List(ctx, db, id)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	_, err = Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash})
	require.ErrorIs(t, err, ErrConflict)
	a := approveBody(t, o)
	bad = a
	bad.TranscriptHash = Encode(Hash("wrong"))
	_, err = Respond(ctx, db, id, "", r.RequestID, bad)
	require.ErrorIs(t, err, ErrConflict)
	_, err = db.Exec(`UPDATE users SET credential_epoch='new-epoch' WHERE id=$1`, id)
	require.NoError(t, err)
	_, err = Respond(ctx, db, id, "", r.RequestID, a)
	require.ErrorIs(t, err, ErrUnauthorized)
	_, err = Respond(ctx, db, id, "new-epoch", r.RequestID, a)
	require.NoError(t, err)
	listed, err = List(ctx, db, id)
	require.NoError(t, err)
	require.Empty(t, listed)
	_, err = Complete(ctx, db, id, "other-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, Encode(Hash("wrong"))})
	require.ErrorIs(t, err, ErrConflict)
	// The requester has no credential epoch, even after the account's epoch rotates.
	_, err = Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash})
	require.NoError(t, err)
	_, err = Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash})
	require.ErrorIs(t, err, ErrConflict)
	polled, err = Poll(ctx, db, id, "public-jti", r.RequestID)
	require.NoError(t, err)
	require.Nil(t, polled.Payload)
	require.Equal(t, "complete", polled.Status)
	require.NotContains(t, marshal(t, polled.Wire()), "requester_public_key")
	require.NotContains(t, marshal(t, polled.Wire()), "encrypted_payload")
}
func runRace(t *testing.T, actions ...func() error) {
	t.Helper()
	start := make(chan struct{})
	out := make(chan error, len(actions))
	var wg sync.WaitGroup
	for _, act := range actions {
		wg.Go(func() { <-start; out <- act() })
	}
	close(start)
	wg.Wait()
	close(out)
	wins := 0
	for err := range out {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, ErrConflict)
		}
	}
	require.Equal(t, 1, wins)
}
func TestDatabaseConflictingWritersAndExactCAS(t *testing.T) {
	db, id := setupDB(t)
	ctx := context.Background()
	r := newRequest(t, db, id)
	o := offerBody(t, r)
	runRace(t, func() error { _, e := Respond(ctx, db, id, "", r.RequestID, o); return e }, func() error { _, e := Respond(ctx, db, id, "", r.RequestID, o); return e })
	a := approveBody(t, o)
	reject := RespondBody{Action: "reject", ProtocolVersion: Version}
	runRace(t, func() error { _, e := Respond(ctx, db, id, "", r.RequestID, a); return e }, func() error { _, e := Respond(ctx, db, id, "", r.RequestID, reject); return e })
	r = newRequest(t, db, id)
	o = offerBody(t, r)
	_, err := Respond(ctx, db, id, "", r.RequestID, o)
	require.NoError(t, err)
	_, err = Respond(ctx, db, id, "", r.RequestID, approveBody(t, o))
	require.NoError(t, err)
	runRace(t, func() error {
		_, e := Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash})
		return e
	}, func() error {
		_, e := Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash})
		return e
	})
	// An altered immutable snapshot can never produce an acknowledged write.
	r = newRequest(t, db, id)
	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, lockUser(ctx, tx, id))
	before, err := lockRequest(ctx, tx, id, r.RequestID, "")
	require.NoError(t, err)
	after := before
	after.Status = "rejected"
	before.ServerOrigin = "https://other.example.test"
	require.ErrorIs(t, cas(ctx, tx, before, after, false), ErrConflict)
	require.NoError(t, tx.Rollback())
}
func TestDatabaseLockWaitExpiresApprovedAndWithholdsCiphertext(t *testing.T) {
	db, id := setupDB(t)
	ctx := context.Background()
	r := newRequest(t, db, id)
	// Preserve valid creation/expiry constraints while arranging a short deadline.
	_, err := db.Exec(`UPDATE recovery_requests SET created_at=clock_timestamp()-interval '1 minute', expires_at=clock_timestamp()+interval '700 milliseconds' WHERE id=$1`, r.RequestID)
	require.NoError(t, err)
	r, err = Poll(ctx, db, id, "public-jti", r.RequestID)
	require.NoError(t, err)
	o := offerBody(t, r)
	_, err = Respond(ctx, db, id, "", r.RequestID, o)
	require.NoError(t, err)
	_, err = Respond(ctx, db, id, "", r.RequestID, approveBody(t, o))
	require.NoError(t, err)
	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, lockUser(ctx, tx, id))
	result := make(chan Row, 1)
	failure := make(chan error, 1)
	go func() { p, e := Poll(ctx, db, id, "public-jti", r.RequestID); result <- p; failure <- e }()
	select {
	case <-result:
		t.Fatal("poll passed a held owner lock")
	case <-time.After(800 * time.Millisecond):
	}
	require.NoError(t, tx.Commit())
	p := <-result
	require.NoError(t, <-failure)
	require.Equal(t, "expired", p.Status)
	require.Nil(t, p.Payload)
	require.NotContains(t, marshal(t, p.Wire()), "encrypted_payload")
	_, err = Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash})
	require.ErrorIs(t, err, ErrConflict)
	_, err = Respond(ctx, db, id, "", r.RequestID, approveBody(t, o))
	require.ErrorIs(t, err, ErrConflict)
}
func TestDatabaseExpiryListCreateAndDependencyErrors(t *testing.T) {
	db, id := setupDB(t)
	ctx := context.Background()
	r := newRequest(t, db, id)
	_, err := db.Exec(`UPDATE recovery_requests SET created_at=clock_timestamp()-interval '2 minutes',expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, r.RequestID)
	require.NoError(t, err)
	listed, err := List(ctx, db, id)
	require.NoError(t, err)
	require.Empty(t, listed)
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM recovery_requests WHERE id=$1`, r.RequestID).Scan(&status))
	require.Equal(t, "expired", status)
	b := createBody(t)
	_, err = Create(ctx, db, id, "public-jti", time.Now().Add(time.Hour), b)
	require.Error(t, err)
	b.AccountBinding = AccountBinding(id)
	_, err = Create(ctx, db, id, "public-jti", time.Time{}, b)
	require.ErrorIs(t, err, ErrUnauthorized)
	_, err = Create(ctx, db, id, "public-jti", time.Now().Add(-time.Hour), b)
	require.ErrorIs(t, err, ErrUnauthorized)
	short := time.Now().Add(time.Minute).Truncate(time.Second)
	r, err = Create(ctx, db, id, "public-jti", short, b)
	require.NoError(t, err)
	require.Equal(t, short.UnixMilli(), r.ExpiresAt)
	_, err = db.Exec(`DELETE FROM trusted_recovery_devices WHERE user_id=$1`, id)
	require.NoError(t, err)
	_, err = Create(ctx, db, id, "public-jti", time.Now().Add(time.Hour), b)
	var api *APIError
	require.ErrorAs(t, err, &api)
	require.Equal(t, 400, api.Status)
	closed, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	_, err = Poll(ctx, closed, id, "jti", r.RequestID)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrNotFound))
}

func TestDatabaseRequestLockExpiryPreventsOfferAndComplete(t *testing.T) {
	for _, action := range []string{"offer", "complete"} {
		t.Run(action, func(t *testing.T) {
			db, id := setupDB(t)
			ctx := context.Background()
			r := newRequest(t, db, id)
			_, err := db.Exec(`UPDATE recovery_requests SET created_at=clock_timestamp()-interval '1 minute',expires_at=clock_timestamp()+interval '600 milliseconds' WHERE id=$1`, r.RequestID)
			require.NoError(t, err)
			r, err = Poll(ctx, db, id, "public-jti", r.RequestID)
			require.NoError(t, err)
			o := offerBody(t, r)
			if action == "complete" {
				_, err = Respond(ctx, db, id, "", r.RequestID, o)
				require.NoError(t, err)
				_, err = Respond(ctx, db, id, "", r.RequestID, approveBody(t, o))
				require.NoError(t, err)
			}
			tx, err := db.Begin()
			require.NoError(t, err)
			var requestID string
			require.NoError(t, tx.QueryRow(`SELECT id FROM recovery_requests WHERE id=$1 FOR UPDATE`, r.RequestID).Scan(&requestID))
			done := make(chan error, 1)
			go func() {
				if action == "offer" {
					_, e := Respond(ctx, db, id, "", r.RequestID, o)
					done <- e
				} else {
					_, e := Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{Version, o.TranscriptHash})
					done <- e
				}
			}()
			select {
			case <-done:
				t.Fatal("write passed held request lock")
			case <-time.After(700 * time.Millisecond):
			}
			require.NoError(t, tx.Commit())
			require.ErrorIs(t, <-done, ErrConflict)
			p, err := Poll(ctx, db, id, "public-jti", r.RequestID)
			require.NoError(t, err)
			require.Equal(t, "expired", p.Status)
			require.Nil(t, p.Payload)
		})
	}
}

func TestDatabaseRejectedWireAndUnsupportedVersions(t *testing.T) {
	db, id := setupDB(t)
	ctx := context.Background()
	r := newRequest(t, db, id)
	_, err := Respond(ctx, db, id, "", r.RequestID, RespondBody{Action: "reject", ProtocolVersion: 2})
	require.NoError(t, err)
	p, err := Poll(ctx, db, id, "public-jti", r.RequestID)
	require.NoError(t, err)
	require.Equal(t, "rejected", p.Status)
	require.NotContains(t, marshal(t, p.Wire()), "transcript_hash")
	_, err = Respond(ctx, db, id, "", r.RequestID, RespondBody{Action: "reject", ProtocolVersion: 2})
	require.ErrorIs(t, err, ErrConflict)
	_, err = Respond(ctx, db, id, "", r.RequestID, RespondBody{Action: "reject", ProtocolVersion: 1})
	require.Error(t, err)
	_, err = Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{1, fixture(t).Hash})
	require.Error(t, err)
	_, err = Complete(ctx, db, id, "public-jti", r.RequestID, CompleteBody{2, "AA=="})
	require.Error(t, err)
	b := createBody(t)
	b.AccountBinding = AccountBinding(id)
	b.ProtocolVersion = 1
	_, err = Create(ctx, db, id, "jti", time.Now().Add(time.Hour), b)
	require.Error(t, err)
	b.ProtocolVersion = 2
	b.ServerOrigin = "https://example.test/"
	_, err = Create(ctx, db, id, "jti", time.Now().Add(time.Hour), b)
	require.Error(t, err)
	b.ServerOrigin = fixture(t).Context.ServerOrigin
	b.RequesterNonce = "AA=="
	_, err = Create(ctx, db, id, "jti", time.Now().Add(time.Hour), b)
	require.Error(t, err)
	b.RequesterNonce = fixture(t).Context.RequesterNonce
	b.RequesterPublicKey = "AA=="
	_, err = Create(ctx, db, id, "jti", time.Now().Add(time.Hour), b)
	require.Error(t, err)
}
