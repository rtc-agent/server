// Package email provides email sending functionality.
package email

import "context"

// Message represents an email message to be sent.
type Message struct {
	// To is the recipient email address.
	To string
	// Subject is the email subject.
	Subject string
	// Body is the HTML content of the email.
	Body string
}

// Sender defines the interface for sending emails.
type Sender interface {
	// Send sends an email message.
	Send(ctx context.Context, msg *Message) error
}
