package notify

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// EmailNotifier delivers notifications over SMTP.
//
// Two details matter more than they look for Chinese mail providers:
//
//   - The Subject must be RFC 2047 encoded. QQ Mail and 163 render a raw
//     UTF-8 subject as mojibake, and some filters reject it outright.
//   - Date and Message-ID must be present. Messages lacking them are
//     disproportionately spam-filtered.
type EmailNotifier struct {
	host       string
	port       int
	user       string
	pass       string
	from       string
	recipients []string
	timeout    time.Duration
}

// NewEmailNotifier builds an email notifier. When port is 465 the connection
// uses implicit TLS; otherwise STARTTLS is negotiated when the server offers it.
func NewEmailNotifier(host string, port int, user, pass, from string, recipients []string, timeout time.Duration) *EmailNotifier {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &EmailNotifier{
		host: host, port: port, user: user, pass: pass,
		from: from, recipients: recipients, timeout: timeout,
	}
}

// Targets returns one target per recipient, so a single bad address does not
// block delivery to the others.
func (n *EmailNotifier) Targets() []Target {
	out := make([]Target, 0, len(n.recipients))
	for _, r := range n.recipients {
		out = append(out, Target{Channel: "email", Address: r})
	}
	return out
}

// Send delivers one batch to one recipient.
func (n *EmailNotifier) Send(ctx context.Context, t Target, events []Event) error {
	if len(events) == 0 {
		return nil
	}

	client, err := n.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Quit() }()

	if n.user != "" {
		if err := client.Auth(smtp.PlainAuth("", n.user, n.pass, n.host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := client.Mail(n.from); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	if err := client.Rcpt(t.Address); err != nil {
		return fmt.Errorf("smtp RCPT TO %s: %w", t.Address, err)
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(buildMessage(n.from, t.Address, events)); err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close: %w", err)
	}
	return nil
}

// dial establishes an authenticated-capable SMTP client.
func (n *EmailNotifier) dial(ctx context.Context) (*smtp.Client, error) {
	addr := net.JoinHostPort(n.host, fmt.Sprint(n.port))

	dialer := &net.Dialer{Timeout: n.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	// Bound the whole SMTP conversation, not just the TCP handshake.
	_ = conn.SetDeadline(time.Now().Add(n.timeout))

	tlsCfg := &tls.Config{
		ServerName: n.host,
		MinVersion: tls.VersionTLS12,
	}

	// Port 465 is implicit TLS: the handshake happens before any SMTP
	// greeting, so smtp.Dial cannot be used at all.
	if n.port == 465 {
		tlsConn := tls.Client(conn, tlsCfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("smtp implicit TLS handshake: %w", err)
		}
		client, err := smtp.NewClient(tlsConn, n.host)
		if err != nil {
			_ = tlsConn.Close()
			return nil, fmt.Errorf("smtp client: %w", err)
		}
		return client, nil
	}

	client, err := smtp.NewClient(conn, n.host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("smtp client: %w", err)
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(tlsCfg); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("smtp STARTTLS: %w", err)
		}
	} else if n.user != "" {
		// Sending credentials over a cleartext session is worse than failing.
		_ = client.Close()
		return nil, fmt.Errorf("smtp server %s offers no STARTTLS; refusing to send credentials in the clear", n.host)
	}
	return client, nil
}

// buildMessage renders the RFC 5322 message. Header lines use CRLF; the smtp
// package's dot-writer handles dot-stuffing.
func buildMessage(from, to string, events []Event) []byte {
	var b strings.Builder

	subject := fmt.Sprintf("考试信息更新：%d 条新公告", len(events))
	if len(events) == 1 {
		subject = "考试信息更新：" + truncate(events[0].Title, 60)
	}

	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	// RFC 2047 encode: a raw UTF-8 subject is mojibake in most Chinese clients.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: %s\r\n", newMessageID(from))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(renderPlainText(events))

	return []byte(b.String())
}

// newMessageID builds a unique Message-ID using the sender's domain.
func newMessageID(from string) string {
	domain := "exams.local"
	if i := strings.LastIndex(from, "@"); i >= 0 && i+1 < len(from) {
		domain = from[i+1:]
	}
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("<%s.%d@%s>", hex.EncodeToString(buf), time.Now().UnixNano(), domain)
}
