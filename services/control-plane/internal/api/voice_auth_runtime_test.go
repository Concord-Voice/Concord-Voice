package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/google/uuid"
	rawnats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// Only the transport-close boundary is replaced. Family ownership and all
// registration/release/completion evidence go through the production SQL rail.
type runtimeFamilyCloser struct {
	err   error
	calls atomic.Int32
}

func (h *runtimeFamilyCloser) DisconnectFamilyContext(ctx context.Context, _, _ uuid.UUID) error {
	h.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return err
	}
	return h.err
}

func runtimeRevoke(t *testing.T, r *voiceAuthRig) dmblock.AuthRevocation {
	t.Helper()
	tx, err := r.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { err := tx.Rollback(); require.True(t, err == nil || errors.Is(err, sql.ErrTxDone)) }()
	barrier, err := loginfamily.AcquireWriterBarrierTx(t.Context(), tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(t.Context(), tx, barrier, []uuid.UUID{r.userID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(r.userID)
	require.NoError(t, err)
	targets, err := loginfamily.CaptureUserTargetsTx(t.Context(), tx, user, nil)
	require.NoError(t, err)
	result, err := loginfamily.RevokeFamiliesTx(t.Context(), tx, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeUser, Reason: loginfamily.ReasonExplicitRevoke})
	require.NoError(t, err)
	require.Len(t, result.Owners, 1)
	require.NoError(t, tx.Commit())
	owner := dmblock.AuthRevocation{OperationID: result.Owners[0].AuthOperationID, UserID: r.userID, Scope: "user", Reason: "explicit_revoke"}
	require.NoError(t, r.db.QueryRow(`SELECT created_at,attempts FROM login_family_revocations WHERE operation_id=$1`, owner.OperationID).Scan(&owner.CreatedAt, &owner.Attempts))
	return owner
}

func runtimeNATS(t *testing.T) (*natsclient.Client, *rawnats.Conn) {
	t.Helper()
	// CI runs a dedicated NATS service on the default port without exporting
	// NATS_URL; fall back to it like the sibling runtime tests. A missing broker
	// still fails here rather than skipping, so coverage cannot silently vanish.
	address := os.Getenv("NATS_URL")
	if address == "" {
		address = rawnats.DefaultURL
	}
	client, err := natsclient.Connect(address)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	observer, err := rawnats.Connect(address, rawnats.NoReconnect())
	require.NoError(t, err)
	t.Cleanup(observer.Close)
	return client, observer
}

func runtimeRelease(t *testing.T, r *voiceAuthRig, binding protocol.Binding, room *protocol.RoomRegister) {
	t.Helper()
	if room != nil {
		w, request := r.request(t, protocol.EndpointRoomRelease, roomBindingRelease(binding, *room), "")
		require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
		r.verify(t, protocol.EndpointRoomRelease, w, request, protocol.ExpectedResponse{Binding: &binding})
	}
	w, request := r.request(t, protocol.EndpointConnectionRelease, closureRelease(binding), "")
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	r.verify(t, protocol.EndpointConnectionRelease, w, request, protocol.ExpectedResponse{Binding: &binding})
}

func runtimeFreshFamily(t *testing.T, r *voiceAuthRig) protocol.Binding {
	t.Helper()
	sid := uuid.New()
	prepared, err := auth.PrepareAccessToken(auth.AccessTokenInput{UserID: r.userID, SID: sid, CredentialEpoch: r.epoch, EmailVerified: true}, time.Now())
	require.NoError(t, err)
	tx, err := r.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { err := tx.Rollback(); require.True(t, err == nil || errors.Is(err, sql.ErrTxDone)) }()
	barrier, err := loginfamily.AcquireWriterBarrierTx(t.Context(), tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(t.Context(), tx, barrier, []uuid.UUID{r.userID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(r.userID)
	require.NoError(t, err)
	family, err := loginfamily.CreateFamilyTx(t.Context(), tx, user, r.epoch)
	require.NoError(t, err)
	require.NoError(t, loginfamily.BindSIDTx(t.Context(), tx, family, sid, prepared.ExpiresAt()))
	require.NoError(t, tx.Commit())
	r.familyID = family.Binding().FamilyID
	r.token, err = prepared.Sign(r.secret)
	require.NoError(t, err)
	r.target.ConnectionGeneration = uuid.NewString()
	r.target.SocketID = "fresh-after-terminal"
	return r.register(t)
}

// Removing json.RawMessage at the publisher must fail at the actual NATS
// boundary; returning success on ACK alone must fail the first delivery.
func TestVoiceAuthRuntimeSignedWireRequiresFreshEmptyEvidence(t *testing.T) {
	for _, withRoom := range []bool{false, true} {
		name := "idle"
		if withRoom {
			name = "room"
		}
		t.Run(name, func(t *testing.T) {
			r := newVoiceAuthRig(t)
			binding := r.register(t)
			var room *protocol.RoomRegister
			if withRoom {
				p := roomBindingFixture(t, r, "channel")
				roomBindingAttach(t, r, binding, p)
				room = &p
			}
			owner := runtimeRevoke(t, r)
			client, observer := runtimeNATS(t)
			sub, err := observer.SubscribeSync("voice.enforce.session." + binding.Target.NodeBootID)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sub.Unsubscribe()) })
			require.NoError(t, observer.Flush())
			hub := &runtimeFamilyCloser{}
			runtime := newVoiceAuthRuntime(r.db, client, r.secret, hub, dmblock.New(r.db, nil))
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- runtime.deliverAuth(ctx, owner) }()
			message, err := sub.NextMsg(2 * time.Second)
			require.NoError(t, err)
			command, err := protocol.DecodeCommand(message.Data)
			require.NoError(t, err, "actual NATS JSON must be an object rather than base64 bytes")
			require.NoError(t, protocol.VerifyCommand(command, []byte(r.secret), time.Now()))
			payload, ok := command.Auth()
			require.True(t, ok)
			require.Equal(t, owner.OperationID.String(), payload.OperationID)
			require.Equal(t, binding, protocol.Binding{UserID: payload.UserID, FamilyID: payload.FamilyID, Target: payload.Target})
			ack, err := protocol.SignAuthACK(command, []byte(r.secret))
			require.NoError(t, err)
			require.NoError(t, message.Respond(ack))
			require.Error(t, <-result, "a valid ACK cannot erase retained SQL evidence")
			live, _ := closureCount(t, r.db, binding.Target.ConnectionGeneration)
			require.Equal(t, 1, live)
			runtimeRelease(t, r, binding, room)
			fresh := runtimeFreshFamily(t, r)
			require.NoError(t, runtime.deliverAuth(ctx, owner), "fresh empty evidence permits completion after a lost prior ACK")
			require.Positive(t, hub.calls.Load())
			freshLive, _ := closureCount(t, r.db, fresh.Target.ConnectionGeneration)
			require.Equal(t, 1, freshLive, "fresh family outside the sealed frontier must survive replay")
		})
	}
}

func TestVoiceAuthRuntimeNoSubscriptionRetainsOriginalOwner(t *testing.T) {
	r := newVoiceAuthRig(t)
	binding := r.register(t)
	owner := runtimeRevoke(t, r)
	client, _ := runtimeNATS(t)
	runtime := newVoiceAuthRuntime(r.db, client, r.secret, &runtimeFamilyCloser{}, dmblock.New(r.db, nil))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.Error(t, runtime.deliverAuth(ctx, owner))
	var created time.Time
	var operation uuid.UUID
	require.NoError(t, r.db.QueryRow(`SELECT r.created_at,f.auth_operation_id FROM login_family_revocations r JOIN login_families f ON f.auth_operation_id=r.operation_id WHERE f.family_id=$1`, r.familyID).Scan(&created, &operation))
	require.True(t, owner.CreatedAt.Equal(created))
	require.Equal(t, owner.OperationID, operation)
	live, _ := closureCount(t, r.db, binding.Target.ConnectionGeneration)
	require.Equal(t, 1, live)
}

func TestVoiceAuthRuntimeRequiresLocalCloseReceiptForEmptyFamily(t *testing.T) {
	r := newVoiceAuthRig(t)
	owner := runtimeRevoke(t, r)
	closeErr := errors.New("transport close not confirmed")
	runtime := newVoiceAuthRuntime(r.db, nil, r.secret, &runtimeFamilyCloser{err: closeErr}, dmblock.New(r.db, nil))
	require.Error(t, runtime.deliverAuth(t.Context(), owner))
}

func TestVoiceAuthRuntimeStartupAndSQLFailureDenyGrants(t *testing.T) {
	r := newVoiceAuthRig(t)
	reconciler := dmblock.New(r.db, nil)
	runtime := newVoiceAuthRuntime(r.db, nil, r.secret, &runtimeFamilyCloser{}, reconciler)
	snapshot, err := runtime.AuthoritySnapshot(t.Context(), uuid.MustParse(r.target.NodeBootID))
	require.NoError(t, err)
	require.True(t, snapshot.SQLAvailable)
	require.False(t, snapshot.ProductiveGrantEligible, "restart cannot infer a fresh process round from SQL timestamps")
	require.True(t, snapshot.SecurityRoundCompletedAt.IsZero())
	_, err = reconciler.ReconcileDue(t.Context(), 100)
	require.NoError(t, err)
	snapshot, err = runtime.AuthoritySnapshot(t.Context(), uuid.MustParse(r.target.NodeBootID))
	require.NoError(t, err)
	require.True(t, snapshot.SecurityRoundFresh)
	require.True(t, snapshot.ProductiveGrantEligible, "a completed current-process round permits an active ready boot")
	missing, err := runtime.AuthoritySnapshot(t.Context(), uuid.New())
	require.NoError(t, err)
	require.False(t, missing.ProductiveGrantEligible, "absence is not an active boot receipt")
	retiredBoot := uuid.New()
	_, err = r.db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id,retired_at) VALUES($1,clock_timestamp())`, retiredBoot)
	require.NoError(t, err)
	retired, err := runtime.AuthoritySnapshot(t.Context(), retiredBoot)
	require.NoError(t, err)
	require.False(t, retired.ProductiveGrantEligible)
	restarted := newVoiceAuthRuntime(r.db, nil, r.secret, &runtimeFamilyCloser{}, dmblock.New(r.db, nil))
	restartSnapshot, err := restarted.AuthoritySnapshot(t.Context(), uuid.MustParse(r.target.NodeBootID))
	require.NoError(t, err)
	require.False(t, restartSnapshot.ProductiveGrantEligible)
	// An independently opened closed pool injects SQL unavailability without
	// closing the shared fixture pool or changing durable activation.
	failedDB, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	require.NoError(t, failedDB.Close())
	broken := newVoiceAuthRuntime(failedDB, nil, r.secret, &runtimeFamilyCloser{}, dmblock.New(failedDB, nil))
	snapshot, err = broken.AuthoritySnapshot(t.Context(), uuid.MustParse(r.target.NodeBootID))
	require.Error(t, err)
	require.False(t, snapshot.SQLAvailable)
	require.False(t, snapshot.ProductiveGrantEligible)
}

// A malformed acknowledgment must never become durable completion. The
// responder uses the real JSON envelope, including a duplicate security key.
func TestVoiceAuthRuntimeStrictACKRejectsDuplicateKey(t *testing.T) {
	r := newVoiceAuthRig(t)
	binding := r.register(t)
	client, observer := runtimeNATS(t)
	sub, err := observer.SubscribeSync("voice.enforce.session." + binding.Target.NodeBootID)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sub.Unsubscribe()) })
	require.NoError(t, observer.Flush())
	commandToSend, err := protocol.NewAuthCommand(binding, uuid.NewString(), "user", "explicit_revoke", protocol.AttemptMeta{Timestamp: strconv.FormatInt(time.Now().Unix(), 10), Nonce: strings.Repeat("b", 64)})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- publishVoiceAuthConnectionEjection(ctx, client, r.secret, commandToSend) }()
	message, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	command, err := protocol.DecodeCommand(message.Data)
	require.NoError(t, err)
	ack, err := protocol.SignAuthACK(command, []byte(r.secret))
	require.NoError(t, err)
	malformed := strings.Replace(string(ack), `"ok":true`, `"ok":true,"ok":true`, 1)
	require.NotEqual(t, string(ack), malformed)
	require.NoError(t, message.Respond([]byte(malformed)))
	require.Error(t, <-result)
}

type runtimeSaturatedRequester struct {
	entered chan struct{}
	active  atomic.Int32
	peak    atomic.Int32
}

func (r *runtimeSaturatedRequester) RequestWithContext(ctx context.Context, _ string, _ interface{}) ([]byte, error) {
	active := r.active.Add(1)
	defer r.active.Add(-1)
	for previous := r.peak.Load(); active > previous; previous = r.peak.Load() {
		if r.peak.CompareAndSwap(previous, active) {
			break
		}
	}
	r.entered <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}

// A per-call worker pool admits all 96 requests and fails this check. The
// boundary blocks actual request lifetimes, so a slot freed before ACK also
// exceeds the limit. All three production publisher variants compete together.
func TestVoiceAuthRuntimeProcessSharedLimitCoversAuthEpochAndDM(t *testing.T) {
	requester := &runtimeSaturatedRequester{entered: make(chan struct{}, 96)}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	binding := protocol.Binding{UserID: uuid.NewString(), FamilyID: uuid.NewString(), Target: protocol.Target{ConnectionGeneration: uuid.NewString(), NodeBootID: uuid.NewString(), SocketID: "shared-limit", CredentialEpoch: strings.Repeat("a", 32)}}
	meta := protocol.AttemptMeta{Timestamp: strconv.FormatInt(time.Now().Unix(), 10), Nonce: strings.Repeat("a", 64)}
	authCommand, err := protocol.NewAuthCommand(binding, uuid.NewString(), "user", "explicit_revoke", meta)
	require.NoError(t, err)
	epochCommand, err := protocol.NewEpochCommand(binding, uuid.NewString(), binding.Target.CredentialEpoch, meta)
	require.NoError(t, err)
	var callers sync.WaitGroup
	for index := range 96 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			switch index % 5 {
			case 0:
				err := publishVoiceAuthConnectionEjection(ctx, requester, voiceEnforcementProtocolTestSecret, authCommand)
				if err == nil {
					t.Error("blocked auth request unexpectedly succeeded")
				}
			case 1:
				err := publishVoiceAuthConnectionEjection(ctx, requester, voiceEnforcementProtocolTestSecret, epochCommand)
				if err == nil {
					t.Error("blocked epoch request unexpectedly succeeded")
				}
			case 2:
				err := publishDMBlockVoiceEjection(ctx, requester, voiceEnforcementProtocolTestSecret, uuid.NewString(), uuid.New())
				if err == nil {
					t.Error("blocked legacy DM request unexpectedly succeeded")
				}
			case 3:
				err := publishCredentialEpochVoiceEjection(ctx, requester, voiceEnforcementProtocolTestSecret, uuid.New(), strings.Repeat("a", 32), strings.Repeat("b", 32))
				if err == nil {
					t.Error("blocked legacy epoch request unexpectedly succeeded")
				}
			default:
				row := voiceEnforcementSessionRow{sessionGeneration: uuid.New(), nodeBootID: uuid.New(), roomID: uuid.New(), roomKind: "dm", userID: uuid.New(), socketID: "shared-dm"}
				err := publishVoiceEnforcementSessionEjection(ctx, requester, voiceEnforcementProtocolTestSecret, uuid.New(), row)
				if err == nil {
					t.Error("blocked DM request unexpectedly succeeded")
				}
			}
		}()
	}
	for range 64 {
		select {
		case <-requester.entered:
		case <-ctx.Done():
			t.Fatal("shared pool never reached its promised capacity")
		}
	}
	select {
	case <-requester.entered:
		t.Error("a sixty-fifth request crossed the process-wide boundary before ACK/cancellation")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	callers.Wait()
	require.EqualValues(t, 64, requester.peak.Load())
	require.Zero(t, requester.active.Load(), "cancellation must release occupied slots")
}

func TestVoiceAuthRuntimeLostACKAfterSignedReleaseCompletesFreshScan(t *testing.T) {
	r := newVoiceAuthRig(t)
	binding := r.register(t)
	owner := runtimeRevoke(t, r)
	client, observer := runtimeNATS(t)
	sub, err := observer.SubscribeSync("voice.enforce.session." + binding.Target.NodeBootID)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sub.Unsubscribe()) })
	require.NoError(t, observer.Flush())
	runtime := newVoiceAuthRuntime(r.db, client, r.secret, &runtimeFamilyCloser{}, dmblock.New(r.db, nil))
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- runtime.deliverAuth(ctx, owner) }()
	message, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	command, err := protocol.DecodeCommand(message.Data)
	require.NoError(t, err)
	require.NoError(t, protocol.VerifyCommand(command, []byte(r.secret), time.Now()))
	runtimeRelease(t, r, binding, nil)
	// Deliberately never respond. The request times out on its own bound; the
	// caller remains live and must finish from retained local/SQL witnesses.
	require.NoError(t, <-result)
	require.NoError(t, ctx.Err())
}

func TestVoiceAuthRuntimeOriginalAgeQuarantinesDespiteFreshRetryRound(t *testing.T) {
	r := newVoiceAuthRig(t)
	r.register(t)
	owner := runtimeRevoke(t, r)
	cleanBoot := uuid.New()
	_, err := r.db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES($1)`, cleanBoot)
	require.NoError(t, err)
	reconciler := dmblock.New(r.db, nil)
	runtime := newVoiceAuthRuntime(r.db, nil, r.secret, &runtimeFamilyCloser{}, reconciler)
	reconciler.SetAuthRevocationEject(runtime.deliverAuth)
	_, err = reconciler.ReconcileDue(t.Context(), 100)
	require.NoError(t, err)
	before, err := runtime.AuthoritySnapshot(t.Context(), uuid.MustParse(r.target.NodeBootID))
	require.NoError(t, err)
	require.True(t, before.ProductiveGrantEligible, "young failed delivery may retain productive permission after recorded round progress")
	require.True(t, owner.CreatedAt.Equal(before.OldestUnsettledAt))
	// Use real elapsed time: no immutable creation metadata or private authority
	// is overwritten to fabricate age. The final round below is newly completed.
	wait := time.Until(owner.CreatedAt.Add(30*time.Second + 100*time.Millisecond))
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	stale, err := runtime.AuthoritySnapshot(t.Context(), cleanBoot)
	require.NoError(t, err)
	require.False(t, stale.SecurityRoundFresh)
	require.False(t, stale.ProductiveGrantEligible, "round staleness alone withholds an otherwise clean active boot")
	require.True(t, stale.OldestUnsettledAt.IsZero())
	_, err = reconciler.ReconcileDue(t.Context(), 100)
	require.NoError(t, err)
	after, err := runtime.AuthoritySnapshot(t.Context(), uuid.MustParse(r.target.NodeBootID))
	require.NoError(t, err)
	require.True(t, after.SecurityRoundFresh)
	require.True(t, after.SecurityRoundCompletedAt.After(before.SecurityRoundCompletedAt))
	require.False(t, after.ProductiveGrantEligible, "a fresh retry cannot reset original unsettled age")
	require.True(t, owner.CreatedAt.Equal(after.OldestUnsettledAt))
	clean, err := runtime.AuthoritySnapshot(t.Context(), cleanBoot)
	require.NoError(t, err)
	require.True(t, clean.ProductiveGrantEligible, "fresh progress restores the clean boot while the aged owner's boot remains quarantined")
	var attempts int
	var created time.Time
	require.NoError(t, r.db.QueryRow(`SELECT attempts,created_at FROM login_family_revocations WHERE operation_id=$1`, owner.OperationID).Scan(&attempts, &created))
	require.GreaterOrEqual(t, attempts, 2)
	require.True(t, owner.CreatedAt.Equal(created))
}

func TestVoiceAuthRuntimeBoundedPagesVisitLaterBootAndRotate(t *testing.T) {
	r := newVoiceAuthRig(t)
	deadBoot := "10000000-0000-4000-8000-000000000001"
	healthyBoot := "f0000000-0000-4000-8000-000000000001"
	_, err := r.db.Exec(`INSERT INTO voice_auth_boot_fences(node_boot_id) VALUES($1),($2)`, deadBoot, healthyBoot)
	require.NoError(t, err)
	for index := range 101 {
		r.target.NodeBootID = deadBoot
		r.target.SocketID = "pending-" + strconv.Itoa(index)
		r.target.ConnectionGeneration = uuid.NewString()
		r.register(t)
	}
	r.target.NodeBootID = healthyBoot
	r.target.SocketID = "healthy-late-boot"
	r.target.ConnectionGeneration = uuid.NewString()
	healthy := r.register(t)
	owner := runtimeRevoke(t, r)
	client, observer := runtimeNATS(t)
	sub, err := observer.SubscribeSync("voice.enforce.session.*")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sub.Unsubscribe()) })
	require.NoError(t, observer.Flush())
	reconciler := dmblock.New(r.db, nil)
	runtime := newVoiceAuthRuntime(r.db, client, r.secret, &runtimeFamilyCloser{}, reconciler)
	reconciler.SetAuthRevocationEject(runtime.deliverAuth)
	seen := make(map[string]bool)
	for round := range 3 {
		if round > 0 {
			_, err = r.db.Exec(`UPDATE login_family_revocations SET reconcile_after=clock_timestamp() WHERE operation_id=$1`, owner.OperationID)
			require.NoError(t, err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		result := make(chan error, 1)
		go func() { _, err := reconciler.ReconcileDue(ctx, 100); result <- err }()
		for range 100 {
			message, err := sub.NextMsg(3 * time.Second)
			require.NoError(t, err)
			command, err := protocol.DecodeCommand(message.Data)
			require.NoError(t, err)
			require.NoError(t, protocol.VerifyCommand(command, []byte(r.secret), time.Now()))
			payload, ok := command.Auth()
			require.True(t, ok)
			require.Equal(t, owner.OperationID.String(), payload.OperationID)
			seen[payload.Target.ConnectionGeneration] = true
			if payload.Target.NodeBootID == healthyBoot {
				require.Zero(t, round, "later healthy boot must progress in the first bounded page")
				runtimeRelease(t, r, healthy, nil)
			}
			ack, err := protocol.SignAuthACK(command, []byte(r.secret))
			require.NoError(t, err)
			require.NoError(t, message.Respond(ack))
		}
		require.NoError(t, <-result)
		require.NoError(t, ctx.Err())
		cancel()
		live, _ := closureCount(t, r.db, healthy.Target.ConnectionGeneration)
		require.Zero(t, live, "healthy target must durably release despite a large retained boot")
		_, err = sub.NextMsg(25 * time.Millisecond)
		require.ErrorIs(t, err, rawnats.ErrTimeout, "one owner attempt may emit at most100 target commands")
	}
	require.Len(t, seen, 102, "attempt rotation must cover targets outside the first100 after the released target shrinks the candidate set")
	var complete sql.NullTime
	require.NoError(t, r.db.QueryRow(`SELECT completed_at FROM login_family_revocations WHERE operation_id=$1`, owner.OperationID).Scan(&complete))
	require.False(t, complete.Valid, "unreleased dead-boot evidence must remain retained")
}

func runtimeRotateEpoch(t *testing.T, r *voiceAuthRig) loginfamily.EpochOwner {
	t.Helper()
	fence := credepoch.New(r.db, redistest.Client(t), nil)
	op, err := fence.Begin(t.Context(), r.userID.String())
	require.NoError(t, err)
	t.Cleanup(func() { op.Rollback(t.Context()) })
	tx, err := r.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { err := tx.Rollback(); require.True(t, err == nil || errors.Is(err, sql.ErrTxDone)) }()
	barrier, err := loginfamily.AcquireWriterBarrierTx(t.Context(), tx)
	require.NoError(t, err)
	users, err := loginfamily.LockUsersTx(t.Context(), tx, barrier, []uuid.UUID{r.userID}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := users.ForUser(r.userID)
	require.NoError(t, err)
	targets, err := loginfamily.CaptureEpochTargetsTx(t.Context(), tx, user, r.epoch)
	require.NoError(t, err)
	owner, err := op.RotateTx(t.Context(), tx)
	require.NoError(t, err)
	result, err := loginfamily.RevokeFamiliesForEpochTx(t.Context(), tx, targets, owner)
	require.NoError(t, err)
	require.Equal(t, []loginfamily.DeliveryOwner{{Kind: loginfamily.OwnerEpoch, UserID: r.userID, EpochGeneration: owner.Generation}}, result.Owners)
	require.NoError(t, tx.Commit())
	op.Commit(t.Context())
	return owner
}

func TestVoiceAuthRuntimeEpochActualWireRetainsParentAndOldFrontier(t *testing.T) {
	r := newVoiceAuthRig(t)
	oldBinding := r.register(t)
	owner := runtimeRotateEpoch(t, r)
	var originalCreated time.Time
	require.NoError(t, r.db.QueryRow(`SELECT created_at FROM credential_epoch_voice_ejections WHERE generation=$1`, owner.Generation).Scan(&originalCreated))
	r.epoch = owner.CredentialEpoch
	r.target.CredentialEpoch = owner.CredentialEpoch
	fresh := runtimeFreshFamily(t, r)
	// The existing room rail has its own activation receipt. Preserve this
	// disposable fixture's prior state and restore it after the isolated test.
	var priorActivation sql.NullTime
	require.NoError(t, r.db.QueryRow(`SELECT activated_at FROM voice_enforcement_rollout WHERE id=true`).Scan(&priorActivation))
	_, err := r.db.Exec(`UPDATE voice_enforcement_rollout SET activated_at=clock_timestamp() WHERE id=true`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := r.db.Exec(`UPDATE voice_enforcement_rollout SET activated_at=$1 WHERE id=true`, priorActivation)
		require.NoError(t, err)
	})
	client, observer := runtimeNATS(t)
	sub, err := observer.SubscribeSync("voice.enforce.session." + oldBinding.Target.NodeBootID)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sub.Unsubscribe()) })
	require.NoError(t, observer.Flush())
	reconciler := dmblock.New(r.db, nil)
	runtime := newVoiceAuthRuntime(r.db, client, r.secret, &runtimeFamilyCloser{}, reconciler)
	reconciler.SetCredentialEpochEjectV2(runtime.deliverEpoch)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- reconciler.DeliverCredentialEpochVoiceEjection(ctx, r.userID.String(), owner.CredentialEpoch, owner.SupersededCredentialEpoch)
	}()
	message, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	command, err := protocol.DecodeCommand(message.Data)
	require.NoError(t, err)
	require.NoError(t, protocol.VerifyCommand(command, []byte(r.secret), time.Now()))
	payload, ok := command.Epoch()
	require.True(t, ok)
	require.Equal(t, owner.Generation.String(), payload.ParentGeneration)
	require.Equal(t, owner.SupersededCredentialEpoch, payload.SupersededCredentialEpoch)
	require.Equal(t, oldBinding, protocol.Binding{UserID: payload.UserID, FamilyID: payload.FamilyID, Target: payload.Target})
	ack, err := protocol.SignEpochACK(command, []byte(r.secret))
	require.NoError(t, err)
	require.NoError(t, message.Respond(ack))
	require.NoError(t, <-result, "a retained delivery failure is recorded rather than losing its parent")
	var generation uuid.UUID
	var created time.Time
	require.NoError(t, r.db.QueryRow(`SELECT generation,created_at FROM credential_epoch_voice_ejections WHERE user_id=$1 AND superseded_credential_epoch=$2`, r.userID, owner.SupersededCredentialEpoch).Scan(&generation, &created))
	require.Equal(t, owner.Generation, generation)
	require.True(t, originalCreated.Equal(created))
	var authParents int
	require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM login_family_revocations WHERE user_id=$1`, r.userID).Scan(&authParents))
	require.Zero(t, authParents, "an epoch owner must never gain a duplicate auth parent")
	runtimeRelease(t, r, oldBinding, nil)
	_, err = r.db.Exec(`UPDATE credential_epoch_voice_ejections SET reconcile_after=clock_timestamp() WHERE generation=$1`, owner.Generation)
	require.NoError(t, err)
	require.NoError(t, reconciler.DeliverCredentialEpochVoiceEjection(ctx, r.userID.String(), owner.CredentialEpoch, owner.SupersededCredentialEpoch))
	var remaining int
	require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM credential_epoch_voice_ejections WHERE generation=$1`, owner.Generation).Scan(&remaining))
	require.Zero(t, remaining)
	live, _ := closureCount(t, r.db, fresh.Target.ConnectionGeneration)
	require.Equal(t, 1, live, "new-epoch family is outside the old sealed frontier")
}

func TestVoiceAuthRuntimePreparedDeniesDespiteFreshRound(t *testing.T) {
	r := newVoiceAuthRigPrepared(t)
	reconciler := dmblock.New(r.db, nil)
	runtime := newVoiceAuthRuntime(r.db, nil, r.secret, &runtimeFamilyCloser{}, reconciler)
	_, err := reconciler.ReconcileDue(t.Context(), 100)
	require.NoError(t, err)
	snapshot, err := runtime.AuthoritySnapshot(t.Context(), uuid.MustParse(r.target.NodeBootID))
	require.NoError(t, err)
	require.True(t, snapshot.SQLAvailable)
	require.True(t, snapshot.SecurityRoundFresh)
	require.Equal(t, "prepared", snapshot.Activation)
	require.False(t, snapshot.ProductiveGrantEligible, "prepared state always withholds grants even after proven process progress")
}

type runtimeHeldFamilyCloser struct {
	heldFamily uuid.UUID
	entered    chan uuid.UUID
	mu         sync.Mutex
	live       map[uuid.UUID]int
}

func (h *runtimeHeldFamilyCloser) DisconnectFamilyContext(ctx context.Context, _, family uuid.UUID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	h.live[family]++
	h.mu.Unlock()
	h.entered <- family
	if family == h.heldFamily {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func runtimeTwoFamilyFrontier(t *testing.T, r *voiceAuthRig) ([]protocol.Binding, dmblock.AuthRevocation, *runtimeHeldFamilyCloser) {
	t.Helper()
	bindings := []protocol.Binding{r.register(t), runtimeFreshFamily(t, r)}
	owner := runtimeRevoke(t, r)
	first := uuid.MustParse(bindings[0].FamilyID)
	second := uuid.MustParse(bindings[1].FamilyID)
	if second.String() < first.String() {
		first = second
	}
	hub := &runtimeHeldFamilyCloser{heldFamily: first, entered: make(chan uuid.UUID, 32), live: make(map[uuid.UUID]int)}
	var targets int
	require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM login_family_revocation_targets WHERE operation_id=$1`, owner.OperationID).Scan(&targets))
	require.Equal(t, 2, targets, "both real core-created families must belong to the sealed frontier")
	return bindings, owner, hub
}

// A local close that consumes the deadline must not consume the independent
// media-delivery opportunity. Both receipts are still required for completion.
func TestVoiceAuthRuntimeHeldHubDoesNotPreventExactMediaDelivery(t *testing.T) {
	r := newVoiceAuthRig(t)
	bindings, owner, hub := runtimeTwoFamilyFrontier(t, r)
	client, observer := runtimeNATS(t)
	sub, err := observer.SubscribeSync("voice.enforce.session." + bindings[0].Target.NodeBootID)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sub.Unsubscribe()) })
	require.NoError(t, observer.Flush())
	reconciler := dmblock.New(r.db, nil)
	runtime := newVoiceAuthRuntime(r.db, client, r.secret, hub, reconciler)
	reconciler.SetAuthRevocationEject(runtime.deliverAuth)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := reconciler.ReconcileDue(ctx, 100); result <- err }()
	observedHeld := false
	for !observedHeld {
		select {
		case family := <-hub.entered:
			observedHeld = family == hub.heldFamily
		case <-ctx.Done():
			t.Fatal("the held local close attempt was not observed")
		}
	}
	seen := make(map[string]bool)
	for range 2 {
		message, err := sub.NextMsg(500 * time.Millisecond)
		if err != nil {
			t.Errorf("healthy exact media target was not attempted while the first hub receipt remained held: %v", err)
			break
		}
		command, decodeErr := protocol.DecodeCommand(message.Data)
		require.NoError(t, decodeErr)
		require.NoError(t, protocol.VerifyCommand(command, []byte(r.secret), time.Now()))
		payload, ok := command.Auth()
		require.True(t, ok)
		require.Equal(t, owner.OperationID.String(), payload.OperationID)
		seen[payload.FamilyID] = true
		ack, signErr := protocol.SignAuthACK(command, []byte(r.secret))
		require.NoError(t, signErr)
		require.NoError(t, message.Respond(ack))
	}
	if len(seen) == 2 {
		for _, binding := range bindings {
			runtimeRelease(t, r, binding, nil)
		}
		var live int
		require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM voice_auth_connections WHERE family_id IN(SELECT family_id FROM login_family_revocation_targets WHERE operation_id=$1)`, owner.OperationID).Scan(&live))
		require.Zero(t, live)
		select {
		case err := <-result:
			t.Fatalf("owner finished before the held local close receipt despite ACK and empty SQL: %v", err)
		default:
		}
	}
	cancel()
	require.Error(t, <-result)
	var complete sql.NullTime
	require.NoError(t, r.db.QueryRow(`SELECT completed_at FROM login_family_revocations WHERE operation_id=$1`, owner.OperationID).Scan(&complete))
	require.False(t, complete.Valid, "ACK and empty SQL cannot replace the outstanding hub receipt")
	require.Len(t, seen, 2, "all exact media targets must progress independently of the held close")
}

// Repeated scans from the smallest family UUID must not starve a later member
// of the same immutable frontier when the first callback uses its whole budget.
func TestVoiceAuthRuntimeHeldFirstFamilyDoesNotStarveLaterFamiliesAcrossRetries(t *testing.T) {
	r := newVoiceAuthRig(t)
	bindings, owner, hub := runtimeTwoFamilyFrontier(t, r)
	reconciler := dmblock.New(r.db, nil)
	runtime := newVoiceAuthRuntime(r.db, nil, r.secret, hub, reconciler)
	reconciler.SetAuthRevocationEject(runtime.deliverAuth)
	for range 4 {
		_, err := r.db.Exec(`UPDATE login_family_revocations SET reconcile_after=clock_timestamp() WHERE operation_id=$1`, owner.OperationID)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		_, err = reconciler.ReconcileDue(ctx, 100)
		cancel()
		require.Error(t, err, "held close must keep its owner pending")
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for _, binding := range bindings {
		require.Positive(t, hub.live[uuid.MustParse(binding.FamilyID)], "every sealed family needs a live-context attempt within bounded retries")
	}
	var complete sql.NullTime
	require.NoError(t, r.db.QueryRow(`SELECT completed_at FROM login_family_revocations WHERE operation_id=$1`, owner.OperationID).Scan(&complete))
	require.False(t, complete.Valid)
}

// This is the network boundary only. The production runtime still loads the
// sealed frontier, creates exact signed commands, shares request slots, and
// records durable retry outcomes. A request occupies its slot until cancellation.
type runtimeHeldExactRequester struct {
	secret         string
	entered        chan string
	active         atomic.Int32
	peak           atomic.Int32
	mu             sync.Mutex
	live           map[string]bool
	protocolErrors []error
}

func (r *runtimeHeldExactRequester) RequestWithContext(ctx context.Context, _ string, data interface{}) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(data)
	if err == nil {
		var command protocol.Command
		command, err = protocol.DecodeCommand(raw)
		if err == nil {
			err = protocol.VerifyCommand(command, []byte(r.secret), time.Now())
		}
		if err == nil {
			payload, ok := command.Auth()
			if !ok {
				err = errors.New("expected auth command at held network boundary")
			} else {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				active := r.active.Add(1)
				defer r.active.Add(-1)
				for previous := r.peak.Load(); active > previous; previous = r.peak.Load() {
					if r.peak.CompareAndSwap(previous, active) {
						break
					}
				}
				r.mu.Lock()
				r.live[payload.Target.ConnectionGeneration] = true
				r.mu.Unlock()
				r.entered <- payload.Target.ConnectionGeneration
				<-ctx.Done()
				return nil, ctx.Err()
			}
		}
	}
	r.mu.Lock()
	r.protocolErrors = append(r.protocolErrors, err)
	r.mu.Unlock()
	return nil, err
}

func TestVoiceAuthRuntimeExactHundredTargetsAdvanceAfterShortSlices(t *testing.T) {
	r := newVoiceAuthRig(t)
	bindings := make([]protocol.Binding, 0, 100)
	for index := range 100 {
		r.target.ConnectionGeneration = uuid.NewString()
		r.target.SocketID = "short-slice-" + strconv.Itoa(index)
		bindings = append(bindings, r.register(t))
	}
	owner := runtimeRevoke(t, r)
	requester := &runtimeHeldExactRequester{secret: r.secret, entered: make(chan string, 100), live: make(map[string]bool)}
	reconciler := dmblock.New(r.db, nil)
	runtime := newVoiceAuthRuntime(r.db, requester, r.secret, &runtimeFamilyCloser{}, reconciler)
	reconciler.SetAuthRevocationEject(runtime.deliverAuth)
	missing := ""
	advanced := false
	for attempt := range 100 {
		_, err := r.db.Exec(`UPDATE login_family_revocations SET reconcile_after=clock_timestamp() WHERE operation_id=$1`, owner.OperationID)
		require.NoError(t, err)
		requester.mu.Lock()
		requester.live = make(map[string]bool)
		requester.mu.Unlock()
		ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
		result := make(chan error, 1)
		go func() { _, err := reconciler.ReconcileDue(ctx, 100); result <- err }()
		if attempt == 0 {
			// Observe the real64occupied request boundary before deriving the
			// unvisited target; no worker-arrival ordering is assumed.
			for range 64 {
				select {
				case <-requester.entered:
				case err := <-result:
					cancel()
					t.Fatalf("first slice did not occupy64 live requests: %v", err)
				}
			}
		}
		err = <-result
		cancel()
		if err != nil {
			require.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled), "unexpected bounded-round error: %v", err)
		}
		requester.mu.Lock()
		require.Empty(t, requester.protocolErrors)
		if attempt == 0 {
			require.Len(t, requester.live, 64, "the held first wave must leave real targets unvisited")
			for _, binding := range bindings {
				if !requester.live[binding.Target.ConnectionGeneration] {
					missing = binding.Target.ConnectionGeneration
					break
				}
			}
			require.NotEmpty(t, missing)
		} else {
			advanced = requester.live[missing]
		}
		requester.mu.Unlock()
		for len(requester.entered) > 0 {
			<-requester.entered
		}
		var complete sql.NullTime
		var targets, live int
		require.NoError(t, r.db.QueryRow(`SELECT completed_at FROM login_family_revocations WHERE operation_id=$1`, owner.OperationID).Scan(&complete))
		require.False(t, complete.Valid, "held network requests cannot become successful receipts")
		require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM login_family_revocation_targets WHERE operation_id=$1`, owner.OperationID).Scan(&targets))
		require.Equal(t, 1, targets)
		require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM voice_auth_connections WHERE family_id=$1`, bindings[0].FamilyID).Scan(&live))
		require.Equal(t, 100, live, "bounded retries must retain every unreleased target")
		if advanced {
			break
		}
	}
	require.True(t, advanced, "an initially unvisited target must receive a live request within bounded retries, even when exactly100targets remain")
	require.EqualValues(t, 64, requester.peak.Load())
	require.Zero(t, requester.active.Load())
}
