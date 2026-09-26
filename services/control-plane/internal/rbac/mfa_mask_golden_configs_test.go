package rbac_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// I-1 of #3453 (T8): extends T0's golden table (mfa_mask_golden_test.go,
// TestMFAMaskGolden_Off) to the two enforcing configurations. Off is already
// covered there; this file adds on+enrolled (byte-identical to golden, C-2's
// owner delta already baked into goldenPerms/goldenHasPerm) and on+unenrolled
// (rbac.MFAMask{Enforcing: true}.Apply(golden), computed here from the golden
// constants rather than hand-copied).
//
// GetMyServerPermissions over HTTP, ListServers over HTTP and the role-guard
// pre-check are exercised end to end by internal/servers/mfa_mask_list_servers_test.go
// (TestListServersMFAMask, TestListServersMFAMask_EnrollmentReadErrorFailsClosed)
// and internal/rbac/role_guard_denial_regression_test.go
// (TestRoleMutationDenial_MFAMaskAppliesInPreCheck, O-1) respectively. The
// golden personas here add no case those two files miss, so they are
// referenced rather than duplicated.

// applyToGoldenPerms returns a persona -> entryPoint -> Permission map with
// mask.Apply run over every golden value -- never hand-derived. Reusing the
// T0 literals rather than re-deriving new ones means a broken Apply cannot
// coincidentally agree with a second set of hand-picked expectations.
func applyToGoldenPerms(mask rbac.MFAMask) map[string]map[string]rbac.Permission {
	out := make(map[string]map[string]rbac.Permission, len(goldenPerms))
	for persona, entries := range goldenPerms {
		row := make(map[string]rbac.Permission, len(entries))
		for entryPoint, want := range entries {
			row[entryPoint] = mask.Apply(want)
		}
		out[persona] = row
	}
	return out
}

// applyToGoldenHasPerm mirrors applyToGoldenPerms for the boolean table,
// deriving every expectation from Permission.Has on the masked per-scope
// value rather than re-deriving true/false by hand for each of the 13 bits.
func applyToGoldenHasPerm(mask rbac.MFAMask) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(goldenPerms))
	for persona, entries := range goldenPerms {
		maskedServer := mask.Apply(entries["GetEffectivePermissions/server"])
		maskedChannel := mask.Apply(entries["GetEffectivePermissions/channel"])
		row := make(map[string]bool, len(goldenHasPermChecks)*2)
		for _, p := range goldenHasPermChecks {
			row["server/"+p.name] = maskedServer.Has(p.perm)
			row["channel/"+p.name] = maskedChannel.Has(p.perm)
		}
		out[persona] = row
	}
	return out
}

// runGoldenConfig runs buildGoldenFixture's seven T0 personas against
// wantPerms/wantHasPerm under the given enforcement setup, sharing
// mfa_mask_golden_test.go's observeAll harness so a failure at any of the
// nine entry points reports by persona and entry-point name, exactly as
// TestMFAMaskGolden_Off does.
func runGoldenConfig(
	t *testing.T, enforcing bool, enrollAll bool,
	wantPerms map[string]map[string]rbac.Permission, wantHasPerm map[string]map[string]bool,
) {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	r := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test"))
	f := buildGoldenFixture(t, ts)

	if enrollAll {
		for _, userID := range f.personas {
			enrollWebAuthn(t, ts, userID)
		}
	}
	if enforcing {
		enforceMFA(t, ts, f.serverID)
	}

	for _, persona := range goldenPersonaOrder {
		persona := persona
		t.Run(persona, func(t *testing.T) {
			userID, ok := f.personas[persona]
			require.True(t, ok, "fixture is missing persona %s", persona)
			obs := observeAll(t, r, ts, f, userID)

			for entryPoint, want := range wantPerms[persona] {
				got, ok := obs.perms[entryPoint]
				require.True(t, ok, "persona %s: observeAll did not record entry point %s", persona, entryPoint)
				assert.Equal(t, want, got,
					"persona %s, entry point %s: got %#x want %#x", persona, entryPoint, int64(got), int64(want))
			}
			for key, want := range wantHasPerm[persona] {
				got, ok := obs.hasPerm[key]
				require.True(t, ok, "persona %s: observeAll did not record HasPermission %s", persona, key)
				assert.Equal(t, want, got, "persona %s, HasPermission %s", persona, key)
			}
		})
	}
}

// TestMFAMaskGoldenConfigs_OnEnrolled is I-1's on+enrolled leg: every value
// at every entry point equals the T0 golden table exactly (goldenPerms'
// owner rows already carry C-2's `golden | PermMentionEveryone` delta as a
// literal, so no further adjustment is made here). Kills: the mask applied
// even when enrolled -- the enrolled check inverted or dropped -- at any one
// of the nine entry points, which would make that entry point disagree with
// the untouched golden value.
func TestMFAMaskGoldenConfigs_OnEnrolled(t *testing.T) {
	runGoldenConfig(t, true, true, goldenPerms, goldenHasPerm)
}

// TestMFAMaskGoldenConfigs_OnUnenrolled is I-1's on+unenrolled leg: every
// value equals rbac.MFAMask{Enforcing: true}.Apply(golden), computed from the
// golden constants rather than hand-copied.
//
// Kills:
//   - the mask missing at any one of the nine exits (that entry point stays
//     raw and disagrees with Apply(golden));
//   - the mask applied BEFORE overrides -- killed by memberAllow. Its channel
//     ALLOW grants PermManageAllMessages (dangerous), which the raw
//     pre-override base does not carry, so a mask that ran before overrides
//     would find nothing to strip there and the ALLOW would survive into the
//     final channel-scope value; Apply(golden) says it must not survive;
//   - the Administrator bypass driven by the MASKED value rather than the
//     raw one -- NOT reachable through buildGoldenFixture's own "adminDeny"
//     persona alone, because its channel override denies only
//     PermManageAllMessages, a bit Apply already strips regardless of
//     whether the override is honoured. TestMFAMaskGoldenConfigs_OnUnenrolled_AdminBypassUsesRawBits
//     below adds the admin+DENY persona this mutant actually needs: a wide
//     channel DENY naming both bit 62 and a NON-dangerous bit
//     (PermSendMessages). If the bypass check read the masked value (bit 62
//     already cleared), the override would apply and PermSendMessages would
//     be missing from a result that should still carry it.
func TestMFAMaskGoldenConfigs_OnUnenrolled(t *testing.T) {
	mask := rbac.MFAMask{Enforcing: true}
	runGoldenConfig(t, true, false, applyToGoldenPerms(mask), applyToGoldenHasPerm(mask))
}

// TestMFAMaskGoldenConfigs_OnUnenrolled_AdminBypassUsesRawBits is the
// admin+DENY persona I-1 calls for by name: an unenrolled Administrator whose
// channel USER-override DENY names bit 62 itself plus a non-dangerous bit
// (PermSendMessages). The raw bypass in applyChannelOverrides/resolver.go
// must ignore that override entirely (raw bit 62 still set pre-mask), so the
// channel-scope result equals concreteMinusDangerous with PermSendMessages
// intact -- masking, not the override, is what removed the dangerous bits.
//
// Kills: the Administrator identity check read from the MASKED value instead
// of the raw one. With that mutant, bit 62 is already cleared by the time the
// bypass is decided, the DENY override is honoured, and PermSendMessages goes
// missing from the channel-scope result even though it is not itself
// dangerous.
func TestMFAMaskGoldenConfigs_OnUnenrolled_AdminBypassUsesRawBits(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	r := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test"))
	f := buildGoldenFixture(t, ts)

	adminDenyWide := ts.CreateTestUser(t, "i1admindenywide")
	ts.AddMemberToServer(t, f.serverID, adminDenyWide.ID, "member")
	wideRole := ts.CreateTestRole(t, f.serverID, "I1AdminWide", 12, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, f.serverID, adminDenyWide.ID, wideRole)
	ts.CreateChannelOverride(t, f.channelID, "user", adminDenyWide.ID, 0,
		int64(rbac.PermSendMessages|rbac.PermAdministrator))

	enforceMFA(t, ts, f.serverID)

	obs := observeAll(t, r, ts, f, adminDenyWide.ID)
	for _, entryPoint := range serverScopedEntryPoints {
		got, ok := obs.perms[entryPoint]
		require.True(t, ok, "entry point %s not observed", entryPoint)
		assert.Equal(t, concreteMinusDangerous, got,
			"server scope %s: got %#x want %#x (concreteMinusDangerous)", entryPoint, int64(got), int64(concreteMinusDangerous))
	}
	for _, entryPoint := range channelScopedEntryPoints {
		got, ok := obs.perms[entryPoint]
		require.True(t, ok, "entry point %s not observed", entryPoint)
		assert.Equal(t, concreteMinusDangerous, got,
			"channel scope %s: got %#x want %#x (concreteMinusDangerous, DENY must be ignored) -- "+
				"if this fails with PermSendMessages missing, the admin bypass is reading masked bits",
			entryPoint, int64(got), int64(concreteMinusDangerous))
	}
	assert.True(t, concreteMinusDangerous.Has(rbac.PermSendMessages),
		"sanity: PermSendMessages must be part of the expected value for this assertion to mean anything")
}
