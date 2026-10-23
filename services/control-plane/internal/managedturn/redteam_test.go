package managedturn_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pion/stun/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedturn"
)

// Regression tests for the #3931 Task 2 red-team findings. Each fails against
// the pre-fix code.

// ---- 1. orphan key wedge ----------------------------------------------------

func TestRedTeam_BootstrapProbeFailureLeavesNoOrphanRow(t *testing.T) {
	f := newFixture(t)
	a, _ := newKey(t)
	b, rawB := newKey(t)
	f.prober.setErr(managedturn.ErrProbeFailed) // e.g. TLS 5349 misconfigured on first activation

	requireRejected(t, f.c.Bootstrap(ctx5(t), a), managedturn.ErrProbeFailed)
	assert.Empty(t, f.rows(), "coturn must not keep accepting a key nothing recorded or proved")

	f.prober.setErr(nil)
	require.NoError(t, f.c.Bootstrap(ctx5(t), b), "a fresh key is not wedged by the failed attempt")
	active, ok := f.activeBytes()
	require.True(t, ok)
	assert.Equal(t, []byte(rawB), active)
	assert.Equal(t, []string{b.ID()}, f.rows())
}

func TestRedTeam_BootstrapProbeFailureCleansUpEvenWhenContextExpired(t *testing.T) {
	f := newFixture(t)
	a, _ := newKey(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.prober.hook = func(managedturn.Key) { cancel() } // the probe's own deadline ran out
	f.prober.setErr(managedturn.ErrProbeFailed)

	requireRejected(t, f.c.Bootstrap(ctx, a), managedturn.ErrProbeFailed)
	assert.Empty(t, f.rows(), "cleanup runs on a detached context")
}

func TestRedTeam_BootstrapReplayOfPublishedKeyKeepsRowOnProbeFailure(t *testing.T) {
	f := newFixture(t)
	a, rawA := newKey(t)
	// Interrupted bootstrap: inserted and published, crashed before the state.
	require.NoError(t, f.store.Insert(ctx5(t), testRealm, a))
	require.NoError(t, managedturn.WriteActiveFile(f.activeDir, os.Getgid(), a))
	f.prober.setErr(managedturn.ErrProbeFailed)

	requireRejected(t, f.c.Bootstrap(ctx5(t), a), managedturn.ErrProbeFailed)
	assert.Equal(t, []string{a.ID()}, f.rows(), "a published key is never deleted")
	active, _ := f.activeBytes()
	assert.Equal(t, []byte(rawA), active)

	other, _ := newKey(t)
	requireRejected(t, f.c.Bootstrap(ctx5(t), other), managedturn.ErrActiveMismatch)

	f.prober.setErr(nil)
	require.NoError(t, f.c.Bootstrap(ctx5(t), a), "replaying the published key completes the bootstrap")
	assert.Equal(t, uint64(1), f.state().Generation)
}

func TestRedTeam_DiscardUnrecordedClearsCrashedAddCandidate(t *testing.T) {
	f := newFixture(t)
	a, rawA := newKey(t)
	b, _ := newKey(t)
	c, _ := newKey(t)
	f.bootstrap(a)
	// Simulated crash: AddCandidate(b) inserted, then died before the state;
	// the operator has since lost b.
	require.NoError(t, f.store.Insert(ctx5(t), testRealm, b))

	for name, err := range map[string]error{
		"add-candidate c": f.c.AddCandidate(ctx5(t), c),
		"recover {a}":     f.c.Recover(ctx5(t), []managedturn.Key{a}),
	} {
		requireRejected(t, err, managedturn.ErrStoreMismatch)
		_ = name
	}
	stateBefore, _ := f.stateBytes()

	gen := uint64(0)
	f.c.ExpectGeneration = &gen
	_, err := f.c.DiscardUnrecorded(ctx5(t))
	requireRejected(t, err, managedturn.ErrStaleGeneration)
	assert.Equal(t, sortedIDs(a, b), f.rows(), "a stale guard discards nothing")

	gen = 1
	n, err := f.c.DiscardUnrecorded(ctx5(t))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{a.ID()}, f.rows(), "only the unrecorded row is removed")
	stateAfter, _ := f.stateBytes()
	assert.Equal(t, stateBefore, stateAfter, "state and generation untouched")
	active, _ := f.activeBytes()
	assert.Equal(t, []byte(rawA), active, "the published key stays")
	assert.Empty(t, f.prober.callIDs(), "discard never probes")

	require.NoError(t, f.c.AddCandidate(ctx5(t), c), "the wedge is gone")
	n, err = f.c.DiscardUnrecorded(ctx5(t))
	require.NoError(t, err)
	assert.Zero(t, n, "recorded keys are never discarded")
	assert.Equal(t, sortedIDs(a, c), f.rows())
}

func TestRedTeam_DiscardUnrecordedWithoutState(t *testing.T) {
	t.Run("no active file: every row goes", func(t *testing.T) {
		f := newFixture(t)
		a, _ := newKey(t)
		b, _ := newKey(t)
		require.NoError(t, f.store.Insert(ctx5(t), testRealm, a))
		require.NoError(t, f.store.Insert(ctx5(t), testRealm, b))
		n, err := f.c.DiscardUnrecorded(ctx5(t))
		require.NoError(t, err)
		assert.Equal(t, 2, n)
		assert.Empty(t, f.rows())
		_, has := f.stateBytes()
		assert.False(t, has, "discard never creates state")
	})
	t.Run("published file: refused", func(t *testing.T) {
		f := newFixture(t)
		a, _ := newKey(t)
		require.NoError(t, f.store.Insert(ctx5(t), testRealm, a))
		require.NoError(t, managedturn.WriteActiveFile(f.activeDir, os.Getgid(), a))
		_, err := f.c.DiscardUnrecorded(ctx5(t))
		requireRejected(t, err, managedturn.ErrActiveMismatch)
		assert.Equal(t, []string{a.ID()}, f.rows(), "a published key is never deleted")
	})
	t.Run("invalid stored value goes too", func(t *testing.T) {
		f := newFixture(t)
		a, _ := newKey(t)
		f.bootstrap(a)
		raw := openRaw(t, f.dbPath, "rw")
		_, err := raw.Exec(`INSERT INTO turn_secret (realm, value) VALUES (?, ?)`, testRealm, "planted")
		require.NoError(t, err)
		n, err := f.c.DiscardUnrecorded(ctx5(t))
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		assert.Equal(t, []string{a.ID()}, f.rows())
	})
}

// ---- 2. status after an interrupted promote ---------------------------------

func TestRedTeam_InterruptedPromoteIsReportedAndOnlyReplayable(t *testing.T) {
	f := newFixture(t)
	a, _ := newKey(t)
	b, rawB := newKey(t)
	c, _ := newKey(t)
	f.bootstrapWithCandidate(a, b)
	// Simulated crash: promote(b) renamed the active file, died before the state.
	require.NoError(t, managedturn.WriteActiveFile(f.activeDir, os.Getgid(), b))
	ino := activeInode(t, f)

	st, err := f.c.Status(ctx5(t))
	require.NoError(t, err)
	assert.Equal(t, a.ID(), st.State.ActiveKeyID)
	assert.Equal(t, b.ID(), st.ActiveFileKeyID, "status reports what the issuer actually serves")
	asJSON, err := json.Marshal(st)
	require.NoError(t, err)
	assert.NotContains(t, string(asJSON), rawB)

	before := f.snapshot()
	for name, err := range map[string]error{
		"rollback a":      f.c.Rollback(ctx5(t), a),
		"promote a":       f.c.Promote(ctx5(t), a),
		"add-candidate c": f.c.AddCandidate(ctx5(t), c),
		"recover":         f.c.Recover(ctx5(t), []managedturn.Key{a, b}),
	} {
		requireRejected(t, err, managedturn.ErrActiveMismatch)
		_ = name
	}
	_, err = f.c.DiscardUnrecorded(ctx5(t))
	requireRejected(t, err, managedturn.ErrActiveMismatch)
	assert.Empty(t, f.prober.callIDs())
	f.requireUnchanged(before, "refusals change nothing")

	// Replay: the file's key is proven afresh and the state commit completes
	// without re-publishing. Rollback works too, despite the candidate phase.
	require.NoError(t, f.c.Rollback(ctx5(t), b))
	assert.Equal(t, []string{b.ID()}, f.prober.callIDs(), "the replay re-proves the key")
	assert.Equal(t, ino, activeInode(t, f), "the active file is not rewritten")
	got := f.state()
	assert.Equal(t, b.ID(), got.ActiveKeyID)
	assert.Equal(t, uint64(2), got.Generation)
	assert.Equal(t, managedturn.PhaseActive, got.Phase)
}

func TestRedTeam_InterruptedPromoteReplayProbeFailureCommitsNothing(t *testing.T) {
	f := newFixture(t)
	a, _ := newKey(t)
	b, _ := newKey(t)
	f.bootstrapWithCandidate(a, b)
	require.NoError(t, managedturn.WriteActiveFile(f.activeDir, os.Getgid(), b))
	before := f.snapshot()
	f.prober.setErr(managedturn.ErrProbeFailed)

	requireRejected(t, f.c.Promote(ctx5(t), b), managedturn.ErrProbeFailed)
	f.requireUnchanged(before, "a failed replay proof commits nothing")
}

func TestRedTeam_ActiveFileThatIsNotAKeyIsRefused(t *testing.T) {
	f := newFixture(t)
	a, _ := newKey(t)
	b, _ := newKey(t)
	f.bootstrapWithCandidate(a, b)
	path := filepath.Join(f.activeDir, activeFileName)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, []byte("garbage"), 0o440)) // #nosec G306 -- fixture mirrors the published file's 0440 mode

	requireRejected(t, f.c.Promote(ctx5(t), b), managedturn.ErrActiveMismatch)
	_, err := f.c.Status(ctx5(t))
	requireRejected(t, err, managedturn.ErrActiveMismatch)
	assert.Empty(t, f.prober.callIDs())

	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Symlink(filepath.Join(f.root, "elsewhere"), path))
	_, err = f.c.Status(ctx5(t))
	requireRejected(t, err, managedturn.ErrUnsafePermission)
}

func activeInode(t testing.TB, f *fixture) uint64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(f.activeDir, activeFileName))
	require.NoError(t, err)
	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	return uint64(st.Ino) //nolint:unconvert // field width differs per platform
}

// ---- 3. keyless responders --------------------------------------------------

// keylessReply answers like a TURN server that holds no key: it challenges an
// unauthenticated Allocate, then "accepts" any authenticated request without
// MESSAGE-INTEGRITY.
func keylessReply(raw []byte) []byte {
	m := &stun.Message{Raw: append([]byte(nil), raw...)}
	if m.Decode() != nil {
		return nil
	}
	tid := stun.NewTransactionIDSetter(m.TransactionID)
	var out *stun.Message
	switch {
	case m.Type.Method == stun.MethodAllocate && !m.Contains(stun.AttrMessageIntegrity):
		out, _ = stun.Build(tid, stun.NewType(stun.MethodAllocate, stun.ClassErrorResponse),
			stun.CodeUnauthorized, stun.NewRealm(testRealm), stun.NewNonce("n0nce"), stun.Fingerprint)
	case m.Type.Method == stun.MethodAllocate:
		relayed := relayedAttr{}
		out, _ = stun.Build(tid, stun.NewType(stun.MethodAllocate, stun.ClassSuccessResponse),
			relayed, stun.RawAttribute{Type: stun.AttrLifetime, Value: []byte{0, 0, 0, 30}}, stun.Fingerprint)
	default:
		out, _ = stun.Build(tid, stun.NewType(m.Type.Method, stun.ClassSuccessResponse), stun.Fingerprint)
	}
	return out.Raw
}

type relayedAttr struct{}

func (relayedAttr) AddTo(m *stun.Message) error {
	return stun.XORMappedAddress{IP: net.IPv4(127, 0, 0, 1), Port: 5000}.AddToAs(m, stun.AttrXORRelayedAddress)
}

func keylessUDP(t testing.TB) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if out := keylessReply(buf[:n]); out != nil {
				_, _ = pc.WriteTo(out, from)
			}
		}
	}()
	return pc.LocalAddr().String()
}

func keylessTCP(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				for {
					var hdr [20]byte
					if _, err := io.ReadFull(c, hdr[:]); err != nil {
						return
					}
					msg := make([]byte, 20+int(binary.BigEndian.Uint16(hdr[2:4])))
					copy(msg, hdr[:])
					if _, err := io.ReadFull(c, msg[20:]); err != nil {
						return
					}
					if out := keylessReply(msg); out != nil {
						_, _ = c.Write(out)
					}
				}
			}(c)
		}
	}()
	return l.Addr().String()
}

// A proof that fails after the authenticated Allocate still sends an
// authenticated lifetime-zero Refresh, so failed attempts strand no relays.
func TestRedTeam_FailedUDPProofStillReleasesTheAllocation(t *testing.T) {
	k, raw := newKey(t)
	f := startFakeTURN(t, fakeTURNOpts{key: raw})
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	var releases atomic.Int32
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			m := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
			if m.Decode() == nil && m.Type.Method == stun.MethodRefresh && m.Contains(stun.AttrMessageIntegrity) {
				releases.Add(1)
			}
			if out := keylessReply(buf[:n]); out != nil {
				_, _ = pc.WriteTo(out, from)
			}
		}
	}()
	p := proberFor(f)
	p.UDPAddr = pc.LocalAddr().String()

	requireRejected(t, p.Probe(withTimeout(t, 15*time.Second), k), managedturn.ErrProbeFailed)
	assert.GreaterOrEqual(t, releases.Load(), int32(1), "the failed proof must still release what it allocated")
}

func TestRedTeam_UDPProofRefusesKeylessResponder(t *testing.T) {
	k, raw := newKey(t)
	f := startFakeTURN(t, fakeTURNOpts{key: raw}) // TCP and TLS really hold the key
	p := proberFor(f)
	p.UDPAddr = keylessUDP(t)

	err := p.Probe(withTimeout(t, 10*time.Second), k)
	requireRejected(t, err, managedturn.ErrProbeFailed)
	assert.Contains(t, err.Error(), "udp")
}

func TestRedTeam_TCPProofRefusesKeylessResponder(t *testing.T) {
	k, raw := newKey(t)
	f := startFakeTURN(t, fakeTURNOpts{key: raw})
	p := proberFor(f)
	p.TCPAddr = keylessTCP(t)

	err := p.Probe(withTimeout(t, 10*time.Second), k)
	requireRejected(t, err, managedturn.ErrProbeFailed)
	assert.Contains(t, err.Error(), "tcp")
}

// wrongSourceUDP relays requests to a real server but sends every response
// back from a different socket: valid integrity, wrong source.
func wrongSourceUDP(t testing.TB, upstream string) string {
	t.Helper()
	in, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	out, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	alt, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = in.Close(); _ = out.Close(); _ = alt.Close() })
	up, err := net.ResolveUDPAddr("udp4", upstream)
	require.NoError(t, err)
	clients := make(chan net.Addr, 16)
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := in.ReadFrom(buf)
			if err != nil {
				return
			}
			select {
			case clients <- from:
			default:
			}
			_, _ = out.WriteTo(buf[:n], up)
		}
	}()
	go func() {
		buf := make([]byte, 1500)
		var client net.Addr
		for {
			n, _, err := out.ReadFrom(buf)
			if err != nil {
				return
			}
			select {
			case client = <-clients:
			default:
			}
			if client != nil {
				_, _ = alt.WriteTo(buf[:n], client)
			}
		}
	}()
	return in.LocalAddr().String()
}

func TestRedTeam_UDPProofRefusesResponseFromAnotherSource(t *testing.T) {
	k, raw := newKey(t)
	f := startFakeTURN(t, fakeTURNOpts{key: raw})
	p := proberFor(f)
	p.UDPAddr = wrongSourceUDP(t, f.UDPAddr)

	err := p.Probe(withTimeout(t, 10*time.Second), k)
	requireRejected(t, err, managedturn.ErrProbeFailed)
	assert.Contains(t, err.Error(), "unexpected source")
}

// ---- 4. database ownership and mode -----------------------------------------

func TestRedTeam_OtherReadableDatabaseIsNeverLeftExposed(t *testing.T) {
	path := newDBPath(t)
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	require.NoError(t, os.Chmod(path, 0o644)) // #nosec G302 -- deliberately unsafe fixture: the tightening under test

	s, err := managedturn.OpenStore(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Zero(t, info.Mode().Perm()&0o037, "other-read is removed before any key is written")
}

func TestRedTeam_DatabaseRefusesGroupOrOtherExecAndWrite(t *testing.T) {
	for _, mode := range []os.FileMode{0o650, 0o670, 0o641, 0o642, 0o740} {
		path := newDBPath(t)
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		require.NoError(t, os.Chmod(path, mode))
		_, err := managedturn.OpenStore(context.Background(), path)
		if mode == 0o740 {
			require.NoError(t, err, "owner execute is not a group/other grant")
			continue
		}
		requireRejected(t, err, managedturn.ErrUnsafePermission)
	}
}

func TestRedTeam_ForeignOwnedPathsAreRefused(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to create files owned by another user")
	}
	t.Run("database", func(t *testing.T) {
		path := newDBPath(t)
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		require.NoError(t, os.Chmod(path, 0o640)) // #nosec G302 -- the accepted DB mode; only the owner is wrong
		require.NoError(t, os.Chown(path, nobodyID, nobodyID))
		_, err := managedturn.OpenStore(context.Background(), path)
		requireRejected(t, err, managedturn.ErrUnsafePermission)
	})
	t.Run("directory", func(t *testing.T) {
		dir := newActiveDir(t)
		require.NoError(t, os.Chown(dir, nobodyID, nobodyID))
		k, _ := newKey(t)
		requireRejected(t, managedturn.WriteActiveFile(dir, os.Getgid(), k), managedturn.ErrUnsafePermission)
	})
}

// ---- 5. generation exhaustion -----------------------------------------------

func TestRedTeam_ExhaustedGenerationRefusedBeforeAnyPublish(t *testing.T) {
	f := newFixture(t)
	a, rawA := newKey(t)
	b, _ := newKey(t)
	f.bootstrapWithCandidate(a, b)
	st := f.state()
	st.Generation = math.MaxUint64
	data, err := json.Marshal(st)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.statePath, data, 0o600))

	requireRejected(t, f.c.Promote(ctx5(t), b), managedturn.ErrInvalidState)
	assert.Empty(t, f.prober.callIDs())
	active, _ := f.activeBytes()
	assert.Equal(t, []byte(rawA), active, "nothing is published")
	assert.True(t, strings.Contains(string(mustRead(t, f.statePath)), "18446744073709551615"))
}

func mustRead(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- test fixture path
	require.NoError(t, err)
	return b
}
