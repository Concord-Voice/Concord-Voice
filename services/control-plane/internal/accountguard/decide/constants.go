package decide

import "time"

// The account guard's constants (child spec §8; parent §6.4). This file is their only Go
// home: every later PR imports them from here and never redeclares them (plan D23). None is
// read from the environment (parent §12 K12). constants_test.go pins each value as a literal.

// Windows that the T table reads.
const (
	// DeltaCP is Δ_cp: the request delay, the maturity interval, and the interval the SQL
	// maturity stamp adds (parent §6.4).
	DeltaCP = 72 * time.Hour
	// NCancelCP is how long a proof or dispute_resolved cancel refuses or disputes a
	// matching reopen (T9, T10; parent §6.4).
	NCancelCP = 30 * 24 * time.Hour
	// DisputeExpiry is how long a disputed request waits for T12 before it is cancelled
	// with no change. It equals NCancelCP (parent §6.4).
	DisputeExpiry = NCancelCP
	// HoldCap bounds the total held time of a request; at the cap the request is cancelled
	// with no change (parent §6.2 Holds). It is also the outbox's terminal age.
	HoldCap = 72 * time.Hour
)

// Runner, token and challenge limits (child spec §8).
const (
	// LockTimeout is the runner's lock timeout (parent §7.3).
	LockTimeout = 3 * time.Second
	// GuardTokenTTL is the lifetime of a guard token (parent §6.4).
	GuardTokenTTL = 5 * time.Minute
	// CeremonyTokenTTL is the lifetime of a ceremony token (parent §6.4).
	CeremonyTokenTTL = 5 * time.Minute
	// ChallengeSkew is the allowed distance between a WebAuthn challenge's issue time and
	// the job's submitted_at (child spec §5.2).
	ChallengeSkew = 60 * time.Second
	// JobExpiry is how long a job may wait before it expires with no effect (child spec §5.2).
	JobExpiry = 5 * time.Minute
	// UsedChallengeRetention is how long after its issue time a spent challenge is kept:
	// the skew, plus the job expiry, plus one minute of margin (child spec §2.2 S8).
	UsedChallengeRetention = ChallengeSkew + JobExpiry + time.Minute
	// BodyCapBytes caps a guard request body (parent §7.2).
	BodyCapBytes = 4096
)

// Budgets (child spec §8; parent §6.4 and §6.11).
const (
	// CancelBudgetFailures is the number of failed guessable proofs a cancel budget admits
	// per CancelBudgetWindow.
	CancelBudgetFailures = 10
	// CancelBudgetWindow is the cancel budget's window.
	CancelBudgetWindow = 15 * time.Minute
	// KnowledgeLegFailures is the number of failed knowledge-leg proofs admitted per
	// KnowledgeLegWindow (a C6 consumer).
	KnowledgeLegFailures = 5
	// KnowledgeLegWindow is the knowledge-leg budget's window.
	KnowledgeLegWindow = 24 * time.Hour
	// KnowledgeLegRefusal is how long the knowledge leg is refused once its budget is spent.
	KnowledgeLegRefusal = 24 * time.Hour
	// AddressCodeSendsPerDigest is the number of address codes sent per identifier digest
	// per AddressCodeWindow (LEAD-C 5a).
	AddressCodeSendsPerDigest = 5
	// AddressCodeSendsPerAddress is the number of address codes sent per (digest, address)
	// per AddressCodeWindow (LEAD-C 5a).
	AddressCodeSendsPerAddress = 2
	// AddressCodeWindow is the address-code budget's window.
	AddressCodeWindow = 24 * time.Hour
	// AddressCodeTTL is the lifetime of a single-use address code.
	AddressCodeTTL = 10 * time.Minute
	// HistoryMaxPasswords is the number of retired passwords kept per account.
	HistoryMaxPasswords = 32
	// HistoryRetention is how long a credential-history row is kept.
	HistoryRetention = 30 * 24 * time.Hour
	// IPBucketBurst is a per-IP bucket's burst size.
	IPBucketBurst = 10
	// IPBucketRefill is the interval at which a per-IP bucket regains one token.
	IPBucketRefill = 6 * time.Second
	// IPBucketMaxKeys bounds the number of per-IP buckets held.
	IPBucketMaxKeys = 50000
	// IPv6GroupPrefixBits is the IPv6 prefix length that shares one bucket.
	IPv6GroupPrefixBits = 64
	// DeviceCredentialBytes is the length of a device credential's random secret (input 12).
	DeviceCredentialBytes = 32
	// DeviceCredentialsPerAcct caps the device credentials per account; the oldest is evicted.
	DeviceCredentialsPerAcct = 10
)

// Worker cadences and retention (child spec §5.2 and §8; LEAD-C, LEAD-E).
const (
	// SweeperInterval is the sweeper's period.
	SweeperInterval = 60 * time.Second
	// OutboxInterval is the outbox worker's period.
	OutboxInterval = 1 * time.Second
	// OutboxBatch is the number of notices the outbox claims per pass.
	OutboxBatch = 50
	// JobsInterval is the jobs worker's period.
	JobsInterval = 1 * time.Second
	// OutboxClaimLease is how long a claimed notice is leased before it is retried.
	OutboxClaimLease = 2 * time.Minute
	// OutboxBackoffStart is the first retry backoff of a notice; it doubles per attempt.
	OutboxBackoffStart = 5 * time.Second
	// OutboxBackoffCap caps the notice retry backoff.
	OutboxBackoffCap = 15 * time.Minute
	// DoneJobRetention is how long a done job is kept.
	DoneJobRetention = 24 * time.Hour
	// OverdueNoticeAge is the age at which an undelivered notice counts as overdue (LEAD-E).
	OverdueNoticeAge = 15 * time.Minute
	// OverdueJobAge is the age at which an unfinished job counts as overdue (LEAD-E).
	OverdueJobAge = 60 * time.Second
	// NoticeRetention is how long a notice is kept after its request ends, or after its
	// delivery when it has no request (input 14).
	NoticeRetention = 30 * 24 * time.Hour
)

// LegacyMaturity is the maturity of every credential that exists when C4 ships:
// 1970-01-01T00:00:00Z (child spec §8; parent §7.1). It is a function because the package
// holds no variables.
func LegacyMaturity() time.Time { return time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC) }

// AwaitingNoticeMaturity is the finite sentinel a credential's maturity holds until its
// enrolment notice is accepted at a qualifying destination: 2999-01-01T00:00:00Z (child
// spec §4 input 4).
func AwaitingNoticeMaturity() time.Time { return time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC) }
