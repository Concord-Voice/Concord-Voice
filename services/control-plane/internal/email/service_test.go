package email

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testHost       = "127.0.0.1"
	testFrom       = "noreply@concord.test"
	testOwnerEmail = "owner@example.com"
	testUserEmail  = "user@example.com"
	smtpOK         = "250 OK"
)

// ── Helpers ─────────────────────────────────────────────────────────────────

func newDevService() *Service {
	cfg := &config.Config{
		SMTPHost: "", // empty = dev mode
	}
	return NewService(cfg, logger.New("test"))
}

// selfSignedTLS generates a self-signed TLS certificate for the mock SMTP server.
func selfSignedTLS() *tls.Config {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost", testHost},
		IPAddresses:  []net.IP{net.ParseIP(testHost)},
	}
	certDER, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, _ := tls.X509KeyPair(certPEM, keyPEM)
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12} //nolint:gosec // test-only self-signed cert
}

// mockSMTPServer starts a local SMTP server that supports STARTTLS.
// Returns the listener address and a function to retrieve the captured message.
type smtpCapture struct {
	mu   sync.Mutex
	msgs []string
}

func (c *smtpCapture) add(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, msg)
}

func (c *smtpCapture) last() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.msgs) == 0 {
		return ""
	}
	return c.msgs[len(c.msgs)-1]
}

type mockSMTPOpts struct {
	advertiseSTARTTLS bool
	rejectAuth        bool
}

func startMockSMTP(t *testing.T, opts mockSMTPOpts) (addr string, capture *smtpCapture) {
	t.Helper()
	tlsCfg := selfSignedTLS()
	ln, err := net.Listen("tcp", testHost+":0")
	require.NoError(t, err)
	capture = &smtpCapture{}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go handleSMTPConn(conn, tlsCfg, capture, opts)
		}
	}()

	return ln.Addr().String(), capture
}

func handleSMTPConn(conn net.Conn, tlsCfg *tls.Config, capture *smtpCapture, opts mockSMTPOpts) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	write := func(s string) { _, _ = fmt.Fprintf(conn, "%s\r\n", s) }

	write("220 localhost ESMTP mock")

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		upper := strings.ToUpper(cmd)

		switch {
		case strings.HasPrefix(upper, "EHLO") || strings.HasPrefix(upper, "HELO"):
			write("250-localhost")
			if opts.advertiseSTARTTLS {
				write("250-STARTTLS")
			}
			write("250-AUTH PLAIN LOGIN")
			write(smtpOK)

		case upper == "STARTTLS":
			if !opts.advertiseSTARTTLS {
				write("502 Not supported")
				continue
			}
			write("220 Ready to start TLS")
			tlsConn := tls.Server(conn, tlsCfg)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			r = bufio.NewReader(conn)
			write = func(s string) { _, _ = fmt.Fprintf(conn, "%s\r\n", s) }

		case strings.HasPrefix(upper, "AUTH"):
			if opts.rejectAuth {
				write("535 Authentication failed")
				continue
			}
			write("235 Authentication successful")

		case strings.HasPrefix(upper, "MAIL FROM:"):
			write(smtpOK)

		case strings.HasPrefix(upper, "RCPT TO:"):
			write(smtpOK)

		case upper == "DATA":
			write("354 Start mail input")
			var body strings.Builder
			for {
				dataLine, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimSpace(dataLine) == "." {
					break
				}
				body.WriteString(dataLine)
			}
			capture.add(body.String())
			write(smtpOK)

		case upper == "QUIT":
			write("221 Bye")
			return

		default:
			write("500 Unrecognized command")
		}
	}
}

// newProdService creates a service pointing at the given mock SMTP address.
func newProdService(addr string) *Service {
	host, port, _ := net.SplitHostPort(addr)
	p := 0
	_, _ = fmt.Sscanf(port, "%d", &p)
	return &Service{
		host:      host,
		port:      p,
		username:  "testuser",
		password:  "testpass", //nolint:gosec // test credential
		from:      testFrom,
		log:       logger.New("test"),
		devMode:   false,
		tlsConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // test-only: self-signed cert
	}
}

// ── Existing Tests ──────────────────────────────────────────────────────────

func TestNewServiceDevMode(t *testing.T) {
	svc := newDevService()
	assert.True(t, svc.IsDevMode())
	assert.True(t, svc.devMode)
}

func TestNewServiceProductionMode(t *testing.T) {
	cfg := &config.Config{
		SMTPHost:     "smtp.example.com",
		SMTPPort:     587,
		SMTPUsername: "user",
		SMTPPassword: "pass",
		SMTPFrom:     "noreply@example.com",
	}
	svc := NewService(cfg, logger.New("test"))
	assert.False(t, svc.IsDevMode())
}

func TestSendVerificationCodeDevMode(t *testing.T) {
	svc := newDevService()
	err := svc.SendVerificationCode("test@example.com", "123456")
	assert.NoError(t, err)
}

func TestSendRecoveryCodeDevMode(t *testing.T) {
	svc := newDevService()
	err := svc.SendRecoveryCode("test@example.com", "654321")
	assert.NoError(t, err)
}

func TestSendOwnershipTransferNotificationDevMode(t *testing.T) {
	svc := newDevService()
	err := svc.SendOwnershipTransferNotification(
		testOwnerEmail,
		"My Server",
		"newowner",
		"token-abc-123",
	)
	assert.NoError(t, err)
}

func TestSendTemplatedEmailInvalidFrom(t *testing.T) {
	svc := &Service{
		from:    "invalid-email",
		devMode: false,
	}
	err := svc.sendTemplatedEmail("to@example.com", "Test", verificationTmpl, map[string]string{"Code": "123456"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid from address")
}

func TestSendTemplatedEmailInvalidTo(t *testing.T) {
	svc := &Service{
		from:    "valid@example.com",
		devMode: false,
	}
	err := svc.sendTemplatedEmail("invalid-email", "Test", verificationTmpl, map[string]string{"Code": "123456"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid to address")
}

func TestTemplatesLoad(t *testing.T) {
	assert.NotNil(t, verificationTmpl)
	assert.NotNil(t, recoveryTmpl)
	assert.NotNil(t, ownershipTransferTmpl)
}

// ── Production Path Tests (mock SMTP) ───────────────────────────────────────

func TestSendVerificationCodeProd(t *testing.T) {
	addr, capture := startMockSMTP(t, mockSMTPOpts{advertiseSTARTTLS: true})
	svc := newProdService(addr)

	err := svc.SendVerificationCode(testUserEmail, "987654")
	require.NoError(t, err)

	msg := capture.last()
	assert.Contains(t, msg, "987654", "message body should contain the verification code")
	assert.Contains(t, msg, "verify your email", "message body should contain verification prompt")
	assert.Contains(t, msg, "Subject: Your Concord verification code")
	assert.Contains(t, msg, "From: ")
	assert.Contains(t, msg, "To: ")
	assert.Contains(t, msg, "MIME-Version: 1.0")
	assert.Contains(t, msg, "Content-Type: text/html")
}

func TestSendRecoveryCodeProd(t *testing.T) {
	addr, capture := startMockSMTP(t, mockSMTPOpts{advertiseSTARTTLS: true})
	svc := newProdService(addr)

	err := svc.SendRecoveryCode(testUserEmail, "112233")
	require.NoError(t, err)

	msg := capture.last()
	assert.Contains(t, msg, "112233", "message body should contain the recovery code")
	assert.Contains(t, msg, "recover your account", "message body should contain recovery prompt")
	assert.Contains(t, msg, "Subject: Your Concord account recovery code")
}

func TestSendOwnershipTransferProd(t *testing.T) {
	addr, capture := startMockSMTP(t, mockSMTPOpts{advertiseSTARTTLS: true})
	svc := newProdService(addr)

	err := svc.SendOwnershipTransferNotification(
		testOwnerEmail,
		"Test Server",
		"newowner42",
		"reversal-token-xyz",
	)
	require.NoError(t, err)

	msg := capture.last()
	assert.Contains(t, msg, "Test Server")
	assert.Contains(t, msg, "newowner42")
	assert.Contains(t, msg, "reversal-token-xyz")
	assert.Contains(t, msg, "Subject: Ownership transfer initiated for Test Server")
}

// ── Sanitization Tests ──────────────────────────────────────────────────────

func TestSubjectSanitization(t *testing.T) {
	addr, capture := startMockSMTP(t, mockSMTPOpts{advertiseSTARTTLS: true})
	svc := newProdService(addr)

	// Inject CRLF into subject via ownership transfer (which builds dynamic subjects)
	err := svc.SendOwnershipTransferNotification(
		testOwnerEmail,
		"Evil\r\nBcc: attacker@evil.com\r\nSubject: Hijacked",
		"newowner",
		"token",
	)
	require.NoError(t, err)

	msg := capture.last()
	// Extract SMTP headers (everything before the first blank line)
	headerEnd := strings.Index(msg, "\r\n\r\n")
	require.Greater(t, headerEnd, 0, "message should have header/body separator")
	headers := msg[:headerEnd]

	// Verify no injected header lines — Bcc: must not appear at line start
	for _, line := range strings.Split(headers, "\r\n") {
		assert.False(t, strings.HasPrefix(line, "Bcc:"), "injected Bcc header should not exist")
	}
	// Subject should be collapsed onto one line (CRLFs stripped from server name)
	assert.Contains(t, headers, "Subject: Ownership transfer initiated for Evil")
}

func TestServerNameSanitization(t *testing.T) {
	addr, capture := startMockSMTP(t, mockSMTPOpts{advertiseSTARTTLS: true})
	svc := newProdService(addr)

	err := svc.SendOwnershipTransferNotification(
		testOwnerEmail,
		"Server\r\nName",
		"newowner",
		"token",
	)
	require.NoError(t, err)

	msg := capture.last()
	// Subject should not contain the raw CRLF
	assert.NotContains(t, msg, "Subject: Ownership transfer initiated for Server\r\nName")
}

// ── Template Rendering Tests ────────────────────────────────────────────────

func TestTemplateRenderingVerification(t *testing.T) {
	var body strings.Builder
	err := verificationTmpl.Execute(&body, map[string]string{"Code": "TESTCODE"})
	require.NoError(t, err)
	html := body.String()
	assert.Contains(t, html, "TESTCODE")
	assert.Contains(t, html, "verify your email")
	assert.Contains(t, html, "10 minutes")
	assert.Contains(t, html, "<!DOCTYPE html>")
}

func TestTemplateRenderingRecovery(t *testing.T) {
	var body strings.Builder
	err := recoveryTmpl.Execute(&body, map[string]string{"Code": "RECOVERME"})
	require.NoError(t, err)
	html := body.String()
	assert.Contains(t, html, "RECOVERME")
	assert.Contains(t, html, "recover your account")
}

func TestTemplateRenderingOwnershipTransfer(t *testing.T) {
	var body strings.Builder
	err := ownershipTransferTmpl.Execute(&body, map[string]string{
		"ServerName":       "My Cool Server",
		"NewOwnerUsername": "alice",
		"ReversalToken":    "abc-def-123",
	})
	require.NoError(t, err)
	html := body.String()
	assert.Contains(t, html, "My Cool Server")
	assert.Contains(t, html, "alice")
	assert.Contains(t, html, "abc-def-123")
	assert.Contains(t, html, "24 hours")
}

// ── SMTP Error Path Tests ───────────────────────────────────────────────────

func TestSendMailNoSTARTTLS(t *testing.T) {
	addr, _ := startMockSMTP(t, mockSMTPOpts{advertiseSTARTTLS: false})
	svc := newProdService(addr)

	err := svc.SendVerificationCode(testUserEmail, "123456")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "STARTTLS")
}

func TestSendMailAuthFailure(t *testing.T) {
	addr, _ := startMockSMTP(t, mockSMTPOpts{advertiseSTARTTLS: true, rejectAuth: true})
	svc := newProdService(addr)

	err := svc.SendVerificationCode(testUserEmail, "123456")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SMTP auth")
}

func TestSendMailDialFailure(t *testing.T) {
	svc := &Service{
		host:    testHost,
		port:    1, // nothing listening
		from:    testFrom,
		log:     logger.New("test"),
		devMode: false,
	}
	err := svc.SendVerificationCode(testUserEmail, "123456")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dial SMTP")
}

func TestSendMailNoAuth(t *testing.T) {
	// Service with no username should skip AUTH step
	addr, capture := startMockSMTP(t, mockSMTPOpts{advertiseSTARTTLS: true})
	host, port, _ := net.SplitHostPort(addr)
	p := 0
	_, _ = fmt.Sscanf(port, "%d", &p)
	svc := &Service{
		host:      host,
		port:      p,
		username:  "", // no auth
		password:  "",
		from:      testFrom,
		log:       logger.New("test"),
		devMode:   false,
		tlsConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // test-only: self-signed cert
	}
	err := svc.SendVerificationCode(testUserEmail, "123456")
	require.NoError(t, err)
	assert.NotEmpty(t, capture.last())
}
