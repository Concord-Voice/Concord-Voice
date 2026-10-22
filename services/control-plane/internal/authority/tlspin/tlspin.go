// Package tlspin is the account authority's mTLS (#3587 PR-B2, DoR §3.3,
// D-204, LD-19): certificates minted in memory from a P-384 key, pins in
// place of a CA, and the anchor-from-key root that lets a stock crypto/tls
// client verify a pinned, self-signed leaf. PR #2627's client imports it, so
// one regression suite covers both ends.
package tlspin

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"time"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// ServerName is every minted leaf's CN and the authority leaf's only DNS SAN
// (B2-D19). #2627's ACCOUNT_AUTHORITY_SERVER_NAME must equal it.
const ServerName = "concord-account-authority"

var (
	notBefore = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter  = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	groups    = []tls.CurveID{tls.SecP384r1MLKEM1024, tls.CurveP384}
	alpn      = []string{"http/1.1"}
)

// Reason is why a peer certificate was refused: the TLS log enum (B2-D9).
type Reason string

// The reasons. ServerConfig returns only the first three; the authority's
// listener returns ReasonUnpinned, ReasonRevoked and ReasonStore after the
// handshake, and ClientConfig returns ReasonUnpinned for an authority it did
// not pin.
const (
	ReasonChainLen Reason = "chain_len"
	ReasonNotP384  Reason = "not_p384"
	ReasonEKU      Reason = "eku"
	ReasonUnpinned Reason = "unpinned"
	ReasonRevoked  Reason = "revoked"
	ReasonStore    Reason = "store"
)

func (r Reason) Error() string { return "tlspin: peer refused: " + string(r) }

func leafTemplate(san bool) *x509.Certificate {
	t := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: ServerName},
		NotBefore: notBefore, NotAfter: notAfter}
	if san {
		t.DNSNames = []string{ServerName}
	}
	return t
}

// Mint returns a self-signed leaf for key with exactly one EKU and KeyUsage
// DigitalSignature. A ServerAuth leaf carries the DNS SAN; a ClientAuth leaf
// carries none; any other EKU is refused. Validity is not a control (LD-19).
func Mint(key *ecdsa.PrivateKey, eku x509.ExtKeyUsage) (tls.Certificate, error) {
	if key == nil || key.Curve != elliptic.P384() {
		return tls.Certificate{}, errors.New("tlspin: the TLS key must be ECDSA P-384")
	}
	if eku != x509.ExtKeyUsageServerAuth && eku != x509.ExtKeyUsageClientAuth {
		return tls.Certificate{}, errors.New("tlspin: the EKU must be ServerAuth or ClientAuth")
	}
	tmpl := leafTemplate(eku == x509.ExtKeyUsageServerAuth)
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{eku}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// Anchors is the CP's RootCAs: one root per authority TLS point, carrying
// that point as its key, the leaf's DN, CA flags and no SAN. A throwaway key
// signs it, because roots are not signature-checked; chain building checks
// the leaf's signature under the root's key, which only the pinned key can
// make. With the leaf's SAN, crypto/x509 takes the root for the leaf itself
// and refuses the chain, measured on go1.26.6 and re-proved by
// TestAnchorCarryingTheLeafSANIsRefused (D-204).
func Anchors(points [][]byte) (*x509.CertPool, error) {
	throwaway, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	for _, p := range points {
		pub, err := ac.ParsePoint(p)
		if err != nil {
			return nil, err
		}
		tmpl := leafTemplate(false)
		tmpl.IsCA, tmpl.BasicConstraintsValid, tmpl.KeyUsage = true, true, x509.KeyUsageCertSign
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, throwaway)
		if err != nil {
			return nil, err
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		pool.AddCert(c)
	}
	return pool, nil
}

// Pin is SHA-384 of a certificate's SPKI DER: the cp_tls_pins form (LD-19,
// ac.TLSPin over the same bytes).
func Pin(c *x509.Certificate) []byte {
	h := sha512.Sum384(c.RawSubjectPublicKeyInfo)
	return h[:]
}

func base(cert tls.Certificate) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, CurvePreferences: groups,
		Certificates: []tls.Certificate{cert}, NextProtos: alpn}
}

// ServerConfig is the authority's side (DoR:1631–1636). Its VerifyConnection
// checks only the client certificate's shape: one certificate, P-384, exactly
// the ClientAuth EKU. It judges no pin, because Go's TLS 1.3 server runs
// VerifyConnection BEFORE it checks the client's CertificateVerify: anything
// here runs for a peer that merely presents a pinned certificate it cannot
// sign for. Pin membership in HW.cp_tls_pins and absence from revoked_keys are
// the listener's to judge (B2 Task 7), after Handshake returns, once the
// handshake has proved possession of the key (E1, 2026-10-16). Until then a
// completed handshake authenticates nothing.
func ServerConfig(cert tls.Certificate) *tls.Config {
	c := base(cert)
	c.ClientAuth = tls.RequireAnyClientCert // no chain building on this side; CertificateVerify proves possession
	c.SessionTicketsDisabled = true
	c.VerifyConnection = func(cs tls.ConnectionState) error {
		_, err := shape(cs, x509.ExtKeyUsageClientAuth)
		return err
	}
	return c
}

// ClientConfig is the CP's side: anchor-from-key roots for HW's authority
// TLS points, the fixed server name, the same shape checks, and the leaf's
// point among them. Chain verification is never skipped; ClientSessionCache
// stays nil.
func ClientConfig(cert tls.Certificate, authorityPoints [][]byte) (*tls.Config, error) {
	roots, err := Anchors(authorityPoints)
	if err != nil {
		return nil, err
	}
	c := base(cert)
	c.RootCAs, c.ServerName = roots, ServerName
	c.VerifyConnection = func(cs tls.ConnectionState) error {
		leaf, err := shape(cs, x509.ExtKeyUsageServerAuth)
		if err != nil {
			return err
		}
		pt, err := leaf.PublicKey.(*ecdsa.PublicKey).ECDH()
		if err != nil {
			return ReasonNotP384
		}
		for _, p := range authorityPoints {
			if bytes.Equal(p, pt.Bytes()) {
				return nil
			}
		}
		return ReasonUnpinned
	}
	return c, nil
}

// shape is the checks both ends share: one certificate, P-384, exactly the
// expected EKU (a missing one refused).
func shape(cs tls.ConnectionState, eku x509.ExtKeyUsage) (*x509.Certificate, error) {
	if len(cs.PeerCertificates) != 1 {
		return nil, ReasonChainLen
	}
	leaf := cs.PeerCertificates[0]
	if pub, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P384() {
		return nil, ReasonNotP384
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != eku || len(leaf.UnknownExtKeyUsage) != 0 {
		return nil, ReasonEKU
	}
	return leaf, nil
}
