package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ResendMailer delivers mail via Resend's REST API (POST /emails). Stdlib HTTP
// only — no third-party SDK so we keep the build slim. Templates mirror the
// Next.js web flow in lib/email/* so login codes look identical across surfaces.
//
// Construct via NewResendMailer; pass NoopMailer{} where Resend is not desired.
type ResendMailer struct {
	apiKey     string
	from       string
	appBaseURL string
	client     *http.Client
}

// NewResendMailer returns a Resend-backed Mailer. apiKey is required (empty →
// returns a NoopMailer wrapped behavior so callers can swap without nil checks).
// from is the RFC 5322 sender, e.g. `PaperBoxd <onboarding@resend.dev>`.
// appBaseURL is the public web origin used to build the password-reset link.
func NewResendMailer(apiKey, from, appBaseURL string) Mailer {
	if strings.TrimSpace(apiKey) == "" {
		return NoopMailer{}
	}
	if strings.TrimSpace(from) == "" {
		from = "PaperBoxd <onboarding@resend.dev>"
	}
	if strings.TrimSpace(appBaseURL) == "" {
		appBaseURL = "https://paperboxd.in"
	}
	return &ResendMailer{
		apiKey:     apiKey,
		from:       from,
		appBaseURL: strings.TrimRight(appBaseURL, "/"),
		client:     &http.Client{Timeout: 10 * time.Second},
	}
}

type resendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
}

type resendErrorBody struct {
	Message    string `json:"message"`
	Name       string `json:"name"`
	StatusCode int    `json:"statusCode"`
}

func (m *ResendMailer) send(ctx context.Context, to, subject, html string) error {
	payload := resendRequest{
		From:    m.from,
		To:      []string{to},
		Subject: subject,
		HTML:    html,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal resend payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build resend request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("call resend: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Drain so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	var rerr resendErrorBody
	_ = json.Unmarshal(respBody, &rerr)
	if rerr.Message != "" {
		return fmt.Errorf("resend %d %s: %s", resp.StatusCode, rerr.Name, rerr.Message)
	}
	return errors.New("resend " + resp.Status + ": " + string(respBody))
}

// SendOTP delivers a one-time login code. Subject + HTML mirror the web template
// in paperboxd/lib/email/otp-login.ts so the user receives a consistent email
// regardless of which surface (web/mobile) requested the code.
func (m *ResendMailer) SendOTP(ctx context.Context, email, code string) error {
	display := email
	if at := strings.Index(email, "@"); at > 0 {
		display = email[:at]
	}
	return m.send(ctx, email, "Your Login Code - PaperBoxd", otpEmailHTML(display, code))
}

// SendPasswordReset delivers a single-use reset link. The link target matches
// the Next.js proxy's buildResetUrl exactly, so a reset started from any surface
// (web, iOS, Android) lands on the same /auth/reset-password page.
//
// The token travels only inside this link — it is never returned to an API
// caller. See auth.Handler.ForgotPassword.
func (m *ResendMailer) SendPasswordReset(ctx context.Context, email, token string) error {
	resetURL := fmt.Sprintf("%s/auth/reset-password?token=%s&email=%s",
		m.appBaseURL, url.QueryEscape(token), url.QueryEscape(email))
	return m.send(ctx, email, "Reset Your Password - PaperBoxd", passwordResetEmailHTML(email, resetURL))
}

func otpEmailHTML(displayName, code string) string {
	year := time.Now().Year()
	return `<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"><title>Your Login Code</title></head>
<body style="margin:0;padding:0;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,'Helvetica Neue',Arial,sans-serif;background:#fff;">
<table role="presentation" style="width:100%;border-collapse:collapse;background:#fff;"><tr><td align="center" style="padding:48px 20px;">
<table role="presentation" style="max-width:600px;width:100%;border-collapse:collapse;background:#fff;border:1px solid #ebebeb;border-radius:10px;">
<tr><td style="padding:48px 48px 32px;text-align:center;border-bottom:1px solid #ebebeb;">
<h1 style="margin:0;font-size:32px;font-weight:600;color:#252525;letter-spacing:-0.02em;">PaperBoxd</h1></td></tr>
<tr><td style="padding:48px;">
<p style="margin:0 0 8px;font-size:16px;line-height:1.6;color:#252525;font-weight:500;">Hi ` + displayName + `,</p>
<p style="margin:0 0 40px;font-size:15px;line-height:1.6;color:#5a5a5a;">Your verification code to sign in to PaperBoxd:</p>
<table role="presentation" style="width:100%;border-collapse:collapse;margin:40px 0;"><tr><td align="center">
<div style="display:inline-block;padding:32px 48px;background:#f7f7f7;border:1px solid #ebebeb;border-radius:10px;">
<p style="margin:0;font-size:40px;font-weight:700;letter-spacing:12px;font-family:'Courier New',monospace;color:#252525;">` + code + `</p>
</div></td></tr></table>
<div style="padding:16px 20px;background:#f7f7f7;border:1px solid #ebebeb;border-radius:10px;margin:32px 0;">
<p style="margin:0;font-size:14px;line-height:1.6;color:#5a5a5a;"><strong style="color:#252525;">This code expires in 10 minutes.</strong> Enter it on the sign-in page to complete your login.</p></div>
<div style="padding:16px 20px;background:#f7f7f7;border:1px solid #ebebeb;border-radius:10px;margin:24px 0;">
<p style="margin:0 0 8px;font-size:14px;line-height:1.6;color:#252525;font-weight:500;">Security Notice</p>
<p style="margin:0;font-size:13px;line-height:1.6;color:#5a5a5a;">Never share this code with anyone. PaperBoxd staff will never ask for your verification code. You have 5 attempts to enter the correct code.</p></div>
<p style="margin:32px 0 0;font-size:14px;line-height:1.6;color:#8a8a8a;">If you didn't request this code, please ignore this email. Your account remains secure.</p>
</td></tr>
<tr><td style="padding:32px 48px;background:#f7f7f7;border-top:1px solid #ebebeb;text-align:center;border-radius:0 0 10px 10px;">
<p style="margin:0 0 8px;font-size:12px;line-height:1.6;color:#8a8a8a;">Need help? Contact us at <a href="mailto:paperboxd@gmail.com" style="color:#252525;text-decoration:underline;text-underline-offset:2px;">paperboxd@gmail.com</a></p>
<p style="margin:0;font-size:12px;line-height:1.6;color:#8a8a8a;">© ` + fmt.Sprintf("%d", year) + ` PaperBoxd. All rights reserved.</p>
</td></tr></table></td></tr></table></body></html>`
}

func passwordResetEmailHTML(email, resetURL string) string {
	year := time.Now().Year()
	display := email
	if at := strings.Index(email, "@"); at > 0 {
		display = email[:at]
	}
	return `<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"><title>Reset Your Password</title></head>
<body style="margin:0;padding:0;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,'Helvetica Neue',Arial,sans-serif;background:#fff;">
<table role="presentation" style="width:100%;border-collapse:collapse;background:#fff;"><tr><td align="center" style="padding:48px 20px;">
<table role="presentation" style="max-width:600px;width:100%;border-collapse:collapse;background:#fff;border:1px solid #ebebeb;border-radius:10px;">
<tr><td style="padding:48px 48px 32px;text-align:center;border-bottom:1px solid #ebebeb;">
<h1 style="margin:0;font-size:32px;font-weight:600;color:#252525;letter-spacing:-0.02em;">PaperBoxd</h1></td></tr>
<tr><td style="padding:48px;">
<p style="margin:0 0 8px;font-size:16px;line-height:1.6;color:#252525;font-weight:500;">Hi ` + display + `,</p>
<p style="margin:0 0 24px;font-size:15px;line-height:1.6;color:#5a5a5a;">Tap the button below to choose a new password. This link is single-use and expires in 1 hour.</p>
<table role="presentation" style="border-collapse:collapse;margin:32px 0;"><tr><td align="center" style="border-radius:10px;background:#252525;">
<a href="` + resetURL + `" style="display:inline-block;padding:14px 32px;font-size:15px;font-weight:600;color:#ffffff;text-decoration:none;border-radius:10px;">Reset your password</a>
</td></tr></table>
<p style="margin:0 0 8px;font-size:13px;line-height:1.6;color:#8a8a8a;">Or paste this link into your browser:</p>
<div style="padding:16px 20px;background:#f7f7f7;border:1px solid #ebebeb;border-radius:10px;margin:0 0 24px;word-break:break-all;">
<p style="margin:0;font-size:13px;line-height:1.6;color:#252525;font-family:'Courier New',monospace;">` + resetURL + `</p></div>
<p style="margin:32px 0 0;font-size:14px;line-height:1.6;color:#8a8a8a;">If you didn't request a password reset, please ignore this email. Your account remains secure.</p>
</td></tr>
<tr><td style="padding:32px 48px;background:#f7f7f7;border-top:1px solid #ebebeb;text-align:center;border-radius:0 0 10px 10px;">
<p style="margin:0;font-size:12px;line-height:1.6;color:#8a8a8a;">© ` + fmt.Sprintf("%d", year) + ` PaperBoxd. All rights reserved.</p>
</td></tr></table></td></tr></table></body></html>`
}
