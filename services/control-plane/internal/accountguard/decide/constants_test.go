package decide

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConstants_PinnedValues pins every spec §8 value as a literal, so a changed constant
// fails here and not silently downstream (plan Task 2, D23). Durations and integers are
// both compared as int64 (durations in nanoseconds). Each row cites its §8 line.
func TestConstants_PinnedValues(t *testing.T) {
	cases := []struct {
		name string
		got  int64
		want int64
	}{
		// §8: DeltaCP = maturity = the SQL stamp interval, 72 h (parent §6.4).
		{"DeltaCP", int64(DeltaCP), int64(72 * time.Hour)},
		// §8: NCancelCP and dispute expiry, 30 d.
		{"NCancelCP", int64(NCancelCP), int64(30 * 24 * time.Hour)},
		{"DisputeExpiry", int64(DisputeExpiry), int64(30 * 24 * time.Hour)},
		// §8: HoldCap = outbox terminal age, 72 h.
		{"HoldCap", int64(HoldCap), int64(72 * time.Hour)},
		// §8: lock timeout 3 s.
		{"LockTimeout", int64(LockTimeout), int64(3 * time.Second)},
		// §8: guard and ceremony token 5 min.
		{"GuardTokenTTL", int64(GuardTokenTTL), int64(5 * time.Minute)},
		{"CeremonyTokenTTL", int64(CeremonyTokenTTL), int64(5 * time.Minute)},
		// §8: challenge skew +-60 s; used-challenge retention issue time + 7 min.
		{"ChallengeSkew", int64(ChallengeSkew), int64(60 * time.Second)},
		{"UsedChallengeRetention", int64(UsedChallengeRetention), int64(7 * time.Minute)},
		// §8: body cap 4096 B.
		{"BodyCapBytes", int64(BodyCapBytes), 4096},
		// §8: cancel budget, 10 failures / 15 min.
		{"CancelBudgetFailures", int64(CancelBudgetFailures), 10},
		{"CancelBudgetWindow", int64(CancelBudgetWindow), int64(15 * time.Minute)},
		// §8: knowledge-leg budget, 5 failures / 24 h, then a 24 h refusal.
		{"KnowledgeLegFailures", int64(KnowledgeLegFailures), 5},
		{"KnowledgeLegWindow", int64(KnowledgeLegWindow), int64(24 * time.Hour)},
		{"KnowledgeLegRefusal", int64(KnowledgeLegRefusal), int64(24 * time.Hour)},
		// §8 / LEAD-C 5a: 5 sends per digest, 2 per (digest, address), per 24 h; a code lives 10 min.
		{"AddressCodeSendsPerDigest", int64(AddressCodeSendsPerDigest), 5},
		{"AddressCodeSendsPerAddress", int64(AddressCodeSendsPerAddress), 2},
		{"AddressCodeWindow", int64(AddressCodeWindow), int64(24 * time.Hour)},
		{"AddressCodeTTL", int64(AddressCodeTTL), int64(10 * time.Minute)},
		// §8: history <= 32 passwords per account; 30 d.
		{"HistoryMaxPasswords", int64(HistoryMaxPasswords), 32},
		{"HistoryRetention", int64(HistoryRetention), int64(30 * 24 * time.Hour)},
		// §8: per-IP buckets, burst 10, 1 refill per 6 s, 50 000 keys, IPv6 grouped by /64.
		{"IPBucketBurst", int64(IPBucketBurst), 10},
		{"IPBucketRefill", int64(IPBucketRefill), int64(6 * time.Second)},
		{"IPBucketMaxKeys", int64(IPBucketMaxKeys), 50000},
		{"IPv6GroupPrefixBits", int64(IPv6GroupPrefixBits), 64},
		// §8: device credentials, 32 random bytes, <= 10 per account.
		{"DeviceCredentialBytes", int64(DeviceCredentialBytes), 32},
		{"DeviceCredentialsPerAcct", int64(DeviceCredentialsPerAcct), 10},
		// §8: sweeper 60 s; outbox 1 s, 50 rows; jobs 1 s.
		{"SweeperInterval", int64(SweeperInterval), int64(60 * time.Second)},
		{"OutboxInterval", int64(OutboxInterval), int64(1 * time.Second)},
		{"OutboxBatch", int64(OutboxBatch), 50},
		{"JobsInterval", int64(JobsInterval), int64(1 * time.Second)},
		// §8: outbox claim lease 2 min; backoff 5 s doubling to 15 min.
		{"OutboxClaimLease", int64(OutboxClaimLease), int64(2 * time.Minute)},
		{"OutboxBackoffStart", int64(OutboxBackoffStart), int64(5 * time.Second)},
		{"OutboxBackoffCap", int64(OutboxBackoffCap), int64(15 * time.Minute)},
		// §8: job expiry 5 min; done-job retention 24 h.
		{"JobExpiry", int64(JobExpiry), int64(5 * time.Minute)},
		{"DoneJobRetention", int64(DoneJobRetention), int64(24 * time.Hour)},
		// §8: overdue thresholds, notice 15 min, job 60 s.
		{"OverdueNoticeAge", int64(OverdueNoticeAge), int64(15 * time.Minute)},
		{"OverdueJobAge", int64(OverdueJobAge), int64(60 * time.Second)},
		// §8: notice retention 30 d.
		{"NoticeRetention", int64(NoticeRetention), int64(30 * 24 * time.Hour)},
	}
	require.Len(t, cases, 39, "every Task 2 constant has a row")
	for _, c := range cases {
		assert.Equal(t, c.want, c.got, c.name)
	}
}

// TestConstants_UsedChallengeRetentionCoversSkewJobAndMargin pins the derivation of the
// 7 min retention: skew + job expiry + 1 min (spec §8; §2.2 S8).
func TestConstants_UsedChallengeRetentionCoversSkewJobAndMargin(t *testing.T) {
	require.Equal(t, ChallengeSkew+JobExpiry+time.Minute, UsedChallengeRetention)
}

// TestConstants_DisputeExpiryIsNCancelCP: spec §8 gives one row, "NCancelCP, dispute expiry, 30 d".
func TestConstants_DisputeExpiryIsNCancelCP(t *testing.T) {
	require.Equal(t, NCancelCP, DisputeExpiry)
}

// TestConstants_Sentinels pins the two maturity sentinels (spec §8: legacy 1970-01-01Z,
// awaiting notice 2999-01-01Z) and that both are UTC.
func TestConstants_Sentinels(t *testing.T) {
	legacy := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	awaiting := time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)

	assert.True(t, LegacyMaturity().Equal(legacy), "LegacyMaturity = %v", LegacyMaturity())
	assert.True(t, AwaitingNoticeMaturity().Equal(awaiting), "AwaitingNoticeMaturity = %v", AwaitingNoticeMaturity())
	assert.Same(t, time.UTC, LegacyMaturity().Location())
	assert.Same(t, time.UTC, AwaitingNoticeMaturity().Location())
	assert.True(t, LegacyMaturity().Before(AwaitingNoticeMaturity()))
}
