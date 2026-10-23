package managedturn

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- SHA1 is required by the TURN REST API credential spec (RFC 5389)
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/pion/logging"
	"github.com/pion/stun/v3"
	"github.com/pion/turn/v4"
)

const (
	// probeAttemptTimeout bounds one Allocate so a retry is still possible
	// inside the caller's deadline.
	probeAttemptTimeout = 10 * time.Second
	// probeRetryDelay spaces retries; rule 4 expects a reader that collided
	// with a commit to succeed moments later.
	probeRetryDelay = 250 * time.Millisecond
	// maxProbeRetries caps a misconfigured retry budget.
	maxProbeRetries = 5
)

// silentLogs keeps pion from writing anything, whatever PION_LOG_* says.
var silentLogs = &logging.DefaultLoggerFactory{Writer: io.Discard, DefaultLogLevel: logging.LogLevelDisabled}

// TURNProber allocates over UDP, TCP and TLS with a minted ≤60 s credential.
// The proof is a mutually authenticated Allocate and Refresh (see prove); no
// relay round-trip is attempted, because production denied-peer-ip rules block
// loopback and bridge peers. Each allocation is released with Refresh lifetime
// 0 right after it is proven.
type TURNProber struct {
	Realm      string
	UDPAddr    string // host:3478
	TCPAddr    string // host:3478
	TLSAddr    string // host:5349
	TLSConfig  *tls.Config
	TTL        time.Duration
	Now        func() time.Time
	MaxRetries int
}

// probeTransport names one of the three proofs.
type probeTransport string

const (
	probeUDP probeTransport = "udp"
	probeTCP probeTransport = "tcp"
	probeTLS probeTransport = "tls"
)

// Probe implements Prober. Every transport must allocate; the first one that
// cannot, after its retries, fails the whole proof with ErrProbeFailed. Error
// text names the transport and stage only: never the credential, and never
// the underlying library error.
func (p *TURNProber) Probe(ctx context.Context, key Key) error {
	if !key.valid() {
		return ErrInvalidKey
	}
	if err := p.validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrProbeFailed, err)
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	retries := min(max(p.MaxRetries, 0), maxProbeRetries)

	for _, tr := range []probeTransport{probeUDP, probeTCP, probeTLS} {
		var err error
		for attempt := 0; attempt <= retries; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return fmt.Errorf("%w: %s: %w", ErrProbeFailed, tr, ctx.Err())
				case <-time.After(probeRetryDelay):
				}
			}
			// A fresh credential per attempt: one shared across every
			// transport and retry could expire mid-proof on a slow leg.
			username, password := MintProbeCredential(key, p.TTL, now())
			if err = p.allocateOnce(ctx, tr, username, password); err == nil {
				break
			}
			if ctx.Err() != nil {
				return fmt.Errorf("%w: %s: %w", ErrProbeFailed, tr, ctx.Err())
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// validate refuses an incomplete prober, and any TLS configuration that would
// accept an unverified server.
func (p *TURNProber) validate() error {
	if p.Realm == "" || p.UDPAddr == "" || p.TCPAddr == "" || p.TLSAddr == "" ||
		p.TLSConfig == nil || p.TLSConfig.InsecureSkipVerify {
		return fmt.Errorf("%w: %w: incomplete prober configuration", ErrProbeFailed, ErrInvalidConfig)
	}
	return nil
}

// ProbeError is a probe failure that names only its transport and a fixed
// stage, so it is safe to print. It matches ErrProbeFailed.
type ProbeError struct {
	Transport string
	Stage     string
}

func (e *ProbeError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrProbeFailed, e.Transport, e.Stage)
}

func (e *ProbeError) Unwrap() error { return ErrProbeFailed }

func stageError(tr probeTransport, stage string) error {
	return &ProbeError{Transport: string(tr), Stage: stage}
}

// allocateOnce performs one fresh authenticated Allocate over tr and releases
// it. The whole attempt, including name resolution and the TLS handshake, is
// bounded by ctx and probeAttemptTimeout.
func (p *TURNProber) allocateOnce(parent context.Context, tr probeTransport, username, password string) error {
	ctx, cancel := context.WithTimeout(parent, probeAttemptTimeout)
	defer cancel()

	conn, serverAddr, err := p.dial(ctx, tr)
	if err != nil {
		return err
	}
	// The verdict is decided by the exchange; a close error on the probe's own
	// socket cannot change it, and failing a proven key over it would be wrong.
	defer func() { _ = conn.Close() }()

	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: serverAddr,
		TURNServerAddr: serverAddr,
		Conn:           conn,
		Username:       username,
		Password:       password,
		Realm:          p.Realm,
		LoggerFactory:  silentLogs,
	})
	if err != nil {
		return stageError(tr, "client setup failed")
	}
	defer client.Close()

	done := make(chan error, 1)
	go func() {
		if err := client.Listen(); err != nil {
			done <- errProofStage("listen failed")
			return
		}
		done <- p.prove(client, username, password)
	}()

	select {
	case err := <-done:
		if err != nil {
			return stageError(tr, err.Error())
		}
		return nil
	case <-ctx.Done():
		// Closing the transactions and the socket unblocks the pending
		// transaction; wait for the goroutine so nothing outlives the probe.
		client.Close()
		_ = conn.Close() // unblocks the transaction; the deferred close reports nothing either
		<-done
		if parent.Err() != nil {
			return fmt.Errorf("%w: %s: %w", ErrProbeFailed, tr, parent.Err())
		}
		return stageError(tr, "allocate timed out")
	}
}

// errProofStage is a proof failure that carries only a fixed stage name.
type errProofStage string

func (e errProofStage) Error() string { return string(e) }

// requestedUDP is REQUESTED-TRANSPORT = UDP (17), RFC 8656 §18.13.
var requestedUDP = stun.RawAttribute{Type: stun.AttrRequestedTransport, Value: []byte{17, 0, 0, 0}}

// lifetimeZero is LIFETIME = 0, which releases the allocation.
var lifetimeZero = stun.RawAttribute{Type: stun.AttrLifetime, Value: []byte{0, 0, 0, 0}}

// prove runs a mutual long-term-credential exchange and releases what it
// allocated. pion's own Allocate accepts any success response matching the
// transaction ID, so this drives the transactions itself and requires every
// success response to (a) come from the server address dialled and (b) carry a
// MESSAGE-INTEGRITY valid under the probe credential. A responder that does
// not hold the key cannot produce (b), so success proves the server read this
// key from its database.
//
//  1. Allocate, unauthenticated: must be challenged 401 with NONCE and the
//     configured REALM.
//  2. Allocate, authenticated: must succeed, be integrity-valid and carry
//     XOR-RELAYED-ADDRESS.
//  3. Refresh LIFETIME=0, authenticated: must succeed and be integrity-valid;
//     this both re-proves the key and releases the allocation.
//
// Once the authenticated Allocate is sent, every failure first sends one more
// best-effort authenticated release: the server may have reserved a relay
// even if its reply was lost or altered, and on UDP nothing else frees it.
func (p *TURNProber) prove(client *turn.Client, username, password string) error {
	server := client.TURNServerAddr()
	allocate := stun.NewType(stun.MethodAllocate, stun.ClassRequest)
	refresh := stun.NewType(stun.MethodRefresh, stun.ClassRequest)

	res, err := transact(client, server, allocate, requestedUDP, stun.Fingerprint)
	if err != nil {
		return err
	}
	var nonce stun.Nonce
	var realm stun.Realm
	if errorCode(res) != stun.CodeUnauthorized || nonce.GetFrom(res) != nil || realm.GetFrom(res) != nil {
		return errProofStage("server did not challenge the allocation")
	}
	if realm.String() != p.Realm {
		return errProofStage("server realm differs from the configured realm")
	}

	user := stun.NewUsername(username)
	ourRealm := stun.NewRealm(p.Realm)
	integrity := stun.NewLongTermIntegrity(username, p.Realm, password)
	authed := func(t stun.MessageType, attr stun.Setter) (*stun.Message, error) {
		for attempt := 0; ; attempt++ {
			res, err := transact(client, server, t, attr, user, ourRealm, &nonce, integrity, stun.Fingerprint)
			if err != nil {
				return nil, err
			}
			if attempt == 0 && errorCode(res) == stun.CodeStaleNonce && nonce.GetFrom(res) == nil {
				continue
			}
			return res, nil
		}
	}

	release := func(stage error) error {
		_, _ = authed(refresh, lifetimeZero) // best effort; its result proves nothing
		return stage
	}

	res, err = authed(allocate, requestedUDP)
	if err != nil {
		return release(err)
	}
	if !isSuccess(res, stun.MethodAllocate) {
		return errProofStage("allocate refused")
	}
	if integrity.Check(res) != nil {
		return release(errProofStage("allocate response not authenticated by the key"))
	}
	var relayed stun.XORMappedAddress
	if relayed.GetFromAs(res, stun.AttrXORRelayedAddress) != nil {
		return release(errProofStage("allocate response has no relayed address"))
	}

	res, err = authed(refresh, lifetimeZero)
	if err != nil {
		return release(err)
	}
	if !isSuccess(res, stun.MethodRefresh) || integrity.Check(res) != nil {
		return release(errProofStage("release not authenticated by the key"))
	}
	return nil
}

// transact performs one STUN transaction and requires the response to come
// from server; pion matches responses by transaction ID alone.
func transact(client *turn.Client, server net.Addr, setters ...stun.Setter) (*stun.Message, error) {
	msg, err := stun.Build(append([]stun.Setter{stun.TransactionID}, setters...)...)
	if err != nil {
		return nil, errProofStage("request build failed")
	}
	res, err := client.PerformTransaction(msg, server, false)
	if err != nil || res.Msg == nil {
		return nil, errProofStage("request unanswered")
	}
	if res.From == nil || server == nil || res.From.String() != server.String() {
		return nil, errProofStage("response from an unexpected source")
	}
	return res.Msg, nil
}

// errorCode returns the ERROR-CODE of an error response, or 0.
func errorCode(m *stun.Message) stun.ErrorCode {
	var code stun.ErrorCodeAttribute
	if m.Type.Class != stun.ClassErrorResponse || code.GetFrom(m) != nil {
		return 0
	}
	return code.Code
}

func isSuccess(m *stun.Message, method stun.Method) bool {
	return m.Type.Method == method && m.Type.Class == stun.ClassSuccessResponse
}

// dial opens the client socket for tr and returns the literal server address
// pion should use, so pion never performs its own (context-free) lookup.
func (p *TURNProber) dial(ctx context.Context, tr probeTransport) (net.PacketConn, string, error) {
	var d net.Dialer
	switch tr {
	case probeUDP:
		server, err := resolveUDP4(ctx, p.UDPAddr)
		if err != nil {
			return nil, "", stageError(tr, "address resolution failed")
		}
		var lc net.ListenConfig
		conn, err := lc.ListenPacket(ctx, "udp4", "0.0.0.0:0")
		if err != nil {
			return nil, "", stageError(tr, "socket setup failed")
		}
		return conn, server, nil
	case probeTCP:
		c, err := d.DialContext(ctx, "tcp4", p.TCPAddr)
		if err != nil {
			return nil, "", stageError(tr, "dial failed")
		}
		return turn.NewSTUNConn(c), c.RemoteAddr().String(), nil
	case probeTLS:
		cfg := p.TLSConfig.Clone()
		if cfg.MinVersion < tls.VersionTLS12 {
			cfg.MinVersion = tls.VersionTLS12
		}
		raw, err := d.DialContext(ctx, "tcp4", p.TLSAddr)
		if err != nil {
			return nil, "", stageError(tr, "dial failed")
		}
		tc := tls.Client(raw, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = raw.Close() // the handshake failure is the result; nothing was sent to keep
			return nil, "", stageError(tr, "tls handshake or verification failed")
		}
		return turn.NewSTUNConn(tc), raw.RemoteAddr().String(), nil
	default:
		return nil, "", stageError(tr, "unknown transport")
	}
}

// resolveUDP4 turns host:port into a literal IPv4 host:port under ctx.
func resolveUDP4(ctx context.Context, addr string) (string, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return "", errors.New("invalid port")
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", errors.New("no IPv4 address")
	}
	return netip.AddrPortFrom(ips[0].Unmap(), uint16(port)).String(), nil
}

// MintProbeCredential returns the TURN REST username "<expiry>:turn-probe" and
// its base64 HMAC-SHA1 password, as coturn's use-auth-secret expects. ttl is
// clamped to (0, MaxProbeTTL]; a non-positive ttl means MaxProbeTTL. The zero
// Key mints nothing.
func MintProbeCredential(key Key, ttl time.Duration, now time.Time) (username, password string) {
	if !key.valid() {
		return "", ""
	}
	if ttl <= 0 || ttl > MaxProbeTTL {
		ttl = MaxProbeTTL
	}
	username = strconv.FormatInt(now.Add(ttl).Unix(), 10) + ":" + ProbeUserTag
	mac := hmac.New(sha1.New, []byte(key.value)) // #nosec G505 -- protocol-required HMAC-SHA1
	_, _ = mac.Write([]byte(username))           // hash.Hash.Write never returns an error
	return username, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
