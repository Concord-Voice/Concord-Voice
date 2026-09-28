package voice

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var errFenceProbeStopped = errors.New("stop after fence observation")

type cleanupFenceProbeConnector struct {
	opened    func() uint64
	seen      uint64
	temporary bool
}

func (c *cleanupFenceProbeConnector) Connect(context.Context) (driver.Conn, error) {
	return &cleanupFenceProbeConn{probe: c}, nil
}

func (*cleanupFenceProbeConnector) Driver() driver.Driver { return cleanupFenceProbeDriver{} }

type cleanupFenceProbeDriver struct{}

func (cleanupFenceProbeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use Connector")
}

type cleanupFenceProbeConn struct{ probe *cleanupFenceProbeConnector }

func (*cleanupFenceProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (*cleanupFenceProbeConn) Close() error                { return nil }
func (c *cleanupFenceProbeConn) Begin() (driver.Tx, error) { return c.begin() }
func (c *cleanupFenceProbeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return c.begin()
}
func (c *cleanupFenceProbeConn) begin() (driver.Tx, error) {
	c.probe.seen = c.probe.opened()
	return nil, errFenceProbeStopped
}
func (c *cleanupFenceProbeConn) QueryContext(
	_ context.Context, query string, _ []driver.NamedValue,
) (driver.Rows, error) {
	if strings.Contains(query, "SELECT EXISTS") {
		return &cleanupFenceProbeRows{columns: []string{"exists"}, values: []driver.Value{c.probe.temporary}}, nil
	}
	return &cleanupFenceProbeRows{
		columns: []string{"temporary", "permanent"}, values: []driver.Value{c.probe.temporary, !c.probe.temporary},
	}, nil
}

type cleanupFenceProbeRows struct {
	columns []string
	values  []driver.Value
	done    bool
}

func (r *cleanupFenceProbeRows) Columns() []string { return r.columns }
func (*cleanupFenceProbeRows) Close() error        { return nil }
func (r *cleanupFenceProbeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, r.values)
	return nil
}

func openCleanupFenceProbeDB(t *testing.T, hub *websocket.Hub) (*sql.DB, *cleanupFenceProbeConnector) {
	t.Helper()
	probe := &cleanupFenceProbeConnector{opened: hub.PresenceAuthzOpenForTest, temporary: true}
	db := sql.OpenDB(probe)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db, probe
}

func TestOrphanTemporaryGrantCleanupRaisesAudienceFenceBeforeTransaction(t *testing.T) {
	hub := websocket.NewHub(nil, nil)
	db, probe := openCleanupFenceProbeDB(t, hub)
	mgr := &tempGrantManager{db: db, hub: hub}

	_, err := mgr.revokeOrphanedTemporaryChannelAccess(
		context.Background(), uuid.NewString(), "11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
	)
	require.ErrorIs(t, err, errFenceProbeStopped)
	require.NotZero(t, probe.seen,
		"orphan temporary-grant cleanup must raise the audience revocation fence before BeginTx")
}

func TestStaleTemporaryGrantCleanupRaisesAudienceFenceBeforeTransaction(t *testing.T) {
	hub := websocket.NewHub(nil, nil)
	db, probe := openCleanupFenceProbeDB(t, hub)
	subscriber := &NATSSubscriber{
		db: db, hub: hub, tempGrant: &tempGrantManager{db: db, hub: hub},
	}
	candidate := staleServerVoiceParticipant{
		channelID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		userID:    uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		serverID:  uuid.MustParse("33333333-3333-3333-3333-333333333333"),
	}
	_, _, err := subscriber.reconcileStaleServerVoiceParticipant(
		context.Background(), candidate, 60, &serverVoiceTerminalOutcomes{},
	)
	require.ErrorIs(t, err, errFenceProbeStopped)
	require.NotZero(t, probe.seen,
		"stale temporary-grant cleanup must raise the audience revocation fence before BeginTx")
}

func TestCleanupWithoutTemporaryGrantSkipsAudienceFence(t *testing.T) {
	for _, cleanup := range []string{"orphan", "stale"} {
		t.Run(cleanup, func(t *testing.T) {
			hub := websocket.NewHub(nil, nil)
			db, probe := openCleanupFenceProbeDB(t, hub)
			probe.temporary = false
			if cleanup == "orphan" {
				mgr := &tempGrantManager{db: db, hub: hub}
				_, err := mgr.revokeOrphanedTemporaryChannelAccess(context.Background(), uuid.NewString(), uuid.NewString(), uuid.NewString())
				require.ErrorIs(t, err, errFenceProbeStopped)
			} else {
				subscriber := &NATSSubscriber{db: db, hub: hub, tempGrant: &tempGrantManager{db: db, hub: hub}}
				candidate := staleServerVoiceParticipant{channelID: uuid.New(), userID: uuid.New(), serverID: uuid.New()}
				_, _, err := subscriber.reconcileStaleServerVoiceParticipant(context.Background(), candidate, 60, &serverVoiceTerminalOutcomes{})
				require.ErrorIs(t, err, errFenceProbeStopped)
			}
			require.Zero(t, probe.seen, "a permanent grant must not open the temporary-grant audience fence")
		})
	}
}

func TestCleanupAudienceFenceClosesAfterCommitBeforeEffects(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	for _, tc := range []struct {
		file, start, end string
	}{
		{"temp_grants.go", "func (m *tempGrantManager) revokeOrphanedTemporaryChannelAccess(", "func orphanTemporaryGrantEligibleTx("},
		{"nats.go", "func (s *NATSSubscriber) reconcileStaleServerVoiceParticipant(", "func (s *NATSSubscriber) prepareStaleServerVoiceTerminalGrant("},
	} {
		t.Run(tc.file, func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join(filepath.Dir(testFile), tc.file)) // #nosec G304 -- fixed sibling source
			require.NoError(t, err)
			start := strings.Index(string(source), tc.start)
			require.GreaterOrEqual(t, start, 0)
			end := strings.Index(string(source[start:]), tc.end)
			require.Greater(t, end, 0)
			body := string(source[start : start+end])
			commit := strings.LastIndex(body, "if err := tx.Commit(); err != nil {")
			closeFence := strings.LastIndex(body, "\tcloseAudienceFence()")
			effects := strings.LastIndex(body, "completeTemporaryGrantRevocation(")
			require.Greater(t, commit, 0)
			require.Greater(t, closeFence, commit, "fence must cover the committing transaction")
			require.Greater(t, effects, closeFence, "post-commit effects must not hold the global fence")
			require.Contains(t, body[:commit], "defer closeAudienceFence()", "error paths must release the fence")
		})
	}
}

func TestExplicitTemporaryGrantRevokeFenceProbeControl(t *testing.T) {
	hub := websocket.NewHub(nil, nil)
	db, probe := openCleanupFenceProbeDB(t, hub)
	mgr := &tempGrantManager{db: db, hub: hub}

	_, _, _, err := mgr.deleteTemporaryGrantWithCapture(
		context.Background(), uuid.NewString(), uuid.NewString(), uuid.NewString(), temporaryGrantAuthorization{},
	)
	require.ErrorIs(t, err, errFenceProbeStopped)
	require.NotZero(t, probe.seen,
		"positive control: the existing explicit temporary-grant revoke must open the fence before BeginTx")
}
