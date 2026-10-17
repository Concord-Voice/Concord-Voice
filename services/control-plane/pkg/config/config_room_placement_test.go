package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadRoomPlacementConfigStrictGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   *string
		want    bool
		wantErr bool
	}{
		{name: "unset defaults off"},
		{name: "false", value: stringPointer("false")},
		{name: "true", value: stringPointer("true"), want: true},
		{name: "empty is malformed", value: stringPointer(""), wantErr: true},
		{name: "uppercase is malformed", value: stringPointer("TRUE"), wantErr: true},
		{name: "whitespace is malformed", value: stringPointer(" true"), wantErr: true},
		{name: "numeric is malformed", value: stringPointer("1"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setOptionalEnv(t, "ROOM_PLACEMENT_ENABLED", tc.value)
			cfg := &Config{}
			err := loadRoomPlacementConfig(cfg)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "ROOM_PLACEMENT_ENABLED")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.RoomPlacementEnabled)
		})
	}

	require.Error(t, loadRoomPlacementConfig(nil))
}

func TestLoadReadsRoomPlacementGate(t *testing.T) {
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("ROOM_PLACEMENT_ENABLED", "true")

	cfg, err := Load()
	require.NoError(t, err)
	assert.True(t, cfg.RoomPlacementEnabled)
}
