//go:build server

package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SMTPSender sends transactional mail through a generic SMTP server.
// In production we point this at Zoho Mail (smtp.zoho.com:587, or .eu
// for EU-region accounts). Implements both shared/email.Sender (used
// by the auth flows: recovery, transfer OTP, email verification) and
// notifications/domain.EmailProvider (the SendTransactional contract)
// so a single instance backs both interfaces.
//
// MAIL FROM is set to the authenticated SMTP user — Zoho enforces this
// to match the account credentials. The From: header can be a verified
// alias on the same mailbox (e.g. noreply@ when authenticating as
// soporte@), which is the standard pattern for keeping a human-friendly
// support inbox separate from the transactional sender.
type SMTPSender struct {
	host     string
	port     string
	username string
	password string
	from     string
	timeout  time.Duration
}

type SMTPOptions struct {
	Host string
	// Port: "587" uses STARTTLS, "465" uses implicit TLS. Default 587.
	Port     string
	Username string
	Password string
	// From is the value of the RFC 5322 From: header. It can be an
	// alias of Username on the same mailbox — Zoho will accept the
	// send as long as the alias is configured under the account.
	From    string
	Timeout time.Duration
}

func NewSMTPSender(opts SMTPOptions) (*SMTPSender, error) {
	if strings.TrimSpace(opts.Host) == "" {
		return nil, errors.New("smtp: host missing")
	}
	if strings.TrimSpace(opts.Username) == "" {
		return nil, errors.New("smtp: username missing")
	}
	if strings.TrimSpace(opts.Password) == "" {
		return nil, errors.New("smtp: password missing")
	}
	if strings.TrimSpace(opts.From) == "" {
		return nil, errors.New("smtp: from address missing")
	}
	if opts.Port == "" {
		opts.Port = "587"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	return &SMTPSender{
		host:     opts.Host,
		port:     opts.Port,
		username: opts.Username,
		password: opts.Password,
		from:     opts.From,
		timeout:  opts.Timeout,
	}, nil
}

// Send implements shared/email.Sender.
func (s *SMTPSender) Send(ctx context.Context, m Message) error {
	_, err := s.SendTransactional(ctx, m.To, m.Subject, m.Body, m.Tag)
	return err
}

// SendTransactional implements notifications/domain.EmailProvider. The
// returned id is a client-generated UUID — SMTP doesn't reliably
// surface a provider message id (the queue id, if any, is buried in
// the 250 OK response and varies per server), and the only consumer
// is the notifications log for grep / dedup.
func (s *SMTPSender) SendTransactional(ctx context.Context, to, subject, body, tag string) (string, error) {
	if strings.TrimSpace(to) == "" {
		return "", nil
	}
	if _, err := mail.ParseAddress(to); err != nil {
		return "", fmt.Errorf("smtp: invalid recipient %q: %w", to, err)
	}
	raw := buildRFC822(s.from, to, subject, body, tag)
	addr := net.JoinHostPort(s.host, s.port)

	dialer := &net.Dialer{Timeout: s.timeout}
	var conn net.Conn
	var err error
	tlsCfg := &tls.Config{ServerName: s.host}
	if s.port == "465" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return "", fmt.Errorf("smtp: dial %s: %w", addr, err)
	}
	// SetDeadline guards against a misbehaving server hanging mid-handshake.
	_ = conn.SetDeadline(time.Now().Add(s.timeout))

	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close()
		return "", fmt.Errorf("smtp: hello: %w", err)
	}
	defer func() { _ = c.Quit() }()

	if s.port != "465" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return "", errors.New("smtp: server does not advertise STARTTLS on submission port")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return "", fmt.Errorf("smtp: starttls: %w", err)
		}
	}

	auth := smtp.PlainAuth("", s.username, s.password, s.host)
	if err := c.Auth(auth); err != nil {
		return "", fmt.Errorf("smtp: auth: %w", err)
	}
	// Envelope sender (MAIL FROM) is the authenticated mailbox. Zoho
	// will 553 if this differs from the credentialed account, even
	// when the From: header is a verified alias.
	if err := c.Mail(s.username); err != nil {
		return "", fmt.Errorf("smtp: mail from: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return "", fmt.Errorf("smtp: rcpt to: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return "", fmt.Errorf("smtp: data: %w", err)
	}
	if _, err := w.Write([]byte(raw)); err != nil {
		_ = w.Close()
		return "", fmt.Errorf("smtp: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("smtp: close data: %w", err)
	}
	return uuid.NewString(), nil
}

// buildRFC822 assembles the wire-format message. CRLF line endings,
// UTF-8 8-bit body, Q-encoded Subject when it contains non-ASCII.
func buildRFC822(from, to, subject, body, tag string) string {
	// Normalize body to CRLF — Go's smtp data writer handles
	// dot-stuffing but expects CRLF-terminated lines.
	normalized := strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + encodeSubject(subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	if tag != "" {
		b.WriteString("X-Tinta-Tag: " + tag + "\r\n")
	}
	b.WriteString("\r\n")
	b.WriteString(normalized)
	return b.String()
}

func encodeSubject(s string) string {
	for _, r := range s {
		if r > 127 {
			return mime.QEncoding.Encode("UTF-8", s)
		}
	}
	return s
}
