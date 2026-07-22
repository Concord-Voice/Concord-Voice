package voice

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
)

type permissionEnforcerShutdownScenario struct {
	blockMemberQuery bool
	started          chan struct{}
	canceled         chan struct{}
	release          chan struct{}
	startOnce        sync.Once
	cancelOnce       sync.Once
	releaseOnce      sync.Once
}

func (s *permissionEnforcerShutdownScenario) block(ctx context.Context) (driver.Rows, error) {
	s.startOnce.Do(func() { close(s.started) })
	select {
	case <-ctx.Done():
		s.cancelOnce.Do(func() { close(s.canceled) })
		return nil, ctx.Err()
	case <-s.release:
		return nil, errors.New("test query released without cancellation")
	}
}

type permissionEnforcerShutdownConnector struct {
	scenario *permissionEnforcerShutdownScenario
}

func (c permissionEnforcerShutdownConnector) Connect(context.Context) (driver.Conn, error) {
	return permissionEnforcerShutdownConn(c), nil
}

func (c permissionEnforcerShutdownConnector) Driver() driver.Driver { return c }
func (c permissionEnforcerShutdownConnector) Open(string) (driver.Conn, error) {
	return c.Connect(context.Background())
}

type permissionEnforcerShutdownConn struct {
	scenario *permissionEnforcerShutdownScenario
}

func (permissionEnforcerShutdownConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}
func (permissionEnforcerShutdownConn) Close() error { return nil }
func (permissionEnforcerShutdownConn) Begin() (driver.Tx, error) {
	return nil, errors.New("begin is not supported")
}
func (c permissionEnforcerShutdownConn) QueryContext(
	ctx context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Rows, error) {
	if strings.Contains(query, "timed_out_until IS NOT NULL") {
		if !c.scenario.blockMemberQuery {
			return c.scenario.block(ctx)
		}
		return permissionEnforcerShutdownRows{}, nil
	}
	if c.scenario.blockMemberQuery && strings.Contains(query, "SELECT EXISTS(SELECT 1 FROM server_members") {
		return c.scenario.block(ctx)
	}
	return nil, errors.New("unexpected permission-enforcer shutdown query")
}

var _ driver.QueryerContext = permissionEnforcerShutdownConn{}

type permissionEnforcerShutdownRows struct{}

func (permissionEnforcerShutdownRows) Columns() []string         { return []string{"user_id"} }
func (permissionEnforcerShutdownRows) Close() error              { return nil }
func (permissionEnforcerShutdownRows) Next([]driver.Value) error { return io.EOF }

type permissionEnforcerShutdownPublisher struct{}

func (permissionEnforcerShutdownPublisher) Publish(string, interface{}) error { return nil }
func (permissionEnforcerShutdownPublisher) Request(string, interface{}, time.Duration) ([]byte, error) {
	return nil, errors.New("unexpected request")
}
func (permissionEnforcerShutdownPublisher) FlushTimeout(time.Duration) error { return nil }

func TestPermissionEnforcerCloseCancelsActiveHeartbeatBatch(t *testing.T) {
	// Regression for #2231: the heartbeat permission drain must be canceled and
	// joined before its database and NATS dependencies are closed.
	for _, test := range []struct {
		name             string
		blockMemberQuery bool
	}{
		{name: "batch timeout lookup", blockMemberQuery: false},
		{name: "per-participant permission resolve", blockMemberQuery: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := &permissionEnforcerShutdownScenario{
				blockMemberQuery: test.blockMemberQuery,
				started:          make(chan struct{}),
				canceled:         make(chan struct{}),
				release:          make(chan struct{}),
			}
			db := sql.OpenDB(permissionEnforcerShutdownConnector{scenario: scenario})
			t.Cleanup(func() {
				scenario.releaseOnce.Do(func() { close(scenario.release) })
				if err := db.Close(); err != nil {
					t.Errorf("close test database: %v", err)
				}
			})
			testLog := logger.NewWithWriter(io.Discard)
			resolver := rbac.NewResolver(db, nil, testLog)
			enforcer := NewPermissionEnforcer(db, testLog, resolver, nil)
			enforcer.nats = permissionEnforcerShutdownPublisher{}

			participants := make([]uuid.UUID, maxServerVoiceParticipantIDs)
			for index := range participants {
				participants[index] = uuid.NewSHA1(uuid.NameSpaceOID, []byte(strconv.Itoa(index)))
			}
			enforcer.RecheckParticipants(
				"11111111-1111-1111-1111-111111111111",
				"22222222-2222-2222-2222-222222222222",
				participants,
			)

			select {
			case <-scenario.started:
			case <-time.After(time.Second):
				t.Fatal("heartbeat permission batch did not reach the blocking query")
			}

			closer, ok := interface{}(enforcer).(interface{ Close() })
			if !ok {
				t.Fatal("PermissionEnforcer has no Close lifecycle; active heartbeat work is untracked")
			}
			closed := make(chan struct{})
			go func() {
				closer.Close()
				close(closed)
			}()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("PermissionEnforcer.Close did not cancel and join the active heartbeat batch")
			}
			select {
			case <-scenario.canceled:
			default:
				t.Fatal("PermissionEnforcer.Close returned before the active query observed cancellation")
			}
		})
	}
}
