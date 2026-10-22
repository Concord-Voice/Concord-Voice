package tlspin

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

func p384(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func p256Key(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func point(t *testing.T, k *ecdsa.PrivateKey) []byte {
	t.Helper()
	e, err := k.PublicKey.ECDH()
	if err != nil {
		t.Fatal(err)
	}
	return e.Bytes()
}

func mint(t *testing.T, k *ecdsa.PrivateKey, eku x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	c, err := Mint(k, eku)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// custom builds the malformed leaves Mint refuses to make: any key, any EKU
// set, an optional SAN, with the minted DN.
func custom(t *testing.T, signer crypto.Signer, ekus []x509.ExtKeyUsage, san bool) tls.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: ServerName},
		NotBefore: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: ekus}
	if san {
		tmpl.DNSNames = []string{ServerName}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, signer.Public(), signer)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: signer}
}

// issued is a ServerAuth leaf with the minted DN, SAN and validity whose key
// is leafKey but which issuerKey signed. Chain building against the
// authority's anchor passes exactly when issuerKey is the authority's key, so
// with it the refusal, if any, is ClientConfig's own.
func issued(t *testing.T, leafKey crypto.Signer, issuerKey *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: ServerName}, DNSNames: []string{ServerName},
		NotBefore: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, leafKey.Public(), issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: leafKey}
}

type pair struct {
	authority, cp *ecdsa.PrivateKey
	srv, cli      *tls.Config
}

// newPair is an authority and a CP: the client trusts only the authority's
// point. The server checks only the client's shape; whether the CP's pin is
// admitted is the listener's decision after the handshake (E1), so no pin is
// configured here.
func newPair(t *testing.T) pair {
	t.Helper()
	p := pair{authority: p384(t), cp: p384(t)}
	p.srv = ServerConfig(mint(t, p.authority, x509.ExtKeyUsageServerAuth))
	var err error
	if p.cli, err = ClientConfig(mint(t, p.cp, x509.ExtKeyUsageClientAuth), [][]byte{point(t, p.authority)}); err != nil {
		t.Fatal(err)
	}
	return p
}

// handshake runs srv and cli over loopback TCP, then moves one byte from the
// server so the client also reads any post-handshake message or alert. In
// TLS 1.3 a client finishes before the server has judged its certificate, so
// the server's refusal reaches the client on that read.
func handshake(t *testing.T, srv, cli *tls.Config) (s, c tls.ConnectionState, serr, cerr error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, err := ln.Accept()
		if err != nil {
			serr = err
			return
		}
		defer func() { _ = raw.Close() }()
		_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
		sc := tls.Server(raw, srv)
		if serr = sc.Handshake(); serr == nil {
			_, _ = sc.Write([]byte{1})
		}
		s = sc.ConnectionState()
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	cc := tls.Client(raw, cli)
	if cerr = cc.Handshake(); cerr == nil {
		var b [1]byte
		_, cerr = io.ReadFull(cc, b[:])
	}
	c = cc.ConnectionState()
	<-done
	return s, c, serr, cerr
}

func TestMintShape(t *testing.T) {
	k := p384(t)
	for _, c := range []struct {
		eku  x509.ExtKeyUsage
		sans []string
	}{
		{x509.ExtKeyUsageServerAuth, []string{ServerName}},
		{x509.ExtKeyUsageClientAuth, nil},
	} {
		wantMinted(t, mint(t, k, c.eku).Leaf, c.eku, c.sans)
	}
	if _, err := Mint(p256Key(t), x509.ExtKeyUsageServerAuth); err == nil {
		t.Fatal("minted a P-256 leaf")
	}
}

// wantMinted is DoR:1630's minted shape: the fixed CN, exactly eku, KeyUsage
// DigitalSignature only, not a CA, the fixed validity, ECDSA with SHA-384,
// the given SANs, and self-signed.
func wantMinted(t *testing.T, c *x509.Certificate, eku x509.ExtKeyUsage, sans []string) {
	t.Helper()
	if c.Subject.CommonName != ServerName || c.KeyUsage != x509.KeyUsageDigitalSignature || c.IsCA || !slices.Equal(c.DNSNames, sans) {
		t.Fatalf("eku %v: shape: %+v", eku, c)
	}
	if len(c.ExtKeyUsage) != 1 || c.ExtKeyUsage[0] != eku || len(c.UnknownExtKeyUsage) != 0 {
		t.Fatalf("eku %v: carries %v and unknown %v", eku, c.ExtKeyUsage, c.UnknownExtKeyUsage)
	}
	if !c.NotBefore.Equal(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) || !c.NotAfter.Equal(time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)) {
		t.Fatalf("eku %v: validity %v to %v", eku, c.NotBefore, c.NotAfter)
	}
	if c.SignatureAlgorithm != x509.ECDSAWithSHA384 {
		t.Fatalf("eku %v: signature algorithm %v", eku, c.SignatureAlgorithm)
	}
	// Self-signed: the issuer is the subject and the leaf's own key signed
	// it. Not CheckSignatureFrom(c), which refuses any parent that is not a
	// CA (RFC 5280 4.2.1.9) and so refuses every minted leaf by design.
	if !bytes.Equal(c.RawIssuer, c.RawSubject) {
		t.Fatalf("eku %v: not self-issued", eku)
	}
	if err := c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature); err != nil {
		t.Fatalf("eku %v: not self-signed: %v", eku, err)
	}
}

// Mint makes only the two leaves the link uses; any other EKU is refused,
// not minted.
func TestMintRefusesAnyOtherEKU(t *testing.T) {
	k := p384(t)
	for _, eku := range []x509.ExtKeyUsage{x509.ExtKeyUsageAny, x509.ExtKeyUsageCodeSigning,
		x509.ExtKeyUsageEmailProtection, x509.ExtKeyUsageTimeStamping, x509.ExtKeyUsageOCSPSigning} {
		if _, err := Mint(k, eku); err == nil {
			t.Fatalf("minted a leaf with EKU %v", eku)
		}
	}
}

// Pin is the listener's view of a peer, and ac.TLSPin is the form
// cp_tls_pins stores (LD-19): admission needs them equal for every leaf Mint
// makes. Pin hashes the SPKI bytes the peer sent, never a re-encoding, so the
// two agree exactly when that encoding is canonical, which Mint's always is.
func TestPinMatchesTheContractPin(t *testing.T) {
	for _, eku := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		k := p384(t)
		want, err := ac.TLSPin(point(t, k))
		if err != nil {
			t.Fatal(err)
		}
		wire, err := x509.ParseCertificate(mint(t, k, eku).Certificate[0]) // what PeerCertificates[0] holds
		if err != nil {
			t.Fatal(err)
		}
		if got := Pin(wire); len(got) != 48 || !bytes.Equal(got, want) {
			t.Fatalf("eku %v: Pin %x, ac.TLSPin %x", eku, got, want)
		}
	}
}

// The regression lock for D-204 (AC14), on go.mod's toolchain (OQ-7, B2-D12).
func TestPinnedAnchorFromKeyHandshakePassesOnBothEnds(t *testing.T) {
	p := newPair(t)
	s, c, serr, cerr := handshake(t, p.srv, p.cli)
	if serr != nil || cerr != nil {
		t.Fatalf("server %v, client %v", serr, cerr)
	}
	for _, st := range []tls.ConnectionState{s, c} {
		if st.Version != tls.VersionTLS13 || st.CurveID != tls.SecP384r1MLKEM1024 || st.NegotiatedProtocol != "http/1.1" {
			t.Fatalf("version %x, group %v, ALPN %q", st.Version, st.CurveID, st.NegotiatedProtocol)
		}
		if st.CipherSuite != tls.TLS_AES_128_GCM_SHA256 && st.CipherSuite != tls.TLS_CHACHA20_POLY1305_SHA256 {
			t.Fatalf("suite %s: X1 allows AES-128-GCM or ChaCha20-Poly1305 only", tls.CipherSuiteName(st.CipherSuite))
		}
		if st.DidResume {
			t.Fatal("resumed")
		}
	}
	if len(s.PeerCertificates) != 1 || !bytes.Equal(Pin(s.PeerCertificates[0]), Pin(mint(t, p.cp, x509.ExtKeyUsageClientAuth).Leaf)) {
		t.Fatal("the server did not see the CP's pinned key")
	}
}

// The measured reason the anchor carries no SAN (DoR:1633): an anchor with
// the leaf's SAN is taken for the leaf itself and chain building refuses it.
// This re-proves it on this toolchain; if it ever passes, OQ-7 applies.
func TestAnchorCarryingTheLeafSANIsRefused(t *testing.T) {
	p := newPair(t)
	throwaway := p384(t)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: ServerName},
		NotBefore: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, DNSNames: []string{ServerName}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &p.authority.PublicKey, throwaway)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(anchor)
	cli := p.cli.Clone()
	cli.RootCAs = pool
	if _, _, _, cerr := handshake(t, p.srv, cli); cerr == nil {
		t.Fatal("an anchor carrying the leaf's SAN verified: the D-204 premise changed (OQ-7)")
	}
}

func reasonOf(err error) Reason {
	var r Reason
	if errors.As(err, &r) {
		return r
	}
	return ""
}

// The server half of §3.3's handshake list (DoR:1662). Each client is the
// pinned CP with one thing changed. The list's pin rows (an unpinned key, the
// authority's own key) are not handshake refusals since E1:
// TestServerLeavesThePinToTheListener pins that they complete here, and the
// listener refuses them after the handshake.
func TestServerRefusesEachBadClient(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p256 := p256Key(t)
	for _, c := range []struct {
		name string
		edit func(p pair, cli *tls.Config)
		want Reason // "" = any handshake error
	}{
		{"no certificate", func(_ pair, cli *tls.Config) { cli.Certificates = nil }, ""},
		{"P-256", func(_ pair, cli *tls.Config) {
			cli.Certificates = []tls.Certificate{custom(t, p256, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, false)}
		}, ReasonNotP384},
		{"RSA", func(_ pair, cli *tls.Config) {
			cli.Certificates = []tls.Certificate{custom(t, rsaKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, false)}
		}, ReasonNotP384},
		{"ServerAuth presented as a client", func(p pair, cli *tls.Config) {
			cli.Certificates = []tls.Certificate{custom(t, p.cp, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, false)}
		}, ReasonEKU},
		{"no EKU", func(p pair, cli *tls.Config) {
			cli.Certificates = []tls.Certificate{custom(t, p.cp, nil, false)}
		}, ReasonEKU},
		{"both EKUs", func(p pair, cli *tls.Config) {
			cli.Certificates = []tls.Certificate{custom(t, p.cp,
				[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, false)}
		}, ReasonEKU},
		{"two certificates", func(p pair, cli *tls.Config) {
			c := mint(t, p.cp, x509.ExtKeyUsageClientAuth)
			c.Certificate = append(c.Certificate, mint(t, p384(t), x509.ExtKeyUsageClientAuth).Certificate[0])
			cli.Certificates = []tls.Certificate{c}
		}, ReasonChainLen},
		{"TLS 1.2 only", func(_ pair, cli *tls.Config) { cli.MinVersion, cli.MaxVersion = tls.VersionTLS12, tls.VersionTLS12 }, ""},
		{"X25519 only", func(_ pair, cli *tls.Config) { cli.CurvePreferences = []tls.CurveID{tls.X25519} }, ""},
		{"ALPN h2", func(_ pair, cli *tls.Config) { cli.NextProtos = []string{"h2"} }, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPair(t)
			cli := p.cli.Clone()
			c.edit(p, cli)
			_, _, serr, cerr := handshake(t, p.srv, cli)
			if serr == nil || cerr == nil {
				t.Fatalf("server %v, client %v: want both to fail", serr, cerr)
			}
			if c.want != "" && reasonOf(serr) != c.want {
				t.Fatalf("server reason %q (%v), want %q", reasonOf(serr), serr, c.want)
			}
		})
	}
}

// Pin membership and revocation are not handshake checks (E1, 2026-10-16).
// Go's TLS 1.3 server runs VerifyConnection before it checks the client's
// CertificateVerify, so a pin or revoked_keys decision there would run for a
// peer that merely presents a pinned certificate. A well-shaped client the
// authority never pinned therefore COMPLETES the handshake at this layer, and
// the server sees exactly the key the listener must then judge. These were
// ReasonUnpinned rows of TestServerRefusesEachBadClient; the refusals are the
// listener's tests now.
func TestServerLeavesThePinToTheListener(t *testing.T) {
	for _, c := range []struct {
		name string
		key  func(t *testing.T, p pair) *ecdsa.PrivateKey
	}{
		{"unpinned key behind an identical DN", func(t *testing.T, _ pair) *ecdsa.PrivateKey { return p384(t) }},
		{"the authority's own TLS key (distinct keys, B2-D30)", func(_ *testing.T, p pair) *ecdsa.PrivateKey { return p.authority }},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPair(t)
			cert := mint(t, c.key(t, p), x509.ExtKeyUsageClientAuth)
			cli := p.cli.Clone()
			cli.Certificates = []tls.Certificate{cert}
			s, _, serr, cerr := handshake(t, p.srv, cli)
			if serr != nil || cerr != nil {
				t.Fatalf("server %v, client %v: want the handshake to complete and leave the pin to the listener", serr, cerr)
			}
			if len(s.PeerCertificates) != 1 || !bytes.Equal(Pin(s.PeerCertificates[0]), Pin(cert.Leaf)) {
				t.Fatal("the server did not see the presented key")
			}
		})
	}
}

// shapeCalls wraps srv's VerifyConnection and records what each call
// returned, so a test can tell a shape refusal from a refusal after it.
func shapeCalls(t *testing.T, srv *tls.Config) (*tls.Config, func() []error) {
	t.Helper()
	c := srv.Clone()
	inner := c.VerifyConnection
	if inner == nil {
		t.Fatal("ServerConfig set no VerifyConnection")
	}
	var mu sync.Mutex
	var got []error
	c.VerifyConnection = func(cs tls.ConnectionState) error {
		err := inner(cs)
		mu.Lock()
		got = append(got, err)
		mu.Unlock()
		return err
	}
	return c, func() []error {
		mu.Lock()
		defer mu.Unlock()
		return append([]error(nil), got...)
	}
}

// With no pin filter in VerifyConnection (E1), possession is what the
// handshake itself proves: a client presenting the CP's own leaf with a key it
// does not hold passes every shape check and is still refused, at the
// CertificateVerify signature. The control holds the key and completes, so the
// key is the only difference between the two.
func TestServerRefusesAClientThatCannotSignForItsCertificate(t *testing.T) {
	p := newPair(t)
	victim := mint(t, p.cp, x509.ExtKeyUsageClientAuth)
	for _, c := range []struct {
		name    string
		key     *ecdsa.PrivateKey
		refused bool
	}{
		{"control: the CP holds its key", p.cp, false},
		{"the CP's leaf with another P-384 key", p384(t), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, calls := shapeCalls(t, p.srv)
			cli := p.cli.Clone()
			cli.Certificates = []tls.Certificate{{Certificate: victim.Certificate, PrivateKey: c.key}}
			_, _, serr, cerr := handshake(t, srv, cli)
			wantShapePassedOnce(t, calls(), serr, cerr)
			if !c.refused {
				if serr != nil || cerr != nil {
					t.Fatalf("server %v, client %v: the control must complete", serr, cerr)
				}
				return
			}
			wantCertificateVerifyRefusal(t, serr, cerr)
		})
	}
}

// wantShapePassedOnce: VerifyConnection ran once and admitted the shape, so
// any refusal after it is not a shape refusal.
func wantShapePassedOnce(t *testing.T, got []error, serr, cerr error) {
	t.Helper()
	switch {
	case len(got) == 0:
		t.Fatalf("VerifyConnection never ran (server %v, client %v): either the handshake failed before the client's certificate, "+
			"or crypto/tls now checks CertificateVerify first, which changes E1's premise", serr, cerr)
	case len(got) != 1 || got[0] != nil:
		t.Fatalf("VerifyConnection returned %v, want one nil: the shape checks must pass for the refusal to be CertificateVerify's", got)
	}
}

// wantCertificateVerifyRefusal: both ends failed, and the server's error is
// crypto/tls's own for a client CertificateVerify that does not verify under
// the presented leaf's key (handshake_server_tls13.go:1104 on go1.27.2), not a
// Reason and not the signature-algorithm refusal that precedes it.
func wantCertificateVerifyRefusal(t *testing.T, serr, cerr error) {
	t.Helper()
	if serr == nil || cerr == nil {
		t.Fatalf("server %v, client %v: want both to fail", serr, cerr)
	}
	if reasonOf(serr) != "" || !strings.Contains(serr.Error(), "invalid signature by the client certificate") {
		t.Fatalf("server %v: want the CertificateVerify refusal", serr)
	}
}

// The client half: the CP refuses an authority it did not pin. text, when
// set, names the layer that refuses: crypto/x509's chain building, crypto/tls's
// CertificateVerify check (go1.27.2's wording), or, through want, ClientConfig.
func TestClientRefusesEachBadServer(t *testing.T) {
	p256 := p256Key(t)
	for _, c := range []struct {
		name string
		cert func(p pair) tls.Certificate
		want Reason
		text string
	}{
		{"unpinned key behind an identical DN", func(pair) tls.Certificate {
			return mint(t, p384(t), x509.ExtKeyUsageServerAuth)
		}, "", "certificate signed by unknown authority"},
		// Chain building passes, since the authority's key signed the leaf;
		// only ClientConfig's pin loop sees the leaf's key is not pinned.
		{"a fresh P-384 key the authority signed", func(p pair) tls.Certificate {
			return issued(t, p384(t), p.authority)
		}, ReasonUnpinned, ""},
		{"a P-256 key the authority signed", func(p pair) tls.Certificate {
			return issued(t, p256, p.authority)
		}, ReasonNotP384, ""},
		// An impostor replaying the authority's own public leaf passes chain
		// building and the pin; only its CertificateVerify, made with a key
		// that is not the authority's, refuses it.
		{"the authority's leaf with another P-384 key", func(p pair) tls.Certificate {
			return tls.Certificate{Certificate: mint(t, p.authority, x509.ExtKeyUsageServerAuth).Certificate, PrivateKey: p384(t)}
		}, "", "invalid signature by the server certificate"},
		{"P-256", func(pair) tls.Certificate {
			return custom(t, p256, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, true)
		}, "", "certificate signed by unknown authority"},
		{"ClientAuth presented as a server", func(p pair) tls.Certificate {
			return custom(t, p.authority, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, true)
		}, "", "incompatible key usage"},
		{"both EKUs", func(p pair) tls.Certificate {
			return custom(t, p.authority, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, true)
		}, ReasonEKU, ""},
		{"two certificates", func(p pair) tls.Certificate {
			c := mint(t, p.authority, x509.ExtKeyUsageServerAuth)
			c.Certificate = append(c.Certificate, mint(t, p384(t), x509.ExtKeyUsageServerAuth).Certificate[0])
			return c
		}, ReasonChainLen, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPair(t)
			srv := p.srv.Clone()
			srv.Certificates = []tls.Certificate{c.cert(p)}
			_, _, _, cerr := handshake(t, srv, p.cli)
			if cerr == nil {
				t.Fatal("the client accepted it")
			}
			if c.want != "" && reasonOf(cerr) != c.want {
				t.Fatalf("client reason %q (%v), want %q", reasonOf(cerr), cerr, c.want)
			}
			if c.text != "" && (reasonOf(cerr) != "" || !strings.Contains(cerr.Error(), c.text)) {
				t.Fatalf("client %v: want a refusal naming %q", cerr, c.text)
			}
		})
	}
}

// DidResume stays false even for a client that offers a session cache
// (SessionTicketsDisabled; DoR:1635). Re-enabling tickets fails this test.
func TestNoResumption(t *testing.T) {
	p := newPair(t)
	cli := p.cli.Clone()
	cli.ClientSessionCache = tls.NewLRUClientSessionCache(4)
	for i := 0; i < 2; i++ {
		s, c, serr, cerr := handshake(t, p.srv, cli)
		if serr != nil || cerr != nil {
			t.Fatalf("handshake %d: %v %v", i, serr, cerr)
		}
		if s.DidResume || c.DidResume {
			t.Fatalf("handshake %d resumed", i)
		}
	}
}

func TestAnchorsRefuseABadPoint(t *testing.T) {
	if _, err := Anchors([][]byte{{4, 1, 2}}); err == nil {
		t.Fatal("accepted a malformed point")
	}
}
