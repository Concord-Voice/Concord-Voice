package api

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// fakePermissionBroadcaster records every BroadcastToUserContext call with the
// state of the context it was handed at that moment.
type fakePermissionBroadcaster struct {
	result bool // returned from every call

	mu    sync.Mutex
	calls []fakeBroadcast
}

type fakeBroadcast struct {
	userID      uuid.UUID
	msg         websocket.OutgoingMessage
	ctxErr      error
	hasDeadline bool
	remaining   time.Duration
	marker      any
}

type broadcastMarker struct{}

func (f *fakePermissionBroadcaster) BroadcastToUserContext(
	ctx context.Context, userID uuid.UUID, msg websocket.OutgoingMessage,
) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	deadline, ok := ctx.Deadline()
	call := fakeBroadcast{
		userID: userID, msg: msg, ctxErr: ctx.Err(), hasDeadline: ok, marker: ctx.Value(broadcastMarker{}),
	}
	if ok {
		call.remaining = time.Until(deadline)
	}
	f.calls = append(f.calls, call)
	return f.result
}

func (f *fakePermissionBroadcaster) recorded() []fakeBroadcast {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeBroadcast(nil), f.calls...)
}

func newTestNotifier(result bool) (*permissionChangeNotifier, *fakePermissionBroadcaster, *bytes.Buffer) {
	var buf bytes.Buffer
	fake := &fakePermissionBroadcaster{result: result}
	return newPermissionChangeNotifier(fake, logger.NewWithWriter(&buf)), fake, &buf
}

// wantFailureMsg is the failure line's message. announceServerPermissionsChanged
// in internal/servers emits the same text and its test pins it too, so the two
// emitters cannot drift apart (backend.md documents the line as byte-identical).
const wantFailureMsg = `msg="Failed to announce a permission change"`

var uuidPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// The frame's bytes are the contract with PermissionsChangedSchema: a nil Data
// would marshal as "data":null, which the schema rejects, and any extra key
// would tell a client why its permissions changed.
//
// Kills: Data changed to nil (null), a key added to Data, the Type renamed, and
// the message built per direction instead of once.
func TestPermissionsChangedMessage_Bytes(t *testing.T) {
	got, err := json.Marshal(permissionsChangedMessage())
	require.NoError(t, err)
	assert.Equal(t, `{"type":"permissions_changed","data":{}}`, string(got))
}

// Kills: the broadcast sent to the wrong user, sent twice, or sent through a
// message other than permissionsChangedMessage; the success path logging.
func TestNotifyPermissionsChanged_SendsOneFrameToTheUser(t *testing.T) {
	n, fake, logs := newTestNotifier(true)
	user := uuid.New()

	n.NotifyPermissionsChanged(context.Background(), user.String())

	calls := fake.recorded()
	require.Len(t, calls, 1, "exactly one user event per notification")
	assert.Equal(t, user, calls[0].userID)
	wire, err := json.Marshal(calls[0].msg)
	require.NoError(t, err)
	assert.Equal(t, `{"type":"permissions_changed","data":{}}`, string(wire))
	assert.Empty(t, logs.String(), "a delivered notification logs nothing")
}

// The send runs on the request goroutine after the commit, so it must survive a
// hung-up client and must be bounded.
//
// Kills: context.WithoutCancel dropped (ctxErr is Canceled and the send is
// refused), context.Background() used instead of the request's (the marker is
// lost), the WithTimeout dropped (no deadline), and the timeout raised past one
// second.
func TestNotifyPermissionsChanged_ContextIsDetachedAndBounded(t *testing.T) {
	n, fake, _ := newTestNotifier(true)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), broadcastMarker{}, "kept"))
	cancel()

	n.NotifyPermissionsChanged(ctx, uuid.NewString())

	calls := fake.recorded()
	require.Len(t, calls, 1)
	assert.NoError(t, calls[0].ctxErr, "a hung-up client must not cancel the broadcast")
	assert.Equal(t, "kept", calls[0].marker, "the request's values must reach the hub")
	require.True(t, calls[0].hasDeadline, "the broadcast must be bounded")
	assert.LessOrEqual(t, calls[0].remaining, time.Second)
	assert.Greater(t, calls[0].remaining, time.Duration(0))
}

// A refused broadcast logs ONE line that names the failure class and nothing
// else (I7): no user id, and nothing that distinguishes a gained factor from a
// lost one, which the signature already cannot carry.
//
// Kills: the false return ignored (no line), a second line added, the message
// or the class renamed, and any field (user id, error text, direction) added to the line.
func TestNotifyPermissionsChanged_RefusedBroadcastLogsOneAnonymousLine(t *testing.T) {
	n, fake, logs := newTestNotifier(false)
	user := uuid.NewString()

	n.NotifyPermissionsChanged(context.Background(), user)

	require.Len(t, fake.recorded(), 1)
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	require.Len(t, lines, 1, "exactly one log line: %q", logs.String())
	assert.Contains(t, lines[0], wantFailureMsg)
	assert.Contains(t, lines[0], "failure_class=perm_change_broadcast")
	assert.NotContains(t, lines[0], user)
	assert.False(t, uuidPattern.MatchString(lines[0]), "the line must carry no identifier: %q", lines[0])
	assert.NotContains(t, lines[0], "error=", "the line carries no error text")
	fields := regexp.MustCompile(`\b[a-z_]+=`).FindAllString(lines[0], -1)
	assert.ElementsMatch(t, []string{"time=", "level=", "msg=", "failure_class="}, fields,
		"the line may carry the class and nothing else: %q", lines[0])
}

// An id that is not a UUID cannot name a user, so nothing is sent; the failure
// is still counted as one line, never silently dropped.
//
// Kills: the parse error ignored (a send to uuid.Nil), and the parse failure
// returning without logging.
func TestNotifyPermissionsChanged_UnparseableUserIDSendsNothingAndLogsOnce(t *testing.T) {
	n, fake, logs := newTestNotifier(true)

	n.NotifyPermissionsChanged(context.Background(), "not-a-uuid")

	assert.Empty(t, fake.recorded(), "no broadcast without a user to address")
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	require.Len(t, lines, 1, "%q", logs.String())
	assert.Contains(t, lines[0], wantFailureMsg)
	assert.Contains(t, lines[0], "failure_class=perm_change_broadcast")
	assert.NotContains(t, lines[0], "not-a-uuid")
}
