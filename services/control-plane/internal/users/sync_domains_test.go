package users

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func syncDomainB64(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }

func validDomainRotation() *changePasswordDomainRotation {
	return &changePasswordDomainRotation{EncryptedData: syncDomainB64(16), ExpectedVersion: 2}
}

func validSyncDomains() *changePasswordSyncDomains {
	return &changePasswordSyncDomains{
		Preferences:        validDomainRotation(),
		SavedGifs:          &changePasswordDomainRotation{ExpectedVersion: 0},
		FriendOrganization: validDomainRotation(),
	}
}

func TestValidateChangePasswordSyncDomains(t *testing.T) {
	t.Run("nil sync_domains is valid (legacy client)", func(t *testing.T) {
		assert.NoError(t, validateChangePasswordSyncDomains(nil))
	})
	t.Run("all three accounted is valid", func(t *testing.T) {
		assert.NoError(t, validateChangePasswordSyncDomains(validSyncDomains()))
	})
	t.Run("missing sub-field rejects", func(t *testing.T) {
		sd := validSyncDomains()
		sd.SavedGifs = nil
		err := validateChangePasswordSyncDomains(sd)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "saved_gifs")
	})
	t.Run("negative expected_version rejects", func(t *testing.T) {
		sd := validSyncDomains()
		sd.Preferences.ExpectedVersion = -1
		assert.Error(t, validateChangePasswordSyncDomains(sd))
	})
	t.Run("version zero with data rejects (assert-absent must carry no data)", func(t *testing.T) {
		sd := validSyncDomains()
		sd.SavedGifs = &changePasswordDomainRotation{EncryptedData: syncDomainB64(8), ExpectedVersion: 0}
		assert.Error(t, validateChangePasswordSyncDomains(sd))
	})
	t.Run("preserve marker (version>0, no data) is valid", func(t *testing.T) {
		sd := validSyncDomains()
		sd.FriendOrganization = &changePasswordDomainRotation{ExpectedVersion: 3}
		assert.NoError(t, validateChangePasswordSyncDomains(sd))
	})
	t.Run("non-base64 data rejects", func(t *testing.T) {
		sd := validSyncDomains()
		sd.Preferences.EncryptedData = "not-base64!!!"
		assert.Error(t, validateChangePasswordSyncDomains(sd))
	})
	t.Run("oversize data rejects", func(t *testing.T) {
		sd := validSyncDomains()
		sd.Preferences.EncryptedData = strings.Repeat("A", presenceOverrideMaxEncryptedDataBytes+4)
		assert.Error(t, validateChangePasswordSyncDomains(sd))
	})
}
