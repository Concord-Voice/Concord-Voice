package websocket

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func awaitDMVisibilityFailureDelivery(t *testing.T, hub *Hub, message DMBroadcastMessage) {
	t.Helper()
	completeDMDelivery(t, hub, message)
}

func TestDMMessageDelivery_FailsClosedForInvalidSource(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	awaitDMVisibilityFailureDelivery(t, fixture.hub, DMBroadcastMessage{
		ConversationID:   fixture.conversationID,
		VisibilitySource: &DMMessageVisibilitySource{},
		VisibilityMode:   dmVisibilityDeliverySubscribers,
		Data:             OutgoingMessage{Type: "private"},
	})
	requireDMDelivery(t, fixture.actorClient, false)
	requireDMDelivery(t, fixture.peerClient, false)
}

func TestDMMessageDeliveryCandidates_UnknownAudienceHasNoCandidates(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	source := NewDMMessageVisibilitySource(fixture.messageID)
	candidates := fixture.hub.snapshotDMMessageDeliveryCandidates(DMBroadcastMessage{
		ConversationID: fixture.conversationID, VisibilitySource: &source,
		VisibilityMode: dmVisibilityDeliveryMode(255),
	}, nil)
	assert.Empty(t, candidates)
	awaitDMVisibilityFailureDelivery(t, fixture.hub, DMBroadcastMessage{
		ConversationID: fixture.conversationID, VisibilitySource: &source,
		VisibilityMode: dmVisibilityDeliveryMode(255), Data: OutgoingMessage{Type: "private"},
	})
	requireDMDelivery(t, fixture.actorClient, false)
	requireDMDelivery(t, fixture.peerClient, false)
}

func TestDMMessageDelivery_FailsClosedWithoutDatabase(t *testing.T) {
	hub := NewHub(nil, nil)
	client := &Client{ID: uuid.New(), UserID: uuid.New(), Send: make(chan []byte, 1)}
	hub.clients[client.ID] = client
	source := NewDMMessageVisibilitySource(uuid.New())
	awaitDMVisibilityFailureDelivery(t, hub, DMBroadcastMessage{
		ConversationID: uuid.New(), VisibilitySource: &source,
		VisibilityMode: dmVisibilityDeliveryOriginClient, OriginClientID: &client.ID,
		Data: OutgoingMessage{Type: "private"},
	})
	requireDMDelivery(t, client, false)
}

func TestDMMessageDelivery_FailsClosedWhenDatabaseIsClosed(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	fixture.hub.db = unavailableDMVisibilityDB(t)
	awaitDMVisibilityFailureDelivery(t, fixture.hub, fixture.broadcastMessage())
	requireDMDelivery(t, fixture.actorClient, false)
	requireDMDelivery(t, fixture.peerClient, false)
}

func TestDMMessageDelivery_FailsClosedWhenParticipantIsMissing(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	_, err := fixture.db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, fixture.conversationID, fixture.peerID)
	require.NoError(t, err)
	awaitDMVisibilityFailureDelivery(t, fixture.hub, fixture.broadcastMessage())
	requireDMDelivery(t, fixture.actorClient, true)
	requireDMDelivery(t, fixture.peerClient, false)
}

func TestDMMessageDeliveryCandidates_FailsClosedForParticipantQueryError(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	fixture.hub.db = unavailableDMVisibilityDB(t)
	source := NewDMMessageVisibilitySource(fixture.messageID)
	awaitDMVisibilityFailureDelivery(t, fixture.hub, DMBroadcastMessage{
		ConversationID: fixture.conversationID, VisibilitySource: &source,
		VisibilityMode: dmVisibilityDeliveryAllConnected, Data: OutgoingMessage{Type: "private"},
	})
	requireDMDelivery(t, fixture.actorClient, false)
	requireDMDelivery(t, fixture.peerClient, false)
}

func unavailableDMVisibilityDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", "postgres://127.0.0.1:1/unavailable?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type dmDeliveryFailureDB struct {
	stage      string
	userID     uuid.UUID
	reached    bool
	rolledBack bool
}

func (d *dmDeliveryFailureDB) Open(string) (driver.Conn, error) {
	return &dmDeliveryFailureConn{db: d}, nil
}

type dmDeliveryFailureConn struct{ db *dmDeliveryFailureDB }

func (*dmDeliveryFailureConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}
func (*dmDeliveryFailureConn) Close() error              { return nil }
func (*dmDeliveryFailureConn) Begin() (driver.Tx, error) { return nil, errors.New("use BeginTx") }
func (c *dmDeliveryFailureConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return &dmDeliveryFailureTx{db: c.db}, nil
}

type dmDeliveryFailureTx struct{ db *dmDeliveryFailureDB }

func (t *dmDeliveryFailureTx) Commit() error   { return nil }
func (t *dmDeliveryFailureTx) Rollback() error { t.db.rolledBack = true; return nil }
func (c *dmDeliveryFailureConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "ORDER BY user_id FOR SHARE") {
		if c.db.stage == "lock-query" {
			c.db.reached = true
			return nil, errors.New("lock query failed")
		}
		return &dmDeliveryFailureRows{columns: []string{"user_id"}, values: [][]driver.Value{{c.db.userID.String()}}}, nil
	}
	if c.db.stage == "visibility-query" {
		c.db.reached = true
		return nil, errors.New("visibility query failed")
	}
	if c.db.stage == "visibility-scan" {
		c.db.reached = true
		return &dmDeliveryFailureRows{columns: []string{"user_id"}, values: [][]driver.Value{{"not-a-uuid"}}}, nil
	}
	if c.db.stage == "visibility-rows" {
		c.db.reached = true
		return &dmDeliveryFailureRows{columns: []string{"user_id"}, terminalErr: errors.New("visibility rows failed")}, nil
	}
	c.db.reached = true
	return &dmDeliveryFailureRows{columns: []string{"user_id"}, values: [][]driver.Value{{c.db.userID.String()}}}, nil
}

var _ driver.QueryerContext = (*dmDeliveryFailureConn)(nil)

type dmDeliveryFailureRows struct {
	columns     []string
	values      [][]driver.Value
	terminalErr error
	index       int
}

func (r *dmDeliveryFailureRows) Columns() []string { return r.columns }
func (*dmDeliveryFailureRows) Close() error        { return nil }
func (r *dmDeliveryFailureRows) Next(dest []driver.Value) error {
	if r.index < len(r.values) {
		copy(dest, r.values[r.index])
		r.index++
		return nil
	}
	if r.terminalErr != nil {
		err := r.terminalErr
		r.terminalErr = nil
		return err
	}
	return io.EOF
}

func openDMDeliveryFailureDB(t *testing.T, stage string) (*sql.DB, *dmDeliveryFailureDB) {
	t.Helper()
	state := &dmDeliveryFailureDB{stage: stage}
	name := "websocket-dm-delivery-failure-" + uuid.NewString()
	sql.Register(name, state)
	db, err := sql.Open(name, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, state
}

func TestDMMessageDelivery_FailsClosedAndRollsBackDatabaseFailures(t *testing.T) {
	for _, stage := range []string{"lock-query", "visibility-query", "visibility-scan", "visibility-rows"} {
		t.Run(stage, func(t *testing.T) {
			db, state := openDMDeliveryFailureDB(t, stage)
			hub := NewHub(db, nil)
			conversationID, userID, clientID := uuid.New(), uuid.New(), uuid.New()
			client := &Client{ID: clientID, UserID: userID, Send: make(chan []byte, 1)}
			state.userID = userID
			hub.clients[clientID] = client
			hub.dmSubscriptions[conversationID] = map[uuid.UUID]bool{clientID: true}
			source := NewDMMessageVisibilitySource(uuid.New())
			awaitDMVisibilityFailureDelivery(t, hub, DMBroadcastMessage{
				ConversationID: conversationID, VisibilitySource: &source,
				VisibilityMode: dmVisibilityDeliverySubscribers, Data: OutgoingMessage{Type: "private"},
			})
			requireDMDelivery(t, client, false)
			assert.True(t, state.reached)
			assert.True(t, state.rolledBack)
		})
	}

	t.Run("success control delivers visible frame", func(t *testing.T) {
		db, state := openDMDeliveryFailureDB(t, "success")
		hub := NewHub(db, nil)
		conversationID, userID, clientID := uuid.New(), uuid.New(), uuid.New()
		state.userID = userID
		client := &Client{ID: clientID, UserID: userID, Send: make(chan []byte, 1)}
		hub.clients[clientID] = client
		hub.dmSubscriptions[conversationID] = map[uuid.UUID]bool{clientID: true}
		source := NewDMMessageVisibilitySource(uuid.New())
		awaitDMVisibilityFailureDelivery(t, hub, DMBroadcastMessage{ConversationID: conversationID, VisibilitySource: &source,
			VisibilityMode: dmVisibilityDeliverySubscribers, Data: OutgoingMessage{Type: "private"}})
		requireDMDelivery(t, client, true)
		assert.True(t, state.reached)
	})
}

func TestDMMessageDelivery_WrapperDispatchesTypedVisibility(t *testing.T) {
	hub := NewHub(nil, nil)
	conversationID, messageID := uuid.New(), uuid.New()
	hub.BroadcastToDMMessageRecipients(conversationID, messageID, OutgoingMessage{Type: "reaction"})
	queued := <-hub.dmBroadcast
	require.NotNil(t, queued.VisibilitySource)
	assert.Equal(t, conversationID, queued.ConversationID)
	assert.Equal(t, messageID, queued.VisibilitySource.messageID)
	assert.Equal(t, dmVisibilityDeliverySubscribers, queued.VisibilityMode)

	excluded := uuid.New()
	source := NewDeletedDMMessageVisibilitySource(uuid.New(), time.Now().UTC())
	hub.BroadcastToDMMessageAllParticipants(conversationID, source, excluded, OutgoingMessage{Type: "pin"})
	queued = <-hub.dmBroadcast
	require.NotNil(t, queued.VisibilitySource)
	assert.Equal(t, source, *queued.VisibilitySource)
	assert.Equal(t, &excluded, queued.ExcludeUser)
	assert.Equal(t, dmVisibilityDeliveryAllConnected, queued.VisibilityMode)
}
