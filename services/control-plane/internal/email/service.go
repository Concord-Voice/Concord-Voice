// Package email provides email sending functionality for user verification and notifications.
package email

import (
	"crypto/tls"
	"embed"
	"fmt"
	"html/template"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

//go:embed templates/*.html
var templateFS embed.FS

var verificationTmpl = template.Must(template.ParseFS(templateFS, "templates/verification.html"))
var recoveryTmpl = template.Must(template.ParseFS(templateFS, "templates/recovery.html"))
var ownershipTransferTmpl = template.Must(template.ParseFS(templateFS, "templates/ownership_transfer.html"))

const (
	errInvalidFromAddr = "invalid from address: %w"
	errInvalidToAddr   = "invalid to address: %w"
	headerFrom         = "From: "
	mimeVersion        = "MIME-Version: 1.0\r\n"
	contentTypeHTML    = "Content-Type: text/html; charset=UTF-8\r\n"
)

// Service sends emails via SMTP. When SMTPHost is empty (dev mode), codes are
// logged to stdout instead of sent.
type Service struct {
	host      string
	port      int
	username  string
	password  string // #nosec G101 -- struct field name, not a secret
	from      string
	log       *logger.Logger
	devMode   bool
	tlsConfig *tls.Config // nil = default (verify against system CAs); set in tests only
}

// NewService creates an email service from the application config.
func NewService(cfg *config.Config, log *logger.Logger) *Service {
	return &Service{
		host:     cfg.SMTPHost,
		port:     cfg.SMTPPort,
		username: cfg.SMTPUsername,
		password: cfg.SMTPPassword, // #nosec G101 -- loaded from env, not hardcoded
		from:     cfg.SMTPFrom,
		log:      log,
		devMode:  cfg.SMTPHost == "",
	}
}

// sendTemplatedEmail renders a template with data, builds the message, and sends it.
func (s *Service) sendTemplatedEmail(to, subject string, tmpl *template.Template, data interface{}) error {
	// Sanitize subject to prevent SMTP header injection
	subject = strings.NewReplacer("\r", "", "\n", "").Replace(subject)

	var body strings.Builder
	if err := tmpl.Execute(&body, data); err != nil {
		return fmt.Errorf("render email template: %w", err)
	}

	fromAddr, err := mail.ParseAddress(s.from)
	if err != nil {
		return fmt.Errorf(errInvalidFromAddr, err)
	}
	toAddr, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf(errInvalidToAddr, err)
	}

	msg := headerFrom + fromAddr.String() + "\r\n" +
		"To: " + toAddr.String() + "\r\n" +
		"Subject: " + subject + "\r\n" +
		mimeVersion +
		contentTypeHTML +
		"\r\n" +
		body.String()

	return s.sendMail(fromAddr.Address, toAddr.Address, msg)
}

// SendVerificationCode sends a 6-digit verification code to the given email address.
func (s *Service) SendVerificationCode(to, code string) error {
	if s.devMode {
		s.log.Info("DEV MODE — email verification code", "to", to, "code", code)
		return nil
	}
	return s.sendTemplatedEmail(to, "Your Concord verification code", verificationTmpl, map[string]string{"Code": code})
}

// SendRecoveryCode sends a 6-digit account recovery code to the given email address.
func (s *Service) SendRecoveryCode(to, code string) error {
	if s.devMode {
		s.log.Info("DEV MODE — account recovery code", "to", to, "code", code)
		return nil
	}
	return s.sendTemplatedEmail(to, "Your Concord account recovery code", recoveryTmpl, map[string]string{"Code": code})
}

// sendMail handles the SMTP connection and message delivery.
func (s *Service) sendMail(from, to, msg string) error {
	addr := net.JoinHostPort(s.host, fmt.Sprintf("%d", s.port))

	auth := smtp.PlainAuth("", s.username, s.password, s.host)
	tlsCfg := s.tlsConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12} // #nosec G402 -- using server name verification
	}

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial SMTP: %w", err)
	}

	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("create SMTP client: %w", err)
	}
	defer func() { _ = client.Close() }()

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	} else {
		return fmt.Errorf("SMTP server does not support STARTTLS — refusing to send codes over plaintext")
	}

	if s.username != "" {
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}

	if err := client.Mail(from); err != nil {
		return fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("SMTP RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("SMTP write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("SMTP close data: %w", err)
	}

	return client.Quit()
}

// SendOwnershipTransferNotification sends an ownership transfer notification to the server owner.
func (s *Service) SendOwnershipTransferNotification(to, serverName, newOwnerUsername, reversalToken string) error {
	if s.devMode {
		s.log.Info("DEV MODE — ownership transfer notification",
			"to", to, "server", serverName, "new_owner", newOwnerUsername, "reversal_token", reversalToken)
		return nil
	}

	// Sanitize server name to prevent SMTP header injection
	safeServerName := strings.NewReplacer("\r", "", "\n", "").Replace(serverName)

	return s.sendTemplatedEmail(to, "Ownership transfer initiated for "+safeServerName, ownershipTransferTmpl, map[string]string{
		"ServerName":       serverName,
		"NewOwnerUsername": newOwnerUsername,
		"ReversalToken":    reversalToken,
	})
}

// IsDevMode returns true when the service is operating without a real SMTP server.
func (s *Service) IsDevMode() bool {
	return s.devMode
}
