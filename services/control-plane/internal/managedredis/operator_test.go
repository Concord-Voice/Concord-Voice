package managedredis

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOperatorDeniesUnreviewedCommandsBeforeRedis(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, _ := newControllerFixture(t, admin)
	prepareController(t, controller)
	for _, args := range [][]string{
		{"CONFIG", "GET", "*"},
		{"GET", "unrelated:key"},
		{"--scan", "--pattern", "voice:room_owner:other*"},
		{"EVAL", "return 1", "2", "voice:room_owner:00000000-0000-4000-8000-000000000001", "voice:room_live:00000000-0000-4000-8000-000000000001", "blue"},
		{"GET", "media:active_colour\nCONFIG GET *"},
	} {
		_, err := controller.ExecuteOperator(context.Background(), args)
		require.ErrorIs(t, err, errController)
		require.Empty(t, admin.operatorArgs)
	}
	admin.operatorOutput = "green"
	output, err := controller.ExecuteOperator(context.Background(), []string{"GET", "media:active_colour"})
	require.NoError(t, err)
	require.Equal(t, "green", output)
	require.Equal(t, []string{"GET", "media:active_colour"}, admin.operatorArgs)

	admin.operatorOutput = strings.Repeat("x", 16*1024+1)
	_, err = controller.ExecuteOperator(context.Background(), []string{"GET", "media:active_colour"})
	require.ErrorIs(t, err, errController)
}

func TestOperatorAllowsOnlyReviewedMediaCommands(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}, operatorOutput: "ok"}
	controller, _ := newControllerFixture(t, admin)
	prepareController(t, controller)
	room := "00000000-0000-4000-8000-000000000001"
	otherRoom := "00000000-0000-4000-8000-000000000002"
	for _, tc := range []struct {
		name    string
		args    []string
		allowed bool
	}{
		{"active colour", []string{"SET", "media:active_colour", "blue"}, true},
		{"drain generation", []string{"SET", "media:colour_draining:green", "generation-12"}, true},
		{"register", []string{"SADD", "media:colours:registered", "green"}, true},
		{"unregister", []string{"SREM", "media:colours:registered", "blue"}, true},
		{"membership", []string{"SISMEMBER", "media:colours:registered", "green"}, true},
		{"read", []string{"EXISTS", "media:colour_draining:blue"}, true},
		{"drain delete", []string{"DEL", "media:colour_draining:blue"}, true},
		{"room scan", []string{"--scan", "--pattern", "voice:room_live:*"}, true},
		{"room cleanup", []string{"EVAL", cleanupRoomScript, "2", "voice:room_owner:" + room, "voice:room_live:" + room, "blue"}, true},
		{"unknown colour", []string{"SET", "media:active_colour", "red"}, false},
		{"unbounded generation", []string{"SET", "media:colour_draining:green", strings.Repeat("x", 129)}, false},
		{"wrong membership key", []string{"SADD", "media:active_colour", "blue"}, false},
		{"wrong room", []string{"EVAL", cleanupRoomScript, "2", "voice:room_owner:" + room, "voice:room_live:" + otherRoom, "blue"}, false},
		{"unknown script", []string{"EVAL", "return 1", "2", "voice:room_owner:" + room, "voice:room_live:" + room, "blue"}, false},
		{"extra argument", []string{"DEL", "media:colour_draining:blue", "media:active_colour"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admin.operatorArgs = nil
			output, err := controller.ExecuteOperator(context.Background(), tc.args)
			if !tc.allowed {
				require.ErrorIs(t, err, errController)
				require.Empty(t, admin.operatorArgs, "unreviewed command must not reach Redis")
				return
			}
			require.NoError(t, err)
			require.Equal(t, "ok", output)
			require.Equal(t, tc.args, admin.operatorArgs)
		})
	}
}
