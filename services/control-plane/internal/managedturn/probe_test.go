package managedturn_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/turn/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedturn"
)

// ---- MintProbeCredential ----------------------------------------------------

func TestMintProbeCredential_UsernameAndPassword(t *testing.T) {
	k, raw := newKey(t)
	now := time.Unix(1_800_000_000, 0)
	ttl := 45 * time.Second

	username, password := managedturn.MintProbeCredential(k, ttl, now)

	wantUser := fmt.Sprintf("%d:%s", now.Add(ttl).Unix(), managedturn.ProbeUserTag)
	assert.Equal(t, wantUser, username, "username is <unix-expiry>:turn-probe with expiry = now+ttl")
	assert.True(t, strings.HasSuffix(username, ":turn-probe"))
	assert.Equal(t, restPassword(raw, wantUser), password, "password is base64(HMAC-SHA1(key, username))")
}

func TestMintProbeCredential_ExpiryTracksNowAndTTL(t *testing.T) {
	k, _ := newKey(t)
	base := time.Unix(1_800_000_000, 0)

	u1, _ := managedturn.MintProbeCredential(k, 10*time.Second, base)
	u2, _ := managedturn.MintProbeCredential(k, 20*time.Second, base)
	u3, _ := managedturn.MintProbeCredential(k, 10*time.Second, base.Add(5*time.Second))

	assert.Equal(t, "1800000010:turn-probe", u1)
	assert.Equal(t, "1800000020:turn-probe", u2)
	assert.Equal(t, "1800000015:turn-probe", u3)
}

func TestMintProbeCredential_DifferentKeysGiveDifferentPasswords(t *testing.T) {
	a, rawA := newKey(t)
	b, rawB := newKey(t)
	now := time.Unix(1_800_000_000, 0)

	uA, pA := managedturn.MintProbeCredential(a, 30*time.Second, now)
	uB, pB := managedturn.MintProbeCredential(b, 30*time.Second, now)

	assert.Equal(t, uA, uB, "the username carries no key-dependent data")
	assert.NotEqual(t, pA, pB)
	for _, s := range []string{uA, pA, pB} {
		assert.NotContains(t, s, rawA)
		assert.NotContains(t, s, rawB)
	}
}

func TestMintProbeCredential_Deterministic(t *testing.T) {
	k, _ := newKey(t)
	now := time.Unix(1_800_000_000, 0)
	u1, p1 := managedturn.MintProbeCredential(k, 30*time.Second, now)
	u2, p2 := managedturn.MintProbeCredential(k, 30*time.Second, now)
	assert.Equal(t, u1, u2)
	assert.Equal(t, p1, p2)
}

// ---- fake server self-check -------------------------------------------------

// directAllocate performs one authenticated Allocate against the fake with raw
// pion, independent of TURNProber. It exists so "wrong key" cases below are
// known to fail because the SERVER rejected the credential, not because the
// fake is broken.
func directAllocate(t testing.TB, f *fakeTURN, transport, username, password string) error {
	t.Helper()
	var (
		addr string
		conn net.PacketConn
	)
	switch transport {
	case "udp":
		addr = f.UDPAddr
		c, err := net.ListenPacket("udp4", "127.0.0.1:0")
		require.NoError(t, err)
		conn = c
	case "tcp":
		addr = f.TCPAddr
		c, err := net.Dial("tcp", addr)
		require.NoError(t, err)
		conn = turn.NewSTUNConn(c)
	case "tls":
		addr = f.TLSAddr
		c, err := tls.Dial("tcp", addr, f.ClientTLS)
		require.NoError(t, err)
		conn = turn.NewSTUNConn(c)
	default:
		t.Fatalf("unknown transport %q", transport)
	}
	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: addr,
		TURNServerAddr: addr,
		Conn:           conn,
		Username:       username,
		Password:       password,
		Realm:          f.Realm,
	})
	require.NoError(t, err)
	defer client.Close()
	require.NoError(t, client.Listen())
	relay, err := client.Allocate()
	if err != nil {
		return err
	}
	return relay.Close()
}

func TestFakeTURN_AcceptsRightCredentialAndRejectsWrongOne(t *testing.T) {
	key := newKeyString(t)
	wrong := newKeyString(t)
	f := startFakeTURN(t, fakeTURNOpts{key: key})
	user := strconv.FormatInt(time.Now().Add(30*time.Second).Unix(), 10) + ":turn-probe"

	for _, transport := range []string{"udp", "tcp", "tls"} {
		t.Run(transport, func(t *testing.T) {
			require.NoError(t, directAllocate(t, f, transport, user, restPassword(key, user)))
			require.Error(t, directAllocate(t, f, transport, user, restPassword(wrong, user)))
			assert.Equal(t, 1, f.createdCount(transport), "only the correct credential allocates")
		})
	}
}

// ---- TURNProber -------------------------------------------------------------

func proberFor(f *fakeTURN) *managedturn.TURNProber {
	return &managedturn.TURNProber{
		Realm:      f.Realm,
		UDPAddr:    f.UDPAddr,
		TCPAddr:    f.TCPAddr,
		TLSAddr:    f.TLSAddr,
		TLSConfig:  f.ClientTLS.Clone(),
		TTL:        30 * time.Second,
		Now:        time.Now,
		MaxRetries: 0,
	}
}

func withTimeout(t testing.TB, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestTURNProber_SucceedsWithRightKeyOnAllThreeTransports(t *testing.T) {
	k, raw := newKey(t)
	f := startFakeTURN(t, fakeTURNOpts{key: raw})

	err := proberFor(f).Probe(withTimeout(t, 15*time.Second), k)
	require.NoError(t, err)

	for _, transport := range []string{"udp", "tcp", "tls"} {
		assert.GreaterOrEqual(t, f.createdCount(transport), 1, "a fresh authenticated allocation must succeed over %s", transport)
	}
	for _, u := range f.seenUsernames() {
		assert.True(t, strings.HasSuffix(u, ":"+managedturn.ProbeUserTag), "unexpected probe username %q", u)
	}
}

func TestTURNProber_ReleasesAllocationsAfterProving(t *testing.T) {
	k, raw := newKey(t)
	f := startFakeTURN(t, fakeTURNOpts{key: raw})
	require.NoError(t, proberFor(f).Probe(withTimeout(t, 15*time.Second), k))

	for _, transport := range []string{"udp", "tcp", "tls"} {
		created := f.createdCount(transport)
		require.GreaterOrEqual(t, created, 1)
		assert.Eventually(t, func() bool { return f.deletedCount(transport) >= created },
			5*time.Second, 25*time.Millisecond, "%s allocation must be released (Refresh lifetime 0), not left to expire", transport)
	}
}

func TestTURNProber_EachProbeMakesFreshAllocations(t *testing.T) {
	k, raw := newKey(t)
	f := startFakeTURN(t, fakeTURNOpts{key: raw})
	p := proberFor(f)

	require.NoError(t, p.Probe(withTimeout(t, 15*time.Second), k))
	first := map[string]int{}
	for _, transport := range []string{"udp", "tcp", "tls"} {
		first[transport] = f.createdCount(transport)
	}
	require.NoError(t, p.Probe(withTimeout(t, 15*time.Second), k))

	for _, transport := range []string{"udp", "tcp", "tls"} {
		assert.Greater(t, f.createdCount(transport), first[transport], "second probe over %s must not reuse a cached allocation", transport)
	}
}

func TestTURNProber_MintsCredentialWithinTTL(t *testing.T) {
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		max  time.Duration
	}{
		{"short ttl is honoured", 20 * time.Second, 20 * time.Second},
		{"ttl above the cap is clamped", 10 * time.Minute, managedturn.MaxProbeTTL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, raw := newKey(t)
			f := startFakeTURN(t, fakeTURNOpts{key: raw})
			p := proberFor(f)
			p.TTL = tc.ttl

			before := time.Now().Unix()
			require.NoError(t, p.Probe(withTimeout(t, 15*time.Second), k))
			after := time.Now().Unix()

			names := f.seenUsernames()
			require.NotEmpty(t, names)
			for _, u := range names {
				expiryText, tag, ok := strings.Cut(u, ":")
				require.True(t, ok, "username %q", u)
				assert.Equal(t, managedturn.ProbeUserTag, tag)
				expiry, err := strconv.ParseInt(expiryText, 10, 64)
				require.NoError(t, err)
				assert.GreaterOrEqual(t, expiry, before, "credential must not be born expired")
				assert.LessOrEqual(t, expiry, after+int64(tc.max/time.Second)+1, "credential lifetime must not exceed %v", tc.max)
			}
		})
	}
}

func TestTURNProber_MintsAFreshCredentialPerAttempt(t *testing.T) {
	k, raw := newKey(t)
	f := startFakeTURN(t, fakeTURNOpts{key: raw})
	p := proberFor(f)
	p.MaxRetries = 1
	calls := 0
	p.Now = func() time.Time {
		calls++
		if calls == 1 {
			// The first mint is born expired; only a re-mint can pass.
			return time.Now().Add(-p.TTL - 5*time.Second)
		}
		return time.Now()
	}

	require.NoError(t, p.Probe(withTimeout(t, 30*time.Second), k),
		"a retry must not reuse a credential that has expired")

	seen := map[string]bool{}
	for _, u := range f.seenUsernames() {
		seen[u] = true
	}
	assert.GreaterOrEqual(t, len(seen), 2, "each attempt must mint its own credential")
}

func TestTURNProber_WrongKeyFailsWithErrProbeFailed(t *testing.T) {
	serverKey := newKeyString(t)
	wrong, _ := newKey(t)
	f := startFakeTURN(t, fakeTURNOpts{key: serverKey})

	err := proberFor(f).Probe(withTimeout(t, 15*time.Second), wrong)

	requireRejected(t, err, managedturn.ErrProbeFailed)
	assert.Zero(t, f.totalCreated(), "no allocation may exist for a key the server does not hold")
}

func TestTURNProber_ErrorDoesNotLeakKey(t *testing.T) {
	serverKey := newKeyString(t)
	wrong, wrongRaw := newKey(t)
	f := startFakeTURN(t, fakeTURNOpts{key: serverKey})

	err := proberFor(f).Probe(withTimeout(t, 15*time.Second), wrong)

	requireRejected(t, err, managedturn.ErrProbeFailed)
	assert.NotContains(t, err.Error(), wrongRaw)
	assert.NotContains(t, err.Error(), serverKey)
}

func TestTURNProber_AnySingleTransportDownFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts func(key string) fakeTURNOpts
		mut  func(p *managedturn.TURNProber)
	}{
		{"udp closed", func(k string) fakeTURNOpts { return fakeTURNOpts{key: k, skipUDP: true} }, nil},
		{"tcp closed", func(k string) fakeTURNOpts { return fakeTURNOpts{key: k, skipTCP: true} }, nil},
		{"tls closed", func(k string) fakeTURNOpts { return fakeTURNOpts{key: k, skipTLS: true} }, nil},
		{"tls certificate not trusted", func(k string) fakeTURNOpts { return fakeTURNOpts{key: k} },
			func(p *managedturn.TURNProber) { p.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, raw := newKey(t)
			f := startFakeTURN(t, tc.opts(raw))
			p := proberFor(f)
			if tc.mut != nil {
				tc.mut(p)
			}

			err := p.Probe(withTimeout(t, 4*time.Second), k)

			requireRejected(t, err, managedturn.ErrProbeFailed)
		})
	}
}

func TestTURNProber_RespectsContextDeadline(t *testing.T) {
	k, raw := newKey(t)
	f := startFakeTURN(t, fakeTURNOpts{key: raw, blackholeUDP: true})
	p := proberFor(f)
	p.MaxRetries = 5 // would run far past the deadline if retries ignored it

	ctx := withTimeout(t, 600*time.Millisecond)
	start := time.Now()
	err := p.Probe(ctx, k)
	elapsed := time.Since(start)

	requireRejected(t, err, managedturn.ErrProbeFailed, context.DeadlineExceeded)
	assert.Less(t, elapsed, 5*time.Second, "Probe must return near the context deadline, not after the retry budget")
}

func TestTURNProber_CancelledContextMakesNoAllocation(t *testing.T) {
	k, raw := newKey(t)
	f := startFakeTURN(t, fakeTURNOpts{key: raw})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := proberFor(f).Probe(ctx, k)

	requireRejected(t, err, managedturn.ErrProbeFailed, context.Canceled)
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.Zero(t, f.totalCreated(), "a cancelled probe must not prove anything")
}
