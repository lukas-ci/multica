package service

import (
	"fmt"
	"html"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	gomail "github.com/go-mail/mail/v2"
	"github.com/resend/resend-go/v2"
)

// maxSubjectFieldRunes caps the length of user-controlled text in email Subject
// lines to prevent header-injection-style abuse via long strings.
const maxSubjectFieldRunes = 60

type EmailSender interface {
	SendVerificationCode(to, code string) error
	SendInvitationEmail(to, inviterName, workspaceName, invitationID string) error
}

type EmailService struct {
	sender EmailSender
}

func NewEmailService() *EmailService {
	provider := os.Getenv("EMAIL_PROVIDER")
	if provider == "smtp" {
		return &EmailService{sender: newSMTPSender()}
	}
	return &EmailService{sender: newResendSender()}
}

func (s *EmailService) SendVerificationCode(to, code string) error {
	return s.sender.SendVerificationCode(to, code)
}

func (s *EmailService) SendInvitationEmail(to, inviterName, workspaceName, invitationID string) error {
	return s.sender.SendInvitationEmail(to, inviterName, workspaceName, invitationID)
}

type resendSender struct {
	client    *resend.Client
	fromEmail string
}

func newResendSender() *resendSender {
	apiKey := os.Getenv("RESEND_API_KEY")
	from := os.Getenv("RESEND_FROM_EMAIL")
	if from == "" {
		from = "noreply@multica.ai"
	}
	var client *resend.Client
	if apiKey != "" {
		client = resend.NewClient(apiKey)
	}
	return &resendSender{
		client:    client,
		fromEmail: from,
	}
}

func (s *resendSender) SendVerificationCode(to, code string) error {
	if s.client == nil {
		fmt.Printf("[DEV] Verification code for %s: %s\n", to, code)
		return nil
	}
	params := &resend.SendEmailRequest{
		From:    s.fromEmail,
		To:      []string{to},
		Subject: "Your Multica verification code",
		Html:    fmt.Sprintf(`<div style="font-family: sans-serif; max-width: 400px; margin: 0 auto;"><h2>Your verification code</h2><p style="font-size: 32px; font-weight: bold; letter-spacing: 8px; margin: 24px 0;">%s</p><p>This code expires in 10 minutes.</p><p style="color: #666; font-size: 14px;">If you didn't request this code, you can safely ignore this email.</p></div>`, code),
	}
	_, err := s.client.Emails.Send(params)
	return err
}

func (s *resendSender) SendInvitationEmail(to, inviterName, workspaceName, invitationID string) error {
	if s.client == nil {
		appURL := strings.TrimSpace(os.Getenv("FRONTEND_ORIGIN"))
		if appURL == "" {
			appURL = "https://app.multica.ai"
		}
		inviteURL := fmt.Sprintf("%s/invite/%s", appURL, invitationID)
		fmt.Printf("[DEV] Invitation email to %s: %s invited you to %s — %s\n", to, inviterName, workspaceName, inviteURL)
		return nil
	}
	appURL := strings.TrimSpace(os.Getenv("FRONTEND_ORIGIN"))
	if appURL == "" {
		appURL = "https://app.multica.ai"
	}
	inviteURL := fmt.Sprintf("%s/invite/%s", appURL, invitationID)
	params := buildInvitationParams(s.fromEmail, to, inviterName, workspaceName, inviteURL)
	_, err := s.client.Emails.Send(params)
	return err
}

type smtpSender struct {
	fromEmail string
	host      string
	port      int
	username  string
	password  string
	devMode   bool
}

func newSMTPSender() *smtpSender {
	host := os.Getenv("SMTP_HOST")
	port := 1025
	if p := os.Getenv("SMTP_PORT"); p != "" {
		if v, err := strconv.Atoi(p); err == nil {
			port = v
		}
	}
	return &smtpSender{
		fromEmail: os.Getenv("SMTP_FROM_EMAIL"),
		host:      host,
		port:      port,
		username:  os.Getenv("SMTP_USERNAME"),
		password:  os.Getenv("SMTP_PASSWORD"),
		devMode:   host == "",
	}
}

// SendVerificationCode sends a verification code email via SMTP.
// When devMode is enabled, it prints the code to stdout instead.
func (s *smtpSender) SendVerificationCode(to, code string) error {
	if s.devMode {
		fmt.Printf("[DEV] Verification code for %s: %s\n", to, code)
		return nil
	}
	msg := gomail.NewMessage()
	msg.SetHeader("From", s.fromEmail)
	msg.SetHeader("To", to)
	msg.SetHeader("Subject", "Your Multica verification code")
	msg.SetBody("text/html", fmt.Sprintf(`<div style="font-family: sans-serif; max-width: 400px; margin: 0 auto;"><h2>Your verification code</h2><p style="font-size: 32px; font-weight: bold; letter-spacing: 8px; margin: 24px 0;">%s</p><p>This code expires in 10 minutes.</p><p style="color: #666; font-size: 14px;">If you didn't request this code, you can safely ignore this email.</p></div>`, code))

	d := gomail.NewDialer(s.host, s.port, s.username, s.password)
	return d.DialAndSend(msg)
}

// SendInvitationEmail sends an invitation email via SMTP.
// When devMode is enabled, it prints the invitation details to stdout instead.
func (s *smtpSender) SendInvitationEmail(to, inviterName, workspaceName, invitationID string) error {
	if s.devMode {
		appURL := strings.TrimSpace(os.Getenv("FRONTEND_ORIGIN"))
		if appURL == "" {
			appURL = "https://app.multica.ai"
		}
		inviteURL := fmt.Sprintf("%s/invite/%s", appURL, invitationID)
		fmt.Printf("[DEV] Invitation email to %s: %s invited you to %s — %s\n", to, inviterName, workspaceName, inviteURL)
		return nil
	}
	appURL := strings.TrimSpace(os.Getenv("FRONTEND_ORIGIN"))
	if appURL == "" {
		appURL = "https://app.multica.ai"
	}
	inviteURL := fmt.Sprintf("%s/invite/%s", appURL, invitationID)

	subjectInviter := sanitizeSubjectField(inviterName)
	subjectWorkspace := sanitizeSubjectField(workspaceName)
	safeWorkspace := html.EscapeString(workspaceName)
	safeInviter := html.EscapeString(inviterName)

	msg := gomail.NewMessage()
	msg.SetHeader("From", s.fromEmail)
	msg.SetHeader("To", to)
	msg.SetHeader("Subject", fmt.Sprintf("%s invited you to %s on Multica", subjectInviter, subjectWorkspace))
	msg.SetBody("text/html", fmt.Sprintf(`<div style="font-family: sans-serif; max-width: 480px; margin: 0 auto;"><h2>You're invited to join %s</h2><p><strong>%s</strong> invited you to collaborate in the <strong>%s</strong> workspace on Multica.</p><p style="margin: 24px 0;"><a href="%s" style="display: inline-block; padding: 12px 24px; background: #000; color: #fff; text-decoration: none; border-radius: 6px; font-weight: 500;">Accept invitation</a></p><p style="color: #666; font-size: 14px;">You'll need to log in to accept or decline the invitation.</p></div>`, safeWorkspace, safeInviter, safeWorkspace, inviteURL))

	d := gomail.NewDialer(s.host, s.port, s.username, s.password)
	return d.DialAndSend(msg)
}

// buildInvitationParams assembles the Resend request for an invitation email.
// Separated from SendInvitationEmail so the sanitization behavior is unit-testable
// without needing to mock the Resend SDK.
func buildInvitationParams(from, to, inviterName, workspaceName, inviteURL string) *resend.SendEmailRequest {
	safeWorkspace := html.EscapeString(workspaceName)
	safeInviter := html.EscapeString(inviterName)
	subjectInviter := sanitizeSubjectField(inviterName)
	subjectWorkspace := sanitizeSubjectField(workspaceName)

	return &resend.SendEmailRequest{
		From:    from,
		To:      []string{to},
		Subject: fmt.Sprintf("%s invited you to %s on Multica", subjectInviter, subjectWorkspace),
		Html: fmt.Sprintf(
			`<div style="font-family: sans-serif; max-width: 480px; margin: 0 auto;">
				<h2>You're invited to join %s</h2>
				<p><strong>%s</strong> invited you to collaborate in the <strong>%s</strong> workspace on Multica.</p>
				<p style="margin: 24px 0;">
					<a href="%s" style="display: inline-block; padding: 12px 24px; background: #000; color: #fff; text-decoration: none; border-radius: 6px; font-weight: 500;">Accept invitation</a>
				</p>
				<p style="color: #666; font-size: 14px;">You'll need to log in to accept or decline the invitation.</p>
			</div>`, safeWorkspace, safeInviter, safeWorkspace, inviteURL),
	}
}

// sanitizeSubjectField prepares user-controlled text for the email Subject line.
// Subject is not HTML-rendered, so HTML-escaping would leak literal entities
// (e.g. &lt;script&gt;) into the recipient's inbox. Instead strip control
// characters (defense in depth against header-injection-adjacent abuse even
// though Resend also filters CR/LF) and cap length so attackers can't stuff
// a full phishing subject into a workspace name.
func sanitizeSubjectField(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	cleaned := b.String()
	if utf8.RuneCountInString(cleaned) <= maxSubjectFieldRunes {
		return cleaned
	}
	runes := []rune(cleaned)
	return string(runes[:maxSubjectFieldRunes-1]) + "…"
}
