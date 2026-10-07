// Package email provides email sending functionality.
package email

import (
	"context"
	"fmt"

	"gopkg.in/gomail.v2"
)

// SMTPConfig holds SMTP server configuration.
type SMTPConfig struct {
	Host        string
	Port        int
	User        string
	Password    string
	FromAddress string
	FromName    string
}

// SMTPSender implements Sender using SMTP.
type SMTPSender struct {
	cfg    SMTPConfig
	dialer *gomail.Dialer
}

// NewSMTPSender creates a new SMTPSender.
func NewSMTPSender(cfg SMTPConfig) *SMTPSender {
	dialer := gomail.NewDialer(cfg.Host, cfg.Port, cfg.User, cfg.Password)
	// gomail uses STARTTLS by default when Port is 587
	return &SMTPSender{
		cfg:    cfg,
		dialer: dialer,
	}
}

// Send sends an email via SMTP.
func (s *SMTPSender) Send(ctx context.Context, msg *Message) error {
	m := gomail.NewMessage()

	// Set sender with display name
	from := m.FormatAddress(s.cfg.FromAddress, s.cfg.FromName)
	m.SetHeader("From", from)
	m.SetHeader("To", msg.To)
	m.SetHeader("Subject", msg.Subject)
	m.SetBody("text/html", msg.Body)

	if err := s.dialer.DialAndSend(m); err != nil {
		return fmt.Errorf("smtp send email to %s: %w", msg.To, err)
	}

	return nil
}
