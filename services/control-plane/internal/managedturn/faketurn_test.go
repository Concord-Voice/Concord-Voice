package managedturn_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- SHA1 is required by the TURN REST API credential spec (RFC 5389)
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/turn/v4"
	"github.com/stretchr/testify/require"
)

// restPassword is the TURN REST password for username under secret:
// base64(HMAC-SHA1(secret, username)). Computed independently of the code
// under test so the fake server verifies the credential format for real.
func restPassword(secret, username string) string {
	mac := hmac.New(sha1.New, []byte(secret)) // #nosec G505 -- protocol-required HMAC-SHA1
	_, _ = mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// fakeTURNOpts selects which transports the fake serves.
type fakeTURNOpts struct {
	key          string // shared secret the server authenticates against
	skipUDP      bool   // advertise a closed UDP port instead
	skipTCP      bool   // advertise a closed TCP port instead
	skipTLS      bool   // advertise a closed TLS port instead
	blackholeUDP bool   // UDP port is bound but never answers
}

// fakeTURN is an in-process pion TURN server on 127.0.0.1 that authenticates
// the TURN REST long-term credential against a shared key, serving UDP, TCP
// and TLS on ephemeral ports.
type fakeTURN struct {
	Realm     string
	UDPAddr   string
	TCPAddr   string
	TLSAddr   string
	ClientTLS *tls.Config // trusts the fake's self-signed certificate

	mu        sync.Mutex
	created   map[string]int
	deleted   map[string]int
	usernames []string
	byPort    map[string]string
}

func closedTCPAddr(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func closedUDPAddr(t testing.TB) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := c.LocalAddr().String()
	require.NoError(t, c.Close())
	return addr
}

// selfSignedTLS returns a server config and a pool that trusts it, valid for
// 127.0.0.1 only. The key pair is generated per test and never reused.
func selfSignedTLS(t testing.TB) (*tls.Config, *x509.CertPool) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "managedturn fake turn"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	parsed, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}},
		MinVersion:   tls.VersionTLS12,
	}, pool
}

func startFakeTURN(t testing.TB, opts fakeTURNOpts) *fakeTURN {
	t.Helper()
	f := &fakeTURN{
		Realm:   testRealm,
		created: map[string]int{},
		deleted: map[string]int{},
		byPort:  map[string]string{},
	}
	serverTLS, pool := selfSignedTLS(t)
	f.ClientTLS = &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}

	gen := func() turn.RelayAddressGenerator {
		return &turn.RelayAddressGeneratorStatic{RelayAddress: net.ParseIP("127.0.0.1"), Address: "127.0.0.1"}
	}
	port := func(addr string) string {
		_, p, err := net.SplitHostPort(addr)
		require.NoError(t, err)
		return p
	}

	var packetConfigs []turn.PacketConnConfig
	var listenerConfigs []turn.ListenerConfig

	switch {
	case opts.skipUDP:
		f.UDPAddr = closedUDPAddr(t)
	case opts.blackholeUDP:
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		f.UDPAddr = c.LocalAddr().String() // bound, never read, never answered
	default:
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		f.UDPAddr = c.LocalAddr().String()
		f.byPort[port(f.UDPAddr)] = "udp"
		packetConfigs = append(packetConfigs, turn.PacketConnConfig{PacketConn: c, RelayAddressGenerator: gen()})
	}

	if opts.skipTCP {
		f.TCPAddr = closedTCPAddr(t)
	} else {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		f.TCPAddr = l.Addr().String()
		f.byPort[port(f.TCPAddr)] = "tcp"
		listenerConfigs = append(listenerConfigs, turn.ListenerConfig{Listener: l, RelayAddressGenerator: gen()})
	}

	if opts.skipTLS {
		f.TLSAddr = closedTCPAddr(t)
	} else {
		l, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
		require.NoError(t, err)
		f.TLSAddr = l.Addr().String()
		f.byPort[port(f.TLSAddr)] = "tls"
		listenerConfigs = append(listenerConfigs, turn.ListenerConfig{Listener: l, RelayAddressGenerator: gen()})
	}

	transport := func(dst net.Addr) string {
		if dst == nil {
			return "unknown"
		}
		if name, ok := f.byPort[port(dst.String())]; ok {
			return name
		}
		return "unknown"
	}

	cfg := turn.ServerConfig{
		Realm:             f.Realm,
		PacketConnConfigs: packetConfigs,
		ListenerConfigs:   listenerConfigs,
		AuthHandler: func(username, realm string, _ net.Addr) ([]byte, bool) {
			f.mu.Lock()
			f.usernames = append(f.usernames, username)
			f.mu.Unlock()
			expiry, _, found := strings.Cut(username, ":")
			if !found {
				return nil, false
			}
			exp, err := strconv.ParseInt(expiry, 10, 64)
			if err != nil || exp < time.Now().Unix() {
				return nil, false
			}
			return turn.GenerateAuthKey(username, realm, restPassword(opts.key, username)), true
		},
		EventHandler: turn.EventHandler{
			OnAllocationCreated: func(_, dst net.Addr, _, _, _ string, _ net.Addr, _ int) {
				f.mu.Lock()
				f.created[transport(dst)]++
				f.mu.Unlock()
			},
			OnAllocationDeleted: func(_, dst net.Addr, _, _, _ string) {
				f.mu.Lock()
				f.deleted[transport(dst)]++
				f.mu.Unlock()
			},
		},
	}
	if len(packetConfigs) == 0 && len(listenerConfigs) == 0 {
		return f
	}
	server, err := turn.NewServer(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	return f
}

func (f *fakeTURN) createdCount(transport string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created[transport]
}

func (f *fakeTURN) deletedCount(transport string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleted[transport]
}

func (f *fakeTURN) totalCreated() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.created {
		n += c
	}
	return n
}

func (f *fakeTURN) seenUsernames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.usernames...)
}
